// Command factory builds, changes, releases and rewinds a real service
// from conversation alone. Nobody writes or edits code in this run.
//
//	go run ./cmd/factory                        # offline: deterministic coder
//	ANTHROPIC_API_KEY=... go run ./cmd/factory  # Claude drafts the spec and writes the code
//	go run ./cmd/factory -faulty-coder          # a coder with a migration bug: watch it get caught
//	go run ./cmd/factory -brittle-coder         # a defect only real traffic shows: watch the canary stop it
//	go run ./cmd/factory -show-prompts          # print every prompt a model would get, calling nothing
//	go run ./cmd/factory -keep                  # keep the generated code and data to read afterwards
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/factory"
	"github.com/PureMunky/reversible/model"
)

//go:embed intent/*.json
var intents embed.FS

var conversation1 = []factory.Turn{
	{Who: "phil", Says: "The platform team needs a feature-flag service, so we can ship code dark and turn things on separately from deploys."},
	{Who: "phil", Says: "A flag has a key, an owner, a description, and whether it's on. Flags start off. Keys show up in code and URLs, so lowercase with dashes."},
	{Who: "phil", Says: "Every flag needs an owner, and two flags can't share a key."},
}

var conversation2 = []factory.Turn{
	{Who: "phil", Says: "Teams want gradual rollouts: on for 10% of traffic, then 50, then everyone. Add a rollout percentage; existing flags should stay at 100%."},
	{Who: "phil", Says: "And people keep deleting flags that are still on, which breaks whatever was reading them. Don't allow deleting a flag while it's on; make them turn it off first."},
}

