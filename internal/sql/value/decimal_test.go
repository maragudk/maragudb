package value_test

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/value"
)

func TestDecimalQuo_integers(t *testing.T) {
	// The quotients are Postgres 18's, of n::numeric / d::numeric.
	tests := []struct{ n, d, want string }{
		{"5", "3", "1.6666666666666667"},
		{"2", "2", "1.00000000000000000000"},
		{"1", "3", "0.33333333333333333333"},
		{"0", "7", "0.00000000000000000000"},
		{"-1", "3", "-0.33333333333333333333"},
		{"-5", "3", "-1.6666666666666667"},
		{"10000", "3", "3333.3333333333333333"},
		{"9999", "3", "3333.0000000000000000"},
		{"15000", "10000", "1.50000000000000000000"},
		{"3", "2", "1.5000000000000000"},
		{"1", "10000", "0.000100000000000000000000"},
		{"123456789012345678", "7", "17636684144620811"},
		{"18446744073709551614", "2", "9223372036854775807"},
		{"-18446744073709551615", "3", "-6148914691236517205"},
		{"7", "7", "1.00000000000000000000"},
		{"9223372036854775807", "1", "9223372036854775807"},
		{"-1", "200000", "-0.000005000000000000000000"},
		{"1", "30000000", "0.000000033333333333333333"},
		{"999999999999", "1000000000000", "0.99999999999900000000"},
		{"5", "10", "0.50000000000000000000"},
	}
	for _, test := range tests {
		t.Run(test.n+"/"+test.d, func(t *testing.T) {
			n, _ := new(big.Int).SetString(test.n, 10)
			d, _ := new(big.Int).SetString(test.d, 10)
			is.Equal(t, test.want, value.Format(must(value.DecimalQuo(value.DecimalFromBig(n), value.DecimalFromBig(d)))))
		})
	}

}

