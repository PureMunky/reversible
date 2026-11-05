package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/PureMunky/reversible/policy"
)

// standIn is NOT a language model. It is a small deterministic heuristic
// that reads the same prompt a model would get and replies in the same
// JSON format, so the demo runs offline and reproducibly. Set
// ANTHROPIC_API_KEY to use Claude instead.
//
// Its strategy: first propose splitting the champion's single price by
// tenure (and one deliberately rule-breaking idea, to show the gate
// working on model output); then hill-climb each segment's price in $5
// steps, largest segment first, never re-proposing a policy already tried.
// Its rationales are plain hypotheses; it does not read the evidence the
// way a model would.
type standIn struct{}

var (
	champLine = regexp.MustCompile(`CURRENT CHAMPION "([^"]+)": (\{.*\})`)
	trialLine = regexp.MustCompile(`(?m)^- (\S+) \[`)
)

type proposal struct {
	Name      string        `json:"name"`
	Rationale string        `json:"rationale"`
	Policy    policy.Policy `json:"policy"`
}

func (standIn) Complete(_ context.Context, _, prompt string) (string, error) {
	m := champLine.FindStringSubmatch(prompt)
	if m == nil {
		return "", fmt.Errorf("stand-in: no champion in prompt")
	}
	var champ policy.Policy
	if err := json.Unmarshal([]byte(m[2]), &champ); err != nil {
		return "", err
	}
	tried := map[string]bool{}
	for _, t := range trialLine.FindAllStringSubmatch(prompt, -1) {
		tried[t[1]] = true
	}

	var out []proposal
	if _, tiered := champ.Vars["loyal"]; !tiered {
		base := 55.0
		if v, ok := champ.Vars["price"]; ok {
			base = v
		}
		p := tieredPolicy(base-10, base, base+15)
		if !tried[tieredName(p)] {
			out = append(out, proposal{Name: tieredName(p), Policy: p,
				Rationale: fmt.Sprintf("Customers likely differ in price tolerance by tenure. Charge new customers less ($%g) to keep them, loyal ones more ($%g), and leave the middle at $%g.", base-10, base+15, base)})
		}
		if !tried["premium-120"] {
			out = append(out, proposal{Name: "premium-120", Policy: flatPolicy(120),
				Rationale: "Test whether demand is much less price-sensitive than assumed."})
		}
	} else {
		// Largest segments first: a change there moves the average most,
		// so it reaches significance soonest.
		for _, seg := range []string{"mid", "new", "loyal"} {
			for _, d := range []float64{-5, 5} {
				v := champ.Vars[seg] + d
				if v < 30 || v > 100 {
					continue
				}
				vars := map[string]float64{}
				for k, x := range champ.Vars {
					vars[k] = x
				}
				vars[seg] = v
				p := tieredPolicy(vars["new"], vars["mid"], vars["loyal"])
				if tried[tieredName(p)] {
					continue
				}
				hyp := "a cut keeps enough extra customers to pay for itself"
				if d > 0 {
					hyp = "this segment tolerates more without leaving"
				}
				out = append(out, proposal{Name: tieredName(p), Policy: p,
					Rationale: fmt.Sprintf("Step the %s-customer price from $%g to $%g; hypothesis: %s.", seg, champ.Vars[seg], v, hyp)})
			}
		}
		if len(out) > 2 {
			out = out[:2]
		}
	}
	b, _ := json.Marshal(map[string]any{"candidates": out})
	return string(b), nil
}

func charge(amount string) []policy.Effect {
	return []policy.Effect{
		{Name: "charge", Type: "card.charge", Params: map[string]string{"customer": "${input.customer}", "amount": amount}},
		{Name: "receipt", Type: "email.send", Params: map[string]string{"to": "${input.customer}"}},
	}
}

func tieredPolicy(newP, mid, loyal float64) policy.Policy {
	return policy.Policy{
		Vars: map[string]float64{"new": newP, "mid": mid, "loyal": loyal},
		Rules: []policy.Rule{
			{When: []policy.Cond{{Input: "tenure_months", Op: "<", Value: "6"}}, Effects: charge("${var.new}")},
			{When: []policy.Cond{{Input: "tenure_months", Op: ">=", Value: "24"}}, Effects: charge("${var.loyal}")},
			{Effects: charge("${var.mid}")},
		},
	}
}

func flatPolicy(price float64) policy.Policy {
	return policy.Policy{Vars: map[string]float64{"price": price}, Rules: []policy.Rule{{Effects: charge("${var.price}")}}}
}

func tieredName(p policy.Policy) string {
	return fmt.Sprintf("tiered-%g-%g-%g", p.Vars["new"], p.Vars["mid"], p.Vars["loyal"])
}
