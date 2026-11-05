// Package counterfactual answers "what would this change have done?"
// before it does anything.
//
// Agent behaviour is modelled as a Program: given a request, it decides an
// Intent (the effects it wants to take). Real requests are recorded as they
// happen. To evaluate a candidate Program, every recorded request is
// replayed through both the current (baseline) and the candidate Program
// with every effect intercepted: nothing touches the world. The plans are
// diffed, checked against invariants, and summarised as a verdict.
//
// Testing stops being "did the suite pass" and becomes "here is exactly
// what would have happened differently, on real traffic".
package counterfactual

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/PureMunky/reversible"
)

// Request is one recorded input to a Program.
type Request struct {
	ID    string            `json:"id"`
	Input map[string]string `json:"input"`
}

// Program decides what to do for a request. It must only describe effects,
// never perform them: Do is never called during replay.
type Program func(ctx context.Context, req Request) (reversible.Intent, error)

// PlannedEffect is an intercepted effect: what would have happened.
type PlannedEffect struct {
	Name        string            `json:"name"`
	Class       reversible.Class  `json:"class"`
	Type        string            `json:"type,omitempty"`
	Params      reversible.Params `json:"params,omitempty"`
	Description string            `json:"description,omitempty"`
}

func (p PlannedEffect) String() string {
	s := p.Name
	if p.Type != "" {
		s += " [" + p.Type + "]"
	}
	if len(p.Params) > 0 {
		keys := make([]string, 0, len(p.Params))
		for k := range p.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var kv []string
		for _, k := range keys {
			kv = append(kv, k+"="+p.Params[k])
		}
		s += " {" + strings.Join(kv, ", ") + "}"
	}
	return s
}

func (p PlannedEffect) equal(o PlannedEffect) bool {
	return p.Name == o.Name && p.Class == o.Class && p.Type == o.Type && maps.Equal(p.Params, o.Params)
}

// Plan is everything a Program would have done for one request.
type Plan struct {
	Goal    string          `json:"goal,omitempty"`
	Effects []PlannedEffect `json:"effects"`
	Err     string          `json:"error,omitempty"`
}

// Irreversible lists the plan's irreversible effects.
func (p Plan) Irreversible() []PlannedEffect {
	var out []PlannedEffect
	for _, e := range p.Effects {
		if e.Class == reversible.Irreversible {
			out = append(out, e)
		}
	}
	return out
}

// Invariant is a rule every plan must satisfy, such as "never charge more
// than $500" or "never delete an active account". Check returns a non-nil
// error describing the violation.
type Invariant struct {
	Name  string
	Check func(req Request, plan Plan) error
}

// Simulate runs a Program for one request with every effect intercepted.
// The intent is validated exactly as the runtime would, so structural
// mistakes (a reversible effect with no Undo, a one-way door that isn't
// last) surface here instead of in production.
func Simulate(ctx context.Context, p Program, req Request) (plan Plan) {
	defer func() {
		if r := recover(); r != nil {
			plan = Plan{Err: fmt.Sprintf("panic: %v", r)}
		}
	}()
	in, err := p(ctx, req)
	if err != nil {
		return Plan{Err: err.Error()}
	}
	plan.Goal = in.Goal
	for _, e := range in.Effects {
		pe := PlannedEffect{Name: e.Name, Class: e.Class, Description: e.Description}
		if e.Spec != nil {
			pe.Type, pe.Params = e.Spec.Type, maps.Clone(e.Spec.Params)
		}
		plan.Effects = append(plan.Effects, pe)
	}
	if in.ID == "" {
		in.ID = "counterfactual-" + req.ID
	}
	if err := in.Validate(); err != nil {
		plan.Err = err.Error()
	}
	return plan
}

// Diff is how the candidate's plan differs from the baseline's for one request.
type Diff struct {
	Request   Request
	Baseline  Plan
	Candidate Plan
	Added     []PlannedEffect                    // in candidate only
	Removed   []PlannedEffect                    // in baseline only
	Changed   []struct{ From, To PlannedEffect } // same name, different class/type/params
	// NewViolations are invariant violations the candidate introduces
	// (ones the baseline also had are listed in Inherited).
	NewViolations []string
	Inherited     []string
	// NewIrreversible are one-way doors the candidate opens that the
	// baseline did not.
	NewIrreversible []PlannedEffect
	NewError        string
}

// Same reports whether the candidate behaves exactly like the baseline.
func (d Diff) Same() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0 &&
		d.Baseline.Err == d.Candidate.Err
}

// Policy decides what blocks promotion.
type Policy struct {
	// AllowNewIrreversible lets a candidate introduce irreversible effects
	// the baseline never took. Off by default: new one-way doors need a
	// human to look at them.
	AllowNewIrreversible bool
	// MaxChangedFraction blocks the candidate if more than this share of
	// requests behave differently (0 means no limit). Large blast radius
	// warrants a closer look even when every individual change is valid.
	MaxChangedFraction float64
}

// Report summarises a replay.
type Report struct {
	Total           int
	Same            int
	Changed         int
	CandidateErrors int
	NewViolations   int
	NewIrreversible int
	Diffs           []Diff // only requests where something differed or was flagged
	Blockers        []string
}

// Promote reports whether the candidate is safe to ship under the policy.
func (r Report) Promote() bool { return len(r.Blockers) == 0 }

