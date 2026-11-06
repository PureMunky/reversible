package factory

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The incident tracker in cmd/incidents is the multi-resource fixture: two
// resources, a reference, filters, a lifecycle and access rules.
func incidentSpec(t *testing.T, n string) ServiceSpec {
	t.Helper()
	b, err := os.ReadFile("../cmd/incidents/intent/incidents-" + n + ".json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ParseSpec(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMultiResourceSpecsAreReadyAndGenerate(t *testing.T) {
	for _, n := range []string{"1", "2"} {
		s := incidentSpec(t, n)
		if qs := s.Check(); len(qs) > 0 {
			t.Fatalf("incidents-%s: unexpected questions %v", n, qs)
		}
		v, err := (&Factory{Root: t.TempDir(), Coder: Codegen{}}).Build(context.Background(), s)
		if err != nil {
			t.Fatalf("incidents-%s: %v", n, err)
		}
		if len(v.Results) != len(s.Examples) {
			t.Fatalf("incidents-%s: %d results for %d examples", n, len(v.Results), len(s.Examples))
		}
	}
}

func TestCheckCatchesBadRelationsAndLifecycles(t *testing.T) {
	cases := map[string]struct {
		edit func(*ServiceSpec)
		want string
	}{
		"unknown reference": {func(s *ServiceSpec) { s.Resources[1].Fields[1].References = "team" }, "team"},
		"transition without enum": {func(s *ServiceSpec) {
			s.Resources[1].Fields[4].Transitions = map[string][]string{"a": {"b"}}
		}, "summary"},
		"transition to unknown state": {func(s *ServiceSpec) {
			s.Resources[1].Fields[3].Transitions = map[string][]string{"open": {"closed"}}
		}, "closed"},
		"duplicate resource": {func(s *ServiceSpec) { s.Resources[1].Name, s.Resources[1].Plural = "service", "services" }, "service"},
		"both forms":         {func(s *ServiceSpec) { r := s.Resources[0]; s.Resource = &r }, "resource"},
		"filter on unfilterable field, expecting success": {func(s *ServiceSpec) {
			s.Examples = append(s.Examples, Example{Name: "x", Request: HTTPCall{Method: "GET", Path: "/incidents?summary=x"}, Expect: Expectancy{Status: 200}})
		}, "filterable"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := clone(incidentSpec(t, "1"))
			c.edit(&s)
			var blocking []string
			for _, q := range s.Check() {
				if q.Severity == Blocking {
					blocking = append(blocking, q.String())
				}
			}
			if len(blocking) == 0 || !strings.Contains(strings.Join(blocking, "\n"), c.want) {
				t.Fatalf("want a blocking question mentioning %q, got %v", c.want, blocking)
			}
		})
	}
}

func TestCheckAsksForRefusalExamples(t *testing.T) {
	s := clone(incidentSpec(t, "2"))
	var kept []Example
	for _, e := range s.Examples {
		if e.Expect.Status != 403 && e.Expect.Status != 409 {
			kept = append(kept, e)
		}
	}
	s.Examples = kept
	all := ""
	for _, q := range s.Check() {
		all += q.String() + "\n"
	}
	for _, want := range []string{"refused a change to services", "refused a change to incidents", "incident.status refusing", "delete being refused"} {
		if !strings.Contains(all, want) {
			t.Errorf("no question about %q in:\n%s", want, all)
		}
	}
}

