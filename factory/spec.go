// Package factory builds software from intent.
//
// This is the root idea of the repo in one vertical slice. A person
// describes a service; the description becomes a ServiceSpec (the source
// of truth); a Coder generates the implementation (the code is a cache);
// the factory builds it, runs it, and checks it against the spec's
// examples (the contract); it replays recorded traffic through the
// current and candidate versions and shows how behaviour would change;
// a person signs off on that reality; and the release is a reversible
// effect, so rewinding the decision that caused it puts the previous
// version back. Nobody edits generated code.
package factory

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// ServiceSpec is the intent for a service: what it manages, the rules it
// must enforce, and examples of exactly how it should behave.
type ServiceSpec struct {
	Service  string    `json:"service"`
	Purpose  string    `json:"purpose"`
	Owner    string    `json:"owner,omitempty"`
	Resource Resource  `json:"resource"`
	Examples []Example `json:"examples"`
}

// Resource is the thing the service manages.
type Resource struct {
	Name   string  `json:"name"`   // singular, e.g. "flag"
	Plural string  `json:"plural"` // URL segment, e.g. "flags"
	Key    string  `json:"key"`    // identifying field
	Fields []Field `json:"fields"`
	Rules  []Rule  `json:"rules,omitempty"`
}

// Field is one property of the resource.
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string, bool, int, number
	Required bool     `json:"required,omitempty"`
	Default  any      `json:"default,omitempty"`
	Pattern  string   `json:"pattern,omitempty"` // strings
	Enum     []string `json:"enum,omitempty"`    // strings
	Min      *float64 `json:"min,omitempty"`     // int, number
	Max      *float64 `json:"max,omitempty"`     // int, number
	Why      string   `json:"why,omitempty"`
}

// Rule is a behavioural constraint beyond field validation.
type Rule struct {
	// ForbidDeleteWhen refuses deletion while a field has a value.
	ForbidDeleteWhen *Condition `json:"forbid_delete_when,omitempty"`
	// ExceptCallers are callers (by X-Caller) the rule doesn't apply to.
	ExceptCallers []string `json:"except_callers,omitempty"`
	Why           string   `json:"why,omitempty"`
}

// Condition is "field equals value".
type Condition struct {
	Field  string `json:"field"`
	Equals any    `json:"equals"`
}

// Example is one step of a scenario: a request and what must come back.
// Examples run in order against a fresh instance, so state carries.
type Example struct {
	Name    string     `json:"name"`
	Request HTTPCall   `json:"request"`
	Expect  Expectancy `json:"expect"`
}

// HTTPCall is a request.
type HTTPCall struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Body   any    `json:"body,omitempty"`
	// Caller identifies who is calling (sent as the X-Caller header), so
	// rules can treat callers differently and production findings can say
	// who was affected.
	Caller string `json:"caller,omitempty"`
}

// Expectancy is what a response must contain. Body is matched as a
// subset: every key given must match; keys not given are not checked.
type Expectancy struct {
	Status int `json:"status"`
	Body   any `json:"body,omitempty"`
}

// ParseSpec reads a spec, rejecting unknown fields.
func ParseSpec(b []byte) (ServiceSpec, error) {
	var s ServiceSpec
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return ServiceSpec{}, fmt.Errorf("service spec: %w", err)
	}
	return s, nil
}

// LoadSpec reads a spec file.
func LoadSpec(path string) (ServiceSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ServiceSpec{}, err
	}
	return ParseSpec(b)
}

// JSON renders the spec deterministically.
func (s ServiceSpec) JSON() string {
	b, _ := json.MarshalIndent(s, "", "  ")
	return string(b)
}

// Hash identifies the intent. The same intent always maps to the same
// cached build.
func (s ServiceSpec) Hash() string {
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:12]
}

// Severity says whether a question blocks building.
type Severity string

const (
	Blocking Severity = "blocking"
	Clarify  Severity = "clarify"
)

// Question is something the factory won't guess about.
type Question struct {
	Severity Severity
	Where    string
	Ask      string
}

func (q Question) String() string { return fmt.Sprintf("[%s] %s: %s", q.Severity, q.Where, q.Ask) }

// Ready reports whether there are no blocking questions.
func Ready(qs []Question) bool {
	for _, q := range qs {
		if q.Severity == Blocking {
			return false
		}
	}
	return true
}

