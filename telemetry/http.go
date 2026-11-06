package telemetry

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MetricsHandler serves the Prometheus text format.
func (r *Registry) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/plain; version=0.0.4; charset=utf-8")
		r.WritePrometheus(w)
	})
}

// EventsHandler serves kept events as JSON (?after=seq for only newer
// ones, ?kind=prefix to filter), or, with Accept: text/event-stream or
// ?stream=1, streams them as server-sent events: everything kept first,
// then each new event as it happens.
func (r *Registry) EventsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		after, _ := strconv.ParseInt(req.URL.Query().Get("after"), 10, 64)
		prefix := req.URL.Query().Get("kind")
		match := func(e Event) bool { return prefix == "" || strings.HasPrefix(e.Kind, prefix) }
		if req.URL.Query().Get("stream") == "" && !strings.Contains(req.Header.Get("Accept"), "text/event-stream") {
			out := []Event{}
			for _, e := range r.Events(after) {
				if match(e) {
					out = append(out, e)
				}
			}
			w.Header().Set("content-type", "application/json")
			json.NewEncoder(w).Encode(out)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		live, stop := r.Subscribe()
		defer stop()
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		send := func(e Event) {
			if e.Seq <= after || !match(e) {
				return
			}
			after = e.Seq
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, b) // unnamed, so EventSource.onmessage gets every kind
		}
		for _, e := range r.Events(after) {
			send(e)
		}
		flusher.Flush()
		ping := time.NewTicker(15 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-req.Context().Done():
				return
			case e := <-live:
				send(e)
				flusher.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": keep-alive\n\n")
				flusher.Flush()
			}
		}
	})
}
