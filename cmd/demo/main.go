// Command demo walks through an agent onboarding customers against a fake
// world, showing escrow, undo, automatic rollback, and gated one-way doors.
//
//	go run ./cmd/demo            # irreversible effects denied (default)
//	go run ./cmd/demo -approve   # a human approves irreversible effects
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PureMunky/reversible"
)

type world struct {
	accounts map[string]bool
	charges  map[string]int
	outbox   []string
	legacy   int
}

func (w *world) String() string {
	var accts []string
	for a := range w.accounts {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	total := 0
	for _, c := range w.charges {
		total += c
	}
	return fmt.Sprintf("accounts=%v  charged=$%d  emails_sent=%d  legacy_records=%d", accts, total, len(w.outbox), w.legacy)
}

func (w *world) createAccount(name string) reversible.Effect {
	return reversible.Effect{
		Name: "create-account", Class: reversible.Reversible,
		Description: "create account " + name,
		Do: func(context.Context) (reversible.Receipt, error) {
			w.accounts[name] = true
			return reversible.Receipt{"account": name}, nil
		},
		Undo: func(_ context.Context, r reversible.Receipt) error {
			delete(w.accounts, r["account"])
			return nil
		},
	}
}

func (w *world) charge(name string, amount int) reversible.Effect {
	return reversible.Effect{
		Name: "charge-card", Class: reversible.Escrowed,
		Description: fmt.Sprintf("charge %s $%d", name, amount),
		Do: func(context.Context) (reversible.Receipt, error) {
			w.charges[name] += amount
			return reversible.Receipt{"account": name, "amount": fmt.Sprint(amount)}, nil
		},
		Undo: func(_ context.Context, r reversible.Receipt) error {
			// A refund: compensable even after release.
			w.charges[r["account"]] -= amount
			return nil
		},
	}
}

func (w *world) welcome(name string) reversible.Effect {
	return reversible.Effect{
		Name: "send-welcome-email", Class: reversible.Escrowed,
		Description: "email " + name,
		Do: func(context.Context) (reversible.Receipt, error) {
			w.outbox = append(w.outbox, name)
			return reversible.Receipt{"to": name}, nil
		},
		// No Undo: once it leaves, it's gone. Escrow is its only safety net.
	}
}

func (w *world) provision(fail bool) reversible.Effect {
	return reversible.Effect{
		Name: "provision-workspace", Class: reversible.Reversible,
		Do: func(context.Context) (reversible.Receipt, error) {
			if fail {
				return nil, errors.New("quota exceeded")
			}
			return reversible.Receipt{"workspace": "ws_1"}, nil
		},
		Undo: func(context.Context, reversible.Receipt) error { return nil },
	}
}

func (w *world) purgeLegacy() reversible.Effect {
	return reversible.Effect{
		Name: "purge-legacy-records", Class: reversible.Irreversible,
		Description: "hard-delete migrated legacy records",
		Do: func(context.Context) (reversible.Receipt, error) {
			n := w.legacy
			w.legacy = 0
			return reversible.Receipt{"deleted": fmt.Sprint(n)}, nil
		},
	}
}

func main() {
	approve := flag.Bool("approve", false, "approve irreversible effects")
	flag.Parse()

	ctx := context.Background()
	w := &world{accounts: map[string]bool{}, charges: map[string]int{}, legacy: 1200}

	gate := reversible.DenyAll
	if *approve {
		gate = reversible.GateFunc(func(_ context.Context, in reversible.Intent, e reversible.Effect) (bool, string, error) {
			return true, "approved by Phil (demo -approve flag)", nil
		})
	}
	rt := reversible.New(reversible.WithGate(gate), reversible.WithHold(10*time.Minute))

	section := func(title string) { fmt.Printf("\n━━ %s\n", title) }
	show := func(res *reversible.Result, err error) {
		fmt.Printf("   status: %s", res.Status)
		if res.Reason != "" {
			fmt.Printf("  (%s)", res.Reason)
		}
		if s := res.Stranded(); len(s) > 0 {
			fmt.Printf("  stranded: %v", s)
		}
		if err != nil {
			fmt.Printf("\n   error: %v", err)
		}
		fmt.Printf("\n   world:  %s\n", w)
	}

	fmt.Printf("start:     %s\n", w)

	section("1. Agent onboards Acme: account created, charge + email held in escrow")
	show(rt.Execute(ctx, reversible.Intent{ID: "onboard-acme", Goal: "onboard Acme on the $49 plan",
		Effects: []reversible.Effect{w.createAccount("acme"), w.charge("acme", 49), w.welcome("acme")}}))

	section("2. Agent realises it picked the wrong plan and undoes it — no refund, no awkward email")
	show(rt.Undo(ctx, "onboard-acme", "wrong plan selected"))

	section("3. Agent onboards Globex and the escrow is released")
	rt.Execute(ctx, reversible.Intent{ID: "onboard-globex", Goal: "onboard Globex on the $99 plan",
		Effects: []reversible.Effect{w.createAccount("globex"), w.charge("globex", 99), w.welcome("globex")}})
	show(rt.Release(ctx, "onboard-globex"))

	section("4. Undo after release: the charge is refunded, the email can't be unsent")
	show(rt.Undo(ctx, "onboard-globex", "customer cancelled"))

	section("5. A step fails mid-intent: everything already done rolls back on its own")
	show(rt.Execute(ctx, reversible.Intent{ID: "onboard-initech", Goal: "onboard Initech",
		Effects: []reversible.Effect{w.createAccount("initech"), w.charge("initech", 49), w.provision(true)}}))

	section("6. Agent wants to purge legacy data: a one-way door, so the gate decides before anything runs")
	show(rt.Execute(ctx, reversible.Intent{ID: "cleanup", Goal: "finish migration",
		Effects: []reversible.Effect{w.createAccount("migration-marker"), w.purgeLegacy()}}))
	if !*approve {
		fmt.Println("   (re-run with -approve to let a human open this door)")
	}

	section("Ledger for onboard-acme (why and what, in order)")
	evs, _ := rt.Ledger().Events("onboard-acme")
	for _, e := range evs {
		subject := "intent"
		if e.Effect != "" {
			subject = e.Effect
		}
		detail := e.Detail
		if e.Goal != "" {
			detail = "goal: " + e.Goal
		}
		fmt.Printf("   %-20s %-12s %s\n", subject, e.Kind, strings.TrimSpace(detail))
	}
}
