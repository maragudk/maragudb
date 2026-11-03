// Package value has the SQL value types, their comparison, an order-preserving key encoding,
// and a row encoding.
package value

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Type of a SQL value.
type Type int

// Types. Unknown is the type of a bare NULL whose type has not been inferred.
const (
	Unknown Type = iota
	Int
	Float
	Bool
	String
	Bytes
	Time
)

var typeNames = map[Type]string{Unknown: "unknown", Int: "int", Float: "float", Bool: "bool", String: "string", Bytes: "bytes", Time: "time"}

func (t Type) String() string {
	if s, ok := typeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("type(%d)", int(t))
}

// ParseType by its SQL name.
func ParseType(name string) (Type, bool) {
	for t, s := range typeNames {
		if t != Unknown && s == name {
			return t, true
		}
	}
	return Unknown, false
}

// Value is a SQL value: a typed payload or a NULL. A NULL carries the type it was inferred to
// have, or [Unknown].
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

// NewTime value, converted to UTC.
func NewTime(t time.Time) Value { return Value{Type: Time, T: t.UTC()} }

// NewNull of type t.
func NewNull(t Type) Value { return Value{Type: t, Null: true} }

// String renders the value as it would appear in SQL.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	switch v.Type {
	case Int:
		return strconv.FormatInt(v.I, 10)
	case Float:
		return FormatFloat(v.F)
	case Bool:
		if v.B {
			return "true"
		}
		return "false"
	case String:
		return strconv.Quote(v.S)
	case Bytes:
		return fmt.Sprintf("X'%x'", v.Raw)
	case Time:
		return v.T.Format(time.RFC3339Nano)
	}
	return "?"
}

// FormatFloat the way a float is rendered as a string: the shortest representation that
// reads back exactly, with NaN and Infinity spelled as in Postgres.
func FormatFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// Compare two non-null values of the same type and return -1, 0 or 1. Strings and bytes compare
// by byte order. Floats compare in a total order where NaN equals NaN and is greater than
// everything else, and negative zero equals zero. It panics on a NULL or on mismatched types,
// which is a programming error, because the binder checks types.
func Compare(a, b Value) int {
	if a.Null || b.Null {
		panic("value: comparing NULL")
	}
	if a.Type != b.Type {
		panic(fmt.Sprintf("value: comparing %v with %v", a.Type, b.Type))
	}
	switch a.Type {
	case Int:
		return cmp(a.I, b.I)
	case Float:
		return compareFloats(a.F, b.F)
	case Bool:
		return cmp(btoi(a.B), btoi(b.B))
	case String:
		return bytes.Compare([]byte(a.S), []byte(b.S))
	case Bytes:
		return bytes.Compare(a.Raw, b.Raw)
	case Time:
		return a.T.Compare(b.T)
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

// Key encoding tags. NULL sorts before every value.
const (
	keyNull  = 0x00
	keyValue = 0x01
)

// EncodeKey appends the order-preserving encoding of v to dst and returns the result. For any two
// values of the same type, the byte order of their encodings equals their [Compare] order, with
// NULL before everything. Encodings are self-delimiting, so composite keys are concatenations.
func EncodeKey(dst []byte, v Value) []byte {
	if v.Null {
		return append(dst, keyNull)
	}
	dst = append(dst, keyValue)
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

// EncodeRow appends the encoding of a row to dst and returns the result.
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
