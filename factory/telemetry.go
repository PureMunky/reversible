package factory

import (
	"fmt"
	"strings"
	"time"

	"github.com/PureMunky/reversible/telemetry"
)

// What the factory reports, so the path from intent to production can be
// watched from outside: every metric and event is about a spec, a version
// or a release, never about lines of code. docs/ARCHITECTURE.md
// ("Telemetry") lists them.
var (
	tel = telemetry.Default

	mBuilds     = tel.Counter("reversible_builds_total", "Builds by outcome: passed, failed (no attempt passed), cached (intent unchanged), blocked (the spec had blocking questions).", "service", "outcome")
	mAttempts   = tel.Counter("reversible_build_attempts_total", "Coding attempts by result: passed, examples_failed, compile_failed, coder_error.", "service", "coder", "result")
	mExamples   = tel.Counter("reversible_examples_total", "Example runs against builds, by result.", "service", "result")
	mBuildSecs  = tel.Histogram("reversible_build_seconds", "Time to produce a passing build (or give up), all attempts included.", []float64{1, 5, 15, 30, 60, 120, 300, 600}, "service")
	mGenLines   = tel.Gauge("reversible_generated_lines", "Lines of generated code per built version (a cache, never edited).", "service", "version")
	mReviews    = tel.Counter("reversible_reviews_total", "Pre-release reviews by outcome: clean, or blocked (unexplained changes or a mixed-version problem).", "service", "outcome")
	mChanges    = tel.Counter("reversible_review_changes_total", "Behaviour differences found by review, by whether the intent explains them (yes) or not (no).", "service", "check", "explained")
	mRequests   = tel.Counter("reversible_requests_total", "Production requests by serving version, role (live or canary), method, resource and status code.", "service", "version", "role", "method", "resource", "code")
	mReqSecs    = tel.Histogram("reversible_request_seconds", "Production request latency by serving version.", []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1}, "service", "version", "role")
	mLive       = tel.Gauge("reversible_live_version", "1 for the version serving production (the label says which).", "service", "version")
	mCanary     = tel.Gauge("reversible_canary_version", "1 for a version running as a canary.", "service", "version")
	mReleases   = tel.Counter("reversible_releases_total", "Changes to what's live: release, promote, rollback (a rewind put an earlier version back), stop.", "service", "action")
	mCanaryEval = tel.Counter("reversible_canary_evaluations_total", "Canary evaluations by result: running, promoted, stopped.", "service", "result")
	mFindings   = tel.Counter("reversible_findings_total", "Production findings (new refusals, server errors) by kind and caller.", "service", "kind", "caller")
)

func coderName(c Coder) string {
	n := fmt.Sprintf("%T", c)
	n = strings.TrimPrefix(n, "*")
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		n = n[i+1:]
	}
	return n
}

// resourceOf names the resource a request addresses, for labels: the
// first path segment, never a key (keys would explode cardinality).
func resourceOf(path string) string {
	p := strings.TrimPrefix(pathOnly(path), "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "/"
	}
	return p
}

func since(t time.Time) float64 { return time.Since(t).Seconds() }

func serviceOf(v *Version) string {
	if v == nil {
		return ""
	}
	return v.Spec.Service
}

// publishRouting updates the live and canary gauges and records what
// changed.
func (r *Router) publishRouting(action string, from, to *Version) {
	r.mu.Lock()
	live, canary := r.live, r.canary
	r.mu.Unlock()
	svc := serviceOf(to)
	if svc == "" {
		svc = serviceOf(from)
	}
	if live != nil {
		mLive.Only(1, live.Spec.Service, telemetry.Short(live.SpecHash))
	} else if svc != "" {
		mLive.Only(0, svc, "none")
	}
	if canary != nil {
		mCanary.Only(1, canary.Spec.Service, telemetry.Short(canary.SpecHash))
	} else if svc != "" {
		mCanary.Only(0, svc, "none")
	}
	f := telemetry.F{"service": svc}
	if from != nil {
		f["from"] = telemetry.Short(from.SpecHash)
	}
	if to != nil {
		f["to"] = telemetry.Short(to.SpecHash)
	}
	if svc != "" {
		mReleases.Inc(svc, action)
	}
	tel.Emit("routing."+action, f)
}
