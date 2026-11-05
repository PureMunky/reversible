package reversible

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Status is the overall state of an intent.
type Status string

const (
	// StatusRunning: effects are being applied right now. An intent found
	// in this state during recovery was interrupted by a crash.
	StatusRunning Status = "running"
	// StatusCommitted: every effect has been applied.
	StatusCommitted Status = "committed"
	// StatusPending: reversible effects applied, escrowed effects still held.
	StatusPending Status = "pending"
	// StatusRolledBack: the intent was undone (by failure or by request) and
	// nothing it did remains in the world.
	StatusRolledBack Status = "rolled_back"
	// StatusPartial: the intent was undone, but some effects could not be
	// (stranded) or may or may not have happened (uncertain).
	StatusPartial Status = "partially_rolled_back"
	// StatusRejected: a gate denied an irreversible effect, so nothing ran.
	StatusRejected Status = "rejected"
)

// StepState is the state of one effect within an intent.
type StepState string

const (
	StepPending     StepState = "pending"
	StepInFlight    StepState = "in_flight" // Do has been called and not yet returned
	StepDone        StepState = "done"
	StepHeld        StepState = "held"
	StepCancelled   StepState = "cancelled"
	StepCompensated StepState = "compensated"
	StepFailed      StepState = "failed"
	StepStranded    StepState = "stranded"  // happened, can't be undone
	StepUncertain   StepState = "uncertain" // interrupted mid-Do; outcome unknown
)

var (
	// ErrUnknownIntent is returned when an intent ID has not been executed.
	ErrUnknownIntent = errors.New("unknown intent")
	// ErrNotDurable is returned when a durable runtime is given an effect
	// that cannot be persisted (it was not built from a Registry).
	ErrNotDurable = errors.New("effect has no Spec; a runtime with a Store needs effects built from a Registry")
)

// StepReport describes one effect's outcome.
type StepReport struct {
	Effect    string
	Class     Class
	State     StepState
	ReleaseAt time.Time // set for held escrow
}

// Result describes an intent's outcome.
type Result struct {
	IntentID string
	Goal     string
	Status   Status
	Reason   string
	Steps    []StepReport
	// Recovered is true when this Recover call found the intent
	// interrupted mid-execution and rolled it back.
	Recovered bool
}

func (r *Result) inState(s StepState) []string {
	var out []string
	for _, st := range r.Steps {
		if st.State == s {
			out = append(out, st.Effect)
		}
	}
	return out
}

// Stranded lists effects that happened and could not be undone.
func (r *Result) Stranded() []string { return r.inState(StepStranded) }

// Uncertain lists effects interrupted by a crash whose outcome is unknown.
// These need a human (or a reconciler) to check the world.
func (r *Result) Uncertain() []string { return r.inState(StepUncertain) }

type step struct {
	effect    Effect
	state     StepState
	receipt   Receipt
	releaseAt time.Time
}

type intentState struct {
	mu     sync.Mutex
	intent Intent
	steps  []*step
	status Status
	reason string
}

// Runtime executes intents, keeps them undoable, and records everything.
type Runtime struct {
	ledger   Ledger
	gate     Gate
	holdFor  time.Duration
	now      func() time.Time
	store    Store
	registry *Registry

	mu      sync.Mutex
	intents map[string]*intentState
	order   []string
}

// Option configures a Runtime.
type Option func(*Runtime)

// WithLedger sets where events are recorded (default: in memory).
func WithLedger(l Ledger) Option { return func(r *Runtime) { r.ledger = l } }

// WithGate sets the approver for irreversible effects (default: DenyAll).
func WithGate(g Gate) Option { return func(r *Runtime) { r.gate = g } }

// WithHold sets how long escrowed effects are held before ReleaseDue
// applies them (default: 15 minutes).
func WithHold(d time.Duration) Option { return func(r *Runtime) { r.holdFor = d } }

// WithClock overrides the clock, mainly for tests.
func WithClock(now func() time.Time) Option { return func(r *Runtime) { r.now = now } }

// WithStore makes the runtime durable: intent state is written ahead of
// every effect, and Recover can resume it in a new process. A durable
// runtime only accepts effects built from a Registry.
func WithStore(s Store) Option { return func(r *Runtime) { r.store = s } }

