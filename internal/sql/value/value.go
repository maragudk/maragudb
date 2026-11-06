// Package value has the SQL value types, their comparison, an order-preserving key encoding, a
// row encoding, and the text input and output of every type as Postgres defines them.
package value

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Type of a SQL value.
type Type int

// Types. The first six are the column types. [Int4] is the type of an integer literal that fits
// in 32 bits and of the functions that return one, as in Postgres, and it widens to [Int] when
// it meets one. [Numeric] is the type of a numeric literal, one with a decimal point, an
// exponent, or too many digits for [Int]; it only exists until it is cast. [Decimal] is
// Postgres's numeric as a value, which only SUM and AVG of integers return. [Unknown] is the
// type of a NULL or a quoted string literal whose type has not been inferred from its context.
const (
	Unknown Type = iota
	Int
	Float
	Bool
	String
	Bytes
	Time
	Int4
	Numeric
	Decimal
)

var typeNames = map[Type]string{
	Unknown: "unknown", Int: "bigint", Float: "double precision", Bool: "boolean", String: "text",
	Bytes: "bytea", Time: "timestamptz", Int4: "integer", Numeric: "numeric",
	Decimal: "numeric",
}

// String is the SQL name of the type.
func (t Type) String() string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("type(%d)", int(t))
}

// OutputName is the name Postgres gives an output column that is a cast to the type.
func (t Type) OutputName() string {
	switch t {
	case Int:
		return "int8"
	case Float:
		return "float8"
	case Bool:
		return "bool"
	case Int4:
		return "int4"
	}
	return t.String()
}

// IsColumnType reports whether a column can have the type.
func (t Type) IsColumnType() bool {
	return t >= Int && t <= Time
}

// Value is a SQL value: a typed payload or a NULL. A NULL carries the type it was inferred to
// have, or [Unknown]. A non-null value of type [Unknown] is a quoted string literal whose type
// has not been inferred, and of type [Numeric] a numeric literal; both hold their text in S.
type Value struct {
	Type Type
	Null bool
	I    int64
	F    float64
	B    bool
	S    string
	Raw  []byte
	T    time.Time
}

// NewInt value.
func NewInt(i int64) Value { return Value{Type: Int, I: i} }

// NewInt4 value.
func NewInt4(i int32) Value { return Value{Type: Int4, I: int64(i)} }

// NewFloat value.
func NewFloat(f float64) Value { return Value{Type: Float, F: f} }

// NewBool value.
func NewBool(b bool) Value { return Value{Type: Bool, B: b} }

// NewString value.
func NewString(s string) Value { return Value{Type: String, S: s} }

// NewBytes value. The slice is not copied.
func NewBytes(b []byte) Value {
	if b == nil {
		b = []byte{}
	}
	return Value{Type: Bytes, Raw: b}
}

// NewTime value, converted to UTC and truncated to microseconds, which is the precision of
// timestamptz.
func NewTime(t time.Time) Value { return Value{Type: Time, T: t.UTC().Truncate(time.Microsecond)} }

// NewUnknown is a quoted string literal whose type is not inferred yet.
func NewUnknown(s string) Value { return Value{Type: Unknown, S: s} }

// NewNumeric is a numeric literal, kept as its text.
func NewNumeric(s string) Value { return Value{Type: Numeric, S: s} }

// NewNull of type t.
func NewNull(t Type) Value { return Value{Type: t, Null: true} }

// String renders the value for messages: NULL, a quoted string, or the text output of
// [Format].
func (v Value) String() string {
	switch {
	case v.Null:
		return "NULL"
	case v.Type == String, v.Type == Unknown:
		return strconv.Quote(v.S)
	}
	return Format(v)
}

// Format a non-null value as text, the way Postgres outputs the type with its default
// settings and the UTC time zone: an integer in decimal, a float as the shortest digits that
// read back exactly, a boolean as true or false, bytes as a hex escape such as \x6869, and a
// timestamptz as 2006-01-02 15:04:05.999999+00.
func Format(v Value) string {
	switch v.Type {
	case Int, Int4:
		return strconv.FormatInt(v.I, 10)
	case Float:
		return FormatFloat(v.F)
	case Bool:
		if v.B {
			return "true"
		}
		return "false"
	case String, Unknown, Numeric, Decimal:
		return v.S
	case Bytes:
		return "\\x" + fmt.Sprintf("%x", v.Raw)
	case Time:
		return FormatTime(v.T)
	}
	panic(fmt.Sprintf("value: formatting %v", v.Type))
}

// FormatFloat like Postgres: the shortest digits that read back exactly, in plain notation when
// the decimal exponent is in [-4, 15) and in exponent notation otherwise, with NaN and Infinity
// spelled as Postgres does.
func FormatFloat(f float64) string {
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
	// Postgres writes the exponent with at least two digits and a sign.
	mantissa, expText, _ := strings.Cut(e, "e")
	sign, digits := expText[:1], expText[1:]
	if len(digits) < 2 {
		digits = "0" + digits
	}
	return mantissa + "e" + sign + digits
}

