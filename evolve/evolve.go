// Package evolve runs several versions of an agent's logic at once and lets
// observed outcomes decide which survives.
//
// A Population has one champion, which serves most traffic, and any number
// of challengers. A challenger only gets traffic after passing the replay
// gate: recorded requests are replayed through it with every effect
// intercepted and checked against the same invariants, guardrails and
// policy as any other change. Live outcomes are then attributed to the
// variant that served each request. When a challenger has enough data,
// Evaluate either promotes it (it beat the champion on the objective),
// retires it (it breached a guardrail), or leaves it running.
//
// Each variant is tied to the decision that introduced it in the decision
// ledger. Outcomes are recorded there, and a retired challenger's decision
// can be rewound automatically: every intent it executed is undone.
package evolve

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
	"github.com/PureMunky/reversible/decision"
)

// Variant is one version of the logic.
type Variant struct {
	Name    string
	Program counterfactual.Program
	// Decision is the ledger decision that introduced this variant. Intents
	// it serves are linked to it, and rewinding it undoes them all.
	Decision string
}

// Objective is what "better" means.
type Objective struct {
	Metric   string
	Maximize bool
	// MinLiftPct is how much better (in percent of the champion's average)
	// a challenger must be to be promoted.
	MinLiftPct float64
	// NonInferior promotes a challenger that is merely no worse: once it
	// has MaxSamples outcomes with no guardrail breach (confirmed or
	// suspected) and no significant evidence it is worse. This is how a
	// canary release works, where the new version exists for reasons other
	// than the metric (new features, new rules) and the question is only
	// whether it does harm.
	NonInferior bool
}

// Options configures a Population.
type Options struct {
	// Gate is the replay configuration every challenger must pass:
	// invariants, guardrails, policy and simulators. Requests, Baseline and
	// Candidate are filled in by Propose.
	Gate counterfactual.Config
	// Recorder captures served traffic for future gates.
	Recorder counterfactual.Recorder
	// ChallengerShare is the total share of traffic challengers get,
	// split evenly between them (default 0.2).
	ChallengerShare float64
	Objective       Objective
	// Guardrails are checked against observed averages, challenger vs
	// champion. A breach retires the challenger.
	Guardrails []counterfactual.Guardrail
	// MinSamples is how many observed outcomes each side needs before a
	// challenger is judged (default 30).
	MinSamples int
	// MaxSamples retires a challenger as inconclusive once it has this many
	// outcomes without a significant result (0 means never).
	MaxSamples int
	// Alpha is the overall false-positive budget for promotions and
	// "worse" verdicts (default 0.05). It is spent across repeated
	// evaluations, so checking often doesn't inflate it.
	Alpha float64
	// GuardrailAlpha is the budget for guardrail breaches (default 0.10):
	// looser than Alpha, because stopping harm early matters more than
	// being sure.
	GuardrailAlpha float64
	// Salt keys traffic assignment. Leave it empty for a random salt; set
	// it only to reproduce an assignment exactly (for example in tests).
	Salt string
	// Calibrator, if set (with Gate.Simulators), receives a prediction for
	// every served request paired with its observed outcome, and the gate
	// uses its latest fit for every new proposal.
	Calibrator *counterfactual.Calibrator

	// Ledger, if set, links served intents to variant decisions and
	// records evaluation outcomes.
	Ledger *decision.Graph
	// AutoRewind rewinds a retired challenger's decision through Undoer.
	AutoRewind bool
	Undoer     decision.Undoer
	// Actor is who evaluations and rewinds are attributed to.
	Actor string
}

// Status is where a variant stands.
type Status string

const (
	Champion   Status = "champion"
	Challenger Status = "challenger"
	Retired    Status = "retired"
	Rejected   Status = "rejected" // failed the replay gate
)

type stats struct {
	n       int
	metrics map[string]*welford
}

func newStats() *stats { return &stats{metrics: map[string]*welford{}} }

