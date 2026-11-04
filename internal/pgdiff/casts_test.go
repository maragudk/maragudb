package pgdiff_test

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/verify"
)

// castInputs are the characters of the text that the cast property reads, chosen so that most
// draws are close to a valid value of some type.
var castInputs = []string{"0", "1", "5", "9", "-", "+", ".", "e", "E", " ", "x", "X", "0x", "0o", "0b", "_", "\\", "\\x", "a", "f", "t", "rue", "alse", "on", "off", "yes", "no",
	"nan", "NaN", "inf", "Infinity", "2024", "-01", "-02", "-29", "-31", "-13", " 03", "T", ":04", ":05", ".123456", "789", "Z", "+02", "-0130", "+14:59", ":60", ":99", "24:00:00", "1e308", "1e-320", "9223372036854775807", "9223372036854775808"}

func TestCasts(t *testing.T) {
	t.Run("text casts to every type, and back to text, the same as Postgres, or are rejected as unsupported", func(t *testing.T) {
		verify.Covers(t, "hegel-drawn text near valid input for bigint, double precision, boolean, bytea, and timestamptz casts to the type and back to text with the same result or the same error class as Postgres, unless maragudb rejects it as unsupported", "INV-PG-SUBSET", "INV-TYPES")
		b := newBoth(t, "")
		types := []string{"bigint", "double precision", "boolean", "bytea", "timestamptz"}
		hegel.Test(t, func(ht *hegel.T) {
			typ := hegel.Draw(ht, hegel.SampledFrom(types))
			var text string
			for range hegel.Draw(ht, hegel.Integers(0, 8)) {
				text += hegel.Draw(ht, hegel.SampledFrom(castInputs))
			}
			query := fmt.Sprintf("SELECT CAST(CAST(%s AS %s) AS text)", ast.QuoteString(text), typ)
			ht.Note(query)
			var m, pg sql.NullString
			mErr := b.m.QueryRowContext(t.Context(), query).Scan(&m)
			if errors.Is(mErr, maragudb.ErrUnsupported) {
				return
			}
			pgErr := b.pg.QueryRowContext(t.Context(), query).Scan(&pg)
			switch {
			case (mErr == nil) != (pgErr == nil):
				ht.Fatalf("%s: maragudb %q, %v; postgres %q, %v", query, m.String, mErr, pg.String, pgErr)
			case mErr != nil && !pgdiff.SameClass(mErr, pgErr):
				ht.Fatalf("%s: maragudb %v; postgres SQLSTATE %s, %v", query, mErr, pgdiff.SQLState(pgErr), pgErr)
			case m != pg:
				ht.Fatalf("%s: maragudb %q, postgres %q", query, m.String, pg.String)
			}
		}, hegel.WithTestCases(castCases()))
	})
}

func TestParameters(t *testing.T) {
	t.Run("a parameter of every Go type in every context gives the same result as Postgres with pgx, or is rejected as unsupported", func(t *testing.T) {
		verify.Covers(t, "hegel-drawn Go values bound as parameters in contexts that infer different types give the same result or error class as Postgres with pgx, or fail on both when pgx refuses to encode them, unless maragudb rejects them as unsupported", "INV-PLACEHOLDERS", "INV-PG-SUBSET")
		b := newBoth(t, "")
		contexts := []string{"SELECT $1::bigint", "SELECT $1 + 1", "SELECT $1 + 2147483647", "SELECT $1 + 0.5::float8", "SELECT $1 || 'x'", "SELECT CASE WHEN $1 THEN 1 END",
			"SELECT length($1)", "SELECT $1 = 'a'", "SELECT coalesce($1, 1)", "SELECT $1 LIKE 'a%'", "SELECT abs($1)", "SELECT $1::timestamptz", "SELECT $1::bytea",
			"SELECT $1::text", "SELECT $1 = 1.5::float8", "SELECT $1 = '\\x01'::bytea", "SELECT $1 < '2024-01-01'::timestamptz", "SELECT NOT $1", "SELECT 1 LIMIT $1"}
		hegel.Test(t, func(ht *hegel.T) {
			query := hegel.Draw(ht, hegel.SampledFrom(contexts))
			var arg any
			switch hegel.Draw(ht, hegel.Integers(0, 7)) {
			case 0:
				arg = hegel.Draw(ht, hegel.OneOf(hegel.Integers[int64](-3, 3), hegel.Integers[int64](-1<<40, 1<<40), hegel.SampledFrom([]int64{math.MaxInt64, math.MinInt64, math.MaxInt32, math.MaxInt32 + 1})))
			case 1:
				arg = hegel.Draw(ht, hegel.OneOf(hegel.Floats[float64](), hegel.SampledFrom([]float64{0, 1, 2.5, -1, 1e20})))
			case 2:
				arg = hegel.Draw(ht, hegel.Booleans())
			case 3:
				var text string
				for range hegel.Draw(ht, hegel.Integers(0, 4)) {
					text += hegel.Draw(ht, hegel.SampledFrom(castInputs))
				}
				arg = text
			case 4:
				arg = hegel.Draw(ht, hegel.Binary(0, 3))
			case 5:
				arg = time.Unix(hegel.Draw(ht, hegel.Integers[int64](-1e10, 1e10)), hegel.Draw(ht, hegel.Integers[int64](0, 999999999)))
			default:
				arg = nil
			}
			ht.Note(fmt.Sprintf("%s with %#v", query, arg))
			var m, pg any
			mErr := b.m.QueryRowContext(t.Context(), query, arg).Scan(&m)
			if errors.Is(mErr, maragudb.ErrUnsupported) {
				return
			}
			pgErr := b.pg.QueryRowContext(t.Context(), query, arg).Scan(&pg)
			switch {
			case pgErr != nil && pgdiff.SQLState(pgErr) == "":
				// pgx refused to encode the value, so the statement never reached Postgres.
				if mErr == nil {
					ht.Fatalf("maragudb %v; postgres client %v", m, pgErr)
				}
			case (mErr == nil) != (pgErr == nil):
				ht.Fatalf("maragudb %#v, %v; postgres %#v, %v", m, mErr, pg, pgErr)
			case mErr != nil && !pgdiff.SameClass(mErr, pgErr):
				ht.Fatalf("maragudb %v; postgres SQLSTATE %s, %v", mErr, pgdiff.SQLState(pgErr), pgErr)
			case mErr == nil && fmt.Sprint(canonicalValue(m)) != fmt.Sprint(canonicalValue(pg)):
				ht.Fatalf("maragudb %#v, postgres %#v", m, pg)
			}
		}, hegel.WithTestCases(castCases()))
	})
}

// canonicalValue makes values that are equal compare equal: times as UTC instants, and every
// NaN the same.
func canonicalValue(v any) any {
	switch v := v.(type) {
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case float64:
		if math.IsNaN(v) {
			return "NaN"
		}
		return fmt.Sprintf("%v(%x)", v, math.Float64bits(v))
	case []byte:
		return fmt.Sprintf("%x", v)
	}
	return v
}

// castCases is 2000, or MARAGUDB_PGDIFF_CASES when set.
func castCases() int {
	if n, err := strconv.Atoi(os.Getenv("MARAGUDB_PGDIFF_CASES")); err == nil {
		return n
	}
	return 2000
}
