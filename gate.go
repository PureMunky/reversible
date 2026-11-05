package reversible

import "context"

// Gate decides whether an irreversible effect may run. This is the only
// place a human (or a policy acting for one) sits in the loop.
type Gate interface {
	Approve(ctx context.Context, in Intent, e Effect) (approved bool, reason string, err error)
}

// GateFunc adapts a function to the Gate interface.
type GateFunc func(ctx context.Context, in Intent, e Effect) (bool, string, error)

func (f GateFunc) Approve(ctx context.Context, in Intent, e Effect) (bool, string, error) {
	return f(ctx, in, e)
}

// DenyAll rejects every irreversible effect. It is the default gate, so an
// unattended agent can never take a one-way action by accident.
var DenyAll Gate = GateFunc(func(context.Context, Intent, Effect) (bool, string, error) {
	return false, "no approver configured; irreversible effects are denied by default", nil
})

// ApproveAll approves every irreversible effect. Use only in tests.
var ApproveAll Gate = GateFunc(func(context.Context, Intent, Effect) (bool, string, error) {
	return true, "approved by ApproveAll", nil
})
