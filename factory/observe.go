package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Production is an input. After a release, Observe compares what the new
// version actually did with what the previous version would have done
// with exactly the same requests, starting from the same data. Where the
// new version treats a caller worse (refuses what used to work, or fails
// outright), that is a finding. Propose turns each finding into a
// question for the intent's owner, with concrete intent changes as the
// answers; the production request itself becomes part of the contract.
// Nobody files a ticket and nobody edits code: the owner answers, and the
// factory regenerates.

// FindingKind classifies what production showed.
type FindingKind string

const (
	// NewRefusal: the new version refused (4xx) requests the previous
	// version accepted.
	NewRefusal FindingKind = "new refusal"
	// ServerError: the new version failed (5xx).
	ServerError FindingKind = "server error"
)

// Finding is a pattern in production traffic since the last release.
type Finding struct {
	Kind     FindingKind
	Caller   string
	Because  string     // the intent change that explains it, if any
	Count    int        // how many requests
	Requests []Exchange // what the new version actually answered
	Previous []Response // what the previous version would have answered
	Since    string     // spec hash of the version that served them
}

func (f Finding) String() string {
	who := f.Caller
	if who == "" {
		who = "anonymous callers"
	}
	ex := f.Requests[0]
	s := fmt.Sprintf("%s: %s, %d request(s), e.g. %s %s → %d %s", f.Kind, who, f.Count, ex.Request.Method, ex.Request.Path, ex.Response.Status, short(ex.Response.Body))
	if f.Kind == NewRefusal {
		s += fmt.Sprintf(" (the previous version answered %d)", f.Previous[0].Status)
	}
	if f.Because != "" {
		s += "; cause: " + f.Because
	}
	return s
}

