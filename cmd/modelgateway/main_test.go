package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PureMunky/reversible/model"
)

const token = "run-token-0123456789abcdef"

// upstream stands in for the Messages API and records which key it got.
func upstream(t *testing.T, gotKey *string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*gotKey = r.Header.Get("x-api-key")
		w.WriteHeader(status)
		if status == 200 {
			w.Write([]byte(`{"model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":400}}`))
			return
		}
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	}))
}

func client(base, key string) *model.Anthropic {
	return &model.Anthropic{APIKey: key, BaseURL: base, Retries: -1, Backoff: time.Millisecond}
}

func TestGatewaySwapsTokenForKey(t *testing.T) {
	var got string
	up := upstream(t, &got, 200)
	defer up.Close()
	gw := httptest.NewServer(newGateway(config{Token: token, Mode: "live", Upstream: up.URL, Key: "sk-real", MaxCalls: 5, MaxOutput: 10000}))
	defer gw.Close()

	if out, err := client(gw.URL, token).Complete(context.Background(), "", "hi"); err != nil || out != "ok" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got != "sk-real" {
		t.Fatalf("upstream got key %q", got)
	}
	// The real key is not a credential for the gateway, and nothing else is either.
	for _, k := range []string{"sk-real", "", "wrong-token-0123456789"} {
		if _, err := client(gw.URL, k).Complete(context.Background(), "", "hi"); err == nil || (k != "" && !strings.Contains(err.Error(), "authentication_error")) {
			t.Fatalf("key %q: err=%v", k, err)
		}
	}
}

func TestGatewayEnforcesBudget(t *testing.T) {
	var got string
	up := upstream(t, &got, 200)
	defer up.Close()
	gw := httptest.NewServer(newGateway(config{Token: token, Mode: "live", Upstream: up.URL, Key: "sk-real", MaxCalls: 2, MaxOutput: 100000}))
	defer gw.Close()
	c := client(gw.URL, token)
	for i := 0; i < 2; i++ {
		if _, err := c.Complete(context.Background(), "", "hi"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Complete(context.Background(), "", "hi")
	if err == nil || !strings.Contains(err.Error(), "budget is spent") {
		t.Fatalf("third call: %v", err)
	}

	// Output tokens too: 400 per call, 500 allowed.
	gw2 := httptest.NewServer(newGateway(config{Token: token, Mode: "live", Upstream: up.URL, Key: "sk-real", MaxCalls: 10, MaxOutput: 500}))
	defer gw2.Close()
	c2 := client(gw2.URL, token)
	c2.Complete(context.Background(), "", "a")
	c2.Complete(context.Background(), "", "b")
	if _, err := c2.Complete(context.Background(), "", "c"); err == nil || !strings.Contains(err.Error(), "budget is spent") {
		t.Fatalf("over the token budget: %v", err)
	}
}

func TestGatewayDoesNotChargeFailures(t *testing.T) {
	var got string
	up := upstream(t, &got, 529)
	defer up.Close()
	g := newGateway(config{Token: token, Mode: "live", Upstream: up.URL, Key: "sk-real", MaxCalls: 1, MaxOutput: 1000})
	gw := httptest.NewServer(g)
	defer gw.Close()
	for i := 0; i < 3; i++ {
		_, err := client(gw.URL, token).Complete(context.Background(), "", "hi")
		if err == nil || !strings.Contains(err.Error(), "529") {
			t.Fatalf("want the upstream's 529 passed through, got %v", err)
		}
	}
	if g.calls != 0 {
		t.Fatalf("failed calls charged: %d", g.calls)
	}
}

func TestRehearsalNeverForwardsARealKey(t *testing.T) {
	t.Setenv("GATEWAY_TOKEN", token)
	t.Setenv("GATEWAY_MODE", "rehearsal")
	t.Setenv("ANTHROPIC_API_KEY", "sk-real")
	cfg, err := fromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Key == "sk-real" {
		t.Fatal("rehearsal would forward the real key")
	}
	t.Setenv("GATEWAY_TOKEN", "short")
	if _, err := fromEnv(); err == nil {
		t.Fatal("accepted a short token")
	}
}

func TestGatewayOnlyForwardsMessages(t *testing.T) {
	gw := httptest.NewServer(newGateway(config{Token: token, Mode: "live", Upstream: "http://127.0.0.1:1", Key: "k", MaxCalls: 1, MaxOutput: 1}))
	defer gw.Close()
	resp, err := http.Get(gw.URL + "/v1/models")
	if err != nil || resp.StatusCode != 404 {
		t.Fatalf("GET /v1/models: %v %v", resp.StatusCode, err)
	}
}
