package value

import (
	"errors"
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
// the weights of the digits of its operands. A numeric has at most 131072 digits before the
// point and at most 16383 after it.
const (
	decDigits        = 4
	nbase            = 10000
	minSigDigits     = 16
	maxDisplayScale  = 1000
	minDisplayScale  = 0
	maxDecimalScale  = 16383
	maxDecimalDigits = 131072
	decimalSignMinus = '-'
)

// ErrDivisionByZero is a numeric division or modulo by zero.
var ErrDivisionByZero = errors.New("division by zero")

// decimal is a numeric as the integer n and its scale: the value n / 10^scale, written with
// scale digits after the point.
type decimal struct {
	n     *big.Int
	scale int
}

// parseDecimal reads the text of a [Decimal] value.
func parseDecimal(s string) decimal {
	whole, frac, _ := strings.Cut(s, ".")
	n, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		panic(fmt.Sprintf("value: bad numeric %q", s))
	}
	return decimal{n: n, scale: len(frac)}
}

// value of the numeric, or [ErrOutOfRange] when it has more digits before the point than
// Postgres's numeric holds.
func (d decimal) value() (Value, error) {
	digits := new(big.Int).Abs(d.n).String()
	if len(digits)-d.scale > maxDecimalDigits {
		return Value{}, fmt.Errorf("%w: value overflows numeric format", ErrOutOfRange)
	}
	return NewDecimal(decimalText(digits, d.scale, d.n.Sign() < 0)), nil
}

// rescale to a scale at least that of d, exactly.
func (d decimal) rescale(scale int) *big.Int {
	return new(big.Int).Mul(d.n, pow10(scale-d.scale))
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// quoRound is num / den rounded half away from zero, as Postgres's round_var rounds.
func quoRound(num, den *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	if r.Abs(r).Lsh(r, 1).CmpAbs(den) >= 0 {
		if (num.Sign() < 0) != (den.Sign() < 0) {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

// DecimalNeg is -a, which keeps the scale of a, and of zero is zero.
func DecimalNeg(a Value) Value {
	switch {
	case strings.HasPrefix(a.S, "-"):
		return NewDecimal(a.S[1:])
	case strings.Trim(a.S, "0.") == "":
		return a
	}
	return NewDecimal("-" + a.S)
}

// DecimalAdd is a + b, exact, with the larger of their scales, as Postgres's add_var.
func DecimalAdd(a, b Value) (Value, error) {
	x, y := parseDecimal(a.S), parseDecimal(b.S)
	scale := max(x.scale, y.scale)
	n := x.rescale(scale)
	return decimal{n: n.Add(n, y.rescale(scale)), scale: scale}.value()
}

// DecimalSub is a - b, exact, with the larger of their scales, as Postgres's sub_var.
func DecimalSub(a, b Value) (Value, error) {
	x, y := parseDecimal(a.S), parseDecimal(b.S)
	scale := max(x.scale, y.scale)
	n := x.rescale(scale)
	return decimal{n: n.Sub(n, y.rescale(scale)), scale: scale}.value()
}

// DecimalMul is a * b, exact with the sum of their scales, rounded half away from zero to a
// scale of 16383 when that sum is larger, as Postgres's numeric_mul.
func DecimalMul(a, b Value) (Value, error) {
	x, y := parseDecimal(a.S), parseDecimal(b.S)
	d := decimal{n: new(big.Int).Mul(x.n, y.n), scale: x.scale + y.scale}
	if d.scale > maxDecimalScale {
		d = decimal{n: quoRound(d.n, pow10(d.scale-maxDecimalScale)), scale: maxDecimalScale}
	}
	return d.value()
}

// DecimalQuo is a / b, rounded half away from zero to the scale that Postgres's
// select_div_scale picks, or [ErrDivisionByZero].
func DecimalQuo(a, b Value) (Value, error) {
	return quotient(parseDecimal(a.S), parseDecimal(b.S))
}

// DecimalRem is a % b, which is a - trunc(a / b) * b, exact, with the sign of a and the larger
// of their scales, as Postgres's mod_var, or [ErrDivisionByZero].
func DecimalRem(a, b Value) (Value, error) {
	x, y := parseDecimal(a.S), parseDecimal(b.S)
	if y.n.Sign() == 0 {
		return Value{}, ErrDivisionByZero
	}
	scale := max(x.scale, y.scale)
	n := x.rescale(scale)
	return decimal{n: n.Rem(n, y.rescale(scale)), scale: scale}.value()
}

// DecimalDiv is the quotient of two integers as numeric, as Postgres's numeric division
// computes it: rounded half away from zero to the scale that select_div_scale picks, which
// gives at least 16 significant digits. The divisor must not be zero.
func DecimalDiv(dividend, divisor *big.Int) Value {
	v, err := quotient(decimal{n: dividend}, decimal{n: divisor})
	if err != nil {
		panic(fmt.Sprintf("value: %v / %v: %v", dividend, divisor, err))
	}
	return v
}

// quotient x / y as Postgres's numeric_div: rounded half away from zero to the scale of
// [divScale].
func quotient(x, y decimal) (Value, error) {
	if y.n.Sign() == 0 {
		return Value{}, ErrDivisionByZero
	}
	scale := divScale(x, y)
	// x / y * 10^scale = x.n * 10^(scale - x.scale + y.scale) / y.n.
	num, den := new(big.Int).Set(x.n), new(big.Int).Set(y.n)
	if e := scale - x.scale + y.scale; e >= 0 {
		num.Mul(num, pow10(e))
	} else {
		den.Mul(den, pow10(-e))
	}
	return decimal{n: quoRound(num, den), scale: scale}.value()
}

// divScale is the scale of x / y that Postgres's select_div_scale picks: enough for at least
// 16 significant digits, by the weights and first digits of the operands in base 10000, and at
// least the scale of either operand, within [0, 1000].
func divScale(x, y decimal) int {
	w1, d1 := x.firstDigit()
	w2, d2 := y.firstDigit()
	// The weight of the quotient, assuming the dividend is the smaller when the first digits
	// are equal.
	qweight := w1 - w2
	if d1 <= d2 {
		qweight--
	}
	scale := minSigDigits - qweight*decDigits
	scale = max(scale, x.scale, y.scale, minDisplayScale)
	return min(scale, maxDisplayScale)
}

// firstDigit of a numeric in base 10000, whose digits Postgres aligns at the point, and its
// weight, which is 0 for the units and -1 for the first four digits after the point. Zero has
// weight 0 and first digit 0.
func (d decimal) firstDigit() (int, int64) {
	if d.n.Sign() == 0 {
		return 0, 0
	}
	// Pad the scale to whole base-10000 digits.
	pad := (decDigits - d.scale%decDigits) % decDigits
	a := new(big.Int).Abs(d.n)
	a.Mul(a, pow10(pad))
	top := (len(a.String()) - 1) / decDigits
	weight := top - (d.scale+pad)/decDigits
	first := a.Quo(a, new(big.Int).Exp(big.NewInt(nbase), big.NewInt(int64(top)), nil))
	return weight, first.Int64()
}

// decimalText of a numeric of the magnitude digits, unscaled, with scale digits after the
// point. Zero has no sign.
func decimalText(digits string, scale int, negative bool) string {
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	var b strings.Builder
	if negative && strings.Trim(digits, "0") != "" {
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
// which rounds to the nearest double, and is out of range when that is infinite, or zero for a
// numeric that is not.
func DecimalToFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || f == 0 && strings.Trim(s, "-0.") != "" {
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
