// Package telemetry makes what the system is doing visible from the
// outside, while it does it: metrics in the Prometheus text format for
// dashboards and alerts, and a stream of structured events (a build
// passed, a change was traced to intent, a canary was stopped, a decision
// was rewound) for following a run as it happens.
//
// The events are the story of how software is being built from intent,
// told to machines: each one names the spec, version or decision it is
// about, so an outside observer can follow a change from conversation to
// production without reading code or logs. Standard library only.
package telemetry

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Registry holds metrics and events. Most code uses Default.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]*family
	order   []string

	events  []Event
	seq     int64
	keep    int
	subs    map[chan Event]struct{}
	sink    io.Writer
	started time.Time
}

// Default is the process-wide registry.
var Default = New()

// New returns an empty registry that keeps the last 2000 events.
func New() *Registry {
	return &Registry{metrics: map[string]*family{}, keep: 2000, subs: map[chan Event]struct{}{}, started: time.Now()}
}

// ── metrics ────────────────────────────────────────────────────────────

type kind string

const (
	counter   kind = "counter"
	gauge     kind = "gauge"
	histogram kind = "histogram"
)

type family struct {
	name, help string
	kind       kind
	labels     []string
	buckets    []float64
	series     map[string]*series
}

type series struct {
	values []string
	value  float64
	counts []uint64 // histogram buckets (cumulative at render)
	sum    float64
	count  uint64
}

func (r *Registry) family(name, help string, k kind, labels []string, buckets []float64) *family {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.metrics[name]; ok {
		return f
	}
	f := &family{name: name, help: help, kind: k, labels: labels, buckets: buckets, series: map[string]*series{}}
	r.metrics[name] = f
	r.order = append(r.order, name)
	return f
}

func (r *Registry) series(f *family, values []string) *series {
	if len(values) != len(f.labels) {
		panic(fmt.Sprintf("telemetry: %s wants %d label values, got %d", f.name, len(f.labels), len(values)))
	}
	key := strings.Join(values, "\x00")
	s, ok := f.series[key]
	if !ok {
		s = &series{values: append([]string(nil), values...)}
		if f.kind == histogram {
			s.counts = make([]uint64, len(f.buckets))
		}
		f.series[key] = s
	}
	return s
}

// Counter is a monotonically increasing metric.
type Counter struct {
	r *Registry
	f *family
}

// Counter declares (or returns) a counter with the given label names.
func (r *Registry) Counter(name, help string, labels ...string) Counter {
	return Counter{r, r.family(name, help, counter, labels, nil)}
}

// Add increases the series for these label values.
func (c Counter) Add(n float64, values ...string) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()
	c.r.series(c.f, values).value += n
}

// Inc adds one.
func (c Counter) Inc(values ...string) { c.Add(1, values...) }

// Gauge is a metric that can go up and down.
type Gauge struct {
	r *Registry
	f *family
}

// Gauge declares (or returns) a gauge.
func (r *Registry) Gauge(name, help string, labels ...string) Gauge {
	return Gauge{r, r.family(name, help, gauge, labels, nil)}
}

// Set sets the series for these label values.
func (g Gauge) Set(v float64, values ...string) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	g.r.series(g.f, values).value = v
}

// Only sets this series to v and every other series of the gauge to 0: for
// "which one is it now" gauges, like the live version.
func (g Gauge) Only(v float64, values ...string) {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	for _, s := range g.f.series {
		s.value = 0
	}
	g.r.series(g.f, values).value = v
}

// Histogram records observations in buckets.
type Histogram struct {
	r *Registry
	f *family
}

// Histogram declares (or returns) a histogram with upper bucket bounds.
func (r *Registry) Histogram(name, help string, buckets []float64, labels ...string) Histogram {
	return Histogram{r, r.family(name, help, histogram, labels, buckets)}
}

// Observe records one value.
func (h Histogram) Observe(v float64, values ...string) {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	s := h.r.series(h.f, values)
	for i, b := range h.f.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.sum += v
	s.count++
}