// WithRegistry sets the registry Recover uses to rebuild effects.
func WithRegistry(reg *Registry) Option { return func(r *Runtime) { r.registry = reg } }

// New creates a Runtime.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		ledger:  NewMemoryLedger(),
		gate:    DenyAll,
		holdFor: 15 * time.Minute,
		now:     time.Now,
		intents: map[string]*intentState{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Ledger returns the runtime's ledger.
func (r *Runtime) Ledger() Ledger { return r.ledger }

// Execute validates an intent, collects approval for every irreversible
// effect before anything runs, then applies effects in order. Escrowed
// effects are held rather than applied. If any effect fails, everything
// already done is compensated in reverse order.
//
// The returned error is non-nil only if the intent was malformed, could not
// be persisted, or an effect failed; a gate denial is StatusRejected.
func (r *Runtime) Execute(ctx context.Context, in Intent) (*Result, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	if r.store != nil {
		for _, e := range in.Effects {
			if e.Spec == nil {
				return nil, fmt.Errorf("intent %q, effect %q: %w", in.ID, e.Name, ErrNotDurable)
			}
		}
	}

	st := &intentState{intent: in, status: StatusRunning}
	for _, e := range in.Effects {
		st.steps = append(st.steps, &step{effect: e, state: StepPending})
	}
	r.mu.Lock()
	if _, dup := r.intents[in.ID]; dup {
		r.mu.Unlock()
		return nil, fmt.Errorf("intent %q has already been executed", in.ID)
	}
	r.intents[in.ID] = st
	r.order = append(r.order, in.ID)
	r.mu.Unlock()

	st.mu.Lock()
	defer st.mu.Unlock()

	r.log(Event{IntentID: in.ID, Goal: in.Goal, Kind: EventDeclared})
	for _, e := range in.Effects {
		r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventDeclared, Detail: e.Description})
	}

	// Preflight: every one-way door is approved before the first step runs,
	// so a denial never leaves the world half-changed.
	for _, e := range in.Effects {
		if e.Class != Irreversible {
			continue
		}
		ok, reason, err := r.gate.Approve(ctx, in, e)
		if err != nil {
			ok, reason = false, "gate error: "+err.Error()
		}
		if !ok {
			r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventDenied, Detail: reason})
			st.status = StatusRejected
			st.reason = fmt.Sprintf("%s: %s", e.Name, reason)
			return r.report(st), r.persist(st)
		}
		r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventApproved, Detail: reason})
	}

	if err := r.persist(st); err != nil {
		st.status, st.reason = StatusRolledBack, "could not persist intent: "+err.Error()
		return r.report(st), fmt.Errorf("intent %q: %w", in.ID, err)
	}

	for _, s := range st.steps {
		e := s.effect
		if e.Class == Escrowed {
			s.state = StepHeld
			s.releaseAt = r.now().Add(r.holdFor)
			r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventHeld,
				Detail: "release at " + s.releaseAt.Format(time.RFC3339)})
			if err := r.persist(st); err != nil {
				r.rollback(ctx, st, "could not persist held escrow: "+err.Error())
				return r.report(st), fmt.Errorf("intent %q: %w", in.ID, err)
			}
			continue
		}
		if err := r.apply(ctx, st, s, EventExecuted); err != nil {
			return r.report(st), err
		}
	}

	st.status = r.liveStatus(st)
	return r.report(st), r.persist(st)
}

// Undo reverses an intent: held escrow is cancelled (it never happened),
// and applied effects are compensated in reverse order. Effects that cannot
// be compensated are reported as stranded.
func (r *Runtime) Undo(ctx context.Context, intentID, reason string) (*Result, error) {
	st, err := r.get(intentID)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	switch st.status {
	case StatusRolledBack, StatusPartial, StatusRejected:
		return r.report(st), nil
	}
	if reason == "" {
		reason = "undo requested"
	}
	r.rollback(ctx, st, reason)
	return r.report(st), nil
}

// Release applies every held escrowed effect of an intent now. If one
// fails, the whole intent is rolled back.
func (r *Runtime) Release(ctx context.Context, intentID string) (*Result, error) {
	st, err := r.get(intentID)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	err = r.release(ctx, st, false)
	return r.report(st), err
}

