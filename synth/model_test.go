package synth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/intent"
	"github.com/PureMunky/reversible/model"
	"github.com/PureMunky/reversible/policy"
)

func flat(price string) policy.Policy {
	return policy.Policy{Rules: []policy.Rule{{Effects: []policy.Effect{{Name: "charge", Type: "card.charge",
		Params: map[string]string{"customer": "${input.customer}", "amount": price}}}}}}
}

// scripted returns canned replies in order and records every prompt.
type scripted struct {
	replies []string
	prompts []string
}

func (s *scripted) Complete(_ context.Context, system, prompt string) (string, error) {
	s.prompts = append(s.prompts, prompt)
	if len(s.replies) == 0 {
		return "", errors.New("no more replies")
	}
	r := s.replies[0]
	s.replies = s.replies[1:]
	return r, nil
}

func modelHarness(t *testing.T, m model.Completer) (*harness, *ModelGenerator) {
	t.Helper()
	var gen *ModelGenerator
	hn := newHarness(t, func(reg *reversible.Registry, _ Knob) Generator {
		spec, _ := intent.Parse([]byte(specJSON))
		gen = &ModelGenerator{Model: m, Spec: spec, Registry: reg,
			Examples: []counterfactual.Request{{ID: "x", Input: map[string]string{"customer": "c000001"}}}}
		gen.Register("$55", flat("55"))
		return gen
	})
	return hn, gen
}

const goodReply = "Here are my proposals:\n```json\n" + `{"candidates": [
  {"name": "flat-52", "rationale": "a small cut should reduce churn more than it costs",
   "policy": {"vars": {"p": 52}, "rules": [{"effects": [{"name": "charge", "type": "card.charge",
     "params": {"customer": "${input.customer}", "amount": "${var.p}"}}]}]}},
  {"name": "over-the-top", "rationale": "maximise revenue per sale",
   "policy": {"rules": [{"effects": [{"name": "charge", "type": "card.charge",
     "params": {"customer": "${input.customer}", "amount": "95"}}]}]}},
  {"name": "broken", "rationale": "uses a made-up effect",
   "policy": {"rules": [{"effects": [{"name": "x", "type": "wire.transfer"}]}]}}
]}` + "\n```"

func TestModelProposalsAreGatedLikeAnythingElse(t *testing.T) {
	m := &scripted{replies: []string{goodReply}}
	hn, gen := modelHarness(t, m)
	hn.serve(t, 30)
	hn.loop.opts.MaxLive = 3
	rep, err := hn.loop.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Admitted) != 1 || rep.Admitted[0].Name != "flat-52" {
		t.Fatalf("admitted: %+v", rep.Admitted)
	}
	reasons := map[string]string{}
	for _, r := range rep.Rejected {
		reasons[r.Name] = r.Reason
	}
	if !strings.Contains(reasons["over-the-top"], "violates invariants") {
		t.Errorf("spec-breaking proposal not rejected by the gate: %q", reasons["over-the-top"])
	}
	if !strings.Contains(reasons["broken"], "fails on") {
		t.Errorf("invalid proposal not rejected by the gate: %q", reasons["broken"])
	}
	// The ledger records what the model tried and why it was refused.
	n, _ := hn.g.Get(rep.Rejected[len(rep.Rejected)-1].Decision)
	if !strings.Contains(n.Rationale, `[invalid: rules[0].effects[0]: unknown effect type "wire.transfer"]`) {
		t.Errorf("ledger rationale: %q", n.Rationale)
	}
	if p, ok := gen.Policy("flat-52"); !ok || p.Vars["p"] != 52 {
		t.Errorf("valid policy not registered: %+v", p)
	}
}

func TestPromptCarriesSpecHistoryAndChampion(t *testing.T) {
	m := &scripted{replies: []string{goodReply, `{"candidates": []}`}}
	hn, _ := modelHarness(t, m)
	hn.serve(t, 30)
	hn.loop.Step(context.Background())
	hn.serve(t, 300)
	hn.pop.Evaluate(context.Background())
	hn.loop.Step(context.Background()) // second call sees the first trials

	p := m.prompts[1]
	for _, want := range []string{
		"GOAL: maximise revenue per customer (owner: phil)",
		"card.charge.amount >= 40 <= 70",
		"churn must not rise more than 50% vs the champion",
		"- card.charge (escrowed)",
		`{"customer":"c000001"}`,
		`CURRENT CHAMPION "$55"`,
		"- flat-52 [challenger] n=",
		"policy: otherwise: charge(amount=52)",
		"hypothesis: a small cut should reduce churn more than it costs",
		"- over-the-top [rejected]",
		"result: candidate violates invariants",
		"Propose 2 new policies",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestModelFailuresSurfaceAsErrorsNotConvergence(t *testing.T) {
	for name, m := range map[string]model.Completer{
		"api error": &scripted{},
		"prose":     &scripted{replies: []string{"I think you should lower prices."}},
		"bad json":  &scripted{replies: []string{`{"candidates": "lots"}`}},
	} {
		t.Run(name, func(t *testing.T) {
			hn, _ := modelHarness(t, m)
			hn.serve(t, 10)
			rep, err := hn.loop.Step(context.Background())
			if err == nil || rep.Converged {
				t.Fatalf("rep=%+v err=%v", rep, err)
			}
		})
	}
}

func TestDuplicateNamesAreDisambiguated(t *testing.T) {
	reply := `{"candidates": [
	  {"name": "$55", "policy": {"rules": [{"effects": [{"name": "c", "type": "card.charge", "params": {"amount": "54"}}]}]}},
	  {"name": "$55", "policy": {"rules": [{"effects": [{"name": "c", "type": "card.charge", "params": {"amount": "56"}}]}]}}]}`
	hn, _ := modelHarness(t, &scripted{replies: []string{reply}})
	hn.serve(t, 10)
	rep, err := hn.loop.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range append(rep.Admitted, rep.Rejected...) {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "_55-2,_55-3" && strings.Join(names, ",") != "_55,_55-2" {
		t.Fatalf("names: %v", names)
	}
}