// FormatTime like Postgres in the UTC time zone: 2006-01-02 15:04:05.999999+00, with the
// fraction only when it is not zero, and years before 1 AD suffixed with BC.
func FormatTime(t time.Time) string {
	t = t.UTC()
	year := t.Year()
	bc := false
	if year <= 0 {
		year, bc = 1-year, true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%04d-%02d-%02d %02d:%02d:%02d", year, t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second())
	if usec := t.Nanosecond() / 1000; usec != 0 {
		b.WriteString(strings.TrimRight(fmt.Sprintf(".%06d", usec), "0"))
	}
	b.WriteString("+00")
	if bc {
		b.WriteString(" BC")
	}
	return b.String()
}

// Compare two non-null values of the same type and return -1, 0 or 1. Strings and bytes compare
// by byte order. Floats compare in a total order where NaN equals NaN and is greater than
// everything else, and negative zero equals zero. Numerics compare by value, whatever their
// scale. Both values must be non-NULL and of one type;
// anything else is a programming error and panics.
func Compare(a, b Value) int {
	if a.Null || b.Null {
		panic("value: comparing NULL")
	}
	if a.Type != b.Type {
		panic(fmt.Sprintf("value: comparing %v with %v", a.Type, b.Type))
	}
	switch a.Type {
	case Int, Int4:
		return cmp(a.I, b.I)
	case Float:
		return compareFloats(a.F, b.F)
	case Bool:
		return cmp(btoi(a.B), btoi(b.B))
	case String:
		return bytes.Compare([]byte(a.S), []byte(b.S))
	case Bytes:
		return bytes.Compare(a.Raw, b.Raw)
	case Decimal:
		return compareDecimals(a.S, b.S)
	case Time:
		// By Unix seconds and nanoseconds, like the key encoding, which never overflows.
		if c := cmp(a.T.Unix(), b.T.Unix()); c != 0 {
			return c
		}
		return cmp(int64(a.T.Nanosecond()), int64(b.T.Nanosecond()))
	}
	panic(fmt.Sprintf("value: comparing %v", a.Type))
}

func compareFloats(a, b float64) int {
	an, bn := math.IsNaN(a), math.IsNaN(b)
	switch {
	case an && bn:
		return 0
	case an:
		return 1
	case bn:
		return -1
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmp[T int64 | int](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EncodeKey appends the order-preserving encoding of v, which must have a column type, to dst
// and returns the result. For any two values of the same type, the byte order of their
// encodings equals their [Compare] order, with NULL before everything. Encodings are
// self-delimiting, so composite keys are concatenations.
func EncodeKey(dst []byte, v Value) []byte {
	// The first byte is 0x00 for NULL and 0x01 for a value, so NULL sorts first.
	if v.Null {
		return append(dst, 0x00)
	}
	dst = append(dst, 0x01)
	switch v.Type {
	case Int:
		return binary.BigEndian.AppendUint64(dst, uint64(v.I)^(1<<63))
	case Float:
		return binary.BigEndian.AppendUint64(dst, floatKeyBits(v.F))
	case Bool:
		return append(dst, byte(btoi(v.B)))
	case String:
		return appendEscaped(dst, []byte(v.S))
	case Bytes:
		return appendEscaped(dst, v.Raw)
	case Time:
		dst = binary.BigEndian.AppendUint64(dst, uint64(v.T.Unix())^(1<<63))
		return binary.BigEndian.AppendUint32(dst, uint32(v.T.Nanosecond()))
	}
	panic(fmt.Sprintf("value: encoding %v", v.Type))
}

// EncodeIndexKey appends the encoding of v as a column of an index to dst, so that the byte
// order of the encodings is the order in which Postgres sorts the column: by [Compare], reversed
// when desc, with NULL first when nullsFirst and last otherwise. Like [EncodeKey], it is
// self-delimiting, so that the encodings of several columns concatenate into one key in
// lexicographic order.
func EncodeIndexKey(dst []byte, v Value, desc, nullsFirst bool) []byte {
	// The first byte places NULL before a value with 0x00 and after it with 0x02.
	if v.Null {
		if nullsFirst {
			return append(dst, 0x00)
		}
		return append(dst, 0x02)
	}
	start := len(dst)
	dst = EncodeKey(dst, v)
	if desc {
		// The payload is self-delimiting, so no encoding is a proper prefix of another, and
		// inverting every byte reverses the order.
		for i := start + 1; i < len(dst); i++ {
			dst[i] = ^dst[i]
		}
	}
	return dst
}

// floatKeyBits maps a float to bits whose unsigned order is the [Compare] order: negative floats
// have every bit flipped, non-negative floats have the sign bit flipped, negative zero becomes
// zero, and NaN becomes a single canonical value above positive infinity.
func floatKeyBits(f float64) uint64 {
	if math.IsNaN(f) {
		return 0xFFF8000000000000
	}
	if f == 0 {
		f = 0
	}
	bits := math.Float64bits(f)
	if bits&(1<<63) != 0 {
		return ^bits
	}
	return bits | (1 << 63)
}

// appendEscaped appends b with every 0x00 byte escaped as 0x00 0xFF and a terminator of 0x00 0x01,
// which keeps byte order and makes the encoding self-delimiting.
func appendEscaped(dst, b []byte) []byte {
	for _, c := range b {
		if c == 0 {
			dst = append(dst, 0, 0xFF)
		} else {
			dst = append(dst, c)
		}
	}
	return append(dst, 0, 1)
}

// Row encoding tags.
const (
	rowNull byte = 0
)

// EncodeRow appends the encoding of a row of column-typed values to dst and returns the result.
func EncodeRow(dst []byte, row []Value) []byte {
	for _, v := range row {
		if v.Null {
			dst = append(dst, rowNull)
			continue
		}
		dst = append(dst, byte(v.Type))
		switch v.Type {
		case Int:
			dst = binary.BigEndian.AppendUint64(dst, uint64(v.I))
		case Float:
			dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(v.F))
		case Bool:
			dst = append(dst, byte(btoi(v.B)))
		case String:
			dst = binary.AppendUvarint(dst, uint64(len(v.S)))
			dst = append(dst, v.S...)
		case Bytes:
			dst = binary.AppendUvarint(dst, uint64(len(v.Raw)))
			dst = append(dst, v.Raw...)
		case Time:
			dst = binary.BigEndian.AppendUint64(dst, uint64(v.T.Unix()))
			dst = binary.BigEndian.AppendUint32(dst, uint32(v.T.Nanosecond()))
		default:
			panic(fmt.Sprintf("value: encoding %v", v.Type))
		}
	}
	return dst
}

// ErrCorruptRow is returned when a row does not decode against its schema.
var ErrCorruptRow = errors.New("value: corrupt row")

// DecodeRow decodes a row with the given column types. A NULL decodes with the column's type.
func DecodeRow(b []byte, types []Type) ([]Value, error) {
	row := make([]Value, len(types))
	for i, t := range types {
		if len(b) < 1 {
			return nil, fmt.Errorf("%w: missing column %d", ErrCorruptRow, i)
		}
		tag := b[0]
		b = b[1:]
		if tag == rowNull {
			row[i] = NewNull(t)
			continue
		}
		if Type(tag) != t {
			return nil, fmt.Errorf("%w: column %d has type %v, want %v", ErrCorruptRow, i, Type(tag), t)
		}
		var err error
		row[i], b, err = decodeValue(b, t)
		if err != nil {
			return nil, fmt.Errorf("%w: column %d: %w", ErrCorruptRow, i, err)
		}
	}
	if len(b) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorruptRow, len(b))
	}
	return row, nil
}

func decodeValue(b []byte, t Type) (Value, []byte, error) {
	need := func(n int) error {
		if len(b) < n {
			return fmt.Errorf("need %d bytes, have %d", n, len(b))
		}
		return nil
	}
	switch t {
	case Int:
		if err := need(8); err != nil {
			return Value{}, nil, err
		}
		return NewInt(int64(binary.BigEndian.Uint64(b))), b[8:], nil
	case Float:
		if err := need(8); err != nil {
			return Value{}, nil, err
		}
		return NewFloat(math.Float64frombits(binary.BigEndian.Uint64(b))), b[8:], nil
	case Bool:
		if err := need(1); err != nil {
			return Value{}, nil, err
		}
		if b[0] > 1 {
			return Value{}, nil, fmt.Errorf("bad bool %d", b[0])
		}
		return NewBool(b[0] == 1), b[1:], nil
	case String, Bytes:
		n, size := binary.Uvarint(b)
		if size <= 0 {
			return Value{}, nil, errors.New("bad length")
		}
		b = b[size:]
		if n > uint64(len(b)) {
			return Value{}, nil, fmt.Errorf("length %d exceeds %d", n, len(b))
		}
		payload := b[:n]
		if t == String {
			return NewString(string(payload)), b[n:], nil
		}
		return NewBytes(bytes.Clone(payload)), b[n:], nil
	case Time:
		if err := need(12); err != nil {
			return Value{}, nil, err
		}
		sec := int64(binary.BigEndian.Uint64(b))
		nsec := binary.BigEndian.Uint32(b[8:])
		if nsec >= 1e9 {
			return Value{}, nil, fmt.Errorf("bad nanoseconds %d", nsec)
		}
		return NewTime(time.Unix(sec, int64(nsec))), b[12:], nil
	}
	return Value{}, nil, fmt.Errorf("bad type %v", t)
}
