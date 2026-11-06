// Command bookings builds appointment booking for a small business from
// intent, including the part no generator can write: logic stated in
// words. Prices with a weekend surcharge, no double-booking, opening
// hours, cancellations with refunds: each is a behaviour in the spec, a
// rule in plain language pinned down by examples. The factory generates
// the structure; a model writes the behaviours against it; the examples,
// the review and the canary judge the result, as for anything else.
//
// Booking is the reference SaaS for the build-out in docs/SAAS-PLAN.md.
// It's an example: nothing in the factory knows about bookings.
//
//	go run ./cmd/bookings                         # replays recorded model replies; no key
//	ANTHROPIC_API_KEY=... go run ./cmd/bookings   # Claude writes the behaviours, fresh
//	... -record dir                               # live, and save every reply for offline replay
//	go run ./cmd/bookings -show-prompts           # print what the model is sent
//	go run ./cmd/bookings -serve :8080            # then keep serving, with /_factory/
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
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

// Replies a model gave to the exact prompts these specs produce, so the
// story runs without a key. Written by Claude for those prompts; a live
// run asks afresh, and -record replaces them.
//
//go:embed recordings/*.reply
var recordings embed.FS

var conversation1 = []factory.Turn{
	{Who: "maya", Says: "I run a small salon. Customers book a service (a haircut, colour) for a date and time; I set the menu and prices."},
	{Who: "maya", Says: "Weekends cost 25% more. Nobody can be double-booked. We're open 9 to 5, and an appointment has to finish by close."},
	{Who: "maya", Says: "Customers can cancel: a full refund if it's at least a day ahead, half if it's later than that."},
}

var conversation2 = []factory.Turn{
	{Who: "maya", Says: "We're closing on Sundays from now on: no Sunday bookings."},
	{Who: "maya", Says: "And late cancellations are killing us. Under 24 hours' notice gets no refund at all."},
}