func (s *stats) add(m map[string]float64) {
	s.n++
	for k, v := range m {
		w := s.metrics[k]
		if w == nil {
			w = &welford{}
			s.metrics[k] = w
		}
		w.add(v)
	}
}

func (s *stats) mean(m string) float64 {
	if w := s.metrics[m]; w != nil {
		return w.mean
	}
	return 0
}

func (s *stats) means() map[string]float64 {
	out := map[string]float64{}
	for k, w := range s.metrics {
		out[k] = w.mean
	}
	return out
}

type entry struct {
	v      Variant
	status Status
	reason string
	stats  *stats
	looks  int // evaluations with enough data, for alpha spending
	// baseline is the champion this challenger was admitted against.
	// Guardrails measure harm against it: what customers would have got
	// had the experiment never run. A later, better champion makes a
	// challenger "worse", not "harmful".
	baseline *entry
}

// Population is a champion and its challengers.
type Population struct {
	mu        sync.Mutex
	opts      Options
	champion  *entry
	entries   map[string]*entry
	order     []string
	served    map[string]string             // request ID -> variant name
	predicted map[string]map[string]float64 // request ID -> raw simulator prediction
	salt      [16]byte
	rounds    int
}

// New creates a population with an initial champion.
func New(champion Variant, opts Options) (*Population, error) {
	if champion.Name == "" {
		return nil, errors.New("champion needs a name")
	}
	if opts.ChallengerShare == 0 {
		opts.ChallengerShare = 0.2
	}
	if opts.ChallengerShare < 0 || opts.ChallengerShare >= 1 {
		return nil, fmt.Errorf("challenger share must be in [0, 1), got %g", opts.ChallengerShare)
	}
	if opts.MinSamples == 0 {
		opts.MinSamples = 30
	}
	if opts.Alpha == 0 {
		opts.Alpha = 0.05
	}
	if opts.GuardrailAlpha == 0 {
		opts.GuardrailAlpha = 0.10
	}
	if opts.Objective.Metric == "" {
		return nil, errors.New("an objective metric is required")
	}
	if opts.Objective.NonInferior && opts.MaxSamples <= 0 {
		return nil, errors.New("a non-inferiority objective needs MaxSamples: how much evidence of no harm is enough")
	}
	if opts.AutoRewind && (opts.Ledger == nil || opts.Undoer == nil) {
		return nil, errors.New("AutoRewind needs a Ledger and an Undoer")
	}
	e := &entry{v: champion, status: Champion, stats: newStats()}
	p := &Population{opts: opts, champion: e, entries: map[string]*entry{champion.Name: e},
		order: []string{champion.Name}, served: map[string]string{}, predicted: map[string]map[string]float64{}}
	if opts.Salt != "" {
		sum := sha256.Sum256([]byte(opts.Salt))
		copy(p.salt[:], sum[:16])
	} else if _, err := rand.Read(p.salt[:]); err != nil {
		return nil, err
	}
	return p, nil
}

// Propose runs a candidate through the replay gate against the current
// champion, using recorded traffic. It joins as a challenger only if the
// gate says it's safe to promote; the report says why either way.
func (p *Population) Propose(ctx context.Context, v Variant) (counterfactual.Report, error) {
	if v.Name == "" || v.Program == nil {
		return counterfactual.Report{}, errors.New("variant needs a name and a program")
	}
	p.mu.Lock()
	if _, dup := p.entries[v.Name]; dup {
		p.mu.Unlock()
		return counterfactual.Report{}, fmt.Errorf("variant %q already exists", v.Name)
	}
	champ := p.champion.v
	p.mu.Unlock()

	var reqs []counterfactual.Request
	if p.opts.Recorder != nil {
		var err error
		if reqs, err = p.opts.Recorder.Requests(); err != nil {
			return counterfactual.Report{}, err
		}
	}
	if len(reqs) == 0 {
		return counterfactual.Report{}, errors.New("no recorded traffic to gate against; serve traffic with a Recorder first")
	}
	cfg := p.opts.Gate
	cfg.Requests, cfg.Baseline, cfg.Candidate = reqs, champ.Program, v.Program
	if p.opts.Calibrator != nil {
		cfg.Calibration = p.opts.Calibrator.Fit()
	}
	rep := counterfactual.ReplayWith(ctx, cfg)

	p.mu.Lock()
	defer p.mu.Unlock()
	e := &entry{v: v, status: Challenger, stats: newStats(), baseline: p.champion}
	if !rep.Promote() {
		e.status, e.reason = Rejected, strings.Join(rep.Blockers, "; ")
	}
	p.entries[v.Name] = e
	p.order = append(p.order, v.Name)
	return rep, nil
}

