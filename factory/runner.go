package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/PureMunky/reversible/telemetry"
)

// A runner starts and stops generated services on the factory's behalf,
// somewhere else: in kind, a separate container in the factory's pod,
// with its own process namespace and no secrets. The factory builds the
// code; the runner is the only thing that executes it.

var (
	mRunnerStarts  = tel.Counter("reversible_runner_starts_total", "Generated service processes started by the runner, by result.", "result")
	mRunnerRunning = tel.Gauge("reversible_runner_running", "Generated service processes the runner is running now.")
)

type runnerStart struct {
	Binary string `json:"binary"`
	Data   string `json:"data"`
}

type runnerReply struct {
	ID    string `json:"id,omitempty"`
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

var runnerClient = &http.Client{Timeout: 30 * time.Second}

func startRemote(ctx context.Context, runner, binary, dataPath string) (*Instance, error) {
	body, _ := json.Marshal(runnerStart{Binary: binary, Data: dataPath})
	var reply runnerReply
	var err error
	// The runner may still be starting (a sidecar starts first, but be patient).
	for attempt := 0; attempt < 20; attempt++ {
		if reply, err = runnerPost(ctx, runner+"/start", body); err == nil || !isConnErr(err) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if err != nil {
		return nil, err
	}
	id := reply.ID
	return &Instance{URL: reply.URL, stop: func() {
		b, _ := json.Marshal(map[string]string{"id": id})
		runnerPost(context.Background(), runner+"/stop", b)
	}}, nil
}

type connErr struct{ error }

func isConnErr(err error) bool { var c connErr; return errors.As(err, &c) }

func runnerPost(ctx context.Context, url string, body []byte) (runnerReply, error) {
	var r runnerReply
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return r, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := runnerClient.Do(req)
	if err != nil {
		return r, connErr{fmt.Errorf("runner: %w", err)}
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&r)
	if resp.StatusCode/100 != 2 {
		if r.Error == "" {
			r.Error = resp.Status
		}
		return r, fmt.Errorf("runner: %s", r.Error)
	}
	return r, nil
}

// RunnerHandler serves the runner's API: POST /start {binary, data}
// starts a built service (both paths must be under root) and answers
// {id, url}; POST /stop {id} stops it. Services are started exactly as
// the factory would start them itself, with only ADDR, DATA and TMPDIR in
// their environment.
func RunnerHandler(root string) (http.Handler, func()) {
	root = filepath.Clean(root)
	var mu sync.Mutex
	running := map[string]*Instance{}
	seq := 0
	under := func(p string) bool {
		p = filepath.Clean(p)
		return strings.HasPrefix(p, root+string(filepath.Separator))
	}
	reply := func(w http.ResponseWriter, status int, r runnerReply) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(r)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("GET /metrics", tel.MetricsHandler())
	mux.HandleFunc("POST /start", func(w http.ResponseWriter, r *http.Request) {
		var req runnerStart
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			reply(w, 400, runnerReply{Error: "unreadable request"})
			return
		}
		if !under(req.Binary) || (req.Data != "" && !under(req.Data)) {
			mRunnerStarts.Inc("refused")
			reply(w, 403, runnerReply{Error: fmt.Sprintf("paths must be under %s", root)})
			return
		}
		inst, err := startLocal(r.Context(), req.Binary, req.Data)
		if err != nil {
			mRunnerStarts.Inc("failed")
			reply(w, 500, runnerReply{Error: err.Error()})
			return
		}
		mu.Lock()
		seq++
		id := fmt.Sprintf("s%d", seq)
		running[id] = inst
		mRunnerRunning.Set(float64(len(running)))
		mu.Unlock()
		mRunnerStarts.Inc("started")
		telemetry.Emit("runner.started", telemetry.F{"id": id, "binary": req.Binary, "url": inst.URL})
		reply(w, 200, runnerReply{ID: id, URL: inst.URL})
	})
	mux.HandleFunc("POST /stop", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID string `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		inst := running[req.ID]
		delete(running, req.ID)
		mRunnerRunning.Set(float64(len(running)))
		mu.Unlock()
		inst.Stop()
		reply(w, 200, runnerReply{ID: req.ID})
	})
	stopAll := func() {
		mu.Lock()
		defer mu.Unlock()
		for id, inst := range running {
			inst.Stop()
			delete(running, id)
		}
		mRunnerRunning.Set(0)
	}
	return mux, stopAll
}
