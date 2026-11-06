// Package model is the narrow interface between the runtime and a
// language model: text in, text out. What callers do with the text
// differs: policies are parsed as data and replayed through the gate
// (policy, synth); specs are parsed strictly and checked (factory.Capture);
// code is compiled and run as a separate process, and only released after
// its behaviour is checked (factory.ModelCoder). Run that last one in a
// container: see docs/USER-GUIDE.md.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PureMunky/reversible/telemetry"
)

// Completer turns a system prompt and a user prompt into a reply.
type Completer interface {
	Complete(ctx context.Context, system, prompt string) (string, error)
}

// Func adapts a function to Completer (handy for tests and stand-ins).
type Func func(ctx context.Context, system, prompt string) (string, error)

func (f Func) Complete(ctx context.Context, system, prompt string) (string, error) {
	return f(ctx, system, prompt)
}

// DefaultModel is used when Anthropic.Model is empty.
const DefaultModel = "claude-sonnet-5-5"

// DefaultMaxTokens leaves room for a whole generated service in one reply
// (a few hundred lines of Go is several thousand tokens).
const DefaultMaxTokens = 16000

// Anthropic calls the Claude Messages API with no dependencies beyond the
// standard library.
type Anthropic struct {
	APIKey    string
	Model     string // default DefaultModel
	MaxTokens int    // default DefaultMaxTokens
	BaseURL   string // default https://api.anthropic.com
	HTTP      *http.Client
	Retries   int           // on 429, 5xx and network errors (default 3; negative for none)
	Backoff   time.Duration // first retry delay, doubling (default 2s)
	// Log, if set, gets one line per call: model, tokens, time.
	Log func(string)

	mu    sync.Mutex
	usage Usage
}

// Usage totals what calls have cost so far.
type Usage struct {
	Calls        int
	InputTokens  int
	OutputTokens int
}

func (u Usage) String() string {
	return fmt.Sprintf("%d call(s), %d input + %d output tokens", u.Calls, u.InputTokens, u.OutputTokens)
}

// Usage returns the totals so far.
func (a *Anthropic) Usage() Usage {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.usage
}

