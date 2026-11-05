package evolve

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
)

// world: each served request sets a customer's price.
type world struct{ price map[string]int }

func registry(w *world) *reversible.Registry {
	reg := reversible.NewRegistry()
	reg.MustRegister("price.set", reversible.Handler{
		Class: reversible.Reversible,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			prev := w.price[p["customer"]]
			w.price[p["customer"]], _ = strconv.Atoi(p["price"])
			return reversible.Receipt{"prev": strconv.Itoa(prev)}, nil
		},
		Undo: func(_ context.Context, p reversible.Params, r reversible.Receipt) error {
			w.price[p["customer"]], _ = strconv.Atoi(r["prev"])
			return nil
		},
	})
	reg.MustRegister("account.purge", reversible.Handler{Class: reversible.Irreversible,
		Do: func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }})
	return reg
}

func priced(reg *reversible.Registry, price int) counterfactual.Program {
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		return reversible.Intent{ID: "price-" + req.ID, Effects: []reversible.Effect{
			reg.MustEffect("set-price", "price.set", reversible.Params{"customer": req.ID, "price": strconv.Itoa(price)}),
		}}, nil
	}
}

func purging(reg *reversible.Registry) counterfactual.Program {
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		return reversible.Intent{ID: "purge-" + req.ID, Effects: []reversible.Effect{
			reg.MustEffect("purge", "account.purge", reversible.Params{"customer": req.ID}),
		}}, nil
	}
}

// outcome: revenue is the price if they stay; customers leave if price > 60.
func outcome(price int) map[string]float64 {
	if price > 60 {
		return map[string]float64{"revenue": 0, "churn": 1}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

type fixture struct {
	pop *Population
	rt  *reversible.Runtime
	g   *decision.Graph
	reg *reversible.Registry
	w   *world
}

func newFixture(t *testing.T, auto bool) *fixture {
	t.Helper()
	w := &world{price: map[string]int{}}
	reg := registry(w)
	rt := reversible.New()
	g, _ := decision.Open(decision.NewMemoryLog())
	g.Goal("revenue", "maximise revenue without losing customers", "phil")
	g.Decide(decision.Decision{ID: "price-40", Parents: []string{"revenue"}, Summary: "price at 40", Actor: "pricing-agent"})
	pop, err := New(Variant{Name: "p40", Program: priced(reg, 40), Decision: "price-40"}, Options{
		Recorder:        counterfactual.NewMemoryRecorder(),
		ChallengerShare: 0.5,
		Objective:       Objective{Metric: "revenue", Maximize: true, MinLiftPct: 5},
		Guardrails:      []counterfactual.Guardrail{counterfactual.NoRiseOver("churn", 10)},
		MinSamples:      20,
		Ledger:          g, AutoRewind: auto, Undoer: rt, Actor: "evolve",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pop: pop, rt: rt, g: g, reg: reg, w: w}
}

func (f *fixture) serve(t *testing.T, from, n int, price func(Variant) int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		req := counterfactual.Request{ID: fmt.Sprintf("c%03d", i)}
		v, _, err := f.pop.Serve(context.Background(), f.rt, req)
		if err != nil {
			t.Fatal(err)
		}
		f.pop.Observe(req.ID, outcome(price(v)))
	}
}

func priceOf(v Variant) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "p"))
	return n
}

