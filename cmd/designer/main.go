// Command designer lets a model design the policy, not just tune a number.
//
// The champion charges everyone $55. The generator sees the spec, the
// customers' inputs and every trial's outcome, and proposes whole new
// policies as JSON; every one goes through the same gate, live test,
// statistics and rewind as anything else.
//
//	go run ./cmd/designer                       # offline stand-in "model"
//	ANTHROPIC_API_KEY=... go run ./cmd/designer # Claude designs the policy
//	go run ./cmd/designer -show-prompt          # print the first prompt
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
	"github.com/PureMunky/reversible/intent"
	"github.com/PureMunky/reversible/model"
	"github.com/PureMunky/reversible/policy"
	"github.com/PureMunky/reversible/synth"
)

const spec = `{
  "goal": "maximise revenue per customer without driving customers away",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "min": 30, "max": 100, "why": "pricing committee's approved range"},
    {"type": "card.charge", "requires": "email.send", "why": "every charge gets a receipt"},
    {"type": "account.purge", "forbid": true, "why": "legal hold on customer data"}
  ],
  "guardrails": [
    {"metric": "churn", "no_rise_over_pct": 50}
  ]
}`

func main() {
	showPrompt := flag.Bool("show-prompt", false, "print the first prompt sent to the model")
	rounds := flag.Int("rounds", 40, "maximum rounds")
	flag.Parse()
	ctx := context.Background()
	check := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}

	var m model.Completer = standIn{}
	who := "offline stand-in (a deterministic heuristic, not a language model; set ANTHROPIC_API_KEY to use Claude)"
	if a, ok := model.FromEnv(); ok {
		m = a
		name := a.Model
		if name == "" {
			name = model.DefaultModel
		}
		who = "Claude (" + name + ")"
	}

	reg := reversible.NewRegistry()
	noop := func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }
	reg.MustRegister("card.charge", reversible.Handler{Class: reversible.Escrowed, Do: noop,
		Undo: func(context.Context, reversible.Params, reversible.Receipt) error { return nil }})
	reg.MustRegister("email.send", reversible.Handler{Class: reversible.Escrowed, Do: noop})
	reg.MustRegister("account.purge", reversible.Handler{Class: reversible.Irreversible, Do: noop})

	s, err := intent.Parse([]byte(spec))
	check(err)
	compiled := intent.Compile(s, intent.Known{Types: reg.Types(), Metrics: []string{"revenue", "churn"}})
	if !compiled.Ready() {
		check(fmt.Errorf("spec: %v", compiled.Questions))
	}

	keys := decision.NewKeyring()
	for _, a := range []string{"phil", "designer", "evolve"} {
		_, err := keys.Generate(a)
		check(err)
	}
	store := decision.NewMemoryLog()
	chain, err := decision.NewChainLog(store, keys)
	check(err)
	g, err := decision.Open(chain)
	check(err)
	_, err = g.Goal("goal", s.Goal, "phil")
	check(err)
	_, err = g.Decide(decision.Decision{ID: "start", Parents: []string{"goal"}, Summary: "charge everyone $55", Actor: "phil"})
	check(err)

	start := flatPolicy(55)
	startProg, err := policyProgram(start, reg)
	check(err)
	rt := reversible.New(reversible.WithRegistry(reg))
	pop, err := evolve.New(evolve.Variant{Name: "flat-55", Program: startProg, Decision: "start"}, evolve.Options{
		Gate:            compiled.Config(counterfactual.Config{}),
		Recorder:        counterfactual.NewMemoryRecorder(),
		ChallengerShare: 0.5,
		Objective:       evolve.Objective{Metric: "revenue", Maximize: true, MinLiftPct: 1},
		Guardrails:      compiled.Guardrails,
		MinSamples:      150,
		MaxSamples:      3000,
		Salt:            "designer-demo",
		Ledger:          g, AutoRewind: true, Undoer: rt, Actor: "evolve",
	})
	check(err)

	var examples []counterfactual.Request
	for i := 1; i <= 4; i++ {
		examples = append(examples, counterfactual.Request{ID: "example", Input: customerAt(900000 + i).input()})
	}
	gen := &synth.ModelGenerator{Model: m, Spec: s, Registry: reg, Examples: examples, PerStep: 2,
		Metrics: []string{"revenue", "churn"}}
	gen.Register("flat-55", start)
	loop, err := synth.New(pop, gen, g, nil, synth.Options{Goal: "goal", Actor: "designer", MaxLive: 2})
	check(err)

	n := 0
	serve := func(count int) {
		for i := 0; i < count; i++ {
			n++
			c := customerAt(n)
			req := counterfactual.Request{ID: c.ID, Input: c.input()}
			v, _, err := pop.Serve(ctx, rt, req)
			check(err)
			check(pop.Observe(req.ID, c.outcome(plannedPrice(v.Program, req))))
		}
	}

	fmt.Println("designer:", who)
	fmt.Println("champion: flat-55 (everyone pays $55)")
	fmt.Println()
	serve(100)
	for round := 1; round <= *rounds; round++ {
		rep, err := loop.Step(ctx)
		if err != nil {
			fmt.Printf("round %2d  model error, retrying next round: %v\n", round, err)
		}
		if *showPrompt && round == 1 {
			fmt.Println(strings.Repeat("─", 72))
			fmt.Print(gen.Transcript()[0].Prompt)
			fmt.Println(strings.Repeat("─", 72))
		}
		if rep.Converged {
			fmt.Printf("round %2d  converged: the designer has nothing new to try and nothing is running\n", round)
			break
		}
		for _, t := range rep.Admitted {
			fmt.Printf("round %2d  ✓ %s\n           %s\n", round, t.Name, wrap(t.Rationale))
		}
		for _, t := range rep.Rejected {
			fmt.Printf("round %2d  ✗ %s rejected by the gate: %s\n", round, t.Name, firstBlocker(t.Reason))
		}
		serve(1000)
		v, err := pop.Evaluate(ctx)
		check(err)
		if v.Promoted != "" {
			fmt.Printf("round %2d  ★ PROMOTE %s over %s (%+.1f%%, p=%.2g)\n", round, v.Promoted, v.Dethroned, v.Lift, v.P)
		}
		for _, r := range v.Retired {
			line := fmt.Sprintf("round %2d  · retire %s (%s)", round, r.Variant, r.Kind)
			if r.Rewind != nil {
				line += fmt.Sprintf(": rewound %d charges", len(r.Rewind.Undone))
			}
			fmt.Println(line)
		}
	}

	champ := pop.Champion().Name
	cp, _ := gen.Policy(champ)
	fmt.Printf("\nchampion after %d customers: %s\n  %s\n", n, champ, cp.Summary())

	bestFlat, bestFlatV := 0, 0.0
	for p := 30; p <= 100; p++ {
		if v := trueValue(func(customer) int { return p }); v > bestFlatV {
			bestFlat, bestFlatV = p, v
		}
	}
	champProg, _ := policyProgram(cp, reg)
	fmt.Println("\ntrue expected revenue per customer (measured on 40,000 customers the system never saw):")
	fmt.Printf("  starting policy, flat $55           $%.2f\n", trueValue(func(customer) int { return 55 }))
	fmt.Printf("  best possible single price ($%d)    $%.2f   ← the ceiling for any price-only search\n", bestFlat, bestFlatV)
	fmt.Printf("  %-35s $%.2f\n", "designed policy ("+champ+")", trueValue(func(c customer) int {
		return plannedPrice(champProg, counterfactual.Request{ID: c.ID, Input: c.input()})
	}))
	fmt.Printf("  %-35s $%.2f\n", "per-segment optimum ($35/$45/$60)", trueValue(func(c customer) int {
		switch {
		case c.Tenure < 6:
			return 35
		case c.Tenure < 24:
			return 45
		}
		return 60
	}))

	records, _ := store.Load()
	verified := decision.VerifyChain(records, keys.PublicKeys()) == nil
	fmt.Printf("\nledger: %d signed records, %d model exchanges kept for audit", len(records), len(gen.Transcript()))
	if verified {
		fmt.Print(", chain verifies ✓")
	}
	fmt.Println()
}

func policyProgram(p policy.Policy, reg *reversible.Registry) (counterfactual.Program, error) {
	return policy.Compile(p, reg, "bill")
}

func plannedPrice(p counterfactual.Program, req counterfactual.Request) int {
	for _, e := range counterfactual.Simulate(context.Background(), p, req).Effects {
		if e.Type == "card.charge" {
			n, _ := strconv.Atoi(e.Params["amount"])
			return n
		}
	}
	return 0
}

func firstBlocker(reason string) string {
	if i := strings.Index(reason, ";"); i > 0 {
		return reason[:i]
	}
	return reason
}

func wrap(s string) string {
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	return s
}
