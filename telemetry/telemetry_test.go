package telemetry

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrometheusText(t *testing.T) {
	r := New()
	c := r.Counter("x_total", "Things.", "kind")
	c.Inc("a")
	c.Add(2, `we"ird`)
	g := r.Gauge("live", "Which one.", "version")
	g.Only(1, "v1")
	g.Only(1, "v2")
	h := r.Histogram("lat_seconds", "Latency.", []float64{0.1, 1}, "v")
	h.Observe(0.05, "a")
	h.Observe(0.5, "a")
	h.Observe(5, "a")
	var b bytes.Buffer
	r.WritePrometheus(&b)
	out := b.String()
	for _, want := range []string{
		"# TYPE x_total counter", `x_total{kind="a"} 1`, `x_total{kind="we\"ird"} 2`,
		`live{version="v1"} 0`, `live{version="v2"} 1`,
		`lat_seconds_bucket{v="a",le="0.1"} 1`, `lat_seconds_bucket{v="a",le="1"} 2`, `lat_seconds_bucket{v="a",le="+Inf"} 3`,
		`lat_seconds_sum{v="a"} 5.55`, `lat_seconds_count{v="a"} 3`, "reversible_uptime_seconds",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEventsKeptFilteredAndSunk(t *testing.T) {
	r := New()
	var sink bytes.Buffer
	r.SetSink(&sink)
	r.Emit("build.started", F{"spec": "a"})
	r.Emit("canary.stopped", F{"reason": "errors"})
	r.Emit("build.finished", F{"spec": "a"})
	if got := r.Events(1); len(got) != 2 || got[0].Kind != "canary.stopped" {
		t.Fatalf("after 1: %+v", got)
	}
	if lines := strings.Count(sink.String(), "\n"); lines != 3 {
		t.Fatalf("sink got %d lines", lines)
	}
	srv := httptest.NewServer(r.EventsHandler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "?kind=build")
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	json.NewDecoder(resp.Body).Decode(&evs)
	if len(evs) != 2 || evs[1].Kind != "build.finished" {
		t.Fatalf("filtered: %+v", evs)
	}
}

func TestEventStreamReplaysThenFollows(t *testing.T) {
	r := New()
	r.Emit("story.step", F{"step": "1"})
	srv := httptest.NewServer(r.EventsHandler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"?stream=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var kinds []string
	go func() {
		time.Sleep(100 * time.Millisecond)
		r.Emit("story.step", F{"step": "2"})
	}()
	for sc.Scan() && len(kinds) < 2 {
		if l := sc.Text(); strings.HasPrefix(l, "data: ") {
			var e Event
			json.Unmarshal([]byte(strings.TrimPrefix(l, "data: ")), &e)
			kinds = append(kinds, e.Fields["step"].(string))
		}
	}
	if strings.Join(kinds, ",") != "1,2" {
		t.Fatalf("stream gave %v", kinds)
	}
}
