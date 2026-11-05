package evolve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PureMunky/reversible/counterfactual"
)

// Services served elsewhere: no programs, just routing and outcomes.
func canaryPop(t *testing.T, extra ...func(*Options)) *Population {
	t.Helper()
	opts := Options{
		ChallengerShare: 0.2,
		Objective:       Objective{Metric: "ok", Maximize: true, NonInferior: true},
		Guardrails:      nil,
		MinSamples:      50,
		MaxSamples:      200,
		Salt:            "canary-test",
	}
	for _, f := range extra {
		f(&opts)
	}
	p, err := New(Variant{Name: "v1"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Admit(Variant{Name: "v2"}); err != nil {
		t.Fatal(err)
	}
	return p
}

func drive(p *Population, from, n int, outcome func(variant string, i int) map[string]float64) map[string]int {
	served := map[string]int{}
	for i := from; i < from+n; i++ {
		id := fmt.Sprintf("req-%d", i)
		v := p.Assign(id)
		served[v.Name]++
		p.Observe(id, outcome(v.Name, i))
	}
	return served
}

func TestNonInferiorityNeedsMaxSamples(t *testing.T) {
	if _, err := New(Variant{Name: "v1"}, Options{Objective: Objective{Metric: "ok", NonInferior: true}}); err == nil {
		t.Fatal("expected error without MaxSamples")
	}
}

func TestAdmitAndAssignWithoutPrograms(t *testing.T) {
	p := canaryPop(t)
	served := drive(p, 0, 1000, func(string, int) map[string]float64 { return map[string]float64{"ok": 1} })
	if share := float64(served["v2"]) / 1000; share < 0.15 || share > 0.25 {
		t.Fatalf("canary share = %.2f", share)
	}
	if _, _, err := p.Serve(context.Background(), nil, reqFor("x")); err == nil || !strings.Contains(err.Error(), "use Assign") {
		t.Fatalf("Serve on a program-less variant: %v", err)
	}
	if err := p.Admit(Variant{Name: "v2"}); err == nil {
		t.Fatal("duplicate admit should fail")
	}
}

func TestHarmlessCanaryIsPromotedAfterEnoughEvidence(t *testing.T) {
	p := canaryPop(t)
	ok := func(string, int) map[string]float64 { return map[string]float64{"ok": 1, "error": 0} }
	drive(p, 0, 400, ok) // ~80 canary outcomes: past MinSamples, short of MaxSamples
	v, _ := p.Evaluate(context.Background())
	if v.Promoted != "" || len(v.Waiting) != 1 || !strings.Contains(v.Waiting[0].Note, "no harm detected so far") {
		t.Fatalf("too early: %s", v)
	}
	drive(p, 400, 1000, ok)
	v, _ = p.Evaluate(context.Background())
	if v.Promoted != "v2" || !v.NonInferior || !strings.Contains(v.String(), "no harm after") {
		t.Fatalf("verdict: %s", v)
	}
}

func TestHarmfulCanaryIsStoppedBeforeFullEvidence(t *testing.T) {
	p := canaryPop(t, func(o *Options) {
		o.Guardrails = guardrails()
	})
	out := func(v string, i int) map[string]float64 {
		if v == "v2" && i%5 == 0 { // the canary fails one request in five
			return map[string]float64{"ok": 0, "error": 1}
		}
		return map[string]float64{"ok": 1, "error": 0}
	}
	drive(p, 0, 400, out) // ~80 canary outcomes, well short of MaxSamples
	v, _ := p.Evaluate(context.Background())
	if len(v.Retired) != 1 || v.Retired[0].Kind != Breach || !strings.Contains(v.Retired[0].Reason, "observed error rises") {
		t.Fatalf("verdict: %s", v)
	}
	if v.Promoted != "" {
		t.Fatal("a harmful canary was promoted")
	}
	// Once retired, it gets no more traffic.
	if served := drive(p, 400, 500, out); served["v2"] != 0 {
		t.Fatalf("retired canary still served %d requests", served["v2"])
	}
}

func guardrails() []counterfactual.Guardrail {
	return []counterfactual.Guardrail{counterfactual.NoRiseOver("error", 0)}
}

func reqFor(id string) counterfactual.Request { return counterfactual.Request{ID: id} }
