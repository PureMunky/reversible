package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
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
	if got.Model != DefaultModel || got.System != "sys" || got.MaxTokens != DefaultMaxTokens || got.Messages[0].Content != "hi" {
		t.Fatalf("request body: %+v", got)
	}
}

func TestAnthropicErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL, Retries: -1}).Complete(context.Background(), "", "x")
	if err == nil || !strings.Contains(err.Error(), "HTTP 429: rate_limit_error: slow down") {
		t.Fatalf("err = %v", err)
	}
	if _, err := (&Anthropic{}).Complete(context.Background(), "", "x"); err == nil {
		t.Fatal("expected error without a key")
	}
}

func TestAnthropicRetriesThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(529)
			w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`))
	}))
	defer srv.Close()
	var logged []string
	a := &Anthropic{APIKey: "k", BaseURL: srv.URL, Backoff: time.Millisecond, Log: func(s string) { logged = append(logged, s) }}
	out, err := a.Complete(context.Background(), "", "x")
	if err != nil || out != "ok" || calls != 3 {
		t.Fatalf("out=%q err=%v calls=%d", out, err, calls)
	}
	if u := a.Usage(); u.Calls != 1 || u.InputTokens != 10 || u.OutputTokens != 3 {
		t.Fatalf("usage %+v", u)
	}
	if len(logged) != 3 || !strings.Contains(logged[0], "retrying") {
		t.Fatalf("log %v", logged)
	}
}

func TestAnthropicDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(401)
		w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL, Backoff: time.Millisecond}).Complete(context.Background(), "", "x")
	if err == nil || calls != 1 || !strings.Contains(err.Error(), "authentication_error") {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestAnthropicTruncatedReplyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{\"content\":[{\"type\":\"text\",\"text\":\"```go\\npackage main\\nfunc ma\"}],\"stop_reason\":\"max_tokens\"}"))
	}))
	defer srv.Close()
	_, err := (&Anthropic{APIKey: "k", BaseURL: srv.URL}).Complete(context.Background(), "", "x")
	if !errors.Is(err, ErrTruncated) {
		t.Fatalf("err = %v", err)
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

func TestRecordThenReplay(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	live := Func(func(_ context.Context, system, prompt string) (string, error) {
		calls++
		return "reply to " + prompt, nil
	})
	rec := Recorder{Model: live, Dir: dir}
	if out, err := rec.Complete(context.Background(), "sys", "a"); err != nil || out != "reply to a" {
		t.Fatalf("record: %q %v", out, err)
	}
	rep := Replayer{FS: os.DirFS(dir)}
	if out, err := rep.Complete(context.Background(), "sys", "a"); err != nil || out != "reply to a" {
		t.Fatalf("replay: %q %v", out, err)
	}
	if _, err := rep.Complete(context.Background(), "sys", "b"); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a different prompt must not be answered: %v", err)
	}
	if _, err := rep.Complete(context.Background(), "other system", "a"); !errors.Is(err, ErrNotRecorded) {
		t.Fatalf("a different system prompt must not be answered: %v", err)
	}
	if calls != 1 {
		t.Fatalf("live model called %d times", calls)
	}
}
