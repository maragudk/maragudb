package value_test

import (
	"math/big"
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

	t.Run("orders quotients as their exact values are ordered", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			a := big.NewInt(hegel.Draw(ht, hegel.Integers[int64](-1_000_000, 1_000_000)))
			b := big.NewInt(hegel.Draw(ht, hegel.Integers[int64](1, 1_000)))
			c := big.NewInt(hegel.Draw(ht, hegel.Integers[int64](-1_000_000, 1_000_000)))
			d := big.NewInt(hegel.Draw(ht, hegel.Integers[int64](1, 1_000)))
			// a/b < c/d exactly when a*d < c*b, and the rounded quotients keep a strict order of
			// values that differ by more than their rounding.
			exact := new(big.Int).Mul(a, d).Cmp(new(big.Int).Mul(c, b))
			got := value.Compare(value.DecimalDiv(a, b), value.DecimalDiv(c, d))
			if exact == 0 {
				is.Equal(t, 0, got)
			} else if got != 0 {
				is.Equal(t, exact, got)
			}
		})
	})
}
