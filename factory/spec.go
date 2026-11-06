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
	Service string `json:"service"`
	Purpose string `json:"purpose"`
	Owner   string `json:"owner,omitempty"`
	// A service manages one resource (Resource) or several (Resources).
	Resource  *Resource  `json:"resource,omitempty"`
	Resources []Resource `json:"resources,omitempty"`
	Examples  []Example  `json:"examples"`
}

// Resource is a kind of thing the service manages.
type Resource struct {
	Name   string  `json:"name"`   // singular, e.g. "flag"
	Plural string  `json:"plural"` // URL segment, e.g. "flags"
	Key    string  `json:"key"`    // identifying field
	Fields []Field `json:"fields"`
	Rules  []Rule  `json:"rules,omitempty"`
	// Writers are the callers (by X-Caller) allowed to create, update and
	// delete; anyone may read. Empty means anyone may write.
	Writers []string `json:"writers,omitempty"`
}

// Field is one property of a resource.
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string, bool, int, number
	Required bool     `json:"required,omitempty"`
	Default  any      `json:"default,omitempty"`
	Pattern  string   `json:"pattern,omitempty"` // strings
	Enum     []string `json:"enum,omitempty"`    // strings
	Min      *float64 `json:"min,omitempty"`     // int, number
	Max      *float64 `json:"max,omitempty"`     // int, number
	// References names another resource: the field holds that resource's
	// key, it must exist, and it can't be deleted while referenced.
	References string `json:"references,omitempty"`
	// Filterable fields can be used to filter the list: GET /plural?field=value.
	Filterable bool `json:"filterable,omitempty"`
	// Transitions restricts how an enum field may change: from each state,
	// the states it may move to. A state with no entry can't change.
	Transitions map[string][]string `json:"transitions,omitempty"`
	Why         string              `json:"why,omitempty"`
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

// All returns the resources the service manages, in either form.
func (s *ServiceSpec) All() []*Resource {
	if len(s.Resources) > 0 {
		out := make([]*Resource, len(s.Resources))
		for i := range s.Resources {
			out[i] = &s.Resources[i]
		}
		return out
	}
	if s.Resource != nil {
		return []*Resource{s.Resource}
	}
	return nil
}

