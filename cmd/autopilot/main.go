// Command autopilot runs the whole system with no human in the loop after
// the spec is written: the synthesis loop proposes prices, the gate checks
// them against the spec, live traffic decides, harmful experiments are
// rewound, and every step is signed into the ledger.
//
//	go run ./cmd/autopilot
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
	"github.com/PureMunky/reversible/intent"
	"github.com/PureMunky/reversible/synth"
)

const spec = `{
  "goal": "find the price that maximises revenue per customer",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "min": 40, "max": 70, "why": "pricing committee's approved range"}
  ],
  "guardrails": [
    {"metric": "churn", "no_rise_over_pct": 50}
  ]
}`

func hash(s string) uint64 { h := fnv.New64a(); h.Write([]byte(s)); return h.Sum64() }

// reality: each customer leaves above a private threshold, uniform in
// [50, 89]. Nobody in the system knows this; the optimum is $50.
func reality(c string, price int) map[string]float64 {
	if price > 50+int(hash(c+"#")%40) {
		return map[string]float64{"revenue": 0, "churn": 1}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

type world struct{ price map[string]int }

func main() {
	ctx := context.Background()
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}
	w := &world{price: map[string]int{}}

	reg := reversible.NewRegistry()
	reg.MustRegister("card.charge", reversible.Handler{
		Class: reversible.Escrowed,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			w.price[p["customer"]], _ = strconv.Atoi(p["amount"])
			return nil, nil
		},
		Undo: func(_ context.Context, p reversible.Params, _ reversible.Receipt) error {
			delete(w.price, p["customer"])
			return nil
		},
	})
	program := func(price int) counterfactual.Program {
		return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
			return reversible.Intent{ID: "bill-" + req.ID, Effects: []reversible.Effect{
				reg.MustEffect("charge", "card.charge", reversible.Params{"customer": req.Input["customer"], "amount": strconv.Itoa(price)}),
			}}, nil
		}
	}

	s, err := intent.Parse([]byte(spec))
	check(err)
	compiled := intent.Compile(s, intent.Known{Types: reg.Types(), Metrics: []string{"revenue", "churn"}})
	if !compiled.Ready() {
		panic(fmt.Sprint(compiled.Questions))
	}

	keys := decision.NewKeyring()
	for _, a := range []string{"phil", "synth", "evolve"} {
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
	_, err = g.Decide(decision.Decision{ID: "start-55", Parents: []string{"goal"}, Summary: "start at $55", Actor: "phil"})
	check(err)

	rt := reversible.New(reversible.WithRegistry(reg))
	pop, err := evolve.New(evolve.Variant{Name: "$55", Program: program(55), Decision: "start-55"}, evolve.Options{
		Gate:            compiled.Config(counterfactual.Config{}),
		Recorder:        counterfactual.NewMemoryRecorder(),
		ChallengerShare: 0.5,
		Objective:       evolve.Objective{Metric: "revenue", Maximize: true},
		Guardrails:      compiled.Guardrails,
		MinSamples:      100,
		MaxSamples:      3000,
		Salt:            "autopilot-demo",
		Ledger:          g, AutoRewind: true, Undoer: rt, Actor: "evolve",
	})
	check(err)

	knob := synth.KnobFromSpec(s, "price", "card.charge", "amount", 3, 0, 1000)
	loop, err := synth.New(pop, synth.ParamSearch{
		Knobs:   []synth.Knob{knob},
		Factory: func(p synth.Point) counterfactual.Program { return program(int(p["price"])) },
		Name:    func(p synth.Point) string { return fmt.Sprintf("$%g", p["price"]) },
	}, g, synth.Point{"price": 55}, synth.Options{Goal: "goal", Actor: "synth"})
	check(err)

	n := 0
	serve := func(count int) {
		for i := 0; i < count; i++ {
			n++
			c := fmt.Sprintf("c%06d", n)
			req := counterfactual.Request{ID: c, Input: map[string]string{"customer": c}}
			v, _, err := pop.Serve(ctx, rt, req)
			check(err)
			price, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "$"))
			check(pop.Observe(req.ID, reality(c, price)))
		}
	}

	fmt.Printf("spec: %s, price %g–%g, churn may not rise >50%%\n", s.Goal, knob.Min, knob.Max)
	fmt.Println("start: champion $55. Nobody is told the true optimum ($50).")
	fmt.Println()
	serve(50)
	batch := 500
	for round := 1; round <= 60; round++ {
		rep, err := loop.Step(ctx)
		check(err)
		if rep.Converged {
			fmt.Printf("round %2d  converged: no untried neighbour of %s is left, nothing is running\n", round, pop.Champion().Name)
			break
		}
		var events []string
		for _, t := range rep.Admitted {
			events = append(events, "propose "+t.Name)
		}
		for _, t := range rep.Rejected {
			events = append(events, "gate rejects "+t.Name)
		}
		serve(batch)
		v, err := pop.Evaluate(ctx)
		check(err)
		if v.Promoted != "" {
			events = append(events, fmt.Sprintf("PROMOTE %s over %s (%+.1f%%, p=%.2g)", v.Promoted, v.Dethroned, v.Lift, v.P))
		}
		for _, r := range v.Retired {
			e := fmt.Sprintf("retire %s (%s)", r.Variant, r.Kind)
			if r.Rewind != nil {
				e += fmt.Sprintf(", rewound %d charges", len(r.Rewind.Undone))
			}
			events = append(events, e)
		}
		if len(events) > 0 {
			fmt.Printf("round %2d  [%6d customers]  champion %-4s  %s\n", round, n, pop.Champion().Name, strings.Join(events, "; "))
		}
	}

	fmt.Println("\ntrials:")
	for _, t := range loop.State().Trials {
		reason := t.Reason
		if len(reason) > 90 {
			reason = reason[:87] + "..."
		}
		fmt.Printf("  %-4s %-10s %s\n", t.Name, t.Status, reason)
	}

	records, _ := store.Load()
	signers := map[string]int{}
	for _, r := range records {
		signers[r.Signer]++
	}
	fmt.Printf("\nledger: %d signed records (phil %d, synth %d, evolve %d)", len(records), signers["phil"], signers["synth"], signers["evolve"])
	if decision.VerifyChain(records, keys.PublicKeys()) == nil {
		fmt.Print(", chain verifies ✓")
	}
	fmt.Println()
	why, err := g.Explain(firstDecision(loop))
	check(err)
	fmt.Println("\nwhy the first experiment happened:")
	fmt.Print(indent(why))
}

func firstDecision(l *synth.Loop) string { return l.State().Trials[1].Decision } // [0] is the starting champion

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
