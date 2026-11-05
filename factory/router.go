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

	"github.com/PureMunky/reversible"
)

// Exchange is one recorded request and the response production gave.
type Exchange struct {
	Request  HTTPCall `json:"request"`
	Response Response `json:"response"`
	Version  string   `json:"version"` // spec hash that served it
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
	call := HTTPCall{Method: req.Method, Path: req.URL.Path, Caller: req.Header.Get("X-Caller")}
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
	resp, err := Call(req.Context(), r.inst.URL, call)
	if err != nil {
		http.Error(w, `{"error":"live version unreachable"}`, http.StatusBadGateway)
		return
	}
	r.traffic = append(r.traffic, Exchange{Request: call, Response: resp, Version: r.live.SpecHash})
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
	mark := ReleaseMark{Version: v, Previous: prev, TrafficIndex: len(r.traffic)}
	if data, err := os.ReadFile(r.DataPath); err == nil {
		mark.Snapshot = fmt.Sprintf("%s.release-%d", r.DataPath, len(r.marks)+1)
		if err := os.WriteFile(mark.Snapshot, data, 0o644); err != nil {
			mark.Snapshot = ""
		}
	}
	r.marks = append(r.marks, mark)
	r.live, r.inst = v, inst
	r.mu.Unlock()
	old.Stop()
	return prev, nil
}

// Stop takes the service down.
func (r *Router) Stop() {
	r.swap(context.Background(), nil)
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
