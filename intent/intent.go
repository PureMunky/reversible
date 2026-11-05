// Package intent compiles a declarative statement of what a person wants,
// and what must never happen, into the checks the counterfactual runtime
// enforces.
//
// A Spec is written by a human. Compile turns it into invariants,
// guardrails and a promotion policy. Anything ambiguous, contradictory or
// probably mistyped is not guessed at: it comes back as a Question, and a
// spec with blocking questions is not Ready.
//
//	{
//	  "goal": "grow revenue without hurting retention",
//	  "owner": "phil",
//	  "rules": [
//	    {"type": "card.charge", "param": "amount", "max": 5000},
//	    {"type": "account.purge", "forbid": true},
//	    {"type": "card.charge", "requires": "email.send"}
//	  ],
//	  "guardrails": [
//	    {"metric": "revenue", "no_drop_over_pct": 5},
//	    {"metric": "churn", "no_rise_over_pct": 10}
//	  ],
//	  "policy": {"max_changed_pct": 25}
//	}
package intent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
)

// Spec is a human's statement of intent.
type Spec struct {
	Goal       string      `json:"goal"`
	Owner      string      `json:"owner,omitempty"`
	Rules      []Rule      `json:"rules,omitempty"`
	Guardrails []Guardrail `json:"guardrails,omitempty"`
	Policy     Policy      `json:"policy,omitempty"`
}

// Rule constrains the effects a plan may contain. Exactly one kind of
// constraint per rule:
//
//   - forbid:        effects of Type must never be planned
//   - min / max:     numeric Param of Type must stay within bounds
//   - requires:      a plan with Type must also include the Requires type
//   - at_most:       no more than AtMost effects of Type per plan
//   - class:         Type must be registered with this class
type Rule struct {
	Type     string   `json:"type"`
	Forbid   bool     `json:"forbid,omitempty"`
	Param    string   `json:"param,omitempty"`
	Min      *float64 `json:"min,omitempty"`
	Max      *float64 `json:"max,omitempty"`
	Requires string   `json:"requires,omitempty"`
	AtMost   *int     `json:"at_most,omitempty"`
	Class    string   `json:"class,omitempty"`
	Why      string   `json:"why,omitempty"` // the human's reason, carried into violations
}

// Guardrail limits a predicted metric's movement.
type Guardrail struct {
	Metric        string   `json:"metric"`
	NoDropOverPct *float64 `json:"no_drop_over_pct,omitempty"`
	NoRiseOverPct *float64 `json:"no_rise_over_pct,omitempty"`
}

// Policy mirrors counterfactual.Policy in human units.
type Policy struct {
	AllowNewIrreversible bool    `json:"allow_new_irreversible,omitempty"`
	MaxChangedPct        float64 `json:"max_changed_pct,omitempty"`
}

// Parse reads a spec, rejecting unknown fields (usually typos that would
// otherwise silently do nothing).
func Parse(b []byte) (Spec, error) {
	var s Spec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("intent spec: %w", err)
	}
	return s, nil
}

// Load reads and parses a spec file.
func Load(path string) (Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Spec{}, err
	}
	return Parse(b)
}

// Severity says whether a question stops compilation.
type Severity string

const (
	// Blocking: contradictory or unusable; the spec is not Ready.
	Blocking Severity = "blocking"
	// Clarify: probably fine, but a human should confirm.
	Clarify Severity = "clarify"
)

// Question is something the compiler won't guess about.
type Question struct {
	Severity Severity
	Where    string // which part of the spec
	Ask      string
}

func (q Question) String() string { return fmt.Sprintf("[%s] %s: %s", q.Severity, q.Where, q.Ask) }

// Compiled is a spec turned into enforceable checks.
type Compiled struct {
	Goal       string
	Owner      string
	Invariants []counterfactual.Invariant
	Guardrails []counterfactual.Guardrail
	Policy     counterfactual.Policy
	Questions  []Question
}