// Named returns the resource with the given singular name.
func (s *ServiceSpec) Named(name string) *Resource {
	for _, r := range s.All() {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// Route maps a request to the resource it addresses and the operation
// (create, list, get, update, delete); nil and "" if none.
func (s *ServiceSpec) Route(c HTTPCall) (*Resource, string) {
	for _, r := range s.All() {
		if op := endpointOf(*r, c); op != "" {
			return r, op
		}
	}
	return nil, ""
}

// KeyOf is the record key a request is about ("" for list and create
// without a key).
func (r Resource) KeyOf(c HTTPCall) string {
	p := pathOnly(c.Path)
	if k, ok := strings.CutPrefix(p, "/"+r.Plural+"/"); ok {
		return k
	}
	if m, ok := c.Body.(map[string]any); ok && c.Method == "POST" {
		if k, ok := m[r.Key].(string); ok {
			return k
		}
	}
	return ""
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
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
	if s.Service == "" || !ident.MatchString(s.Service) {
		ask(Blocking, "service", "what is the service called? (lowercase letters, digits and _)")
	}
	if strings.TrimSpace(s.Purpose) == "" {
		ask(Blocking, "purpose", "what is this service for, and who uses it?")
	}
	if s.Owner == "" {
		ask(Clarify, "owner", "who is accountable for this service's behaviour?")
	}
	if s.Resource != nil && len(s.Resources) > 0 {
		ask(Blocking, "resources", "the spec has both resource and resources; use resources for all of them")
	}
	all := s.All()
	if len(all) == 0 {
		ask(Blocking, "resource", "what does the service manage? (singular name and plural URL segment)")
	}
	multi := len(all) > 1
	names, plurals := map[string]bool{}, map[string]bool{}
	for i, r := range all {
		at := "resource"
		if len(s.Resources) > 0 {
			at = fmt.Sprintf("resources[%d]", i)
		}
		if !ident.MatchString(r.Name) || !ident.MatchString(r.Plural) {
			ask(Blocking, at, "what does the service manage? (singular name and plural URL segment)")
			continue
		}
		if names[r.Name] || plurals[r.Plural] {
			ask(Blocking, at, "%s/%s is defined twice", r.Name, r.Plural)
		}
		names[r.Name], plurals[r.Plural] = true, true
		if multi {
			at += " (" + r.Name + ")"
		}
		s.checkResource(*r, at, ask)
	}

	if len(s.Examples) == 0 {
		ask(Blocking, "examples", "how will we know it works? Give examples of requests and the responses you expect")
	}
	type key struct{ res, op string }
	covered := map[key]bool{}
	statuses := map[key]map[int]bool{}
	for i, e := range s.Examples {
		where := fmt.Sprintf("examples[%d] (%s)", i, e.Name)
		r, op := s.Route(e.Request)
		if r == nil {
			ask(Blocking, where, "%s %s is not an endpoint of this service", e.Request.Method, e.Request.Path)
			continue
		}
		k := key{r.Name, op}
		covered[k] = true
		if statuses[k] == nil {
			statuses[k] = map[int]bool{}
		}
		statuses[k][e.Expect.Status] = true
		if e.Expect.Status < 100 || e.Expect.Status > 599 {
			ask(Blocking, where, "what status should come back?")
		}
		if q := strings.IndexByte(e.Request.Path, '?'); q >= 0 && op == "list" {
			for _, kv := range strings.Split(e.Request.Path[q+1:], "&") {
				name, _, _ := strings.Cut(kv, "=")
				f, ok := fieldOf(*r, name)
				if (!ok || !f.Filterable) && e.Expect.Status != 400 { // an example may show the refusal
					ask(Blocking, where, "%s isn't a filterable field of %s", name, r.Plural)
				}
			}
		}
	}
	if len(s.Examples) > 0 {
		for _, r := range all {
			of := ""
			if multi {
				of = " for " + r.Plural
			}
			for _, op := range []string{"create", "get", "list", "update", "delete"} {
				if !covered[key{r.Name, op}] {
					ask(Clarify, "examples", "no example shows what %s does%s; the generated behaviour there will be unchecked", op, of)
				}
			}
			for _, rule := range r.Rules {
				if c := rule.ForbidDeleteWhen; c != nil && covered[key{r.Name, "delete"}] && !statuses[key{r.Name, "delete"}][409] {
					ask(Clarify, "examples", "no example shows a delete being refused while %s is %v", c.Field, c.Equals)
				}
			}
			if len(r.Writers) > 0 {
				refused := false
				for _, op := range []string{"create", "update", "delete"} {
					refused = refused || statuses[key{r.Name, op}][403]
				}
				if !refused {
					ask(Clarify, "examples", "no example shows someone outside %v being refused a change to %s", r.Writers, r.Plural)
				}
			}
			for _, f := range r.Fields {
				if len(f.Transitions) > 0 && !statuses[key{r.Name, "update"}][409] {
					ask(Clarify, "examples", "no example shows %s.%s refusing a change it doesn't allow", r.Name, f.Name)
				}
			}
		}
	}

	sort.SliceStable(qs, func(i, j int) bool { return qs[i].Severity == Blocking && qs[j].Severity != Blocking })
	return qs
}

func fieldOf(r Resource, name string) (Field, bool) {
	for _, f := range r.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

func (s *ServiceSpec) checkResource(r Resource, at string, ask func(Severity, string, string, ...any)) {
	fields := map[string]Field{}
	for i, f := range r.Fields {
		where := fmt.Sprintf("%s.fields[%d]", at, i)
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
		if f.Default != nil && len(f.Enum) > 0 {
			if d, _ := f.Default.(string); !contains(f.Enum, d) {
				ask(Blocking, where, "default %v isn't one of %v", f.Default, f.Enum)
			}
		}
		if f.References != "" {
			target := s.Named(f.References)
			switch {
			case target == nil:
				ask(Blocking, where, "references %q, which this service doesn't manage", f.References)
			case f.Type != "string":
				ask(Blocking, where, "a reference holds a %s's key, so it must be a string", f.References)
			case f.Name == r.Key:
				ask(Blocking, where, "the key can't also be a reference")
			}
		}
		if len(f.Transitions) > 0 {
			if f.Type != "string" || len(f.Enum) == 0 {
				ask(Blocking, where, "transitions need an enum: which states can %s be in?", f.Name)
			} else {
				for from, tos := range f.Transitions {
					if !contains(f.Enum, from) {
						ask(Blocking, where, "transition from %q, which isn't one of %v", from, f.Enum)
					}
					for _, to := range tos {
						if !contains(f.Enum, to) {
							ask(Blocking, where, "transition from %q to %q, which isn't one of %v", from, to, f.Enum)
						}
					}
				}
			}
		}
	}
	if kf, ok := fields[r.Key]; !ok {
		ask(Blocking, at+".key", "which field identifies a %s?", or(r.Name, "record"))
	} else if kf.Type != "string" || !kf.Required {
		ask(Blocking, at+".key", "the key field %q must be a required string", r.Key)
	}
	for _, w := range r.Writers {
		if strings.TrimSpace(w) == "" {
			ask(Blocking, at+".writers", "writers has an empty caller name")
		}
	}

	for i, rule := range r.Rules {
		where := fmt.Sprintf("%s.rules[%d]", at, i)
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
}

// endpointOf maps a request to an operation name, or "".
func endpointOf(r Resource, c HTTPCall) string {
	base := "/" + r.Plural
	path := pathOnly(c.Path)
	switch {
	case path == base && c.Method == "POST":
		return "create"
	case path == base && c.Method == "GET":
		return "list"
	case strings.HasPrefix(path, base+"/") && !strings.Contains(path[len(base)+1:], "/"):
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