// WritePrometheus writes every metric in the Prometheus text format.
func (r *Registry) WritePrometheus(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := append([]string(nil), r.order...)
	sort.Strings(names)
	for _, name := range names {
		f := r.metrics[name]
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", f.name, escapeHelp(f.help), f.name, f.kind)
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s := f.series[k]
			switch f.kind {
			case histogram:
				for i, b := range f.buckets {
					fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, "le", num(b)), s.counts[i])
				}
				fmt.Fprintf(w, "%s_bucket%s %d\n", f.name, labelSet(f.labels, s.values, "le", "+Inf"), s.count)
				fmt.Fprintf(w, "%s_sum%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), num(s.sum))
				fmt.Fprintf(w, "%s_count%s %d\n", f.name, labelSet(f.labels, s.values, "", ""), s.count)
			default:
				fmt.Fprintf(w, "%s%s %s\n", f.name, labelSet(f.labels, s.values, "", ""), num(s.value))
			}
		}
	}
	fmt.Fprintf(w, "# HELP reversible_uptime_seconds Seconds since the process started.\n# TYPE reversible_uptime_seconds gauge\nreversible_uptime_seconds %s\n", num(time.Since(r.started).Seconds()))
}

func labelSet(names, values []string, extraName, extraValue string) string {
	var parts []string
	for i, n := range names {
		parts = append(parts, n+`="`+escapeLabel(values[i])+`"`)
	}
	if extraName != "" {
		parts = append(parts, extraName+`="`+extraValue+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

func escapeHelp(s string) string { return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s) }

func num(v float64) string {
	if math.IsInf(v, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ── events ─────────────────────────────────────────────────────────────

// Event is one thing that happened, as data.
type Event struct {
	Seq    int64          `json:"seq"`
	Time   time.Time      `json:"time"`
	Kind   string         `json:"kind"` // e.g. "build.finished", "canary.stopped"
	Fields map[string]any `json:"fields,omitempty"`
}

// F is shorthand for event fields.
type F = map[string]any

// Emit records an event, writes it to the sink (one JSON line) if there
// is one, and sends it to every subscriber.
func (r *Registry) Emit(kind string, fields F) Event {
	r.mu.Lock()
	r.seq++
	e := Event{Seq: r.seq, Time: time.Now().UTC(), Kind: kind, Fields: fields}
	r.events = append(r.events, e)
	if len(r.events) > r.keep {
		r.events = r.events[len(r.events)-r.keep:]
	}
	sink := r.sink
	subs := make([]chan Event, 0, len(r.subs))
	for c := range r.subs {
		subs = append(subs, c)
	}
	r.mu.Unlock()
	if sink != nil {
		b, _ := json.Marshal(e)
		sink.Write(append(b, '\n'))
	}
	for _, c := range subs {
		select {
		case c <- e:
		default: // a slow subscriber misses events rather than slowing the system
		}
	}
	return e
}

// SetSink sends every event, as a JSON line, to w (nil for none).
func (r *Registry) SetSink(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sink = w
}

// Events returns the kept events after seq (0 for all).
func (r *Registry) Events(after int64) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Event
	for _, e := range r.events {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel of new events and a function to stop.
func (r *Registry) Subscribe() (<-chan Event, func()) {
	c := make(chan Event, 256)
	r.mu.Lock()
	r.subs[c] = struct{}{}
	r.mu.Unlock()
	return c, func() {
		r.mu.Lock()
		delete(r.subs, c)
		r.mu.Unlock()
	}
}

// Package-level shorthands on Default.

// Emit records an event on Default.
func Emit(kind string, fields F) Event { return Default.Emit(kind, fields) }

// Short shortens a hash for labels and fields.
func Short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// SinkFromEnv sends events to where REVERSIBLE_EVENTS says: "stderr", or
// a file path (appended to, one JSON object per line). Unset: no sink.
func SinkFromEnv() (close func(), err error) {
	switch to := os.Getenv("REVERSIBLE_EVENTS"); to {
	case "":
		return func() {}, nil
	case "stderr":
		Default.SetSink(os.Stderr)
		return func() {}, nil
	default:
		f, err := os.OpenFile(to, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return func() {}, err
		}
		Default.SetSink(f)
		return func() { Default.SetSink(nil); f.Close() }, nil
	}
}
