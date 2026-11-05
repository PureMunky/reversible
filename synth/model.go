package synth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/intent"
	"github.com/PureMunky/reversible/model"
	"github.com/PureMunky/reversible/policy"
)

// ModelGenerator asks a language model for new policies. It gives the
// model the intent spec (with every rule's "why"), the registered effect
// types, example requests, the champion's policy and every trial so far
// with its observed outcomes, and asks for new policies as JSON.
//
// The model writes data, not code: each proposal is parsed as a Policy,
// validated against the registry and compiled. A proposal that doesn't
// parse or validate still becomes a candidate (with a program that fails),
// so the gate rejects it and the ledger records what the model tried and
// why it was refused. Nothing a model writes reaches a customer without
// passing the same gate as everything else.
type ModelGenerator struct {
	Model    model.Completer
	Spec     intent.Spec
	Registry *reversible.Registry
	// Examples are a few recorded requests, so the model knows which
	// inputs exist and what they look like.
	Examples []counterfactual.Request
	// PerStep is how many candidates to ask for (default 2).
	PerStep int
	// Metrics to show the model for each trial (default: all observed).
	Metrics []string

	mu       sync.Mutex
	policies map[string]policy.Policy
	log      []Exchange
}

// Exchange is one prompt and reply, kept for audit.
type Exchange struct {
	Prompt, Reply string
	Err           error
}

// Register records the policy behind a variant name, typically the
// starting champion's.
func (g *ModelGenerator) Register(name string, p policy.Policy) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.policies == nil {
		g.policies = map[string]policy.Policy{}
	}
	g.policies[name] = p
}

// Policy returns the policy behind a variant name.
func (g *ModelGenerator) Policy(name string) (policy.Policy, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.policies[name]
	return p, ok
}

// Transcript returns every prompt and reply so far.
func (g *ModelGenerator) Transcript() []Exchange {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]Exchange(nil), g.log...)
}

// SystemPrompt frames the model's job.
const SystemPrompt = `You design pricing and billing policies for a software system that runs them safely.
Every policy you propose is replayed against recorded traffic and checked against the owner's rules before any customer sees it, then tested live against the current champion; harmful experiments are rolled back automatically.
Propose policies that are likely to beat the champion on the goal without breaking any rule or guardrail. Learn from the trial history: build on what worked, avoid what failed, and say what you expect and why.
Reply with JSON only.`

