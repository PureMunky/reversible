// Command modelgateway is the only thing in the kind environment that
// holds the Anthropic key and can reach the internet. The factory talks to
// it as if it were the Messages API, authenticating with a random per-run
// token; the gateway checks the token, enforces a budget, and forwards the
// call upstream with the real key.
//
// That keeps the key out of the pod where model-written code runs, and
// lets that pod have no route out at all. The budget is a hard stop
// outside the factory: whatever a run does, it can't spend more than the
// calls and tokens it was given.
//
// Configuration (environment):
//
//	GATEWAY_TOKEN              required; the token callers present as x-api-key
//	GATEWAY_MODE               live (default) or rehearsal
//	GATEWAY_UPSTREAM           default https://api.anthropic.com
//	ANTHROPIC_API_KEY          the real key (live only; rehearsal never sends it)
//	GATEWAY_MAX_CALLS          successful calls allowed (default 15)
//	GATEWAY_MAX_OUTPUT_TOKENS  output tokens allowed (default 150000)
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PureMunky/reversible/telemetry"
)

type config struct {
	Token     string
	Mode      string
	Upstream  string
	Key       string
	MaxCalls  int
	MaxOutput int
}

var (
	mReqs      = telemetry.Default.Counter("reversible_gateway_requests_total", "Requests to the model gateway by outcome: forwarded, unauthorized, over_budget, upstream_error, bad_request.", "outcome")
	mUpstream  = telemetry.Default.Counter("reversible_gateway_upstream_responses_total", "Upstream responses by status code.", "code")
	mTokens    = telemetry.Default.Counter("reversible_gateway_tokens_total", "Tokens through the gateway by direction.", "direction")
	mRemaining = telemetry.Default.Gauge("reversible_gateway_budget_remaining", "What's left of this run's budget.", "unit")
)

func fromEnv() (config, error) {
	c := config{
		Token: os.Getenv("GATEWAY_TOKEN"), Mode: os.Getenv("GATEWAY_MODE"), Upstream: os.Getenv("GATEWAY_UPSTREAM"),
		Key: strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")), MaxCalls: 15, MaxOutput: 150000,
	}
	if c.Mode == "" {
		c.Mode = "live"
	}
	if c.Upstream == "" {
		c.Upstream = "https://api.anthropic.com"
	}
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_MAX_CALLS")); err == nil && n > 0 {
		c.MaxCalls = n
	}
	if n, err := strconv.Atoi(os.Getenv("GATEWAY_MAX_OUTPUT_TOKENS")); err == nil && n > 0 {
		c.MaxOutput = n
	}
	switch {
	case len(c.Token) < 16:
		return c, fmt.Errorf("GATEWAY_TOKEN must be set (at least 16 characters)")
	case c.Mode != "live" && c.Mode != "rehearsal":
		return c, fmt.Errorf("GATEWAY_MODE is live or rehearsal")
	case c.Mode == "rehearsal":
		c.Key = "rehearsal-not-a-real-key" // never forward a real key to a stand-in
	}
	return c, nil
}

type gateway struct {
	cfg    config
	client *http.Client
	mu     sync.Mutex
	calls  int // successful, plus in flight
	output int
}

func newGateway(cfg config) *gateway {
	g := &gateway{cfg: cfg, client: &http.Client{Timeout: 10 * time.Minute}}
	g.publish()
	return g
}

func (g *gateway) publish() {
	mRemaining.Set(float64(g.cfg.MaxCalls-g.calls), "calls")
	mRemaining.Set(float64(g.cfg.MaxOutput-g.output), "output_tokens")
}

