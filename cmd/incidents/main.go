// Command incidents builds, changes and releases an incident tracker for
// an on-call rotation from intent alone. Nobody writes or edits code.
//
// The domain is an example. The same factory, unchanged, builds the
// feature-flag service in cmd/factory; this demo exists to show the
// mechanism isn't tied to one shape of software: several resources that
// reference each other, records that move through a lifecycle, filters,
// and who may change what, all stated as intent and all generated.
//
//	go run ./cmd/incidents                        # offline: deterministic coder
//	ANTHROPIC_API_KEY=... go run ./cmd/incidents  # Claude writes the code for each version of the intent
//	go run ./cmd/incidents -keep                  # keep the generated code and data
//	go run ./cmd/incidents -serve :8080           # then keep production serving, with an inspector at /_factory/
//
// Live, the intent is still the hand-written spec (Priya's conversation is
// shown, not captured): this run isolates the claim that a model can write
// the software from intent, and that the review, not a person reading the
// code, decides whether what it wrote is right. cmd/factory has Claude
// draft the spec from conversation too.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/cmd/internal/serve"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/factory"
	"github.com/PureMunky/reversible/model"
	"github.com/PureMunky/reversible/telemetry"
)

//go:embed intent/*.json
var intents embed.FS

var conversation1 = []factory.Turn{
	{Who: "priya", Says: "On-call needs one place to track incidents. Every incident is about a service we know, has a severity, and moves open → acknowledged → resolved."},
	{Who: "priya", Says: "Only the platform team registers services, and a service can't be removed while incidents point at it."},
	{Who: "priya", Says: "Never delete an open incident, and a resolved one stays resolved: if it comes back, that's a new incident. The status page needs to list what's being worked on."},
}

var conversation2 = []factory.Turn{
	{Who: "priya", Says: "Our postmortems keep finding incidents marked resolved that were never actually mitigated. Add a mitigated step: acknowledged → mitigated → resolved. Open straight to resolved is still fine for false alarms."},
	{Who: "priya", Says: "Every incident should be able to name a commander. And only on-call changes incidents; everyone else reads."},
}

