package factory

import (
	"context"
	"errors"
	"strings"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
	"github.com/PureMunky/reversible/evolve"
)

// Review is everything checked before a candidate may touch production:
// the behaviour diff on recorded traffic, the read-back of existing data,
// and whether the two versions can safely run side by side.
type Review struct {
	Replay BehaviourDiff
	State  BehaviourDiff
	Mixed  []string // problems running both versions on the same data
}

// Clean reports whether every difference is explained by the intent and
// the versions can share data.
func (r Review) Clean() bool {
	return len(r.Replay.Unexpected()) == 0 && len(r.State.Unexpected()) == 0 && len(r.Mixed) == 0
}

func (r Review) String() string {
	var b strings.Builder
	b.WriteString("replaying recorded traffic through both versions:\n")
	b.WriteString(indentLines(r.Replay.String(), "  "))
	b.WriteString("reading every existing record through both versions:\n")
	b.WriteString(indentLines(r.State.String(), "  "))
	if len(r.Mixed) == 0 {
		b.WriteString("running both versions on the same data: each keeps the other's fields\n")
	} else {
		b.WriteString("running both versions on the same data:\n")
		for _, p := range r.Mixed {
			b.WriteString("  ⚠ " + p + "\n")
		}
	}
	return b.String()
}

// ReviewCandidate runs every pre-release check of a candidate against the
// router's live version and its recorded traffic and data.
func ReviewCandidate(ctx context.Context, router *Router, candidate *Version) (Review, error) {
	live := router.Live()
	if live == nil {
		return Review{}, errors.New("nothing is live to review against")
	}
	var r Review
	var err error
	if r.Replay, err = Diff(ctx, router.Traffic(), live, candidate); err != nil {
		return r, err
	}
	if r.State, err = DiffState(ctx, router.DataPath, live, candidate); err != nil {
		return r, err
	}
	if r.Mixed, err = MixedCheck(ctx, live, candidate); err != nil {
		return r, err
	}
	return r, nil
}

// CanaryOptions shape a canary release.
type CanaryOptions struct {
	Share      float64 // of traffic the canary serves (default 0.2)
	MinSamples int     // canary outcomes before any judgement (default 30)
	MaxSamples int     // canary outcomes that count as enough evidence of no harm (default 150)
	Salt       string  // reproducible assignment (default: random)
	Actor      string  // who evaluations are attributed to (default "factory")
}

// Canary is a release in progress: the candidate serves a share of real
// traffic beside the live version, every outcome is attributed to the
// version that produced it, and the evolution runtime decides. If the
// candidate does harm (server errors, significantly more than the live
// version), it is retired and the decision behind it is rewound, which
// stops the canary. If it shows no harm after enough evidence, it is
// promoted and becomes the live version.
type Canary struct {
	Candidate *Version
	Pop       *evolve.Population
	router    *Router
	ledger    *decision.Graph
	rt        *reversible.Runtime
	decision  string
	intentID  string
	done      bool
}

// StartCanary admits a candidate (which should already have a clean
// Review and sign-off) as a canary under a decision in the ledger.
func StartCanary(ctx context.Context, router *Router, candidate *Version, ledger *decision.Graph, rt *reversible.Runtime, decisionID string, opts CanaryOptions) (*Canary, error) {
	live := router.Live()
	if live == nil {
		return nil, errors.New("a canary needs a live version")
	}
	if opts.Share == 0 {
		opts.Share = 0.2
	}
	if opts.MinSamples == 0 {
		opts.MinSamples = 30
	}
	if opts.MaxSamples == 0 {
		opts.MaxSamples = 150
	}
	if opts.Actor == "" {
		opts.Actor = "factory"
	}
	pop, err := evolve.New(evolve.Variant{Name: live.SpecHash}, evolve.Options{
		ChallengerShare: opts.Share,
		Objective:       evolve.Objective{Metric: "ok", Maximize: true, NonInferior: true},
		Guardrails:      []counterfactual.Guardrail{counterfactual.NoRiseOver("error", 0)},
		MinSamples:      opts.MinSamples,
		MaxSamples:      opts.MaxSamples,
		Salt:            opts.Salt,
		Ledger:          ledger,
		AutoRewind:      ledger != nil,
		Undoer:          rt,
		Actor:           opts.Actor,
	})
	if err != nil {
		return nil, err
	}
	if err := pop.Admit(evolve.Variant{Name: candidate.SpecHash, Decision: decisionID}); err != nil {
		return nil, err
	}
	c := &Canary{Candidate: candidate, Pop: pop, router: router, ledger: ledger, rt: rt, decision: decisionID,
		intentID: "canary-" + candidate.SpecHash}
	effect := router.CanaryEffect(candidate,
		func(id string) bool { return pop.Assign(id).Name == candidate.SpecHash },
		func(id string, ex Exchange) { pop.Observe(id, outcomeOf(ex)) })
	in := reversible.Intent{ID: c.intentID, Goal: "canary " + candidate.SpecHash, Effects: []reversible.Effect{effect}}
	if ledger != nil {
		_, err = ledger.Execute(ctx, rt, decisionID, in)
	} else {
		_, err = rt.Execute(ctx, in)
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

// outcomeOf is what a canary is judged on, per request.
func outcomeOf(ex Exchange) map[string]float64 {
	ok, failed := 1.0, 0.0
	if ex.Response.Status >= 500 {
		ok, failed = 0, 1
	}
	return map[string]float64{"ok": ok, "error": failed, "latency_ms": ex.LatencyMS}
}

// CanaryResult says how an evaluation went.
type CanaryResult struct {
	Verdict  evolve.Verdict
	Promoted bool
	Stopped  bool // retired and rewound
	Waiting  string
}

// Evaluate judges the canary. On promotion the candidate becomes live
// (recorded as an effect under the same decision); on a breach the
// decision is rewound and the canary stops.
func (c *Canary) Evaluate(ctx context.Context) (CanaryResult, error) {
	if c.done {
		return CanaryResult{}, errors.New("this canary has already finished")
	}
	v, err := c.Pop.Evaluate(ctx)
	res := CanaryResult{Verdict: v}
	if err != nil {
		return res, err
	}
	switch {
	case v.Promoted == c.Candidate.SpecHash:
		in := reversible.Intent{ID: "promote-" + c.Candidate.SpecHash, Goal: "promote canary " + c.Candidate.SpecHash,
			Effects: []reversible.Effect{c.router.PromoteEffect()}}
		if c.ledger != nil {
			_, err = c.ledger.Execute(ctx, c.rt, c.decision, in)
		} else {
			_, err = c.rt.Execute(ctx, in)
		}
		if err != nil {
			return res, err
		}
		res.Promoted, c.done = true, true
	case len(v.Retired) > 0:
		if c.ledger == nil {
			if _, err := c.rt.Undo(ctx, c.intentID, "canary retired: "+v.Retired[0].Reason); err != nil {
				return res, err
			}
		}
		if c.router.Canary() == c.Candidate { // a worse-but-harmless canary isn't rewound by evolve
			c.router.endCanary()
		}
		res.Stopped, c.done = true, true
	default:
		if len(v.Waiting) > 0 {
			res.Waiting = v.Waiting[0].Note
		}
	}
	return res, nil
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// String summarises a canary result.
func (r CanaryResult) String() string {
	v := strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(r.Verdict.String()), "\n", "; ")), " ")
	switch {
	case r.Promoted:
		return "promoted: " + v
	case r.Stopped:
		return "stopped: " + v
	default:
		return "running: " + r.Waiting
	}
}
