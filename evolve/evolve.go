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
	"errors"
	"fmt"
	"hash/fnv"
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
	n    int
	sums map[string]float64
}

func (s *stats) mean(m string) float64 {
	if s.n == 0 {
		return 0
	}
	return s.sums[m] / float64(s.n)
}

func (s *stats) means() map[string]float64 {
	out := map[string]float64{}
	for k := range s.sums {
		out[k] = s.mean(k)
	}
	return out
}

type entry struct {
	v      Variant
	status Status
	reason string
	stats  *stats
}

// Population is a champion and its challengers.
type Population struct {
	mu       sync.Mutex
	opts     Options
	champion *entry
	entries  map[string]*entry
	order    []string
	served   map[string]string // request ID -> variant name
	rounds   int
}

// New creates a population with an initial champion.
func New(champion Variant, opts Options) (*Population, error) {
	if champion.Name == "" || champion.Program == nil {
		return nil, errors.New("champion needs a name and a program")
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
	if opts.Objective.Metric == "" {
		return nil, errors.New("an objective metric is required")
	}
	if opts.AutoRewind && (opts.Ledger == nil || opts.Undoer == nil) {
		return nil, errors.New("AutoRewind needs a Ledger and an Undoer")
	}
	e := &entry{v: champion, status: Champion, stats: &stats{sums: map[string]float64{}}}
	return &Population{opts: opts, champion: e, entries: map[string]*entry{champion.Name: e},
		order: []string{champion.Name}, served: map[string]string{}}, nil
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
	rep := counterfactual.ReplayWith(ctx, cfg)

	p.mu.Lock()
	defer p.mu.Unlock()
	e := &entry{v: v, status: Challenger, stats: &stats{sums: map[string]float64{}}}
	if !rep.Promote() {
		e.status, e.reason = Rejected, strings.Join(rep.Blockers, "; ")
	}
	p.entries[v.Name] = e
	p.order = append(p.order, v.Name)
	return rep, nil
}

// Route picks the variant for a request. Assignment is a stable hash of the
// request ID, so retries land on the same variant.
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
	h := fnv.New64a()
	h.Write([]byte(req.ID))
	x := h.Sum64()
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
	in, err := v.Program(ctx, req)
	if err != nil {
		return v, nil, fmt.Errorf("variant %s: %w", v.Name, err)
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
	e := p.entries[name]
	if e.status == Retired || e.status == Rejected {
		return nil // late outcomes for retired variants don't count
	}
	e.stats.n++
	for k, v := range metrics {
		e.stats.sums[k] += v
	}
	return nil
}

// Retirement is a challenger that was stopped, and what was done about it.
type Retirement struct {
	Variant  string
	Reason   string
	Decision string                 // the decision to rewind
	Rewind   *decision.RewindReport // set when AutoRewind ran
}

// Verdict is the result of an evaluation round.
type Verdict struct {
	Promoted  string // new champion, if any
	Dethroned string // old champion, if one was replaced
	Lift      float64
	Retired   []Retirement
	Waiting   []string // challengers without enough samples yet
}

func (v Verdict) String() string {
	var b strings.Builder
	if v.Promoted != "" {
		fmt.Fprintf(&b, "promoted %s over %s (%+.1f%% on objective)\n", v.Promoted, v.Dethroned, v.Lift)
	}
	for _, r := range v.Retired {
		fmt.Fprintf(&b, "retired %s: %s\n", r.Variant, r.Reason)
		if r.Rewind != nil {
			fmt.Fprintf(&b, "  rewound decision %s: %d intent(s) undone", r.Decision, len(r.Rewind.Undone))
			if len(r.Rewind.Stranded) > 0 {
				fmt.Fprintf(&b, ", %d stranded", len(r.Rewind.Stranded))
			}
			b.WriteString("\n")
		} else if r.Decision != "" {
			fmt.Fprintf(&b, "  suggest rewinding decision %s\n", r.Decision)
		}
	}
	for _, w := range v.Waiting {
		fmt.Fprintf(&b, "waiting on %s: not enough outcomes yet\n", w)
	}
	if b.Len() == 0 {
		return "no challengers\n"
	}
	return b.String()
}

// Evaluate judges every challenger with enough outcomes. Guardrail breaches
// retire (and optionally rewind); otherwise the challenger with the best
// lift past MinLiftPct becomes champion.
func (p *Population) Evaluate(ctx context.Context) (Verdict, error) {
	p.mu.Lock()
	p.rounds++
	round := p.rounds
	champ := p.champion
	var verdict Verdict
	type candidate struct {
		e    *entry
		lift float64
		ms   []counterfactual.MetricChange
	}
	var best *candidate
	var toRetire []Retirement
	var outcomes []decision.Outcome

	for _, e := range p.challengers() {
		if e.stats.n < p.opts.MinSamples || champ.stats.n < p.opts.MinSamples {
			verdict.Waiting = append(verdict.Waiting, e.v.Name)
			continue
		}
		ms := compare(champ.stats.means(), e.stats.means())
		if e.v.Decision != "" {
			outcomes = append(outcomes, decision.Outcome{
				ID:      fmt.Sprintf("eval-%d-%s", round, e.v.Name),
				Parents: []string{e.v.Decision},
				Summary: fmt.Sprintf("%s vs %s after %d outcomes", e.v.Name, champ.v.Name, e.stats.n),
				Metrics: e.stats.means(),
				Actor:   p.opts.Actor,
			})
		}
		var breaches []string
		for _, g := range p.opts.Guardrails {
			if msg := g.Check(ms); msg != "" {
				breaches = append(breaches, strings.Replace(msg, "predicted ", "observed ", 1))
			}
		}
		if len(breaches) > 0 {
			e.status, e.reason = Retired, strings.Join(breaches, "; ")
			toRetire = append(toRetire, Retirement{Variant: e.v.Name, Reason: e.reason, Decision: e.v.Decision})
			continue
		}
		lift := liftPct(champ.stats.mean(p.opts.Objective.Metric), e.stats.mean(p.opts.Objective.Metric), p.opts.Objective.Maximize)
		if lift >= p.opts.Objective.MinLiftPct && (best == nil || lift > best.lift) {
			best = &candidate{e: e, lift: lift, ms: ms}
		}
	}
	if best != nil {
		champ.status, champ.reason = Retired, "replaced by "+best.e.v.Name
		best.e.status = Champion
		p.champion = best.e
		verdict.Promoted, verdict.Dethroned, verdict.Lift = best.e.v.Name, champ.v.Name, best.lift
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
		if p.opts.AutoRewind && r.Decision != "" {
			rep, err := p.opts.Ledger.Rewind(ctx, r.Decision, p.opts.Undoer, "auto-rewind: "+r.Reason, p.opts.Actor)
			r.Rewind = &rep
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	verdict.Retired = toRetire
	sort.Strings(verdict.Waiting)
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
