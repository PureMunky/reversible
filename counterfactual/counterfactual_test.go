package counterfactual

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
)

// sideEffects counts how many times any real Do ran. Replay must keep it at 0.
var sideEffects int

func registry() *reversible.Registry {
	do := func(context.Context, reversible.Params) (reversible.Receipt, error) { sideEffects++; return nil, nil }
	undo := func(context.Context, reversible.Params, reversible.Receipt) error { return nil }
	reg := reversible.NewRegistry()
	reg.MustRegister("plan.set", reversible.Handler{Class: reversible.Reversible, Do: do, Undo: undo})
	reg.MustRegister("card.charge", reversible.Handler{Class: reversible.Escrowed, Do: do, Undo: undo})
	reg.MustRegister("account.purge", reversible.Handler{Class: reversible.Irreversible, Do: do})
	return reg
}

var prices = map[string]int{"starter": 49, "team": 199, "enterprise": 450}

func pricing(reg *reversible.Registry, priceFor func(tier string) int, purgeInactive bool) Program {
	return func(_ context.Context, req Request) (reversible.Intent, error) {
		tier := req.Input["tier"]
		if _, ok := prices[tier]; !ok {
			return reversible.Intent{}, fmt.Errorf("unknown tier %q", tier)
		}
		in := reversible.Intent{ID: "bill-" + req.ID, Goal: "bill " + req.Input["customer"], Effects: []reversible.Effect{
			reg.MustEffect("set-plan", "plan.set", reversible.Params{"customer": req.Input["customer"], "tier": tier}),
			reg.MustEffect("charge", "card.charge", reversible.Params{"customer": req.Input["customer"], "amount": strconv.Itoa(priceFor(tier))}),
		}}
		if purgeInactive && req.Input["active"] == "false" {
			in.Effects = append(in.Effects, reg.MustEffect("purge", "account.purge", reversible.Params{"customer": req.Input["customer"]}))
		}
		return in, nil
	}
}

var traffic = []Request{
	{ID: "r1", Input: map[string]string{"customer": "acme", "tier": "starter", "active": "true"}},
	{ID: "r2", Input: map[string]string{"customer": "globex", "tier": "team", "active": "true"}},
	{ID: "r3", Input: map[string]string{"customer": "initech", "tier": "enterprise", "active": "false"}},
	{ID: "r4", Input: map[string]string{"customer": "hooli", "tier": "starter", "active": "false"}},
}

var maxCharge = Invariant{Name: "max-charge-500", Check: func(_ Request, p Plan) error {
	for _, e := range p.Effects {
		if e.Type == "card.charge" {
			if n, _ := strconv.Atoi(e.Params["amount"]); n > 500 {
				return fmt.Errorf("charges $%d", n)
			}
		}
	}
	return nil
}}

func TestIdenticalCandidatePromotes(t *testing.T) {
	sideEffects = 0
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	rep := Replay(context.Background(), traffic, base, base, []Invariant{maxCharge}, Policy{})
	if !rep.Promote() || rep.Same != 4 || len(rep.Diffs) != 0 {
		t.Fatalf("report:\n%s", rep)
	}
	if sideEffects != 0 {
		t.Fatalf("replay touched the world %d times", sideEffects)
	}
}

func TestRiskyCandidateIsBlockedWithReasons(t *testing.T) {
	sideEffects = 0
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	// v2: enterprise price hike past the cap, and purges inactive accounts.
	cand := pricing(reg, func(t string) int {
		if t == "enterprise" {
			return 650
		}
		return prices[t]
	}, true)
	rep := Replay(context.Background(), traffic, base, cand, []Invariant{maxCharge}, Policy{})

	if rep.Promote() {
		t.Fatalf("expected block:\n%s", rep)
	}
	if rep.Changed != 2 || rep.Same != 2 || rep.NewViolations != 1 || rep.NewIrreversible != 2 {
		t.Fatalf("counts wrong:\n%s", rep)
	}
	out := rep.String()
	for _, want := range []string{
		"~ charge [card.charge] {amount=450, customer=initech}  →  charge [card.charge] {amount=650, customer=initech}",
		"+ purge [account.purge] {customer=hooli}",
		"⚠ new one-way door: purge",
		"✗ violates max-charge-500: charges $650",
		"introduces irreversible effects on 2 request(s)",
		"violates invariants on 1 request(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
	if sideEffects != 0 {
		t.Fatalf("replay touched the world %d times", sideEffects)
	}

	// Allowing new one-way doors removes that blocker but not the invariant one.
	rep = Replay(context.Background(), traffic, base, cand, []Invariant{maxCharge}, Policy{AllowNewIrreversible: true})
	if len(rep.Blockers) != 1 || !strings.Contains(rep.Blockers[0], "invariants") {
		t.Fatalf("blockers = %v", rep.Blockers)
	}
}

func TestInheritedViolationsDontBlock(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(string) int { return 999 }, false)
	cand := pricing(reg, func(string) int { return 999 }, false)
	rep := Replay(context.Background(), traffic, base, cand, []Invariant{maxCharge}, Policy{})
	if !rep.Promote() {
		t.Fatalf("a candidate shouldn't be blocked for the baseline's sins:\n%s", rep)
	}
	if !strings.Contains(rep.String(), "already violated by baseline") {
		t.Fatalf("inherited violations should still be shown:\n%s", rep)
	}
}

func TestCandidateErrorsPanicsAndInvalidIntentsBlock(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	cases := map[string]Program{
		"error": func(context.Context, Request) (reversible.Intent, error) {
			return reversible.Intent{}, errors.New("model timeout")
		},
		"panic": func(context.Context, Request) (reversible.Intent, error) { panic("nil map") },
		"invalid": func(_ context.Context, req Request) (reversible.Intent, error) {
			// One-way door before a reversible effect: the runtime would refuse it.
			return reversible.Intent{ID: "x", Effects: []reversible.Effect{
				reg.MustEffect("purge", "account.purge", nil),
				reg.MustEffect("set-plan", "plan.set", nil),
			}}, nil
		},
	}
	for name, cand := range cases {
		rep := Replay(context.Background(), traffic[:1], base, cand, nil, Policy{AllowNewIrreversible: true})
		if rep.Promote() || rep.CandidateErrors != 1 {
			t.Errorf("%s: expected block on candidate error:\n%s", name, rep)
		}
	}
}

func TestBlastRadiusLimit(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	cand := pricing(reg, func(t string) int { return prices[t] + 1 }, false)
	rep := Replay(context.Background(), traffic, base, cand, nil, Policy{MaxChangedFraction: 0.5})
	if rep.Promote() || !strings.Contains(rep.Blockers[0], "100% of requests") {
		t.Fatalf("blockers = %v", rep.Blockers)
	}
}

func TestRecordingThenReplayFromFile(t *testing.T) {
	sideEffects = 0
	path := filepath.Join(t.TempDir(), "traffic.jsonl")
	rec := NewFileRecorder(path)
	reg := registry()
	live := Recording(pricing(reg, func(t string) int { return prices[t] }, false), rec)
	for _, r := range traffic {
		if _, err := live(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := NewFileRecorder(path).Requests()
	if err != nil || len(got) != len(traffic) || got[2].Input["customer"] != "initech" {
		t.Fatalf("recorded = %v, %v", got, err)
	}
	rep := Replay(context.Background(), got, live, live, nil, Policy{})
	if rep.Same != 4 {
		t.Fatalf("report:\n%s", rep)
	}
	b, err := rep.JSON()
	if err != nil || !strings.Contains(string(b), `"Total": 4`) {
		t.Fatalf("json: %s %v", b, err)
	}
}
