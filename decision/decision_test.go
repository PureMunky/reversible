package decision

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
)

type world struct{ plans map[string]string }

func (w *world) setPlan(customer, plan string) reversible.Effect {
	var prev string
	return reversible.Effect{
		Name: "set-plan", Class: reversible.Reversible,
		Do: func(context.Context) (reversible.Receipt, error) {
			prev = w.plans[customer]
			w.plans[customer] = plan
			return reversible.Receipt{"prev": prev}, nil
		},
		Undo: func(_ context.Context, r reversible.Receipt) error {
			w.plans[customer] = r["prev"]
			return nil
		},
	}
}

func setup(t *testing.T, log Log) (*Graph, *reversible.Runtime, *world) {
	t.Helper()
	g, err := Open(log)
	if err != nil {
		t.Fatal(err)
	}
	return g, reversible.New(), &world{plans: map[string]string{"acme": "monthly", "globex": "monthly"}}
}

// buildStory records: goal -> annual-plan -> discount -> outcome, plus an
// unrelated sibling decision, with executions for the two pricing decisions.
func buildStory(t *testing.T, g *Graph, rt *reversible.Runtime, w *world) {
	t.Helper()
	ctx := context.Background()
	must := func(_ *Node, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(g.Goal("grow-revenue", "grow revenue 10% this quarter", "phil"))
	must(g.Decide(Decision{ID: "annual-plan", Parents: []string{"grow-revenue"}, Actor: "pricing-agent",
		Summary: "move customers to an annual plan", Rationale: "annual plans cut churn",
		Alternatives: []string{"raise monthly price", "do nothing"}}))
	must(g.Decide(Decision{ID: "annual-discount", Parents: []string{"annual-plan"}, Actor: "pricing-agent",
		Summary: "discount annual plan for globex", Rationale: "globex is price sensitive"}))
	must(g.Decide(Decision{ID: "new-onboarding", Parents: []string{"grow-revenue"}, Actor: "growth-agent",
		Summary: "shorten onboarding"}))

	if _, err := g.Execute(ctx, rt, "annual-plan", reversible.Intent{ID: "move-acme", Effects: []reversible.Effect{w.setPlan("acme", "annual")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(ctx, rt, "annual-plan", reversible.Intent{ID: "move-globex", Effects: []reversible.Effect{w.setPlan("globex", "annual")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Execute(ctx, rt, "annual-discount", reversible.Intent{ID: "discount-globex", Effects: []reversible.Effect{w.setPlan("globex", "annual-discounted")}}); err != nil {
		t.Fatal(err)
	}
	must(g.Observe(Outcome{ID: "q3-churn", Parents: []string{"annual-plan"}, Summary: "churn rose",
		Metrics: map[string]float64{"churn_pct": 12.5}}))
}

func TestRewindUndoesEverythingDownstreamNewestFirst(t *testing.T) {
	g, rt, w := setup(t, NewMemoryLog())
	buildStory(t, g, rt, w)
	if w.plans["globex"] != "annual-discounted" {
		t.Fatalf("setup: %v", w.plans)
	}

	rep, err := g.Rewind(context.Background(), "annual-plan", rt, "churn went up", "phil")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Clean() {
		t.Fatalf("rewind not clean: %+v", rep)
	}
	// Undone newest first, so globex is restored through both changes.
	if w.plans["acme"] != "monthly" || w.plans["globex"] != "monthly" {
		t.Fatalf("world not restored: %v", w.plans)
	}
	if got := strings.Join(rep.Rewound, ","); got != "q3-churn,annual-discount,annual-plan" {
		t.Fatalf("rewound = %s", got)
	}
	if len(rep.Undone) != 3 {
		t.Fatalf("undone = %v", rep.Undone)
	}
	// Siblings and the goal are untouched.
	if n, _ := g.Get("new-onboarding"); n.Rewound() {
		t.Fatal("unrelated decision was rewound")
	}
	if n, _ := g.Get("grow-revenue"); n.Rewound() {
		t.Fatal("goal was rewound")
	}
	n, _ := g.Get("annual-discount")
	if n.RewindOf != "annual-plan" || n.RewoundBy != "phil" {
		t.Fatalf("rewind metadata: %+v", n)
	}
}

func TestRewoundDecisionsCannotBeBuiltOn(t *testing.T) {
	g, rt, w := setup(t, NewMemoryLog())
	buildStory(t, g, rt, w)
	g.Rewind(context.Background(), "annual-plan", rt, "", "phil")
	if _, err := g.Decide(Decision{ID: "x", Parents: []string{"annual-plan"}, Summary: "x"}); err == nil {
		t.Fatal("expected error building on a rewound decision")
	}
	if err := g.Executed("annual-plan", "late-intent"); err == nil {
		t.Fatal("expected error executing a rewound decision")
	}
	// A replacement decision on the goal is fine.
	if _, err := g.Decide(Decision{ID: "raise-monthly", Parents: []string{"grow-revenue"}, Summary: "raise monthly price"}); err != nil {
		t.Fatal(err)
	}
}

func TestGraphValidation(t *testing.T) {
	g, _, _ := setup(t, NewMemoryLog())
	g.Goal("g", "goal", "")
	cases := map[string]error{}
	_, cases["no parent"] = g.Decide(Decision{ID: "d", Summary: "d"})
	_, cases["unknown parent"] = g.Decide(Decision{ID: "d", Parents: []string{"nope"}})
	g.Decide(Decision{ID: "d1", Parents: []string{"g"}})
	_, cases["duplicate"] = g.Decide(Decision{ID: "d1", Parents: []string{"g"}})
	g.Observe(Outcome{ID: "o", Parents: []string{"d1"}})
	_, cases["outcome as cause"] = g.Decide(Decision{ID: "d2", Parents: []string{"o"}})
	cases["execute a goal"] = g.Executed("g", "i")
	_, rewindErr := g.Rewind(context.Background(), "g", reversible.New(), "", "")
	cases["rewind a goal"] = rewindErr
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestExplainTracesWhy(t *testing.T) {
	g, rt, w := setup(t, NewMemoryLog())
	buildStory(t, g, rt, w)
	why, err := g.ExplainIntent("discount-globex")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"goal: grow revenue 10% this quarter (phil)",
		"decision: move customers to an annual plan (pricing-agent)",
		"because: annual plans cut churn",
		"instead of: raise monthly price; do nothing",
		"decision: discount annual plan for globex",
	} {
		if !strings.Contains(why, want) {
			t.Errorf("explain missing %q:\n%s", want, why)
		}
	}
	if strings.Contains(why, "shorten onboarding") {
		t.Errorf("explain included an unrelated branch:\n%s", why)
	}
}

func TestFileLogReplaysIncludingRewinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	g, rt, w := setup(t, NewFileLog(path))
	buildStory(t, g, rt, w)
	g.Rewind(context.Background(), "annual-discount", rt, "too generous", "phil")

	g2, err := Open(NewFileLog(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Nodes()) != 5 {
		t.Fatalf("nodes = %d", len(g2.Nodes()))
	}
	n, _ := g2.Get("annual-discount")
	if !n.Rewound() || n.RewindReason != "too generous" || len(n.Executions) != 1 {
		t.Fatalf("replayed node: %+v", n)
	}
	if p, _ := g2.Get("annual-plan"); p.Rewound() || len(p.Executions) != 2 {
		t.Fatalf("parent wrongly affected: %+v", p)
	}
	down, _ := g2.Downstream("annual-plan")
	if len(down) != 3 {
		t.Fatalf("downstream = %v", down)
	}
}

func TestRewindReportsStrandedEffects(t *testing.T) {
	g, rt, _ := setup(t, NewMemoryLog())
	g.Goal("g", "goal", "")
	g.Decide(Decision{ID: "announce", Parents: []string{"g"}, Summary: "announce price change"})
	email := reversible.Effect{Name: "email-all", Class: reversible.Escrowed,
		Do: func(context.Context) (reversible.Receipt, error) { return nil, nil }}
	ctx := context.Background()
	g.Execute(ctx, rt, "announce", reversible.Intent{ID: "announce-1", Effects: []reversible.Effect{email}})
	rt.Release(ctx, "announce-1")

	rep, err := g.Rewind(ctx, "announce", rt, "wrong price", "phil")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Clean() || len(rep.Stranded) != 1 || rep.Stranded[0] != "announce-1/email-all" {
		t.Fatalf("report: %+v", rep)
	}
}