// Observe looks at the traffic served since the last release.
func Observe(ctx context.Context, r *Router) ([]Finding, error) {
	mark, ok := r.LastRelease()
	if !ok || mark.Version == nil {
		return nil, nil
	}
	live := mark.Version
	var served []Exchange
	for _, t := range r.Traffic()[mark.TrafficIndex:] {
		if t.Version == live.SpecHash {
			served = append(served, t)
		}
	}
	if len(served) == 0 {
		return nil, nil
	}

	groups := map[string]*Finding{}
	var order []string
	add := func(kind FindingKind, caller, because string, t Exchange, prev Response) {
		k := string(kind) + "|" + caller + "|" + because
		f, ok := groups[k]
		if !ok {
			f = &Finding{Kind: kind, Caller: caller, Because: because, Since: live.SpecHash}
			groups[k] = f
			order = append(order, k)
		}
		f.Count++
		f.Requests = append(f.Requests, t)
		f.Previous = append(f.Previous, prev)
	}

	// Ask the previous version, from the same starting data, about the
	// same requests in the same order.
	var prevResp []Response
	if mark.Previous != nil {
		dir, err := os.MkdirTemp("", "factory-observe-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		data := filepath.Join(dir, "data.json")
		if mark.Snapshot != "" {
			b, err := os.ReadFile(mark.Snapshot)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(data, b, 0o644); err != nil {
				return nil, err
			}
		}
		inst, err := Start(ctx, mark.Previous.Binary, data)
		if err != nil {
			return nil, err
		}
		defer inst.Stop()
		for _, t := range served {
			resp, err := Call(ctx, inst.URL, t.Request)
			if err != nil {
				return nil, err
			}
			prevResp = append(prevResp, resp)
		}
	}

	var ex *explainer
	if mark.Previous != nil {
		ex = newExplainer(mark.Previous.Spec, live.Spec)
	}
	for i, t := range served {
		var prev Response
		if prevResp != nil {
			prev = prevResp[i]
		}
		switch {
		case t.Response.Status >= 500:
			add(ServerError, t.Request.Caller, "", t, prev)
		case prevResp != nil && t.Response.Status >= 400 && prev.Status < 400:
			c := Change{Index: i, Request: t.Request, Old: prev, New: t.Response}
			c.Because = ex.explain(c)
			ex.track(c)
			add(NewRefusal, t.Request.Caller, c.Because, t, prev)
		}
	}

	var out []Finding
	for _, k := range order {
		out = append(out, *groups[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}

// Option is one answer to a proposal: the intent that would result.
type Option struct {
	Label string
	Spec  ServiceSpec
}

// Proposal is a question for the intent's owner, raised by production.
type Proposal struct {
	Finding  Finding
	Question string
	Options  []Option
}

// Propose turns a finding into a question with concrete intent changes as
// answers. Every option captures the production request as a new example,
// so whatever the owner decides becomes part of the contract and can
// never silently regress.
func Propose(f Finding, live ServiceSpec) Proposal {
	who := f.Caller
	if who == "" {
		who = "anonymous callers"
	}
	t := f.Requests[0]
	p := Proposal{Finding: f}
	res, _ := live.Route(t.Request)
	if res == nil {
		p.Question = fmt.Sprintf("Since release %s, %s %s isn't an endpoint of the intent. What should it do?", f.Since, t.Request.Method, t.Request.Path)
		return p
	}
	idx := resourceIndex(live, res.Name)

	switch f.Kind {
	case ServerError:
		p.Question = fmt.Sprintf("Since release %s, %s hit %d server error(s), e.g. %s %s. What should that request return?",
			f.Since, who, f.Count, t.Request.Method, t.Request.Path)
		if f.Previous[0].Status != 0 && f.Previous[0].Status < 500 {
			s := clone(live)
			s.Examples = append(s.Examples, capture(live, t.Request, nil, f.Previous[0].Status, fmt.Sprintf("production: %s %s must not fail", t.Request.Method, t.Request.Path))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("it's a defect: answer %d, as the previous version did", f.Previous[0].Status), Spec: s})
		}

	case NewRefusal:
		p.Question = fmt.Sprintf("Since release %s, %s has had %d request(s) refused that the previous version accepted (e.g. %s %s → %d)",
			f.Since, who, f.Count, t.Request.Method, t.Request.Path, t.Response.Status)
		if f.Because != "" {
			p.Question += ", because of the " + f.Because
		}
		p.Question += ". What should happen?"
		prev := f.Previous[0].Status

		// What state the record must be in to reproduce the refusal.
		setup := map[string]any{}
		ri := ruleBehind(*res, f.Because)
		if ri >= 0 {
			c := res.Rules[ri].ForbidDeleteWhen
			setup[c.Field] = c.Equals
		}
		field, from, to, isTransition := transitionRefused(*res, t)
		if isTransition {
			setup[field] = from
		}

		keep := clone(live)
		keep.Examples = append(keep.Examples, capture(live, t.Request, setup, t.Response.Status, fmt.Sprintf("production: %s is still refused", who))...)
		p.Options = append(p.Options, Option{Label: fmt.Sprintf("keep refusing; %s must change", who), Spec: keep})

		switch {
		case ri >= 0:
			if f.Caller != "" {
				ex := clone(live)
				rr := ex.All()[idx]
				rr.Rules[ri].ExceptCallers = append(rr.Rules[ri].ExceptCallers, f.Caller)
				ex.Examples = append(ex.Examples, capture(live, t.Request, setup, prev, fmt.Sprintf("production: %s is exempt", who))...)
				p.Options = append(p.Options, Option{Label: fmt.Sprintf("exempt %s from the rule", who), Spec: ex})
			}
			drop := clone(live)
			rr := drop.All()[idx]
			rr.Rules = append(rr.Rules[:ri:ri], rr.Rules[ri+1:]...)
			drop.Examples = append(without(drop, res.Name, "delete", 409), capture(live, t.Request, setup, prev, fmt.Sprintf("production: %s is allowed again", who))...)
			p.Options = append(p.Options, Option{Label: "drop the rule for everyone", Spec: drop})

		case t.Response.Status == 403 && len(res.Writers) > 0 && f.Caller != "":
			add := clone(live)
			rr := add.All()[idx]
			rr.Writers = append(rr.Writers, f.Caller)
			add.Examples = append(withoutCaller(add, res.Name, f.Caller, 403), capture(live, t.Request, setup, prev, fmt.Sprintf("production: %s may change %s", who, res.Plural))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("let %s change %s", who, res.Plural), Spec: add})
			open := clone(live)
			open.All()[idx].Writers = nil
			ex := without(open, res.Name, "", 403)
			open.Examples = append(ex, capture(live, t.Request, setup, prev, fmt.Sprintf("production: anyone may change %s", res.Plural))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("let anyone change %s", res.Plural), Spec: open})

		case isTransition:
			allow := clone(live)
			for i := range allow.All()[idx].Fields {
				fl := &allow.All()[idx].Fields[i]
				if fl.Name == field {
					tr := map[string][]string{}
					for k, v := range fl.Transitions {
						tr[k] = append([]string(nil), v...)
					}
					tr[from] = append(tr[from], to)
					fl.Transitions = tr
				}
			}
			var kept []Example
			for _, e := range allow.Examples {
				r, eop := allow.Route(e.Request)
				b, _ := e.Request.Body.(map[string]any)
				if r != nil && r.Name == res.Name && eop == "update" && e.Expect.Status == 409 && b[field] == to {
					continue // showed the refusal now being allowed
				}
				kept = append(kept, e)
			}
			allow.Examples = append(kept, capture(live, t.Request, setup, prev, fmt.Sprintf("production: %s may go from %s to %s", field, from, to))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("allow %s to go from %s to %s", field, from, to), Spec: allow})
		}
	}
	return p
}

