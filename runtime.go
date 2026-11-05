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
	// StatusCommitted: every effect has been applied.
	StatusCommitted Status = "committed"
	// StatusPending: reversible effects applied, escrowed effects still held.
	StatusPending Status = "pending"
	// StatusRolledBack: the intent was undone (by failure or by request) and
	// nothing it did remains in the world.
	StatusRolledBack Status = "rolled_back"
	// StatusPartial: the intent was undone, but some effects could not be,
	// and are listed as stranded.
	StatusPartial Status = "partially_rolled_back"
	// StatusRejected: a gate denied an irreversible effect, so nothing ran.
	StatusRejected Status = "rejected"
)

// StepState is the state of one effect within an intent.
type StepState string

const (
	StepPending     StepState = "pending"
	StepDone        StepState = "done"
	StepHeld        StepState = "held"
	StepCancelled   StepState = "cancelled"
	StepCompensated StepState = "compensated"
	StepFailed      StepState = "failed"
	StepStranded    StepState = "stranded"
)

// ErrUnknownIntent is returned when an intent ID has not been executed.
var ErrUnknownIntent = errors.New("unknown intent")

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
	Status   Status
	Reason   string
	Steps    []StepReport
}

// Stranded lists effects that happened and could not be undone.
func (r *Result) Stranded() []string {
	var out []string
	for _, s := range r.Steps {
		if s.State == StepStranded {
			out = append(out, s.Effect)
		}
	}
	return out
}

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
	ledger  Ledger
	gate    Gate
	holdFor time.Duration
	now     func() time.Time

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
// The returned error is non-nil only if the intent was malformed or an
// effect failed; a gate denial is reported as StatusRejected.
func (r *Runtime) Execute(ctx context.Context, in Intent) (*Result, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	st := &intentState{intent: in}
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
			return r.report(st), nil
		}
		r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventApproved, Detail: reason})
	}

	for _, s := range st.steps {
		e := s.effect
		if e.Class == Escrowed {
			s.state = StepHeld
			s.releaseAt = r.now().Add(r.holdFor)
			r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventHeld,
				Detail: "release at " + s.releaseAt.Format(time.RFC3339)})
			continue
		}
		rec, err := e.Do(ctx)
		if err != nil {
			s.state = StepFailed
			r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventFailed, Detail: err.Error()})
			r.rollback(ctx, st, fmt.Sprintf("%s failed: %v", e.Name, err))
			return r.report(st), fmt.Errorf("intent %q: effect %q failed: %w", in.ID, e.Name, err)
		}
		s.state = StepDone
		s.receipt = rec
		r.log(Event{IntentID: in.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventExecuted, Receipt: rec})
	}

	st.status = r.liveStatus(st)
	return r.report(st), nil
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
	if err := r.release(ctx, st, false); err != nil {
		return r.report(st), err
	}
	return r.report(st), nil
}

// ReleaseDue applies held escrowed effects whose hold window has passed,
// across all intents. Call it on a ticker.
func (r *Runtime) ReleaseDue(ctx context.Context) ([]*Result, error) {
	r.mu.Lock()
	ids := append([]string(nil), r.order...)
	r.mu.Unlock()

	var results []*Result
	var errs []error
	for _, id := range ids {
		st, _ := r.get(id)
		st.mu.Lock()
		if st.status == StatusPending {
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

// --- internals ---

func (r *Runtime) release(ctx context.Context, st *intentState, onlyDue bool) error {
	if st.status != StatusPending {
		return nil
	}
	now := r.now()
	for _, s := range st.steps {
		if s.state != StepHeld || (onlyDue && now.Before(s.releaseAt)) {
			continue
		}
		e := s.effect
		rec, err := e.Do(ctx)
		if err != nil {
			s.state = StepFailed
			r.log(Event{IntentID: st.intent.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventFailed, Detail: err.Error()})
			r.rollback(ctx, st, fmt.Sprintf("release of %s failed: %v", e.Name, err))
			return fmt.Errorf("intent %q: releasing %q failed: %w", st.intent.ID, e.Name, err)
		}
		s.state = StepDone
		s.receipt = rec
		r.log(Event{IntentID: st.intent.ID, Effect: e.Name, Class: e.Class.String(), Kind: EventReleased, Receipt: rec})
	}
	st.status = r.liveStatus(st)
	return nil
}

// rollback walks the steps in reverse, cancelling held escrow and
// compensating applied effects.
func (r *Runtime) rollback(ctx context.Context, st *intentState, reason string) {
	id := st.intent.ID
	stranded := false
	for i := len(st.steps) - 1; i >= 0; i-- {
		s := st.steps[i]
		e := s.effect
		switch s.state {
		case StepHeld:
			s.state = StepCancelled
			r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCancelled, Detail: reason})
		case StepDone:
			if e.Undo == nil {
				s.state = StepStranded
				stranded = true
				r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventStranded,
					Detail: "no compensation available; " + reason})
				continue
			}
			if err := e.Undo(ctx, s.receipt); err != nil {
				s.state = StepStranded
				stranded = true
				r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCompensationFailed, Detail: err.Error()})
				continue
			}
			s.state = StepCompensated
			r.log(Event{IntentID: id, Effect: e.Name, Class: e.Class.String(), Kind: EventCompensated, Receipt: s.receipt, Detail: reason})
		}
	}
	st.reason = reason
	if stranded {
		st.status = StatusPartial
	} else {
		st.status = StatusRolledBack
	}
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
	res := &Result{IntentID: st.intent.ID, Status: st.status, Reason: st.reason}
	for _, s := range st.steps {
		res.Steps = append(res.Steps, StepReport{Effect: s.effect.Name, Class: s.effect.Class, State: s.state, ReleaseAt: s.releaseAt})
	}
	return res
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
	// The ledger is best-effort here; a production runtime would treat a
	// ledger write failure as fatal (write-ahead) before acting.
	_ = r.ledger.Append(e)
}
