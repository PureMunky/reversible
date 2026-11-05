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

	switch f.Kind {
	case ServerError:
		p.Question = fmt.Sprintf("Since release %s, %s hit %d server error(s), e.g. %s %s. What should that request return?",
			f.Since, who, f.Count, t.Request.Method, t.Request.Path)
		if f.Previous[0].Status != 0 && f.Previous[0].Status < 500 {
			s := clone(live)
			s.Examples = append(s.Examples, capture(live, t.Request, "", f.Previous[0].Status, fmt.Sprintf("production: %s %s must not fail", t.Request.Method, t.Request.Path))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("it's a defect: answer %d, as the previous version did", f.Previous[0].Status), Spec: s})
		}

	case NewRefusal:
		ri := ruleBehind(live, f.Because)
		p.Question = fmt.Sprintf("Since release %s, %s has had %d request(s) refused that the previous version accepted (e.g. %s %s → %d)",
			f.Since, who, f.Count, t.Request.Method, t.Request.Path, t.Response.Status)
		if f.Because != "" {
			p.Question += ", because of the " + f.Because
		}
		p.Question += ". What should happen?"

		cond := ""
		if ri >= 0 {
			c := live.Resource.Rules[ri].ForbidDeleteWhen
			cond = c.Field + "=" + fmt.Sprint(c.Equals)
		}
		keep := clone(live)
		keep.Examples = append(keep.Examples, capture(live, t.Request, cond, t.Response.Status, fmt.Sprintf("production: %s is still refused", who))...)
		p.Options = append(p.Options, Option{Label: fmt.Sprintf("keep refusing; %s must change", who), Spec: keep})

		if ri >= 0 && f.Caller != "" {
			ex := clone(live)
			ex.Resource.Rules[ri].ExceptCallers = append(ex.Resource.Rules[ri].ExceptCallers, f.Caller)
			ex.Examples = append(ex.Examples, capture(live, t.Request, cond, f.Previous[0].Status, fmt.Sprintf("production: %s is exempt", who))...)
			p.Options = append(p.Options, Option{Label: fmt.Sprintf("exempt %s from the rule", who), Spec: ex})
		}
		if ri >= 0 {
			drop := clone(live)
			drop.Resource.Rules = append(drop.Resource.Rules[:ri:ri], drop.Resource.Rules[ri+1:]...)
			var kept []Example
			for _, e := range drop.Examples {
				if endpointOf(drop.Resource, e.Request) == "delete" && e.Expect.Status == 409 {
					continue // this example only existed to show the rule
				}
				kept = append(kept, e)
			}
			drop.Examples = append(kept, capture(live, t.Request, cond, f.Previous[0].Status, fmt.Sprintf("production: %s is allowed again", who))...)
			p.Options = append(p.Options, Option{Label: "drop the rule for everyone", Spec: drop})
		}
	}
	return p
}

// ruleBehind finds the rule an explanation names, or -1.
func ruleBehind(s ServiceSpec, because string) int {
	for i, r := range s.Resource.Rules {
		if c := r.ForbidDeleteWhen; c != nil && strings.Contains(because, fmt.Sprintf("no delete while %s is %v", c.Field, c.Equals)) {
			return i
		}
	}
	return -1
}

// capture turns a production request into examples that reproduce it on
// a fresh instance: a setup step that creates the record it acts on (in
// the state the rule cares about), then the request itself.
func capture(s ServiceSpec, req HTTPCall, cond string, status int, name string) []Example {
	r := s.Resource
	key := strings.TrimPrefix(req.Path, "/"+r.Plural+"/")
	if key == req.Path || strings.Contains(key, "/") {
		return []Example{{Name: name, Request: req, Expect: Expectancy{Status: status}}}
	}
	// Keep the production key: the defect may depend on it. Rename only if
	// the spec's examples already use that key.
	fresh := key
	for _, e := range s.Examples {
		if strings.HasSuffix(e.Request.Path, "/"+key) {
			fresh = "prod-" + key
		}
		if m, ok := e.Request.Body.(map[string]any); ok && m[r.Key] == key {
			fresh = "prod-" + key
		}
	}
	body := map[string]any{r.Key: fresh}
	for _, f := range r.Fields {
		if f.Required && f.Name != r.Key {
			switch f.Type {
			case "string":
				body[f.Name] = "production"
			case "bool":
				body[f.Name] = false
			default:
				body[f.Name] = float64(0)
			}
		}
	}
	if field, val, ok := strings.Cut(cond, "="); ok {
		for _, f := range r.Fields {
			if f.Name == field {
				switch f.Type {
				case "bool":
					body[field] = val == "true"
				case "string":
					body[field] = val
				}
			}
		}
	}
	replay := req
	replay.Path = "/" + r.Plural + "/" + fresh
	return []Example{
		{Name: name + " (setup)", Request: HTTPCall{Method: "POST", Path: "/" + r.Plural, Body: body}, Expect: Expectancy{Status: 201}},
		{Name: name, Request: replay, Expect: Expectancy{Status: status}},
	}
}

func clone(s ServiceSpec) ServiceSpec {
	var out ServiceSpec
	b := []byte(s.JSON())
	out, _ = ParseSpec(b)
	return out
}
