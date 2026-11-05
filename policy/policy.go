// Package policy is a small, data-only language for agent behaviour.
//
// A model (or a person) writes a Policy as JSON: named variables, then
// ordered rules. The first rule whose conditions all hold decides which
// effects to plan for the request. Compile turns a policy into a
// counterfactual.Program, so it can be replayed, gated, evolved and
// rewound like any other.
//
// Policies are data, not code. A model proposing one can't run anything,
// can't reach the network, and can't produce an effect type that isn't
// registered. Mistakes are caught by Validate, and anything Validate
// misses (a rule that matches no one, a price outside the spec) is caught
// by the replay gate before a single customer sees it.
//
//	{
//	  "name": "tiered-by-tenure",
//	  "vars": {"new": 45, "loyal": 65},
//	  "rules": [
//	    {"when": [{"input": "tenure_months", "op": "<", "value": "6"}],
//	     "effects": [{"name": "charge", "type": "card.charge",
//	                  "params": {"customer": "${input.customer}", "amount": "${var.new}"}}]},
//	    {"effects": [{"name": "charge", "type": "card.charge",
//	                  "params": {"customer": "${input.customer}", "amount": "${var.loyal}"}}]}
//	  ]
//	}
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
)

// Policy is a whole behaviour.
type Policy struct {
	Name  string             `json:"name,omitempty"`
	Vars  map[string]float64 `json:"vars,omitempty"`
	Rules []Rule             `json:"rules"`
}

// Rule plans Effects when every condition in When holds. A rule with no
// conditions always matches, so it belongs last as the default.
type Rule struct {
	When    []Cond   `json:"when,omitempty"`
	Effects []Effect `json:"effects"`
}

