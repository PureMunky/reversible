package synth

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
	"github.com/PureMunky/reversible/intent"
)

func h(s string) uint64 { x := fnv.New64a(); x.Write([]byte(s)); return x.Sum64() }

// Reality: each customer leaves above a private threshold, uniform in
// [50, 89]. Expected revenue per customer is maximised at $50.
func reality(c string, price int) map[string]float64 {
	if price > 50+int(h(c+"#")%40) {
		return map[string]float64{"revenue": 0, "churn": 1}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

type harness struct {
	pop  *evolve.Population
	loop *Loop
	rt   *reversible.Runtime
	g    *decision.Graph
	reg  *reversible.Registry
	n    int
}

func program(reg *reversible.Registry, price int) counterfactual.Program {
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		return reversible.Intent{ID: "bill-" + req.ID, Effects: []reversible.Effect{
			reg.MustEffect("charge", "card.charge", reversible.Params{"customer": req.Input["customer"], "amount": strconv.Itoa(price)}),
		}}, nil
	}
}

const specJSON = `{
  "goal": "maximise revenue per customer",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "min": 40, "max": 70}
  ],
  "guardrails": [{"metric": "churn", "no_rise_over_pct": 50}]
}`

func newHarness(t *testing.T, gen func(*reversible.Registry, Knob) Generator) *harness {
	t.Helper()
	reg := reversible.NewRegistry()
	noop := func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }
	reg.MustRegister("card.charge", reversible.Handler{Class: reversible.Escrowed, Do: noop,
		Undo: func(context.Context, reversible.Params, reversible.Receipt) error { return nil }})
	spec, err := intent.Parse([]byte(specJSON))
	if err != nil {
		t.Fatal(err)
	}
	compiled := intent.Compile(spec, intent.Known{Types: reg.Types(), Metrics: []string{"revenue", "churn"}})
	if !compiled.Ready() {
		t.Fatalf("spec: %v", compiled.Questions)
	}
	g, _ := decision.Open(decision.NewMemoryLog())
	g.Goal("goal", spec.Goal, "phil")
	g.Decide(decision.Decision{ID: "start-55", Parents: []string{"goal"}, Summary: "start at $55", Actor: "phil"})
	rt := reversible.New()
	pop, err := evolve.New(evolve.Variant{Name: "$55", Program: program(reg, 55), Decision: "start-55"}, evolve.Options{
		Gate:            compiled.Config(counterfactual.Config{}),
		Recorder:        counterfactual.NewMemoryRecorder(),
		ChallengerShare: 0.5,
		Objective:       evolve.Objective{Metric: "revenue", Maximize: true},
		Guardrails:      compiled.Guardrails,
		MinSamples:      100,
		MaxSamples:      3000,
		Salt:            "synth-test",
		Ledger:          g, AutoRewind: true, Undoer: rt, Actor: "evolve",
	})
	if err != nil {
		t.Fatal(err)
	}
	knob := KnobFromSpec(spec, "price", "card.charge", "amount", 3, 0, 1000)
	loop, err := New(pop, gen(reg, knob), g, Point{"price": 55}, Options{Goal: "goal", Actor: "synth"})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{pop: pop, loop: loop, rt: rt, g: g, reg: reg}
}

func paramSearch(reg *reversible.Registry, k Knob) Generator {
	return ParamSearch{
		Knobs:   []Knob{k},
		Factory: func(p Point) counterfactual.Program { return program(reg, int(p["price"])) },
		Name:    func(p Point) string { return fmt.Sprintf("$%g", p["price"]) },
	}
}

func (hn *harness) serve(t *testing.T, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		hn.n++
		c := fmt.Sprintf("c%06d", hn.n)
		req := counterfactual.Request{ID: c, Input: map[string]string{"customer": c}}
		v, _, err := hn.pop.Serve(context.Background(), hn.rt, req)
		if err != nil {
			t.Fatal(err)
		}
		price, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "$"))
		hn.pop.Observe(req.ID, reality(c, price))
	}
}

func TestKnobFromSpec(t *testing.T) {
	spec, _ := intent.Parse([]byte(specJSON))
	k := KnobFromSpec(spec, "price", "card.charge", "amount", 3, 0, 1000)
	if k.Min != 40 || k.Max != 70 || k.Step != 3 {
		t.Fatalf("knob = %+v", k)
	}
	if k := KnobFromSpec(spec, "x", "other.type", "amount", 1, 5, 9); k.Min != 5 || k.Max != 9 {
		t.Fatalf("fallback knob = %+v", k)
	}
}

func TestLoopConvergesNearTheOptimumWithoutLeavingTheSpec(t *testing.T) {
	hn := newHarness(t, paramSearch)
	ctx := context.Background()
	hn.serve(t, 50) // recorded traffic for the first gate

	converged := false
	for round := 0; round < 60 && !converged; round++ {
		rep, err := hn.loop.Step(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if converged = rep.Converged; converged {
			break
		}
		hn.serve(t, 500)
		if _, err := hn.pop.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !converged {
		t.Fatalf("did not converge; champion %s", hn.pop.Champion().Name)
	}
	champ, _ := strconv.Atoi(strings.TrimPrefix(hn.pop.Champion().Name, "$"))
	if champ < 47 || champ > 53 {
		t.Fatalf("converged at $%d, true optimum is $50", champ)
	}
	for _, tr := range hn.loop.State().Trials[1:] { // [0] is the starting champion
		if p := tr.Point["price"]; p < 40 || p > 70 {
			t.Fatalf("proposed %s outside the spec's bounds", tr.Name)
		}
		n, ok := hn.g.Get(tr.Decision)
		if !ok || n.Actor != "synth" || !strings.Contains(n.Rationale, "from champion") {
			t.Fatalf("trial %s not recorded as a decision: %+v", tr.Name, n)
		}
	}
}

func TestGateStillAppliesToGeneratedCandidates(t *testing.T) {
	// A generator that ignores the spec's bounds: the gate catches it.
	rogue := func(reg *reversible.Registry, _ Knob) Generator {
		return GeneratorFunc(func(_ context.Context, s State) ([]Candidate, error) {
			if s.Tried(Point{"price": 500}) {
				return nil, nil
			}
			return []Candidate{{Name: "$500", Point: Point{"price": 500}, Program: program(reg, 500), Rationale: "go big"}}, nil
		})
	}
	hn := newHarness(t, rogue)
	hn.serve(t, 20)
	rep, err := hn.loop.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rejected) != 1 || !strings.Contains(rep.Rejected[0].Reason, "violates invariants") {
		t.Fatalf("report: %+v", rep)
	}
	if rep2, _ := hn.loop.Step(context.Background()); !rep2.Converged {
		t.Fatal("expected convergence once the generator is out of ideas")
	}
}

func TestMaxLiveCapsConcurrentChallengers(t *testing.T) {
	hn := newHarness(t, paramSearch)
	hn.serve(t, 20)
	rep, _ := hn.loop.Step(context.Background())
	if len(rep.Admitted)+len(rep.Rejected) != 2 {
		t.Fatalf("first step: %+v", rep)
	}
	if rep, _ := hn.loop.Step(context.Background()); len(rep.Admitted)+len(rep.Rejected) != 0 {
		t.Fatalf("proposed past MaxLive: %+v", rep)
	}
}
