// Command evolution runs the second layer end to end:
//
//	intent spec ─▶ compiler questions ─▶ replay gate (with simulators)
//	            ─▶ live evolution ─▶ automatic rewind ─▶ signed, verified ledger
//
//	go run ./cmd/evolution
package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
	"github.com/PureMunky/reversible/intent"
)

const sloppySpec = `{
  "goal": "maximise subscription revenue without driving customers away",
  "rules": [
    {"type": "card.charg", "param": "amount", "max": 5000},
    {"type": "account.purge", "forbid": true},
    {"type": "card.charge", "param": "amount", "min": 100},
    {"type": "card.charge", "param": "amount", "max": 80}
  ],
  "guardrails": [{"metric": "revenu", "no_drop_over_pct": 10}]
}`

const spec = `{
  "goal": "maximise subscription revenue without driving customers away",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "max": 5000, "why": "finance signs off above $5k"},
    {"type": "account.purge", "forbid": true, "why": "legal hold on customer data"},
    {"type": "card.charge", "requires": "email.send", "why": "every charge gets a receipt"}
  ],
  "guardrails": [
    {"metric": "revenue", "no_drop_over_pct": 10},
    {"metric": "churn", "no_rise_over_pct": 25}
  ],
  "policy": {"max_changed_pct": 100}
}`

// --- the world ---

type world struct {
	price  map[string]int
	emails int
}

