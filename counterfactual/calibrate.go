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
	// SlopeLearned is false when the data couldn't distinguish the slope
	// from the model's own, so only a level shift is applied.
	SlopeLearned bool
	// Lo and Hi are the range of predictions the correction was fitted on.
	Lo, Hi float64
	// RMSEBefore and RMSEAfter are the root-mean-square error of raw and
	// corrected predictions on the data the correction was fitted to
	// (least squares minimises exactly this).
	RMSEBefore, RMSEAfter float64
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
		n := float64(len(ps))
		var mx, my float64
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range ps {
			mx += p[0]
			my += p[1]
			lo, hi = math.Min(lo, p[0]), math.Max(hi, p[0])
		}
		mx /= n
		my /= n
		var sxx, sxy float64
		for _, p := range ps {
			sxx += (p[0] - mx) * (p[0] - mx)
			sxy += (p[0] - mx) * (p[1] - my)
		}
		// Default: a pure level shift, keeping the model's own slope.
		corr := Correction{Metric: m, N: len(ps), Slope: 1, Intercept: my - mx, Lo: lo, Hi: hi}
		if sxx > 1e-12 && len(ps) > 2 {
			slope := sxy / sxx
			var ssr float64
			for _, p := range ps {
				r := p[1] - (my + slope*(p[0]-mx))
				ssr += r * r
			}
			se := math.Sqrt(ssr / (n - 2) / sxx)
			if se == 0 || math.Abs(slope-1)/se >= 2 {
				corr.Slope, corr.Intercept, corr.SlopeLearned = slope, my-slope*mx, true
			}
		}
		for _, p := range ps {
			before, after := p[0]-p[1], cal.clamp(m, corr.at(p[0]))-p[1]
			corr.RMSEBefore += before * before
			corr.RMSEAfter += after * after
		}
		corr.RMSEBefore = math.Sqrt(corr.RMSEBefore / n)
		corr.RMSEAfter = math.Sqrt(corr.RMSEAfter / n)
		cal.Corrections[m] = corr
	}
	return cal
}

// at applies the correction without extrapolating it: beyond the fitted
// range, the edge correction is held and the raw model's slope continues.
func (c Correction) at(x float64) float64 {
	edge := math.Max(c.Lo, math.Min(c.Hi, x))
	return c.Intercept + c.Slope*edge + (x - edge)
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
			pred[m] = cal.clamp(m, c.at(v))
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
		how := "level shift only (slope not distinguishable from the model's)"
		if c.SlopeLearned {
			how = fmt.Sprintf("observed ≈ %.3g + %.3g × predicted", c.Intercept, c.Slope)
		} else {
			how = fmt.Sprintf("observed ≈ predicted %+.3g; %s", c.Intercept, how)
		}
		fmt.Fprintf(&b, "%-8s n=%-6d %s, error (RMSE) %.3g → %.3g\n", m, c.N, how, c.RMSEBefore, c.RMSEAfter)
	}
	return b.String()
}
