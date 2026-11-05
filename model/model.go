// Package model is the narrow interface between the runtime and a
// language model: text in, text out. Nothing a model returns is executed;
// callers parse it as data (see the policy package) and everything it
// proposes still goes through the replay gate.
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
	"strings"
	"time"
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

// Anthropic calls the Claude Messages API with no dependencies beyond the
// standard library.
type Anthropic struct {
	APIKey    string
	Model     string // default DefaultModel
	MaxTokens int    // default 4096
	BaseURL   string // default https://api.anthropic.com
	HTTP      *http.Client
}

// FromEnv builds a client from ANTHROPIC_API_KEY (and optionally
// REVERSIBLE_MODEL). ok is false if no key is set.
func FromEnv() (*Anthropic, bool) {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil, false
	}
	return &Anthropic{APIKey: key, Model: os.Getenv("REVERSIBLE_MODEL")}, true
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
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends one message and returns the text of the reply.
func (a *Anthropic) Complete(ctx context.Context, system, prompt string) (string, error) {
	if a.APIKey == "" {
		return "", errors.New("anthropic: no API key")
	}
	m, max, base := a.Model, a.MaxTokens, a.BaseURL
	if m == "" {
		m = DefaultModel
	}
	if max == 0 {
		max = 4096
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	hc := a.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	body, _ := json.Marshal(request{Model: m, MaxTokens: max, System: system, Messages: []message{{Role: "user", Content: prompt}}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", fmt.Errorf("anthropic: reading response: %w", err)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("anthropic: HTTP %d, unparseable response: %.200s", resp.StatusCode, raw)
	}
	if resp.StatusCode/100 != 2 || r.Error != nil {
		msg := strings.TrimSpace(string(raw))
		if r.Error != nil {
			msg = r.Error.Type + ": " + r.Error.Message
		}
		return "", fmt.Errorf("anthropic: HTTP %d: %s", resp.StatusCode, msg)
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
