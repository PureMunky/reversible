package counterfactual

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
)

// A toy churn model: customers with high price sensitivity are likelier to
// leave as the charge rises. Revenue is the charge times the chance they stay.
var sims = Simulators{
	"card.charge": func(req Request, e PlannedEffect) map[string]float64 {
		amount, _ := strconv.ParseFloat(e.Params["amount"], 64)
		sens, _ := strconv.ParseFloat(req.Input["sensitivity"], 64)
		churn := math.Min(1, sens*amount/1000)
		return map[string]float64{"revenue": amount * (1 - churn), "churn": churn}
	},
	"account.purge": func(Request, PlannedEffect) map[string]float64 {
		return map[string]float64{"accounts": -1}
	},
}

var sensitiveTraffic = []Request{
	{ID: "r1", Input: map[string]string{"customer": "acme", "tier": "starter", "active": "true", "sensitivity": "0.2"}},
	{ID: "r2", Input: map[string]string{"customer": "globex", "tier": "team", "active": "true", "sensitivity": "1.5"}},
	{ID: "r3", Input: map[string]string{"customer": "initech", "tier": "enterprise", "active": "false", "sensitivity": "0.1"}},
}

func TestSimulatorsPredictConsequences(t *testing.T) {
	sideEffects = 0
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	// Raise team price: more charged per customer, but globex is sensitive.
	cand := pricing(reg, func(t string) int {
		if t == "team" {
			return 299
		}
		return prices[t]
	}, false)
	rep := ReplayWith(context.Background(), Config{
		Requests: sensitiveTraffic, Baseline: base, Candidate: cand, Simulators: sims,
		Guardrails: []Guardrail{NoDropOver("revenue", 5), NoRiseOver("churn", 25)},
	})

	m := map[string]MetricChange{}
	for _, mc := range rep.Metrics {
		m[mc.Metric] = mc
	}
	// globex: 199*(1-0.2985)=139.60 -> 299*(1-0.4485)=164.90; churn 0.2985 -> 0.4485
	if d := m["revenue"].Delta(); math.Abs(d-25.3) > 0.1 {
		t.Fatalf("revenue delta = %v\n%s", d, rep)
	}
	if d := m["churn"].Delta(); math.Abs(d-0.15) > 1e-9 {
		t.Fatalf("churn delta = %v", d)
	}
	if rep.Promote() || len(rep.Blockers) != 1 || !strings.Contains(rep.Blockers[0], "predicted churn rises") {
		t.Fatalf("blockers = %v\n%s", rep.Blockers, rep)
	}
	out := rep.String()
	for _, want := range []string{"predicted impact:", "Δ churn +0.15, revenue +25.3"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	if sideEffects != 0 {
		t.Fatal("simulation touched the world")
	}
}

func TestRevenueGuardrailAndNewMetrics(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	cand := pricing(reg, func(t string) int { return prices[t] / 2 }, true)
	rep := ReplayWith(context.Background(), Config{
		Requests: sensitiveTraffic, Baseline: base, Candidate: cand, Simulators: sims,
		Policy:     Policy{AllowNewIrreversible: true},
		Guardrails: []Guardrail{NoDropOver("revenue", 5), NoDropOver("accounts", 0)},
	})
	joined := strings.Join(rep.Blockers, "; ")
	if !strings.Contains(joined, "predicted revenue falls") || !strings.Contains(joined, "predicted accounts falls from zero") {
		t.Fatalf("blockers = %v\n%s", rep.Blockers, rep)
	}
}

func TestNoSimulatorsMeansNoMetrics(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	rep := Replay(context.Background(), sensitiveTraffic, base, base, nil, Policy{})
	if rep.Metrics != nil || strings.Contains(rep.String(), "predicted impact") {
		t.Fatalf("unexpected metrics: %+v", rep.Metrics)
	}
}
