package reversible

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// registryFor wires effect types to a world. Building a new registry over
// the same world simulates a process restart: the world (a database, a
// payment provider) outlives the process, the closures don't.
func registryFor(w *world, crashOn string) *Registry {
	reg := NewRegistry()
	reg.MustRegister("account.create", Handler{
		Class: Reversible,
		Do: func(_ context.Context, p Params) (Receipt, error) {
			if crashOn == "account.create" {
				panic("simulated crash")
			}
			w.accounts[p["name"]] = true
			return Receipt{"account": p["name"]}, nil
		},
		Undo: func(_ context.Context, _ Params, r Receipt) error {
			delete(w.accounts, r["account"])
			return nil
		},
	})
	reg.MustRegister("card.charge", Handler{
		Class:    Escrowed,
		Describe: func(p Params) string { return "charge " + p["name"] + " $" + p["amount"] },
		Do: func(context.Context, Params) (Receipt, error) {
			if crashOn == "card.charge" {
				panic("simulated crash")
			}
			w.charges++
			return Receipt{"charge": "ch_1"}, nil
		},
		Undo: func(context.Context, Params, Receipt) error { w.charges--; return nil },
	})
	reg.MustRegister("workspace.provision", Handler{
		Class: Reversible,
		Do: func(context.Context, Params) (Receipt, error) {
			if crashOn == "workspace.provision" {
				panic("simulated crash")
			}
			return Receipt{"ws": "1"}, nil
		},
		Undo: func(context.Context, Params, Receipt) error { return nil },
	})
	return reg
}

func onboard(reg *Registry, id, name string) Intent {
	return Intent{ID: id, Goal: "onboard " + name, Effects: []Effect{
		reg.MustEffect("create-account", "account.create", Params{"name": name}),
		reg.MustEffect("provision", "workspace.provision", nil),
		reg.MustEffect("charge-card", "card.charge", Params{"name": name, "amount": "49"}),
	}}
}

func durableRuntime(t *testing.T, dir string, reg *Registry, now *time.Time) *Runtime {
	t.Helper()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return New(WithStore(store), WithRegistry(reg), WithHold(10*time.Minute),
		WithClock(func() time.Time { return *now }))
}

func TestRegistryRejectsBadHandlers(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("x", Handler{Class: Reversible, Do: func(context.Context, Params) (Receipt, error) { return nil, nil }}); err == nil {
		t.Fatal("expected error for reversible handler without Undo")
	}
	if _, err := reg.Effect("e", "missing", nil); err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestDurableRuntimeRequiresSpecs(t *testing.T) {
	w := newWorld()
	now := time.Now()
	rt := durableRuntime(t, t.TempDir(), registryFor(w, ""), &now)
	_, err := rt.Execute(context.Background(), Intent{ID: "i1", Effects: []Effect{w.createAccount("a")}})
	if !errors.Is(err, ErrNotDurable) {
		t.Fatalf("err = %v, want ErrNotDurable", err)
	}
	if len(w.accounts) != 0 {
		t.Fatal("effect ran despite error")
	}
}

func TestEscrowSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := newWorld()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	rt1 := durableRuntime(t, dir, registryFor(w, ""), &now)
	if res, err := rt1.Execute(ctx, onboard(registryFor(w, ""), "i1", "acme")); err != nil || res.Status != StatusPending {
		t.Fatalf("execute: %v %+v", err, res)
	}

	// Process dies. A new one starts with a fresh registry over the same world.
	rt1.Close() // the previous process is gone; the OS would release its lock
	rt2 := durableRuntime(t, dir, registryFor(w, ""), &now)
	results, err := rt2.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != StatusPending {
		t.Fatalf("recovered: %+v", results)
	}

	now = now.Add(11 * time.Minute)
	if _, err := rt2.ReleaseDue(ctx); err != nil {
		t.Fatal(err)
	}
	if w.charges != 1 {
		t.Fatalf("charge not released after restart: %+v", w)
	}

	// And a third process can still undo it, using the persisted receipts.
	rt2.Close() // the previous process is gone; the OS would release its lock
	rt3 := durableRuntime(t, dir, registryFor(w, ""), &now)
	rt3.Recover(ctx)
	res, err := rt3.Undo(ctx, "i1", "customer cancelled")
	if err != nil || res.Status != StatusRolledBack {
		t.Fatalf("undo: %v %+v", err, res)
	}
	if w.charges != 0 || len(w.accounts) != 0 {
		t.Fatalf("world not clean: %+v", w)
	}
}

func TestCrashMidIntentRollsBackOnRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := newWorld()
	now := time.Now()

	// Crash while provisioning, after the account was created.
	crashing := registryFor(w, "workspace.provision")
	rt1 := durableRuntime(t, dir, crashing, &now)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated crash")
			}
		}()
		rt1.Execute(ctx, onboard(crashing, "i1", "acme"))
	}()
	if !w.accounts["acme"] {
		t.Fatal("setup: account should exist before recovery")
	}

	rt1.Close() // the previous process is gone; the OS would release its lock
	rt2 := durableRuntime(t, dir, registryFor(w, ""), &now)
	results, err := rt2.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := results[0]
	if !res.Recovered {
		t.Fatal("expected Recovered = true on the recovering call")
	}
	if res.Status != StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	if u := res.Uncertain(); len(u) != 1 || u[0] != "provision" {
		t.Fatalf("uncertain = %v", u)
	}
	if len(w.accounts) != 0 {
		t.Fatalf("completed effect not compensated on recovery: %+v", w)
	}
	if w.charges != 0 {
		t.Fatalf("escrow after the crash point should never run: %+v", w)
	}
	got := kinds(t, rt2, "i1")
	if !strings.Contains(got, "provision:uncertain") || !strings.Contains(got, "create-account:compensated") {
		t.Fatalf("ledger = %q", got)
	}

	// Recovering again is a no-op: nothing is compensated twice.
	rt2.Close() // the previous process is gone; the OS would release its lock
	rt3 := durableRuntime(t, dir, registryFor(w, ""), &now)
	results, _ = rt3.Recover(ctx)
	if results[0].Status != StatusPartial || results[0].Recovered {
		t.Fatalf("second recovery: status=%s recovered=%v", results[0].Status, results[0].Recovered)
	}
}

func TestRecoverFailsLoudlyOnUnknownType(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := newWorld()
	now := time.Now()
	rt1 := durableRuntime(t, dir, registryFor(w, ""), &now)
	rt1.Execute(ctx, onboard(registryFor(w, ""), "i1", "acme"))

	rt1.Close()                                        // the previous process is gone; the OS would release its lock
	rt2 := durableRuntime(t, dir, NewRegistry(), &now) // forgot to register types
	if _, err := rt2.Recover(ctx); err == nil || !strings.Contains(err.Error(), "unknown effect type") {
		t.Fatalf("err = %v", err)
	}
}

type failingStore struct{}

func (failingStore) Save(IntentSnapshot) error          { return errors.New("disk full") }
func (failingStore) LoadAll() ([]IntentSnapshot, error) { return nil, nil }

func TestNoEffectRunsIfWriteAheadFails(t *testing.T) {
	w := newWorld()
	reg := registryFor(w, "")
	rt := New(WithStore(failingStore{}), WithRegistry(reg))
	_, err := rt.Execute(context.Background(), onboard(reg, "i1", "acme"))
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if len(w.accounts) != 0 || w.charges != 0 {
		t.Fatalf("effects ran without being persisted first: %+v", w)
	}
}

func TestSnapshotIsReadable(t *testing.T) {
	dir := t.TempDir()
	w := newWorld()
	now := time.Now()
	reg := registryFor(w, "")
	rt := durableRuntime(t, dir, reg, &now)
	rt.Execute(context.Background(), onboard(reg, "team/acme", "acme"))
	b, err := os.ReadFile(dir + "/team%2Facme.intent.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"status": "pending"`, `"class": "escrowed"`, `"type": "card.charge"`, `"description": "charge acme $49"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("snapshot missing %s:\n%s", want, b)
		}
	}
}

func TestStoreLockExcludesSecondProcess(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	w := newWorld()
	now := time.Now()
	rt1 := durableRuntime(t, dir, registryFor(w, ""), &now)
	if _, err := rt1.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	rt2 := durableRuntime(t, dir, registryFor(w, ""), &now)
	if _, err := rt2.Recover(ctx); !errors.Is(err, ErrLocked) {
		t.Fatalf("second runtime: err = %v, want ErrLocked", err)
	}
	if err := rt1.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rt2.Recover(ctx); err != nil {
		t.Fatalf("after Close: %v", err)
	}
	rt2.Close()
}
