package factory

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
)

type prod struct {
	router *Router
	url    string
	rt     *reversible.Runtime
	close  func()
}

func newProd(t *testing.T) *prod {
	t.Helper()
	r := &Router{DataPath: filepath.Join(t.TempDir(), "flags.json")}
	srv := httptest.NewServer(r)
	return &prod{router: r, url: srv.URL, rt: reversible.New(), close: func() { srv.Close(); r.Stop() }}
}

func (p *prod) release(t *testing.T, id string, v *Version) {
	t.Helper()
	if _, err := p.rt.Execute(context.Background(), reversible.Intent{ID: id, Effects: []reversible.Effect{p.router.Release(v)}}); err != nil {
		t.Fatal(err)
	}
}

func (p *prod) do(t *testing.T, calls ...HTTPCall) []Response {
	t.Helper()
	var out []Response
	for _, c := range calls {
		r, err := Call(context.Background(), p.url, c)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func create(key, owner string) HTTPCall {
	return HTTPCall{Method: "POST", Path: "/flags", Body: map[string]any{"key": key, "owner": owner}, Caller: owner}
}
func enable(key string) HTTPCall {
	return HTTPCall{Method: "PATCH", Path: "/flags/" + key, Body: map[string]any{"enabled": true}, Caller: "web"}
}
func del(key, caller string) HTTPCall {
	return HTTPCall{Method: "DELETE", Path: "/flags/" + key, Caller: caller}
}

func TestProductionRaisesAQuestionAndTheAnswerBecomesIntent(t *testing.T) {
	ctx := context.Background()
	v1, v2 := build(t, spec(t, "flags-v1.json")), build(t, spec(t, "flags-v2.json"))
	p := newProd(t)
	defer p.close()

	p.release(t, "r1", v1)
	p.do(t, create("a", "web"), create("b", "web"), create("c", "web"), create("d", "web"), create("e", "web"),
		enable("a"), enable("b"), enable("c"))
	p.release(t, "r2", v2)

	// After the release: the cleanup bot deletes flags, some of them on;
	// the web team carries on as normal.
	p.do(t, del("a", "cleanup-bot"), del("b", "cleanup-bot"), del("d", "cleanup-bot"), del("c", "cleanup-bot"),
		create("f", "web"), enable("f"), HTTPCall{Method: "GET", Path: "/flags", Caller: "web"})

	fs, err := Observe(ctx, p.router)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 {
		t.Fatalf("findings: %v", fs)
	}
	f := fs[0]
	if f.Kind != NewRefusal || f.Caller != "cleanup-bot" || f.Count != 3 || !strings.Contains(f.Because, "new rule: no delete while enabled is true") {
		t.Fatalf("finding: %s", f)
	}

	prop := Propose(f, v2.Spec)
	if !strings.Contains(prop.Question, "cleanup-bot has had 3 request(s) refused that the previous version accepted") {
		t.Fatalf("question: %s", prop.Question)
	}
	labels := []string{}
	for _, o := range prop.Options {
		labels = append(labels, o.Label)
		v, err := (&Factory{Root: t.TempDir()}).Build(ctx, o.Spec)
		if err != nil {
			t.Fatalf("option %q doesn't build: %v\n%s", o.Label, err, v.Report())
		}
	}
	if strings.Join(labels, "|") != "keep refusing; cleanup-bot must change|exempt cleanup-bot from the rule|drop the rule for everyone" {
		t.Fatalf("options: %v", labels)
	}

	// The owner exempts the bot. The answer is new intent; production
	// gets a version where the bot's deletes work and everyone else's
	// are still refused.
	v3 := build(t, prop.Options[1].Spec)
	if v3.Spec.Hash() == v2.Spec.Hash() {
		t.Fatal("the answer didn't change the intent")
	}
	p.release(t, "r3", v3)
	got := p.do(t, enable("e"), del("e", "cleanup-bot"), del("f", "web"))
	if got[1].Status != 204 || got[2].Status != 409 {
		t.Fatalf("v3 behaviour: bot %d, web %d", got[1].Status, got[2].Status)
	}
	if fs, _ := Observe(ctx, p.router); len(fs) != 0 {
		t.Fatalf("v3 should raise nothing new: %v", fs)
	}
}

// crashy fails on one key: a defect production finds and examples didn't.
type crashy struct{}

func (crashy) Code(ctx context.Context, s ServiceSpec, prev map[string]string, f []Result) (map[string]string, error) {
	files, err := Codegen{}.Code(ctx, s, prev, f)
	if err == nil {
		files["main.go"] = strings.Replace(files["main.go"], `		rec, ok := storeFlag[r.PathValue("key")]`,
			`		if r.PathValue("key") == "boom" {
			fail(w, 500, "internal error")
			return
		}
		rec, ok := storeFlag[r.PathValue("key")]`, 1)
	}
	return files, err
}

func TestServerErrorsBecomeContract(t *testing.T) {
	ctx := context.Background()
	v1 := build(t, spec(t, "flags-v1.json"))
	s := spec(t, "flags-v1.json")
	s.Purpose += " "
	bad, err := (&Factory{Root: t.TempDir(), Coder: crashy{}}).Build(ctx, s)
	if err != nil {
		t.Fatalf("the defect is invisible to the examples: %v", err)
	}
	p := newProd(t)
	defer p.close()
	p.release(t, "r1", v1)
	p.do(t, create("boom", "web"))
	p.release(t, "r2", bad)
	p.do(t, HTTPCall{Method: "GET", Path: "/flags/boom", Caller: "mobile"})

	fs, err := Observe(ctx, p.router)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 1 || fs[0].Kind != ServerError || fs[0].Caller != "mobile" {
		t.Fatalf("findings: %v", fs)
	}
	prop := Propose(fs[0], bad.Spec)
	if len(prop.Options) != 1 || !strings.Contains(prop.Options[0].Label, "answer 200, as the previous version did") {
		t.Fatalf("proposal: %+v", prop)
	}
	// Regenerating from the answered intent with the same faulty coder now
	// fails, because the production request is part of the contract.
	if _, err := (&Factory{Root: t.TempDir(), Coder: crashy{}, MaxAttempts: 1}).Build(ctx, prop.Options[0].Spec); err == nil {
		t.Fatal("the captured production example should catch the defect")
	}
	if _, err := (&Factory{Root: t.TempDir()}).Build(ctx, prop.Options[0].Spec); err != nil {
		t.Fatalf("a correct coder should pass the new contract: %v", err)
	}
}

func TestNothingToObserveBeforeTraffic(t *testing.T) {
	p := newProd(t)
	defer p.close()
	if fs, err := Observe(context.Background(), p.router); err != nil || fs != nil {
		t.Fatalf("%v %v", fs, err)
	}
	p.release(t, "r1", build(t, spec(t, "flags-v1.json")))
	if fs, err := Observe(context.Background(), p.router); err != nil || fs != nil {
		t.Fatalf("%v %v", fs, err)
	}
}

func TestLooseningChangesAreExplained(t *testing.T) {
	ctx := context.Background()
	v2 := build(t, spec(t, "flags-v2.json"))
	f := Finding{Kind: NewRefusal, Caller: "cleanup-bot", Because: "new rule: no delete while enabled is true", Count: 1, Since: v2.SpecHash,
		Requests: []Exchange{{Request: del("x", "cleanup-bot"), Response: Response{Status: 409}}}, Previous: []Response{{Status: 204}}}
	prop := Propose(f, v2.Spec)
	traffic := []Exchange{
		{Request: create("x", "web")}, {Request: enable("x")},
		{Request: del("x", "cleanup-bot")}, // exempt under v3
		{Request: create("y", "web")}, {Request: enable("y")},
		{Request: del("y", "web")}, // still refused under the exemption; allowed when the rule is dropped
	}
	for i, want := range map[int][]string{
		1: {"cleanup-bot exempted from rule: no delete while enabled is true"},
		2: {"rule removed: no delete while enabled is true"},
	} {
		v := build(t, prop.Options[i].Spec)
		d, err := Diff(ctx, traffic, v2, v)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Unexpected()) != 0 || !strings.Contains(d.String(), want[0]) {
			t.Fatalf("option %q:\n%s", prop.Options[i].Label, d)
		}
	}
}