func main() {
	keep := flag.Bool("keep", false, "keep the work directory (generated code, builds, production data)")
	serveAddr := flag.String("serve", "", "after the story, keep production serving on this address (e.g. :8080) until stopped, with an inspector at /_factory/")
	flag.Parse()
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			f, ok := r.(fatal)
			if !ok {
				panic(r)
			}
			fmt.Fprintln(os.Stderr, "error:", f.err)
			os.Exit(1)
		}
	}()
	check := func(err error) {
		if err != nil {
			panic(fatal{err})
		}
	}
	work, err := os.MkdirTemp("", "incidents-demo-")
	check(err)
	if *keep {
		defer fmt.Printf("\nwork directory kept: %s\n", work)
	} else {
		defer os.RemoveAll(work)
	}

	var coder factory.Coder = factory.Codegen{}
	var usage func() string
	mode := "offline: the spec is written alongside each conversation, and code is compiled from it by a deterministic generator (set ANTHROPIC_API_KEY for Claude to write the code)"
	if a, ok := model.FromEnv(); ok {
		a.Log = func(l string) { fmt.Println("   ·", l) }
		coder = factory.ModelCoder{Model: a}
		usage = func() string { return a.Usage().String() }
		mode = "live: Claude writes every version of the code from the intent; the examples and the review decide whether it ships"
		defer func() { fmt.Println("   model usage:", usage()) }()
	}
	fab := &factory.Factory{Root: filepath.Join(work, "builds"), Coder: coder}
	keys := decision.NewKeyring()
	for _, a := range []string{"priya", "factory"} {
		_, err := keys.Generate(a)
		check(err)
	}
	store := decision.NewMemoryLog()
	chain, err := decision.NewChainLog(store, keys)
	check(err)
	g, err := decision.Open(chain)
	check(err)
	router := &factory.Router{DataPath: filepath.Join(work, "production", "incidents.json")}
	check(os.MkdirAll(filepath.Dir(router.DataPath), 0o755))
	defer router.Stop()
	prod := httptest.NewServer(router)
	defer prod.Close()
	rt := reversible.New()
	closeSink, err := telemetry.SinkFromEnv()
	check(err)
	defer closeSink()
	if *serveAddr != "" {
		// Production, the inspector and telemetry listen from the start, so
		// the run can be watched while it happens.
		srv, err := serve.Start(*serveAddr, router, serve.Info{Why: func() string { return whyLive(g, router) }, Usage: usage, Mode: mode})
		check(err)
		defer srv.Close()
		defer func() { // after the story, before the router stops; not after an error
			if r := recover(); r != nil {
				panic(r)
			}
			check(srv.Ready())
		}()
	}

	head := func(s string) {
		fmt.Printf("\n━━━ %s\n", s)
		telemetry.Emit("story.step", telemetry.F{"demo": "incidents", "step": s})
	}
	say := func(turns []factory.Turn) {
		for _, t := range turns {
			fmt.Printf("   %s: %s\n", t.Who, t.Says)
		}
	}
	spec := func(file string) factory.ServiceSpec {
		b, err := intents.ReadFile("intent/" + file)
		check(err)
		s, err := factory.ParseSpec(b)
		check(err)
		return s
	}
	run := func(calls []factory.HTTPCall) {
		for _, c := range calls {
			_, err := factory.Call(ctx, prod.URL, c)
			check(err)
		}
	}
	build := func(s factory.ServiceSpec) *factory.Version {
		v, err := fab.Build(ctx, s)
		if v != nil {
			fmt.Print(indent(v.Report()))
		}
		check(err)
		return v
	}
	generated, reads := 0, 0
	canary := func(decisionID string, v *factory.Version) factory.CanaryResult {
		c, err := factory.StartCanary(ctx, router, v, g, rt, decisionID, factory.CanaryOptions{
			Share: 0.2, MinSamples: 30, MaxSamples: 80, Salt: "incidents-demo-" + v.SpecHash, Actor: "factory"})
		check(err)
		fmt.Printf("   canary: %s takes 20%% of traffic beside %s, on the same data\n", v.SpecHash, router.Live().SpecHash)
		var res factory.CanaryResult
		for round := 1; round <= 20; round++ {
			run(readers(200, reads))
			reads += 200
			res, err = c.Evaluate(ctx)
			check(err)
			fmt.Printf("   +%d requests: %s\n", round*200, res)
			if res.Promoted || res.Stopped {
				break
			}
		}
		fmt.Printf("   live: %s\n", router.Live().SpecHash)
		return res
	}

	fmt.Println("Software built from intent: an incident tracker. Nobody writes or edits code in this run.")
	fmt.Println("The domain is an example: this is the same factory that builds the feature-flag service, with nothing incident-specific in it.")
	fmt.Println("mode:", mode)

	head("1 · Priya, the on-call lead, describes what she needs")
	say(conversation1)
	_, err = g.Goal("incidents", "an incident tracker for the on-call rotation", "priya")
	check(err)
	spec1 := spec("incidents-1.json")
	qs := spec1.Check()
	fmt.Printf("   The intent: %d resources (%s), %d examples; the spec checker has %d question(s)\n",
		len(spec1.All()), resourceNames(spec1), len(spec1.Examples), len(qs))
	for _, q := range qs {
		fmt.Println("   ?", q)
	}
	_, err = g.Decide(decision.Decision{ID: "intent-1", Parents: []string{"incidents"}, Actor: "priya",
		Summary: "services and incidents; lifecycle; who registers services", Rationale: joinTurns(conversation1)})
	check(err)

	head("2 · The factory generates, builds and verifies it")
	v1 := build(spec1)
	generated += v1.Lines()

	head("3 · Priya signs off on the behaviour; it goes live")
	for _, r := range v1.Results {
		fmt.Printf("   ✓ %s\n", r.Example.Name)
	}
	_, err = g.Decide(decision.Decision{ID: "signoff-1", Parents: []string{"intent-1"}, Actor: "priya",
		Summary: "release incidents " + v1.SpecHash, Rationale: "all examples pass; first release"})
	check(err)
	_, err = g.Execute(ctx, rt, "signoff-1", reversible.Intent{ID: "release-" + v1.SpecHash, Effects: []reversible.Effect{router.Release(v1)}})
	check(err)
	fmt.Printf("   live: %s\n", router.Live().SpecHash)

	head("4 · A week of on-call")
	run(weekOne())
	fmt.Printf("   %d requests recorded; %s\n", len(router.Traffic()), census(ctx, prod.URL))

	head("5 · Priya changes the intent. Not the code")
	say(conversation2)
	spec2 := spec("incidents-2.json")
	_, err = g.Decide(decision.Decision{ID: "intent-2", Parents: []string{"intent-1"}, Actor: "priya",
		Summary: "mitigated step; commanders; only on-call changes incidents", Rationale: joinTurns(conversation2)})
	check(err)
	v2 := build(spec2)
	generated += v2.Lines()

	head("6 · What would change, on real traffic and real data")
	review, err := factory.ReviewCandidate(ctx, router, v2)
	check(err)
	fmt.Print(indent(review.String()))
	if !review.Clean() {
		head("7 · Priya does NOT sign off")
		fmt.Println("   Some changes aren't explained by anything Priya asked for. Nothing is released.")
		finish(g, store, keys, generated, "described the service twice and refused a release nobody asked for")
		return
	}

	head("7 · Priya signs off on that reality; v2 is released as a canary")
	_, err = g.Decide(decision.Decision{ID: "signoff-2", Parents: []string{"intent-2"}, Actor: "priya",
		Summary: "release incidents " + v2.SpecHash, Rationale: "every behaviour change is explained by intent-2"})
	check(err)
	if res := canary("signoff-2", v2); res.Stopped {
		finish(g, store, keys, generated, "described the service twice; the canary stopped a bad release")
		return
	}
	fmt.Printf("   incidents opened under v1 keep their state: %s\n", census(ctx, prod.URL))

	head("8 · Another week. The factory watches what v2 actually did")
	run(weekTwo())
	findings, err := factory.Observe(ctx, router)
	check(err)
	fmt.Printf("   compared every request since the release with what v1 would have done from the same data: %d finding(s)\n", len(findings))
	for _, f := range findings {
		fmt.Println("   •", f)
	}

	head("9 · Each finding becomes a question for Priya, with intent changes as answers")
	answers := map[string]string{"status-page-bot": "let ", "on-call": "keep"}
	spec3 := spec2
	var asked []string
	for _, f := range findings {
		prop := factory.Propose(f, spec3)
		fmt.Println("   ?", prop.Question)
		choice := -1
		answer, ok := answers[f.Caller]
		for i, o := range prop.Options {
			mark := " "
			if ok && choice < 0 && strings.HasPrefix(o.Label, answer) {
				mark, choice = "→", i
			}
			fmt.Printf("     %s %d. %s\n", mark, i+1, o.Label)
		}
		if choice < 0 {
			fmt.Println("     (left for later)")
			continue
		}
		fmt.Printf("     Priya answers %d.\n", choice+1)
		asked = append(asked, fmt.Sprintf("%s → %s", prop.Question, prop.Options[choice].Label))
		spec3 = prop.Options[choice].Spec
		_, err = g.Observe(decision.Outcome{ID: fmt.Sprintf("finding-%d", len(asked)), Parents: []string{"signoff-2"}, Actor: "factory",
			Summary: f.String(), Metrics: map[string]float64{"requests": float64(f.Count)}})
		check(err)
	}
	_, err = g.Decide(decision.Decision{ID: "intent-3", Parents: []string{"intent-2"}, Actor: "priya",
		Summary: "answers to production's questions", Rationale: strings.Join(asked, " / ")})
	check(err)

	head("10 · The answers are the new intent. The factory regenerates")
	v3 := build(spec3)
	generated += v3.Lines()
	fmt.Printf("   %d of its %d examples were captured from production requests (with the records they act on)\n", captured(spec3), len(spec3.Examples))
	d3, err := factory.Diff(ctx, router.Traffic(), v2, v3)
	check(err)
	fmt.Print(indent(d3.String()))
	if len(d3.Unexpected()) > 0 {
		check(fmt.Errorf("unexplained changes in v3"))
	}
	_, err = g.Decide(decision.Decision{ID: "signoff-3", Parents: []string{"intent-3"}, Actor: "priya",
		Summary: "release incidents " + v3.SpecHash, Rationale: "behaviour changes match the answers"})
	check(err)
	canary("signoff-3", v3)
	run(weekThree())
	after, err := factory.Observe(ctx, router)
	check(err)
	if len(after) == 0 {
		fmt.Println("   A week later: no new findings; the status page writes its summaries, on-call mitigates before resolving.")
	} else {
		for _, f := range after {
			fmt.Println("   •", f)
		}
	}

	finish(g, store, keys, generated, "described an incident tracker in two short conversations, signed off on behaviour three times, and answered two questions production raised (canaries and evidence decided the releases)")
}

