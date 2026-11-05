// Package reversible is a prototype effect system for agentic software.
//
// The core idea: agents should not need permission to act. They need every
// action to be declared up front and undoable by construction. Only the
// actions that truly cannot be undone are routed to a human.
//
// Every Effect belongs to one of three classes:
//
//   - Reversible:   runs immediately and carries a compensating Undo.
//   - Escrowed:     is held, not applied, until it is released (manually or
//     after a hold window). Cancelling a held effect costs nothing.
//   - Irreversible: cannot be undone, so it must be approved by a Gate
//     before the intent runs at all, and must come last in the intent.
package reversible

import (
	"context"
	"errors"
	"fmt"
)

// Class describes how (or whether) an effect can be undone.
type Class int

const (
	// Reversible effects execute immediately and must provide Undo.
	Reversible Class = iota
	// Escrowed effects are held and only applied on release.
	// Undo is optional and applies only after release.
	Escrowed
	// Irreversible effects cannot be undone and require Gate approval.
	Irreversible
)

func (c Class) String() string {
	switch c {
	case Reversible:
		return "reversible"
	case Escrowed:
		return "escrowed"
	case Irreversible:
		return "irreversible"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// Receipt is the data an effect returns when it runs, and the data its
// Undo needs to compensate it (an ID that was created, a prior value, etc.).
// Receipts are written to the ledger, so keep them small and non-secret.
type Receipt map[string]string

// Effect is a single declared action an agent wants to take on the world.
type Effect struct {
	Name        string
	Class       Class
	Description string
	Do          func(ctx context.Context) (Receipt, error)
	Undo        func(ctx context.Context, r Receipt) error
}

func (e Effect) validate() error {
	if e.Name == "" {
		return errors.New("effect has no name")
	}
	if e.Do == nil {
		return fmt.Errorf("effect %q has no Do", e.Name)
	}
	if e.Class == Reversible && e.Undo == nil {
		return fmt.Errorf("effect %q is reversible but has no Undo", e.Name)
	}
	if e.Class < Reversible || e.Class > Irreversible {
		return fmt.Errorf("effect %q has unknown class %v", e.Name, e.Class)
	}
	return nil
}

// Intent is a goal plus the complete set of effects that achieve it.
// Effects are declared before anything runs, so the runtime can check
// approvals and ordering up front instead of discovering problems halfway.
type Intent struct {
	ID      string
	Goal    string
	Effects []Effect
}

// Validate checks that the intent is well formed. Irreversible effects must
// come last, which keeps the window where a failure strands work as small
// as possible: everything before them can still be rolled back.
func (in Intent) Validate() error {
	if in.ID == "" {
		return errors.New("intent has no ID")
	}
	if len(in.Effects) == 0 {
		return fmt.Errorf("intent %q declares no effects", in.ID)
	}
	seen := map[string]bool{}
	sawIrreversible := false
	for _, e := range in.Effects {
		if err := e.validate(); err != nil {
			return fmt.Errorf("intent %q: %w", in.ID, err)
		}
		if seen[e.Name] {
			return fmt.Errorf("intent %q: duplicate effect name %q", in.ID, e.Name)
		}
		seen[e.Name] = true
		if e.Class == Irreversible {
			sawIrreversible = true
		} else if sawIrreversible {
			return fmt.Errorf("intent %q: %s effect %q follows an irreversible effect; irreversible effects must come last", in.ID, e.Class, e.Name)
		}
	}
	return nil
}