var ident = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Check returns everything that is ambiguous, contradictory or missing,
// blocking issues first. The factory refuses to build until the blocking
// ones are answered: a vague intent produces vague software.
func (s ServiceSpec) Check() []Question {
	var qs []Question
	ask := func(sev Severity, where, format string, a ...any) {
		qs = append(qs, Question{sev, where, fmt.Sprintf(format, a...)})
	}
	r := s.Resource
	if s.Service == "" || !ident.MatchString(s.Service) {
		ask(Blocking, "service", "what is the service called? (lowercase letters, digits and _)")
	}
	if strings.TrimSpace(s.Purpose) == "" {
		ask(Blocking, "purpose", "what is this service for, and who uses it?")
	}
	if s.Owner == "" {
		ask(Clarify, "owner", "who is accountable for this service's behaviour?")
	}
	if !ident.MatchString(r.Name) || !ident.MatchString(r.Plural) {
		ask(Blocking, "resource", "what does the service manage? (singular name and plural URL segment)")
	}

	fields := map[string]Field{}
	for i, f := range r.Fields {
		where := fmt.Sprintf("resource.fields[%d]", i)
		if !ident.MatchString(f.Name) {
			ask(Blocking, where, "field name %q must be lowercase letters, digits and _", f.Name)
			continue
		}
		if _, dup := fields[f.Name]; dup {
			ask(Blocking, where, "field %q is defined twice", f.Name)
		}
		fields[f.Name] = f
		where += " (" + f.Name + ")"
		switch f.Type {
		case "string":
			if f.Pattern != "" {
				if _, err := regexp.Compile(f.Pattern); err != nil {
					ask(Blocking, where, "pattern %q is not a valid regular expression: %v", f.Pattern, err)
				}
			}
			if f.Min != nil || f.Max != nil {
				ask(Blocking, where, "min/max apply to numbers; did you mean a pattern or enum for this string?")
			}
		case "int", "number":
			if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
				ask(Blocking, where, "must be at least %g and at most %g; no value satisfies both", *f.Min, *f.Max)
			}
			if f.Pattern != "" || len(f.Enum) > 0 {
				ask(Blocking, where, "pattern and enum apply to strings")
			}
		case "bool":
			if f.Pattern != "" || len(f.Enum) > 0 || f.Min != nil || f.Max != nil {
				ask(Blocking, where, "a bool can't have a pattern, enum or range")
			}
		default:
			ask(Blocking, where, "type %q is not one of string, bool, int, number", f.Type)
		}
		if f.Required && f.Default != nil {
			ask(Clarify, where, "required fields don't need a default; which did you mean?")
		}
	}
	if kf, ok := fields[r.Key]; !ok {
		ask(Blocking, "resource.key", "which field identifies a %s?", or(r.Name, "record"))
	} else if kf.Type != "string" || !kf.Required {
		ask(Blocking, "resource.key", "the key field %q must be a required string", r.Key)
	}

	for i, rule := range r.Rules {
		where := fmt.Sprintf("resource.rules[%d]", i)
		c := rule.ForbidDeleteWhen
		if c == nil {
			ask(Blocking, where, "this rule constrains nothing (supported: forbid_delete_when)")
			continue
		}
		f, ok := fields[c.Field]
		if !ok {
			ask(Blocking, where, "refers to unknown field %q", c.Field)
			continue
		}
		if !valueFits(f.Type, c.Equals) {
			ask(Blocking, where, "%q is a %s, but the rule compares it with %v", c.Field, f.Type, c.Equals)
		}
		if rule.Why == "" {
			ask(Clarify, where, "why does this rule exist? The reason is shown to whoever hits it")
		}
		for _, c := range rule.ExceptCallers {
			if strings.TrimSpace(c) == "" {
				ask(Blocking, where, "except_callers has an empty caller name")
			}
		}
	}

	if len(s.Examples) == 0 {
		ask(Blocking, "examples", "how will we know it works? Give examples of requests and the responses you expect")
	}
	covered := map[string]bool{}
	created := map[string]bool{}
	for i, e := range s.Examples {
		where := fmt.Sprintf("examples[%d] (%s)", i, e.Name)
		op := endpointOf(r, e.Request)
		if op == "" {
			ask(Blocking, where, "%s %s is not an endpoint of this service", e.Request.Method, e.Request.Path)
			continue
		}
		covered[op] = true
		if e.Expect.Status < 100 || e.Expect.Status > 599 {
			ask(Blocking, where, "what status should come back?")
		}
		if op == "create" && e.Expect.Status/100 == 2 {
			if m, ok := e.Request.Body.(map[string]any); ok {
				for k := range m {
					created[k] = true
				}
			}
		}
	}
	for _, op := range []string{"create", "get", "list", "update", "delete"} {
		if len(s.Examples) > 0 && !covered[op] {
			ask(Clarify, "examples", "no example shows what %s does; the generated behaviour there will be unchecked", op)
		}
	}
	for _, rule := range r.Rules {
		if c := rule.ForbidDeleteWhen; c != nil && covered["delete"] {
			found := false
			for _, e := range s.Examples {
				if endpointOf(r, e.Request) == "delete" && e.Expect.Status == 409 {
					found = true
				}
			}
			if !found {
				ask(Clarify, "examples", "no example shows a delete being refused while %s is %v", c.Field, c.Equals)
			}
		}
	}

	sort.SliceStable(qs, func(i, j int) bool { return qs[i].Severity == Blocking && qs[j].Severity != Blocking })
	return qs
}

// endpointOf maps a request to an operation name, or "".
func endpointOf(r Resource, c HTTPCall) string {
	base := "/" + r.Plural
	switch {
	case c.Path == base && c.Method == "POST":
		return "create"
	case c.Path == base && c.Method == "GET":
		return "list"
	case strings.HasPrefix(c.Path, base+"/") && !strings.Contains(c.Path[len(base)+1:], "/"):
		switch c.Method {
		case "GET":
			return "get"
		case "PATCH":
			return "update"
		case "DELETE":
			return "delete"
		}
	}
	return ""
}

func valueFits(typ string, v any) bool {
	switch typ {
	case "string":
		_, ok := v.(string)
		return ok
	case "bool":
		_, ok := v.(bool)
		return ok
	case "int", "number":
		_, ok := v.(float64)
		return ok
	}
	return false
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