// ReleaseDue applies held escrowed effects whose hold window has passed,
// across all intents. Call it on a ticker.
func (r *Runtime) ReleaseDue(ctx context.Context) ([]*Result, error) {
	var results []*Result
	var errs []error
	for _, id := range r.ids() {
		st, _ := r.get(id)
		st.mu.Lock()
		if st.status == StatusPending && r.anyDue(st) {
			if err := r.release(ctx, st, true); err != nil {
				errs = append(errs, err)
			}
			results = append(results, r.report(st))
		}
		st.mu.Unlock()
	}
	return results, errors.Join(errs...)
}

// Status reports the current state of an intent.
func (r *Runtime) Status(intentID string) (*Result, error) {
	st, err := r.get(intentID)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return r.report(st), nil
}

// List reports every intent the runtime knows about, oldest first.
func (r *Runtime) List() []*Result {
	var out []*Result
	for _, id := range r.ids() {
		res, _ := r.Status(id)
		out = append(out, res)
	}
	return out
}

// Recover loads persisted intents into a fresh runtime, rebuilding their
// effects from the registry. Held escrow resumes waiting for release.
// An intent that was still running when the process died is treated as a
// failure: the effect that was mid-flight is marked uncertain (it may or
// may not have happened) and everything else it did is rolled back.
//
// Call Recover once at startup, before Execute.
func (r *Runtime) Recover(ctx context.Context) ([]*Result, error) {
	if r.store == nil || r.registry == nil {
		return nil, errors.New("recover needs both WithStore and WithRegistry")
	}
	snaps, loadErr := r.store.LoadAll()
	var errs []error
	if loadErr != nil {
		errs = append(errs, loadErr)
	}
	var results []*Result
	for _, snap := range snaps {
		st, err := r.rehydrate(snap)
		if err != nil {
			errs = append(errs, fmt.Errorf("intent %q: %w", snap.ID, err))
			continue
		}
		r.mu.Lock()
		if _, exists := r.intents[snap.ID]; exists {
			r.mu.Unlock()
			continue
		}
		r.intents[snap.ID] = st
		r.order = append(r.order, snap.ID)
		r.mu.Unlock()

		st.mu.Lock()
		wasRunning := st.status == StatusRunning
		if wasRunning {
			r.log(Event{IntentID: st.intent.ID, Kind: EventRecovered, Detail: "process stopped mid-intent; rolling back"})
			for _, s := range st.steps {
				if s.state == StepInFlight {
					s.state = StepUncertain
					r.log(Event{IntentID: st.intent.ID, Effect: s.effect.Name, Class: s.effect.Class.String(), Kind: EventUncertain,
						Detail: "interrupted mid-execution; outcome unknown, check the world"})
				}
			}
			r.rollback(ctx, st, "recovered after crash mid-execution")
		}
		res := r.report(st)
		res.Recovered = wasRunning
		results = append(results, res)
		st.mu.Unlock()
	}
	return results, errors.Join(errs...)
}

// --- internals ---

// apply runs one effect with write-ahead persistence: the step is saved as
// in-flight before Do is called, so a crash during Do is detectable.
func (r *Runtime) apply(ctx context.Context, st *intentState, s *step, okKind EventKind) error {
	e := s.effect
	prev := s.state
	s.state = StepInFlight
	if err := r.persist(st); err != nil {
		s.state = prev
		r.rollback(ctx, st, fmt.Sprintf("could not persist before %s: %v", e.Name, err))
		return fmt.Errorf("intent %q: persisting before %q: %w", st.intent.ID, e.Name, err)
	}
	rec, err := e.Do(ctx)
	if err != nil {
		s.state = StepFailed
		r.log(Event{IntentID: st.intent.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventFailed, Detail: err.Error()})
		r.rollback(ctx, st, fmt.Sprintf("%s failed: %v", e.Name, err))
		return fmt.Errorf("intent %q: effect %q failed: %w", st.intent.ID, e.Name, err)
	}
	s.state = StepDone
	s.receipt = rec
	r.log(Event{IntentID: st.intent.ID, Effect: e.Name, Class: e.Class.String(), Kind: okKind, Receipt: rec})
	return r.persist(st)
}

func (r *Runtime) release(ctx context.Context, st *intentState, onlyDue bool) error {
	if st.status != StatusPending {
		return nil
	}
	now := r.now()
	st.status = StatusRunning
	for _, s := range st.steps {
		if s.state != StepHeld || (onlyDue && now.Before(s.releaseAt)) {
			continue
		}
		if err := r.apply(ctx, st, s, EventReleased); err != nil {
			return err
		}
	}
	st.status = r.liveStatus(st)
	return r.persist(st)
}