func TestDecimalArithmetic(t *testing.T) {
	// The results are Postgres 18's, of a::numeric op b::numeric, where dz is division by zero.
	tests := []struct{ a, b, sum, diff, prod, quo, rem string }{
		{"1.5", "2.25", "3.75", "-0.75", "3.375", "0.66666666666666666667", "1.50"},
		{"-1.50", "1.5", "0.00", "-3.00", "-2.250", "-1.00000000000000000000", "0.00"},
		{"0.00", "-3", "-3.00", "3.00", "0.00", "0.00000000000000000000", "0.00"},
		{"12.3400", "0.0066", "12.3466", "12.3334", "0.08144400", "1869.6969696969696970", "0.0046"},
		{"-7", "3", "-4", "-10", "-21", "-2.3333333333333333", "-1"},
		{"7", "-3", "4", "10", "-21", "-2.3333333333333333", "1"},
		{"-7.5", "2", "-5.5", "-9.5", "-15.0", "-3.7500000000000000", "-1.5"},
		{"1", "0.0003", "1.0003", "0.9997", "0.0003", "3333.3333333333333333", "0.0001"},
		{"0.00001", "3", "3.00001", "-2.99999", "0.00003", "0.000003333333333333333333", "0.00001"},
		{"99999999", "0.0001", "99999999.0001", "99999998.9999", "9999.9999", "999999990000.00000000", "0.0000"},
		{"-0.5", "0.00", "-0.50", "-0.50", "0.000", "dz", "dz"},
		{"1.00000000000000000000", "3", "4.00000000000000000000", "-2.00000000000000000000", "3.00000000000000000000", "0.33333333333333333333", "1.00000000000000000000"},
		{"2.5000000000000000", "-0.00000001", "2.4999999900000000", "2.5000000100000000", "-0.000000025000000000000000", "-250000000.0000000000000000", "0.0000000000000000"},
		{"123456789012345678901234567890", "0.000000001", "123456789012345678901234567890.000000001", "123456789012345678901234567889.999999999", "123456789012345678901.234567890", "123456789012345678901234567890000000000.000000000", "0.000000000"},
		{"0.000000000000000000000000000001", "7", "7.000000000000000000000000000001", "-6.999999999999999999999999999999", "0.000000000000000000000000000007", "0.000000000000000000000000000000142857142857142857", "0.000000000000000000000000000001"},
		{"5", "0.3", "5.3", "4.7", "1.5", "16.6666666666666667", "0.2"},
		{"-5", "0.3", "-4.7", "-5.3", "-1.5", "-16.6666666666666667", "-0.2"},
		{"1", "-8", "-7", "9", "-8", "-0.12500000000000000000", "1"},
		{"10000", "10000.0000", "20000.0000", "0.0000", "100000000.0000", "1.00000000000000000000", "0.0000"},
		{"9999", "10000", "19999", "-1", "99990000", "0.99990000000000000000", "9999"},
		{"0", "0.5", "0.5", "-0.5", "0.0", "0.0000000000000000", "0.0"},
	}
	result := func(v value.Value, err error) string {
		if errors.Is(err, value.ErrDivisionByZero) {
			return "dz"
		}
		if err != nil {
			return err.Error()
		}
		return value.Format(v)
	}
	for _, test := range tests {
		t.Run(test.a+" and "+test.b, func(t *testing.T) {
			a, b := value.NewDecimal(test.a), value.NewDecimal(test.b)
			is.Equal(t, test.sum, result(value.DecimalAdd(a, b)), "+")
			is.Equal(t, test.diff, result(value.DecimalSub(a, b)), "-")
			is.Equal(t, test.prod, result(value.DecimalMul(a, b)), "*")
			is.Equal(t, test.quo, result(value.DecimalQuo(a, b)), "/")
			is.Equal(t, test.rem, result(value.DecimalRem(a, b)), "%")
		})
	}

	t.Run("rounds a quotient half away from zero", func(t *testing.T) {
		d := value.NewDecimal
		is.Equal(t, "61728394506172839451", result(value.DecimalQuo(d("123456789012345678901"), d("2"))))
		is.Equal(t, "-61728394506172839451", result(value.DecimalQuo(d("-123456789012345678901"), d("2"))))
		is.Equal(t, "-61728394506172839451", result(value.DecimalQuo(d("123456789012345678901"), d("-2"))))
		is.Equal(t, "308641972530864197253", result(value.DecimalQuo(d("1234567890123456789012"), d("4"))))
	})

	t.Run("gives a quotient at most 1000 digits after the point, and rounds it to zero without a sign", func(t *testing.T) {
		for _, a := range []string{"0." + strings.Repeat("0", 1100) + "3", "-0." + strings.Repeat("0", 1100) + "3"} {
			is.Equal(t, "0."+strings.Repeat("0", 1000), result(value.DecimalQuo(value.NewDecimal(a), value.NewDecimal("7"))))
		}
	})

	t.Run("rounds a product half away from zero to 16383 digits after the point", func(t *testing.T) {
		tiny := func(digits string) value.Value { return value.NewDecimal("0." + strings.Repeat("0", 8190) + digits) }
		got := result(value.DecimalMul(tiny("5"), tiny("1")))
		is.Equal(t, "0."+strings.Repeat("0", 16381)+"5", got, "exact")
		got = result(value.DecimalMul(tiny("5"), tiny("0999")))
		is.Equal(t, "0."+strings.Repeat("0", 16381)+"50", got, "rounded up from 49.95")
		got = result(value.DecimalMul(value.DecimalNeg(tiny("5")), tiny("0999")))
		is.Equal(t, "-0."+strings.Repeat("0", 16381)+"50", got, "rounded down from -49.95")
		got = result(value.DecimalMul(value.DecimalNeg(tiny("01")), tiny("01")))
		is.Equal(t, "0."+strings.Repeat("0", 16383), got, "rounded to zero without a sign")
	})

	t.Run("fails with more than 131072 digits before the point", func(t *testing.T) {
		big := value.NewDecimal("9" + strings.Repeat("0", 131071))
		v, err := value.DecimalAdd(big, value.NewDecimal("1"))
		is.NotError(t, err)
		is.Equal(t, 131072, len(v.S))
		_, err = value.DecimalMul(big, value.NewDecimal("10"))
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalAdd(value.NewDecimal(strings.Repeat("9", 131072)), value.NewDecimal("1"))
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalSub(value.NewDecimal("-"+strings.Repeat("9", 131072)), value.NewDecimal("0.1"))
		is.NotError(t, err)
		_, err = value.DecimalSub(value.NewDecimal("-"+strings.Repeat("9", 131072)), value.NewDecimal("1"))
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalQuo(big, value.NewDecimal("0.01"))
		is.Error(t, value.ErrOutOfRange, err)
	})

	t.Run("negates without changing the scale, and zero stays without a sign", func(t *testing.T) {
		d := value.NewDecimal
		is.Equal(t, "-1.50", value.Format(value.DecimalNeg(d("1.50"))))
		is.Equal(t, "1.50", value.Format(value.DecimalNeg(d("-1.50"))))
		is.Equal(t, "0.00", value.Format(value.DecimalNeg(d("0.00"))))
		is.Equal(t, "0", value.Format(value.DecimalNeg(d("0"))))
	})

	t.Run("adds, subtracts, and multiplies exactly, divides to within half a unit of the last digit rounding half away from zero, and takes the remainder of the truncated quotient", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			a, b := drawDecimal(ht), drawDecimal(ht)
			ht.Note(fmt.Sprintf("a = %s, b = %s", a.S, b.S))
			x, y := rat(ht, a), rat(ht, b)
			check := func(name string, v value.Value, err error, scale int, want *big.Rat) {
				if err != nil {
					ht.Fatalf("%s: %v", name, err)
				}
				if got := scaleOf(v.S); got != scale {
					ht.Fatalf("%s is %s, of scale %d, want %d", name, v.S, got, scale)
				}
				if strings.HasPrefix(v.S, "-") && rat(ht, v).Sign() == 0 {
					ht.Fatalf("%s is %s, a zero with a sign", name, v.S)
				}
				if want != nil && rat(ht, v).Cmp(want) != 0 {
					ht.Fatalf("%s is %s, want %s", name, v.S, want.FloatString(scale))
				}
			}
			sa, sb := scaleOf(a.S), scaleOf(b.S)
			v, err := value.DecimalAdd(a, b)
			check("a + b", v, err, max(sa, sb), new(big.Rat).Add(x, y))
			v, err = value.DecimalSub(a, b)
			check("a - b", v, err, max(sa, sb), new(big.Rat).Sub(x, y))
			v, err = value.DecimalMul(a, b)
			check("a * b", v, err, sa+sb, new(big.Rat).Mul(x, y))
			check("-a", value.DecimalNeg(a), nil, sa, new(big.Rat).Neg(x))
			if y.Sign() == 0 {
				_, err := value.DecimalQuo(a, b)
				is.Error(ht, value.ErrDivisionByZero, err)
				_, err = value.DecimalRem(a, b)
				is.Error(ht, value.ErrDivisionByZero, err)
				return
			}
			q, err := value.DecimalQuo(a, b)
			if err != nil {
				ht.Fatalf("a / b: %v", err)
			}
			exact := new(big.Rat).Quo(x, y)
			scale := scaleOf(q.S)
			if scale < max(sa, sb) || scale > max(sa, sb, 1000) {
				ht.Fatalf("a / b is %s, of scale %d", q.S, scale)
			}
			unit := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
			// Half away from zero: |q| is |exact| rounded to the unit, where a half rounds up.
			mag := new(big.Rat).Quo(new(big.Rat).Abs(exact), unit)
			mag.Add(mag, big.NewRat(1, 2))
			floor := new(big.Int).Quo(mag.Num(), mag.Denom())
			want := new(big.Rat).Mul(new(big.Rat).SetInt(floor), unit)
			if exact.Sign() < 0 {
				want.Neg(want)
			}
			check("a / b", q, nil, scale, want)
			// The remainder is a - trunc(a / b) * b.
			trunc := new(big.Int).Quo(exact.Num(), exact.Denom())
			v, err = value.DecimalRem(a, b)
			check("a % b", v, err, max(sa, sb), new(big.Rat).Sub(x, new(big.Rat).Mul(new(big.Rat).SetInt(trunc), y)))
		}, hegel.WithTestCases(2000))
	})
}