// FromEnv builds a client from ANTHROPIC_API_KEY, and optionally
// ANTHROPIC_BASE_URL (e.g. the cmd/fakeclaude rehearsal server),
// REVERSIBLE_MODEL and REVERSIBLE_MAX_TOKENS. ok is false if no key is set.
func FromEnv() (*Anthropic, bool) {
	key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY"))
	if key == "" {
		return nil, false
	}
	a := &Anthropic{APIKey: key, Model: os.Getenv("REVERSIBLE_MODEL"), BaseURL: os.Getenv("ANTHROPIC_BASE_URL")}
	if n, err := strconv.Atoi(os.Getenv("REVERSIBLE_MAX_TOKENS")); err == nil && n > 0 {
		a.MaxTokens = n
	}
	return a, true
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type request struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type response struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

var (
	mCalls  = telemetry.Default.Counter("reversible_model_calls_total", "Model API calls by outcome: ok, retried (rate limit, overload or network; retried with backoff), error, truncated (hit max_tokens).", "model", "outcome")
	mTokens = telemetry.Default.Counter("reversible_model_tokens_total", "Model tokens by direction (input, output).", "model", "direction")
	mSecs   = telemetry.Default.Histogram("reversible_model_call_seconds", "Model call duration.", []float64{1, 5, 10, 30, 60, 120, 300, 600}, "model")
)

// ErrTruncated means the reply hit the token limit and is incomplete.
var ErrTruncated = errors.New("anthropic: reply cut off at max_tokens; raise REVERSIBLE_MAX_TOKENS")

// Complete sends one message and returns the text of the reply. Rate
// limits, overload and network errors are retried with backoff; a reply
// cut off at the token limit is an error, never returned as if complete.
func (a *Anthropic) Complete(ctx context.Context, system, prompt string) (string, error) {
	if a.APIKey == "" {
		return "", errors.New("anthropic: no API key")
	}
	m, max, base := a.Model, a.MaxTokens, a.BaseURL
	if m == "" {
		m = DefaultModel
	}
	if max == 0 {
		max = DefaultMaxTokens
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute} // a long reply, unstreamed, takes minutes
	}
	retries, backoff := a.Retries, a.Backoff
	switch {
	case retries == 0:
		retries = 3
	case retries < 0: // no retries
		retries = 0
	}
	if backoff == 0 {
		backoff = 2 * time.Second
	}
	body, _ := json.Marshal(request{Model: m, MaxTokens: max, System: system, Messages: []message{{Role: "user", Content: prompt}}})

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(backoff << (attempt - 1)):
			}
		}
		start := time.Now()
		r, status, err := a.send(ctx, hc, base, body)
		mSecs.Observe(time.Since(start).Seconds(), m)
		if err != nil {
			lastErr = err
			if status == 0 || status == 429 || status >= 500 {
				mCalls.Inc(m, "retried")
				telemetry.Emit("model.call", telemetry.F{"model": m, "outcome": "retried", "status": status, "error": err.Error(), "attempt": attempt + 1})
				if a.Log != nil {
					a.Log(fmt.Sprintf("model %s: %v (retrying)", m, err))
				}
				continue
			}
			mCalls.Inc(m, "error")
			telemetry.Emit("model.call", telemetry.F{"model": m, "outcome": "error", "status": status, "error": err.Error()})
			return "", err
		}
		mTokens.Add(float64(r.Usage.InputTokens), m, "input")
		mTokens.Add(float64(r.Usage.OutputTokens), m, "output")
		outcome := "ok"
		if r.StopReason == "max_tokens" {
			outcome = "truncated"
		}
		mCalls.Inc(m, outcome)
		telemetry.Emit("model.call", telemetry.F{"model": m, "outcome": outcome, "input_tokens": r.Usage.InputTokens, "output_tokens": r.Usage.OutputTokens,
			"seconds": time.Since(start).Seconds(), "stop_reason": r.StopReason})
		a.mu.Lock()
		a.usage.Calls++
		a.usage.InputTokens += r.Usage.InputTokens
		a.usage.OutputTokens += r.Usage.OutputTokens
		a.mu.Unlock()
		if a.Log != nil {
			a.Log(fmt.Sprintf("model %s: %d in / %d out tokens, %s", m, r.Usage.InputTokens, r.Usage.OutputTokens, time.Since(start).Round(time.Second)))
		}
		if r.StopReason == "max_tokens" {
			return "", ErrTruncated
		}
		var out strings.Builder
		for _, c := range r.Content {
			if c.Type == "text" {
				out.WriteString(c.Text)
			}
		}
		if out.Len() == 0 {
			return "", fmt.Errorf("anthropic: empty reply (stop_reason %q)", r.StopReason)
		}
		return out.String(), nil
	}
	return "", lastErr
}

// send makes one request. status is 0 for network errors.
func (a *Anthropic) send(ctx context.Context, hc *http.Client, base string, body []byte) (response, int, error) {
	var r response
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return r, -1, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return r, -1, ctx.Err()
		}
		return r, 0, fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return r, 0, fmt.Errorf("anthropic: reading response: %w", err)
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, resp.StatusCode, fmt.Errorf("anthropic: HTTP %d, unparseable response: %.200s", resp.StatusCode, raw)
	}
	if resp.StatusCode/100 != 2 || r.Error != nil {
		msg := strings.TrimSpace(string(raw))
		if r.Error != nil {
			msg = r.Error.Type + ": " + r.Error.Message
		}
		return r, resp.StatusCode, fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, msg)
	}
	return r, resp.StatusCode, nil
}

// ExtractJSON returns the first top-level JSON object in a model reply,
// tolerating prose and code fences around it.
func ExtractJSON(s string) (string, error) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", errors.New("no JSON object in reply")
	}
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", errors.New("unterminated JSON object in reply")
}
