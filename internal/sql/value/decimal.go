package value

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// A value of type [Decimal] holds an exact decimal number in S, in the text that Postgres outputs
// for it: an optional minus sign, the integer digits without leading zeros, and the digits of
// its scale after a point, trailing zeros included, such as -12.3400. Zero has no sign.

// NewDecimal value from its text, which must be in the form [Decimal] values have.
func NewDecimal(s string) Value { return Value{Type: Decimal, S: s} }

// DecimalFromInt is the numeric of an integer, of scale 0.
func DecimalFromInt(i int64) Value { return NewDecimal(strconv.FormatInt(i, 10)) }

// DecimalFromBig is the numeric of an integer, of scale 0.
func DecimalFromBig(n *big.Int) Value { return NewDecimal(n.String()) }

// Postgres's numeric stores its digits in base 10000, and chooses the scale of a quotient from
// the weights of the digits of its operands.
const (
	decDigits        = 4
	nbase            = 10000
	minSigDigits     = 16
	maxDisplayScale  = 1000
	minDisplayScale  = 0
	decimalSignMinus = '-'
)

// DecimalDiv is the quotient of two integers as numeric, as Postgres's numeric division
// computes it: rounded half away from zero to the scale that select_div_scale picks, which
// gives at least 16 significant digits. The divisor must not be zero.
func DecimalDiv(dividend, divisor *big.Int) Value {
	w1, d1 := baseWeight(dividend)
	w2, d2 := baseWeight(divisor)
	// The weight of the quotient, assuming the dividend is the smaller when the first digits
	// are equal.
	qweight := w1 - w2
	if d1 <= d2 {
		qweight--
	}
	scale := minSigDigits - qweight*decDigits
	scale = max(scale, minDisplayScale)
	scale = min(scale, maxDisplayScale)

	num := new(big.Int).Abs(dividend)
	num.Mul(num, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil))
	den := new(big.Int).Abs(divisor)
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	// Half away from zero.
	if r.Lsh(r, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	negative := (dividend.Sign() < 0) != (divisor.Sign() < 0) && q.Sign() != 0
	return NewDecimal(decimalText(q, scale, negative))
}

// baseWeight of an integer's first digit in base 10000, which is 0 for the units, and that
// digit. Zero has weight 0 and first digit 0.
func baseWeight(n *big.Int) (int, int64) {
	a := new(big.Int).Abs(n)
	if a.Sign() == 0 {
		return 0, 0
	}
	digits := len(a.String())
	weight := (digits - 1) / decDigits
	first := new(big.Int).Quo(a, new(big.Int).Exp(big.NewInt(nbase), big.NewInt(int64(weight)), nil))
	return weight, first.Int64()
}

// decimalText of the unscaled magnitude q with scale digits after the point.
func decimalText(q *big.Int, scale int, negative bool) string {
	digits := q.String()
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	var b strings.Builder
	if negative {
		b.WriteByte(decimalSignMinus)
	}
	b.WriteString(digits[:len(digits)-scale])
	if scale > 0 {
		b.WriteByte('.')
		b.WriteString(digits[len(digits)-scale:])
	}
	return b.String()
}

// decimalParts of the text of a numeric: its sign, its integer digits without leading zeros,
// and its fraction digits without trailing zeros.
func decimalParts(s string) (bool, string, string) {
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	frac = strings.TrimRight(frac, "0")
	if whole == "" && frac == "" {
		negative = false
	}
	return negative, whole, frac
}

// compareDecimals by their values, so that numerics that differ only in their scale are equal,
// as Postgres compares them.
func compareDecimals(a, b string) int {
	an, aw, af := decimalParts(a)
	bn, bw, bf := decimalParts(b)
	if an != bn {
		if an {
			return -1
		}
		return 1
	}
	c := compareMagnitudes(aw, af, bw, bf)
	if an {
		return -c
	}
	return c
}

func compareMagnitudes(aw, af, bw, bf string) int {
	if len(aw) != len(bw) {
		return cmp(len(aw), len(bw))
	}
	if c := strings.Compare(aw, bw); c != 0 {
		return c
	}
	return strings.Compare(af, bf)
}

// DecimalKey is a text that two numerics have in common exactly when they are equal.
func DecimalKey(s string) string {
	negative, whole, frac := decimalParts(s)
	key := whole + "." + frac
	if negative {
		key = "-" + key
	}
	return key
}

// DecimalToFloat converts a numeric to double precision as Postgres does, through its text,
// which rounds to the nearest double.
func DecimalToFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%w: %q is out of range for type double precision", ErrOutOfRange, s)
	}
	return f, nil
}

// DecimalToInt converts a numeric to bigint as Postgres does: rounded to the nearest integer,
// half away from zero, and an error when that is out of range.
func DecimalToInt(s string) (int64, error) {
	negative, whole, frac := decimalParts(s)
	n, ok := new(big.Int).SetString("0"+whole, 10)
	if !ok {
		return 0, fmt.Errorf("value: bad numeric %q", s)
	}
	if frac != "" && frac[0] >= '5' {
		n.Add(n, big.NewInt(1))
	}
	if negative {
		n.Neg(n)
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("%w: bigint out of range", ErrOutOfRange)
	}
	return n.Int64(), nil
}
