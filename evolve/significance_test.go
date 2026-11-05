package evolve

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/PureMunky/reversible/counterfactual"
)

// noisy returns a revenue outcome around the variant's price with
// deterministic noise: each customer buys with probability 0.5.
func noisy(reqID string, price int) map[string]float64 {
	h := fnv.New64a()
	h.Write([]byte(reqID))
	if h.Sum64()%2 == 0 {
		return map[string]float64{"revenue": 0, "churn": 0}
	}
	return map[string]float64{"revenue": float64(price), "churn": 0}
}

func (f *fixture) serveNoisy(t *testing.T, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		req := counterfactual.Request{ID: fmt.Sprintf("n%05d", i)}
		v, _, err := f.pop.Serve(context.Background(), f.rt, req)
		if err != nil {
			t.Fatal(err)
		}
		f.pop.Observe(req.ID, noisy(req.ID, priceOf(v)))
	}
}

func TestSmallNoisyLiftNeedsEvidence(t *testing.T) {
	f := newFixture(t, false)
	f.serveNoisy(t, 0, 10)
	f.propose(t, "p42", priced(f.reg, 42)) // +5% on a coin-flip outcome

	f.serveNoisy(t, 10, 60) // ~30 each: far too noisy to tell
	v, _ := f.pop.Evaluate(context.Background())
	if v.Promoted != "" || len(v.Waiting) != 1 || !strings.Contains(v.Waiting[0].Note, "p=") {
		t.Fatalf("promoted on noise: %s", v)
	}
	if f.pop.Champion().Name != "p40" {
		t.Fatal("champion changed on noise")
	}
}

func TestLargeNoisyLiftIsPromotedOnceSignificant(t *testing.T) {
	f := newFixture(t, false)
	f.serveNoisy(t, 0, 10)
	f.propose(t, "p60", priced(f.reg, 60)) // +50% when they buy; price 60 is still under the churn line
	promoted := false
	for round := 0; round < 20 && !promoted; round++ {
		f.serveNoisy(t, 10+round*100, 100)
		v, _ := f.pop.Evaluate(context.Background())
		promoted = v.Promoted == "p60"
		if promoted && (v.P >= 0.05 || v.Lift < 40) {
			t.Fatalf("weak promotion: %s", v)
		}
	}
	if !promoted {
		t.Fatal("a 50% lift was never promoted")
	}
}

func TestSignificantlyWorseIsRetiredButNotRewound(t *testing.T) {
	f := newFixture(t, true) // AutoRewind on: should still not rewind
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "p20", priced(f.reg, 20)) // safe (no churn) but half the revenue
	f.serve(t, 10, 120, priceOf)
	v, err := f.pop.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Retired) != 1 || v.Retired[0].Kind != Worse || v.Retired[0].Rewind != nil {
		t.Fatalf("verdict: %s", v)
	}
	if n, _ := f.g.Get("decide-p20"); n.Rewound() {
		t.Fatal("a worse-but-safe variant was rewound")
	}
	at20 := 0
	for _, p := range f.w.price {
		if p == 20 {
			at20++
		}
	}
	if at20 == 0 {
		t.Fatal("customers served at $20 should keep their (valid) price")
	}
}

func TestInconclusiveAtMaxSamples(t *testing.T) {
	f := newFixture(t, false)
	f.pop.opts.MaxSamples = 40
	f.serveNoisy(t, 0, 10)
	f.propose(t, "p41", priced(f.reg, 41))
	f.serveNoisy(t, 10, 200)
	v, _ := f.pop.Evaluate(context.Background())
	if len(v.Retired) != 1 || v.Retired[0].Kind != Inconclusive {
		t.Fatalf("verdict: %s", v)
	}
}

func TestServedPredictionsFeedTheCalibrator(t *testing.T) {
	f := newFixture(t, false)
	f.pop.opts.Gate.Simulators = counterfactual.Simulators{
		"price.set": func(_ counterfactual.Request, e counterfactual.PlannedEffect) map[string]float64 {
			return map[string]float64{"churn": 0.01} // the model thinks nobody leaves
		},
	}
	c := counterfactual.NewCalibrator()
	c.MinPairs = 20
	f.pop.opts.Calibrator = c
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "p80", priced(f.reg, 80))
	f.serve(t, 10, 120, priceOf) // p80 customers all churn in reality

	cal := f.pop.Calibration()
	corr, ok := cal.Corrections["churn"]
	if !ok || corr.N != 130 {
		t.Fatalf("calibration: %+v", cal)
	}
	if corr.RMSEAfter >= corr.RMSEBefore {
		t.Fatalf("calibration didn't help: %+v", corr)
	}
}

// A challenger that was fine against the champion it launched against
// must not be rewound just because a later champion is much better.
func TestGuardrailsMeasureHarmAgainstTheLaunchBaseline(t *testing.T) {
	f := newFixture(t, true)
	f.serve(t, 0, 10, priceOf)
	f.propose(t, "pA", priced(f.reg, 41))
	f.propose(t, "pB", priced(f.reg, 42))
	// Hand-crafted outcomes: champion p40 churns 30%, A 25% (better), B 5%
	// and far more revenue. Noise keeps variances non-zero.
	metrics := map[string][2]float64{"p40": {50, 0.30}, "pA": {52, 0.25}, "pB": {80, 0.05}}
	observe := func(from, n int) {
		for i := from; i < from+n; i++ {
			req := counterfactual.Request{ID: fmt.Sprintf("g%05d", i)}
			v, _, err := f.pop.Serve(context.Background(), f.rt, req)
			if err != nil {
				t.Fatal(err)
			}
			m := metrics[v.Name]
			churn := 0.0
			if float64(i%100) < m[1]*100 {
				churn = 1
			}
			f.pop.Observe(req.ID, map[string]float64{"revenue": m[0] + float64(i%3), "churn": churn})
		}
	}
	observe(0, 300)
	v, _ := f.pop.Evaluate(context.Background())
	if v.Promoted != "pB" {
		t.Fatalf("setup: expected pB promoted: %s", v)
	}
	observe(300, 600)
	v, _ = f.pop.Evaluate(context.Background())
	if len(v.Retired) != 1 || v.Retired[0].Variant != "pA" {
		t.Fatalf("verdict: %s", v)
	}
	if v.Retired[0].Kind != Worse || v.Retired[0].Rewind != nil {
		t.Fatalf("pA should be retired as worse, not rewound as harmful: %s", v)
	}
}
