package factory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Contract is what every generated service must do, whoever writes it.
// It is given verbatim to model coders.
const Contract = `- package main, a single file main.go, Go standard library only
- listen on the address in env ADDR (default "127.0.0.1:0") and print exactly one line "LISTENING http://<host:port>" to stdout once listening
- if env DATA is set, it holds all records as one JSON object mapping each resource's plural to an array of record objects; a missing file means empty; other versions of the service may be running against the same file at the same time, so read it at the start of every request (not just at startup) and rewrite it after every change
- a record missing a field gets that field's default (data written by an older version must keep working); fields and resources you don't know about must be kept when rewriting the file, though not returned in responses (data written by a newer version must survive)
- each resource lives at /<plural>: POST /<plural> creates (201 + record), GET /<plural> lists all sorted by key (200), GET /<plural>/{key} (200 or 404), PATCH /<plural>/{key} partially updates (200), DELETE /<plural>/{key} (204)
- GET /<plural>?field=value&... returns only records whose filterable fields equal the given values (compared as text); filtering by a field that isn't filterable is 400
- apply each field's default to fields missing on create; reject unknown fields, missing required fields, wrong types and invalid values with 400; a duplicate key on create is 409; changing the key on PATCH is 400; a missing record is 404
- a field that references another resource must name an existing record of it (400 if not); a record can't be deleted while another record references it (409)
- a field with transitions may only change from a state to one of the states listed for it (409 otherwise); a state with no entry can't change
- the caller is identified by the X-Caller request header; if a resource lists writers, only they may create, update or delete it (403 otherwise; anyone may read); a rule's except_callers lists callers it does not apply to; a delete refused by a rule is 409
- every error response is JSON {"error": "<message>"}; GET /healthz returns 200
- a computed field is set only by its behaviour, never by callers (400 if a request sets it); it is recomputed on every create, update and action
- a resource's behaviours are logic stated in words (rule) and pinned down by the examples that name them: computed behaviours set a computed field; check behaviours may refuse a create or update with a status and reason; an action behaviour is POST /<plural>/{key}/<action>, answering 200 with its result (or the record) or refusing with a status and reason; after an action the record must still be valid, follow its transitions (409 otherwise) and pass its checks
- the spec's examples are the contract: they will be run in order against a fresh instance and every one must pass`

// Coder turns intent into source files. A previous attempt and the
// examples it failed are passed back so a coder can repair its own work.
type Coder interface {
	Code(ctx context.Context, spec ServiceSpec, previous map[string]string, failures []Result) (map[string]string, error)
}

// Codegen is a deterministic coder: it compiles the spec directly into Go.
// It needs no model, covers exactly what the spec language can express,
// and is how the factory runs offline and in tests.
type Codegen struct{}

func (Codegen) Code(_ context.Context, s ServiceSpec, _ map[string]string, _ []Result) (map[string]string, error) {
	if qs := s.Check(); !Ready(qs) {
		return nil, fmt.Errorf("spec is not ready: %v", qs[0])
	}
	if s.HasBehaviours() {
		return nil, fmt.Errorf("this spec has behaviours, which are stated in words: a model has to write them (BehaviourCoder), on top of the structure Codegen generates")
	}
	return map[string]string{"main.go": generate(s)}, nil
}

