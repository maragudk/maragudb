package value_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/value"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

var allTypes = []value.Type{value.Int, value.Float, value.Bool, value.String, value.Bytes, value.Time}

// drawValue of type t, NULL with probability about one in eight when nullable.
func drawValue(ht hegel.TestCase, t value.Type, nullable bool) value.Value {
	if nullable && hegel.Draw(ht, hegel.Integers(0, 7)) == 0 {
		return value.NewNull(t)
	}
	switch t {
	case value.Int:
		return value.NewInt(hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
	case value.Float:
		return value.NewFloat(hegel.Draw(ht, hegel.Floats[float64]().AllowNaN(true).AllowInfinity(true)))
	case value.Bool:
		return value.NewBool(hegel.Draw(ht, hegel.Booleans()))
	case value.String:
		return value.NewString(hegel.Draw(ht, hegel.Text().MaxSize(6)))
	case value.Bytes:
		return value.NewBytes(hegel.Draw(ht, hegel.Binary(0, 6)))
	case value.Time:
		// The common era up to 9999, and the whole range of timestamptz, from 4714-11-24 BC to the
		// end of 294276.
		sec := hegel.Draw(ht, hegel.OneOf(hegel.Integers[int64](-62135596800, 253402300799), hegel.Integers[int64](-210866803200, 9224318015999)))
		nsec := hegel.Draw(ht, hegel.Integers[int64](0, 999999999))
		return value.NewTime(time.Unix(sec, nsec))
	}
	panic(t)
}

// compareNullsFirst orders NULL before every value, like the key encoding.
func compareNullsFirst(a, b value.Value) int {
	switch {
	case a.Null && b.Null:
		return 0
	case a.Null:
		return -1
	case b.Null:
		return 1
	}
	return value.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestEncodeKey(t *testing.T) {
	t.Run("byte order of single keys equals value order for every type, NULL first", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			typ := hegel.Draw(ht, hegel.SampledFrom(allTypes))
			a := drawValue(ht, typ, true)
			b := drawValue(ht, typ, true)
			ka := value.EncodeKey(nil, a)
			kb := value.EncodeKey(nil, b)
			want := compareNullsFirst(a, b)
			got := sign(bytes.Compare(ka, kb))
			ht.Note(fmt.Sprintf("a=%v b=%v ka=%x kb=%x", a, b, ka, kb))
			if got != want {
				ht.Fatalf("encoding order %d, value order %d", got, want)
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("byte order of composite keys equals lexicographic value order", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			types := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(allTypes)).MinSize(1).MaxSize(4))
			a := make([]value.Value, len(types))
			b := make([]value.Value, len(types))
			var ka, kb []byte
			for i, typ := range types {
				a[i] = drawValue(ht, typ, true)
				// Equal prefixes are the interesting case, so b often repeats a.
				if hegel.Draw(ht, hegel.Booleans()) {
					b[i] = a[i]
				} else {
					b[i] = drawValue(ht, typ, true)
				}
				ka = value.EncodeKey(ka, a[i])
				kb = value.EncodeKey(kb, b[i])
			}
			want := 0
			for i := range types {
				if want = compareNullsFirst(a[i], b[i]); want != 0 {
					break
				}
			}
			got := sign(bytes.Compare(ka, kb))
			ht.Note(fmt.Sprintf("a=%v b=%v ka=%x kb=%x", a, b, ka, kb))
			if got != want {
				ht.Fatalf("encoding order %d, value order %d", got, want)
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("encodings are self-delimiting: no encoding is a proper prefix of another of the same type", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			typ := hegel.Draw(ht, hegel.SampledFrom(allTypes))
			a := drawValue(ht, typ, true)
			b := drawValue(ht, typ, true)
			ka := value.EncodeKey(nil, a)
			kb := value.EncodeKey(nil, b)
			if len(ka) < len(kb) && bytes.HasPrefix(kb, ka) {
				ht.Fatalf("%v (%x) is a prefix of %v (%x)", a, ka, b, kb)
			}
		}, hegel.WithTestCases(2000))
	})

	t.Run("orders the float special values like Postgres", func(t *testing.T) {
		values := []float64{math.Inf(-1), -1, math.Copysign(0, -1), 0, 1, math.Inf(1), math.NaN()}
		for i := 1; i < len(values); i++ {
			a, b := value.NewFloat(values[i-1]), value.NewFloat(values[i])
			c := bytes.Compare(value.EncodeKey(nil, a), value.EncodeKey(nil, b))
			if values[i-1] == 0 && values[i] == 0 {
				is.Equal(t, 0, c, "negative zero equals zero")
				is.Equal(t, 0, value.Compare(a, b))
			} else {
				is.Equal(t, -1, c, fmt.Sprintf("%v < %v", values[i-1], values[i]))
				is.Equal(t, -1, value.Compare(a, b))
			}
		}
		nan := value.NewFloat(math.NaN())
		is.Equal(t, 0, value.Compare(nan, nan))
		is.True(t, bytes.Equal(value.EncodeKey(nil, nan), value.EncodeKey(nil, value.NewFloat(math.Float64frombits(0x7FF0000000000001)))), "every NaN encodes the same")
	})
}

func TestEncodeIndexKey(t *testing.T) {
	// compareIndex is the order of an index column as Postgres sorts it: by value, reversed when
	// descending, with NULL first or last.
	compareIndex := func(a, b value.Value, desc, nullsFirst bool) int {
		switch {
		case a.Null && b.Null:
			return 0
		case a.Null && nullsFirst, b.Null && !nullsFirst:
			return -1
		case a.Null, b.Null:
			return 1
		}
		c := value.Compare(a, b)
		if desc {
			c = -c
		}
		return sign(c)
	}

	t.Run("byte order of composite index keys equals the order of every column with its direction and NULL placement", func(t *testing.T) {
		verify.Covers(t, "the index key encoding sorts composite keys of every type, ascending or descending, with NULLs first or last, in the order Postgres sorts the index", "INV-INDEX-CONSISTENT")
		hegel.Test(t, func(ht *hegel.T) {
			types := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(allTypes)).MinSize(1).MaxSize(4))
			var ka, kb []byte
			want := 0
			for _, typ := range types {
				desc := hegel.Draw(ht, hegel.Booleans())
				nullsFirst := hegel.Draw(ht, hegel.Booleans())
				a := drawValue(ht, typ, true)
				b := a
				if hegel.Draw(ht, hegel.Booleans()) {
					b = drawValue(ht, typ, true)
				}
				ka = value.EncodeIndexKey(ka, a, desc, nullsFirst)
				kb = value.EncodeIndexKey(kb, b, desc, nullsFirst)
				if want == 0 {
					want = compareIndex(a, b, desc, nullsFirst)
				}
				ht.Note(fmt.Sprintf("%v %v desc=%v nullsFirst=%v", a, b, desc, nullsFirst))
			}
			if got := sign(bytes.Compare(ka, kb)); got != want {
				ht.Fatalf("encoding order %d, index order %d: %x, %x", got, want, ka, kb)
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("values that Postgres's index compares as equal encode the same, and others differently", func(t *testing.T) {
		zero := value.EncodeIndexKey(nil, value.NewFloat(0), true, false)
		is.True(t, bytes.Equal(zero, value.EncodeIndexKey(nil, value.NewFloat(math.Copysign(0, -1)), true, false)), "0 and -0")
		is.True(t, !bytes.Equal(value.EncodeIndexKey(nil, value.NewNull(value.Int), false, true), value.EncodeIndexKey(nil, value.NewNull(value.Int), false, false)), "NULL first and last")
		hegel.Test(t, func(ht *hegel.T) {
			typ := hegel.Draw(ht, hegel.SampledFrom(allTypes))
			desc := hegel.Draw(ht, hegel.Booleans())
			a := drawValue(ht, typ, false)
			b := drawValue(ht, typ, false)
			ka := value.EncodeIndexKey(nil, a, desc, false)
			kb := value.EncodeIndexKey(nil, b, desc, false)
			if bytes.Equal(ka, kb) != (value.Compare(a, b) == 0) {
				ht.Fatalf("%v and %v: %x, %x", a, b, ka, kb)
			}
			if len(ka) < len(kb) && bytes.HasPrefix(kb, ka) {
				ht.Fatalf("%v (%x) is a prefix of %v (%x)", a, ka, b, kb)
			}
		}, hegel.WithTestCases(2000))
	})
}

func TestEncodeRow(t *testing.T) {
	t.Run("round trips every type and NULL", func(t *testing.T) {
		verify.Covers(t, "every value of the six types and NULL round-trips through the row encoding", "INV-TYPES")
		hegel.Test(t, func(ht *hegel.T) {
			types := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(allTypes)).MaxSize(6))
			row := make([]value.Value, len(types))
			for i, typ := range types {
				row[i] = drawValue(ht, typ, true)
			}
			b := value.EncodeRow(nil, row)
			got, err := value.DecodeRow(b, types)
			if err != nil {
				ht.Fatal(err)
			}
			for i := range row {
				if !equalValue(row[i], got[i]) {
					ht.Fatalf("column %d: encoded %v, decoded %v", i, row[i], got[i])
				}
			}
		}, hegel.WithTestCases(2000))
	})

	t.Run("rejects every row encoding corrupted by one byte, or decodes it differently, never panics", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			types := hegel.Draw(ht, hegel.Lists(hegel.SampledFrom(allTypes)).MinSize(1).MaxSize(4))
			row := make([]value.Value, len(types))
			for i, typ := range types {
				row[i] = drawValue(ht, typ, true)
			}
			b := value.EncodeRow(nil, row)
			switch hegel.Draw(ht, hegel.Integers(0, 2)) {
			case 0:
				b = b[:hegel.Draw(ht, hegel.Integers(0, len(b)-1))]
			case 1:
				b = append(b, hegel.Draw(ht, hegel.Binary(1, 3))...)
			case 2:
				i := hegel.Draw(ht, hegel.Integers(0, len(b)-1))
				b[i] ^= byte(hegel.Draw(ht, hegel.Integers(1, 255)))
			}
			_, _ = value.DecodeRow(b, types)
		}, hegel.WithTestCases(2000))
	})

	t.Run("rejects a type that does not match the schema", func(t *testing.T) {
		b := value.EncodeRow(nil, []value.Value{value.NewInt(1)})
		_, err := value.DecodeRow(b, []value.Type{value.String})
		is.Error(t, value.ErrCorruptRow, err)
	})

	t.Run("rejects values that no write stores: text that is not UTF-8, and times finer than a microsecond or outside the range of timestamptz", func(t *testing.T) {
		verify.Covers(t, "a row of text that is not UTF-8, or of a time finer than a microsecond or before 4714-11-24 BC or after 294276, fails to decode with ErrCorruptRow", "INV-DAMAGE-DETECTED")
		row := func(tag value.Type, rest ...byte) []byte {
			return append([]byte{byte(tag)}, rest...)
		}
		times := func(sec int64, nsec uint32) []byte {
			b := binary.BigEndian.AppendUint64(nil, uint64(sec))
			return binary.BigEndian.AppendUint32(b, nsec)
		}
		for _, test := range []struct {
			b   []byte
			typ value.Type
		}{
			{row(value.String, 2, 0xff, 0xfe), value.String},
			{row(value.Time, times(0, 1)...), value.Time},
			{row(value.Time, times(-210866803201, 0)...), value.Time},
			{row(value.Time, times(9224318016000, 0)...), value.Time},
			{row(value.Time, times(math.MinInt64, 0)...), value.Time},
		} {
			_, err := value.DecodeRow(test.b, []value.Type{test.typ})
			is.Error(t, value.ErrCorruptRow, err, fmt.Sprintf("%x", test.b))
		}
		for _, sec := range []int64{0, -210866803200, 9224318015999} {
			_, err := value.DecodeRow(row(value.Time, times(sec, 1000)...), []value.Type{value.Time})
			is.NotError(t, err)
		}
	})
}