// drawDecimal is a numeric of up to 40 digits, with up to 25 of them after the point, often
// with trailing zeros, and now and then zero.
func drawDecimal(ht *hegel.T) value.Value {
	digits := hegel.Draw(ht, hegel.OneOf(
		hegel.Text().Alphabet("0123456789").MinSize(1).MaxSize(40),
		hegel.SampledFrom([]string{"0", "1", "5", "9999", "10000", "100000000", "99999999999999999999"}),
	))
	scale := hegel.Draw(ht, hegel.Integers(0, min(25, len(digits))))
	digits = strings.TrimLeft(digits[:len(digits)-scale], "0") + "." + digits[len(digits)-scale:]
	s := strings.TrimSuffix(digits, ".")
	if strings.HasPrefix(s, ".") || s == "" {
		s = "0" + s
	}
	if hegel.Draw(ht, hegel.Booleans()) && strings.Trim(s, "0.") != "" {
		s = "-" + s
	}
	return value.NewDecimal(s)
}

// rat is the exact value of a numeric.
func rat(ht *hegel.T, v value.Value) *big.Rat {
	r, ok := new(big.Rat).SetString(v.S)
	if !ok {
		ht.Fatalf("bad numeric text %q", v.S)
	}
	return r
}

func must(v value.Value, err error) value.Value {
	if err != nil {
		panic(err)
	}
	return v
}