func TestMixedCheckAcrossResources(t *testing.T) {
	ctx := context.Background()
	f := &Factory{Root: t.TempDir(), Coder: Codegen{}}
	v1, err := f.Build(ctx, incidentSpec(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := f.Build(ctx, incidentSpec(t, "2"))
	if err != nil {
		t.Fatal(err)
	}
	problems, err := MixedCheck(ctx, v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) > 0 {
		t.Fatalf("generated versions should share data: %v", problems)
	}
}

func TestProposeForAccessAndLifecycleRefusals(t *testing.T) {
	live := incidentSpec(t, "2")
	access := Finding{Kind: NewRefusal, Caller: "status-page-bot", Count: 2, Since: "v2", Because: "new access rule: only on-call may change incidents",
		Requests: []Exchange{{Request: HTTPCall{Method: "PATCH", Path: "/incidents/inc-7", Body: map[string]any{"summary": "s"}, Caller: "status-page-bot"},
			Response: Response{Status: 403, Body: map[string]any{"error": "status-page-bot may not change incidents"}}}},
		Previous: []Response{{Status: 200}}}
	p := Propose(access, live)
	labels := optionLabels(p)
	if !strings.Contains(labels, "let status-page-bot change incidents") || !strings.Contains(labels, "let anyone change incidents") {
		t.Fatalf("access options: %s", labels)
	}
	for _, o := range p.Options {
		if qs := blockingOf(o.Spec); len(qs) > 0 {
			t.Fatalf("option %q isn't buildable: %v", o.Label, qs)
		}
	}
	grant := p.Options[1].Spec
	if w := grant.Named("incident").Writers; len(w) != 2 || w[1] != "status-page-bot" {
		t.Fatalf("writers after granting: %v", w)
	}
	if live.Named("incident").Writers[0] != "on-call" || len(live.Named("incident").Writers) != 1 {
		t.Fatal("Propose changed the live spec")
	}
	v, err := (&Factory{Root: t.TempDir(), Coder: Codegen{}}).Build(context.Background(), grant)
	if err != nil {
		t.Fatalf("granting option doesn't build: %v", err)
	}
	_ = v

	lifecycle := Finding{Kind: NewRefusal, Caller: "on-call", Count: 1, Since: "v2", Because: "rules for incident.status changed",
		Requests: []Exchange{{Request: HTTPCall{Method: "PATCH", Path: "/incidents/inc-9", Body: map[string]any{"status": "resolved"}, Caller: "on-call"},
			Response: Response{Status: 409, Body: map[string]any{"error": "status can't change from acknowledged to resolved (why)"}}}},
		Previous: []Response{{Status: 200}}}
	p = Propose(lifecycle, live)
	if !strings.Contains(optionLabels(p), "allow status to go from acknowledged to resolved") {
		t.Fatalf("lifecycle options: %s", optionLabels(p))
	}
	for _, o := range p.Options {
		if _, err := (&Factory{Root: t.TempDir(), Coder: Codegen{}}).Build(context.Background(), o.Spec); err != nil {
			t.Fatalf("option %q doesn't build: %v", o.Label, err)
		}
	}
}

func optionLabels(p Proposal) string {
	var out []string
	for _, o := range p.Options {
		out = append(out, o.Label)
	}
	return strings.Join(out, " | ")
}

func blockingOf(s ServiceSpec) []Question {
	var out []Question
	for _, q := range s.Check() {
		if q.Severity == Blocking {
			out = append(out, q)
		}
	}
	return out
}

func TestRouterForwardsFilters(t *testing.T) {
	ctx := context.Background()
	v, err := (&Factory{Root: t.TempDir(), Coder: Codegen{}}).Build(ctx, incidentSpec(t, "1"))
	if err != nil {
		t.Fatal(err)
	}
	router := &Router{DataPath: t.TempDir() + "/data.json"}
	defer router.Stop()
	if _, err := router.Release(v).Do(ctx); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()
	for _, c := range []HTTPCall{
		{Method: "POST", Path: "/services", Body: map[string]any{"name": "a", "team": "t"}, Caller: "platform-team"},
		{Method: "POST", Path: "/incidents", Body: map[string]any{"id": "inc-1", "service": "a", "severity": "sev1", "summary": "x"}},
		{Method: "POST", Path: "/incidents", Body: map[string]any{"id": "inc-2", "service": "a", "severity": "sev2", "summary": "y"}},
	} {
		if r, err := Call(ctx, srv.URL, c); err != nil || r.Status != 201 {
			t.Fatalf("%v: %v %v", c, r, err)
		}
	}
	r, err := Call(ctx, srv.URL, HTTPCall{Method: "GET", Path: "/incidents?severity=sev2"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Body.([]any); len(got) != 1 {
		t.Fatalf("filter through the router returned %v", r.Body)
	}
	if p := router.Traffic()[3].Request.Path; p != "/incidents?severity=sev2" {
		t.Fatalf("recorded path %q: replay would lose the filter", p)
	}
}

func TestGeneratedCodeDoesNotSeeSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "also-secret")
	for name, env := range map[string][]string{"build": buildEnv(), "service": serviceEnv()} {
		for _, kv := range env {
			if strings.Contains(kv, "secret") {
				t.Errorf("%s environment carries %q", name, kv)
			}
		}
	}
}
