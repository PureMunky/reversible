package evolve

import "math"

// welford tracks a running mean and variance (Welford's algorithm).
type welford struct {
	n    int
	mean float64
	m2   float64
}

func (w *welford) add(x float64) {
	w.n++
	d := x - w.mean
	w.mean += d / float64(w.n)
	w.m2 += d * (x - w.mean)
}

func (w *welford) variance() float64 {
	if w.n < 2 {
		return 0
	}
	return w.m2 / float64(w.n-1)
}

// pGreater is the one-sided p-value for "a's true mean is greater than
// b's", from Welch's t-test. Small values are evidence that a > b.
func pGreater(a, b *welford) float64 {
	if a == nil || b == nil || a.n < 2 || b.n < 2 {
		return 1
	}
	va, vb := a.variance()/float64(a.n), b.variance()/float64(b.n)
	se2 := va + vb
	diff := a.mean - b.mean
	if se2 == 0 {
		switch {
		case diff > 0:
			return 0
		case diff < 0:
			return 1
		default:
			return 0.5
		}
	}
	t := diff / math.Sqrt(se2)
	den := 0.0
	if va > 0 {
		den += va * va / float64(a.n-1)
	}
	if vb > 0 {
		den += vb * vb / float64(b.n-1)
	}
	df := se2 * se2 / den
	if df < 1 || math.IsNaN(df) || math.IsInf(df, 0) {
		df = 1
	}
	return tSurvival(t, df)
}

// tSurvival is P(T > t) for Student's t with df degrees of freedom.
func tSurvival(t, df float64) float64 {
	tail := 0.5 * betaInc(df/2, 0.5, df/(df+t*t)) // P(T > |t|)
	if t >= 0 {
		return tail
	}
	return 1 - tail
}

// betaInc is the regularized incomplete beta function I_x(a, b).
func betaInc(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(lab - la - lb + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return front * betaCF(a, b, x) / a
	}
	return 1 - front*betaCF(b, a, 1-x)/b
}

// betaCF evaluates the continued fraction for betaInc (modified Lentz).
func betaCF(a, b, x float64) float64 {
	const (
		maxIter = 300
		eps     = 3e-14
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}

// spend returns the significance threshold for the k-th look (k ≥ 1) at
// the data: 6α/(π²k²). These sum to α over all looks, so peeking after
// every batch can't inflate the overall false-positive rate (a union
// bound). It decays polynomially, so a real effect can still reach
// significance after many looks; halving per look (α/2, α/4, ...) makes
// that nearly impossible after ten or so.
func spend(alpha float64, k int) float64 {
	return 6 * alpha / (math.Pi * math.Pi * float64(k) * float64(k))
}
