package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
)

func registry() *reversible.Registry {
	reg := reversible.NewRegistry()
	noop := func(context.Context, reversible.Params) (reversible.Receipt, error) { return nil, nil }
	undo := func(context.Context, reversible.Params, reversible.Receipt) error { return nil }
	reg.MustRegister("card.charge", reversible.Handler{Class: reversible.Escrowed, Do: noop, Undo: undo})
	reg.MustRegister("email.send", reversible.Handler{Class: reversible.Escrowed, Do: noop})
	return reg
}

const tiered = `{
  "name": "tiered",
  "vars": {"new": 45, "loyal": 65, "mid": 55},
  "rules": [
    {"when": [{"input": "tenure_months", "op": "<", "value": "6"}],
     "effects": [{"name": "charge", "type": "card.charge", "params": {"customer": "${input.customer}", "amount": "${var.new}"}},
                 {"name": "receipt", "type": "email.send", "params": {"to": "${input.customer}"}}]},
    {"when": [{"input": "tenure_months", "op": ">=", "value": "24"}, {"input": "region", "in": "", "op": "in", "value": "us, ca"}],
     "effects": [{"name": "charge", "type": "card.charge", "params": {"customer": "${input.customer}", "amount": "${var.loyal}"}}]},
    {"effects": [{"name": "charge", "type": "card.charge", "params": {"customer": "${input.customer}", "amount": "${var.mid}"}}]}
  ]
}`

func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(tiered)); err == nil || !strings.Contains(err.Error(), `"in"`) {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func mustParse(t *testing.T, s string) Policy {
	t.Helper()
	p, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompileAndRun(t *testing.T) {
	p := mustParse(t, strings.Replace(tiered, `"in": "", `, "", 1))
	prog, err := Compile(p, registry(), "bill")
	if err != nil {
		t.Fatal(err)
	}
	plan := func(input map[string]string) counterfactual.Plan {
		return counterfactual.Simulate(context.Background(), prog, counterfactual.Request{ID: "r1", Input: input})
	}
	cases := []struct {
		input  map[string]string
		amount string
		n      int
	}{
		{map[string]string{"customer": "a", "tenure_months": "2", "region": "us"}, "45", 2},
		{map[string]string{"customer": "b", "tenure_months": "30", "region": "ca"}, "65", 1},
		{map[string]string{"customer": "c", "tenure_months": "30", "region": "eu"}, "55", 1},
		{map[string]string{"customer": "d", "tenure_months": "12", "region": "us"}, "55", 1},
	}
	for _, c := range cases {
		pl := plan(c.input)
		if pl.Err != "" || len(pl.Effects) != c.n || pl.Effects[0].Params["amount"] != c.amount || pl.Effects[0].Params["customer"] != c.input["customer"] {
			t.Errorf("%v -> %+v", c.input, pl)
		}
	}
	if !strings.Contains(p.Summary(), "if tenure_months < 6: charge(amount=45) + receipt; ") {
		t.Errorf("summary: %s", p.Summary())
	}
}

func TestValidateFindsEveryProblem(t *testing.T) {
	bad := Policy{
		Vars: map[string]float64{"p": 1},
		Rules: []Rule{
			{Effects: []Effect{{Name: "charge", Type: "card.charge", Params: map[string]string{"amount": "${var.q}"}}}},
			{When: []Cond{{Input: "x", Op: "~", Value: "1"}, {Input: "y", Op: "<", Value: "ten"}},
				Effects: []Effect{{Name: "a", Type: "wire.transfer"}, {Name: "a", Type: "email.send", Params: map[string]string{"to": "${input.who"}}}},
			{},
		},
	}
	err := bad.Validate(registry().Types())
	for _, want := range []string{
		"rules[0] has no conditions, so the rules after it can never match",
		`unknown variable "q"`,
		`unknown op "~"`,
		`< needs a number, got "ten"`,
		`unknown effect type "wire.transfer"`,
		`duplicate effect name "a"`,
		"malformed reference",
		"rules[2] plans no effects",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestNoMatchingRuleIsAnErrorTheGateWillSee(t *testing.T) {
	p := Policy{Name: "narrow", Rules: []Rule{{When: []Cond{{Input: "tenure_months", Op: ">", Value: "100"}},
		Effects: []Effect{{Name: "c", Type: "card.charge"}}}}}
	prog, err := Compile(p, registry(), "")
	if err != nil {
		t.Fatal(err)
	}
	pl := counterfactual.Simulate(context.Background(), prog, counterfactual.Request{ID: "r", Input: map[string]string{"tenure_months": "3"}})
	if !strings.Contains(pl.Err, "no rule of narrow matched") {
		t.Fatalf("plan: %+v", pl)
	}
}

func TestNonNumericInputForNumericOp(t *testing.T) {
	p := Policy{Rules: []Rule{{When: []Cond{{Input: "tenure_months", Op: "<", Value: "6"}}, Effects: []Effect{{Name: "c", Type: "card.charge"}}},
		{Effects: []Effect{{Name: "c", Type: "card.charge"}}}}}
	prog, _ := Compile(p, registry(), "")
	pl := counterfactual.Simulate(context.Background(), prog, counterfactual.Request{ID: "r", Input: map[string]string{"tenure_months": "lots"}})
	if !strings.Contains(pl.Err, "is not a number") {
		t.Fatalf("plan: %+v", pl)
	}
}
