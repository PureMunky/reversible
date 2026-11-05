package reversible

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ledgerWorld is a fake downstream system keyed by idempotency key, the way
// a payment provider or job queue would be.
type ledgerWorld struct {
	accounts   map[string]bool
	workspaces map[string]string // idempotency key -> workspace ID
	keysSeen   []string
	reachable  bool
}

func newLedgerWorld() *ledgerWorld {
	return &ledgerWorld{accounts: map[string]bool{}, workspaces: map[string]string{}, reachable: true}
}

// crashMode: "" (normal), "before" (die before the world changes),
// "after" (die after the world changed but before Do returns).
func (w *ledgerWorld) registry(crashMode string, withReconcile bool) *Registry {
	reg := NewRegistry()
	reg.MustRegister("account.create", Handler{
		Class: Reversible,
		Do: func(_ context.Context, p Params) (Receipt, error) {
			w.accounts[p["name"]] = true
			return Receipt{"account": p["name"]}, nil
		},
		Undo: func(_ context.Context, _ Params, r Receipt) error { delete(w.accounts, r["account"]); return nil },
	})
	h := Handler{
		Class: Reversible,
		Do: func(ctx context.Context, p Params) (Receipt, error) {
			key := IdempotencyKey(ctx)
			w.keysSeen = append(w.keysSeen, key)
			if crashMode == "before" {
				panic("crash before effect")
			}
			w.workspaces[key] = "ws-" + p["name"]
			if crashMode == "after" {
				panic("crash after effect")
			}
			return Receipt{"workspace": w.workspaces[key]}, nil
		},
		Undo: func(ctx context.Context, _ Params, r Receipt) error {
			key := IdempotencyKey(ctx)
			if w.workspaces[key] != r["workspace"] {
				return errors.New("receipt does not match world")
			}
			delete(w.workspaces, key)
			return nil
		},
	}
	if withReconcile {
		h.Reconcile = func(ctx context.Context, _ Params) (bool, Receipt, error) {
			if !w.reachable {
				return false, nil, errors.New("provider unreachable")
			}
			ws, ok := w.workspaces[IdempotencyKey(ctx)]
			if !ok {
				return false, nil, nil
			}
			return true, Receipt{"workspace": ws}, nil
		}
	}
	reg.MustRegister("workspace.provision", h)
	return reg
}

func (w *ledgerWorld) intent(reg *Registry) Intent {
	return Intent{ID: "onboard-acme", Effects: []Effect{
		reg.MustEffect("create-account", "account.create", Params{"name": "acme"}),
		reg.MustEffect("provision", "workspace.provision", Params{"name": "acme"}),
	}}
}

// crashThenRecover runs the intent with a crashing registry, then recovers
// in a "new process" with a healthy one.
func (w *ledgerWorld) crashThenRecover(t *testing.T, crashMode string, withReconcile bool) (*Runtime, *Result, error) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now()
	crashing := w.registry(crashMode, withReconcile)
	rt1 := durableRuntime(t, dir, crashing, &now)
	func() {
		defer func() { _ = recover() }()
		rt1.Execute(ctx, w.intent(crashing))
	}()
	rt1.Close() // the previous process is gone; the OS would release its lock
	rt2 := durableRuntime(t, dir, w.registry("", withReconcile), &now)
	results, err := rt2.Recover(ctx)
	if len(results) != 1 {
		t.Fatalf("results = %v, err = %v", results, err)
	}
	return rt2, results[0], err
}

func TestIdempotencyKeyIsStable(t *testing.T) {
	w := newLedgerWorld()
	reg := w.registry("", true)
	now := time.Now()
	rt := durableRuntime(t, t.TempDir(), reg, &now)
	if _, err := rt.Execute(context.Background(), w.intent(reg)); err != nil {
		t.Fatal(err)
	}
	if len(w.keysSeen) != 1 || w.keysSeen[0] != "onboard-acme/provision" {
		t.Fatalf("keys = %v", w.keysSeen)
	}
	if IdempotencyKey(context.Background()) != "" {
		t.Fatal("key outside a step should be empty")
	}
}

func TestReconcileFindsEffectHappenedAndCompensates(t *testing.T) {
	w := newLedgerWorld()
	rt, res, err := w.crashThenRecover(t, "after", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRolledBack || len(res.Uncertain()) != 0 {
		t.Fatalf("status=%s uncertain=%v", res.Status, res.Uncertain())
	}
	if len(w.workspaces) != 0 || len(w.accounts) != 0 {
		t.Fatalf("world not clean: %+v", w)
	}
	got := kinds(t, rt, "onboard-acme")
	want := "provision:uncertain provision:reconciled provision:compensated create-account:compensated"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("ledger tail = %q\nwant suffix %q", got, want)
	}
}

func TestReconcileFindsEffectNeverHappened(t *testing.T) {
	w := newLedgerWorld()
	_, res, err := w.crashThenRecover(t, "before", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRolledBack {
		t.Fatalf("status = %s", res.Status)
	}
	for _, s := range res.Steps {
		if s.Effect == "provision" && s.State != StepCancelled {
			t.Fatalf("provision state = %s, want cancelled", s.State)
		}
	}
	if len(w.accounts) != 0 {
		t.Fatalf("account not compensated: %+v", w)
	}
}

func TestWithoutReconcileStepStaysUncertain(t *testing.T) {
	w := newLedgerWorld()
	_, res, _ := w.crashThenRecover(t, "after", false)
	if res.Status != StatusPartial || len(res.Uncertain()) != 1 {
		t.Fatalf("status=%s uncertain=%v", res.Status, res.Uncertain())
	}
	if len(w.workspaces) != 1 {
		t.Fatal("expected the orphaned workspace to remain for a human")
	}
}

func TestReconcileRetriesWhenWorldWasUnreachable(t *testing.T) {
	w := newLedgerWorld()
	w.reachable = false
	rt, res, err := w.crashThenRecover(t, "after", true)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err = %v", err)
	}
	if res.Status != StatusPartial || len(res.Uncertain()) != 1 {
		t.Fatalf("status=%s uncertain=%v", res.Status, res.Uncertain())
	}

	w.reachable = true
	res, err = rt.Reconcile(context.Background(), "onboard-acme")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRolledBack || len(w.workspaces) != 0 {
		t.Fatalf("after retry: status=%s world=%+v", res.Status, w)
	}
	if res.Reason != "recovered after crash mid-execution" {
		t.Fatalf("reason lost: %q", res.Reason)
	}
}