// scaleOf the text of a numeric: its digits after the point.
func scaleOf(s string) int {
	_, frac, _ := strings.Cut(s, ".")
	return len(frac)
}

func TestDecimalToInt(t *testing.T) {
	// Postgres 18 rounds half away from zero.
	tests := []struct {
		s    string
		want int64
	}{
		{"2.5", 3}, {"-2.5", -3}, {"2.4999", 2}, {"0.5", 1}, {"-0.5", -1}, {"9223372036854775807.4", 9223372036854775807}, {"0.00000", 0},
	}
	for _, test := range tests {
		t.Run(test.s, func(t *testing.T) {
			got, err := value.DecimalToInt(test.s)
			is.NotError(t, err)
			is.Equal(t, test.want, got)
		})
	}

	t.Run("fails beyond the range of bigint", func(t *testing.T) {
		_, err := value.DecimalToInt("9223372036854775807.5")
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalToInt("-9223372036854775809")
		is.Error(t, value.ErrOutOfRange, err)
	})
}

func TestDecimalToFloat(t *testing.T) {
	t.Run("rounds to the nearest double", func(t *testing.T) {
		f, err := value.DecimalToFloat("0.1000000000000000055511151231257827")
		is.NotError(t, err)
		is.Equal(t, 0.1, f)
		f, err = value.DecimalToFloat("-1.6666666666666667")
		is.NotError(t, err)
		is.Equal(t, -1.6666666666666667, f)
	})

	t.Run("fails when the double is infinite, or zero for a numeric that is not, as Postgres's float8in", func(t *testing.T) {
		// Postgres 18 reads 2.4703282292062328e-324 as 5e-324 and fails on
		// 2.4703282292062327e-324, which is just below half the smallest subnormal.
		half := "0." + strings.Repeat("0", 323)
		f, err := value.DecimalToFloat(half + "24703282292062328")
		is.NotError(t, err)
		is.Equal(t, 5e-324, f)
		_, err = value.DecimalToFloat(half + "24703282292062327")
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalToFloat("-" + half + "1")
		is.Error(t, value.ErrOutOfRange, err)
		_, err = value.DecimalToFloat("1" + strings.Repeat("0", 309))
		is.Error(t, value.ErrOutOfRange, err)
		f, err = value.DecimalToFloat("0." + strings.Repeat("0", 400))
		is.NotError(t, err)
		is.Equal(t, 0.0, f)
	})

	t.Run("rounds every numeric to the nearest double, ties to even, as a correctly rounded strtod", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			var s string
			if hegel.Draw(ht, hegel.Booleans()) {
				// Near the midpoint of two adjacent doubles, which is an exact decimal.
				f := math.Abs(hegel.Draw(ht, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false)))
				mid := new(big.Rat).Add(new(big.Rat).SetFloat64(f), new(big.Rat).SetFloat64(math.Nextafter(f, 0)))
				mid.Quo(mid, big.NewRat(2, 1))
				switch hegel.Draw(ht, hegel.Integers(0, 2)) {
				case 1:
					mid.Add(mid, new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(1200), nil)))
				case 2:
					mid.Sub(mid, new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Exp(big.NewInt(10), big.NewInt(1200), nil)))
				}
				s = mid.FloatString(1200)
			} else {
				s = value.Format(drawDecimal(ht))
				if exp := hegel.Draw(ht, hegel.Integers(-340, 320)); exp < 0 {
					s = value.Format(must(value.DecimalMul(value.NewDecimal(s), value.NewDecimal("0."+strings.Repeat("0", -exp-1)+"1"))))
				} else {
					s = value.Format(must(value.DecimalMul(value.NewDecimal(s), value.NewDecimal("1"+strings.Repeat("0", exp)))))
				}
			}
			ht.Note(s)
			want, _ := rat(ht, value.NewDecimal(s)).Float64()
			got, err := value.DecimalToFloat(s)
			switch {
			case math.IsInf(want, 0) || want == 0 && strings.Trim(s, "-0.") != "":
				is.Error(ht, value.ErrOutOfRange, err)
			case err != nil:
				ht.Fatalf("%s: %v", s, err)
			case math.Float64bits(got) != math.Float64bits(want) && !(got == 0 && want == 0):
				ht.Fatalf("%s is %v, want %v", s, got, want)
			}
		}, hegel.WithTestCases(2000))
	})
}

