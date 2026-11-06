// Package serve exposes a demo's production while its story runs and
// after it ends, so the software built from intent can be watched, used
// and inspected directly: telemetry from the first moment,
// requests go to whichever version is live (and are recorded like any
// other production traffic), and /_factory/ shows what that version is
// and why it exists, without reading any code to understand it.
package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/PureMunky/reversible/factory"
	"github.com/PureMunky/reversible/telemetry"
)

// Info is what the inspector can show beyond the router itself.
type Info struct {
	Why   func() string // why the live version is live (the decision chain)
	Usage func() string // model usage so far, or "" offline
	Mode  string        // how the software was built
}

// Handler serves production at / and the inspector at /_factory/.
func Handler(router *factory.Router, info Info) http.Handler {
	mux := http.NewServeMux()
	text := func(w http.ResponseWriter, s string) {
		w.Header().Set("content-type", "text/plain; charset=utf-8")
		fmt.Fprint(w, s)
	}
	live := func(w http.ResponseWriter) *factory.Version {
		v := router.Live()
		if v == nil {
			http.Error(w, "nothing is live", http.StatusServiceUnavailable)
		}
		return v
	}
	mux.HandleFunc("GET /_factory/{$}", func(w http.ResponseWriter, r *http.Request) {
		v := live(w)
		if v == nil {
			return
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %s\n\n", v.Spec.Service, v.Spec.Purpose)
		fmt.Fprintf(&b, "built: %s\n", info.Mode)
		passed := 0
		for _, res := range v.Results {
			if res.Pass {
				passed++
			}
		}
		fmt.Fprintf(&b, "live version: %s (%d/%d examples pass, %d generated lines, %d attempt(s))\n", v.SpecHash, passed, len(v.Results), v.Lines(), v.Attempts)
		if c := router.Canary(); c != nil {
			fmt.Fprintf(&b, "canary: %s\n", c.SpecHash)
		}
		if info.Usage != nil {
			if u := info.Usage(); u != "" {
				fmt.Fprintf(&b, "model usage: %s\n", u)
			}
		}
		fmt.Fprintf(&b, "production requests recorded: %d\n\nendpoints (the intent, not the code, defines them):\n", len(router.Traffic()))
		for _, res := range v.Spec.All() {
			who := "anyone"
			if len(res.Writers) > 0 {
				who = strings.Join(res.Writers, ", ") + " (X-Caller header)"
			}
			fmt.Fprintf(&b, "  /%s            GET list, POST create   (changes: %s)\n", res.Plural, who)
			fmt.Fprintf(&b, "  /%s/{%s}  GET, PATCH, DELETE\n", res.Plural, res.Key)
			var filters []string
			for _, f := range res.Fields {
				if f.Filterable {
					filters = append(filters, f.Name)
				}
			}
			if len(filters) > 0 {
				fmt.Fprintf(&b, "  /%s?%s=...  filter by %s\n", res.Plural, filters[0], strings.Join(filters, ", "))
			}
		}
		b.WriteString("\ninspect:\n")
		for _, l := range [][2]string{
			{"/_factory/spec", "the intent the live version was built from (the source of truth)"},
			{"/_factory/examples", "the contract: every example and what the live version answered"},
			{"/_factory/why", "the decisions behind the live version, back to the conversation"},
			{"/_factory/traffic", "the most recent production requests and which version served each"},
			{"/_factory/code", "the generated code (a cache: change the intent, not this)"},
			{"/_factory/live", "a live view: what's happening, as it happens"},
			{"/_factory/events", "every event as JSON (?after=seq, ?kind=prefix, ?stream=1 to follow)"},
			{"/metrics", "Prometheus metrics"},
		} {
			fmt.Fprintf(&b, "  %-20s %s\n", l[0], l[1])
		}
		text(w, b.String())
	})
	mux.HandleFunc("GET /_factory/spec", func(w http.ResponseWriter, r *http.Request) {
		if v := live(w); v != nil {
			w.Header().Set("content-type", "application/json")
			fmt.Fprintln(w, v.Spec.JSON())
		}
	})
	mux.HandleFunc("GET /_factory/code", func(w http.ResponseWriter, r *http.Request) {
		if v := live(w); v != nil {
			names := make([]string, 0, len(v.Files))
			for n := range v.Files {
				names = append(names, n)
			}
			sort.Strings(names)
			var b strings.Builder
			for _, n := range names {
				if len(names) > 1 {
					fmt.Fprintf(&b, "// ── %s ──\n", n)
				}
				b.WriteString(v.Files[n])
			}
			text(w, b.String())
		}
	})
	mux.HandleFunc("GET /_factory/examples", func(w http.ResponseWriter, r *http.Request) {
		if v := live(w); v != nil {
			var b strings.Builder
			for _, res := range v.Results {
				mark := "✓"
				if !res.Pass {
					mark = "✗"
				}
				caller := ""
				if res.Example.Request.Caller != "" {
					caller = " as " + res.Example.Request.Caller
				}
				fmt.Fprintf(&b, "%s %s\n    %s %s%s → %d %s\n", mark, res.Example.Name, res.Example.Request.Method, res.Example.Request.Path, caller, res.Got.Status, compact(res.Got.Body))
			}
			text(w, b.String())
		}
	})
	mux.HandleFunc("GET /_factory/why", func(w http.ResponseWriter, r *http.Request) {
		if info.Why == nil {
			http.Error(w, "no ledger", http.StatusNotFound)
			return
		}
		text(w, info.Why())
	})
	mux.HandleFunc("GET /_factory/traffic", func(w http.ResponseWriter, r *http.Request) {
		t := router.Traffic()
		if len(t) > 50 {
			t = t[len(t)-50:]
		}
		var b strings.Builder
		for _, ex := range t {
			caller := ex.Request.Caller
			if caller == "" {
				caller = "-"
			}
			fmt.Fprintf(&b, "%s  %-16s %-6s %-36s → %d\n", short(ex.Version), caller, ex.Request.Method, ex.Request.Path, ex.Response.Status)
		}
		text(w, b.String())
	})
	mux.Handle("/", router)
	return mux
}

// Server is production plus the inspector and telemetry, listening from
// the start of a run so it can be watched while the story happens.
type Server struct {
	srv   *http.Server
	ready chan struct{}
	errs  chan error
	addr  net.Addr
}

// Start listens on addr and serves until Close. /_factory/ready answers
// 503 until Ready is called.
func Start(addr string, router *factory.Router, info Info) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ready: make(chan struct{}), errs: make(chan error, 1), addr: ln.Addr()}
	mux := http.NewServeMux()
	mux.Handle("/", Handler(router, info))
	mux.Handle("GET /metrics", telemetry.Default.MetricsHandler())
	mux.Handle("GET /_factory/events", telemetry.Default.EventsHandler())
	mux.HandleFunc("GET /_factory/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html; charset=utf-8")
		fmt.Fprint(w, livePage)
	})
	mux.HandleFunc("GET /_factory/ready", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-s.ready:
			fmt.Fprintln(w, "ready")
		default:
			http.Error(w, "the story is still running", http.StatusServiceUnavailable)
		}
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { s.errs <- s.srv.Serve(ln) }()
	fmt.Printf("telemetry: http://%s/metrics (Prometheus), /_factory/events (events; ?stream=1 to follow), /_factory/live\n", ln.Addr())
	return s, nil
}

// Ready marks the story finished, then serves until SIGINT or SIGTERM (a
// pod being deleted).
func (s *Server) Ready() error {
	close(s.ready)
	telemetry.Emit("story.serving", telemetry.F{"addr": s.addr.String()})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("\n━━━ Serving production on %s until stopped\n", s.addr)
	fmt.Println("   /             the live version (requests are recorded like any production traffic)")
	fmt.Println("   /_factory/    what's live, the intent behind it, its contract, its decisions and its code")
	fmt.Println("   /metrics, /_factory/events, /_factory/live    telemetry")
	select {
	case <-ctx.Done():
		fmt.Println("   stopping")
		return nil
	case err := <-s.errs:
		return err
	}
}

// Close stops serving.
func (s *Server) Close() {
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.Shutdown(shut)
}

func short(h string) string {
	if len(h) > 7 {
		return h[:7]
	}
	return h
}

func compact(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	if len(b) > 120 {
		return string(b[:117]) + "..."
	}
	return string(b)
}
