package factory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Change is one recorded request that the candidate would answer
// differently from the live version.
type Change struct {
	Index    int
	Request  HTTPCall
	Old, New Response
	// Because says which change in intent explains this difference; empty
	// means nothing in the intent explains it, which is what a reviewer
	// most needs to see.
	Because string
}

// BehaviourDiff is what a new version would change, on real traffic.
type BehaviourDiff struct {
	Replayed int
	Changes  []Change
}

// Unexpected lists changes nothing in the new intent explains.
func (d BehaviourDiff) Unexpected() []Change {
	var out []Change
	for _, c := range d.Changes {
		if c.Because == "" {
			out = append(out, c)
		}
	}
	return out
}

// Diff replays recorded traffic, in order, through fresh instances of the
// live version and the candidate, and reports every request whose
// response would differ, explained against what changed in the intent.
//
// Replay starts both from empty state, so the recording must start from
// empty state too (or from a snapshot, not yet supported).
func Diff(ctx context.Context, traffic []Exchange, live, candidate *Version) (BehaviourDiff, error) {
	dir, err := os.MkdirTemp("", "factory-diff-")
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer os.RemoveAll(dir)
	a, err := Start(ctx, live.Binary, filepath.Join(dir, "live.json"))
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer a.Stop()
	b, err := Start(ctx, candidate.Binary, filepath.Join(dir, "candidate.json"))
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer b.Stop()

	ex := newExplainer(live.Spec, candidate.Spec)
	d := BehaviourDiff{Replayed: len(traffic)}
	for i, t := range traffic {
		oldR, err := Call(ctx, a.URL, t.Request)
		if err != nil {
			return d, err
		}
		newR, err := Call(ctx, b.URL, t.Request)
		if err != nil {
			return d, err
		}
		if oldR.Status == newR.Status && reflect.DeepEqual(oldR.Body, newR.Body) {
			continue
		}
		c := Change{Index: i, Request: t.Request, Old: oldR, New: newR}
		c.Because = ex.explain(c)
		d.Changes = append(d.Changes, c)
	}
	return d, nil
}

// DiffState checks the candidate against production's existing data:
// both versions are started on copies of a snapshot, and every record is
// read back from each. Replaying traffic from empty state can't see a
// version that misreads data written by its predecessor; this can.
func DiffState(ctx context.Context, snapshot string, live, candidate *Version) (BehaviourDiff, error) {
	data, err := os.ReadFile(snapshot)
	if err != nil {
		if os.IsNotExist(err) {
			return BehaviourDiff{}, nil
		}
		return BehaviourDiff{}, err
	}
	dir, err := os.MkdirTemp("", "factory-state-")
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer os.RemoveAll(dir)
	start := func(v *Version, name string) (*Instance, error) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return nil, err
		}
		return Start(ctx, v.Binary, p)
	}
	a, err := start(live, "live.json")
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer a.Stop()
	b, err := start(candidate, "candidate.json")
	if err != nil {
		return BehaviourDiff{}, err
	}
	defer b.Stop()

	list := HTTPCall{Method: "GET", Path: "/" + live.Spec.Resource.Plural}
	reads := []HTTPCall{list}
	if r, err := Call(ctx, a.URL, list); err == nil {
		if items, ok := r.Body.([]any); ok {
			for _, it := range items {
				if m, ok := it.(map[string]any); ok {
					reads = append(reads, HTTPCall{Method: "GET", Path: fmt.Sprintf("/%s/%v", live.Spec.Resource.Plural, m[live.Spec.Resource.Key])})
				}
			}
		}
	}
	ex := newExplainer(live.Spec, candidate.Spec)
	d := BehaviourDiff{Replayed: len(reads)}
	for i, call := range reads {
		oldR, err := Call(ctx, a.URL, call)
		if err != nil {
			return d, err
		}
		newR, err := Call(ctx, b.URL, call)
		if err != nil {
			return d, err
		}
		if oldR.Status == newR.Status && reflect.DeepEqual(oldR.Body, newR.Body) {
			continue
		}
		c := Change{Index: i, Request: call, Old: oldR, New: newR}
		c.Because = ex.explain(c)
		d.Changes = append(d.Changes, c)
	}
	return d, nil
}

type explainer struct {
	newFields map[string]bool
	oldSpec   ServiceSpec
	newSpec   ServiceSpec
	newRules  []Rule
	changed   map[string]bool // fields whose definition changed
}