func main() {
	faulty := flag.Bool("faulty-coder", false, "use a coder that forgets defaults when loading old data")
	showPrompts := flag.Bool("show-prompts", false, "print exactly what a model would be sent at each step, without calling one")
	brittleCoder := flag.Bool("brittle-coder", false, "use a coder whose later builds fail on real traffic in a way no example or review shows: watch the canary stop it")
	keep := flag.Bool("keep", false, "keep the work directory (generated code, builds, production data) and print where it is")
	flag.Parse()
	ctx := context.Background()
	// Errors unwind through the deferred cleanup (stopping every generated
	// service process) before exiting, rather than os.Exit skipping it.
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
	work, err := os.MkdirTemp("", "factory-demo-")
	check(err)
	if *keep {
		defer fmt.Printf("\nwork directory kept: %s\n  builds/<spec hash>/main.go   the generated code for each intent\n  production/flags.json*       production data, with a snapshot per release\n", work)
	} else {
		defer os.RemoveAll(work)
	}

	var coder factory.Coder = factory.Codegen{}
	var m model.Completer
	who := "offline: the spec is written alongside each conversation, and code is compiled from it by a deterministic generator (set ANTHROPIC_API_KEY for Claude to draft the spec and write the code)"
	if a, ok := model.FromEnv(); ok {
		m, coder = a, factory.ModelCoder{Model: a}
		who = "Claude drafts the spec from the conversation and writes the code"
	}
	var shower model.Completer
	if *showPrompts && m == nil {
		seen := map[string]bool{}
		shower = model.Func(func(_ context.Context, system, prompt string) (string, error) {
			if !seen[system+prompt] {
				seen[system+prompt] = true
				fmt.Printf("\n   ┌── what the model would be sent (not sent: -show-prompts) ──\n")
				for _, l := range strings.Split("SYSTEM: "+system+"\n\n"+prompt, "\n") {
					fmt.Println("   │ " + l)
				}
				fmt.Printf("   └──\n\n")
			}
			return "", errors.New("show-prompts: no model is called")
		})
		coder = showing{shower: shower, inner: coder}
		who += "; -show-prompts: printing every model prompt, calling nothing"
	}
	if *brittleCoder {
		coder = &brittle{inner: coder}
		who += "; -brittle-coder: builds after the first fail reads of flags that have a description"
	}
	if *faulty {
		coder = forgetful{coder}
		who += "; -faulty-coder: the coder forgets defaults when loading existing data"
	}
	fab := &factory.Factory{Root: filepath.Join(work, "builds"), Coder: coder}

	keys := decision.NewKeyring()
	for _, a := range []string{"phil", "factory"} {
		_, err := keys.Generate(a)
		check(err)
	}
	store := decision.NewMemoryLog()
	chain, err := decision.NewChainLog(store, keys)
	check(err)
	g, err := decision.Open(chain)
	check(err)

	router := &factory.Router{DataPath: filepath.Join(work, "production", "flags.json")}
	check(os.MkdirAll(filepath.Dir(router.DataPath), 0o755))
	defer router.Stop()
	prod := httptest.NewServer(router)
	defer prod.Close()
	rt := reversible.New()

	head := func(s string) { fmt.Printf("\n━━━ %s\n", s) }
	say := func(turns []factory.Turn) {
		for _, t := range turns {
			fmt.Printf("   %s: %s\n", t.Who, t.Says)
		}
	}
	capture := func(turns []factory.Turn, current *factory.ServiceSpec, file string) factory.ServiceSpec {
		if m != nil {
			c, err := factory.Capture(ctx, m, turns, current)
			check(err)
			for _, q := range c.Questions {
				fmt.Println("   ? (from Claude)", q)
			}
			return c.Spec
		}
		if shower != nil {
			factory.Capture(ctx, shower, turns, current) // prints the prompt; the offline spec is used
		}
		b, err := intents.ReadFile("intent/" + file)
		check(err)
		s, err := factory.ParseSpec(b)
		check(err)
		return s
	}
	totalGenerated := 0
	reads := 0
	// canaryRelease runs v beside the live version on a fifth of real
	// traffic until the evolution runtime promotes or stops it.
	canaryRelease := func(decisionID string, v *factory.Version) factory.CanaryResult {
		live := router.Live().SpecHash
		c, err := factory.StartCanary(ctx, router, v, g, rt, decisionID, factory.CanaryOptions{
			Share: 0.2, MinSamples: 30, MaxSamples: 80, Salt: "factory-demo-" + v.SpecHash})
		check(err)
		fmt.Printf("   canary: %s takes 20%% of traffic beside %s, on the same data\n", v.SpecHash, live)
		var res factory.CanaryResult
		for round := 1; round <= 20; round++ {
			for _, call := range appReads(200, reads) {
				_, err := factory.Call(ctx, prod.URL, call)
				check(err)
			}
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

	fmt.Println("Software built from intent. Nobody writes or edits code in this run.")
	fmt.Println("mode:", who)

	// ── 1. Intent ─────────────────────────────────────────────────────────
	head("1 · Phil describes what he wants")
	say(conversation1)
	_, err = g.Goal("flags", "a feature-flag service for the platform team", "phil")
	check(err)
	draft := capture(conversation1, nil, "flags-draft.json")
	if m == nil {
		fmt.Println("\n   The first draft of the spec is checked before anything is built:")
		for _, q := range draft.Check() {
			fmt.Println("   ?", q)
		}
		fmt.Println("   Phil answers: key is required and lowercase-with-dashes; owner is phil; here's how list and delete should behave.")
	}
	spec1 := capture(conversation1, nil, "flags-1.json")
	_, err = g.Decide(decision.Decision{ID: "intent-1", Parents: []string{"flags"}, Actor: "phil",
		Summary: "feature flags: key, owner, on/off", Rationale: joinTurns(conversation1)})
	check(err)

	// ── 2. Build ──────────────────────────────────────────────────────────
	head("2 · The factory generates, builds and verifies the service")
	v1, err := fab.Build(ctx, spec1)
	if v1 != nil {
		fmt.Print(indent(v1.Report()))
	}
	check(err)
	totalGenerated += v1.Lines()
	fmt.Printf("   generated main.go begins: %s\n", firstLine(v1.Files["main.go"]))

	head("3 · Phil signs off on the behaviour; it goes live")
	for _, r := range v1.Results {
		fmt.Printf("   ✓ %s\n", r.Example.Name)
	}
	_, err = g.Decide(decision.Decision{ID: "signoff-1", Parents: []string{"intent-1"}, Actor: "phil",
		Summary: "release flags " + v1.SpecHash, Rationale: "all examples pass; first release"})
	check(err)
	_, err = g.Execute(ctx, rt, "signoff-1", reversible.Intent{ID: "release-" + v1.SpecHash, Effects: []reversible.Effect{router.Release(v1)}})
	check(err)
	fmt.Printf("   live: %s\n", router.Live().SpecHash)

	// ── 3. Production ─────────────────────────────────────────────────────
	head("4 · The platform team uses it for a week")
	for _, c := range teamWeek() {
		_, err := factory.Call(ctx, prod.URL, c)
		check(err)
	}
	all, _ := factory.Call(ctx, prod.URL, factory.HTTPCall{Method: "GET", Path: "/flags"})
	on := 0
	for _, f := range all.Body.([]any) {
		if f.(map[string]any)["enabled"] == true {
			on++
		}
	}
	fmt.Printf("   %d requests recorded; %d flags in production, %d of them on\n", len(router.Traffic()), len(all.Body.([]any)), on)

	// ── 4. Changing intent ────────────────────────────────────────────────
	head("5 · Phil changes the intent. Not the code")
	say(conversation2)
	spec2 := capture(conversation2, &spec1, "flags-2.json")
	_, err = g.Decide(decision.Decision{ID: "intent-2", Parents: []string{"intent-1"}, Actor: "phil",
		Summary: "gradual rollout; no deleting flags that are on", Rationale: joinTurns(conversation2)})
	check(err)
	v2, err := fab.Build(ctx, spec2)
	if v2 != nil {
		fmt.Print(indent(v2.Report()))
	}
	check(err)
	totalGenerated += v2.Lines()

	head("6 · What would change, on real traffic and real data")
	review, err := factory.ReviewCandidate(ctx, router, v2)
	check(err)
	fmt.Print(indent(review.String()))
	d, sd := review.Replay, review.State

	if !review.Clean() {
		head("7 · Phil does NOT sign off")
		fmt.Printf("   %d change(s) aren't explained by anything Phil asked for. Nothing is released.\n", len(d.Unexpected())+len(sd.Unexpected())+len(review.Mixed))
		fmt.Println("   The examples all passed; only checking the change against production data caught it.")
		finish(g, store, keys, totalGenerated, router, "wrote two short conversations, answered the spec's questions, signed off once, and refused a release whose effect on existing data nobody asked for")
		return
	}

	head("7 · Phil signs off on that reality; v2 is released as a canary, and outcomes decide")
	_, err = g.Decide(decision.Decision{ID: "signoff-2", Parents: []string{"intent-2"}, Actor: "phil",
		Summary: "release flags " + v2.SpecHash, Rationale: fmt.Sprintf("%d behaviour changes, all explained by intent-2", len(d.Changes)+len(sd.Changes))})
	check(err)
	if res := canaryRelease("signoff-2", v2); res.Stopped {
		errs, total := 0, 0
		for _, ex := range router.Traffic() {
			total++
			if ex.Response.Status >= 500 {
				errs++
			}
		}
		fmt.Printf("   The build passed all its examples and the full review; real traffic found the defect.\n")
		fmt.Printf("   %d of %d requests ever failed, all on the canary's share; the decision behind it is rewound and v1 serves everything.\n", errs, total)
		finish(g, store, keys, totalGenerated, router, "wrote two short conversations, answered the spec's questions, signed off once; the canary and its evidence stopped a bad release without anyone watching")
		return
	}
	sample := firstEnabled(ctx, prod.URL)
	fmt.Printf("   %s\n", sample)
	r, _ := factory.Call(ctx, prod.URL, factory.HTTPCall{Method: "DELETE", Path: "/flags/" + strings.Fields(sample)[0], Caller: "web"})
	fmt.Printf("   deleting it now: %d %s\n", r.Status, compact(r.Body))

	// ── 5. Production is an input ─────────────────────────────────────────
	head("8 · Another week in production. The factory watches what v2 actually did")
	for _, c := range weekTwo() {
		_, err := factory.Call(ctx, prod.URL, c)
		check(err)
	}
	findings, err := factory.Observe(ctx, router)
	check(err)
	fmt.Printf("   compared every request since the release with what v1 would have done from the same data: %d finding(s)\n", len(findings))
	for _, f := range findings {
		fmt.Println("   •", f)
	}

	head("9 · Each finding becomes a question for the owner, with intent changes as answers")
	answers := map[string]string{"cleanup-bot": "exempt", "web": "keep"}
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
		fmt.Printf("     Phil answers %d.\n", choice+1)
		asked = append(asked, fmt.Sprintf("%s → %s", prop.Question, prop.Options[choice].Label))
		spec3 = prop.Options[choice].Spec
		_, err = g.Observe(decision.Outcome{ID: fmt.Sprintf("finding-%d", len(asked)), Parents: []string{"signoff-2"}, Actor: "factory",
			Summary: f.String(), Metrics: map[string]float64{"requests": float64(f.Count)}})
		check(err)
	}
	_, err = g.Decide(decision.Decision{ID: "intent-3", Parents: []string{"intent-2"}, Actor: "phil",
		Summary: "answers to production's questions", Rationale: strings.Join(asked, " / ")})
	check(err)

	head("10 · The answers are the new intent. The factory regenerates")
	v3, err := fab.Build(ctx, spec3)
	if v3 != nil {
		fmt.Print(indent(v3.Report()))
	}
	check(err)
	totalGenerated += v3.Lines()
	fmt.Printf("   %d of its %d examples were captured from production requests\n", len(spec3.Examples)-len(spec2.Examples), len(spec3.Examples))
	d3, err := factory.Diff(ctx, router.Traffic(), v2, v3)
	check(err)
	fmt.Print(indent(d3.String()))
	if len(d3.Unexpected()) > 0 {
		check(fmt.Errorf("unexplained changes in v3"))
	}
	_, err = g.Decide(decision.Decision{ID: "signoff-3", Parents: []string{"intent-3"}, Actor: "phil",
		Summary: "release flags " + v3.SpecHash, Rationale: "behaviour changes match the answers"})
	check(err)
	canaryRelease("signoff-3", v3)
	for _, c := range weekThree() {
		_, err := factory.Call(ctx, prod.URL, c)
		check(err)
	}
	after, err := factory.Observe(ctx, router)
	check(err)
	if len(after) == 0 {
		fmt.Println("   A week later: no new findings; the cleanup bot's deletes work, everyone else still turns flags off first.")
	} else {
		for _, f := range after {
			fmt.Println("   •", f)
		}
	}

	// ── 6. Every decision is reversible ───────────────────────────────────
	head("11 · Every decision stays reversible. Phil rewinds his answer, then makes it again")
	rep, err := g.Rewind(ctx, "intent-3", rt, "checking the rollback path", "phil")
	check(err)
	fmt.Printf("   rewound %s; live: %s (v2 again, same data)\n", strings.Join(rep.Rewound, " ← "), router.Live().SpecHash)
	_, err = g.Decide(decision.Decision{ID: "intent-3b", Parents: []string{"intent-2"}, Actor: "phil",
		Summary: "answers to production's questions, reaffirmed", Rationale: "same answers as intent-3"})
	check(err)
	again, err := fab.Build(ctx, spec3)
	check(err)
	fmt.Print(indent("same intent, same software: " + again.Report()))
	_, err = g.Execute(ctx, rt, "intent-3b", reversible.Intent{ID: "release-" + again.SpecHash + "-again", Effects: []reversible.Effect{router.Release(again)}})
	check(err)
	fmt.Printf("   live: %s\n", router.Live().SpecHash)

	finish(g, store, keys, totalGenerated, router, "wrote two short conversations, answered the spec's questions and production's, signed off on behaviour three times, rewound one decision (canaries and evidence decided the releases)")
}

func finish(g *decision.Graph, store decision.Log, keys *decision.Keyring, generated int, router *factory.Router, did string) {
	fmt.Printf("\n━━━ What people did\n")
	fmt.Println("   " + did)
	fmt.Printf("   lines of code written or edited by a person: 0   (generated: %d, kept only as a cache)\n", generated)
	records, _ := store.Load()
	ok := decision.VerifyChain(records, keys.PublicKeys()) == nil
	fmt.Printf("   decision ledger: %d signed records", len(records))
	if ok {
		fmt.Print(", chain verifies ✓")
	}
	fmt.Println()
	if live := router.Live(); live != nil {
		var why string
		var err error
		for _, id := range []string{"release-" + live.SpecHash + "-again", "promote-" + live.SpecHash, "release-" + live.SpecHash} {
			if why, err = g.ExplainIntent(id); err == nil {
				break
			}
		}
		if err == nil {
			fmt.Println("\n   why is this version live?")
			fmt.Print(indent(indent(why)))
		}
	}
}

// teamWeek is a week of the platform team using the service.
func teamWeek() []factory.HTTPCall {
	var out []factory.HTTPCall
	teams := []string{"web", "search", "payments", "mobile"}
	names := []string{"dark-mode", "new-search", "one-click-pay", "offline-sync", "beta-banner", "smart-sort", "fast-login", "push-v2"}
	for i, n := range names {
		t := teams[i%len(teams)]
		out = append(out, factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": n, "owner": t}, Caller: t})
	}
	out = append(out,
		factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": "Holiday_Theme", "owner": "web"}, Caller: "web"},
		factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": "dark-mode", "owner": "mobile"}, Caller: "mobile"},
	)
	for _, n := range []string{"dark-mode", "one-click-pay", "fast-login", "smart-sort"} {
		out = append(out, factory.HTTPCall{Method: "PATCH", Path: "/flags/" + n, Body: map[string]any{"enabled": true}, Caller: "web"})
	}
	out = append(out,
		factory.HTTPCall{Method: "GET", Path: "/flags/dark-mode", Caller: "mobile"},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/beta-banner", Caller: "web"},
		factory.HTTPCall{Method: "PATCH", Path: "/flags/smart-sort", Body: map[string]any{"enabled": false}, Caller: "search"},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/smart-sort", Caller: "search"},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/fast-login", Caller: "cleanup-bot"},
		factory.HTTPCall{Method: "GET", Path: "/flags", Caller: "web"},
	)
	return out
}

// appReads is most of a flag service's traffic: app servers reading flags,
// with the occasional note from a person.
func appReads(n, offset int) []factory.HTTPCall {
	keys := []string{"dark-mode", "new-search", "one-click-pay", "offline-sync", "push-v2"}
	var out []factory.HTTPCall
	for i := offset; i < offset+n; i++ {
		k := keys[i%len(keys)]
		if i%25 == 0 {
			out = append(out, factory.HTTPCall{Method: "PATCH", Path: "/flags/" + k, Body: map[string]any{"description": fmt.Sprint("reviewed ", i)}, Caller: "web"})
			continue
		}
		out = append(out, factory.HTTPCall{Method: "GET", Path: "/flags/" + k, Caller: "app-servers"})
	}
	return out
}

// weekTwo: teams adopt rollouts; the nightly cleanup bot deletes stale
// flags, some of which are still on; one person tries to delete a live one.
func weekTwo() []factory.HTTPCall {
	return []factory.HTTPCall{
		{Method: "POST", Path: "/flags", Body: map[string]any{"key": "ai-summaries", "owner": "search"}, Caller: "search"},
		{Method: "PATCH", Path: "/flags/ai-summaries", Body: map[string]any{"enabled": true, "rollout_percent": 10}, Caller: "search"},
		{Method: "PATCH", Path: "/flags/ai-summaries", Body: map[string]any{"rollout_percent": 50}, Caller: "search"},
		{Method: "PATCH", Path: "/flags/push-v2", Body: map[string]any{"enabled": true, "rollout_percent": 25}, Caller: "mobile"},
		{Method: "DELETE", Path: "/flags/offline-sync", Caller: "cleanup-bot"},
		{Method: "DELETE", Path: "/flags/one-click-pay", Caller: "cleanup-bot"},
		{Method: "DELETE", Path: "/flags/ai-summaries", Caller: "cleanup-bot"},
		{Method: "DELETE", Path: "/flags/push-v2", Caller: "web"},
		{Method: "GET", Path: "/flags", Caller: "web"},
	}
}

// weekThree: the same habits, under the answered intent.
func weekThree() []factory.HTTPCall {
	return []factory.HTTPCall{
		{Method: "POST", Path: "/flags", Body: map[string]any{"key": "winter-theme", "owner": "web"}, Caller: "web"},
		{Method: "PATCH", Path: "/flags/winter-theme", Body: map[string]any{"enabled": true}, Caller: "web"},
		{Method: "DELETE", Path: "/flags/dark-mode", Caller: "cleanup-bot"},
		{Method: "DELETE", Path: "/flags/winter-theme", Caller: "web"},
		{Method: "PATCH", Path: "/flags/ai-summaries", Body: map[string]any{"rollout_percent": 100}, Caller: "search"},
		{Method: "GET", Path: "/flags", Caller: "web"},
	}
}

func firstEnabled(ctx context.Context, url string) string {
	r, _ := factory.Call(ctx, url, factory.HTTPCall{Method: "GET", Path: "/flags"})
	for _, f := range r.Body.([]any) {
		m := f.(map[string]any)
		if m["enabled"] == true {
			return fmt.Sprintf("%v (created under v1) reads as enabled=%v rollout_percent=%v", m["key"], m["enabled"], m["rollout_percent"])
		}
	}
	return ""
}

type fatal struct{ err error }

// showing prints the prompt a model coder would be sent, then lets the
// offline coder do the work.
type showing struct {
	shower model.Completer
	inner  factory.Coder
}

func (s showing) Code(ctx context.Context, spec factory.ServiceSpec, prev map[string]string, fail []factory.Result) (map[string]string, error) {
	factory.ModelCoder{Model: s.shower}.Code(ctx, spec, prev, fail)
	return s.inner.Code(ctx, spec, prev, fail)
}

// brittle wraps a coder: every build after the first fails reads of flags
// that have a description. No example reads such a flag, and replay and
// review see no descriptions yet, so only real traffic can find it.
type brittle struct {
	inner factory.Coder
	first string
}

func (b *brittle) Code(ctx context.Context, s factory.ServiceSpec, prev map[string]string, fail []factory.Result) (map[string]string, error) {
	files, err := b.inner.Code(ctx, s, prev, fail)
	if b.first == "" {
		b.first = s.Hash()
	}
	if err == nil && s.Hash() != b.first {
		files["main.go"] = strings.Replace(files["main.go"], "\t\treply(w, 200, rec)\n\t})\n\n\tmux.HandleFunc(\"PATCH",
			"\t\tif rec.Description != \"\" {\n\t\t\tfail(w, 500, \"internal error\")\n\t\t\treturn\n\t\t}\n\t\treply(w, 200, rec)\n\t})\n\n\tmux.HandleFunc(\"PATCH", 1)
	}
	return files, err
}

// forgetful wraps a coder and introduces a classic migration bug: records
// loaded from existing data don't get the new fields' defaults.
type forgetful struct{ factory.Coder }

func (f forgetful) Code(ctx context.Context, s factory.ServiceSpec, prev map[string]string, fail []factory.Result) (map[string]string, error) {
	files, err := f.Coder.Code(ctx, s, prev, fail)
	if err == nil {
		files["main.go"] = strings.Replace(files["main.go"], "r := defaultsFlag() // records written by an older version get today's defaults", "var r Flag", 1)
	}
	return files, err
}

func joinTurns(ts []factory.Turn) string {
	var parts []string
	for _, t := range ts {
		parts = append(parts, t.Who+": "+t.Says)
	}
	return strings.Join(parts, " / ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func compact(v any) string {
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "   " + l
	}
	return strings.Join(lines, "\n") + "\n"
}
