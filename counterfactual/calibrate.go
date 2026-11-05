package counterfactual

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// Predict returns what the simulators expect for one plan and request
// (raw, uncalibrated). Use it when an intent is actually served, so the
// prediction can later be paired with what really happened.
func Predict(sims Simulators, req Request, p Plan) map[string]float64 {
	sims.predict(req, &p)
	return p.Predicted
}

// Calibrator learns how wrong the simulators are. Each served request
// contributes a pair: what the simulators predicted for it, and what was
// observed. Fit produces a per-metric linear correction,
//
//	corrected = Intercept + Slope × predicted
//
// fitted by least squares. A model that is off by a constant factor, or
// systematically too optimistic at the extremes, gets pulled back toward
// reality as evidence accumulates.
type Calibrator struct {
	mu sync.Mutex
	// MinPairs is how many observations a metric needs before it is
	// corrected (default 30). Below that, predictions pass through.
	MinPairs int
	// Bounds clamps corrected values (for example churn to [0, 1]).
	Bounds map[string][2]float64
	pairs  map[string][][2]float64
}

// NewCalibrator returns an empty calibrator.
func NewCalibrator() *Calibrator { return &Calibrator{pairs: map[string][][2]float64{}} }

// Add records one prediction and the outcome that followed. Metrics that
// were observed but not predicted count as a prediction of zero.
func (c *Calibrator) Add(predicted, observed map[string]float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for m, obs := range observed {
		c.pairs[m] = append(c.pairs[m], [2]float64{predicted[m], obs})
	}
}

// Correction is the fitted adjustment for one metric.
type Correction struct {
	Metric           string
	N                int
	Intercept, Slope float64
	// MAEBefore and MAEAfter are the mean absolute error of raw and
	// corrected predictions on the data the correction was fitted to.
	MAEBefore, MAEAfter float64
}

// Calibration is a fitted set of corrections, ready to apply.
type Calibration struct {
	Corrections map[string]Correction
	Bounds      map[string][2]float64
}

// Fit computes a correction for every metric with enough pairs.
func (c *Calibrator) Fit() *Calibration {
	c.mu.Lock()
	defer c.mu.Unlock()
	min := c.MinPairs
	if min == 0 {
		min = 30
	}
	cal := &Calibration{Corrections: map[string]Correction{}, Bounds: c.Bounds}
	for m, ps := range c.pairs {
		if len(ps) < min {
			continue
		}
		var sx, sy, sxx, sxy float64
		for _, p := range ps {
			sx += p[0]
			sy += p[1]
			sxx += p[0] * p[0]
			sxy += p[0] * p[1]
		}
		n := float64(len(ps))
		corr := Correction{Metric: m, N: len(ps), Slope: 1}
		if den := n*sxx - sx*sx; math.Abs(den) > 1e-12 {
			corr.Slope = (n*sxy - sx*sy) / den
			corr.Intercept = (sy - corr.Slope*sx) / n
		} else {
			// All predictions identical: only a shift can be learned.
			corr.Intercept = (sy - sx) / n
		}
		for _, p := range ps {
			corr.MAEBefore += math.Abs(p[0] - p[1])
			corr.MAEAfter += math.Abs(cal.clamp(m, corr.Intercept+corr.Slope*p[0]) - p[1])
		}
		corr.MAEBefore /= n
		corr.MAEAfter /= n
		cal.Corrections[m] = corr
	}
	return cal
}

func (cal *Calibration) clamp(m string, v float64) float64 {
	if cal == nil {
		return v
	}
	if b, ok := cal.Bounds[m]; ok {
		return math.Max(b[0], math.Min(b[1], v))
	}
	return v
}

func (cal *Calibration) apply(pred map[string]float64) {
	if cal == nil {
		return
	}
	for m, v := range pred {
		if c, ok := cal.Corrections[m]; ok {
			pred[m] = cal.clamp(m, c.Intercept+c.Slope*v)
		}
	}
}

// String summarises how much each metric was corrected.
func (cal *Calibration) String() string {
	if cal == nil || len(cal.Corrections) == 0 {
		return "no calibration yet\n"
	}
	names := make([]string, 0, len(cal.Corrections))
	for m := range cal.Corrections {
		names = append(names, m)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, m := range names {
		c := cal.Corrections[m]
		fmt.Fprintf(&b, "%-10s n=%-5d observed ≈ %.3g + %.3g × predicted   mean error %.3g → %.3g\n",
			m, c.N, c.Intercept, c.Slope, c.MAEBefore, c.MAEAfter)
	}
	return b.String()
}
