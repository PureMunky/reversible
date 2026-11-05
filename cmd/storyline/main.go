// Command storyline runs all three foundations together on one story:
// a pricing agent makes a decision, acts on it, gets it wrong, rewinds it,
// and then proposes new pricing logic that is replayed against real traffic
// before it is allowed anywhere near customers.
//
//	go run ./cmd/storyline
package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
)

// --- the world: stands in for a billing system, payments, email ---

type world struct {
	plans   map[string]string
	charged map[string]int
	emails  []string
	purged  []string
}

func (w *world) String() string {
	var names []string
	for n := range w.plans {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s:%s/$%d", n, w.plans[n], w.charged[n]))
	}
	return fmt.Sprintf("%s  emails=%d", strings.Join(parts, "  "), len(w.emails))
}

func registry(w *world) *reversible.Registry {
	reg := reversible.NewRegistry()
	reg.MustRegister("plan.set", reversible.Handler{
		Class: reversible.Reversible,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			prev := w.plans[p["customer"]]
			w.plans[p["customer"]] = p["plan"]
			return reversible.Receipt{"prev": prev}, nil
		},
		Undo: func(_ context.Context, p reversible.Params, r reversible.Receipt) error {
			w.plans[p["customer"]] = r["prev"]
			return nil
		},
	})
	reg.MustRegister("card.charge", reversible.Handler{
		Class: reversible.Escrowed,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			n, _ := strconv.Atoi(p["amount"])
			w.charged[p["customer"]] += n
			return nil, nil
		},
		Undo: func(_ context.Context, p reversible.Params, _ reversible.Receipt) error {
			n, _ := strconv.Atoi(p["amount"])
			w.charged[p["customer"]] -= n // refund
			return nil
		},
	})
	reg.MustRegister("email.send", reversible.Handler{
		Class: reversible.Escrowed,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			w.emails = append(w.emails, p["to"]+": "+p["subject"])
			return nil, nil
		},
		// No Undo: you can't unsend an email.
	})
	reg.MustRegister("account.purge", reversible.Handler{
		Class: reversible.Irreversible,
		Do: func(_ context.Context, p reversible.Params) (reversible.Receipt, error) {
			w.purged = append(w.purged, p["customer"])
			return nil, nil
		},
	})
	return reg
}

// --- programs: how the agent turns a request into effects ---

type pricing struct {
	name          string
	price         map[string]int
	purgeInactive bool
}

func (p pricing) program(reg *reversible.Registry) counterfactual.Program {
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		c, tier := req.Input["customer"], req.Input["tier"]
		price, ok := p.price[tier]
		if !ok {
			return reversible.Intent{}, fmt.Errorf("no price for tier %q", tier)
		}
		plan := "annual-" + tier
		in := reversible.Intent{ID: "annual-" + c, Goal: fmt.Sprintf("move %s to %s", c, plan), Effects: []reversible.Effect{
			reg.MustEffect("set-plan", "plan.set", reversible.Params{"customer": c, "plan": plan}),
			reg.MustEffect("charge", "card.charge", reversible.Params{"customer": c, "amount": strconv.Itoa(price)}),
			reg.MustEffect("confirm-email", "email.send", reversible.Params{"to": c, "subject": "Welcome to annual"}),
		}}
		if p.purgeInactive && req.Input["active"] == "false" {
			in.Effects = append(in.Effects, reg.MustEffect("purge-inactive", "account.purge", reversible.Params{"customer": c}))
		}
		return in, nil
	}
}

