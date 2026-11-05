package evolve

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"testing"

	"github.com/PureMunky/reversible"
	"github.com/PureMunky/reversible/counterfactual"
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
	for k := 1; k <= 10000; k++ {
		total += spend(0.05, k)
	}
	if total > 0.05 || total < 0.0499 {
		t.Fatalf("spent %v", total)
	}
	// Still usable after many looks: look 10 gets ~0.0003, not ~0.00005.
	if s := spend(0.05, 10); s < 0.0003 {
		t.Fatalf("look 10 threshold %v is too strict", s)
	}
}

// Assignment must be independent of other hashes of the same ID. With
// FNV routing, a customer trait derived from FNV(id+"#") was correlated
// with the variant a customer got, biasing every comparison.
func TestRoutingIndependentOfOtherHashesOfTheID(t *testing.T) {
	p, err := New(Variant{Name: "", Program: nil}, Options{Objective: Objective{Metric: "m"}})
	if err == nil {
		t.Fatal("expected error for a nameless champion")
	}
	p, _ = New(Variant{Name: "a", Program: func(context.Context, counterfactual.Request) (reversible.Intent, error) {
		return reversible.Intent{}, nil
	}}, Options{Objective: Objective{Metric: "m"}, ChallengerShare: 0.5})
	p.entries["b"] = &entry{v: Variant{Name: "b"}, status: Challenger, stats: newStats()}
	p.order = append(p.order, "b")

	// trait: the low bit of FNV(id + "#"), as a demo world might compute it.
	var n [2][2]int // [variant][trait]
	for i := 0; i < 20000; i++ {
		id := fmt.Sprintf("c%06d", i)
		h := fnv.New64a()
		h.Write([]byte(id + "#"))
		trait := int(h.Sum64() % 2)
		v := 0
		if p.Route(counterfactual.Request{ID: id}).Name == "b" {
			v = 1
		}
		n[v][trait]++
	}
	rateA := float64(n[0][1]) / float64(n[0][0]+n[0][1])
	rateB := float64(n[1][1]) / float64(n[1][0]+n[1][1])
	if math.Abs(rateA-rateB) > 0.03 {
		t.Fatalf("trait rate differs by variant: %.3f vs %.3f", rateA, rateB)
	}
}

func TestSaltMakesAssignmentReproducible(t *testing.T) {
	mk := func(salt string) *Population {
		p, _ := New(Variant{Name: "a", Program: func(context.Context, counterfactual.Request) (reversible.Intent, error) {
			return reversible.Intent{}, nil
		}}, Options{Objective: Objective{Metric: "m"}, ChallengerShare: 0.5, Salt: salt})
		p.entries["b"] = &entry{v: Variant{Name: "b"}, status: Challenger, stats: newStats()}
		p.order = append(p.order, "b")
		return p
	}
	x, y, z := mk("s1"), mk("s1"), mk("s2")
	same, diff := 0, 0
	for i := 0; i < 1000; i++ {
		req := counterfactual.Request{ID: fmt.Sprint(i)}
		if x.Route(req).Name == y.Route(req).Name {
			same++
		}
		if x.Route(req).Name != z.Route(req).Name {
			diff++
		}
	}
	if same != 1000 || diff < 400 {
		t.Fatalf("same=%d diff=%d", same, diff)
	}
}
