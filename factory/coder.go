package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PureMunky/reversible/model"
)

// ModelCoder asks a language model to write the service. It is given the
// contract every service must meet and the full spec; on a retry it also
// gets its previous code and exactly which examples failed and how, so it
// repairs its own work. What it writes is only ever built and run as a
// separate process, and only released after it passes the examples.
type ModelCoder struct {
	Model model.Completer
}

// CoderSystemPrompt frames the model's job.
const CoderSystemPrompt = `You write small, correct Go services from a precise specification.
The specification is the source of truth; your code is a generated artifact that will be rebuilt whenever the specification changes, and nobody will edit it by hand.
Follow the contract exactly. Make every example pass. Use only the Go standard library.
Reply with the complete main.go in a single fenced code block marked go, and nothing else.`

func (m ModelCoder) Code(ctx context.Context, s ServiceSpec, previous map[string]string, failures []Result) (map[string]string, error) {
	if m.Model == nil {
		return nil, errors.New("ModelCoder needs a Model")
	}
	var b strings.Builder
	b.WriteString("CONTRACT\n")
	b.WriteString(Contract)
	b.WriteString("\n\nSPECIFICATION\n")
	b.WriteString(s.JSON())
	b.WriteString("\n")
	if prev := previous["main.go"]; prev != "" && len(failures) > 0 {
		b.WriteString("\nYOUR PREVIOUS main.go\n```go\n")
		b.WriteString(prev)
		b.WriteString("\n```\n\nIT FAILED THESE CHECKS\n")
		for _, f := range failures {
			if f.Example.Name != "" {
				fmt.Fprintf(&b, "- %s: %s %s → %s\n", f.Example.Name, f.Example.Request.Method, f.Example.Request.Path, f.Problem)
			} else {
				fmt.Fprintf(&b, "- %s\n", f.Problem)
			}
		}
		b.WriteString("\nFix every failure and return the complete corrected main.go.\n")
	} else {
		b.WriteString("\nWrite main.go.\n")
	}
	reply, err := m.Model.Complete(ctx, CoderSystemPrompt, b.String())
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	src, err := extractGo(reply)
	if err != nil {
		return nil, err
	}
	return map[string]string{"main.go": src}, nil
}

// extractGo returns the contents of the first ```go fenced block (or the
// whole reply if it is bare Go source).
func extractGo(reply string) (string, error) {
	for _, fence := range []string{"```go\n", "```golang\n", "```\n"} {
		if i := strings.Index(reply, fence); i >= 0 {
			rest := reply[i+len(fence):]
			if j := strings.Index(rest, "```"); j >= 0 {
				return strings.TrimSpace(rest[:j]) + "\n", nil
			}
		}
	}
	if strings.HasPrefix(strings.TrimSpace(reply), "package ") || strings.Contains(reply, "\npackage main") {
		return strings.TrimSpace(reply) + "\n", nil
	}
	return "", errors.New("model reply contains no Go code block")
}

// BehaviourCoder writes services whose spec has behaviours: logic stated
// in words that no generator can compile. The deterministic generator
// writes everything structural (main.go: types, storage, endpoints,
// validation, relations, lifecycles, access) and calls fixed hooks; the
// model writes only those hooks (behaviours.go), against the generated
// code. Its surface is the domain logic, not the plumbing. Specs without
// behaviours are compiled without a model.
type BehaviourCoder struct {
	Model model.Completer
}

// BehaviourSystemPrompt frames the model's job.
const BehaviourSystemPrompt = `You write the domain logic of a Go service whose structure is generated from a specification.
The specification is the source of truth; the generated main.go is fixed and calls the functions you write. Your file is a generated artifact too: it will be rewritten whenever the specification changes, and nobody will edit it by hand.
Implement exactly the listed functions, in package main, in one file behaviours.go, using only the Go standard library. Follow each behaviour's rule precisely, and make every example pass.
You may read other records through the generated store maps and sorted functions, but never write to them: only an action may change its own record, through the pointer it is given. No goroutines, file or network access, or reading the clock; anything time-dependent comes from the request or the record.
Reply with the complete behaviours.go in a single fenced code block marked go, and nothing else.`

func (c BehaviourCoder) Code(ctx context.Context, s ServiceSpec, previous map[string]string, failures []Result) (map[string]string, error) {
	if !s.HasBehaviours() {
		return Codegen{}.Code(ctx, s, previous, failures)
	}
	if qs := s.Check(); !Ready(qs) {
		return nil, fmt.Errorf("spec is not ready: %v", qs[0])
	}
	if c.Model == nil {
		return nil, errors.New("this spec has behaviours, which a model has to write: set ANTHROPIC_API_KEY (or replay recorded replies)")
	}
	structure := Structure(s)
	reply, err := c.Model.Complete(ctx, BehaviourSystemPrompt, BehaviourPrompt(s, structure, previous, failures))
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	src, err := extractGo(reply)
	if err != nil {
		return nil, err
	}
	return map[string]string{"main.go": structure, "behaviours.go": src}, nil
}

// BehaviourPrompt is what the model is sent: the behaviours to write and
// their examples, the full spec, the generated code they plug into, and on
// a retry its previous file and exactly what failed.
func BehaviourPrompt(s ServiceSpec, structure string, previous map[string]string, failures []Result) string {
	var b strings.Builder
	b.WriteString("FUNCTIONS TO WRITE (in behaviours.go)\n")
	b.WriteString(Hooks(s))
	b.WriteString("EXAMPLES THAT PIN THEM DOWN (run in order with all the other examples against a fresh instance)\n")
	for _, e := range s.Examples {
		if e.Behaviour == "" {
			continue
		}
		req, _ := json.Marshal(e.Request)
		exp, _ := json.Marshal(e.Expect)
		fmt.Fprintf(&b, "- [%s] %s: %s → %s\n", e.Behaviour, e.Name, req, exp)
	}
	b.WriteString("\nSPECIFICATION\n")
	b.WriteString(s.JSON())
	b.WriteString("\n\nGENERATED main.go (fixed; for reference: the types, stores and helpers you can use)\n```go\n")
	b.WriteString(structure)
	b.WriteString("```\n")
	if prev := previous["behaviours.go"]; prev != "" && len(failures) > 0 {
		b.WriteString("\nYOUR PREVIOUS behaviours.go\n```go\n")
		b.WriteString(prev)
		b.WriteString("\n```\n\nIT FAILED THESE CHECKS\n")
		for _, f := range failures {
			if f.Example.Name != "" {
				fmt.Fprintf(&b, "- %s: %s %s → %s\n", f.Example.Name, f.Example.Request.Method, f.Example.Request.Path, f.Problem)
			} else {
				fmt.Fprintf(&b, "- %s\n", f.Problem)
			}
		}
		b.WriteString("\nFix every failure and return the complete corrected behaviours.go.\n")
	} else {
		b.WriteString("\nWrite behaviours.go.\n")
	}
	return b.String()
}