func registry(w *world) *reversible.Registry {
	reg := reversible.NewRegistry()
	reg.MustRegister("plan.set", reversible.Handler{
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
	noop := func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }
	reg.MustRegister("card.charge", reversible.Handler{Class: reversible.Escrowed, Do: noop,
		Undo: func(context.Context, reversible.Params, reversible.Receipt) error { return nil }})
	reg.MustRegister("email.send", reversible.Handler{Class: reversible.Escrowed,
		Do: func(context.Context, reversible.Params) (reversible.Receipt, error) { w.emails++; return nil, nil }})
	reg.MustRegister("account.purge", reversible.Handler{Class: reversible.Irreversible, Do: noop})
	return reg
}

// customer traits, derived deterministically from the ID
func hash(s string) uint64 { h := fnv.New64a(); h.Write([]byte(s)); return h.Sum64() }

// sensitivity is what the business *thinks* it knows (it feeds the simulator).
func sensitivity(c string) float64 { return 0.5 + float64(hash(c)%100)/100 }

// threshold is the truth: the price above which this customer actually leaves.
func threshold(c string) int { return 50 + int(hash(c+"#")%40) }

// --- the model (what replay predicts) and reality (what live traffic does) ---

var simulators = counterfactual.Simulators{
	"card.charge": func(req counterfactual.Request, e counterfactual.PlannedEffect) map[string]float64 {
		price, _ := strconv.ParseFloat(e.Params["amount"], 64)
		churn := math.Min(1, sensitivity(req.Input["customer"])*price/1000)
		return map[string]float64{"revenue": price * (1 - churn), "churn": churn}
	},
}

func reality(customer string, price int) map[string]float64 {
	if price > threshold(customer) {
		return map[string]float64{"revenue": 0, "churn": 1}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

// --- programs ---

type variant struct {
	name    string
	price   int
	noEmail bool
	purge   bool
}

func (v variant) program(reg *reversible.Registry) counterfactual.Program {
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		c := req.Input["customer"]
		in := reversible.Intent{ID: "bill-" + req.ID, Goal: fmt.Sprintf("bill %s at $%d", c, v.price), Effects: []reversible.Effect{
			reg.MustEffect("set-price", "plan.set", reversible.Params{"customer": c, "price": strconv.Itoa(v.price)}),
			reg.MustEffect("charge", "card.charge", reversible.Params{"customer": c, "amount": strconv.Itoa(v.price)}),
		}}
		if !v.noEmail {
			in.Effects = append(in.Effects, reg.MustEffect("receipt", "email.send", reversible.Params{"to": c}))
		}
		if v.purge {
			in.Effects = append(in.Effects, reg.MustEffect("purge-lapsed", "account.purge", reversible.Params{"customer": c}))
		}
		return in, nil
	}
}

func main() {
	ctx := context.Background()
	head := func(s string) { fmt.Printf("\n━━━ %s\n", s) }
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	w := &world{price: map[string]int{}}
	reg := registry(w)
	known := intent.Known{Types: reg.Types(), Metrics: []string{"revenue", "churn"}}

	// ── 1. The spec, and what the compiler won't guess about ──────────────
	head("1 · Phil writes an intent spec. The first draft has problems")
	draft, err := intent.Parse([]byte(sloppySpec))
	check(err)
	for _, q := range intent.Compile(draft, known).Questions {
		fmt.Println("   ?", q)
	}
	head("   Phil answers the questions; the revised spec compiles cleanly")
	s, err := intent.Parse([]byte(spec))
	check(err)
	compiled := intent.Compile(s, known)
	if !compiled.Ready() {
		panic("spec not ready")
	}
	fmt.Printf("   %d invariants, %d guardrails, owner %s\n", len(compiled.Invariants), len(compiled.Guardrails), compiled.Owner)

	// ── 2. A signed ledger ─────────────────────────────────────────────────
	keys := decision.NewKeyring()
	for _, a := range []string{"phil", "pricing-agent", "evolve"} {
		_, err := keys.Generate(a)
		check(err)
	}
	store := decision.NewMemoryLog()
	chain, err := decision.NewChainLog(store, keys)
	check(err)
	g, err := decision.Open(chain)
	check(err)
	_, err = g.Goal("revenue", compiled.Goal, "phil")
	check(err)
	decide := func(id, summary, why string) {
		_, err := g.Decide(decision.Decision{ID: id, Parents: []string{"revenue"}, Summary: summary, Rationale: why, Actor: "pricing-agent"})
		check(err)
	}
	decide("price-55", "price at $55", "current list price")

	// ── 3. Population: champion serves, traffic is recorded ───────────────
	rt := reversible.New(reversible.WithRegistry(reg))
	champion := variant{name: "$55", price: 55}
	pop, err := evolve.New(evolve.Variant{Name: champion.name, Program: champion.program(reg), Decision: "price-55"}, evolve.Options{
		Gate:            compiled.Config(counterfactual.Config{Simulators: simulators}),
		Recorder:        counterfactual.NewMemoryRecorder(),
		ChallengerShare: 0.5,
		Objective:       evolve.Objective{Metric: "revenue", Maximize: true, MinLiftPct: 2},
		Guardrails:      compiled.Guardrails,
		MinSamples:      60,
		Ledger:          g, AutoRewind: true, Undoer: rt, Actor: "evolve",
	})
	check(err)

	n := 0
	serve := func(count int) {
		for i := 0; i < count; i++ {
			n++
			c := fmt.Sprintf("cust-%04d", n)
			req := counterfactual.Request{ID: fmt.Sprintf("req-%04d", n), Input: map[string]string{"customer": c}}
			v, _, err := pop.Serve(ctx, rt, req)
			check(err)
			price, _ := strconv.Atoi(strings.TrimPrefix(v.Name, "$"))
			check(pop.Observe(req.ID, reality(c, price)))
		}
	}
	head("2 · The $55 champion serves 80 customers; their requests are recorded")
	serve(80)

	// ── 4. Proposals through the gate ─────────────────────────────────────
	head("3 · The pricing agent proposes five variants. Each must pass the replay gate")
	for _, v := range []variant{
		{name: "$55+purge", price: 55, purge: true},
		{name: "$55-no-receipt", price: 55, noEmail: true},
		{name: "$99", price: 99},
		{name: "$65", price: 65},
		{name: "$52", price: 52},
	} {
		id := "try-" + strings.NewReplacer("$", "", "+", "-").Replace(v.name)
		decide(id, "try "+v.name, "the agent's hypothesis")
		rep, err := pop.Propose(ctx, evolve.Variant{Name: v.name, Program: v.program(reg), Decision: id})
		check(err)
		if rep.Promote() {
			rev, churn := predicted(rep)
			fmt.Printf("   ✓ %-15s admitted   (predicted revenue %s, churn %s)\n", v.name, rev, churn)
		} else {
			fmt.Printf("   ✗ %-15s rejected   %s\n", v.name, rep.Blockers[0])
		}
	}

	// ── 5. Live traffic and evaluation ────────────────────────────────────
	head("4 · 600 more customers arrive; real outcomes are attributed to each variant")
	serve(600)
	for _, st := range pop.Status() {
		if st.Samples > 0 {
			fmt.Printf("   %-6s %-10s n=%-4d revenue/customer=$%5.2f  churn=%4.1f%%\n", st.Name, st.Status, st.Samples, st.Averages["revenue"], st.Averages["churn"]*100)
		}
	}
	held := 0
	for _, p := range w.price {
		if p == 65 {
			held++
		}
	}
	fmt.Printf("   customers currently on $65: %d\n", held)

	head("5 · Evaluate: outcomes decide")
	verdict, err := pop.Evaluate(ctx)
	check(err)
	fmt.Print(indent(verdict.String()))
	held = 0
	for _, p := range w.price {
		if p == 65 {
			held++
		}
	}
	fmt.Printf("   customers on $65 after auto-rewind: %d   (their charges and receipts were never released)\n", held)
	fmt.Printf("   champion is now %s\n", pop.Champion().Name)
	fmt.Println("   the simulator predicted $65 was safe; reality disagreed, and reality won")

	// ── 6. Accountability ─────────────────────────────────────────────────
	head("6 · Anyone with the public keys can verify who decided what")
	records, _ := store.Load()
	signers := map[string]int{}
	for _, r := range records {
		signers[r.Signer]++
	}
	err = decision.VerifyChain(records, keys.PublicKeys())
	fmt.Printf("   %d records, hash-chained and signed: phil=%d pricing-agent=%d evolve=%d\n",
		len(records), signers["phil"], signers["pricing-agent"], signers["evolve"])
	if err == nil {
		fmt.Println("   chain verifies ✓")
	}
	for i, r := range records {
		if r.Node != nil && r.Node.Kind == decision.KindDecision && r.Node.ID == "try-65" {
			records[i].Node.Rationale = "Phil told me to" // try to shift the blame
			break
		}
	}
	if err := decision.VerifyChain(records, keys.PublicKeys()); err != nil {
		fmt.Println("   after rewriting try-65's rationale:", err)
	}
	if fresh, _ := store.Load(); decision.VerifyChain(fresh, keys.PublicKeys()) == nil {
		fmt.Println("   (the stored log itself is untouched ✓)")
	}
}

func predicted(r counterfactual.Report) (string, string) {
	out := map[string]string{}
	for _, m := range r.Metrics {
		pct := m.Pct()
		out[m.Metric] = fmt.Sprintf("%+.1f%%", pct)
	}
	return out["revenue"], out["churn"]
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
