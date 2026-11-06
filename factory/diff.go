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
		ex.track(c)
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

	var reads []HTTPCall
	for _, res := range live.Spec.All() {
		list := HTTPCall{Method: "GET", Path: "/" + res.Plural}
		reads = append(reads, list)
		if r, err := Call(ctx, a.URL, list); err == nil {
			if items, ok := r.Body.([]any); ok {
				for _, it := range items {
					if m, ok := it.(map[string]any); ok {
						reads = append(reads, HTTPCall{Method: "GET", Path: fmt.Sprintf("/%s/%v", res.Plural, m[res.Key])})
					}
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
	// diverged records which records ("resource/key") the two versions now
	// hold differently, and the explained change that caused it. Later
	// differences confined to those records follow from that cause.
	diverged  map[string]string
	newFields map[string]map[string]bool // resource -> fields the intent added
	changed   map[string]map[string]bool // resource -> fields whose definition changed
	oldSpec   ServiceSpec
	newSpec   ServiceSpec
}

func newExplainer(oldS, newS ServiceSpec) *explainer {
	e := &explainer{newFields: map[string]map[string]bool{}, changed: map[string]map[string]bool{}, diverged: map[string]string{}, oldSpec: oldS, newSpec: newS}
	for _, nr := range newS.All() {
		e.newFields[nr.Name], e.changed[nr.Name] = map[string]bool{}, map[string]bool{}
		or := oldS.Named(nr.Name)
		old := map[string]Field{}
		if or != nil {
			for _, f := range or.Fields {
				old[f.Name] = f
			}
		}
		for _, f := range nr.Fields {
			o, ok := old[f.Name]
			if !ok {
				e.newFields[nr.Name][f.Name] = true
			} else if !reflect.DeepEqual(o, f) {
				e.changed[nr.Name][f.Name] = true
			}
		}
	}
	return e
}

// resourceFor finds the resource a request addresses, in either spec.
func (e *explainer) resourceFor(c HTTPCall) (*Resource, *Resource, string) {
	nr, op := e.newSpec.Route(c)
	if nr == nil {
		or, oop := e.oldSpec.Route(c)
		return or, nil, oop
	}
	return e.oldSpec.Named(nr.Name), nr, op
}

func (e *explainer) explain(c Change) string {
	or, nr, op := e.resourceFor(c.Request)
	if nr == nil {
		return "" // a resource the new intent doesn't have: let a person look
	}
	if or == nil {
		return "new resource " + nr.Plural
	}
	res := nr.Name

	// Responses that differ only by fields the intent added, carrying
	// their declared default (unless this request set them). A new field
	// showing up with any other value, say on records written by the old
	// version, is not explained: that's a migration bug.
	if c.Old.Status == c.New.Status {
		vals := map[string][]any{}
		if added := onlyAdded(c.Old.Body, c.New.Body, vals); added != nil && allIn(added, e.newFields[res]) {
			setByRequest := map[string]bool{}
			if m, ok := c.Request.Body.(map[string]any); ok {
				for k := range m {
					setByRequest[k] = true
				}
			}
			for _, k := range added {
				def, has := e.defaultOf(res, k)
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
	// Who may write.
	caller := c.Request.Caller
	if caller == "" {
		caller = "anonymous"
	}
	if c.New.Status == 403 && c.Old.Status != 403 && len(nr.Writers) > 0 && !contains(nr.Writers, caller) &&
		(len(or.Writers) == 0 || contains(or.Writers, caller)) {
		return fmt.Sprintf("new access rule: only %s may change %s", strings.Join(nr.Writers, ", "), nr.Plural)
	}
	if c.Old.Status == 403 && c.New.Status != 403 && (len(nr.Writers) == 0 || contains(nr.Writers, caller)) {
		return fmt.Sprintf("%s may now change %s", caller, nr.Plural)
	}
	// Requests that use a field the old intent didn't have, or whose
	// rules changed.
	if m, ok := c.Request.Body.(map[string]any); ok {
		names := make([]string, 0, len(m))
		for k := range m {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if e.newFields[res][k] {
				return "request uses new field " + k
			}
			if e.changed[res][k] {
				return "rules for " + nr.Name + "." + k + " changed"
			}
		}
	}
	// A delete now allowed because a rule was loosened or removed.
	if op == "delete" && c.Old.Status == 409 && c.New.Status < 400 {
		for _, orule := range or.Rules {
			oc := orule.ForbidDeleteWhen
			if oc == nil {
				continue
			}
			name := fmt.Sprintf("no delete while %s is %v", oc.Field, oc.Equals)
			found := false
			for _, nrule := range nr.Rules {
				nc := nrule.ForbidDeleteWhen
				if nc == nil || nc.Field != oc.Field || fmt.Sprint(nc.Equals) != fmt.Sprint(oc.Equals) {
					continue
				}
				found = true
				if c.Request.Caller != "" && contains(nrule.ExceptCallers, c.Request.Caller) && !contains(orule.ExceptCallers, c.Request.Caller) {
					return fmt.Sprintf("%s exempted from rule: %s", c.Request.Caller, name)
				}
			}
			if !found {
				return "rule removed: " + name
			}
		}
	}
	// A delete now refused by a new rule, or by a new relation.
	if op == "delete" && c.New.Status == 409 {
		oldRules := map[string]bool{}
		for _, r := range or.Rules {
			oldRules[fmt.Sprint(r.ForbidDeleteWhen)] = true
		}
		for _, r := range nr.Rules {
			if r.ForbidDeleteWhen != nil && !oldRules[fmt.Sprint(r.ForbidDeleteWhen)] {
				return fmt.Sprintf("new rule: no delete while %s is %v", r.ForbidDeleteWhen.Field, r.ForbidDeleteWhen.Equals)
			}
		}
		for _, other := range e.newSpec.All() {
			for _, f := range other.Fields {
				if f.References == res && (e.newFields[other.Name][f.Name] || e.changed[other.Name][f.Name] || e.oldSpec.Named(other.Name) == nil) {
					return fmt.Sprintf("new relation: %s.%s references %s", other.Name, f.Name, res)
				}
			}
		}
	}
	// Differences confined to records an earlier, explained change made
	// diverge follow from that change.
	if why := e.downstream(c, *nr); why != "" {
		return why
	}
	return ""
}

// track remembers records that now differ between versions because of an
// explained change to a request that writes.
func (e *explainer) track(c Change) {
	if c.Because == "" || c.Request.Method == "GET" || c.Old.Status == c.New.Status {
		return
	}
	_, nr, _ := e.resourceFor(c.Request)
	if nr == nil {
		return
	}
	if k := nr.KeyOf(c.Request); k != "" {
		id := nr.Name + "/" + k
		if _, seen := e.diverged[id]; !seen {
			e.diverged[id] = c.Because
		}
	}
}

func (e *explainer) downstream(c Change, r Resource) string {
	if len(e.diverged) == 0 {
		return ""
	}
	if k := r.KeyOf(c.Request); k != "" {
		if why, ok := e.diverged[r.Name+"/"+k]; ok {
			e.track(Change{Request: c.Request, Old: c.Old, New: c.New, Because: why})
			return "follows from earlier change (" + why + ")"
		}
		return ""
	}
	// A list: every differing item must be a diverged record, or differ
	// only by new fields at their defaults.
	oldL, ok1 := c.Old.Body.([]any)
	newL, ok2 := c.New.Body.([]any)
	if c.Request.Method != "GET" || !ok1 || !ok2 {
		return ""
	}
	index := func(l []any) map[string]any {
		out := map[string]any{}
		for _, it := range l {
			if m, ok := it.(map[string]any); ok {
				out[fmt.Sprint(m[r.Key])] = it
			}
		}
		return out
	}
	oi, ni := index(oldL), index(newL)
	reasons := map[string]bool{}
	for k := range union(oi, ni) {
		o, inOld := oi[k]
		n, inNew := ni[k]
		if inOld && inNew {
			if reflect.DeepEqual(o, n) {
				continue
			}
			vals := map[string][]any{}
			if added := onlyAdded(o, n, vals); added != nil && allIn(added, e.newFields[r.Name]) && e.atDefaults(r.Name, vals) {
				continue
			}
		}
		why, ok := e.diverged[r.Name+"/"+k]
		if !ok {
			return ""
		}
		reasons[why] = true
	}
	if len(reasons) == 0 {
		return ""
	}
	var rs []string
	for r := range reasons {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return "follows from earlier change (" + strings.Join(rs, "; ") + ")"
}

func (e *explainer) atDefaults(res string, vals map[string][]any) bool {
	for k, vs := range vals {
		def, ok := e.defaultOf(res, k)
		if !ok {
			continue
		}
		for _, v := range vs {
			if !reflect.DeepEqual(v, def) {
				return false
			}
		}
	}
	return true
}

func union(a, b map[string]any) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func (e *explainer) defaultOf(res, field string) (any, bool) {
	if r := e.newSpec.Named(res); r != nil {
		for _, f := range r.Fields {
			if f.Name == field && f.Default != nil {
				return f.Default, true
			}
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
		fmt.Fprintf(&b, "  ×%-3d %s\n        e.g. %s %s: %d %s → %d %s\n", len(cs), k, c.Request.Method, c.Request.Path,
			c.Old.Status, short(c.Old.Body), c.New.Status, short(c.New.Body))
	}
	return b.String()
}

func short(v any) string {
	s := compact(v)
	if len(s) > 90 {
		s = s[:87] + "..."
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