func goName(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

func goType(t string) string {
	switch t {
	case "bool":
		return "bool"
	case "int":
		return "int64"
	case "number":
		return "float64"
	}
	return "string"
}

func goLiteral(t string, v any) string {
	switch t {
	case "string":
		return strconv.Quote(fmt.Sprint(v))
	case "bool":
		return fmt.Sprint(v)
	case "int":
		f, _ := v.(float64)
		return strconv.FormatInt(int64(f), 10)
	default:
		f, _ := v.(float64)
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

func quoteList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return strings.Join(q, ", ")
}

// generate compiles a spec into one Go file.
func generate(s ServiceSpec) string {
	all := s.All()
	var b strings.Builder
	w := func(format string, a ...any) {
		if len(a) == 0 {
			b.WriteString(format) // literal code: % is not a verb here
			return
		}
		fmt.Fprintf(&b, format, a...)
	}

	w("// Code generated by reversible/factory from intent %q (spec %s). DO NOT EDIT.\n", s.Service, s.Hash())
	w("// This file is a cache: change the intent, not the code.\n\n")
	w("package main\n\n")
	w("import (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"net\"\n\t\"net/http\"\n\t\"os\"\n\t\"regexp\"\n\t\"sort\"\n\t\"sync\"\n)\n\n")
	w("var _ = regexp.MustCompile\n\n")
	w("// %s: %s\n\n", s.Service, oneLine(s.Purpose))

	for _, r := range all {
		genTypes(w, s, *r)
	}
	if s.HasBehaviours() {
		w(`// Refusal is how behaviour code refuses a request: the status and the
// reason the caller sees.
type Refusal struct {
	Status  int
	Message string
}

func refuse(status int, format string, args ...any) *Refusal {
	return &Refusal{Status: status, Message: fmt.Sprintf(format, args...)}
}

`)
		for _, r := range all {
			genBehave(w, *r)
		}
	}

	w(`var (
	mu       sync.Mutex
	dataPath = os.Getenv("DATA")
	unknown  = map[string]json.RawMessage{} // resources other versions keep that this one doesn't know
)

// load reads shared state. Other versions may be running against the same
// data, so it is read at the start of every request, not just once.
func load() {
	unknown = map[string]json.RawMessage{}
`)
	for _, r := range all {
		w("\tload%s(nil)\n", goName(r.Name))
	}
	w(`	if dataPath == "" {
		return
	}
	b, err := os.ReadFile(dataPath)
	if err != nil {
		return
	}
	var data map[string]json.RawMessage
	if json.Unmarshal(b, &data) != nil {
`)
	if len(all) == 1 {
		w("\t\t// a bare array is the older single-resource layout\n")
		w("\t\tvar list []map[string]json.RawMessage\n\t\tif json.Unmarshal(b, &list) == nil {\n\t\t\tload%s(list)\n\t\t}\n", goName(all[0].Name))
	}
	w("\t\treturn\n\t}\n\tfor plural, raw := range data {\n\t\tvar list []map[string]json.RawMessage\n\t\tswitch plural {\n")
	for _, r := range all {
		w("\t\tcase %q:\n\t\t\tif json.Unmarshal(raw, &list) == nil {\n\t\t\t\tload%s(list)\n\t\t\t}\n", r.Plural, goName(r.Name))
	}
	w("\t\tdefault:\n\t\t\tunknown[plural] = raw\n\t\t}\n\t}\n}\n\n")

	w(`// save writes shared state, keeping fields and resources this version
// doesn't know about so a newer version's data survives an older one's write.
func save() {
	if dataPath == "" {
		return
	}
	data := map[string]any{}
	for k, v := range unknown {
		data[k] = v
	}
`)
	for _, r := range all {
		w("\tdata[%q] = dump%s()\n", r.Plural, goName(r.Name))
	}
	w(`	b, _ := json.MarshalIndent(data, "", "  ")
	tmp := dataPath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, dataPath)
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]string{"error": msg})
}

func caller(r *http.Request) string {
	if c := r.Header.Get("X-Caller"); c != "" {
		return c
	}
	return "anonymous"
}

func oneOf(x string, xs ...string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

func body(r *http.Request, known map[string]bool) (map[string]json.RawMessage, string) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil || raw == nil {
		return nil, "body must be a JSON object"
	}
	for k := range raw {
		if !known[k] {
			return nil, fmt.Sprintf("unknown field %q", k)
		}
	}
	return raw, ""
}

func apply(rec any, raw map[string]json.RawMessage) string {
	b, _ := json.Marshal(raw)
	if err := json.Unmarshal(b, rec); err != nil {
		return "invalid value: " + err.Error()
	}
	return ""
}

`)

	w("func main() {\n\tmux := http.NewServeMux()\n")
	w("\tmux.HandleFunc(\"GET /healthz\", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]string{\"status\": \"ok\"}) })\n\n")
	for _, r := range all {
		genHandlers(w, s, *r)
	}
	w(`	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("LISTENING http://%s\n", ln.Addr())
	http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		load()
		mux.ServeHTTP(w, r)
	}))
}
`)
	return b.String()
}

// genTypes writes a resource's type, validation and storage.
func genTypes(w func(string, ...any), s ServiceSpec, r Resource) {
	T := goName(r.Name)
	K := goName(r.Key)
	w("// %s is one %s.\ntype %s struct {\n", T, r.Name, T)
	for _, f := range r.Fields {
		w("\t%s %s `json:%q`\n", goName(f.Name), goType(f.Type), f.Name)
	}
	w("}\n\n")

	w("var known%s = map[string]bool{", T)
	for _, f := range r.Fields {
		w("%q: true, ", f.Name)
	}
	w("}\n\n")

	for _, f := range r.Fields {
		if f.Pattern != "" {
			w("var pattern%s%s = regexp.MustCompile(%s)\n", T, goName(f.Name), strconv.Quote(f.Pattern))
		}
	}

	w("\nfunc defaults%s() %s {\n\tvar r %s\n", T, T, T)
	for _, f := range r.Fields {
		if f.Default != nil {
			w("\tr.%s = %s\n", goName(f.Name), goLiteral(f.Type, f.Default))
		}
	}
	w("\treturn r\n}\n\n")

	w("func validate%s(r %s) string {\n", T, T)
	for _, f := range r.Fields {
		g := goName(f.Name)
		why := ""
		if f.Why != "" {
			why = " (" + f.Why + ")"
		}
		switch f.Type {
		case "string":
			if f.Required {
				w("\tif r.%s == \"\" {\n\t\treturn %q\n\t}\n", g, f.Name+" is required")
			}
			if f.Pattern != "" {
				w("\tif r.%s != \"\" && !pattern%s%s.MatchString(r.%s) {\n\t\treturn %q\n\t}\n", g, T, g, g, fmt.Sprintf("%s must match %s%s", f.Name, f.Pattern, why))
			}
			if len(f.Enum) > 0 {
				w("\tif r.%s != \"\" && !oneOf(r.%s, %s) {\n\t\treturn %q\n\t}\n", g, g, quoteList(f.Enum), fmt.Sprintf("%s must be one of %s%s", f.Name, strings.Join(f.Enum, ", "), why))
			}
		case "int", "number":
			if f.Min != nil {
				w("\tif float64(r.%s) < %v {\n\t\treturn %q\n\t}\n", g, *f.Min, fmt.Sprintf("%s must be at least %g%s", f.Name, *f.Min, why))
			}
			if f.Max != nil {
				w("\tif float64(r.%s) > %v {\n\t\treturn %q\n\t}\n", g, *f.Max, fmt.Sprintf("%s must be at most %g%s", f.Name, *f.Max, why))
			}
		}
	}
	for _, f := range r.Fields {
		if f.References == "" {
			continue
		}
		ref := s.Named(f.References)
		w("\tif r.%s != \"\" {\n\t\tif _, ok := store%s[r.%s]; !ok {\n\t\t\treturn fmt.Sprintf(%q, r.%s)\n\t\t}\n\t}\n",
			goName(f.Name), goName(ref.Name), goName(f.Name), f.Name+": "+ref.Name+" %s not found", goName(f.Name))
	}
	w("\treturn \"\"\n}\n\n")

	var required []string
	for _, f := range r.Fields {
		if f.Required {
			required = append(required, f.Name)
		}
	}
	sort.Strings(required)
	w("var required%s = []string{%s}\n\n", T, quoteList(required))

	w(`var (
	store%[1]s  = map[string]%[1]s{}
	extras%[1]s = map[string]map[string]json.RawMessage{} // fields other versions wrote that this one doesn't know
)

func load%[1]s(list []map[string]json.RawMessage) {
	store%[1]s = map[string]%[1]s{}
	extras%[1]s = map[string]map[string]json.RawMessage{}
	for _, fields := range list {
		raw, _ := json.Marshal(fields)
		r := defaults%[1]s() // records written by an older version get today's defaults
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		store%[1]s[r.%[2]s] = r
		for k, v := range fields {
			if !known%[1]s[k] {
				if extras%[1]s[r.%[2]s] == nil {
					extras%[1]s[r.%[2]s] = map[string]json.RawMessage{}
				}
				extras%[1]s[r.%[2]s][k] = v
			}
		}
	}
}

func dump%[1]s() []map[string]json.RawMessage {
	out := []map[string]json.RawMessage{}
	for _, r := range sorted%[1]s() {
		raw, _ := json.Marshal(r)
		var fields map[string]json.RawMessage
		json.Unmarshal(raw, &fields)
		for k, v := range extras%[1]s[r.%[2]s] {
			if _, ok := fields[k]; !ok {
				fields[k] = v
			}
		}
		out = append(out, fields)
	}
	return out
}

func sorted%[1]s() []%[1]s {
	out := make([]%[1]s, 0, len(store%[1]s))
	for _, r := range store%[1]s {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].%[2]s < out[j].%[2]s })
	return out
}

`, T, K)
}

// genHandlers writes a resource's endpoints.
func genHandlers(w func(string, ...any), s ServiceSpec, r Resource) {
	T := goName(r.Name)
	K := goName(r.Key)
	base := "/" + r.Plural

	writers := ""
	if len(r.Writers) > 0 {
		writers = fmt.Sprintf("\t\tif !oneOf(caller(r), %s) {\n\t\t\tfail(w, 403, caller(r)+%q)\n\t\t\treturn\n\t\t}\n", quoteList(r.Writers), " may not change "+r.Plural)
	}

	// create
	w("\tmux.HandleFunc(\"POST %s\", func(w http.ResponseWriter, r *http.Request) {\n", base)
	w("%s", writers)
	w(`		raw, msg := body(r, known%[1]s)
		if msg != "" {
			fail(w, 400, msg)
			return
		}
%[5]s		for _, f := range required%[1]s {
			if _, ok := raw[f]; !ok {
				fail(w, 400, f+" is required")
				return
			}
		}
		rec := defaults%[1]s()
		if msg := apply(&rec, raw); msg != "" {
			fail(w, 400, msg)
			return
		}
		if msg := validate%[1]s(rec); msg != "" {
			fail(w, 400, msg)
			return
		}
		if _, exists := store%[1]s[rec.%[2]s]; exists {
			fail(w, 409, fmt.Sprintf("%[3]s %%s already exists", rec.%[2]s))
			return
		}
%[4]s		store%[1]s[rec.%[2]s] = rec
		save()
		reply(w, 201, rec)
	})

`, T, K, r.Name, behaveCall(r, "nil", "create"), computedReject(r))

	// list, with filters
	w("\tmux.HandleFunc(\"GET %s\", func(w http.ResponseWriter, r *http.Request) {\n", base)
	w("\t\tq := r.URL.Query()\n\t\tfor name := range q {\n\t\t\tif !oneOf(name")
	var filterable []Field
	for _, f := range r.Fields {
		if f.Filterable {
			filterable = append(filterable, f)
			w(", %q", f.Name)
		}
	}
	w(") {\n\t\t\t\tfail(w, 400, fmt.Sprintf(\"can't filter %s by %%s\", name))\n\t\t\t\treturn\n\t\t\t}\n\t\t}\n", r.Plural)
	w("\t\tout := []%s{}\n\t\tfor _, rec := range sorted%s() {\n", T, T)
	for _, f := range filterable {
		w("\t\t\tif v, ok := q[%q]; ok && fmt.Sprint(rec.%s) != v[0] {\n\t\t\t\tcontinue\n\t\t\t}\n", f.Name, goName(f.Name))
	}
	w("\t\t\tout = append(out, rec)\n\t\t}\n\t\treply(w, 200, out)\n\t})\n\n")

	// get
	w(`	mux.HandleFunc("GET %[1]s/{key}", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := store%[2]s[r.PathValue("key")]
		if !ok {
			fail(w, 404, fmt.Sprintf("%[3]s %%s not found", r.PathValue("key")))
			return
		}
		reply(w, 200, rec)
	})

`, base, T, r.Name)

	// update
	w("\tmux.HandleFunc(\"PATCH %s/{key}\", func(w http.ResponseWriter, r *http.Request) {\n", base)
	w("%s", writers)
	w(`		key := r.PathValue("key")
		raw, msg := body(r, known%[1]s)
		if msg != "" {
			fail(w, 400, msg)
			return
		}
%[5]s		rec, ok := store%[1]s[key]
		if !ok {
			fail(w, 404, fmt.Sprintf("%[3]s %%s not found", key))
			return
		}
		old := rec
		_ = old
		if msg := apply(&rec, raw); msg != "" {
			fail(w, 400, msg)
			return
		}
		if rec.%[2]s != key {
			fail(w, 400, "%[4]s cannot be changed")
			return
		}
		if msg := validate%[1]s(rec); msg != "" {
			fail(w, 400, msg)
			return
		}
`, T, K, r.Name, r.Key, computedReject(r))
	for _, f := range r.Fields {
		if len(f.Transitions) == 0 {
			continue
		}
		g := goName(f.Name)
		w("\t\tif rec.%s != old.%s {\n\t\t\tallowed := map[string][]string{", g, g)
		froms := make([]string, 0, len(f.Transitions))
		for from := range f.Transitions {
			froms = append(froms, from)
		}
		sort.Strings(froms)
		for _, from := range froms {
			w("%q: {%s}, ", from, quoteList(f.Transitions[from]))
		}
		msg := f.Name + " can't change from %s to %s"
		if f.Why != "" {
			msg += " (" + f.Why + ")"
		}
		w("}\n\t\t\tif !oneOf(rec.%s, allowed[old.%s]...) {\n\t\t\t\tfail(w, 409, fmt.Sprintf(%q, old.%s, rec.%s))\n\t\t\t\treturn\n\t\t\t}\n\t\t}\n", g, g, msg, g, g)
	}
	w("%s", behaveCall(r, "&old", "update"))
	w(`		store%[1]s[key] = rec
		save()
		reply(w, 200, rec)
	})

`, T)
	genActions(w, r)

	// delete
	w("\tmux.HandleFunc(\"DELETE %s/{key}\", func(w http.ResponseWriter, r *http.Request) {\n", base)
	w("%s", writers)
	w(`		key := r.PathValue("key")
		rec, ok := store%[1]s[key]
		if !ok {
			fail(w, 404, fmt.Sprintf("%[2]s %%s not found", key))
			return
		}
		_ = rec
`, T, r.Name)
	for _, rule := range r.Rules {
		c := rule.ForbidDeleteWhen
		if c == nil {
			continue
		}
		field, _ := fieldOf(r, c.Field)
		msg := fmt.Sprintf("cannot delete a %s while %s is %v", r.Name, c.Field, c.Equals)
		if rule.Why != "" {
			msg += " (" + rule.Why + ")"
		}
		cond := fmt.Sprintf("rec.%s == %s", goName(c.Field), goLiteral(field.Type, c.Equals))
		for _, ex := range rule.ExceptCallers {
			cond += fmt.Sprintf(" && r.Header.Get(\"X-Caller\") != %q", ex)
		}
		w("\t\tif %s {\n\t\t\tfail(w, 409, %q)\n\t\t\treturn\n\t\t}\n", cond, msg)
	}
	for _, other := range s.All() {
		for _, f := range other.Fields {
			if f.References != r.Name {
				continue
			}
			O := goName(other.Name)
			w("\t\tfor _, o := range sorted%s() {\n\t\t\tif o.%s == key {\n\t\t\t\tfail(w, 409, fmt.Sprintf(%q, key, o.%s))\n\t\t\t\treturn\n\t\t\t}\n\t\t}\n",
				O, goName(f.Name), r.Name+" %s is still referenced by "+other.Name+" %s", goName(other.Key))
		}
	}
	w(`		delete(store%[1]s, key)
		delete(extras%[1]s, key)
		save()
		reply(w, 204, nil)
	})

`, T)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ── behaviours: the structure calls code a model writes ──────────────

// hookName is a behaviour's Go name.
func hookName(b Behaviour) string { return goName(b.Name) }

// computedReject refuses requests that set computed fields.
func computedReject(r Resource) string {
	var b strings.Builder
	for _, bh := range r.Behaviours {
		if bh.Kind != "computed" {
			continue
		}
		fmt.Fprintf(&b, "\t\tif _, ok := raw[%q]; ok {\n\t\t\tfail(w, 400, %q)\n\t\t\treturn\n\t\t}\n", bh.Field, bh.Field+" is computed ("+oneLine(bh.Rule)+"); it can't be set")
	}
	return b.String()
}

// behaveCall runs a resource's computed and check behaviours, if it has any.
func behaveCall(r Resource, old, op string) string {
	if len(r.Behaviours) == 0 {
		return ""
	}
	return fmt.Sprintf("\t\tif ref := behave%s(%s, &rec, %q, caller(r)); ref != nil {\n\t\t\tfail(w, ref.Status, ref.Message)\n\t\t\treturn\n\t\t}\n", goName(r.Name), old, op)
}

// genBehave writes the function that runs a resource's computed
// behaviours, then its checks, around every write.
func genBehave(w func(string, ...any), r Resource) {
	if len(r.Behaviours) == 0 {
		return
	}
	T := goName(r.Name)
	w("// behave%s runs %s's computed behaviours, then the checks for op.\n", T, r.Name)
	w("func behave%s(old *%s, rec *%s, op, caller string) *Refusal {\n", T, T, T)
	for _, b := range r.Behaviours {
		if b.Kind == "computed" {
			w("\tif v, ref := compute%s(*rec); ref != nil {\n\t\treturn ref\n\t} else {\n\t\trec.%s = v\n\t}\n", hookName(b), goName(b.Field))
		}
	}
	for _, b := range r.Behaviours {
		if b.Kind != "check" {
			continue
		}
		var ops []string
		for _, op := range []string{"create", "update"} {
			if b.Checks(op) {
				ops = append(ops, fmt.Sprintf("op == %q", op))
			}
		}
		w("\tif %s {\n\t\tif ref := check%s(old, *rec, caller); ref != nil {\n\t\t\treturn ref\n\t\t}\n\t}\n", strings.Join(ops, " || "), hookName(b))
	}
	w("\treturn nil\n}\n\n")
}

// genActions writes a resource's action endpoints.
func genActions(w func(string, ...any), r Resource) {
	T := goName(r.Name)
	K := goName(r.Key)
	writers := ""
	if len(r.Writers) > 0 {
		writers = fmt.Sprintf("\t\tif !oneOf(caller(r), %s) {\n\t\t\tfail(w, 403, caller(r)+%q)\n\t\t\treturn\n\t\t}\n", quoteList(r.Writers), " may not change "+r.Plural)
	}
	for _, b := range r.Behaviours {
		if b.Kind != "action" {
			continue
		}
		w("\t// %s: %s\n", b.Name, oneLine(b.Rule))
		w("\tmux.HandleFunc(\"POST /%s/{key}/%s\", func(w http.ResponseWriter, r *http.Request) {\n", r.Plural, b.Action)
		w("%s", writers)
		w(`		key := r.PathValue("key")
		rec, ok := store%[1]s[key]
		if !ok {
			fail(w, 404, fmt.Sprintf("%[3]s %%s not found", key))
			return
		}
		in := map[string]json.RawMessage{}
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				fail(w, 400, "body must be a JSON object")
				return
			}
		}
		old := rec
		out, ref := action%[4]s(&rec, in, caller(r))
		if ref != nil {
			fail(w, ref.Status, ref.Message)
			return
		}
		if rec.%[2]s != key {
			fail(w, 500, "%[5]s changed the key")
			return
		}
		if msg := validate%[1]s(rec); msg != "" {
			fail(w, 500, "%[5]s left the record invalid: "+msg)
			return
		}
`, T, K, r.Name, hookName(b), b.Name)
		for _, f := range r.Fields {
			if len(f.Transitions) == 0 {
				continue
			}
			g := goName(f.Name)
			w("\t\tif rec.%s != old.%s {\n\t\t\tallowed := map[string][]string{", g, g)
			froms := make([]string, 0, len(f.Transitions))
			for from := range f.Transitions {
				froms = append(froms, from)
			}
			sort.Strings(froms)
			for _, from := range froms {
				w("%q: {%s}, ", from, quoteList(f.Transitions[from]))
			}
			msg := f.Name + " can't change from %s to %s"
			if f.Why != "" {
				msg += " (" + f.Why + ")"
			}
			w("}\n\t\t\tif !oneOf(rec.%s, allowed[old.%s]...) {\n\t\t\t\tfail(w, 409, fmt.Sprintf(%q, old.%s, rec.%s))\n\t\t\t\treturn\n\t\t\t}\n\t\t}\n", g, g, msg, g, g)
		}
		w("%s", behaveCall(r, "&old", "update"))
		w(`		store%[1]s[key] = rec
		save()
		if out == nil {
			out = rec
		}
		reply(w, 200, out)
	})

`, T)
	}
}

// Hooks lists the functions a model must write for a spec's behaviours,
// as Go declarations with doc comments.
func Hooks(s ServiceSpec) string {
	var b strings.Builder
	for _, r := range s.All() {
		T := goName(r.Name)
		for _, bh := range r.Behaviours {
			fmt.Fprintf(&b, "// %s (%s, on %s): %s\n", bh.Name, bh.Kind, r.Plural, oneLine(bh.Rule))
			if bh.Why != "" {
				fmt.Fprintf(&b, "// Why: %s\n", oneLine(bh.Why))
			}
			switch bh.Kind {
			case "computed":
				f, _ := fieldOf(*r, bh.Field)
				fmt.Fprintf(&b, "// Returns the value of %s.%s for rec (called on every create, update and action), or a refusal.\nfunc compute%s(rec %s) (%s, *Refusal)\n\n", r.Name, bh.Field, hookName(bh), T, goType(f.Type))
			case "check":
				on := "create and update"
				if len(bh.On) > 0 {
					on = strings.Join(bh.On, " and ")
				}
				fmt.Fprintf(&b, "// Called on %s after computed fields are set; old is nil on create. Return nil to allow.\nfunc check%s(old *%s, rec %s, caller string) *Refusal\n\n", on, hookName(bh), T, T)
			case "action":
				fmt.Fprintf(&b, "// POST /%s/{key}/%s. May change *rec; in is the request body's fields. Return the response body (nil for the record), or a refusal.\nfunc action%s(rec *%s, in map[string]json.RawMessage, caller string) (any, *Refusal)\n\n", r.Plural, bh.Action, hookName(bh), T)
			}
		}
	}
	return b.String()
}

// Structure is the generated main.go for a spec: everything but its
// behaviours. A BehaviourCoder's model writes against it.
func Structure(s ServiceSpec) string { return generate(s) }