// rollback walks the steps in reverse, cancelling held escrow and
// compensating applied effects.
func (r *Runtime) rollback(ctx context.Context, st *intentState, reason string) {
	id := st.intent.ID
	partial := false
	for i := len(st.steps) - 1; i >= 0; i-- {
		s := st.steps[i]
		e := s.effect
		switch s.state {
		case StepUncertain:
			partial = true
		case StepHeld:
			s.state = StepCancelled
			r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCancelled, Detail: reason})
		case StepDone:
			if e.Undo == nil {
				s.state = StepStranded
				partial = true
				r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventStranded,
					Detail: "no compensation available; " + reason})
				continue
			}
			if err := e.Undo(ctx, s.receipt); err != nil {
				s.state = StepStranded
				partial = true
				r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCompensationFailed, Detail: err.Error()})
				continue
			}
			s.state = StepCompensated
			r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCompensated, Receipt: s.receipt, Detail: reason})
		}
		// Persist as we go, so a crash mid-rollback doesn't compensate twice.
		_ = r.persist(st)
	}
	st.reason = reason
	if partial {
		st.status = StatusPartial
	} else {
		st.status = StatusRolledBack
	}
	_ = r.persist(st)
}

func (r *Runtime) rehydrate(snap IntentSnapshot) (*intentState, error) {
	in := Intent{ID: snap.ID, Goal: snap.Goal}
	st := &intentState{status: snap.Status, reason: snap.Reason}
	for _, ss := range snap.Steps {
		if ss.Spec == nil {
			return nil, fmt.Errorf("effect %q: %w", ss.Name, ErrNotDurable)
		}
		e, err := r.registry.build(ss.Name, ss.Description, *ss.Spec)
		if err != nil {
			return nil, fmt.Errorf("effect %q: %w", ss.Name, err)
		}
		if e.Class != ss.Class {
			return nil, fmt.Errorf("effect %q: registry class %s does not match stored class %s", ss.Name, e.Class, ss.Class)
		}
		in.Effects = append(in.Effects, e)
		st.steps = append(st.steps, &step{effect: e, state: ss.State, receipt: ss.Receipt, releaseAt: ss.ReleaseAt})
	}
	st.intent = in
	return st, nil
}

func (r *Runtime) persist(st *intentState) error {
	if r.store == nil {
		return nil
	}
	snap := IntentSnapshot{
		ID: st.intent.ID, Goal: st.intent.Goal, Status: st.status, Reason: st.reason, UpdatedAt: r.now(),
	}
	for _, s := range st.steps {
		snap.Steps = append(snap.Steps, StepSnapshot{
			Name: s.effect.Name, Description: s.effect.Description, Class: s.effect.Class,
			Spec: s.effect.Spec, State: s.state, Receipt: s.receipt, ReleaseAt: s.releaseAt,
		})
	}
	return r.store.Save(snap)
}

func (r *Runtime) anyDue(st *intentState) bool {
	now := r.now()
	for _, s := range st.steps {
		if s.state == StepHeld && !now.Before(s.releaseAt) {
			return true
		}
	}
	return false
}

func (r *Runtime) liveStatus(st *intentState) Status {
	for _, s := range st.steps {
		if s.state == StepHeld {
			return StatusPending
		}
	}
	return StatusCommitted
}

func (r *Runtime) report(st *intentState) *Result {
	res := &Result{IntentID: st.intent.ID, Goal: st.intent.Goal, Status: st.status, Reason: st.reason}
	for _, s := range st.steps {
		res.Steps = append(res.Steps, StepReport{Effect: s.effect.Name, Class: s.effect.Class, State: s.state, ReleaseAt: s.releaseAt})
	}
	return res
}

func (r *Runtime) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

func (r *Runtime) get(id string) (*intentState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.intents[id]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownIntent, id)
	}
	return st, nil
}

func (r *Runtime) log(e Event) {
	e.Time = r.now()
	// The ledger is an audit trail and best-effort here; the Store is what
	// recovery depends on, and it is written ahead of every effect.
	_ = r.ledger.Append(e)
}
