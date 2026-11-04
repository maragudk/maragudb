package value

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Errors of [Parse].
var (
	// ErrInvalidInput is text that is not a value of the type, as Postgres reports with
	// invalid_text_representation, invalid_datetime_format, or datetime_field_overflow.
	ErrInvalidInput = errors.New("invalid input syntax")
	// ErrOutOfRange is text that is a value of the type but does not fit in it.
	ErrOutOfRange = errors.New("out of range")
	// ErrUnsupportedInput is text in a form that Postgres accepts and maragudb does not, such as
	// a timestamp that is not ISO 8601.
	ErrUnsupportedInput = errors.New("unsupported input")
)

// Parse text as a value of a column type, the way the type's Postgres input function does, which
// is how a quoted literal takes the type its context demands and how CAST(text AS type) works.
func Parse(s string, t Type) (Value, error) {
	switch t {
	case Int:
		i, err := parseInt(s)
		return NewInt(i), err
	case Int4:
		i, err := parseInt(s)
		if err == nil && (i < math.MinInt32 || i > math.MaxInt32) {
			return Value{}, fmt.Errorf("%w: value %q is out of range for type integer", ErrOutOfRange, s)
		}
		return Value{Type: Int4, I: i}, err
	case Float:
		f, err := parseFloat(s)
		return NewFloat(f), err
	case Bool:
		b, ok := parseBool(s)
		if !ok {
			return Value{}, fmt.Errorf("%w for type boolean: %q", ErrInvalidInput, s)
		}
		return NewBool(b), nil
	case String:
		return NewString(s), nil
	case Bytes:
		b, err := parseBytea(s)
		return NewBytes(b), err
	case Time:
		tm, err := ParseTime(s)
		return NewTime(tm), err
	}
	panic(fmt.Sprintf("value: parsing %v", t))
}

var (
	decimalInt = regexp.MustCompile(`^[+-]?[0-9]+(_[0-9]+)*$`)
	hexInt     = regexp.MustCompile(`^[+-]?0[xX][0-9a-fA-F]+(_[0-9a-fA-F]+)*$`)
	octInt     = regexp.MustCompile(`^[+-]?0[oO][0-7]+(_[0-7]+)*$`)
	binInt     = regexp.MustCompile(`^[+-]?0[bB][01]+(_[01]+)*$`)
)

// parseInt as Postgres's int8in does: decimal, or hexadecimal, octal, and binary with a 0x, 0o,
// or 0b prefix, with single underscores between digits, and surrounding whitespace.
func parseInt(s string) (int64, error) {
	trimmed := strings.TrimSpace(s)
	base := 10
	switch {
	case decimalInt.MatchString(trimmed):
	case hexInt.MatchString(trimmed):
		base = 16
	case octInt.MatchString(trimmed):
		base = 8
	case binInt.MatchString(trimmed):
		base = 2
	default:
		return 0, fmt.Errorf("%w for type bigint: %q", ErrInvalidInput, s)
	}
	digits := strings.ReplaceAll(trimmed, "_", "")
	neg := false
	if digits[0] == '+' || digits[0] == '-' {
		neg = digits[0] == '-'
		digits = digits[1:]
	}
	if base != 10 {
		digits = digits[2:]
	}
	u, err := strconv.ParseUint(digits, base, 64)
	limit := uint64(math.MaxInt64)
	if neg {
		limit++
	}
	if err != nil || u > limit {
		return 0, fmt.Errorf("%w: value %q is out of range for type bigint", ErrOutOfRange, s)
	}
	if neg {
		return int64(-u), nil
	}
	return int64(u), nil
}

