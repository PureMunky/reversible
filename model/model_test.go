package model

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicRequestAndReply(t *testing.T) {
	var got request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("bad request: %s %v", r.URL.Path, r.Header)
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"content":[{"type":"text","text":"hello "},{"type":"text","text":"world"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	a := &Anthropic{APIKey: "k", BaseURL: srv.URL}
	out, err := a.Complete(context.Background(), "sys", "hi")
	if err != nil || out != "hello world" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.Model != DefaultModel || got.System != "sys" || got.MaxTokens != 4096 || got.Messages[0].Content != "hi" {
		t.Fatalf("request body: %+v", got)
	}
}

func TestAnthropicErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(), "", "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 429: rate_limit_error: slow down") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (&Anthropic{}).Complete(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error without a key")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		"plain":   `{"a":1}`,
		"fenced":  "Here you go:\n```json\n{\"a\":{\"b\":\"}\"}}\n```\nThanks",
		"escapes": `text {"s":"quote \" and brace }"} trailing {"x":2}`,
		"nested":  `{"candidates":[{"policy":{"rules":[{}]}}]}`,
	}
	want := map[string]string{
		"plain":   `{"a":1}`,
		"fenced":  `{"a":{"b":"}"}}`,
		"escapes": `{"s":"quote \" and brace }"}`,
		"nested":  `{"candidates":[{"policy":{"rules":[{}]}}]}`,
	}
	for name, in := range cases {
		got, err := ExtractJSON(in)
		if err != nil || got != want[name] {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	if _, err := ExtractJSON("no json here"); err == nil {
		t.Error("expected error")
	}
	if _, err := ExtractJSON(`{"open": [`); err == nil {
		t.Error("expected unterminated error")
	}
}