func newExplainer(oldS, newS ServiceSpec) *explainer {
	e := &explainer{newFields: map[string]bool{}, changed: map[string]bool{}, oldSpec: oldS, newSpec: newS}
	old := map[string]Field{}
	for _, f := range oldS.Resource.Fields {
		old[f.Name] = f
	}
	for _, f := range newS.Resource.Fields {
		o, ok := old[f.Name]
		if !ok {
			e.newFields[f.Name] = true
		} else if !reflect.DeepEqual(o, f) {
			e.changed[f.Name] = true
		}
	}
	oldRules := map[string]bool{}
	for _, r := range oldS.Resource.Rules {
		oldRules[fmt.Sprint(r)] = true
	}
	for _, r := range newS.Resource.Rules {
		if !oldRules[fmt.Sprint(r)] {
			e.newRules = append(e.newRules, r)
		}
	}
	return e
}

func (e *explainer) explain(c Change) string {
	op := endpointOf(e.newSpec.Resource, c.Request)

	// Responses that differ only by fields the intent added, carrying
	// their declared default (unless this request set them). A new field
	// showing up with any other value, say on records written by the old
	// version, is not explained: that's a migration bug.
	if c.Old.Status == c.New.Status {
		vals := map[string][]any{}
		if added := onlyAdded(c.Old.Body, c.New.Body, vals); added != nil && allIn(added, e.newFields) {
			setByRequest := map[string]bool{}
			if m, ok := c.Request.Body.(map[string]any); ok {
				for k := range m {
					setByRequest[k] = true
				}
			}
			for _, k := range added {
				def, has := e.defaultOf(k)
				if setByRequest[k] || !has {
					continue
				}
				for _, v := range vals[k] {
					if !reflect.DeepEqual(v, def) {
						return ""
					}
				}
			}
			return "new field " + strings.Join(added, ", ")
		}
	}
	// Requests that use a field the old intent didn't have.
	if m, ok := c.Request.Body.(map[string]any); ok {
		for k := range m {
			if e.newFields[k] {
				return "request uses new field " + k
			}
			if e.changed[k] {
				return "rules for " + k + " changed"
			}
		}
	}
	// A delete now refused by a new rule.
	if op == "delete" && c.New.Status == 409 {
		for _, r := range e.newRules {
			if r.ForbidDeleteWhen != nil {
				return fmt.Sprintf("new rule: no delete while %s is %v", r.ForbidDeleteWhen.Field, r.ForbidDeleteWhen.Equals)
			}
		}
	}
	// Anything downstream of a request the new intent treats differently
	// is reported as unexpected: the reviewer should look.
	return ""
}

func (e *explainer) defaultOf(field string) (any, bool) {
	for _, f := range e.newSpec.Resource.Fields {
		if f.Name == field && f.Default != nil {
			return f.Default, true
		}
	}
	return nil, false
}

// onlyAdded returns the keys present in got but not in want, if that is
// the only difference (recursively through lists); nil otherwise. The
// values of added keys are collected into vals.
func onlyAdded(want, got any, vals map[string][]any) []string {
	if !addedOnly(want, got, vals) || len(vals) == 0 {
		return nil
	}
	var out []string
	for k := range vals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func addedOnly(want, got any, keys map[string][]any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range w {
			if gv, ok := g[k]; !ok || !addedOnly(v, gv, keys) {
				return false
			}
		}
		for k, v := range g {
			if _, ok := w[k]; !ok {
				keys[k] = append(keys[k], v)
			}
		}
		return true
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return false
		}
		for i := range w {
			if !addedOnly(w[i], g[i], keys) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}

func allIn(ks []string, set map[string]bool) bool {
	for _, k := range ks {
		if !set[k] {
			return false
		}
	}
	return true
}

// String renders the diff for whoever signs off: grouped by reason, one
// example each, unexpected changes first.
func (d BehaviourDiff) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replayed %d recorded requests: %d would behave differently", d.Replayed, len(d.Changes))
	if u := len(d.Unexpected()); u > 0 {
		fmt.Fprintf(&b, ", %d UNEXPLAINED by the new intent", u)
	}
	b.WriteString("\n")
	groups := map[string][]Change{}
	var order []string
	for _, c := range d.Changes {
		k := c.Because
		if k == "" {
			k = "⚠ not explained by the intent change"
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], c)
	}
	sort.SliceStable(order, func(i, j int) bool { return strings.HasPrefix(order[i], "⚠") && !strings.HasPrefix(order[j], "⚠") })
	for _, k := range order {
		cs := groups[k]
		c := cs[0]
		fmt.Fprintf(&b, "  %-45s ×%d  e.g. %s %s: %d %s → %d %s\n", k, len(cs), c.Request.Method, c.Request.Path,
			c.Old.Status, compact(c.Old.Body), c.New.Status, compact(c.New.Body))
	}
	return b.String()
}