// whyLive explains the live version from the ledger.
func whyLive(g *decision.Graph, router *factory.Router) string {
	live := router.Live()
	if live == nil {
		return ""
	}
	for _, id := range []string{"promote-" + live.SpecHash, "release-" + live.SpecHash} {
		if why, err := g.ExplainIntent(id); err == nil {
			return why
		}
	}
	return ""
}

func finish(g *decision.Graph, store decision.Log, keys *decision.Keyring, generated int, did string) {
	fmt.Printf("\n━━━ What people did\n")
	fmt.Println("   Priya " + did)
	fmt.Printf("   lines of code written or edited by a person: 0   (generated: %d, kept only as a cache)\n", generated)
	records, _ := store.Load()
	fmt.Printf("   decision ledger: %d signed records", len(records))
	if decision.VerifyChain(records, keys.PublicKeys()) == nil {
		fmt.Print(", chain verifies ✓")
	}
	fmt.Println()
	fmt.Println("\n   What's still missing for any software to be built this way: the spec language covers")
	fmt.Println("   records, relations, lifecycles and access. Computation, jobs, events and integrations")
	fmt.Println("   with other systems still have to be written by hand (see PRINCIPLES.md).")
}

// weekOne: the platform team registers services; on-call works incidents;
// the status page bot rewrites summaries for customers.
func weekOne() []factory.HTTPCall {
	var out []factory.HTTPCall
	for _, s := range [][2]string{{"checkout", "payments"}, {"search", "discovery"}, {"login", "identity"}, {"payments-api", "payments"}, {"notifications", "growth"}} {
		out = append(out, factory.HTTPCall{Method: "POST", Path: "/services", Body: map[string]any{"name": s[0], "team": s[1]}, Caller: "platform-team"})
	}
	out = append(out,
		factory.HTTPCall{Method: "PATCH", Path: "/services/checkout", Body: map[string]any{"tier": "critical"}, Caller: "platform-team"},
		factory.HTTPCall{Method: "PATCH", Path: "/services/login", Body: map[string]any{"tier": "critical"}, Caller: "platform-team"},
		factory.HTTPCall{Method: "POST", Path: "/services", Body: map[string]any{"name": "experiments", "team": "growth"}, Caller: "growth"},
	)
	open := func(id, svc, sev, summary string) factory.HTTPCall {
		return factory.HTTPCall{Method: "POST", Path: "/incidents", Body: map[string]any{"id": id, "service": svc, "severity": sev, "summary": summary}, Caller: "on-call"}
	}
	set := func(id, caller string, body map[string]any) factory.HTTPCall {
		return factory.HTTPCall{Method: "PATCH", Path: "/incidents/" + id, Body: body, Caller: caller}
	}
	out = append(out,
		open("inc-101", "checkout", "sev1", "checkout errors after deploy"),
		open("inc-102", "search", "sev3", "slow autocomplete"),
		open("inc-103", "login", "sev2", "SSO timeouts"),
		open("inc-104", "notifications", "sev3", "duplicate emails"),
		open("inc-105", "billing", "sev2", "invoices delayed"), // not a known service
		set("inc-101", "on-call", map[string]any{"status": "acknowledged"}),
		set("inc-101", "status-page-bot", map[string]any{"summary": "Some customers can't complete checkout"}),
		set("inc-103", "on-call", map[string]any{"status": "acknowledged"}),
		set("inc-103", "status-page-bot", map[string]any{"summary": "Some customers can't sign in"}),
		set("inc-102", "on-call", map[string]any{"status": "resolved"}), // false alarm
		set("inc-101", "on-call", map[string]any{"status": "resolved"}),
		set("inc-101", "on-call", map[string]any{"status": "open"}), // it came back: refused
		open("inc-106", "checkout", "sev1", "checkout errors again"),
		factory.HTTPCall{Method: "DELETE", Path: "/incidents/inc-104", Caller: "on-call"}, // still open: refused
		factory.HTTPCall{Method: "DELETE", Path: "/services/search", Caller: "platform-team"},
		factory.HTTPCall{Method: "DELETE", Path: "/incidents/inc-102", Caller: "on-call"},
		factory.HTTPCall{Method: "DELETE", Path: "/services/search", Caller: "platform-team"},
		factory.HTTPCall{Method: "GET", Path: "/incidents?status=open", Caller: "status-page-bot"},
		factory.HTTPCall{Method: "GET", Path: "/incidents?service=checkout", Caller: "on-call"},
	)
	return out
}