// Ready reports whether there are no blocking questions.
func (c Compiled) Ready() bool {
	for _, q := range c.Questions {
		if q.Severity == Blocking {
			return false
		}
	}
	return true
}

// Config applies the compiled checks to a replay configuration.
func (c Compiled) Config(base counterfactual.Config) counterfactual.Config {
	base.Invariants = append(append([]counterfactual.Invariant(nil), base.Invariants...), c.Invariants...)
	base.Guardrails = append(append([]counterfactual.Guardrail(nil), base.Guardrails...), c.Guardrails...)
	base.Policy = c.Policy
	return base
}

// Known tells the compiler what exists, so it can catch rules about
// effect types or metrics nobody has defined.
type Known struct {
	Types   map[string]reversible.Class // usually Registry.Types()
	Metrics []string                    // metrics your simulators produce
}

// Compile turns a spec into checks, asking rather than guessing.
func Compile(s Spec, known Known) Compiled {
	c := Compiled{Goal: s.Goal, Owner: s.Owner}
	ask := func(sev Severity, where, format string, args ...any) {
		c.Questions = append(c.Questions, Question{Severity: sev, Where: where, Ask: fmt.Sprintf(format, args...)})
	}

	if strings.TrimSpace(s.Goal) == "" {
		ask(Blocking, "goal", "what is this for? A spec with no goal can't be traced or evaluated")
	}
	if s.Owner == "" {
		ask(Clarify, "owner", "who is accountable for this intent?")
	}
	if len(s.Rules) == 0 && len(s.Guardrails) == 0 {
		ask(Clarify, "spec", "nothing constrains this intent; any plan is acceptable. Is that what you mean?")
	}

	forbidden := map[string]bool{}
	for _, r := range s.Rules {
		if r.Forbid {
			forbidden[r.Type] = true
		}
	}
	type bound struct{ min, max *float64 }
	bounds := map[string]*bound{}

	for i, r := range s.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		if r.Type == "" {
			ask(Blocking, where, "which effect type does this rule apply to?")
			continue
		}
		where += " (" + r.Type + ")"
		if known.Types != nil {
			if _, ok := known.Types[r.Type]; !ok {
				msg := fmt.Sprintf("no effect type %q is registered", r.Type)
				if s := closest(r.Type, keys(known.Types)); s != "" {
					msg += fmt.Sprintf("; did you mean %q?", s)
				}
				ask(Blocking, where, "%s", msg)
				continue
			}
		}

		kinds := 0
		if r.Forbid {
			kinds++
		}
		if r.Min != nil || r.Max != nil || r.Param != "" {
			kinds++
		}
		if r.Requires != "" {
			kinds++
		}
		if r.AtMost != nil {
			kinds++
		}
		if r.Class != "" {
			kinds++
		}
		switch {
		case kinds == 0:
			ask(Blocking, where, "this rule names a type but no constraint (forbid, min/max, requires, at_most or class)")
			continue
		case kinds > 1:
			ask(Blocking, where, "this rule mixes several constraints; split it into one rule per constraint so each can be explained")
			continue
		}

		why := r.Why
		switch {
		case r.Forbid:
			c.Invariants = append(c.Invariants, forbidRule(r.Type, why))
			if known.Types[r.Type] == reversible.Irreversible && s.Policy.AllowNewIrreversible {
				ask(Clarify, where, "policy allows new irreversible effects but this one is forbidden outright; forbid wins, confirm that's intended")
			}

		case r.Param != "" || r.Min != nil || r.Max != nil:
			if r.Param == "" {
				ask(Blocking, where, "min/max given but no param: which parameter is bounded?")
				continue
			}
			if r.Min == nil && r.Max == nil {
				ask(Blocking, where, "param %q named but no min or max", r.Param)
				continue
			}
			key := r.Type + "." + r.Param
			b := bounds[key]
			if b == nil {
				b = &bound{}
				bounds[key] = b
			}
			if r.Min != nil {
				if b.min != nil && *b.min != *r.Min {
					ask(Blocking, where, "%s already has min %g; which minimum is right, %g or %g?", key, *b.min, *b.min, *r.Min)
				}
				b.min = r.Min
			}
			if r.Max != nil {
				if b.max != nil && *b.max != *r.Max {
					ask(Blocking, where, "%s already has max %g; which maximum is right, %g or %g?", key, *b.max, *b.max, *r.Max)
				}
				b.max = r.Max
			}
			if b.min != nil && b.max != nil && *b.min > *b.max {
				ask(Blocking, where, "%s must be at least %g and at most %g; no value satisfies both", key, *b.min, *b.max)
				continue
			}
			if forbidden[r.Type] {
				ask(Clarify, where, "%s is forbidden, so bounding its %s never applies; remove one of the rules?", r.Type, r.Param)
			}
			c.Invariants = append(c.Invariants, boundRule(r.Type, r.Param, r.Min, r.Max, why))

		case r.Requires != "":
			if known.Types != nil {
				if _, ok := known.Types[r.Requires]; !ok {
					ask(Blocking, where, "requires %q, which is not a registered effect type", r.Requires)
					continue
				}
			}
			if forbidden[r.Requires] && !forbidden[r.Type] {
				ask(Blocking, where, "%s requires %s, but %s is forbidden, so %s can never be planned; is that the intent?", r.Type, r.Requires, r.Requires, r.Type)
				continue
			}
			c.Invariants = append(c.Invariants, requiresRule(r.Type, r.Requires, why))

		case r.AtMost != nil:
			if *r.AtMost < 0 {
				ask(Blocking, where, "at_most must be zero or more")
				continue
			}
			if *r.AtMost == 0 {
				ask(Clarify, where, "at_most 0 is the same as forbid; use forbid to say so directly")
			}
			c.Invariants = append(c.Invariants, atMostRule(r.Type, *r.AtMost, why))

		case r.Class != "":
			var want reversible.Class
			if err := want.UnmarshalText([]byte(r.Class)); err != nil {
				ask(Blocking, where, "class must be reversible, escrowed or irreversible, not %q", r.Class)
				continue
			}
			if got, ok := known.Types[r.Type]; ok && got != want {
				ask(Blocking, where, "%s is registered as %s but the spec requires %s; change the handler or the spec", r.Type, got, want)
				continue
			}
			c.Invariants = append(c.Invariants, classRule(r.Type, want, why))
		}
	}

	knownMetric := map[string]bool{}
	for _, m := range known.Metrics {
		knownMetric[m] = true
	}
	for i, g := range s.Guardrails {
		where := fmt.Sprintf("guardrails[%d]", i)
		if g.Metric == "" {
			ask(Blocking, where, "which metric does this guardrail protect?")
			continue
		}
		where += " (" + g.Metric + ")"
		if known.Metrics != nil && !knownMetric[g.Metric] {
			msg := fmt.Sprintf("no simulator predicts %q, so this guardrail could never fire", g.Metric)
			if s := closest(g.Metric, known.Metrics); s != "" {
				msg += fmt.Sprintf("; did you mean %q?", s)
			}
			ask(Blocking, where, "%s", msg)
			continue
		}
		if g.NoDropOverPct == nil && g.NoRiseOverPct == nil {
			ask(Blocking, where, "protect it how? Give no_drop_over_pct and/or no_rise_over_pct")
			continue
		}
		if g.NoDropOverPct != nil {
			if *g.NoDropOverPct < 0 {
				ask(Blocking, where, "no_drop_over_pct can't be negative")
				continue
			}
			c.Guardrails = append(c.Guardrails, counterfactual.NoDropOver(g.Metric, *g.NoDropOverPct))
		}
		if g.NoRiseOverPct != nil {
			if *g.NoRiseOverPct < 0 {
				ask(Blocking, where, "no_rise_over_pct can't be negative")
				continue
			}
			c.Guardrails = append(c.Guardrails, counterfactual.NoRiseOver(g.Metric, *g.NoRiseOverPct))
		}
	}

	if s.Policy.MaxChangedPct < 0 || s.Policy.MaxChangedPct > 100 {
		ask(Blocking, "policy.max_changed_pct", "must be between 0 and 100")
	}
	if s.Policy.AllowNewIrreversible {
		ask(Clarify, "policy.allow_new_irreversible", "candidates may open new one-way doors without a human looking; confirm that's intended")
	}
	c.Policy = counterfactual.Policy{AllowNewIrreversible: s.Policy.AllowNewIrreversible, MaxChangedFraction: s.Policy.MaxChangedPct / 100}

	sort.SliceStable(c.Questions, func(i, j int) bool {
		return c.Questions[i].Severity == Blocking && c.Questions[j].Severity != Blocking
	})
	return c
}