func main() {
	ctx := context.Background()
	w := &world{
		plans:   map[string]string{"acme": "monthly", "globex": "monthly", "initech": "monthly", "hooli": "monthly"},
		charged: map[string]int{},
	}
	reg := registry(w)
	rt := reversible.New(reversible.WithRegistry(reg))
	g, _ := decision.Open(decision.NewMemoryLog())
	traffic := counterfactual.NewMemoryRecorder()

	v1 := pricing{name: "v1", price: map[string]int{"starter": 490, "team": 1990, "enterprise": 4500}}
	live := counterfactual.Recording(v1.program(reg), traffic)

	head := func(s string) { fmt.Printf("\n━━━ %s\n", s) }
	check := func(err error) {
		if err != nil {
			panic(err)
		}
	}

	fmt.Println("world:", w)

	// ── Act 1: decide, and act on the decision ─────────────────────────────
	head("1 · The pricing agent decides, and acts")
	_, err := g.Goal("grow-revenue", "grow revenue 10% this quarter", "phil")
	check(err)
	_, err = g.Decide(decision.Decision{ID: "annual-plans", Parents: []string{"grow-revenue"}, Actor: "pricing-agent",
		Summary:      "move monthly customers to annual plans",
		Rationale:    "annual billing pulls revenue forward and historically cuts churn",
		Alternatives: []string{"raise monthly prices 8%", "upsell add-ons"}})
	check(err)

	requests := []counterfactual.Request{
		{ID: "req-1", Input: map[string]string{"customer": "acme", "tier": "starter", "active": "true"}},
		{ID: "req-2", Input: map[string]string{"customer": "globex", "tier": "team", "active": "true"}},
		{ID: "req-3", Input: map[string]string{"customer": "initech", "tier": "enterprise", "active": "false"}},
		{ID: "req-4", Input: map[string]string{"customer": "hooli", "tier": "starter", "active": "false"}},
	}
	for _, req := range requests[:3] {
		in, err := live(ctx, req)
		check(err)
		_, err = g.Execute(ctx, rt, "annual-plans", in)
		check(err)
	}
	fmt.Println("   3 customers moved; charges and emails held in escrow")
	fmt.Println("   world:", w)

	rt.Release(ctx, "annual-acme") // acme's hold window passes: charge and email go out
	fmt.Println("   acme's escrow released")
	fmt.Println("   world:", w)

	_, err = g.Decide(decision.Decision{ID: "loyalty-credit", Parents: []string{"annual-plans"}, Actor: "pricing-agent",
		Summary: "give globex a loyalty credit", Rationale: "globex pushed back on the annual price"})
	check(err)
	in, _ := live(ctx, requests[3])
	_, err = g.Execute(ctx, rt, "annual-plans", in)
	check(err)
	_, err = g.Execute(ctx, rt, "loyalty-credit", reversible.Intent{ID: "credit-globex", Goal: "loyalty credit", Effects: []reversible.Effect{
		reg.MustEffect("set-plan", "plan.set", reversible.Params{"customer": "globex", "plan": "annual-team-credit"}),
	}})
	check(err)
	_, err = g.Observe(decision.Outcome{ID: "week-1", Parents: []string{"annual-plans"}, Summary: "support tickets spiked",
		Metrics: map[string]float64{"tickets": 41, "cancellations": 3}})
	check(err)
	fmt.Println("   world:", w)

	// ── Act 2: why? ─────────────────────────────────────────────────────────
	head("2 · Phil asks: why is globex on annual-team-credit?")
	why, err := g.ExplainIntent("credit-globex")
	check(err)
	fmt.Print(indent(why))

	// ── Act 3: rewind the decision ──────────────────────────────────────────
	head("3 · Phil rewinds the decision, not just an action")
	rep, err := g.Rewind(ctx, "annual-plans", rt, "support load too high", "phil")
	check(err)
	fmt.Printf("   rewound: %s\n", strings.Join(rep.Rewound, " ← "))
	ids := make([]string, 0, len(rep.Undone))
	for id := range rep.Undone {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Printf("   %-16s %s\n", id, rep.Undone[id].Status)
	}
	if len(rep.Stranded) > 0 {
		fmt.Printf("   couldn't undo (a human should follow up): %v\n", rep.Stranded)
	}
	fmt.Println("   world:", w)
	fmt.Println("   (acme's refund went out; the email it already received is the one thing that stays)")

	// ── Act 4: propose new logic, replay it against real traffic ───────────
	head("4 · The agent proposes v2 pricing. It's replayed against recorded traffic first")
	recorded, _ := traffic.Requests()
	maxCharge := counterfactual.Invariant{Name: "charge-at-most-5000", Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		for _, e := range p.Effects {
			if e.Type == "card.charge" {
				if n, _ := strconv.Atoi(e.Params["amount"]); n > 5000 {
					return fmt.Errorf("charges $%d", n)
				}
			}
		}
		return nil
	}}
	before := w.String()
	v2 := pricing{name: "v2", price: map[string]int{"starter": 490, "team": 1990, "enterprise": 6500}, purgeInactive: true}
	r2 := counterfactual.Replay(ctx, recorded, v1.program(reg), v2.program(reg), []counterfactual.Invariant{maxCharge}, counterfactual.Policy{})
	fmt.Print(indent(r2.String()))

	head("5 · The agent revises to v3 and tries again")
	v3 := pricing{name: "v3", price: map[string]int{"starter": 490, "team": 1790, "enterprise": 4500}}
	r3 := counterfactual.Replay(ctx, recorded, v1.program(reg), v3.program(reg), []counterfactual.Invariant{maxCharge}, counterfactual.Policy{MaxChangedFraction: 0.5})
	fmt.Print(indent(r3.String()))
	if w.String() == before {
		fmt.Println("   world unchanged by either replay ✓")
	}

	head("Decision ledger")
	for _, n := range g.Nodes() {
		status := ""
		if n.Rewound() {
			status = "  [rewound]"
		}
		fmt.Printf("   %-8s %-15s %s%s\n", n.Kind, n.ID, n.Summary, status)
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
