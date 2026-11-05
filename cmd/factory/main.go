// Command factory builds, changes, releases and rewinds a real service
// from conversation alone. Nobody writes or edits code in this run.
//
//	go run ./cmd/factory                        # offline: deterministic coder
//	ANTHROPIC_API_KEY=... go run ./cmd/factory  # Claude drafts the spec and writes the code
//	go run ./cmd/factory -faulty-coder          # a coder with a migration bug: watch it get caught
package main

import (
	"context"
	"embed"
	"encoding/json"
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
	flag.Parse()
	ctx := context.Background()
	check := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	work, err := os.MkdirTemp("", "factory-demo-")
	check(err)
	defer os.RemoveAll(work)

	var coder factory.Coder = factory.Codegen{}
	var m model.Completer
	who := "offline: the spec is written alongside each conversation, and code is compiled from it by a deterministic generator (set ANTHROPIC_API_KEY for Claude to draft the spec and write the code)"
	if a, ok := model.FromEnv(); ok {
		m, coder = a, factory.ModelCoder{Model: a}
		who = "Claude drafts the spec from the conversation and writes the code"
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
		b, err := intents.ReadFile("intent/" + file)
		check(err)
		s, err := factory.ParseSpec(b)
		check(err)
		return s
	}
	totalGenerated := 0

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
	fmt.Println("   replaying the week's traffic through both versions:")
	d, err := factory.Diff(ctx, router.Traffic(), v1, v2)
	check(err)
	fmt.Print(indent(indent(d.String())))
	fmt.Println("   reading every existing flag through both versions:")
	sd, err := factory.DiffState(ctx, router.DataPath, v1, v2)
	check(err)
	fmt.Print(indent(indent(sd.String())))

	unexplained := len(d.Unexpected()) + len(sd.Unexpected())
	if unexplained > 0 {
		head("7 · Phil does NOT sign off")
		fmt.Printf("   %d change(s) aren't explained by anything Phil asked for. Nothing is released.\n", unexplained)
		fmt.Println("   The examples all passed; only checking the change against production data caught it.")
		finish(g, store, keys, totalGenerated, router, "wrote two short conversations, answered the spec's questions, signed off once, and refused a release whose effect on existing data nobody asked for")
		return
	}

	head("7 · Phil signs off on that reality; v2 goes live")
	_, err = g.Decide(decision.Decision{ID: "signoff-2", Parents: []string{"intent-2"}, Actor: "phil",
		Summary: "release flags " + v2.SpecHash, Rationale: fmt.Sprintf("%d behaviour changes, all explained by intent-2", len(d.Changes)+len(sd.Changes))})
	check(err)
	_, err = g.Execute(ctx, rt, "signoff-2", reversible.Intent{ID: "release-" + v2.SpecHash, Effects: []reversible.Effect{router.Release(v2)}})
	check(err)
	sample := firstEnabled(ctx, prod.URL)
	fmt.Printf("   live: %s. %s\n", router.Live().SpecHash, sample)
	r, _ := factory.Call(ctx, prod.URL, factory.HTTPCall{Method: "DELETE", Path: "/flags/" + strings.Fields(sample)[0]})
	fmt.Printf("   deleting it now: %d %s\n", r.Status, compact(r.Body))

	// ── 5. Rewind ─────────────────────────────────────────────────────────
	head("8 · A team's cleanup script breaks on the new delete rule. Phil rewinds the decision")
	rep, err := g.Rewind(ctx, "intent-2", rt, "cleanup scripts depend on deleting enabled flags; revisit with that team", "phil")
	check(err)
	fmt.Printf("   rewound %s; live: %s (v1 again, same data)\n", strings.Join(rep.Rewound, " ← "), router.Live().SpecHash)
	r, _ = factory.Call(ctx, prod.URL, factory.HTTPCall{Method: "GET", Path: "/flags"})
	fmt.Printf("   flags still in production: %d\n", len(r.Body.([]any)))

	head("9 · Later, the intent is agreed again. Same intent, same software: nothing is regenerated")
	_, err = g.Decide(decision.Decision{ID: "intent-2b", Parents: []string{"intent-1"}, Actor: "phil",
		Summary: "gradual rollout; no deleting flags that are on (agreed with the cleanup-script team)", Rationale: "same intent as intent-2, after the cleanup scripts were updated"})
	check(err)
	again, err := fab.Build(ctx, spec2)
	check(err)
	fmt.Print(indent(again.Report()))

	finish(g, store, keys, totalGenerated, router, "wrote two short conversations, answered the spec's questions, signed off on behaviour twice, rewound one decision")
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
		if why, err := g.ExplainIntent("release-" + live.SpecHash); err == nil {
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
		out = append(out, factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": n, "owner": teams[i%len(teams)]}})
	}
	out = append(out,
		factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": "Holiday_Theme", "owner": "web"}},
		factory.HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": "dark-mode", "owner": "mobile"}},
	)
	for _, n := range []string{"dark-mode", "one-click-pay", "fast-login", "smart-sort"} {
		out = append(out, factory.HTTPCall{Method: "PATCH", Path: "/flags/" + n, Body: map[string]any{"enabled": true}})
	}
	out = append(out,
		factory.HTTPCall{Method: "GET", Path: "/flags/dark-mode"},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/beta-banner"},
		factory.HTTPCall{Method: "PATCH", Path: "/flags/smart-sort", Body: map[string]any{"enabled": false}},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/smart-sort"},
		factory.HTTPCall{Method: "DELETE", Path: "/flags/fast-login"},
		factory.HTTPCall{Method: "GET", Path: "/flags"},
	)
	return out
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

// forgetful wraps a coder and introduces a classic migration bug: records
// loaded from existing data don't get the new fields' defaults.
type forgetful struct{ factory.Coder }

func (f forgetful) Code(ctx context.Context, s factory.ServiceSpec, prev map[string]string, fail []factory.Result) (map[string]string, error) {
	files, err := f.Coder.Code(ctx, s, prev, fail)
	if err == nil {
		files["main.go"] = strings.Replace(files["main.go"], "r := defaults() // records written by an older version get today's defaults", "var r Flag", 1)
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
