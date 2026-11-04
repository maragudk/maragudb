package value_test

import (
	"errors"
	"math"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/value"
)

func TestParse(t *testing.T) {
	// Every expected value here was checked against Postgres 18.
	t.Run("reads integers as int8in does", func(t *testing.T) {
		for _, test := range []struct {
			in   string
			want int64
		}{
			{"1_000", 1000}, {" 1_000 ", 1000}, {"0x10", 16}, {"0o17", 15}, {"0b101", 5}, {"-0x10", -16}, {"017", 17},
			{"+5", 5}, {"-9223372036854775808", math.MinInt64}, {"9223372036854775807", math.MaxInt64}, {"0X1F", 31},
		} {
			got, err := value.Parse(test.in, value.Int)
			is.NotError(t, err, test.in)
			is.Equal(t, test.want, got.I, test.in)
		}
		for _, in := range []string{"+ 1", "1__0", "0x", "_1", "1_", "", "1.0", "abc", "1e3", "0x1_", "- 1"} {
			_, err := value.Parse(in, value.Int)
			is.Error(t, value.ErrInvalidInput, err, in)
		}
		for _, in := range []string{"99999999999999999999", "9223372036854775808", "-9223372036854775809", "0x8000000000000000"} {
			_, err := value.Parse(in, value.Int)
			is.Error(t, value.ErrOutOfRange, err, in)
		}
		_, err := value.Parse("2147483648", value.Int4)
		is.Error(t, value.ErrOutOfRange, err)
	})

	t.Run("reads floats as float8in does", func(t *testing.T) {
		for _, test := range []struct {
			in   string
			want float64
		}{
			{"0x10", 16}, {" 1.5 ", 1.5}, {"1.", 1}, {".5", 0.5}, {"1e5", 1e5}, {"0x1.8p1", 3}, {"-0", math.Copysign(0, -1)},
			{"+inf", math.Inf(1)}, {"-Infinity", math.Inf(-1)}, {"infinity ", math.Inf(1)}, {"1e-310", 1e-310}, {"INF", math.Inf(1)},
		} {
			got, err := value.Parse(test.in, value.Float)
			is.NotError(t, err, test.in)
			is.Equal(t, math.Float64bits(test.want), math.Float64bits(got.F), test.in)
		}
		for _, in := range []string{"NaN", "nan", "NAN", "+nan"} {
			got, err := value.Parse(in, value.Float)
			is.NotError(t, err, in)
			is.True(t, math.IsNaN(got.F), in)
		}
		for _, in := range []string{"0x", "1e", ".", "nan(", "1.5e", "1_000", "", "x", "0x1p", "nan(", "infinityx"} {
			_, err := value.Parse(in, value.Float)
			is.Error(t, value.ErrInvalidInput, err, in)
		}
		for _, in := range []string{"1e-400", "1e400", "-1e400", "0x1p-2000"} {
			_, err := value.Parse(in, value.Float)
			is.Error(t, value.ErrOutOfRange, err, in)
		}
	})

	t.Run("reads booleans as boolin does", func(t *testing.T) {
		for _, test := range []struct {
			in   string
			want bool
		}{
			{" t ", true}, {"ye", true}, {"of", false}, {"yes ", true}, {"1", true}, {"tr", true}, {"on", true}, {"TRUE", true},
			{"f", false}, {"n", false}, {"0", false}, {"off", false},
		} {
			got, err := value.Parse(test.in, value.Bool)
			is.NotError(t, err, test.in)
			is.Equal(t, test.want, got.B, test.in)
		}
		for _, in := range []string{"10", "o", "", "yess", "2"} {
			_, err := value.Parse(in, value.Bool)
			is.Error(t, value.ErrInvalidInput, err, in)
		}
	})

	t.Run("reads bytes as byteain does, in hex and escape format", func(t *testing.T) {
		for _, test := range []struct {
			in   string
			want []byte
		}{
			{`\x`, []byte{}}, {`\x6869`, []byte("hi")}, {`\xAB`, []byte{0xab}}, {`\x 12`, []byte{0x12}}, {`\x68 69`, []byte("hi")},
			{`ab\\cd`, []byte(`ab\cd`)}, {`a\101`, []byte("aA")}, {`abc`, []byte("abc")}, {`a\000b`, []byte{'a', 0, 'b'}}, {`ä`, []byte("ä")}, {``, []byte{}},
		} {
			got, err := value.Parse(test.in, value.Bytes)
			is.NotError(t, err, test.in)
			is.EqualSlice(t, test.want, got.Raw, test.in)
		}
		for _, in := range []string{`\x1`, `\x0G`, `a\`, `a\1`, `a\400`, `a\777`, `\X6869`, ` \x6869`} {
			_, err := value.Parse(in, value.Bytes)
			is.Error(t, value.ErrInvalidInput, err, in)
		}
		got, err := value.Parse(`\\x01`, value.Bytes)
		is.NotError(t, err)
		is.Equal(t, `\x01`, string(got.Raw), "an escaped backslash followed by text")
	})

	t.Run("reads ISO 8601 timestamps as timestamptz_in does, rounding to microseconds half to even", func(t *testing.T) {
		at := func(y int, mo time.Month, d, h, mi, s, usec int) time.Time {
			return time.Date(y, mo, d, h, mi, s, usec*1000, time.UTC)
		}
		for _, test := range []struct {
			in   string
			want time.Time
		}{
			{"2024-01-02 03:04:05.5+00", at(2024, 1, 2, 3, 4, 5, 500000)},
			{"2024-01-02T03:04:05.500+00", at(2024, 1, 2, 3, 4, 5, 500000)},
			{"2024-01-02 03:04:05.000001+00", at(2024, 1, 2, 3, 4, 5, 1)},
			{"2024-1-2 3:4:5", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05+0530", at(2024, 1, 1, 21, 34, 5, 0)},
			{"2024-01-02 03:04:05.5 +05:30", at(2024, 1, 1, 21, 34, 5, 500000)},
			{"2024-01-02T03:04:05+05:30", at(2024, 1, 1, 21, 34, 5, 0)},
			{"2024-01-02 03:04:05-0530", at(2024, 1, 2, 8, 34, 5, 0)},
			{"2024-01-02 03:04:05 +05", at(2024, 1, 1, 22, 4, 5, 0)},
			{"2024-01-02  03:04:05", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02T03:04:05.5", at(2024, 1, 2, 3, 4, 5, 500000)},
			{" 2024-01-02 ", at(2024, 1, 2, 0, 0, 0, 0)},
			{"2024-01-02 03:04", at(2024, 1, 2, 3, 4, 0, 0)},
			{"2024-01-02 03:04:5", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05 Z", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05Z ", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05z", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05+00:00", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05.", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 24:00:00", at(2024, 1, 3, 0, 0, 0, 0)},
			{"2024-01-02 23:59:60", at(2024, 1, 3, 0, 0, 0, 0)},
			{"2024-01-02 03:04:05+15", at(2024, 1, 1, 12, 4, 5, 0)},
			{"2024-01-02 03:04:05+1501", at(2024, 1, 1, 12, 3, 5, 0)},
			{"2024-01-02 03:04:05+14:59", at(2024, 1, 1, 12, 5, 5, 0)},
			{"2024-01-02 03:04:05.0000005+00", at(2024, 1, 2, 3, 4, 5, 0)},
			{"2024-01-02 03:04:05.0000015+00", at(2024, 1, 2, 3, 4, 5, 2)},
			{"2024-01-02 03:04:05.0000025+00", at(2024, 1, 2, 3, 4, 5, 2)},
			{"2024-01-02 03:04:05.0000035+00", at(2024, 1, 2, 3, 4, 5, 4)},
			{"2024-01-02 03:04:05.00000050000000001+00", at(2024, 1, 2, 3, 4, 5, 1)},
			{"2024-01-02 03:04:05.1234565+00", at(2024, 1, 2, 3, 4, 5, 123456)},
			{"2024-01-02 03:04:05.1234567+00", at(2024, 1, 2, 3, 4, 5, 123457)},
			{"2024-01-02 03:04:05.1234575+00", at(2024, 1, 2, 3, 4, 5, 123458)},
			{"2024-01-02 03:04:05.1234595+00", at(2024, 1, 2, 3, 4, 5, 123460)},
			{"2024-01-02 03:04:05.9999995+00", at(2024, 1, 2, 3, 4, 6, 0)},
			{"2024-01-02 03:04:05.123456789Z", at(2024, 1, 2, 3, 4, 5, 123457)},
			{"10000-01-01", at(10000, 1, 1, 0, 0, 0, 0)},
			{"0999-01-01", at(999, 1, 1, 0, 0, 0, 0)},
			{"0001-01-01 00:00:00+00", at(1, 1, 1, 0, 0, 0, 0)},
			{"294276-12-31 23:59:59.999999+00", at(294276, 12, 31, 23, 59, 59, 999999)},
			{"294277-01-01 00:00:00+01", at(294276, 12, 31, 23, 0, 0, 0)},
		} {
			got, err := value.Parse(test.in, value.Time)
			is.NotError(t, err, test.in)
			is.True(t, got.T.Equal(test.want), test.in+" gave "+got.T.String())
		}
		for _, in := range []string{"2024-13-01", "0000-01-01", "2024-02-30", "2024-01-02 03:04:05-16", "2024-01-02 03:04:05+05:99", "2024-01-02 03:04:05+0560", "", "2024-01-02 25:00", "2024-01-02 24:00:01", "2024-01-02 03:60", "2024-01-02 03:04:61"} {
			_, err := value.Parse(in, value.Time)
			is.Error(t, value.ErrInvalidInput, err, in)
		}
		for _, in := range []string{"January 2, 2024", "now", "epoch", "infinity", "4714-11-24 00:00:00+00 BC", "2024-01-02 03:04:05 UTC", "20240102", "99-01-01", "1-01-01", "2024-01-02 03:04:05 PM", "today", "2024-01-02 03", "2024-01-02T", "2024-01-02 03:04:05+", "-29", "2024-01"} {
			_, err := value.Parse(in, value.Time)
			is.Error(t, value.ErrUnsupportedInput, err, in)
			is.True(t, !errors.Is(err, value.ErrInvalidInput), in)
		}
		_, err := value.Parse("0001-01-01 00:00:00+01", value.Time)
		is.Error(t, value.ErrUnsupportedInput, err, "before year 1 in UTC is written BC")
		_, err = value.Parse("294277-01-01 00:00:00+00", value.Time)
		is.Error(t, value.ErrOutOfRange, err)
	})

	t.Run("converts numeric literals to bigint half away from zero and to double precision by text", func(t *testing.T) {
		for _, test := range []struct {
			in   string
			want int64
		}{
			{"2.5", 3}, {"3.5", 4}, {"-2.5", -3}, {"1.5e1", 15}, {"1.50", 2}, {"0.49999", 0}, {"-0.5", -1}, {"9223372036854775807.4", math.MaxInt64}, {"1e2", 100},
		} {
			got, err := value.NumericToInt(test.in)
			is.NotError(t, err, test.in)
			is.Equal(t, test.want, got, test.in)
		}
		for _, in := range []string{"9223372036854775807.5", "9223372036854775808", "1e400", "-9223372036854775808.5"} {
			_, err := value.NumericToInt(in)
			is.Error(t, value.ErrOutOfRange, err, in)
		}
		f, err := value.NumericToFloat("1.5e1")
		is.NotError(t, err)
		is.Equal(t, 15.0, f)
		_, err = value.NumericToFloat("1e400")
		is.Error(t, value.ErrOutOfRange, err)
	})
}

func TestFormat(t *testing.T) {
	t.Run("formats every type as Postgres outputs it in UTC", func(t *testing.T) {
		for _, test := range []struct {
			v    value.Value
			want string
		}{
			{value.NewInt(1), "1"}, {value.NewInt4(-5), "-5"}, {value.NewFloat(1), "1"}, {value.NewFloat(1e20), "1e+20"}, {value.NewFloat(1e15), "1e+15"},
			{value.NewFloat(1e14), "100000000000000"}, {value.NewFloat(0.0001), "0.0001"}, {value.NewFloat(0.00001), "1e-05"}, {value.NewFloat(1.5e-7), "1.5e-07"},
			{value.NewFloat(123456789.123456789), "123456789.12345679"}, {value.NewFloat(1e21), "1e+21"}, {value.NewFloat(1e15 + 0.5), "1.0000000000000005e+15"},
			{value.NewFloat(999999999999999.9), "999999999999999.9"}, {value.NewFloat(123456789012345678), "1.2345678901234568e+17"}, {value.NewFloat(math.Copysign(0, -1)), "-0"},
			{value.NewFloat(math.NaN()), "NaN"}, {value.NewFloat(math.Inf(-1)), "-Infinity"}, {value.NewFloat(5e-324), "5e-324"}, {value.NewFloat(-1.5), "-1.5"},
			{value.NewBool(true), "true"}, {value.NewBool(false), "false"}, {value.NewString("x"), "x"},
			{value.NewBytes([]byte("hi")), `\x6869`}, {value.NewBytes(nil), `\x`}, {value.NewBytes([]byte{0xab}), `\xab`},
			{value.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 500000000, time.UTC)), "2024-01-02 03:04:05.5+00"},
			{value.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)), "2024-01-02 03:04:05+00"},
			{value.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 1000, time.UTC)), "2024-01-02 03:04:05.000001+00"},
			{value.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 100000000, time.UTC)), "2024-01-02 03:04:05.1+00"},
			{value.NewTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)), "10000-01-01 00:00:00+00"},
			{value.NewTime(time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)), "0999-01-01 00:00:00+00"},
			{value.NewTime(time.Date(0, 12, 31, 23, 0, 0, 0, time.UTC)), "0001-12-31 23:00:00+00 BC"},
			{value.NewTime(time.Date(2024, 1, 2, 5, 4, 5, 0, time.FixedZone("x", 2*3600))), "2024-01-02 03:04:05+00"},
		} {
			is.Equal(t, test.want, value.Format(test.v), test.v.String())
		}
	})

	t.Run("NewTime keeps microseconds only", func(t *testing.T) {
		v := value.NewTime(time.Date(2024, 1, 2, 3, 4, 5, 123456789, time.UTC))
		is.Equal(t, 123456000, v.T.Nanosecond())
	})
}

func TestParseFormat_Properties(t *testing.T) {
	t.Run("parsing the text output of a value gives the value back, for every column type", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			typ := hegel.Draw(ht, hegel.SampledFrom(allTypes))
			v := drawValue(ht, typ, false)
			if typ == value.Time {
				// Postgres's range, which Format and Parse cover.
				sec := hegel.Draw(ht, hegel.Integers[int64](-62135596800, 9224318015999))
				usec := hegel.Draw(ht, hegel.Integers[int64](0, 999999))
				v = value.NewTime(time.Unix(sec, usec*1000))
			}
			text := value.Format(v)
			ht.Note(text)
			got, err := value.Parse(text, typ)
			if err != nil {
				ht.Fatalf("parse %q: %v", text, err)
			}
			if typ == value.Float && math.IsNaN(v.F) {
				if !math.IsNaN(got.F) {
					ht.Fatalf("NaN read back as %v", got.F)
				}
				return
			}
			if value.Compare(v, got) != 0 || (typ == value.Float && math.Signbit(v.F) != math.Signbit(got.F)) {
				ht.Fatalf("%v read back as %v", v, got)
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("a fraction of seconds rounds to the nearest microsecond, and to even on a tie", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			usec := hegel.Draw(ht, hegel.Integers(0, 999999))
			extra := hegel.Draw(ht, hegel.Text().Alphabet("0123456789").MaxSize(6))
			text := "2024-01-02 03:04:05." + leftPad(usec) + extra + "+00"
			got, err := value.Parse(text, value.Time)
			if err != nil {
				ht.Fatalf("parse %q: %v", text, err)
			}
			want := usec
			switch trimmed := trimRightZeros(extra); {
			case trimmed == "", trimmed[0] < '5':
			case trimmed[0] > '5', len(trimmed) > 1, usec%2 == 1:
				want++
			}
			gotUsec := got.T.Nanosecond()/1000 + 1000000*(got.T.Second()-5)
			if gotUsec != want {
				ht.Fatalf("%q gave %d microseconds, want %d", text, gotUsec, want)
			}
		}, hegel.WithTestCases(2000))
	})
}

func leftPad(usec int) string {
	s := "000000" + itoa(usec)
	return s[len(s)-6:]
}

func itoa(n int) string {
	return value.NewInt(int64(n)).String()
}

func trimRightZeros(s string) string {
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	return s
}
