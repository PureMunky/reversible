package factory

import (
	"context"
	"errors"
	"fmt"
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
	if qs := spec.Check(); !Ready(qs) {
		var blocking []Question
		for _, q := range qs {
			if q.Severity == Blocking {
				blocking = append(blocking, q)
			}
		}
		return nil, &QuestionsError{Questions: blocking}
	}
	hash := spec.Hash()
	f.mu.Lock()
	if f.cache == nil {
		f.cache = map[string]*Version{}
	}
	if v, ok := f.cache[hash]; ok {
		f.mu.Unlock()
		c := *v
		c.Cached = true
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
	var prev map[string]string
	var failures []Result
	var lastErr error
	for v.Attempts < max {
		v.Attempts++
		files, err := coder.Code(ctx, spec, prev, failures)
		if err != nil {
			return v, fmt.Errorf("attempt %d: %w", v.Attempts, err)
		}
		v.Files, prev = files, files
		os.RemoveAll(v.Dir)
		bin, err := compile(ctx, v.Dir, files)
		if err != nil {
			failures, lastErr = []Result{{Problem: err.Error()}}, err
			continue
		}
		v.Binary = bin
		results, err := runExamples(ctx, bin, spec.Examples)
		if err != nil {
			failures, lastErr = []Result{{Problem: "running the service: " + err.Error()}}, err
			continue
		}
		v.Results = results
		failures = nil
		for _, r := range results {
			if !r.Pass {
				failures = append(failures, r)
			}
		}
		if len(failures) == 0 {
			f.mu.Lock()
			f.cache[hash] = v
			f.mu.Unlock()
			return v, nil
		}
		lastErr = fmt.Errorf("%d of %d examples failed", len(failures), len(results))
	}
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
