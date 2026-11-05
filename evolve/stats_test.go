package evolve

import (
	"math"
	"testing"
)

func TestTSurvivalKnownValues(t *testing.T) {
	cases := []struct{ t, df, want float64 }{
		{0, 10, 0.5},
		{2.228, 10, 0.025},  // two-sided 5% critical value
		{1.96, 1e6, 0.025},  // ~normal
		{-2.228, 10, 0.975}, // symmetric
		{3.0, 5, 0.01505},
	}
	for _, c := range cases {
		if got := tSurvival(c.t, c.df); math.Abs(got-c.want) > 5e-4 {
			t.Errorf("tSurvival(%v, %v) = %v, want %v", c.t, c.df, got, c.want)
		}
	}
}

func TestWelfordAndPGreater(t *testing.T) {
	a, b := &welford{}, &welford{}
	for i := 0; i < 200; i++ {
		a.add(float64(i%10) + 1) // 1..10, mean 5.5
		b.add(float64(i % 10))   // 0..9, mean 4.5; t ≈ 3.47
	}
	if math.Abs(a.mean-5.5) > 1e-9 || math.Abs(a.variance()-8.291) > 0.01 {
		t.Fatalf("welford: mean %v var %v", a.mean, a.variance())
	}
	if p := pGreater(a, b); p > 0.001 || p < 0.0001 {
		t.Fatalf("clearly greater, p = %v", p)
	}
	if p := pGreater(b, a); p < 0.999 {
		t.Fatalf("clearly smaller, p = %v", p)
	}
	if p := pGreater(a, a); math.Abs(p-0.5) > 1e-9 {
		t.Fatalf("identical, p = %v", p)
	}
}

func TestAlphaSpendingSumsToAlpha(t *testing.T) {
	total := 0.0
	for k := 1; k <= 60; k++ {
		total += spend(0.05, k)
	}
	if total > 0.05 || total < 0.0499 {
		t.Fatalf("spent %v", total)
	}
}
