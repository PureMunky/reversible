package intent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
)

var known = Known{
	Types: map[string]reversible.Class{
		"plan.set":      reversible.Reversible,
		"card.charge":   reversible.Escrowed,
		"email.send":    reversible.Escrowed,
		"account.purge": reversible.Irreversible,
	},
	Metrics: []string{"revenue", "churn"},
}

const goodSpec = `{
  "goal": "grow revenue without hurting retention",
  "owner": "phil",
  "rules": [
    {"type": "card.charge", "param": "amount", "max": 5000, "why": "finance sign-off needed above $5k"},
    {"type": "card.charge", "param": "amount", "min": 1},
    {"type": "account.purge", "forbid": true, "why": "legal hold on customer data"},
    {"type": "card.charge", "requires": "email.send", "why": "customers are told about every charge"},
    {"type": "card.charge", "at_most": 1},
    {"type": "email.send", "class": "escrowed"}
  ],
  "guardrails": [
    {"metric": "revenue", "no_drop_over_pct": 5},
    {"metric": "churn", "no_rise_over_pct": 10}
  ],
  "policy": {"max_changed_pct": 50}
}`

func mustCompile(t *testing.T, spec string) Compiled {
	t.Helper()
	s, err := Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	return Compile(s, known)
}

func asks(c Compiled) string {
	var out []string
	for _, q := range c.Questions {
		out = append(out, q.String())
	}
	return strings.Join(out, "\n")
}

func TestGoodSpecCompiles(t *testing.T) {
	c := mustCompile(t, goodSpec)
	if !c.Ready() || len(c.Questions) != 0 {
		t.Fatalf("questions:\n%s", asks(c))
	}
	if len(c.Invariants) != 6 || len(c.Guardrails) != 2 || c.Policy.MaxChangedFraction != 0.5 {
		t.Fatalf("compiled: %d invariants, %d guardrails, %+v", len(c.Invariants), len(c.Guardrails), c.Policy)
	}
}

func TestUnknownFieldsRejected(t *testing.T) {
	_, err := Parse([]byte(`{"goal": "x", "rules": [{"type": "card.charge", "maximum": 5}]}`))
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("err = %v", err)
	}
}

func TestAmbiguitiesBecomeQuestions(t *testing.T) {
	cases := map[string]struct {
		spec     string
		blocking bool
		want     string
	}{
		"typo in type": {`{"goal":"g","owner":"p","rules":[{"type":"card.charg","forbid":true}]}`, true, `did you mean "card.charge"?`},
		"impossible bounds": {`{"goal":"g","owner":"p","rules":[
			{"type":"card.charge","param":"amount","min":100},
			{"type":"card.charge","param":"amount","max":50}]}`, true, "no value satisfies both"},
		"conflicting max": {`{"goal":"g","owner":"p","rules":[
			{"type":"card.charge","param":"amount","max":100},
			{"type":"card.charge","param":"amount","max":200}]}`, true, "which maximum is right"},
		"requires a forbidden type": {`{"goal":"g","owner":"p","rules":[
			{"type":"email.send","forbid":true},
			{"type":"card.charge","requires":"email.send"}]}`, true, "can never be planned"},
		"mixed constraints":  {`{"goal":"g","owner":"p","rules":[{"type":"card.charge","forbid":true,"max":5}]}`, true, "split it"},
		"no constraint":      {`{"goal":"g","owner":"p","rules":[{"type":"card.charge"}]}`, true, "no constraint"},
		"class mismatch":     {`{"goal":"g","owner":"p","rules":[{"type":"email.send","class":"reversible"}]}`, true, "registered as escrowed"},
		"unknown metric":     {`{"goal":"g","owner":"p","guardrails":[{"metric":"revenu","no_drop_over_pct":5}]}`, true, `did you mean "revenue"?`},
		"guardrail no limit": {`{"goal":"g","owner":"p","guardrails":[{"metric":"revenue"}]}`, true, "protect it how"},
		"no goal":            {`{"owner":"p","rules":[{"type":"account.purge","forbid":true}]}`, true, "what is this for"},
		"no owner":           {`{"goal":"g","rules":[{"type":"account.purge","forbid":true}]}`, false, "who is accountable"},
		"unconstrained":      {`{"goal":"g","owner":"p"}`, false, "any plan is acceptable"},
		"open one-way doors": {`{"goal":"g","owner":"p","rules":[{"type":"card.charge","at_most":1}],
			"policy":{"allow_new_irreversible":true}}`, false, "new one-way doors"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := mustCompile(t, tc.spec)
			if c.Ready() == tc.blocking {
				t.Errorf("Ready() = %v, want %v\n%s", c.Ready(), !tc.blocking, asks(c))
			}
			if !strings.Contains(asks(c), tc.want) {
				t.Errorf("questions missing %q:\n%s", tc.want, asks(c))
			}
		})
	}
}

// End to end: a spec file drives replay of two candidate programs.
func TestCompiledSpecGatesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intent.json")
	os.WriteFile(path, []byte(goodSpec), 0o644)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c := Compile(s, known)

	reg := reversible.NewRegistry()
	noop := func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }
	undo := func(context.Context, reversible.Params, reversible.Receipt) error { return nil }
	for typ, class := range known.Types {
		h := reversible.Handler{Class: class, Do: noop}
		if class != reversible.Irreversible {
			h.Undo = undo
		}
		reg.MustRegister(typ, h)
	}
	program := func(price int, email bool) counterfactual.Program {
		return func(_ context.Context, req counterfactual.Request) (reversible.Intent, error) {
			p := price
			if req.ID == "b" {
				p = 100 // only request "a" is affected by a candidate, keeping blast radius at 50%
			}
			in := reversible.Intent{ID: req.ID, Effects: []reversible.Effect{
				reg.MustEffect("charge", "card.charge", reversible.Params{"amount": strconv.Itoa(p)}),
			}}
			if email {
				in.Effects = append(in.Effects, reg.MustEffect("receipt", "email.send", nil))
			}
			return in, nil
		}
	}
	reqs := []counterfactual.Request{{ID: "a"}, {ID: "b"}}

	ok := counterfactual.ReplayWith(context.Background(), c.Config(counterfactual.Config{
		Requests: reqs, Baseline: program(100, true), Candidate: program(120, true)}))
	if !ok.Promote() {
		t.Fatalf("compliant candidate blocked:\n%s", ok)
	}

	bad := counterfactual.ReplayWith(context.Background(), c.Config(counterfactual.Config{
		Requests: reqs, Baseline: program(100, true), Candidate: program(9000, false)}))
	out := bad.String()
	for _, want := range []string{
		"card.charge.amount <=5000: charge.amount = 9000 (finance sign-off needed above $5k)",
		"card.charge requires email.send: plans card.charge without email.send (customers are told about every charge)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n%s", want, out)
		}
	}
	if bad.Promote() {
		t.Fatal("non-compliant candidate promoted")
	}
}
