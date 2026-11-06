package factory

import (
	"time"

	"context"
	"errors"
	"fmt"
	"github.com/PureMunky/reversible/telemetry"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Version is one build of the service, generated from one intent.
type Version struct {
	SpecHash string
	Spec     ServiceSpec
	Files    map[string]string // the cache: generated, never edited
	Dir      string
	Binary   string
	Results  []Result // the spec's examples, run against this build
	Attempts int
	Cached   bool // true if this intent had already been built
}

// Passed reports whether every example passed.
func (v *Version) Passed() bool {
	if v == nil || len(v.Results) == 0 {
		return false
	}
	for _, r := range v.Results {
		if !r.Pass {
			return false
		}
	}
	return true
}

// Lines counts generated lines of code.
func (v *Version) Lines() int {
	n := 0
	for _, src := range v.Files {
		n += strings.Count(src, "\n")
	}
	return n
}

// Factory turns intent into running, verified builds.
type Factory struct {
	Root  string // where builds are cached
	Coder Coder
	// MaxAttempts bounds the generate → build → verify → repair loop
	// (default 3).
	MaxAttempts int

	mu    sync.Mutex
	cache map[string]*Version
}

// ErrNotReady is returned when the intent still has blocking questions.
var ErrNotReady = errors.New("intent is not ready to build")

// QuestionsError carries the questions that block a build.
type QuestionsError struct{ Questions []Question }

func (e *QuestionsError) Error() string {
	var b strings.Builder
	b.WriteString("intent is not ready to build:")
	for _, q := range e.Questions {
		b.WriteString("\n  " + q.String())
	}
	return b.String()
}

func (e *QuestionsError) Is(t error) bool { return t == ErrNotReady }

// Build generates, compiles and verifies a version from the spec. An
// intent that has been built before is served from the cache: the code is
// a pure function of the intent. A build that doesn't pass the examples
// is fed back to the coder, with the failures, up to MaxAttempts times.
func (f *Factory) Build(ctx context.Context, spec ServiceSpec) (*Version, error) {
	hash := spec.Hash()
	qs := spec.Check()
	var blocking []Question
	for _, q := range qs {
		if q.Severity == Blocking {
			blocking = append(blocking, q)
		}
	}
	tel.Emit("spec.checked", telemetry.F{"service": spec.Service, "spec": telemetry.Short(hash), "blocking": len(blocking), "clarify": len(qs) - len(blocking)})
	if !Ready(qs) {
		mBuilds.Inc(spec.Service, "blocked")
		return nil, &QuestionsError{Questions: blocking}
	}
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]*Version{}
	}
	if v, ok := f.cache[hash]; ok {
		f.mu.Unlock()
		c := *v
		c.Cached = true
		mBuilds.Inc(spec.Service, "cached")
		tel.Emit("build.finished", telemetry.F{"service": spec.Service, "spec": telemetry.Short(hash), "outcome": "cached"})
		return &c, nil
	}
	f.mu.Unlock()

	coder := f.Coder
	if coder == nil {
		coder = Codegen{}
	}
	max := f.MaxAttempts
	if max == 0 {
		max = 3
	}
	v := &Version{SpecHash: hash, Spec: spec, Dir: filepath.Join(f.Root, hash)}
	started, cname := time.Now(), coderName(coder)
	tel.Emit("build.started", telemetry.F{"service": spec.Service, "spec": telemetry.Short(hash), "coder": cname, "examples": len(spec.Examples)})
	attempt := func(result string, f telemetry.F) {
		mAttempts.Inc(spec.Service, cname, result)
		f["service"], f["spec"], f["attempt"], f["result"] = spec.Service, telemetry.Short(hash), v.Attempts, result
		tel.Emit("build.attempt", f)
	}
	finish := func(outcome string) {
		mBuilds.Inc(spec.Service, outcome)
		mBuildSecs.Observe(since(started), spec.Service)
		ev := telemetry.F{"service": spec.Service, "spec": telemetry.Short(hash), "outcome": outcome, "attempts": v.Attempts, "seconds": since(started)}
		if outcome == "passed" {
			mGenLines.Set(float64(v.Lines()), spec.Service, telemetry.Short(hash))
			ev["lines"] = v.Lines()
		}
		tel.Emit("build.finished", ev)
	}
	var prev map[string]string
	var failures []Result
	var lastErr error
	for v.Attempts < max {
		v.Attempts++
		files, err := coder.Code(ctx, spec, prev, failures)
		if err != nil {
			attempt("coder_error", telemetry.F{"error": err.Error()})
			finish("failed")
			return v, fmt.Errorf("attempt %d: %w", v.Attempts, err)
		}
		v.Files, prev = files, files
		os.RemoveAll(v.Dir)
		bin, err := compile(ctx, v.Dir, files)
		if err != nil {
			failures, lastErr = []Result{{Problem: err.Error()}}, err
			attempt("compile_failed", telemetry.F{"error": firstLine(strings.TrimPrefix(err.Error(), "build failed:\n"), 300)})
			continue
		}
		v.Binary = bin
		results, err := runExamples(ctx, bin, spec.Examples)
		if err != nil {
			failures, lastErr = []Result{{Problem: "running the service: " + err.Error()}}, err
			attempt("compile_failed", telemetry.F{"error": "running the service: " + err.Error()})
			continue
		}
		v.Results = results
		failures = nil
		var failed []string
		for _, r := range results {
			if !r.Pass {
				failures = append(failures, r)
				failed = append(failed, r.Example.Name)
			}
		}
		mExamples.Add(float64(len(results)-len(failures)), spec.Service, "pass")
		mExamples.Add(float64(len(failures)), spec.Service, "fail")
		if len(failures) == 0 {
			attempt("passed", telemetry.F{"examples": len(results)})
			f.mu.Lock()
			f.cache[hash] = v
			f.mu.Unlock()
			finish("passed")
			return v, nil
		}
		attempt("examples_failed", telemetry.F{"examples": len(results), "failed": failed})
		lastErr = fmt.Errorf("%d of %d examples failed", len(failures), len(results))
	}
	finish("failed")
	return v, fmt.Errorf("no passing build after %d attempts: %w", v.Attempts, lastErr)
}

// Report summarises a build's verification.
func (v *Version) Report() string {
	var b strings.Builder
	pass := 0
	for _, r := range v.Results {
		if r.Pass {
			pass++
		}
	}
	fmt.Fprintf(&b, "spec %s: %d/%d examples pass, %d generated lines, %d attempt(s)", v.SpecHash, pass, len(v.Results), v.Lines(), v.Attempts)
	if v.Cached {
		b.WriteString(" (cached: intent unchanged)")
	}
	b.WriteString("\n")
	names := make([]string, 0, len(v.Results))
	for _, r := range v.Results {
		if !r.Pass {
			names = append(names, fmt.Sprintf("  ✗ %s: %s", r.Example.Name, r.Problem))
		}
	}
	sort.Strings(names)
	for _, n := range names {
		b.WriteString(n + "\n")
	}
	return b.String()
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max]
	}
	return s
}