// readers is most of the tracker's traffic: the status page and dashboards
// polling, with on-call occasionally naming a commander.
func readers(n, offset int) []factory.HTTPCall {
	paths := []string{"/incidents?status=open", "/incidents?status=acknowledged", "/services/checkout", "/incidents/inc-106", "/services?tier=critical", "/incidents/inc-103"}
	var out []factory.HTTPCall
	for i := offset; i < offset+n; i++ {
		if i%40 == 0 {
			out = append(out, factory.HTTPCall{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"severity": []string{"sev1", "sev2"}[i/40%2]}, Caller: "on-call"})
			continue
		}
		out = append(out, factory.HTTPCall{Method: "GET", Path: paths[i%len(paths)], Caller: "dashboards"})
	}
	return out
}

// weekTwo: old habits meet the new intent. The status page keeps writing
// customer-facing summaries; on-call resolves without mitigating.
func weekTwo() []factory.HTTPCall {
	return []factory.HTTPCall{
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"status": "acknowledged", "commander": "sam"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"summary": "Checkout is failing for some customers"}, Caller: "status-page-bot"},
		{Method: "PATCH", Path: "/incidents/inc-103", Body: map[string]any{"status": "resolved"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-103", Body: map[string]any{"summary": "Sign-in is recovering"}, Caller: "status-page-bot"},
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"status": "resolved"}, Caller: "on-call"},
		{Method: "POST", Path: "/incidents", Body: map[string]any{"id": "inc-107", "service": "payments-api", "severity": "sev2", "summary": "card declines spiking"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-107", Body: map[string]any{"status": "resolved"}, Caller: "on-call"}, // false alarm: still fine
		{Method: "GET", Path: "/incidents?status=acknowledged", Caller: "status-page-bot"},
	}
}

