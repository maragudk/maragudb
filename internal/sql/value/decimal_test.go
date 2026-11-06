package value_test

import (
	"fmt"
	"math/big"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/value"
)

func TestDecimalDiv(t *testing.T) {
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
			is.Equal(t, test.want, value.Format(value.DecimalDiv(n, d)))
		})
	}

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
			x, y := value.DecimalDiv(a, b), value.DecimalDiv(c, d)
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
