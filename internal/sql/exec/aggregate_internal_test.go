package exec

import (
	"math"
	"math/rand/v2"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"
)

// sumInOrder adds xs as Postgres's float8pl does for SUM, starting at the first value, or as
// float8_accum does for AVG, starting at 0 and also adding the squares of the deviations, and
// reports whether it overflows, which Postgres reports as an error.
func sumInOrder(xs []float64, avg bool) (float64, bool) {
	var sum, sxx float64
	for i, x := range xs {
		if !avg {
			if i == 0 {
				sum = x
				continue
			}
			r := sum + x
			if math.IsInf(r, 0) && !math.IsInf(sum, 0) && !math.IsInf(x, 0) {
				return 0, true
			}
			sum = r
			continue
		}
		n := float64(i + 1)
		old := sum
		sum += x
		if i > 0 {
			tmp := x*n - sum
			sxx += tmp * tmp / (n * float64(i))
			if math.IsInf(sum, 0) || math.IsInf(sxx, 0) {
				if !math.IsInf(old, 0) && !math.IsInf(x, 0) {
					return 0, true
				}
				sxx = math.NaN()
			}
		} else if math.IsNaN(x) || math.IsInf(x, 0) {
			sxx = math.NaN()
		}
	}
	return sum, false
}

func TestOrderFreeSum(t *testing.T) {
	t.Run("gives the sum of every order of the values when it accepts them, and none of the orders overflows", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			n := hegel.Draw(ht, hegel.Integers(1, 8))
			xs := make([]float64, n)
			for i := range xs {
				switch hegel.Draw(ht, hegel.Integers(0, 5)) {
				case 0:
					xs[i] = hegel.Draw(ht, hegel.Floats[float64]())
				case 1:
					xs[i] = math.Ldexp(float64(hegel.Draw(ht, hegel.Integers[int64](-1<<20, 1<<20))), hegel.Draw(ht, hegel.Integers(-1074, 1000)))
				case 2:
					xs[i] = hegel.Draw(ht, hegel.SampledFrom([]float64{math.Inf(1), math.Inf(-1), math.NaN(), 0, math.Copysign(0, -1), 1e308, math.MaxFloat64}))
				default:
					xs[i] = float64(hegel.Draw(ht, hegel.Integers[int64](-1<<53, 1<<53))) / float64(int64(1)<<hegel.Draw(ht, hegel.Integers(0, 4)))
				}
			}
			avg := hegel.Draw(ht, hegel.Booleans())
			sum, ok, _ := orderFreeSum(xs, avg)
			if !ok {
				return
			}
			rng := rand.New(rand.NewPCG(uint64(n), 1))
			for range 20 {
				perm := append([]float64(nil), xs...)
				rng.Shuffle(len(perm), func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
				got, overflow := sumInOrder(perm, avg)
				is.True(t, !overflow, "no order overflows")
				if math.IsNaN(sum) {
					is.True(t, math.IsNaN(got))
					continue
				}
				is.Equal(t, math.Float64bits(sum), math.Float64bits(got))
			}
		})
	})

	t.Run("rejects sums whose order matters, and accepts exact ones", func(t *testing.T) {
		tests := []struct {
			xs        []float64
			avg       bool
			ok        bool
			overflows bool
		}{
			{xs: []float64{0.1, 0.2, 0.3}},
			{xs: []float64{1e16, 1, -1e16}},
			{xs: []float64{1.5, 2.25, -0.75}, ok: true},
			{xs: []float64{1 << 52, 1 << 52}, ok: true},
			{xs: []float64{1 << 53, 1}},
			{xs: []float64{math.Inf(1), 1, 2}, ok: true},
			{xs: []float64{math.Inf(1), 1e308, 1e308}, overflows: true},
			{xs: []float64{1e308, 1e308}, overflows: true},
			{xs: []float64{1e200, -1e200}, avg: true, overflows: true},
			{xs: []float64{math.NaN(), 0.1, 0.2}, ok: true},
			{xs: []float64{math.Copysign(0, -1), math.Copysign(0, -1)}, ok: true},
		}
		for _, test := range tests {
			_, ok, overflows := orderFreeSum(test.xs, test.avg)
			is.Equal(t, test.ok, ok, test.xs)
			is.Equal(t, test.overflows, overflows, test.xs)
		}
	})
}
