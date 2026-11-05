package factory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/PureMunky/reversible/model"
)

// Turn is one message in the conversation that defines the software.
type Turn struct {
	Who  string `json:"who"`
	Says string `json:"says"`
}

// Captured is a spec drafted from a conversation, plus what the drafter
// wants to ask before anything is built.
type Captured struct {
	Spec      ServiceSpec
	Questions []string // the drafter's own questions
}

// CaptureSystemPrompt frames the model's job when turning a conversation
// into intent.
const CaptureSystemPrompt = `You turn conversations about software into a precise service specification.
The specification is the source of truth: code will be generated from it and nobody will edit that code, so anything left vague becomes a guess. Do not guess.
Capture every behaviour the people asked for as a field, rule or example; keep everything from the current specification that the conversation doesn't change; and list as questions anything that is ambiguous or missing.
Reply with JSON only: {"spec": {...}, "questions": ["..."]}.`

// Capture asks a model to draft (or update) the spec from a conversation.
// The result still goes through Check before anything is built.
func Capture(ctx context.Context, m model.Completer, conversation []Turn, current *ServiceSpec) (Captured, error) {
	if m == nil {
		return Captured{}, errors.New("capture needs a model")
	}
	var b strings.Builder
	b.WriteString("SPEC FORMAT (fields: string, bool, int, number; rules: forbid_delete_when; examples run in order against a fresh instance and are the contract)\n")
	if current != nil {
		b.WriteString("CURRENT SPECIFICATION\n" + current.JSON() + "\n")
	} else {
		example, _ := json.MarshalIndent(ServiceSpec{
			Service: "name", Purpose: "what it is for and who uses it", Owner: "accountable person",
			Resource: Resource{Name: "item", Plural: "items", Key: "id",
				Fields: []Field{{Name: "id", Type: "string", Required: true, Pattern: "^[a-z0-9-]+$"}, {Name: "done", Type: "bool", Default: false}},
				Rules:  []Rule{{ForbidDeleteWhen: &Condition{Field: "done", Equals: false}, Why: "why the rule exists"}}},
			Examples: []Example{{Name: "create", Request: HTTPCall{Method: "POST", Path: "/items", Body: map[string]any{"id": "a"}},
				Expect: Expectancy{Status: 201, Body: map[string]any{"id": "a", "done": false}}}},
		}, "", "  ")
		b.WriteString("EXAMPLE OF THE FORMAT\n" + string(example) + "\n")
	}
	b.WriteString("\nSERVICE CONTRACT (what every generated service does)\n" + Contract + "\n\nCONVERSATION\n")
	for _, t := range conversation {
		fmt.Fprintf(&b, "%s: %s\n", t.Who, t.Says)
	}
	if current != nil {
		b.WriteString("\nReturn the updated specification and your questions.\n")
	} else {
		b.WriteString("\nReturn the specification and your questions.\n")
	}

	reply, err := m.Complete(ctx, CaptureSystemPrompt, b.String())
	if err != nil {
		return Captured{}, fmt.Errorf("model: %w", err)
	}
	raw, err := model.ExtractJSON(reply)
	if err != nil {
		return Captured{}, err
	}
	var out struct {
		Spec      json.RawMessage `json:"spec"`
		Questions []string        `json:"questions"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return Captured{}, fmt.Errorf("capture reply: %w", err)
	}
	spec, err := ParseSpec(out.Spec)
	if err != nil {
		return Captured{}, err
	}
	return Captured{Spec: spec, Questions: out.Questions}, nil
}