// --- rule implementations ---

func because(why string) string {
	if why == "" {
		return ""
	}
	return " (" + why + ")"
}

func forbidRule(typ, why string) counterfactual.Invariant {
	return counterfactual.Invariant{Name: "forbid " + typ, Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		for _, e := range p.Effects {
			if e.Type == typ {
				return fmt.Errorf("plans %s%s", e.Name, because(why))
			}
		}
		return nil
	}}
}

func boundRule(typ, param string, min, max *float64, why string) counterfactual.Invariant {
	var parts []string
	if min != nil {
		parts = append(parts, fmt.Sprintf(">=%g", *min))
	}
	if max != nil {
		parts = append(parts, fmt.Sprintf("<=%g", *max))
	}
	name := fmt.Sprintf("%s.%s %s", typ, param, strings.Join(parts, " "))
	return counterfactual.Invariant{Name: name, Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		for _, e := range p.Effects {
			if e.Type != typ {
				continue
			}
			raw, ok := e.Params[param]
			if !ok {
				return fmt.Errorf("%s has no %s to check", e.Name, param)
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return fmt.Errorf("%s.%s = %q is not a number", e.Name, param, raw)
			}
			if min != nil && v < *min {
				return fmt.Errorf("%s.%s = %g%s", e.Name, param, v, because(why))
			}
			if max != nil && v > *max {
				return fmt.Errorf("%s.%s = %g%s", e.Name, param, v, because(why))
			}
		}
		return nil
	}}
}

