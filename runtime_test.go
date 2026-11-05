package reversible

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// world is a tiny fake system the effects act on.
type world struct {
	accounts map[string]bool
	charges  int
	emails   int
	purged   bool
}

func newWorld() *world { return &world{accounts: map[string]bool{}} }

func (w *world) createAccount(id string) Effect {
	return Effect{
		Name: "create-account", Class: Reversible,
		Do: func(context.Context) (Receipt, error) {
			w.accounts[id] = true
			return Receipt{"account": id}, nil
		},
		Undo: func(_ context.Context, r Receipt) error {
			delete(w.accounts, r["account"])
			return nil
		},
	}
}

func (w *world) charge() Effect {
	return Effect{
		Name: "charge-card", Class: Escrowed,
		Do:   func(context.Context) (Receipt, error) { w.charges++; return Receipt{"charge": "ch_1"}, nil },
		Undo: func(context.Context, Receipt) error { w.charges--; return nil },
	}
}

func (w *world) email() Effect {
	return Effect{
		Name: "send-welcome-email", Class: Escrowed,
		Do: func(context.Context) (Receipt, error) { w.emails++; return nil, nil },
		// No Undo: once an email is sent, it's sent.
	}
}

func (w *world) purge() Effect {
	return Effect{
		Name: "purge-legacy-data", Class: Irreversible,
		Do: func(context.Context) (Receipt, error) { w.purged = true; return nil, nil },
	}
}

func failing(name string) Effect {
	return Effect{
		Name: name, Class: Reversible,
		Do:   func(context.Context) (Receipt, error) { return nil, errors.New("boom") },
		Undo: func(context.Context, Receipt) error { return nil },
	}
}

func kinds(t *testing.T, rt *Runtime, id string) string {
	t.Helper()
	evs, err := rt.Ledger().Events(id)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, e := range evs {
		if e.Effect != "" {
			parts = append(parts, e.Effect+":"+string(e.Kind))
		}
	}
	return strings.Join(parts, " ")
}

func TestValidateRequiresUndoForReversible(t *testing.T) {
	in := Intent{ID: "x", Effects: []Effect{{Name: "a", Class: Reversible, Do: func(context.Context) (Receipt, error) { return nil, nil }}}}
	if err := in.Validate(); err == nil {
		t.Fatal("expected error for reversible effect without Undo")
	}
}

func TestValidateIrreversibleMustComeLast(t *testing.T) {
	w := newWorld()
	in := Intent{ID: "x", Effects: []Effect{w.purge(), w.createAccount("a")}}
	if err := in.Validate(); err == nil || !strings.Contains(err.Error(), "must come last") {
		t.Fatalf("expected ordering error, got %v", err)
	}
}

func TestEscrowHeldUntilRelease(t *testing.T) {
	w := newWorld()
	rt := New()
	res, err := rt.Execute(context.Background(), Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.charge(), w.email()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPending {
		t.Fatalf("status = %s, want pending", res.Status)
	}
	if !w.accounts["a"] || w.charges != 0 || w.emails != 0 {
		t.Fatalf("unexpected world before release: %+v", w)
	}
	res, err = rt.Release(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCommitted || w.charges != 1 || w.emails != 1 {
		t.Fatalf("after release: status=%s world=%+v", res.Status, w)
	}
}

func TestUndoBeforeReleaseLeavesNoTrace(t *testing.T) {
	w := newWorld()
	rt := New()
	ctx := context.Background()
	if _, err := rt.Execute(ctx, Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.charge(), w.email()}}); err != nil {
		t.Fatal(err)
	}
	res, err := rt.Undo(ctx, "i1", "wrong customer")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRolledBack {
		t.Fatalf("status = %s, want rolled_back", res.Status)
	}
	if len(w.accounts) != 0 || w.charges != 0 || w.emails != 0 {
		t.Fatalf("world not clean: %+v", w)
	}
	got := kinds(t, rt, "i1")
	want := "send-welcome-email:cancelled charge-card:cancelled create-account:compensated"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("ledger tail = %q, want suffix %q", got, want)
	}
}