func (f *fixture) propose(t *testing.T, name string, prog counterfactual.Program) counterfactual.Report {
	t.Helper()
	f.g.Decide(decision.Decision{ID: "decide-" + name, Parents: []string{"revenue"}, Summary: "try " + name, Actor: "pricing-agent"})
	rep, err := f.pop.Propose(context.Background(), Variant{Name: name, Program: prog, Decision: "decide-" + name})
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestProposeNeedsTraffic(t *testing.T) {
	f := newFixture(t, false)
	if _, err := f.pop.Propose(context.Background(), Variant{Name: "p50", Program: priced(f.reg, 50)}); err == nil {
		t.Fatal("expected error with no recorded traffic")
	}
}

func TestGateRejectsUnsafeChallenger(t *testing.T) {
	f := newFixture(t, false)
	f.serve(t, 0, 10, priceOf)
	rep := f.propose(t, "purger", purging(f.reg))
	if rep.Promote() {
		t.Fatal("purging variant passed the gate")
	}
	for _, s := range f.pop.Status() {
		if s.Name == "purger" && s.Status != Rejected {
			t.Fatalf("purger status = %s", s.Status)
		}
	}
	// A rejected variant never gets traffic.
	f.serve(t, 100, 50, func(v Variant) int {
		if v.Name == "purger" {
			t.Fatal("rejected variant served traffic")
		}
		return priceOf(v)
	})
}

func TestRoutingIsStableAndSplit(t *testing.T) {
	f := newFixture(t, false)
	f.serve(t, 0, 5, priceOf)
	f.propose(t, "p50", priced(f.reg, 50))
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		req := counterfactual.Request{ID: fmt.Sprintf("r%d", i)}
		v := f.pop.Route(req)
		if f.pop.Route(req).Name != v.Name {
			t.Fatal("routing is not stable")
		}
		counts[v.Name]++
	}
	if share := float64(counts["p50"]) / 2000; share < 0.45 || share > 0.55 {
		t.Fatalf("challenger share = %.2f, want ~0.5 (%v)", share, counts)
	}
}

func TestBetterChallengerIsPromoted(t *testing.T) {
	f := newFixture(t, false)
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "p55", priced(f.reg, 55))

	f.serve(t, 10, 20, priceOf)
	if v, _ := f.pop.Evaluate(context.Background()); v.Promoted != "" || len(v.Waiting) != 1 {
		t.Fatalf("judged too early: %+v", v)
	}
	f.serve(t, 30, 100, priceOf)
	v, err := f.pop.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v.Promoted != "p55" || v.Dethroned != "p40" || v.Lift < 37 || v.Lift > 38 {
		t.Fatalf("verdict: %s", v)
	}
	if f.pop.Champion().Name != "p55" {
		t.Fatal("champion not updated")
	}
	// The evaluation is on the ledger, attributed to the challenger's decision.
	n, ok := f.g.Get("eval-2-p55")
	if !ok || n.Parents[0] != "decide-p55" || n.Metrics["revenue"] != 55 {
		t.Fatalf("outcome node: %+v", n)
	}
}

func TestGuardrailBreachRetiresAndAutoRewinds(t *testing.T) {
	f := newFixture(t, true)
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "p80", priced(f.reg, 80)) // more revenue per sale on paper; everyone leaves
	f.serve(t, 10, 120, priceOf)

	served80 := 0
	for c, p := range f.w.price {
		if p == 80 {
			served80++
			_ = c
		}
	}
	if served80 == 0 {
		t.Fatal("setup: challenger served nobody")
	}

	v, err := f.pop.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Retired) != 1 || v.Retired[0].Variant != "p80" || !strings.Contains(v.Retired[0].Reason, "observed churn rises") {
		t.Fatalf("verdict: %s", v)
	}
	if v.Retired[0].Rewind == nil || len(v.Retired[0].Rewind.Undone) != served80 {
		t.Fatalf("rewind: %+v (served %d)", v.Retired[0].Rewind, served80)
	}
	for c, p := range f.w.price {
		if p == 80 {
			t.Fatalf("customer %s still at the retired price", c)
		}
	}
	if n, _ := f.g.Get("decide-p80"); !n.Rewound() {
		t.Fatal("challenger decision not rewound")
	}
	if n, _ := f.g.Get("price-40"); n.Rewound() {
		t.Fatal("champion decision was rewound")
	}
	if f.pop.Champion().Name != "p40" {
		t.Fatal("champion changed")
	}
	// Retired variants stop getting traffic.
	f.serve(t, 500, 100, func(v Variant) int {
		if v.Name == "p80" {
			t.Fatal("retired variant served traffic")
		}
		return priceOf(v)
	})
}

func TestWithoutAutoRewindItSuggests(t *testing.T) {
	f := newFixture(t, false)
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "p80", priced(f.reg, 80))
	f.serve(t, 10, 120, priceOf)
	v, _ := f.pop.Evaluate(context.Background())
	if !strings.Contains(v.String(), "suggest rewinding decision decide-p80") {
		t.Fatalf("verdict: %s", v)
	}
	if n, _ := f.g.Get("decide-p80"); n.Rewound() {
		t.Fatal("rewound without AutoRewind")
	}
}
