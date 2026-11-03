package exec

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maragu.dev/maragudb/internal/sql/parser"
	"maragu.dev/maragudb/internal/sql/value"
)

// cast binds CAST(x AS t). Casts exist between int and float, from every type to string, and
// from string to every type. A float becomes an int by rounding half to even, as in Postgres.
func (b *binder) cast(x *cexpr, t value.Type) (*cexpr, error) {
	from := x.typ
	if from == value.Unknown {
		return constant(value.NewNull(t)), nil
	}
	var f func(v value.Value) (value.Value, error)
	switch {
	case from == t:
		f = func(v value.Value) (value.Value, error) { return v, nil }
	case from == value.Int && t == value.Float:
		f = func(v value.Value) (value.Value, error) { return value.NewFloat(float64(v.I)), nil }
	case from == value.Float && t == value.Int:
		f = func(v value.Value) (value.Value, error) {
			r := math.RoundToEven(v.F)
			if math.IsNaN(r) || r < -9223372036854775808.0 || r >= 9223372036854775808.0 {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
			return value.NewInt(int64(r)), nil
		}
	case t == value.String:
		f = func(v value.Value) (value.Value, error) {
			if v.Type == value.Bytes && !validString(v.Raw) {
				return value.Value{}, fmt.Errorf("%w: bytes are not valid UTF-8", ErrCast)
			}
			return value.NewString(formatValue(v)), nil
		}
	case from == value.String:
		f = func(v value.Value) (value.Value, error) { return parseValue(v.S, t) }
	default:
		return nil, fmt.Errorf("%w: cannot cast %v to %v", ErrCast, from, t)
	}
	return b.fold(&cexpr{typ: t, eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil || v.Null {
			return value.NewNull(t), err
		}
		return f(v)
	}}, x)
}

// formatValue as a string, the way CAST(x AS string) does.
func formatValue(v value.Value) string {
	switch v.Type {
	case value.Int:
		return strconv.FormatInt(v.I, 10)
	case value.Float:
		return formatFloat(v.F)
	case value.Bool:
		if v.B {
			return "true"
		}
		return "false"
	case value.String:
		return v.S
	case value.Bytes:
		return string(v.Raw)
	case value.Time:
		return v.T.Format(time.RFC3339Nano)
	}
	panic(fmt.Sprintf("exec: formatting %v", v.Type))
}

// formatFloat like Postgres: the shortest digits that read back exactly, in plain notation when
// the decimal exponent is in [-4, 15) and in exponent notation otherwise.
func formatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		if math.Signbit(f) {
			return "-0"
		}
		return "0"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64)
	exp, _ := strconv.Atoi(e[strings.IndexByte(e, 'e')+1:])
	if exp >= -4 && exp < 15 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return e
}

// parseValue parses a string as type t, the way CAST(s AS t) does.
func parseValue(s string, t value.Type) (value.Value, error) {
	switch t {
	case value.Int:
		i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return value.Value{}, fmt.Errorf("%w: %q is out of range for int", ErrOutOfRange, s)
			}
			return value.Value{}, fmt.Errorf("%w: invalid input syntax for int: %q", ErrCast, s)
		}
		return value.NewInt(i), nil
	case value.Float:
		trimmed := strings.TrimSpace(s)
		switch strings.ToLower(trimmed) {
		case "nan":
			return value.NewFloat(math.NaN()), nil
		case "infinity", "inf", "+infinity", "+inf":
			return value.NewFloat(math.Inf(1)), nil
		case "-infinity", "-inf":
			return value.NewFloat(math.Inf(-1)), nil
		}
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
				return value.Value{}, fmt.Errorf("%w: %q is out of range for float", ErrOutOfRange, s)
			}
			return value.Value{}, fmt.Errorf("%w: invalid input syntax for float: %q", ErrCast, s)
		}
		return value.NewFloat(f), nil
	case value.Bool:
		b, ok := parseBool(s)
		if !ok {
			return value.Value{}, fmt.Errorf("%w: invalid input syntax for bool: %q", ErrCast, s)
		}
		return value.NewBool(b), nil
	case value.Bytes:
		return value.NewBytes([]byte(s)), nil
	case value.Time:
		tm, err := parser.ParseTime(strings.TrimSpace(s))
		if err != nil {
			return value.Value{}, fmt.Errorf("%w: %v", ErrCast, err)
		}
		return value.NewTime(tm), nil
	}
	panic(fmt.Sprintf("exec: parsing %v", t))
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

// validString reports whether b is valid UTF-8, which every string value must be.
func validString(b []byte) bool {
	return utf8.Valid(b)
}