// Propose implements Generator.
func (g *ModelGenerator) Propose(ctx context.Context, s State) ([]Candidate, error) {
	if g.Model == nil || g.Registry == nil {
		return nil, errors.New("ModelGenerator needs a Model and a Registry")
	}
	champ, ok := g.Policy(s.Champion)
	if !ok {
		return nil, fmt.Errorf("no policy registered for champion %q", s.Champion)
	}
	prompt := g.prompt(s, champ)
	reply, err := g.Model.Complete(ctx, SystemPrompt, prompt)
	g.mu.Lock()
	g.log = append(g.log, Exchange{Prompt: prompt, Reply: reply, Err: err})
	g.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("model: %w", err)
	}
	raw, err := model.ExtractJSON(reply)
	if err != nil {
		return nil, fmt.Errorf("model reply: %w", err)
	}
	var parsed struct {
		Candidates []struct {
			Name      string          `json:"name"`
			Rationale string          `json:"rationale"`
			Policy    json.RawMessage `json:"policy"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("model reply: %w", err)
	}

	taken := map[string]bool{}
	for _, t := range s.Trials {
		taken[t.Name] = true
	}
	var out []Candidate
	for i, c := range parsed.Candidates {
		name := sanitize(strings.TrimSpace(c.Name))
		if name == "" {
			name = fmt.Sprintf("model-%d", len(s.Trials)+i)
		}
		for base, n := name, 2; taken[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		taken[name] = true

		pol, perr := policy.Parse(c.Policy)
		if perr == nil {
			pol.Name = name
		}
		var prog counterfactual.Program
		if perr == nil {
			prog, perr = policy.Compile(pol, g.Registry, "bill")
		}
		rationale := strings.TrimSpace(c.Rationale)
		if perr != nil {
			// Keep it as a candidate so the gate rejects it on the record.
			bad := perr
			prog = func(context.Context, counterfactual.Request) (reversible.Intent, error) {
				return reversible.Intent{}, fmt.Errorf("invalid policy from model: %w", bad)
			}
			rationale += " [invalid: " + oneLine(perr.Error()) + "]"
		} else {
			g.Register(name, pol)
		}
		out = append(out, Candidate{Name: name, Program: prog, Rationale: rationale})
	}
	return out, nil
}

func (g *ModelGenerator) prompt(s State, champ policy.Policy) string {
	per := g.PerStep
	if per == 0 {
		per = 2
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w("GOAL: %s (owner: %s)\n\n", g.Spec.Goal, g.Spec.Owner)
	w("RULES every policy must satisfy (violations are rejected before launch):\n")
	for _, r := range g.Spec.Rules {
		w("- %s\n", describeRule(r))
	}
	if len(g.Spec.Guardrails) > 0 {
		w("\nGUARDRAILS (checked on predicted and on observed outcomes; a breach retires the policy and rolls back what it did):\n")
		for _, gr := range g.Spec.Guardrails {
			if gr.NoDropOverPct != nil {
				w("- %s must not fall more than %g%% vs the champion\n", gr.Metric, *gr.NoDropOverPct)
			}
			if gr.NoRiseOverPct != nil {
				w("- %s must not rise more than %g%% vs the champion\n", gr.Metric, *gr.NoRiseOverPct)
			}
		}
	}
	if g.Spec.Policy.MaxChangedPct > 0 {
		w("- a policy may change at most %g%% of requests vs the champion\n", g.Spec.Policy.MaxChangedPct)
	}

	w("\nEFFECT TYPES you may use:\n")
	types := g.Registry.Types()
	names := make([]string, 0, len(types))
	for t := range types {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		w("- %s (%s)\n", t, types[t])
	}

	if len(g.Examples) > 0 {
		w("\nEXAMPLE REQUESTS (inputs available to conditions and ${input.NAME}):\n")
		for _, r := range g.Examples {
			eb, _ := json.Marshal(r.Input)
			w("- %s\n", eb)
		}
	}

	w("\nPOLICY LANGUAGE: {\"vars\": {NAME: number}, \"rules\": [{\"when\": [{\"input\": NAME, \"op\": OP, \"value\": STRING}], \"effects\": [{\"name\": NAME, \"type\": EFFECT_TYPE, \"params\": {KEY: STRING}}]}]}\n")
	w("- rules are checked in order; the first whose conditions all hold applies; the last rule must have no \"when\" (the default)\n")
	w("- ops: == != < <= > >= (numeric) and in (comma-separated list)\n")
	w("- params may contain ${input.NAME} and ${var.NAME}\n")

	w("\nCURRENT CHAMPION %q: %s\n", s.Champion, champ.JSON())
	w("\nTRIALS SO FAR (newest last):\n")
	for _, t := range s.Trials {
		w("- %s [%s]", t.Name, t.Status)
		if t.Samples > 0 {
			w(" n=%d %s", t.Samples, g.formatAverages(t.Averages))
		}
		if p, ok := g.Policy(t.Name); ok {
			w("\n    policy: %s", p.Summary())
		}
		if t.Rationale != "" {
			w("\n    hypothesis: %s", oneLine(t.Rationale))
		}
		if t.Reason != "" {
			w("\n    result: %s", oneLine(t.Reason))
		}
		w("\n")
	}

	w("\nPropose %d new policies that differ from every trial above. Reply with only this JSON:\n", per)
	w(`{"candidates": [{"name": "short-kebab-name", "rationale": "hypothesis and the evidence behind it", "policy": {...}}]}`)
	w("\n")
	return b.String()
}

func (g *ModelGenerator) formatAverages(a map[string]float64) string {
	keys := g.Metrics
	if len(keys) == 0 {
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
	}
	var parts []string
	for _, k := range keys {
		if v, ok := a[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.4g", k, v))
		}
	}
	return strings.Join(parts, " ")
}

func describeRule(r intent.Rule) string {
	var s string
	switch {
	case r.Forbid:
		s = "never plan " + r.Type
	case r.Requires != "":
		s = "every " + r.Type + " must be accompanied by " + r.Requires
	case r.AtMost != nil:
		s = fmt.Sprintf("at most %d %s per request", *r.AtMost, r.Type)
	case r.Class != "":
		s = r.Type + " must be " + r.Class
	default:
		s = r.Type + "." + r.Param
		if r.Min != nil {
			s += fmt.Sprintf(" >= %g", *r.Min)
		}
		if r.Max != nil {
			s += fmt.Sprintf(" <= %g", *r.Max)
		}
	}
	if r.Why != "" {
		s += " (" + r.Why + ")"
	}
	return s
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	return s
}
