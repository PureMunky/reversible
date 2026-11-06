package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/PureMunky/reversible"
)

// Exchange is one recorded request and the response production gave.
type Exchange struct {
	ID        string   `json:"id"`
	Request   HTTPCall `json:"request"`
	Response  Response `json:"response"`
	Version   string   `json:"version"` // spec hash that served it
	LatencyMS float64  `json:"latency_ms"`
}

// Router is production: it forwards traffic to the live version, records
// every exchange (for replaying against future versions), and swaps
// versions on release. State lives in DataPath, shared across versions.
type Router struct {
	DataPath string

	mu      sync.RWMutex
	live    *Version
	inst    *Instance
	traffic []Exchange
	marks   []ReleaseMark
	seq     int

	// A canary: a second version taking a share of traffic.
	canary     *Version
	canaryInst *Instance
	toCanary   func(id string) bool         // which requests the canary serves
	outcome    func(id string, ex Exchange) // told about every exchange
}

// Canary returns the version serving canary traffic, if any.
func (r *Router) Canary() *Version {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.canary
}

// ReleaseMark records a change of live version: what was live before,
// where the traffic log stood, and a snapshot of production data at that
// moment. Together they let the factory ask what the previous version
// would have done with exactly the traffic the new one served.
type ReleaseMark struct {
	Version      *Version // nil if the service was taken down
	Previous     *Version // nil for a first release
	TrafficIndex int      // traffic recorded before this release
	Snapshot     string   // copy of production data at the moment of release ("" if none yet)
}

// LastRelease returns the most recent release mark.
func (r *Router) LastRelease() (ReleaseMark, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.marks) == 0 {
		return ReleaseMark{}, false
	}
	return r.marks[len(r.marks)-1], true
}

// Live returns the version serving traffic.
func (r *Router) Live() *Version {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.live
}

// Traffic returns every recorded exchange, oldest first.
func (r *Router) Traffic() []Exchange {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Exchange(nil), r.traffic...)
}

// ServeHTTP forwards a request to the live version and records it.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	call := HTTPCall{Method: req.Method, Path: req.URL.RequestURI(), Caller: req.Header.Get("X-Caller")}
	if raw, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20)); len(raw) > 0 {
		var body any
		if json.Unmarshal(raw, &body) != nil {
			body = string(raw)
		}
		call.Body = body
	}
	r.mu.Lock() // serialise, so recorded order is the order production saw
	defer r.mu.Unlock()
	if r.inst == nil {
		http.Error(w, `{"error":"no version is live"}`, http.StatusServiceUnavailable)
		return
	}
	r.seq++
	id := fmt.Sprintf("r%06d", r.seq)
	inst, version := r.inst, r.live
	if r.canaryInst != nil && r.toCanary != nil && r.toCanary(id) {
		inst, version = r.canaryInst, r.canary
	}
	start := time.Now()
	resp, err := Call(req.Context(), inst.URL, call)
	if err != nil {
		resp = Response{Status: http.StatusBadGateway, Body: map[string]any{"error": "version unreachable"}}
	}
	ex := Exchange{ID: id, Request: call, Response: resp, Version: version.SpecHash, LatencyMS: float64(time.Since(start).Microseconds()) / 1000}
	r.traffic = append(r.traffic, ex)
	if r.outcome != nil {
		r.outcome(id, ex)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	if resp.Body != nil {
		json.NewEncoder(w).Encode(resp.Body)
	}
}

// swap starts v on the shared data, makes it live, and stops the old one.
// It returns the version that was live before (nil if none). Passing nil
// takes the service down.
func (r *Router) swap(ctx context.Context, v *Version) (*Version, error) {
	var inst *Instance
	if v != nil {
		if v.Binary == "" {
			return nil, errors.New("version has no build")
		}
		var err error
		if inst, err = Start(ctx, v.Binary, r.DataPath); err != nil {
			return nil, fmt.Errorf("starting %s: %w", v.SpecHash, err)
		}
	}
	r.mu.Lock()
	prev, old := r.live, r.inst
	r.marks = append(r.marks, r.markLocked(v, prev))
	r.live, r.inst = v, inst
	r.mu.Unlock()
	old.Stop()
	return prev, nil
}

// markLocked records a release, snapshotting production data.
func (r *Router) markLocked(v, prev *Version) ReleaseMark {
	mark := ReleaseMark{Version: v, Previous: prev, TrafficIndex: len(r.traffic)}
	if data, err := os.ReadFile(r.DataPath); err == nil {
		mark.Snapshot = fmt.Sprintf("%s.release-%d", r.DataPath, len(r.marks)+1)
		if err := os.WriteFile(mark.Snapshot, data, 0o644); err != nil {
			mark.Snapshot = ""
		}
	}
	return mark
}