// FuzzDecodeRow decodes arbitrary bytes as a row of arbitrary column types, which never panics,
// fails with [value.ErrCorruptRow] when it fails, and yields values that encode to bytes that
// decode to the same values.
func FuzzDecodeRow(f *testing.F) {
	columns := []value.Type{value.Int, value.Float, value.Bool, value.String, value.Bytes, value.Time}
	b := value.EncodeRow(nil, []value.Value{value.NewInt(-3), value.NewFloat(math.NaN()), value.NewBool(true), value.NewString("é"), value.NewBytes([]byte{0}), value.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 1000, time.UTC))})
	f.Add(b, []byte{0, 1, 2, 3, 4, 5})
	f.Add(value.EncodeRow(nil, []value.Value{value.NewNull(value.Int), value.NewString("")}), []byte{0, 3})
	f.Add([]byte{4, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, []byte{3})
	f.Fuzz(func(t *testing.T, b, schema []byte) {
		types := make([]value.Type, len(schema))
		for i, c := range schema {
			types[i] = columns[int(c)%len(columns)]
		}
		row, err := value.DecodeRow(b, types)
		if err != nil {
			if !errors.Is(err, value.ErrCorruptRow) {
				t.Fatalf("error %v does not wrap ErrCorruptRow", err)
			}
			return
		}
		again, err := value.DecodeRow(value.EncodeRow(nil, row), types)
		if err != nil {
			t.Fatalf("a decoded row does not decode again: %v", err)
		}
		for i := range row {
			if !equalValue(row[i], again[i]) {
				t.Fatalf("column %d decoded as %v, and then as %v", i, row[i], again[i])
			}
		}
	})
}

func TestFormatFloat(t *testing.T) {
	t.Run("reads back exactly", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			f := hegel.Draw(ht, hegel.Floats[float64]().AllowNaN(true).AllowInfinity(true))
			s := value.FormatFloat(f)
			got, err := strconv.ParseFloat(s, 64)
			if err != nil {
				ht.Fatal(err)
			}
			if !(got == f || (math.IsNaN(got) && math.IsNaN(f))) {
				ht.Fatalf("%v formatted as %q, parsed as %v", f, s, got)
			}
		}, hegel.WithTestCases(2000))
	})
}

func equalValue(a, b value.Value) bool {
	if a.Type != b.Type || a.Null != b.Null {
		return false
	}
	if a.Null {
		return true
	}
	if a.Type == value.Float && math.IsNaN(a.F) && math.IsNaN(b.F) {
		return math.Float64bits(a.F) == math.Float64bits(b.F)
	}
	if a.Type == value.Float {
		return math.Float64bits(a.F) == math.Float64bits(b.F)
	}
	return value.Compare(a, b) == 0
}