func TestUndoAfterReleaseStrandsWhatCannotBeUndone(t *testing.T) {
	w := newWorld()
	rt := New()
	ctx := context.Background()
	rt.Execute(ctx, Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.charge(), w.email()}})
	rt.Release(ctx, "i1")
	res, _ := rt.Undo(ctx, "i1", "")
	if res.Status != StatusPartial {
		t.Fatalf("status = %s, want partial", res.Status)
	}
	if got := res.Stranded(); len(got) != 1 || got[0] != "send-welcome-email" {
		t.Fatalf("stranded = %v", got)
	}
	if w.charges != 0 || len(w.accounts) != 0 {
		t.Fatalf("compensable effects not undone: %+v", w)
	}
}

func TestFailureRollsBackAutomatically(t *testing.T) {
	w := newWorld()
	rt := New()
	res, err := rt.Execute(context.Background(), Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.charge(), failing("provision")}})
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Status != StatusRolledBack || len(w.accounts) != 0 || w.charges != 0 {
		t.Fatalf("status=%s world=%+v", res.Status, w)
	}
}

func TestIrreversibleDeniedByDefaultAndNothingRuns(t *testing.T) {
	w := newWorld()
	rt := New() // DenyAll
	res, err := rt.Execute(context.Background(), Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.purge()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusRejected {
		t.Fatalf("status = %s, want rejected", res.Status)
	}
	if len(w.accounts) != 0 || w.purged {
		t.Fatalf("effects ran despite denial: %+v", w)
	}
}

func TestIrreversibleApproved(t *testing.T) {
	w := newWorld()
	rt := New(WithGate(ApproveAll))
	res, err := rt.Execute(context.Background(), Intent{ID: "i1", Effects: []Effect{w.createAccount("a"), w.purge()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCommitted || !w.purged {
		t.Fatalf("status=%s world=%+v", res.Status, w)
	}
	res, _ = rt.Undo(context.Background(), "i1", "")
	if res.Status != StatusPartial || res.Stranded()[0] != "purge-legacy-data" {
		t.Fatalf("undo: status=%s stranded=%v", res.Status, res.Stranded())
	}
}

func TestReleaseDueRespectsHoldWindow(t *testing.T) {
	w := newWorld()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	rt := New(WithHold(10*time.Minute), WithClock(func() time.Time { return now }))
	ctx := context.Background()
	rt.Execute(ctx, Intent{ID: "i1", Effects: []Effect{w.charge()}})

	now = now.Add(5 * time.Minute)
	rt.ReleaseDue(ctx)
	if w.charges != 0 {
		t.Fatal("released before hold window elapsed")
	}
	now = now.Add(6 * time.Minute)
	rt.ReleaseDue(ctx)
	if w.charges != 1 {
		t.Fatal("not released after hold window elapsed")
	}
	if st, _ := rt.Status("i1"); st.Status != StatusCommitted {
		t.Fatalf("status = %s", st.Status)
	}
}

func TestDuplicateIntentRejected(t *testing.T) {
	w := newWorld()
	rt := New()
	in := Intent{ID: "i1", Effects: []Effect{w.createAccount("a")}}
	rt.Execute(context.Background(), in)
	if _, err := rt.Execute(context.Background(), in); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestFileLedgerRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	w := newWorld()
	rt := New(WithLedger(NewFileLedger(path)))
	ctx := context.Background()
	rt.Execute(ctx, Intent{ID: "i1", Goal: "onboard", Effects: []Effect{w.createAccount("a")}})
	rt.Execute(ctx, Intent{ID: "i2", Effects: []Effect{w.createAccount("b")}})

	evs, err := NewFileLedger(path).Events("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 || evs[0].Goal != "onboard" || evs[2].Receipt["account"] != "a" {
		t.Fatalf("unexpected events: %+v", evs)
	}
}

func TestUnknownIntent(t *testing.T) {
	if _, err := New().Undo(context.Background(), "nope", ""); !errors.Is(err, ErrUnknownIntent) {
		t.Fatalf("err = %v", err)
	}
}