// Stop takes the service down, canary included.
func (r *Router) Stop() {
	r.endCanary()
	r.swap(context.Background(), nil)
}

// startCanary runs v alongside the live version on the same data, serving
// the requests toCanary picks; outcome hears about every exchange.
func (r *Router) startCanary(ctx context.Context, v *Version, toCanary func(string) bool, outcome func(string, Exchange)) error {
	if r.Live() == nil {
		return errors.New("a canary needs a live version to run beside")
	}
	if r.Canary() != nil {
		return errors.New("a canary is already running")
	}
	inst, err := Start(ctx, v.Binary, r.DataPath)
	if err != nil {
		return fmt.Errorf("starting canary %s: %w", v.SpecHash, err)
	}
	r.mu.Lock()
	r.canary, r.canaryInst, r.toCanary, r.outcome = v, inst, toCanary, outcome
	r.mu.Unlock()
	return nil
}

// endCanary stops the canary; all traffic goes to the live version.
func (r *Router) endCanary() {
	r.mu.Lock()
	inst := r.canaryInst
	r.canary, r.canaryInst, r.toCanary, r.outcome = nil, nil, nil, nil
	r.mu.Unlock()
	inst.Stop()
}

// promoteCanary makes the running canary the live version, recording a
// release mark, and stops the old live version. It returns the old one.
func (r *Router) promoteCanary() (*Version, error) {
	r.mu.Lock()
	if r.canaryInst == nil {
		r.mu.Unlock()
		return nil, errors.New("no canary to promote")
	}
	prev, old := r.live, r.inst
	r.marks = append(r.marks, r.markLocked(r.canary, prev))
	r.live, r.inst = r.canary, r.canaryInst
	r.canary, r.canaryInst, r.toCanary, r.outcome = nil, nil, nil, nil
	r.mu.Unlock()
	old.Stop()
	return prev, nil
}

// CanaryEffect starts v as a canary. Undoing it stops the canary, so
// rewinding the decision behind it takes the canary away.
func (r *Router) CanaryEffect(v *Version, toCanary func(string) bool, outcome func(string, Exchange)) reversible.Effect {
	return reversible.Effect{
		Name:        "canary",
		Class:       reversible.Reversible,
		Description: fmt.Sprintf("run %s (spec %s) as a canary", v.Spec.Service, v.SpecHash),
		Do: func(ctx context.Context) (reversible.Receipt, error) {
			if err := r.startCanary(ctx, v, toCanary, outcome); err != nil {
				return nil, err
			}
			return reversible.Receipt{"canary": v.SpecHash}, nil
		},
		Undo: func(context.Context, reversible.Receipt) error {
			if c := r.Canary(); c == v {
				r.endCanary()
			}
			return nil
		},
	}
}

// PromoteEffect makes the canary live. Undoing it puts the previous
// version back.
func (r *Router) PromoteEffect() reversible.Effect {
	var prev *Version
	return reversible.Effect{
		Name:  "promote",
		Class: reversible.Reversible,
		Do: func(context.Context) (reversible.Receipt, error) {
			c := r.Canary()
			p, err := r.promoteCanary()
			if err != nil {
				return nil, err
			}
			prev = p
			return reversible.Receipt{"from": p.SpecHash, "to": c.SpecHash}, nil
		},
		Undo: func(ctx context.Context, _ reversible.Receipt) error {
			_, err := r.swap(ctx, prev)
			return err
		},
	}
}

// Release is the effect of putting a version live. It is reversible: its
// Undo puts back whichever version was live before. That is what makes
// rewinding a decision about intent take the running software back too.
func (r *Router) Release(v *Version) reversible.Effect {
	var prev *Version
	return reversible.Effect{
		Name:        "release",
		Class:       reversible.Reversible,
		Description: fmt.Sprintf("put %s (spec %s) live", v.Spec.Service, v.SpecHash),
		Do: func(ctx context.Context) (reversible.Receipt, error) {
			p, err := r.swap(ctx, v)
			if err != nil {
				return nil, err
			}
			prev = p
			from := "nothing"
			if p != nil {
				from = p.SpecHash
			}
			return reversible.Receipt{"from": from, "to": v.SpecHash}, nil
		},
		Undo: func(ctx context.Context, _ reversible.Receipt) error {
			_, err := r.swap(ctx, prev)
			return err
		},
	}
}