// weekThree: the same people, under the answered intent.
func weekThree() []factory.HTTPCall {
	return []factory.HTTPCall{
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"status": "mitigated"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"summary": "A fix is in place; we're monitoring"}, Caller: "status-page-bot"},
		{Method: "PATCH", Path: "/incidents/inc-106", Body: map[string]any{"status": "resolved"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-103", Body: map[string]any{"status": "mitigated"}, Caller: "on-call"},
		{Method: "PATCH", Path: "/incidents/inc-103", Body: map[string]any{"status": "resolved"}, Caller: "on-call"},
		{Method: "GET", Path: "/incidents?status=resolved", Caller: "status-page-bot"},
	}
}

// census counts services and incidents by status.
func census(ctx context.Context, url string) string {
	s, _ := factory.Call(ctx, url, factory.HTTPCall{Method: "GET", Path: "/services"})
	i, _ := factory.Call(ctx, url, factory.HTTPCall{Method: "GET", Path: "/incidents"})
	services, _ := s.Body.([]any)
	incidents, _ := i.Body.([]any)
	by := map[string]int{}
	for _, x := range incidents {
		by[fmt.Sprint(x.(map[string]any)["status"])]++
	}
	var parts []string
	for k, n := range by {
		parts = append(parts, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d services, %d incidents (%s)", len(services), len(incidents), strings.Join(parts, ", "))
}

func resourceNames(s factory.ServiceSpec) string {
	var out []string
	for _, r := range s.All() {
		out = append(out, r.Plural)
	}
	return strings.Join(out, ", ")
}

func captured(s factory.ServiceSpec) int {
	n := 0
	for _, e := range s.Examples {
		if strings.HasPrefix(e.Name, "production:") {
			n++
		}
	}
	return n
}

type fatal struct{ err error }

func joinTurns(ts []factory.Turn) string {
	var parts []string
	for _, t := range ts {
		parts = append(parts, t.Who+": "+t.Says)
	}
	return strings.Join(parts, " / ")
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
