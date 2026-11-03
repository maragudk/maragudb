package value_test

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/value"
)

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
		sec := hegel.Draw(ht, hegel.OneOf(hegel.Integers[int64](-62135596800, 253402300799), hegel.Integers[int64](math.MinInt64, math.MaxInt64)))
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

func TestEncodeRow(t *testing.T) {
	t.Run("round trips every type and NULL", func(t *testing.T) {
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