func main() {
	keep := flag.Bool("keep", false, "keep the work directory (generated code, builds, production data)")
	serveAddr := flag.String("serve", "", "keep production serving on this address (e.g. :8080) after the story, with an inspector at /_factory/")
	record := flag.String("record", "", "live only: save every model reply in this directory, for offline replay")
	showPrompts := flag.Bool("show-prompts", false, "print each prompt the model is sent")
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
	work, err := os.MkdirTemp("", "bookings-demo-")
	check(err)
	if *keep {
		defer fmt.Printf("\nwork directory kept: %s\n", work)
	} else {
		defer os.RemoveAll(work)
	}

	// The model that writes behaviours: Claude when there's a key, else the
	// recorded replies to the same prompts.
	recorded, err := fs.Sub(recordings, "recordings")
	check(err)
	var m model.Completer = model.Replayer{FS: recorded}
	var usage func() string
	mode := "offline: the behaviour code is a recorded model reply to the exact prompt this run sends (no key needed; a changed spec has no recording and stops). The structure is generated"
	if a, ok := model.FromEnv(); ok {
		a.Log = func(l string) { fmt.Println("   ·", l) }
		m, usage = a, func() string { return a.Usage().String() }
		mode = "live: Claude writes the behaviour code from the intent; the structure is generated"
		if *record != "" {
			m = model.Recorder{Model: a, Dir: *record}
			mode += "; replies are recorded in " + *record
		}
		defer func() { fmt.Println("   model usage:", usage()) }()
	}
	if *showPrompts {
		inner := m
		m = model.Func(func(ctx context.Context, system, prompt string) (string, error) {
			fmt.Printf("\n   ┌── prompt (key %s)\n", model.RecordingKey(system, prompt))
			for _, l := range strings.Split("SYSTEM: "+system+"\n\n"+prompt, "\n") {
				fmt.Println("   │ " + l)
			}
			fmt.Printf("   └──\n\n")
			return inner.Complete(ctx, system, prompt)
		})
	}
	fab := &factory.Factory{Root: filepath.Join(work, "builds"), Coder: factory.BehaviourCoder{Model: m}}

	keys := decision.NewKeyring()
	for _, a := range []string{"maya", "factory"} {
		_, err := keys.Generate(a)
		check(err)
	}
	store := decision.NewMemoryLog()
	chain, err := decision.NewChainLog(store, keys)
	check(err)
	g, err := decision.Open(chain)
	check(err)
	router := &factory.Router{DataPath: filepath.Join(work, "production", "bookings.json")}
	check(os.MkdirAll(filepath.Dir(router.DataPath), 0o755))
	defer router.Stop()
	prod := httptest.NewServer(router)
	defer prod.Close()
	rt := reversible.New()
	closeSink, err := telemetry.SinkFromEnv()
	check(err)
	defer closeSink()
	if *serveAddr != "" {
		srv, err := serve.Start(*serveAddr, router, serve.Info{Why: func() string { return whyLive(g, router) }, Usage: usage, Mode: mode})
		check(err)
		defer srv.Close()
		defer func() {
			if r := recover(); r != nil {
				panic(r)
			}
			check(srv.Ready())
		}()
	}

	head := func(s string) {
		fmt.Printf("\n━━━ %s\n", s)
		telemetry.Emit("story.step", telemetry.F{"demo": "bookings", "step": s})
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
	modelLines, structureLines := 0, 0
	build := func(s factory.ServiceSpec) *factory.Version {
		v, err := fab.Build(ctx, s)
		if v != nil {
			fmt.Print(indent(v.Report()))
		}
		check(err)
		st, bh := lines(v.Files["main.go"]), lines(v.Files["behaviours.go"])
		structureLines += st
		modelLines += bh
		fmt.Printf("   generated structure: %d lines · behaviour code written by the model: %d lines\n", st, bh)
		return v
	}
	reads := 0
	canary := func(decisionID string, v *factory.Version) factory.CanaryResult {
		c, err := factory.StartCanary(ctx, router, v, g, rt, decisionID, factory.CanaryOptions{
			Share: 0.2, MinSamples: 30, MaxSamples: 80, Salt: "bookings-demo-" + v.SpecHash, Actor: "factory"})
		check(err)
		fmt.Printf("   canary: %s takes 20%% of traffic beside %s, on the same data\n", v.SpecHash, router.Live().SpecHash)
		var res factory.CanaryResult
		for round := 1; round <= 20; round++ {
			run(browsing(200, reads))
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

	fmt.Println("Software built from intent, including the logic only words can state. Nobody writes or edits code in this run.")
	fmt.Println("The domain is an example: the factory knows nothing about bookings.")
	fmt.Println("mode:", mode)

	head("1 · Maya describes what she needs")
	say(conversation1)
	_, err = g.Goal("bookings", "appointment booking for a small salon", "maya")
	check(err)
	spec1 := spec("bookings-1.json")
	var behaviours []string
	for _, r := range spec1.All() {
		for _, b := range r.Behaviours {
			behaviours = append(behaviours, fmt.Sprintf("%s (%s)", b.Name, b.Kind))
		}
	}
	fmt.Printf("   The intent: %d resources, %d examples, and %d behaviours stated in words: %s\n", len(spec1.All()), len(spec1.Examples), len(behaviours), strings.Join(behaviours, ", "))
	fmt.Printf("   The spec checker has %d question(s).\n", len(spec1.Check()))
	_, err = g.Decide(decision.Decision{ID: "intent-1", Parents: []string{"bookings"}, Actor: "maya",
		Summary: "services and bookings; weekend pricing, no double-booking, opening hours, cancellations", Rationale: joinTurns(conversation1)})
	check(err)

	head("2 · The structure is generated; the behaviours need a model")
	if _, err := (factory.Codegen{}).Code(ctx, spec1, nil, nil); err != nil {
		fmt.Println("   The deterministic generator, alone:", err)
	}
	v1 := build(spec1)

	head("3 · Maya signs off on the behaviour; it goes live")
	for _, r := range v1.Results {
		if r.Example.Behaviour != "" {
			fmt.Printf("   ✓ [%s] %s\n", r.Example.Behaviour, r.Example.Name)
		}
	}
	fmt.Printf("   ✓ and %d structural examples\n", countStructural(v1))
	_, err = g.Decide(decision.Decision{ID: "signoff-1", Parents: []string{"intent-1"}, Actor: "maya",
		Summary: "release bookings " + v1.SpecHash, Rationale: "all examples pass, including every behaviour's"})
	check(err)
	_, err = g.Execute(ctx, rt, "signoff-1", reversible.Intent{ID: "release-" + v1.SpecHash, Effects: []reversible.Effect{router.Release(v1)}})
	check(err)
	fmt.Printf("   live: %s\n", router.Live().SpecHash)

	head("4 · A week of bookings")
	run(weekOne())
	fmt.Printf("   %d requests recorded; %s\n", len(router.Traffic()), census(ctx, prod.URL))

	head("5 · Maya changes the intent. Not the code")
	say(conversation2)
	spec2 := spec("bookings-2.json")
	_, err = g.Decide(decision.Decision{ID: "intent-2", Parents: []string{"intent-1"}, Actor: "maya",
		Summary: "closed Sundays; no refund for late cancellations", Rationale: joinTurns(conversation2)})
	check(err)
	v2 := build(spec2)

	head("6 · What would change, on real traffic and real data")
	review, err := factory.ReviewCandidate(ctx, router, v2)
	check(err)
	fmt.Print(indent(review.String()))
	if !review.Clean() {
		head("7 · Maya does NOT sign off")
		fmt.Println("   Some changes aren't explained by anything Maya asked for. Nothing is released.")
		finish(store, keys, structureLines, modelLines)
		return
	}

	head("7 · Maya signs off on that reality; v2 is released as a canary")
	_, err = g.Decide(decision.Decision{ID: "signoff-2", Parents: []string{"intent-2"}, Actor: "maya",
		Summary: "release bookings " + v2.SpecHash, Rationale: "every behaviour change is explained by intent-2"})
	check(err)
	if res := canary("signoff-2", v2); res.Stopped {
		finish(store, keys, structureLines, modelLines)
		return
	}

	head("8 · Another week. The factory watches what v2 actually did")
	run(weekTwo())
	findings, err := factory.Observe(ctx, router)
	check(err)
	fmt.Printf("   compared every request since the release with what v1 would have done from the same data: %d finding(s)\n", len(findings))
	for _, f := range findings {
		fmt.Println("   •", f)
		p := factory.Propose(f, spec2)
		fmt.Println("     ?", p.Question)
		for i, o := range p.Options {
			fmt.Printf("       %d. %s\n", i+1, o.Label)
		}
	}
	if len(findings) > 0 {
		fmt.Println("   Maya keeps the rule: that's the intent working. (cmd/incidents follows answers through to the next version.)")
	}
	finish(store, keys, structureLines, modelLines)
}

func finish(store decision.Log, keys *decision.Keyring, structure, behaviours int) {
	fmt.Printf("\n━━━ What people did\n")
	fmt.Println("   Maya described a booking system in two short conversations and signed off on behaviour twice.")
	fmt.Printf("   lines of code written or edited by a person: 0   (generated structure: %d · written by the model from rules in words: %d; both kept only as a cache)\n", structure, behaviours)
	records, _ := store.Load()
	fmt.Printf("   decision ledger: %d signed records", len(records))
	if decision.VerifyChain(records, keys.PublicKeys()) == nil {
		fmt.Print(", chain verifies ✓")
	}
	fmt.Println()
	fmt.Println("\n   Toward a full SaaS (docs/SAAS-PLAN.md): this is phase 1, logic stated in words. Still to come: a real")
	fmt.Println("   database, a web front end, accounts and tenants, jobs and email, payments, one pod per version.")
}

// weekOne: customers booking, clashing, cancelling; some on Sundays.
func weekOne() []factory.HTTPCall {
	out := []factory.HTTPCall{
		{Method: "POST", Path: "/services", Body: map[string]any{"name": "haircut", "minutes": 60, "price": 40}, Caller: "owner"},
		{Method: "POST", Path: "/services", Body: map[string]any{"name": "colour", "minutes": 120, "price": 90}, Caller: "owner"},
		{Method: "POST", Path: "/services", Body: map[string]any{"name": "trim", "minutes": 30, "price": 20}, Caller: "owner"},
	}
	book := func(id, svc, who, date, at string) factory.HTTPCall {
		return factory.HTTPCall{Method: "POST", Path: "/bookings", Body: map[string]any{"id": id, "service": svc, "customer": who, "date": date, "time": at}, Caller: who}
	}
	cancel := func(id, who, at string) factory.HTTPCall {
		return factory.HTTPCall{Method: "POST", Path: "/bookings/" + id + "/cancel", Body: map[string]any{"at": at}, Caller: who}
	}
	out = append(out,
		book("bk-101", "haircut", "ana", "2026-06-15", "09:00"),
		book("bk-102", "colour", "ben", "2026-06-15", "10:00"),
		book("bk-103", "trim", "cy", "2026-06-15", "10:30"), // clashes with bk-102
		book("bk-103", "trim", "cy", "2026-06-15", "12:00"),
		book("bk-104", "haircut", "di", "2026-06-20", "11:00"), // Saturday: +25%
		book("bk-105", "colour", "eve", "2026-06-21", "13:00"), // Sunday
		book("bk-106", "trim", "fay", "2026-06-21", "15:00"),   // Sunday
		book("bk-107", "colour", "gus", "2026-06-16", "16:00"), // past closing
		book("bk-107", "colour", "gus", "2026-06-16", "14:00"),
		factory.HTTPCall{Method: "PATCH", Path: "/bookings/bk-101", Body: map[string]any{"time": "13:00"}, Caller: "ana"},
		cancel("bk-102", "ben", "2026-06-12T09:00"),             // early
		cancel("bk-104", "di", "2026-06-20T08:00"),              // late
		book("bk-108", "haircut", "hal", "2026-06-15", "10:00"), // bk-102's freed slot
		factory.HTTPCall{Method: "GET", Path: "/bookings?date=2026-06-15", Caller: "owner"},
		factory.HTTPCall{Method: "PATCH", Path: "/services/trim", Body: map[string]any{"price": 25}, Caller: "owner"},
	)
	return out
}

// browsing is most of the traffic: customers looking at the menu and the diary.
func browsing(n, offset int) []factory.HTTPCall {
	paths := []string{"/services", "/bookings?date=2026-06-15", "/bookings/bk-101", "/services/colour", "/bookings?status=confirmed"}
	var out []factory.HTTPCall
	for i := offset; i < offset+n; i++ {
		out = append(out, factory.HTTPCall{Method: "GET", Path: paths[i%len(paths)], Caller: "web"})
	}
	return out
}

// weekTwo: old habits meet the new intent.
func weekTwo() []factory.HTTPCall {
	return []factory.HTTPCall{
		{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-201", "service": "haircut", "customer": "ivy", "date": "2026-06-28", "time": "10:00"}, Caller: "ivy"}, // Sunday
		{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-202", "service": "trim", "customer": "jo", "date": "2026-06-27", "time": "10:00"}, Caller: "jo"},      // Saturday
		{Method: "POST", Path: "/bookings", Body: map[string]any{"id": "bk-203", "service": "colour", "customer": "ivy", "date": "2026-06-28", "time": "13:00"}, Caller: "ivy"},  // Sunday again
		{Method: "POST", Path: "/bookings/bk-202/cancel", Body: map[string]any{"at": "2026-06-27T07:00"}, Caller: "jo"},                                                          // late: no refund now
		{Method: "GET", Path: "/bookings?status=confirmed", Caller: "owner"},
	}
}

func census(ctx context.Context, url string) string {
	r, _ := factory.Call(ctx, url, factory.HTTPCall{Method: "GET", Path: "/bookings"})
	list, _ := r.Body.([]any)
	by := map[string]int{}
	revenue := 0.0
	for _, x := range list {
		m := x.(map[string]any)
		by[fmt.Sprint(m["status"])]++
		if m["status"] == "confirmed" {
			p, _ := m["price"].(float64)
			revenue += p
		}
	}
	var parts []string
	for k, n := range by {
		parts = append(parts, fmt.Sprintf("%d %s", n, k))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d bookings (%s), $%.2f confirmed", len(list), strings.Join(parts, ", "), revenue)
}

func countStructural(v *factory.Version) int {
	n := 0
	for _, r := range v.Results {
		if r.Example.Behaviour == "" {
			n++
		}
	}
	return n
}

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

func lines(s string) int { return strings.Count(s, "\n") }

type fatal struct{ err error }

func joinTurns(ts []factory.Turn) string {
	var parts []string
	for _, t := range ts {
		parts = append(parts, t.Who+": "+t.Says)
	}
	return strings.Join(parts, " / ")
}

func indent(s string) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range ls {
		ls[i] = "   " + l
	}
	return strings.Join(ls, "\n") + "\n"
}