func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/healthz":
		fmt.Fprintln(w, "ok")
		return
	case r.Method == http.MethodGet && r.URL.Path == "/metrics":
		telemetry.Default.MetricsHandler().ServeHTTP(w, r)
		return
	case r.Method != http.MethodPost || r.URL.Path != "/v1/messages":
		mReqs.Inc("bad_request")
		apiError(w, 404, "not_found_error", "the gateway only forwards POST /v1/messages")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("x-api-key")), []byte(g.cfg.Token)) != 1 {
		mReqs.Inc("unauthorized")
		telemetry.Emit("gateway.unauthorized", telemetry.F{"remote": r.RemoteAddr})
		apiError(w, 401, "authentication_error", "not this run's gateway token")
		return
	}
	if g.cfg.Mode == "live" && g.cfg.Key == "" {
		apiError(w, 503, "api_error", "the gateway has no API key (run.sh key)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		mReqs.Inc("bad_request")
		apiError(w, 400, "invalid_request_error", "unreadable body")
		return
	}
	// Reserve a call before forwarding, so concurrent calls can't overspend.
	g.mu.Lock()
	if g.calls >= g.cfg.MaxCalls || g.output >= g.cfg.MaxOutput {
		calls, output := g.calls, g.output
		g.mu.Unlock()
		mReqs.Inc("over_budget")
		telemetry.Emit("gateway.over_budget", telemetry.F{"calls": calls, "output_tokens": output})
		// 403, not 429: a spent budget isn't something to retry.
		apiError(w, 403, "permission_error", fmt.Sprintf("this run's budget is spent (%d/%d calls, %d/%d output tokens); raise GATEWAY_MAX_CALLS or GATEWAY_MAX_OUTPUT_TOKENS", calls, g.cfg.MaxCalls, output, g.cfg.MaxOutput))
		return
	}
	g.calls++
	g.publish()
	g.mu.Unlock()
	refund := func() {
		g.mu.Lock()
		g.calls--
		g.publish()
		g.mu.Unlock()
	}

	start := time.Now()
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(g.cfg.Upstream, "/")+"/v1/messages", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", r.Header.Get("anthropic-version"))
	req.Header.Set("x-api-key", g.cfg.Key)
	resp, err := g.client.Do(req)
	if err != nil {
		refund()
		mReqs.Inc("upstream_error")
		telemetry.Emit("gateway.upstream_error", telemetry.F{"error": err.Error()})
		apiError(w, 502, "api_error", "upstream unreachable")
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	mUpstream.Inc(strconv.Itoa(resp.StatusCode))
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	json.Unmarshal(out, &parsed)
	if resp.StatusCode/100 == 2 {
		mReqs.Inc("forwarded")
		mTokens.Add(float64(parsed.Usage.InputTokens), "input")
		mTokens.Add(float64(parsed.Usage.OutputTokens), "output")
		g.mu.Lock()
		g.output += parsed.Usage.OutputTokens
		g.publish()
		calls, output := g.calls, g.output
		g.mu.Unlock()
		telemetry.Emit("gateway.forwarded", telemetry.F{"model": parsed.Model, "input_tokens": parsed.Usage.InputTokens, "output_tokens": parsed.Usage.OutputTokens,
			"seconds": time.Since(start).Seconds(), "calls_used": calls, "output_tokens_used": output})
		log.Printf("forwarded: %s, %d in / %d out tokens, %s (budget used: %d/%d calls, %d/%d output tokens)",
			parsed.Model, parsed.Usage.InputTokens, parsed.Usage.OutputTokens, time.Since(start).Round(time.Second), calls, g.cfg.MaxCalls, output, g.cfg.MaxOutput)
	} else {
		refund() // a refused or failed call doesn't count against the budget
		mReqs.Inc("upstream_error")
		telemetry.Emit("gateway.upstream_error", telemetry.F{"status": resp.StatusCode})
		log.Printf("upstream answered %d", resp.StatusCode)
	}
	w.Header().Set("content-type", resp.Header.Get("content-type"))
	w.WriteHeader(resp.StatusCode)
	w.Write(out)
}

func main() {
	addr := ":8080"
	if len(os.Args) > 2 && os.Args[1] == "-addr" {
		addr = os.Args[2]
	}
	cfg, err := fromEnv()
	if err != nil {
		log.Fatal("modelgateway: ", err)
	}
	keyState := "no key"
	if cfg.Key != "" {
		keyState = "key loaded"
	}
	log.Printf("modelgateway: %s mode, upstream %s, %s, budget %d calls / %d output tokens, on %s",
		cfg.Mode, cfg.Upstream, keyState, cfg.MaxCalls, cfg.MaxOutput, addr)
	srv := &http.Server{Addr: addr, Handler: newGateway(cfg), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