var (
	decimalFloat = regexp.MustCompile(`^[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	hexFloat     = regexp.MustCompile(`^[+-]?0[xX]([0-9a-fA-F]+\.?[0-9a-fA-F]*|\.[0-9a-fA-F]+)([pP][+-]?[0-9]+)?$`)
)

// parseFloat as Postgres's float8in does, which is strtod plus a check that nothing but
// whitespace follows: decimal and hexadecimal notation, NaN, Infinity, and inf, ignoring case.
// Overflow and underflow to zero are out of range; a subnormal result is not.
func parseFloat(s string) (float64, error) {
	trimmed := strings.TrimSpace(s)
	switch strings.ToLower(trimmed) {
	case "nan", "+nan", "-nan":
		return math.NaN(), nil
	case "infinity", "inf", "+infinity", "+inf":
		return math.Inf(1), nil
	case "-infinity", "-inf":
		return math.Inf(-1), nil
	}
	text := trimmed
	switch {
	case decimalFloat.MatchString(trimmed):
	case hexFloat.MatchString(trimmed):
		if !strings.ContainsAny(trimmed, "pP") {
			text += "p0"
		}
	default:
		return 0, fmt.Errorf("%w for type double precision: %q", ErrInvalidInput, s)
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return 0, fmt.Errorf("%w for type double precision: %q", ErrInvalidInput, s)
	}
	if math.IsInf(f, 0) || (f == 0 && hasNonZeroDigit(trimmed)) {
		return 0, fmt.Errorf("%w: %q is out of range for type double precision", ErrOutOfRange, s)
	}
	return f, nil
}

// hasNonZeroDigit reports whether the mantissa of a numeric text has a digit other than zero,
// so that a result of zero means underflow.
func hasNonZeroDigit(s string) bool {
	s = strings.ToLower(strings.TrimLeft(s, "+-"))
	var mantissa string
	if strings.HasPrefix(s, "0x") {
		mantissa, _, _ = strings.Cut(s[2:], "p")
	} else {
		mantissa, _, _ = strings.Cut(s, "e")
	}
	return strings.ContainsFunc(mantissa, func(r rune) bool { return (r >= '1' && r <= '9') || (r >= 'a' && r <= 'f') })
}

// parseBool accepts what Postgres accepts: any prefix of true, false, yes, no, on, and off,
// plus 1 and 0, ignoring case and surrounding space.
func parseBool(s string) (bool, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return false, false
	}
	for _, w := range []string{"true", "yes", "on", "1"} {
		if strings.HasPrefix(w, s) && (w != "on" || s == "on") {
			return true, true
		}
	}
	for _, w := range []string{"false", "no", "off", "0"} {
		if strings.HasPrefix(w, s) && (w != "off" || len(s) >= 2) {
			return false, true
		}
	}
	return false, false
}

// parseBytea as Postgres's byteain does: the hex format \x6869, with whitespace allowed between
// digit pairs, or the escape format, where \\ is a backslash, \ooo is an octal byte, and every
// other byte stands for itself.
func parseBytea(s string) ([]byte, error) {
	if strings.HasPrefix(s, `\x`) {
		var out []byte
		rest := s[2:]
		for len(rest) > 0 {
			if rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == '\r' {
				rest = rest[1:]
				continue
			}
			if len(rest) < 2 {
				return nil, fmt.Errorf("%w: invalid hexadecimal data: odd number of digits", ErrInvalidInput)
			}
			b, err := strconv.ParseUint(rest[:2], 16, 8)
			if err != nil {
				return nil, fmt.Errorf("%w: invalid hexadecimal digit in %q", ErrInvalidInput, s)
			}
			out = append(out, byte(b))
			rest = rest[2:]
		}
		return out, nil
	}
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			out = append(out, s[i])
			continue
		}
		switch {
		case i+1 < len(s) && s[i+1] == '\\':
			out = append(out, '\\')
			i++
		case i+3 < len(s) && s[i+1] >= '0' && s[i+1] <= '3' && s[i+2] >= '0' && s[i+2] <= '7' && s[i+3] >= '0' && s[i+3] <= '7':
			b, _ := strconv.ParseUint(s[i+1:i+4], 8, 8)
			out = append(out, byte(b))
			i += 3
		default:
			return nil, fmt.Errorf("%w for type bytea", ErrInvalidInput)
		}
	}
	return out, nil
}

var isoTimestamp = regexp.MustCompile(`^(\d{4,6})-(\d{1,2})-(\d{1,2})(?:(?:T|\s+)(\d{1,2}):(\d{1,2})(?::(\d{1,2})(?:\.(\d*))?)?)?\s*(?:([Zz])|([+-])(\d{1,2})(?::?(\d{2}))?)?$`)

// Timestamptz range, as in Postgres. The lower bound is where maragudb stops: Postgres goes back
// to 4714 BC, but writes those years with a BC suffix, which maragudb does not support.
var (
	minTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxTime = time.Date(294277, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ParseTime parses a timestamptz as Postgres does for its ISO 8601 forms: a date, an optional
// time with up to any number of fractional digits, which round half to even at microseconds,
// and an optional zone, Z or an offset such as +02, +0200, or +02:00; without a zone the time
// is UTC. The result is in the range from 0001-01-01 to the end of 294276. Other forms that
// Postgres accepts, such as month names, BC dates, or named zones, are [ErrUnsupportedInput].
func ParseTime(s string) (time.Time, error) {
	trimmed := strings.TrimSpace(s)
	m := isoTimestamp.FindStringSubmatch(trimmed)
	if m == nil {
		if trimmed == "" || isoLike.MatchString(trimmed) && !shortYear.MatchString(trimmed) {
			return time.Time{}, fmt.Errorf("%w for type timestamp with time zone: %q", ErrInvalidInput, s)
		}
		// Letters that no ISO 8601 timestamp has, digits without separators, or a year of fewer
		// than four digits: Postgres may read them as a date word, a month name, a named zone,
		// or a date in another style.
		return time.Time{}, fmt.Errorf("%w: timestamp %q is not in the ISO 8601 form 2006-01-02 15:04:05.999999+00", ErrUnsupportedInput, s)
	}
	field := func(i int) int {
		n, _ := strconv.Atoi(m[i])
		return n
	}
	year, month, day := field(1), field(2), field(3)
	hour, minute, sec := 0, 0, 0
	if m[4] != "" {
		hour, minute = field(4), field(5)
		if m[6] != "" {
			sec = field(6)
		}
	}
	usec, carry := roundFraction(m[7])
	sec += carry
	if year < 1 || month < 1 || month > 12 || day < 1 || day > daysIn(year, time.Month(month)) || hour > 24 || minute > 59 || sec > 60 ||
		(hour == 24 && (minute != 0 || sec != 0 || usec != 0)) {
		return time.Time{}, fmt.Errorf("%w: date/time field value out of range: %q", ErrInvalidInput, s)
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, usec*1000, time.UTC)
	if m[9] != "" {
		offset := field(10)*3600 + field(11)*60
		if offset > 15*3600+59*60 {
			return time.Time{}, fmt.Errorf("%w: time zone displacement out of range: %q", ErrInvalidInput, s)
		}
		if m[9] == "+" {
			offset = -offset
		}
		t = t.Add(time.Duration(offset) * time.Second)
	}
	if t.Before(minTime) {
		return time.Time{}, fmt.Errorf("%w: timestamps before 0001-01-01 00:00:00+00 are written with a BC suffix", ErrUnsupportedInput)
	}
	if !t.Before(maxTime) {
		return time.Time{}, fmt.Errorf("%w: timestamp out of range: %q", ErrOutOfRange, s)
	}
	return t, nil
}

// isoLike matches text made of the characters an ISO 8601 timestamp uses, with a dash, so that
// a malformed one such as "2024-13-45" or "2024-01-02 03" is invalid input rather than
// unsupported; shortYear matches a year of fewer than four digits, which Postgres reads in
// another style.
var (
	isoLike   = regexp.MustCompile(`^[0-9 :.+TZz-]*-[0-9 :.+TZz-]*$`)
	shortYear = regexp.MustCompile(`^\d{1,3}-`)
)

// roundFraction rounds a string of fractional second digits to microseconds, half to even, and
// returns the microseconds and the seconds carried out when it rounds up to a whole second.
func roundFraction(digits string) (int, int) {
	if len(digits) <= 6 {
		digits += strings.Repeat("0", 6-len(digits))
		n, _ := strconv.Atoi(digits)
		return n, 0
	}
	n, _ := strconv.Atoi(digits[:6])
	rest := strings.TrimRight(digits[6:], "0")
	switch {
	case rest == "", rest[0] < '5':
	case rest[0] > '5', len(rest) > 1, n%2 == 1:
		n++
	}
	if n == 1000000 {
		return 0, 1
	}
	return n, 0
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// ParseNumeric reads a numeric literal exactly, which [NumericToInt] and [NumericToFloat] use.
func ParseNumeric(s string) (*big.Rat, bool) {
	r, ok := new(big.Rat).SetString(s)
	return r, ok
}

// NumericToFloat converts a numeric literal to double precision as Postgres does, by reading
// its text as a float. A numeric zero has no sign, so -0.0 is 0.
func NumericToFloat(s string) (float64, error) {
	f, err := parseFloat(s)
	if f == 0 {
		f = 0
	}
	return f, err
}

// NumericToInt converts a numeric literal to bigint as Postgres does: it rounds half away from
// zero, and a result outside the range is [ErrOutOfRange].
func NumericToInt(s string) (int64, error) {
	r, ok := ParseNumeric(s)
	if !ok {
		return 0, fmt.Errorf("%w for type numeric: %q", ErrInvalidInput, s)
	}
	// Round half away from zero: add or subtract one half, then truncate toward zero.
	half := big.NewRat(1, 2)
	if r.Sign() < 0 {
		r.Sub(r, half)
	} else {
		r.Add(r, half)
	}
	q := new(big.Int).Quo(r.Num(), r.Denom())
	if !q.IsInt64() {
		return 0, fmt.Errorf("%w: bigint out of range", ErrOutOfRange)
	}
	return q.Int64(), nil
}

// NumericText is the text output of a numeric literal, as Postgres's numeric type writes it: the
// value in plain notation, without leading zeros or a sign on zero, with as many fractional
// digits as the literal has after its decimal point, less its exponent.
func NumericText(s string) (string, error) {
	sign := ""
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		sign, s = s[:1], s[1:]
	}
	if sign == "+" {
		sign = ""
	}
	mantissa, expText, hasExp := strings.Cut(strings.ToLower(s), "e")
	exp := 0
	if hasExp {
		n, err := strconv.Atoi(expText)
		if err != nil || n > 1000 || n < -1000 {
			return "", fmt.Errorf("numeric literal %q is out of the range maragudb supports", s)
		}
		exp = n
	}
	intPart, frac, _ := strings.Cut(mantissa, ".")
	if strings.Trim(intPart+frac, "0123456789") != "" || intPart+frac == "" {
		return "", fmt.Errorf("%w for type numeric: %q", ErrInvalidInput, s)
	}
	digits := intPart + frac
	point := len(intPart) + exp
	scale := max(0, len(frac)-exp)
	var ip, fp string
	switch {
	case point <= 0:
		ip, fp = "0", strings.Repeat("0", -point)+digits
	case point >= len(digits):
		ip = digits + strings.Repeat("0", point-len(digits))
	default:
		ip, fp = digits[:point], digits[point:]
	}
	ip = strings.TrimLeft(ip, "0")
	if ip == "" {
		ip = "0"
	}
	if strings.Trim(digits, "0") == "" {
		sign = ""
	}
	if scale == 0 {
		return sign + ip, nil
	}
	return sign + ip + "." + fp, nil
}
