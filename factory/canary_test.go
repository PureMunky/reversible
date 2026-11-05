package factory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PureMunky/reversible/decision"
)

// appTraffic is what a flag service mostly sees: apps reading flags, with
// the occasional write.
func appTraffic(n, offset int) []HTTPCall {
	var out []HTTPCall
	for i := offset; i < offset+n; i++ {
		key := fmt.Sprintf("f%d", i%12)
		switch {
		case i%20 == 0:
			out = append(out, HTTPCall{Method: "PATCH", Path: "/flags/" + key, Body: map[string]any{"description": fmt.Sprint("note ", i)}, Caller: "web"})
		default:
			out = append(out, HTTPCall{Method: "GET", Path: "/flags/" + key, Caller: "app-servers"})
		}
	}
	return out
}

func seed(t *testing.T, p *prod) {
	t.Helper()
	for i := 0; i < 12; i++ {
		p.do(t, create(fmt.Sprintf("f%d", i), "web"))
	}
}

func ledger(t *testing.T) *decision.Graph {
	t.Helper()
	g, err := decision.Open(decision.NewMemoryLog())
	if err != nil {
		t.Fatal(err)
	}
	g.Goal("flags", "feature flags", "phil")
	g.Decide(decision.Decision{ID: "ship-v2", Parents: []string{"flags"}, Summary: "release v2", Actor: "phil"})
	return g
}

func TestHarmlessCanaryIsPromoted(t *testing.T) {
	ctx := context.Background()
	v1, v2 := build(t, spec(t, "flags-v1.json")), build(t, spec(t, "flags-v2.json"))
	p := newProd(t)
	defer p.close()
	p.release(t, "r1", v1)
	seed(t, p)

	rev, err := ReviewCandidate(ctx, p.router, v2)
	if err != nil {
		t.Fatal(err)
	}
	if !rev.Clean() {
		t.Fatalf("review:\n%s", rev)
	}

	g := ledger(t)
	c, err := StartCanary(ctx, p.router, v2, g, p.rt, "ship-v2", CanaryOptions{MinSamples: 20, MaxSamples: 60, Salt: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if p.router.Canary() != v2 || p.router.Live() != v1 {
		t.Fatal("canary not running beside v1")
	}
	var res CanaryResult
	for round := 0; round < 10 && !res.Promoted && !res.Stopped; round++ {
		p.do(t, appTraffic(100, round*100)...)
		if res, err = c.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !res.Promoted {
		t.Fatalf("not promoted: %s", res)
	}
	if p.router.Live() != v2 || p.router.Canary() != nil {
		t.Fatal("v2 should be live with no canary")
	}
	served := map[string]int{}
	for _, ex := range p.router.Traffic() {
		served[ex.Version]++
	}
	if served[v2.SpecHash] == 0 || served[v1.SpecHash] <= served[v2.SpecHash] {
		t.Fatalf("traffic split: %v", served)
	}
	// The promotion is a release: Observe works from it, and rewinding the
	// decision puts v1 back.
	if m, ok := p.router.LastRelease(); !ok || m.Version != v2 || m.Previous != v1 {
		t.Fatalf("release mark: %+v", m)
	}
	if _, err := g.Rewind(ctx, "ship-v2", p.rt, "test", "phil"); err != nil {
		t.Fatal(err)
	}
	if p.router.Live() != v1 {
		t.Fatal("rewinding the decision should put v1 back")
	}
}

// brittle fails every single-flag read: a defect no example covers,
// because the examples only read flags right after writing them... and it
// fails only when the flag has a description.
type brittle struct{}

func (brittle) Code(ctx context.Context, s ServiceSpec, prev map[string]string, f []Result) (map[string]string, error) {
	files, err := Codegen{}.Code(ctx, s, prev, f)
	if err == nil {
		files["main.go"] = strings.Replace(files["main.go"], `		reply(w, 200, rec)
	})

	mux.HandleFunc("PATCH`, `		if rec.Description != "" {
			fail(w, 500, "internal error")
			return
		}
		reply(w, 200, rec)
	})

	mux.HandleFunc("PATCH`, 1)
	}
	return files, err
}

func TestHarmfulCanaryIsStoppedAndRewound(t *testing.T) {
	ctx := context.Background()
	v1 := build(t, spec(t, "flags-v1.json"))
	bad, err := (&Factory{Root: t.TempDir(), Coder: brittle{}}).Build(ctx, spec(t, "flags-v2.json"))
	if err != nil {
		t.Fatalf("the defect is invisible to the examples: %v", err)
	}
	p := newProd(t)
	defer p.close()
	p.release(t, "r1", v1)
	seed(t, p)
	if rev, _ := ReviewCandidate(ctx, p.router, bad); !rev.Clean() {
		t.Fatalf("the defect should be invisible to the review too (no descriptions yet):\n%s", rev)
	}

	g := ledger(t)
	c, err := StartCanary(ctx, p.router, bad, g, p.rt, "ship-v2", CanaryOptions{MinSamples: 20, MaxSamples: 400, Salt: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var res CanaryResult
	for round := 0; round < 10 && !res.Promoted && !res.Stopped; round++ {
		p.do(t, appTraffic(100, round*100)...) // writes descriptions; the canary starts failing reads
		if res, err = c.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !res.Stopped || res.Promoted {
		t.Fatalf("harmful canary not stopped: %s", res)
	}
	if !strings.Contains(res.String(), "guardrail breach") {
		t.Fatalf("result: %s", res)
	}
	if p.router.Live() != v1 || p.router.Canary() != nil {
		t.Fatal("v1 should be the only version serving")
	}
	if n, _ := g.Get("ship-v2"); !n.Rewound() {
		t.Fatal("the decision behind the canary should be rewound")
	}
	// Blast radius: only the canary's share of traffic ever saw the defect.
	errs, total := 0, 0
	for _, ex := range p.router.Traffic() {
		total++
		if ex.Response.Status >= 500 {
			errs++
		}
	}
	if errs == 0 || float64(errs)/float64(total) > 0.25 {
		t.Fatalf("errors %d of %d requests", errs, total)
	}
	// After it's stopped, nothing fails.
	before := errs
	p.do(t, appTraffic(100, 2000)...)
	errs = 0
	for _, ex := range p.router.Traffic() {
		if ex.Response.Status >= 500 {
			errs++
		}
	}
	if errs != before {
		t.Fatalf("errors continued after the canary stopped: %d → %d", before, errs)
	}
}