func requiresRule(typ, req, why string) counterfactual.Invariant {
	return counterfactual.Invariant{Name: typ + " requires " + req, Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		has, needs := false, false
		for _, e := range p.Effects {
			has = has || e.Type == req
			needs = needs || e.Type == typ
		}
		if needs && !has {
			return fmt.Errorf("plans %s without %s%s", typ, req, because(why))
		}
		return nil
	}}
}

func atMostRule(typ string, n int, why string) counterfactual.Invariant {
	return counterfactual.Invariant{Name: fmt.Sprintf("at most %d %s", n, typ), Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		count := 0
		for _, e := range p.Effects {
			if e.Type == typ {
				count++
			}
		}
		if count > n {
			return fmt.Errorf("plans %d%s", count, because(why))
		}
		return nil
	}}
}

func classRule(typ string, want reversible.Class, why string) counterfactual.Invariant {
	return counterfactual.Invariant{Name: fmt.Sprintf("%s is %s", typ, want), Check: func(_ counterfactual.Request, p counterfactual.Plan) error {
		for _, e := range p.Effects {
			if e.Type == typ && e.Class != want {
				return fmt.Errorf("%s is %s%s", e.Name, e.Class, because(why))
			}
		}
		return nil
	}}
}

// --- helpers ---

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// closest returns the candidate within a small edit distance, if any.
func closest(s string, candidates []string) string {
	best, bestD := "", 1<<30
	for _, c := range candidates {
		if d := editDistance(s, c); d < bestD {
			best, bestD = c, d
		}
	}
	if bestD <= max(2, len(s)/4) {
		return best
	}
	return ""
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