// bucket hashes a request ID with the population's secret salt. Keyed
// SHA-256 makes assignment independent of any other hash of the same ID
// (a customer-trait hash, a shard key), which a plain hash like FNV is
// not: a correlated assignment is selection bias, and it produces
// confident, wrong verdicts.
func (p *Population) bucket(id string) uint64 {
	h := sha256.New()
	h.Write(p.salt[:])
	h.Write([]byte(id))
	return binary.BigEndian.Uint64(h.Sum(nil))
}

// Route picks the variant for a request. Assignment is a stable keyed hash
// of the request ID, so retries land on the same variant.
func (p *Population) Route(req counterfactual.Request) Variant {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.route(req).v
}

func (p *Population) route(req counterfactual.Request) *entry {
	chs := p.challengers()
	if len(chs) == 0 {
		return p.champion
	}
	x := p.bucket(req.ID)
	if float64(x%10000)/10000 >= p.opts.ChallengerShare {
		return p.champion
	}
	return chs[(x/10000)%uint64(len(chs))]
}

func (p *Population) challengers() []*entry {
	var out []*entry
	for _, n := range p.order {
		if e := p.entries[n]; e.status == Challenger {
			out = append(out, e)
		}
	}
	return out
}

// Admit adds a challenger that passed a gate the caller ran itself (for
// example a behaviour diff of a built service), so it gets traffic
// straight away. Its guardrail baseline is the current champion.
func (p *Population) Admit(v Variant) error {
	if v.Name == "" {
		return errors.New("variant needs a name")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, dup := p.entries[v.Name]; dup {
		return fmt.Errorf("variant %q already exists", v.Name)
	}
	p.entries[v.Name] = &entry{v: v, status: Challenger, stats: newStats(), baseline: p.champion}
	p.order = append(p.order, v.Name)
	return nil
}

// Assign routes a request and records which variant got it, without
// running anything: for variants served elsewhere, such as running
// services behind a router. Report the outcome with Observe.
func (p *Population) Assign(requestID string) Variant {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.route(counterfactual.Request{ID: requestID})
	p.served[requestID] = e.v.Name
	return e.v
}

// Serve routes a request, records it for future gates, asks the chosen
// variant for an intent, and executes it (linked to the variant's decision
// when a ledger is configured).
func (p *Population) Serve(ctx context.Context, rt *reversible.Runtime, req counterfactual.Request) (Variant, *reversible.Result, error) {
	p.mu.Lock()
	e := p.route(req)
	p.served[req.ID] = e.v.Name
	p.mu.Unlock()
	v := e.v

	if p.opts.Recorder != nil {
		if err := p.opts.Recorder.Record(req); err != nil {
			return v, nil, err
		}
	}
	if v.Program == nil {
		return v, nil, fmt.Errorf("variant %s has no program; it is served elsewhere (use Assign)", v.Name)
	}
	in, err := v.Program(ctx, req)
	if err != nil {
		return v, nil, fmt.Errorf("variant %s: %w", v.Name, err)
	}
	if p.opts.Calibrator != nil && p.opts.Gate.Simulators != nil {
		pred := counterfactual.Predict(p.opts.Gate.Simulators, req, counterfactual.PlanOf(in))
		p.mu.Lock()
		p.predicted[req.ID] = pred
		p.mu.Unlock()
	}
	if p.opts.Ledger != nil && v.Decision != "" {
		res, err := p.opts.Ledger.Execute(ctx, rt, v.Decision, in)
		return v, res, err
	}
	res, err := rt.Execute(ctx, in)
	return v, res, err
}

// Observe attributes an outcome to whichever variant served the request.
func (p *Population) Observe(requestID string, metrics map[string]float64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	name, ok := p.served[requestID]
	if !ok {
		return fmt.Errorf("request %q was not served by this population", requestID)
	}
	if pred, ok := p.predicted[requestID]; ok {
		delete(p.predicted, requestID)
		p.opts.Calibrator.Add(pred, metrics) // reality teaches the model, whoever served
	}
	e := p.entries[name]
	if e.status == Retired || e.status == Rejected {
		return nil // late outcomes for retired variants don't count toward evaluation
	}
	e.stats.add(metrics)
	return nil
}

// RetireKind says why a challenger was stopped.
type RetireKind string

const (
	// Breach: an observed guardrail breach. With AutoRewind, the
	// challenger's decision is rewound and everything it did is undone.
	Breach RetireKind = "guardrail breach"
	// Worse: significantly worse on the objective. Its actions were within
	// bounds, so they stand; it just stops getting traffic.
	Worse RetireKind = "worse on objective"
	// Inconclusive: reached MaxSamples with no significant difference.
	Inconclusive RetireKind = "inconclusive"
)

// Retirement is a challenger that was stopped, and what was done about it.
type Retirement struct {
	Variant  string
	Kind     RetireKind
	Reason   string
	Decision string                 // the challenger's decision
	Rewind   *decision.RewindReport // set when AutoRewind ran (breaches only)
}

// Pending is a challenger still being evaluated.
type Pending struct {
	Variant string
	Note    string
}

// Verdict is the result of an evaluation round.
type Verdict struct {
	Promoted  string // new champion, if any
	Dethroned string // old champion, if one was replaced
	Lift      float64
	P         float64 // p-value behind the promotion (for a non-inferior promotion: of the "worse" test)
	// NonInferior is true when the promotion was for doing no harm, not
	// for being better; Samples is the evidence it rests on.
	NonInferior bool
	Samples     int
	Retired     []Retirement
	Waiting     []Pending
}

func (v Verdict) String() string {
	var b strings.Builder
	if v.Promoted != "" && v.NonInferior {
		fmt.Fprintf(&b, "promoted %s over %s: no harm after %d outcomes (%+.1f%% on objective)\n", v.Promoted, v.Dethroned, v.Samples, v.Lift)
	} else if v.Promoted != "" {
		fmt.Fprintf(&b, "promoted %s over %s (%+.1f%% on objective, p=%.2g)\n", v.Promoted, v.Dethroned, v.Lift, v.P)
	}
	for _, r := range v.Retired {
		fmt.Fprintf(&b, "retired %s (%s): %s\n", r.Variant, r.Kind, r.Reason)
		switch {
		case r.Rewind != nil:
			fmt.Fprintf(&b, "  rewound decision %s: %d intent(s) undone", r.Decision, len(r.Rewind.Undone))
			if len(r.Rewind.Stranded) > 0 {
				fmt.Fprintf(&b, ", %d stranded", len(r.Rewind.Stranded))
			}
			b.WriteString("\n")
		case r.Kind == Breach && r.Decision != "":
			fmt.Fprintf(&b, "  suggest rewinding decision %s\n", r.Decision)
		}
	}
	for _, w := range v.Waiting {
		fmt.Fprintf(&b, "waiting on %s: %s\n", w.Variant, w.Note)
	}
	if b.Len() == 0 {
		return "no challengers\n"
	}
	return b.String()
}

// Evaluate judges every challenger with enough outcomes, in this order:
//
//  1. A guardrail breached significantly (GuardrailAlpha) against the
//     champion the challenger was admitted against retires it and, with
//     AutoRewind, rewinds its decision.
//  2. A challenger significantly better on the objective (Alpha), by at
//     least MinLiftPct, is a promotion candidate; the best one wins.
//  3. One significantly worse is retired; its actions stand.
//  4. One at MaxSamples with no significant result is retired.
//  5. Otherwise it keeps running.
//
// Significance uses Welch's t-test, and each challenger's budget is spent
// across evaluations (alpha/2, alpha/4, ...), so evaluating often is safe.
func (p *Population) Evaluate(ctx context.Context) (Verdict, error) {
	p.mu.Lock()
	p.rounds++
	round := p.rounds
	champ := p.champion
	obj := p.opts.Objective
	var verdict Verdict
	type candidate struct {
		e           *entry
		lift, p     float64
		nonInferior bool
	}
	var best *candidate
	var toRetire []Retirement
	var outcomes []decision.Outcome

	for _, e := range p.challengers() {
		base := e.baseline
		if base == nil {
			base = champ
		}
		if e.stats.n < p.opts.MinSamples || champ.stats.n < p.opts.MinSamples || base.stats.n < p.opts.MinSamples {
			verdict.Waiting = append(verdict.Waiting, Pending{e.v.Name,
				fmt.Sprintf("%d/%d outcomes (champion %d)", e.stats.n, p.opts.MinSamples, champ.stats.n)})
			continue
		}
		e.looks++
		alpha, galpha := spend(p.opts.Alpha, e.looks), spend(p.opts.GuardrailAlpha, e.looks)
		if e.v.Decision != "" {
			outcomes = append(outcomes, decision.Outcome{
				ID:      fmt.Sprintf("eval-%d-%s", round, e.v.Name),
				Parents: []string{e.v.Decision},
				Summary: fmt.Sprintf("%s vs %s after %d outcomes", e.v.Name, champ.v.Name, e.stats.n),
				Metrics: e.stats.means(),
				Actor:   p.opts.Actor,
			})
		}

		ms := compare(base.stats.means(), e.stats.means())
		var breaches, suspected []string
		for _, g := range p.opts.Guardrails {
			msg := g.Check(ms)
			if msg == "" {
				continue
			}
			msg = strings.Replace(msg, "predicted ", "observed ", 1)
			var pv float64
			if g.Rise {
				pv = pGreater(e.stats.metrics[g.Metric], base.stats.metrics[g.Metric])
			} else {
				pv = pGreater(base.stats.metrics[g.Metric], e.stats.metrics[g.Metric])
			}
			if pv < galpha {
				breaches = append(breaches, fmt.Sprintf("%s, p=%.2g", msg, pv))
			} else {
				suspected = append(suspected, fmt.Sprintf("%s but not yet significant (p=%.2g, need <%.2g)", msg, pv, galpha))
			}
		}
		if len(breaches) > 0 {
			e.status, e.reason = Retired, strings.Join(breaches, "; ")
			toRetire = append(toRetire, Retirement{Variant: e.v.Name, Kind: Breach, Reason: e.reason, Decision: e.v.Decision})
			continue
		}

		cw, chw := e.stats.metrics[obj.Metric], champ.stats.metrics[obj.Metric]
		better, worse := pGreater(cw, chw), pGreater(chw, cw)
		if !obj.Maximize {
			better, worse = worse, better
		}
		lift := liftPct(champ.stats.mean(obj.Metric), e.stats.mean(obj.Metric), obj.Maximize)
		switch {
		case obj.NonInferior && len(suspected) == 0 && worse >= alpha && e.stats.n >= p.opts.MaxSamples:
			if best == nil || lift > best.lift {
				best = &candidate{e: e, lift: lift, p: worse, nonInferior: true}
			}
		case len(suspected) == 0 && better < alpha && lift >= obj.MinLiftPct:
			if best == nil || lift > best.lift {
				best = &candidate{e: e, lift: lift, p: better}
			}
		case worse < alpha:
			e.status = Retired
			e.reason = fmt.Sprintf("%s %+.1f%% vs %s, p=%.2g", obj.Metric, lift, champ.v.Name, worse)
			toRetire = append(toRetire, Retirement{Variant: e.v.Name, Kind: Worse, Reason: e.reason, Decision: e.v.Decision})
		case p.opts.MaxSamples > 0 && e.stats.n >= p.opts.MaxSamples:
			e.status = Retired
			e.reason = fmt.Sprintf("no significant difference after %d outcomes (%s %+.1f%%)", e.stats.n, obj.Metric, lift)
			toRetire = append(toRetire, Retirement{Variant: e.v.Name, Kind: Inconclusive, Reason: e.reason, Decision: e.v.Decision})
		case obj.NonInferior:
			note := fmt.Sprintf("%d/%d outcomes, %s %+.1f%%, no harm detected so far", e.stats.n, p.opts.MaxSamples, obj.Metric, lift)
			if len(suspected) > 0 {
				note = fmt.Sprintf("%d outcomes; %s", e.stats.n, strings.Join(suspected, "; "))
			}
			verdict.Waiting = append(verdict.Waiting, Pending{e.v.Name, note})
		default:
			note := fmt.Sprintf("%s %+.1f%%, p=%.2g (need <%.2g", obj.Metric, lift, better, alpha)
			if obj.MinLiftPct > 0 {
				note += fmt.Sprintf(" and lift ≥%g%%", obj.MinLiftPct)
			}
			note += ")"
			if len(suspected) > 0 {
				note += "; " + strings.Join(suspected, "; ")
			}
			verdict.Waiting = append(verdict.Waiting, Pending{e.v.Name, note})
		}
	}
	if best != nil {
		champ.status, champ.reason = Retired, "replaced by "+best.e.v.Name
		best.e.status = Champion
		p.champion = best.e
		verdict.Promoted, verdict.Dethroned, verdict.Lift, verdict.P = best.e.v.Name, champ.v.Name, best.lift, best.p
		verdict.NonInferior, verdict.Samples = best.nonInferior, best.e.stats.n
	}
	p.mu.Unlock()

	var errs []error
	if p.opts.Ledger != nil {
		for _, o := range outcomes {
			if _, err := p.opts.Ledger.Observe(o); err != nil {
				errs = append(errs, err)
			}
		}
	}
	for i := range toRetire {
		r := &toRetire[i]
		if r.Kind == Breach && p.opts.AutoRewind && r.Decision != "" {
			rep, err := p.opts.Ledger.Rewind(ctx, r.Decision, p.opts.Undoer, "auto-rewind: "+r.Reason, p.opts.Actor)
			r.Rewind = &rep
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	verdict.Retired = toRetire
	sort.Slice(verdict.Waiting, func(i, j int) bool { return verdict.Waiting[i].Variant < verdict.Waiting[j].Variant })
	return verdict, errors.Join(errs...)
}

// VariantStatus describes one variant.
type VariantStatus struct {
	Name     string
	Status   Status
	Reason   string
	Samples  int
	Averages map[string]float64
}

// Status lists every variant ever proposed, in order.
func (p *Population) Status() []VariantStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []VariantStatus
	for _, n := range p.order {
		e := p.entries[n]
		out = append(out, VariantStatus{Name: n, Status: e.status, Reason: e.reason, Samples: e.stats.n, Averages: e.stats.means()})
	}
	return out
}

// Calibration returns the calibrator's current fit (nil without one).
func (p *Population) Calibration() *counterfactual.Calibration {
	if p.opts.Calibrator == nil {
		return nil
	}
	return p.opts.Calibrator.Fit()
}

// Champion returns the current champion.
func (p *Population) Champion() Variant {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.champion.v
}

func compare(base, cand map[string]float64) []counterfactual.MetricChange {
	names := map[string]bool{}
	for k := range base {
		names[k] = true
	}
	for k := range cand {
		names[k] = true
	}
	var out []counterfactual.MetricChange
	for k := range names {
		out = append(out, counterfactual.MetricChange{Metric: k, Baseline: base[k], Candidate: cand[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

func liftPct(base, cand float64, maximize bool) float64 {
	mc := counterfactual.MetricChange{Baseline: base, Candidate: cand}
	pct := mc.Pct()
	if !maximize {
		pct = -pct
	}
	return pct
}