// Cond compares a request input with a value. Numeric operators (<, <=,
// >, >=) compare as numbers; == and != compare as strings; "in" takes a
// comma-separated list.
type Cond struct {
	Input string `json:"input"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

// Effect is an effect template. Param values may contain ${input.NAME}
// and ${var.NAME} references.
type Effect struct {
	Name   string            `json:"name"`
	Type   string            `json:"type"`
	Params map[string]string `json:"params,omitempty"`
}

var ops = map[string]bool{"==": true, "!=": true, "<": true, "<=": true, ">": true, ">=": true, "in": true}

var ref = regexp.MustCompile(`\$\{(input|var)\.([A-Za-z0-9_]+)\}`)

// Parse reads a policy, rejecting unknown fields.
func Parse(b []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy: %w", err)
	}
	return p, nil
}

// JSON renders a policy compactly and deterministically.
func (p Policy) JSON() string {
	b, _ := json.Marshal(p)
	return string(b)
}

// Validate checks a policy against the registered effect types. It
// returns every problem it finds, not just the first.
func (p Policy) Validate(types map[string]reversible.Class) error {
	var errs []error
	if len(p.Rules) == 0 {
		errs = append(errs, errors.New("policy has no rules"))
	}
	for i, r := range p.Rules {
		where := fmt.Sprintf("rules[%d]", i)
		if len(r.Effects) == 0 {
			errs = append(errs, fmt.Errorf("%s plans no effects", where))
		}
		if len(r.When) == 0 && i != len(p.Rules)-1 {
			errs = append(errs, fmt.Errorf("%s has no conditions, so the rules after it can never match", where))
		}
		for j, c := range r.When {
			cw := fmt.Sprintf("%s.when[%d]", where, j)
			if c.Input == "" {
				errs = append(errs, fmt.Errorf("%s names no input", cw))
			}
			if !ops[c.Op] {
				errs = append(errs, fmt.Errorf("%s: unknown op %q (use ==, !=, <, <=, >, >=, in)", cw, c.Op))
				continue
			}
			if strings.ContainsAny(c.Op, "<>") {
				if _, err := strconv.ParseFloat(c.Value, 64); err != nil {
					errs = append(errs, fmt.Errorf("%s: %s needs a number, got %q", cw, c.Op, c.Value))
				}
			}
		}
		names := map[string]bool{}
		for j, e := range r.Effects {
			ew := fmt.Sprintf("%s.effects[%d]", where, j)
			if e.Name == "" {
				errs = append(errs, fmt.Errorf("%s has no name", ew))
			} else if names[e.Name] {
				errs = append(errs, fmt.Errorf("%s: duplicate effect name %q", ew, e.Name))
			}
			names[e.Name] = true
			if _, ok := types[e.Type]; !ok {
				errs = append(errs, fmt.Errorf("%s: unknown effect type %q", ew, e.Type))
			}
			for k, v := range e.Params {
				for _, m := range ref.FindAllStringSubmatch(v, -1) {
					if m[1] == "var" {
						if _, ok := p.Vars[m[2]]; !ok {
							errs = append(errs, fmt.Errorf("%s.params.%s: unknown variable %q", ew, k, m[2]))
						}
					}
				}
				if rest := ref.ReplaceAllString(v, ""); strings.Contains(rest, "${") {
					errs = append(errs, fmt.Errorf("%s.params.%s: malformed reference in %q", ew, k, v))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// Compile validates the policy and returns a Program that plans its
// effects. Intents are named "<prefix>-<request ID>".
func Compile(p Policy, reg *reversible.Registry, prefix string) (counterfactual.Program, error) {
	if err := p.Validate(reg.Types()); err != nil {
		return nil, err
	}
	if prefix == "" {
		prefix = "policy"
	}
	return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
		for i, r := range p.Rules {
			ok, err := matches(r.When, req)
			if err != nil {
				return reversible.Intent{}, fmt.Errorf("rules[%d]: %w", i, err)
			}
			if !ok {
				continue
			}
			in := reversible.Intent{ID: prefix + "-" + req.ID, Goal: fmt.Sprintf("%s rule %d", p.Name, i)}
			for _, e := range r.Effects {
				params := reversible.Params{}
				for k, v := range e.Params {
					params[k] = expand(v, req, p.Vars)
				}
				eff, err := reg.Effect(e.Name, e.Type, params)
				if err != nil {
					return reversible.Intent{}, err
				}
				in.Effects = append(in.Effects, eff)
			}
			return in, nil
		}
		return reversible.Intent{}, fmt.Errorf("no rule of %s matched request %s", p.Name, req.ID)
	}, nil
}

func matches(conds []Cond, req counterfactual.Request) (bool, error) {
	for _, c := range conds {
		got, present := req.Input[c.Input]
		if !present {
			return false, nil
		}
		switch c.Op {
		case "==":
			if got != c.Value {
				return false, nil
			}
		case "!=":
			if got == c.Value {
				return false, nil
			}
		case "in":
			found := false
			for _, v := range strings.Split(c.Value, ",") {
				if strings.TrimSpace(v) == got {
					found = true
				}
			}
			if !found {
				return false, nil
			}
		default:
			a, err := strconv.ParseFloat(got, 64)
			if err != nil {
				return false, fmt.Errorf("input %s=%q is not a number", c.Input, got)
			}
			b, _ := strconv.ParseFloat(c.Value, 64)
			if !((c.Op == "<" && a < b) || (c.Op == "<=" && a <= b) || (c.Op == ">" && a > b) || (c.Op == ">=" && a >= b)) {
				return false, nil
			}
		}
	}
	return true, nil
}

func expand(v string, req counterfactual.Request, vars map[string]float64) string {
	return ref.ReplaceAllStringFunc(v, func(m string) string {
		sub := ref.FindStringSubmatch(m)
		if sub[1] == "input" {
			return req.Input[sub[2]]
		}
		return strconv.FormatFloat(vars[sub[2]], 'f', -1, 64)
	})
}

// Summary is a one-line human description of a policy.
func (p Policy) Summary() string {
	var parts []string
	for _, r := range p.Rules {
		var conds []string
		for _, c := range r.When {
			conds = append(conds, c.Input+" "+c.Op+" "+c.Value)
		}
		var effs []string
		for _, e := range r.Effects {
			s := e.Name
			var kv []string
			keys := make([]string, 0, len(e.Params))
			for k := range e.Params {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				v := e.Params[k]
				if strings.HasPrefix(v, "${input.") {
					continue
				}
				kv = append(kv, k+"="+expand(v, counterfactual.Request{}, p.Vars))
			}
			if len(kv) > 0 {
				s += "(" + strings.Join(kv, ",") + ")"
			}
			effs = append(effs, s)
		}
		when := "otherwise"
		if len(conds) > 0 {
			when = "if " + strings.Join(conds, " and ")
		}
		parts = append(parts, when+": "+strings.Join(effs, " + "))
	}
	return strings.Join(parts, "; ")
}