// Replay runs every request through baseline and candidate with all effects
// intercepted, and reports what would have changed.
func Replay(ctx context.Context, reqs []Request, baseline, candidate Program, invariants []Invariant, pol Policy) Report {
	rep := Report{Total: len(reqs)}
	for _, req := range reqs {
		d := Diff{Request: req, Baseline: Simulate(ctx, baseline, req), Candidate: Simulate(ctx, candidate, req)}
		diffEffects(&d)

		baseViol := violations(req, d.Baseline, invariants)
		for v := range violations(req, d.Candidate, invariants) {
			if baseViol[v] {
				d.Inherited = append(d.Inherited, v)
			} else {
				d.NewViolations = append(d.NewViolations, v)
			}
		}
		sort.Strings(d.NewViolations)
		sort.Strings(d.Inherited)

		baseIrr := map[string]bool{}
		for _, e := range d.Baseline.Irreversible() {
			baseIrr[e.Name] = true
		}
		for _, e := range d.Candidate.Irreversible() {
			if !baseIrr[e.Name] {
				d.NewIrreversible = append(d.NewIrreversible, e)
			}
		}
		if d.Candidate.Err != "" && d.Baseline.Err == "" {
			d.NewError = d.Candidate.Err
		}

		if d.Same() {
			rep.Same++
		} else {
			rep.Changed++
		}
		if d.NewError != "" {
			rep.CandidateErrors++
		}
		if len(d.NewViolations) > 0 {
			rep.NewViolations++
		}
		if len(d.NewIrreversible) > 0 {
			rep.NewIrreversible++
		}
		if !d.Same() || len(d.NewViolations) > 0 || len(d.Inherited) > 0 {
			rep.Diffs = append(rep.Diffs, d)
		}
	}

	if rep.CandidateErrors > 0 {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("candidate fails on %d request(s) the baseline handles", rep.CandidateErrors))
	}
	if rep.NewViolations > 0 {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("candidate violates invariants on %d request(s)", rep.NewViolations))
	}
	if rep.NewIrreversible > 0 && !pol.AllowNewIrreversible {
		rep.Blockers = append(rep.Blockers, fmt.Sprintf("candidate introduces irreversible effects on %d request(s)", rep.NewIrreversible))
	}
	if pol.MaxChangedFraction > 0 && rep.Total > 0 {
		if frac := float64(rep.Changed) / float64(rep.Total); frac > pol.MaxChangedFraction {
			rep.Blockers = append(rep.Blockers, fmt.Sprintf("candidate changes %.0f%% of requests (limit %.0f%%)", frac*100, pol.MaxChangedFraction*100))
		}
	}
	return rep
}

// String renders the report for a person deciding whether to promote.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replayed %d requests: %d unchanged, %d changed\n", r.Total, r.Same, r.Changed)
	for _, d := range r.Diffs {
		fmt.Fprintf(&b, "  %s %s\n", d.Request.ID, formatInput(d.Request.Input))
		for _, e := range d.Removed {
			fmt.Fprintf(&b, "    - %s\n", e)
		}
		for _, e := range d.Added {
			fmt.Fprintf(&b, "    + %s\n", e)
		}
		for _, c := range d.Changed {
			fmt.Fprintf(&b, "    ~ %s  →  %s\n", c.From, c.To)
		}
		if d.NewError != "" {
			fmt.Fprintf(&b, "    ✗ error: %s\n", d.NewError)
		}
		for _, e := range d.NewIrreversible {
			fmt.Fprintf(&b, "    ⚠ new one-way door: %s\n", e.Name)
		}
		for _, v := range d.NewViolations {
			fmt.Fprintf(&b, "    ✗ violates %s\n", v)
		}
		for _, v := range d.Inherited {
			fmt.Fprintf(&b, "    · already violated by baseline: %s\n", v)
		}
	}
	if r.Promote() {
		b.WriteString("verdict: SAFE TO PROMOTE\n")
	} else {
		b.WriteString("verdict: BLOCKED\n")
		for _, x := range r.Blockers {
			fmt.Fprintf(&b, "  - %s\n", x)
		}
	}
	return b.String()
}

// JSON renders the report as JSON, for CI or another agent to consume.
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }

func diffEffects(d *Diff) {
	base := map[string]PlannedEffect{}
	for _, e := range d.Baseline.Effects {
		base[e.Name] = e
	}
	cand := map[string]bool{}
	for _, e := range d.Candidate.Effects {
		cand[e.Name] = true
		b, ok := base[e.Name]
		switch {
		case !ok:
			d.Added = append(d.Added, e)
		case !b.equal(e):
			d.Changed = append(d.Changed, struct{ From, To PlannedEffect }{b, e})
		}
	}
	for _, e := range d.Baseline.Effects {
		if !cand[e.Name] {
			d.Removed = append(d.Removed, e)
		}
	}
}

func violations(req Request, p Plan, invs []Invariant) map[string]bool {
	out := map[string]bool{}
	if p.Err != "" {
		return out
	}
	for _, inv := range invs {
		if err := inv.Check(req, p); err != nil {
			out[inv.Name+": "+err.Error()] = true
		}
	}
	return out
}

func formatInput(in map[string]string) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + in[k]
	}
	return "(" + strings.Join(parts, " ") + ")"
}
