package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scripted replies in order and records prompts.
type scripted struct {
	replies []string
	prompts []string
}

func (s *scripted) Complete(_ context.Context, _, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.replies) == 0 {
		return "", errors.New("no more replies")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func TestModelCoderRepairsItsOwnWork(t *testing.T) {
	s := spec(t, "flags-v1.json")
	good, _ := Codegen{}.Code(context.Background(), s, nil, nil)
	broken := strings.Replace(good["main.go"], `fail(w, 400, f+" is required")`, `fail(w, 400, "missing")`, 1)
	m := &scripted{replies: []string{
		"```go\npackage main\nfunc main() { this does not compile }\n```",
		"Here you go:\n```go\n" + broken + "```",
		"```go\n" + good["main.go"] + "```",
	}}
	v, err := (&Factory{Root: t.TempDir(), Coder: ModelCoder{Model: m}}).Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if v.Attempts != 3 || !v.Passed() {
		t.Fatalf("attempts=%d\n%s", v.Attempts, v.Report())
	}
	if !strings.Contains(m.prompts[0], "CONTRACT") || !strings.Contains(m.prompts[0], `"service": "flags"`) {
		t.Error("first prompt lacks the contract or the spec")
	}
	if !strings.Contains(m.prompts[1], "build failed") {
		t.Errorf("second prompt should report the build failure:\n%s", m.prompts[1])
	}
	if !strings.Contains(m.prompts[2], `every flag needs an owner: POST /flags → expected body to include {"error":"owner is required"}`) {
		t.Errorf("third prompt should report the failing example precisely")
	}
}

func TestModelCoderGivesUp(t *testing.T) {
	m := &scripted{replies: []string{"I'd rather not.", "", ""}}
	_, err := (&Factory{Root: t.TempDir(), Coder: ModelCoder{Model: m}, MaxAttempts: 2}).Build(context.Background(), spec(t, "flags-v1.json"))
	if err == nil {
		t.Fatal("expected failure")
	}
}

func TestExtractGo(t *testing.T) {
	for in, want := range map[string]string{
		"```go\npackage main\n```":           "package main\n",
		"text\n```golang\npackage main\n```": "package main\n",
		"package main\n\nfunc main() {}":     "package main\n\nfunc main() {}\n",
	} {
		got, err := extractGo(in)
		if err != nil || got != want {
			t.Errorf("%q → %q, %v", in, got, err)
		}
	}
	if _, err := extractGo("no code"); err == nil {
		t.Error("expected error")
	}
}