func resourceIndex(s ServiceSpec, name string) int {
	for i, r := range s.All() {
		if r.Name == name {
			return i
		}
	}
	return -1
}

// without drops examples on a resource's operation ("" for any) that
// expect a status: examples that only existed to show a rule being dropped.
func without(s ServiceSpec, res, op string, status int) []Example {
	var kept []Example
	for _, e := range s.Examples {
		r, eop := s.Route(e.Request)
		if r != nil && r.Name == res && (op == "" || eop == op) && e.Expect.Status == status {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// withoutCaller drops examples showing a caller refused with a status on
// a resource: the answer just changed what that caller may do.
func withoutCaller(s ServiceSpec, res, caller string, status int) []Example {
	var kept []Example
	for _, e := range s.Examples {
		if r, _ := s.Route(e.Request); r != nil && r.Name == res && e.Request.Caller == caller && e.Expect.Status == status {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// transitionRefused reads a refused state change from the response.
func transitionRefused(r Resource, t Exchange) (field, from, to string, ok bool) {
	if t.Response.Status != 409 || t.Request.Method != "PATCH" {
		return "", "", "", false
	}
	m, _ := t.Response.Body.(map[string]any)
	msg, _ := m["error"].(string)
	for _, f := range r.Fields {
		if len(f.Transitions) == 0 {
			continue
		}
		var a, b string
		if n, _ := fmt.Sscanf(msg, f.Name+" can't change from %s to %s", &a, &b); n == 2 {
			return f.Name, a, strings.TrimRight(b, ".)"), true
		}
	}
	return "", "", "", false
}

// ruleBehind finds the delete rule an explanation names, or -1.
func ruleBehind(r Resource, because string) int {
	for i, rule := range r.Rules {
		if c := rule.ForbidDeleteWhen; c != nil && strings.Contains(because, fmt.Sprintf("no delete while %s is %v", c.Field, c.Equals)) {
			return i
		}
	}
	return -1
}

// capture turns a production request into examples that reproduce it on
// a fresh instance: setup steps that create the record it acts on (in the
// state that matters, with any records it references), then the request.
func capture(s ServiceSpec, req HTTPCall, state map[string]any, status int, name string) []Example {
	r, op := s.Route(req)
	if r == nil || op == "create" || op == "list" {
		return []Example{{Name: name, Request: req, Expect: Expectancy{Status: status}}}
	}
	key := r.KeyOf(req)
	// Keep the production key: the defect may depend on it. Rename only if
	// the spec's examples already use that key.
	fresh := key
	for _, e := range s.Examples {
		if er, _ := s.Route(e.Request); er != nil && er.Name == r.Name && er.KeyOf(e.Request) == key {
			fresh = "prod-" + key
		}
	}
	setup := setupFor(s, *r, fresh, state, name, 0)
	replay := req
	replay.Path = "/" + r.Plural + "/" + fresh
	return append(setup, Example{Name: name, Request: replay, Expect: Expectancy{Status: status}})
}

// setupFor creates a record (and, first, any records it must reference).
func setupFor(s ServiceSpec, r Resource, key string, state map[string]any, name string, depth int) []Example {
	var out []Example
	body := map[string]any{r.Key: key}
	for _, f := range r.Fields {
		if !f.Required || f.Name == r.Key {
			continue
		}
		switch {
		case f.References != "":
			if ref := s.Named(f.References); ref != nil && depth < 3 {
				parent := sampleKey(s, *ref, "prod-"+ref.Name+"-"+strings.ToLower(key))
				out = append(out, setupFor(s, *ref, parent, nil, name, depth+1)...)
				body[f.Name] = parent
			}
		case len(f.Enum) > 0:
			body[f.Name] = f.Enum[0]
		case f.Type == "string":
			body[f.Name] = "production"
		case f.Type == "bool":
			body[f.Name] = false
		default:
			v := 0.0
			if f.Min != nil {
				v = *f.Min
			}
			body[f.Name] = v
		}
	}
	for k, v := range state {
		body[k] = v
	}
	caller := ""
	if len(r.Writers) > 0 {
		caller = r.Writers[0]
	}
	return append(out, Example{Name: name + " (setup " + r.Name + ")", Request: HTTPCall{Method: "POST", Path: "/" + r.Plural, Body: body, Caller: caller},
		Expect: Expectancy{Status: 201}})
}

func clone(s ServiceSpec) ServiceSpec {
	var out ServiceSpec
	b := []byte(s.JSON())
	out, _ = ParseSpec(b)
	return out
}
