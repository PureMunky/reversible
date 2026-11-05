package factory

import (
	"context"
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
