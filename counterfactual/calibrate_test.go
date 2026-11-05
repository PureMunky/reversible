package counterfactual

import (
	"context"
	"math"
	"strings"
	"testing"
)

func TestCalibratorLearnsLinearBias(t *testing.T) {
	c := NewCalibrator()
	// Reality: churn is 4x the model's prediction, minus 0.1.
	for i := 0; i < 50; i++ {
		p := 0.04 + float64(i)*0.002
		c.Add(map[string]float64{"churn": p}, map[string]float64{"churn": 4*p - 0.1})
	}
	cal := c.Fit()
	corr := cal.Corrections["churn"]
	if math.Abs(corr.Slope-4) > 1e-9 || math.Abs(corr.Intercept+0.1) > 1e-9 {
		t.Fatalf("fit: %+v", corr)
	}
	if corr.RMSEAfter > 1e-9 || corr.RMSEBefore < 0.1 {
		t.Fatalf("errors: %+v", corr)
	}
	if !corr.SlopeLearned || !strings.Contains(cal.String(), "observed ≈ -0.1 + 4 × predicted") {
		t.Fatalf("string: %s", cal)
	}
}

func TestCalibratorNeedsEnoughPairsAndClamps(t *testing.T) {
	c := NewCalibrator()
	c.MinPairs = 10
	c.Bounds = map[string][2]float64{"churn": {0, 1}}
	for i := 0; i < 5; i++ {
		c.Add(map[string]float64{"churn": 0.1}, map[string]float64{"churn": 0.5})
	}
	if len(c.Fit().Corrections) != 0 {
		t.Fatal("fitted with too few pairs")
	}
	for i := 0; i < 5; i++ {
		c.Add(map[string]float64{"churn": 0.1}, map[string]float64{"churn": 0.5})
	}
	cal := c.Fit()
	pred := map[string]float64{"churn": 0.9}
	cal.apply(pred) // identical predictions: shift of +0.4 -> 1.3, clamped to 1
	if pred["churn"] != 1 {
		t.Fatalf("clamped = %v", pred["churn"])
	}
}

func TestCalibrationChangesTheGateVerdict(t *testing.T) {
	reg := registry()
	base := pricing(reg, func(t string) int { return prices[t] }, false)
	cand := pricing(reg, func(t string) int {
		if t == "team" {
			return 299
		}
		return prices[t]
	}, false)
	cfg := Config{Requests: sensitiveTraffic, Baseline: base, Candidate: cand, Simulators: sims,
		Guardrails: []Guardrail{NoDropOver("revenue", 5)}}

	// Raw model: raising the team price looks good for revenue.
	if rep := ReplayWith(context.Background(), cfg); !rep.Promote() {
		t.Fatalf("raw model should pass:\n%s", rep)
	}

	// Observed reality: revenue is far more sensitive to price than modelled.
	c := NewCalibrator()
	for i := 0; i < 40; i++ {
		p := 100 + float64(i)*5
		c.Add(map[string]float64{"revenue": p}, map[string]float64{"revenue": 900 - 3*p})
	}
	cfg.Calibration = c.Fit()
	rep := ReplayWith(context.Background(), cfg)
	if rep.Promote() || !strings.Contains(strings.Join(rep.Blockers, ";"), "predicted revenue falls") {
		t.Fatalf("calibrated model should block:\n%s", rep)
	}
}

func TestNoisySlopeFallsBackToLevelShift(t *testing.T) {
	c := NewCalibrator()
	// Predictions vary (by a feature that doesn't matter); outcomes don't
	// depend on them at all. Only the level is learnable.
	var sumPred, sumObs float64
	for i := 0; i < 210; i++ {
		pred := 0.05 + float64(i%10)*0.001
		obs := 0.0
		if i%7 == 0 { // independent of the prediction (every pred value gets the same churn rate)
			obs = 1
		}
		sumPred += pred
		sumObs += obs
		c.Add(map[string]float64{"churn": pred}, map[string]float64{"churn": obs})
	}
	corr := c.Fit().Corrections["churn"]
	if corr.SlopeLearned || corr.Slope != 1 {
		t.Fatalf("learned a slope from noise: %+v", corr)
	}
	if want := (sumObs - sumPred) / 210; math.Abs(corr.Intercept-want) > 1e-9 {
		t.Fatalf("shift = %v, want %v", corr.Intercept, want)
	}
}

func TestCorrectionIsNotExtrapolated(t *testing.T) {
	c := NewCalibrator()
	for i := 0; i < 50; i++ {
		p := 0.05 + float64(i)*0.0002 // predictions in [0.05, 0.0598]
		c.Add(map[string]float64{"churn": p}, map[string]float64{"churn": 10 * p})
	}
	cal := c.Fit()
	inside := map[string]float64{"churn": 0.055}
	far := map[string]float64{"churn": 0.5}
	cal.apply(inside)
	cal.apply(far)
	if math.Abs(inside["churn"]-0.55) > 1e-9 {
		t.Fatalf("inside range = %v", inside["churn"])
	}
	// At the edge (0.0598) the correction gives 0.598; beyond it the raw
	// slope (1) continues: 0.598 + (0.5 - 0.0598).
	if want := 0.598 + 0.5 - 0.0598; math.Abs(far["churn"]-want) > 1e-9 {
		t.Fatalf("extrapolated = %v, want %v", far["churn"], want)
	}
}
