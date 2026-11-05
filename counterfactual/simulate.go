package counterfactual

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Simulator predicts how the world would respond to one effect, as deltas
// to named metrics: a charge adds revenue and some churn risk, a purge
// removes an account, a discount email lifts conversion. It sees the
// request too, so predictions can depend on who the customer is.
//
// Simulators are models, and models are wrong in useful ways. Their job is
// to make a candidate's expected consequences visible before it runs, and
// to be compared later against outcomes the decision ledger observes.
type Simulator func(req Request, e PlannedEffect) map[string]float64

// Simulators maps effect types to their simulator. Effects with no
// simulator predict nothing.
type Simulators map[string]Simulator

func (s Simulators) predict(req Request, p *Plan) {
	if p.Err != "" {
		return
	}
	out := map[string]float64{}
	for _, e := range p.Effects {
		if sim, ok := s[e.Type]; ok {
			addInto(out, sim(req, e))
		}
	}
	p.Predicted = out
}

// MetricChange is a predicted total under baseline and candidate.
type MetricChange struct {
	Metric    string
	Baseline  float64
	Candidate float64
}

// Delta is candidate minus baseline.
func (m MetricChange) Delta() float64 { return m.Candidate - m.Baseline }

// Pct is the change as a percentage of the baseline (±Inf if the
// baseline is zero and the candidate isn't).
func (m MetricChange) Pct() float64 {
	if m.Baseline == 0 {
		if m.Candidate == 0 {
			return 0
		}
		return math.Copysign(math.Inf(1), m.Candidate)
	}
	return (m.Candidate - m.Baseline) / math.Abs(m.Baseline) * 100
}

func (m MetricChange) change() string {
	if m.Delta() == 0 {
		return "(no change)"
	}
	if math.IsInf(m.Pct(), 0) {
		return fmt.Sprintf("(%+.4g, new)", m.Delta())
	}
	return fmt.Sprintf("(%+.4g, %+.1f%%)", m.Delta(), m.Pct())
}

// Guardrail blocks promotion when a predicted total moves too far in the
// wrong direction.
type Guardrail struct {
	Metric string
	// LimitPct is the largest allowed move, as a percentage of baseline.
	LimitPct float64
	// Rise is true when the metric must not rise past the limit (churn,
	// refunds), false when it must not fall past it (revenue, accounts).
	Rise bool
}

// NoDropOver blocks if metric is predicted to fall more than pct percent.
func NoDropOver(metric string, pct float64) Guardrail {
	return Guardrail{Metric: metric, LimitPct: pct}
}

// NoRiseOver blocks if metric is predicted to rise more than pct percent.
func NoRiseOver(metric string, pct float64) Guardrail {
	return Guardrail{Metric: metric, LimitPct: pct, Rise: true}
}

func (g Guardrail) check(ms []MetricChange) string {
	for _, m := range ms {
		if m.Metric != g.Metric {
			continue
		}
		pct := m.Pct()
		if g.Rise && pct > g.LimitPct {
			return fmt.Sprintf("predicted %s rises %s (guardrail: at most +%g%%)", m.Metric, fmtPct(pct), g.LimitPct)
		}
		if !g.Rise && pct < -g.LimitPct {
			return fmt.Sprintf("predicted %s falls %s (guardrail: at most -%g%%)", m.Metric, fmtPct(pct), g.LimitPct)
		}
	}
	return ""
}

func fmtPct(p float64) string {
	if math.IsInf(p, 0) {
		return "from zero"
	}
	return fmt.Sprintf("%+.1f%%", p)
}

func addInto(dst, src map[string]float64) {
	for k, v := range src {
		dst[k] += v
	}
}

func delta(base, cand map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range cand {
		out[k] += v
	}
	for k, v := range base {
		out[k] -= v
	}
	for k, v := range out {
		if math.Abs(v) < 1e-9 {
			delete(out, k)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func metricChanges(base, cand map[string]float64) []MetricChange {
	names := map[string]bool{}
	for k := range base {
		names[k] = true
	}
	for k := range cand {
		names[k] = true
	}
	var out []MetricChange
	for k := range names {
		out = append(out, MetricChange{Metric: k, Baseline: base[k], Candidate: cand[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Metric < out[j].Metric })
	return out
}

func formatDelta(d map[string]float64) string {
	if len(d) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %+.4g", k, d[k])
	}
	return strings.Join(parts, ", ")
}