func TestCompare_decimal(t *testing.T) {
	t.Run("compares numerics by value, whatever their scale", func(t *testing.T) {
		d := value.NewDecimal
		is.Equal(t, 0, value.Compare(d("1.00000000000000000000"), d("1")))
		is.Equal(t, -1, value.Compare(d("-1.5"), d("1")))
		is.Equal(t, 1, value.Compare(d("-1.5"), d("-2")))
		is.Equal(t, -1, value.Compare(d("0.33333333333333333333"), d("0.3333333333333333334")))
		is.Equal(t, 1, value.Compare(d("10"), d("9.99")))
		is.Equal(t, 0, value.Compare(d("0.000"), d("0")))
		is.Equal(t, value.DecimalKey("2.50"), value.DecimalKey("2.5"))
	})

	t.Run("rounds every quotient to within half a unit of its last digit, and orders quotients as their exact values", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			draw := func() (*big.Int, *big.Int) {
				n := big.NewInt(hegel.Draw(ht, hegel.Integers[int64](-1<<62, 1<<62)))
				n.Mul(n, big.NewInt(hegel.Draw(ht, hegel.Integers[int64](1, 1<<20))))
				return n, big.NewInt(hegel.Draw(ht, hegel.Integers[int64](1, 1<<40)))
			}
			a, b := draw()
			c, d := draw()
			x, y := must(value.DecimalQuo(value.DecimalFromBig(a), value.DecimalFromBig(b))), must(value.DecimalQuo(value.DecimalFromBig(c), value.DecimalFromBig(d)))
			ht.Note(fmt.Sprintf("%v/%v = %s, %v/%v = %s", a, b, x.S, c, d, y.S))
			for _, q := range []struct {
				n, d *big.Int
				v    value.Value
			}{{a, b, x}, {c, d, y}} {
				got, ok := new(big.Rat).SetString(q.v.S)
				if !ok {
					ht.Fatalf("bad text %q", q.v.S)
				}
				scale := 0
				if _, frac, ok := strings.Cut(q.v.S, "."); ok {
					scale = len(frac)
				}
				diff := new(big.Rat).Sub(got, new(big.Rat).SetFrac(q.n, q.d))
				half := new(big.Rat).SetFrac(big.NewInt(1), new(big.Int).Mul(big.NewInt(2), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
				if diff.Abs(diff).Cmp(half) > 0 {
					ht.Fatalf("%v/%v is %s, off by more than half a unit", q.n, q.d, q.v.S)
				}
				if digits := len(strings.TrimLeft(strings.NewReplacer("-", "", ".", "").Replace(q.v.S), "0")); got.Sign() != 0 && digits < 16 {
					ht.Fatalf("%s has %d significant digits, fewer than 16", q.v.S, digits)
				}
			}
			exact := new(big.Int).Mul(a, d).Cmp(new(big.Int).Mul(c, b))
			if got := value.Compare(x, y); exact == 0 && got != 0 || exact != 0 && got != 0 && got != exact {
				ht.Fatalf("exact order %d, compared %d", exact, got)
			}
		})
	})
}
