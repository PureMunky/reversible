package reversible

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
)

// Handler implements a named effect type. Unlike an Effect, whose Do and
// Undo are closures, a Handler takes its inputs as Params, so an effect
// built from it can be written to disk and rebuilt after a restart.
//
// Handlers should be idempotent: after a crash, recovery may compensate an
// effect whose outcome is unknown, or a caller may retry.
type Handler struct {
	Class Class
	// Describe optionally renders a human-readable description.
	Describe func(p Params) string
	Do       func(ctx context.Context, p Params) (Receipt, error)
	Undo     func(ctx context.Context, p Params, r Receipt) error
	// Reconcile optionally reports whether the effect happened, for steps
	// a crash left uncertain. See Effect.Reconcile.
	Reconcile func(ctx context.Context, p Params) (happened bool, r Receipt, err error)
}

// Registry maps effect type names to handlers.
type Registry struct {
	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{handlers: map[string]Handler{}} }

// Register adds an effect type.
func (r *Registry) Register(typ string, h Handler) error {
	if typ == "" {
		return errors.New("effect type has no name")
	}
	if h.Do == nil {
		return fmt.Errorf("effect type %q has no Do", typ)
	}
	if h.Class == Reversible && h.Undo == nil {
		return fmt.Errorf("effect type %q is reversible but has no Undo", typ)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[typ]; dup {
		return fmt.Errorf("effect type %q already registered", typ)
	}
	r.handlers[typ] = h
	return nil
}

// MustRegister is Register that panics on error, for use at startup.
func (r *Registry) MustRegister(typ string, h Handler) {
	if err := r.Register(typ, h); err != nil {
		panic(err)
	}
}

// Effect builds a named, serializable effect of a registered type.
func (r *Registry) Effect(name, typ string, p Params) (Effect, error) {
	return r.build(name, "", Spec{Type: typ, Params: p})
}

// MustEffect is Effect that panics on error.
func (r *Registry) MustEffect(name, typ string, p Params) Effect {
	e, err := r.Effect(name, typ, p)
	if err != nil {
		panic(err)
	}
	return e
}

func (r *Registry) build(name, description string, spec Spec) (Effect, error) {
	r.mu.RLock()
	h, ok := r.handlers[spec.Type]
	r.mu.RUnlock()
	if !ok {
		return Effect{}, fmt.Errorf("unknown effect type %q", spec.Type)
	}
	p := maps.Clone(spec.Params)
	if p == nil {
		p = Params{}
	}
	if description == "" && h.Describe != nil {
		description = h.Describe(p)
	}
	e := Effect{
		Name:        name,
		Class:       h.Class,
		Description: description,
		Spec:        &Spec{Type: spec.Type, Params: p},
		Do:          func(ctx context.Context) (Receipt, error) { return h.Do(ctx, p) },
	}
	if h.Undo != nil {
		e.Undo = func(ctx context.Context, rc Receipt) error { return h.Undo(ctx, p, rc) }
	}
	if h.Reconcile != nil {
		e.Reconcile = func(ctx context.Context) (bool, Receipt, error) { return h.Reconcile(ctx, p) }
	}
	return e, nil
}
