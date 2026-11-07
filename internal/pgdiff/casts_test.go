package pgdiff_test

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

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

	t.Run("every value casts to every type the same as Postgres, or is rejected as unsupported", func(t *testing.T) {
		verify.Covers(t, "hegel-drawn values of every supported type, and integer and numeric literals, cast to every supported type and back to text with the same result or error class as Postgres, unless maragudb rejects the cast as unsupported", "INV-PG-SUBSET", "INV-TYPES")
		b := newBoth(t, "")
		sources := []string{"0", "1", "-7", "2147483647", "9223372036854775807", "1.5", "-0.5", "1e400", "'NaN'::float8", "'-0'::float8", "2.5::float8", "1e308::float8", "TRUE", "FALSE", "'t'", "'abc'", "''",
			"'\\x0102'::bytea", "'\\x'::bytea", "'\\x0102030405060708090a'::bytea", "'2024-01-02 03:04:05.5+00'::timestamptz", "NULL", "9223372036854775807::bigint", "'12'::text", "' 12 '"}
		targets := []string{"bigint", "double precision", "boolean", "text", "bytea", "timestamptz", "integer", "numeric"}
		hegel.Test(t, func(ht *hegel.T) {
			query := fmt.Sprintf("SELECT CAST(CAST(%s AS %s) AS text)", hegel.Draw(ht, hegel.SampledFrom(sources)), hegel.Draw(ht, hegel.SampledFrom(targets)))
			if hegel.Draw(ht, hegel.Booleans()) {
				query = fmt.Sprintf("SELECT CAST(CAST(CAST(%s AS %s) AS %s) AS text)", hegel.Draw(ht, hegel.SampledFrom(sources)), hegel.Draw(ht, hegel.SampledFrom(targets)), hegel.Draw(ht, hegel.SampledFrom(targets)))
			}
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
		verify.Covers(t, "hegel-drawn Go values bound as parameters in contexts that infer different types, among them times at and beyond both ends of timestamptz to the nanosecond and strings that are not UTF-8 or hold NUL, give the same result or error class as Postgres with pgx, or fail on both when pgx refuses to encode them, unless maragudb rejects them as unsupported", "INV-PLACEHOLDERS", "INV-PG-SUBSET")
		b := newBoth(t, "")
		contexts := []string{"SELECT $1::bigint", "SELECT $1 + 1", "SELECT $1 + 2147483647", "SELECT $1 + 0.5::float8", "SELECT $1 || 'x'", "SELECT CASE WHEN $1 THEN 1 END",
			"SELECT length($1)", "SELECT $1 = 'a'", "SELECT coalesce($1, 1)", "SELECT $1 LIKE 'a%'", "SELECT abs($1)", "SELECT $1::timestamptz", "SELECT $1::bytea",
			"SELECT $1::text", "SELECT $1 = 1.5::float8", "SELECT $1 = '\\x01'::bytea", "SELECT $1 < '2024-01-01'::timestamptz", "SELECT NOT $1", "SELECT 1 LIMIT $1",
			"SELECT CAST($1::timestamptz AS text)", "SELECT 1 WHERE $1::timestamptz IS NULL"}
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
				if hegel.Draw(ht, hegel.Integers(0, 4)) == 0 {
					// Text that is not valid UTF-8, or that holds a NUL, which Postgres refuses
					// whatever type the parameter has.
					text += hegel.Draw(ht, hegel.SampledFrom([]string{"\xff", "\x00", "\xc3", "\xed\xa0\x80", "\xf4\x90\x80\x80"}))
				}
				arg = text
			case 4:
				arg = hegel.Draw(ht, hegel.Binary(0, 3))
			case 5:
				arg = time.Unix(hegel.Draw(ht, hegel.Integers[int64](-1e10, 1e10)), hegel.Draw(ht, hegel.Integers[int64](0, 999999999)))
				if hegel.Draw(ht, hegel.Booleans()) {
					// Near either end of timestamptz, and at the year 1, by a few nanoseconds,
					// microseconds, or more, inside and outside.
					at := hegel.Draw(ht, hegel.SampledFrom(timestampBounds))
					offset := hegel.Draw(ht, hegel.SampledFrom([]time.Duration{0, 1, 999, 1000, 1001, time.Second, 24 * time.Hour}))
					if hegel.Draw(ht, hegel.Booleans()) {
						offset = -offset
					}
					arg = at.Add(offset)
				}
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

	t.Run("a statement whose parameters repeat in several contexts or skip a number gives the same result as Postgres with pgx, or is rejected as unsupported", func(t *testing.T) {
		verify.Covers(t, "hegel-drawn statements that reference parameters in several contexts each, skip parameter numbers, or put a parameter only where its type is not determined, such as IS NULL and count(), next to analysis and folding errors, give the same result or error class as Postgres with pgx, among them 42P18 for a parameter whose type Postgres cannot determine and the error of the lowest refused value after every analysis error, unless maragudb rejects them as unsupported", "INV-PLACEHOLDERS", "INV-PG-SUBSET")
		b := newBoth(t, "")
		// Each context is an expression with $%[1]d for a parameter, or none.
		contexts := []string{"$%[1]d::bigint", "$%[1]d + 1", "$%[1]d || 'x'", "$%[1]d = 'a'", "$%[1]d IS NULL", "count($%[1]d)", "coalesce($%[1]d, 1)", "length($%[1]d)", "$%[1]d::text",
			"CASE WHEN $%[1]d THEN 1 END", "$%[1]d = 1.5::float8", "NOT $%[1]d", "$%[1]d", "CASE WHEN false THEN $%[1]d = 5 END", "$%[1]d::bigint + $%[1]d::bigint", "nope", "1 / 0"}
		hegel.Test(t, func(ht *hegel.T) {
			var items []string
			n := 0
			for range hegel.Draw(ht, hegel.Integers(1, 3)) {
				p := hegel.Draw(ht, hegel.Integers(1, 3))
				n = max(n, p)
				items = append(items, fmt.Sprintf(hegel.Draw(ht, hegel.SampledFrom(contexts)), p))
			}
			query := "SELECT " + strings.Join(items, ", ")
			if !strings.Contains(query, "$") {
				return
			}
			args := make([]any, n)
			for i := range args {
				switch hegel.Draw(ht, hegel.Integers(0, 4)) {
				case 0:
					args[i] = hegel.Draw(ht, hegel.SampledFrom([]int64{0, 1, -7, math.MaxInt64}))
				case 1:
					args[i] = hegel.Draw(ht, hegel.SampledFrom([]string{"1", "t", "a", "abc", "9223372036854775807", "1.5", "a\xffb"}))
				case 2:
					args[i] = nil
				default:
					args[i] = hegel.Draw(ht, hegel.SampledFrom([]string{"5", "0", "-1", "x", ""}))
				}
			}
			ht.Note(fmt.Sprintf("%s with %#v", query, args))
			m, mErr := queryRow(t, b.m, query, args)
			if errors.Is(mErr, maragudb.ErrUnsupported) {
				return
			}
			pg, pgErr := queryRow(t, b.pg, query, args)
			switch {
			case pgErr != nil && pgdiff.SQLState(pgErr) == "" && strings.Contains(pgErr.Error(), "failed to encode"):
				// pgx refused to encode a value, which maragudb refuses as out of range.
				if !errors.Is(mErr, maragudb.ErrOutOfRange) {
					ht.Fatalf("maragudb %v, %v; postgres client %v", m, mErr, pgErr)
				}
			case (mErr == nil) != (pgErr == nil):
				ht.Fatalf("maragudb %#v, %v; postgres %#v, %v", m, mErr, pg, pgErr)
			case mErr != nil && !pgdiff.SameClass(mErr, pgErr):
				ht.Fatalf("maragudb %v; postgres SQLSTATE %s, %v", mErr, pgdiff.SQLState(pgErr), pgErr)
			case mErr == nil && fmt.Sprint(m) != fmt.Sprint(pg):
				ht.Fatalf("maragudb %#v, postgres %#v", m, pg)
			}
		}, hegel.WithTestCases(castCases()/4))
	})

	t.Run("a SELECT whose parameters repeat across its clauses gives the same result as Postgres with pgx, or is rejected as unsupported", func(t *testing.T) {
		verify.Covers(t, "hegel-drawn SELECTs on a table that reference parameters in several contexts each, in the output, WHERE, ORDER BY, and LIMIT, with typed contexts, IS NULL, coalesce, CASE, IN, BETWEEN, and comparisons of two parameters, give the same result or error class as Postgres with pgx, which types each parameter at its first context in the order in which it analyzes the clauses, unless maragudb rejects them as unsupported", "INV-PLACEHOLDERS", "INV-PG-SUBSET")
		b := newBoth(t, "CREATE TABLE dev (k bigint PRIMARY KEY, i bigint, s text, f double precision, b boolean)")
		b.exec(t, "INSERT INTO dev VALUES (1, 10, 'a', 1.5, TRUE), (2, 20, 'b', 2.5, FALSE), (3, NULL, NULL, NULL, NULL)")
		// Each context has $%[1]d for a parameter and $%[2]d for another one.
		outputs := []string{"$%[1]d", "$%[1]d IS NULL", "k + $%[1]d", "coalesce($%[1]d, s)", "$%[1]d::bigint", "CASE WHEN $%[1]d IS NULL THEN 0 ELSE k END", "$%[1]d || s", "+$%[1]d"}
		conditions := []string{"k = $%[1]d", "s = $%[1]d", "f > $%[1]d", "b = $%[1]d", "$%[1]d IS NULL", "$%[1]d IS NOT NULL", "k = $%[1]d OR $%[1]d IS NULL", "$%[1]d IS NULL OR k = $%[1]d",
			"$%[1]d::bigint IS NULL OR k = $%[1]d", "k = coalesce($%[1]d, k)", "$%[1]d + 1 > k", "$%[1]d IN (k, i)", "k IN ($%[1]d, 2)", "k BETWEEN $%[1]d AND 5", "$%[1]d BETWEEN k AND 5",
			"CASE WHEN $%[1]d IS NULL THEN TRUE ELSE s = $%[1]d END", "length($%[1]d) > 0", "$%[1]d = $%[2]d", "coalesce($%[1]d, $%[2]d) = k", "$%[1]d"}
		orders := []string{"k + $%[1]d", "$%[1]d IS NULL", "coalesce($%[1]d, k)"}
		hegel.Test(t, func(ht *hegel.T) {
			n := 0
			param := func() int {
				p := hegel.Draw(ht, hegel.Integers(1, 2))
				n = max(n, p)
				return p
			}
			context := func(forms []string) string {
				p := param()
				return fmt.Sprintf(hegel.Draw(ht, hegel.SampledFrom(forms)), p, 3-p)
			}
			var items, conds []string
			for range hegel.Draw(ht, hegel.Integers(1, 2)) {
				items = append(items, context(outputs))
			}
			for range hegel.Draw(ht, hegel.Integers(0, 2)) {
				conds = append(conds, "("+context(conditions)+")")
			}
			query := "SELECT " + strings.Join(items, ", ") + " FROM dev"
			if len(conds) > 0 {
				query += " WHERE " + strings.Join(conds, " AND ")
			}
			query += " ORDER BY "
			if hegel.Draw(ht, hegel.Booleans()) {
				query += context(orders) + ", "
			}
			query += "k"
			if hegel.Draw(ht, hegel.Booleans()) {
				query += fmt.Sprintf(" LIMIT $%d", param())
			}
			if strings.Contains(query, "$2") {
				n = 2
			}
			args := make([]any, n)
			for i := range args {
				args[i] = hegel.Draw(ht, hegel.SampledFrom([]any{nil, int64(1), int64(2), "1", "a", "t", "2.5", 1.5, true}))
			}
			ht.Note(fmt.Sprintf("%s with %#v", query, args))
			m, mErr := queryRow(t, b.m, query, args)
			if errors.Is(mErr, maragudb.ErrUnsupported) {
				return
			}
			pg, pgErr := queryRow(t, b.pg, query, args)
			switch {
			case pgErr != nil && pgdiff.SQLState(pgErr) == "":
				// pgx refused to encode a value for the type that Postgres gave its parameter.
				if mErr == nil {
					ht.Fatalf("maragudb %v; postgres client %v", m, pgErr)
				}
			case (mErr == nil) != (pgErr == nil):
				ht.Fatalf("maragudb %#v, %v; postgres %#v, %v", m, mErr, pg, pgErr)
			case mErr != nil && !pgdiff.SameClass(mErr, pgErr):
				ht.Fatalf("maragudb %v; postgres SQLSTATE %s, %v", mErr, pgdiff.SQLState(pgErr), pgErr)
			case mErr == nil && fmt.Sprint(m) != fmt.Sprint(pg):
				ht.Fatalf("maragudb %#v, postgres %#v", m, pg)
			}
		}, hegel.WithTestCases(castCases()/4))
	})

	t.Run("a parameter gets the type of its first context in the order in which Postgres analyzes the statement, and later references have that type", func(t *testing.T) {
		verify.Covers(t, "statements that reference a parameter in several contexts, among them the optional filters k = $1 OR $1 IS NULL, ($1::bigint IS NULL OR k = $1), coalesce($1, k), and k = coalesce($1, k), and references across the WITH clause, FROM and join conditions, the output, WHERE, HAVING, ORDER BY, GROUP BY, OFFSET, LIMIT, subqueries, INSERT rows, and UPDATE SET and WHERE, give Postgres's result, or its error class: 42P08 for a reference made before the parameter has a type that a later context coerces to another type or reads in IS NULL, and the error of a later reference whose context does not take the type", "INV-PLACEHOLDERS", "INV-PG-SUBSET")
		b := newBoth(t, "CREATE TABLE dev (k bigint PRIMARY KEY, i bigint, s text, f double precision, b boolean)")
		b.exec(t, "INSERT INTO dev VALUES (1, 10, 'a', 1.5, TRUE), (2, 20, 'b', 2.5, FALSE), (3, NULL, NULL, NULL, NULL)")
		b.exec(t, "CREATE TABLE dev2 (k bigint PRIMARY KEY, j bigint)")
		b.exec(t, "INSERT INTO dev2 VALUES (1, 100), (2, 200)")
		tests := []struct {
			sql  string
			args []any
			// code is the SQLSTATE that Postgres fails with, or "" when it succeeds.
			code string
		}{
			// Optional filters.
			{sql: "SELECT k FROM dev WHERE k = $1 OR $1 IS NULL ORDER BY k", args: []any{int64(2)}},
			{sql: "SELECT k FROM dev WHERE k = $1 OR $1 IS NULL ORDER BY k", args: []any{nil}},
			{sql: "SELECT k FROM dev WHERE s = $1 OR $1 IS NULL ORDER BY k", args: []any{"a"}},
			{sql: "SELECT k FROM dev WHERE (k = $1 OR $1 IS NULL) AND (s = $2 OR $2 IS NULL) AND (f > $3 OR $3 IS NULL) ORDER BY k LIMIT $4", args: []any{nil, nil, 1.0, int64(10)}},
			{sql: "SELECT k FROM dev WHERE $1 IS NULL OR k = $1 ORDER BY k", args: []any{int64(2)}, code: "42P08"},
			{sql: "SELECT k FROM dev WHERE ($1::bigint IS NULL OR k = $1) ORDER BY k", args: []any{nil}},
			{sql: "SELECT k FROM dev WHERE ($1::bigint IS NULL OR k = $1) ORDER BY k", args: []any{int64(1)}},
			{sql: "SELECT coalesce($1, k) FROM dev ORDER BY k", args: []any{nil}},
			{sql: "SELECT k FROM dev WHERE k = coalesce($1, k) ORDER BY k", args: []any{nil}},
			{sql: "SELECT k FROM dev WHERE k = coalesce($1, k) ORDER BY k", args: []any{"2"}},
			{sql: "SELECT count(*) FROM dev GROUP BY k HAVING k > $1 OR $1 IS NULL ORDER BY k", args: []any{int64(1)}},
			// The order of the clauses.
			{sql: "SELECT $1 IS NULL FROM dev WHERE k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k = $1 FROM dev WHERE $1 IS NULL", args: []any{int64(1)}},
			{sql: "SELECT dev.k FROM dev JOIN dev2 ON $1 IS NULL WHERE dev.k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT $1 IS NULL FROM dev JOIN dev2 ON dev.k = $1", args: []any{int64(1)}},
			{sql: "SELECT dev.k FROM dev JOIN dev2 ON dev.k = dev2.k AND dev.k = $1 JOIN dev2 AS v ON $1 IS NULL", args: []any{int64(1)}},
			{sql: "SELECT dev.k FROM dev JOIN dev2 ON dev.k = dev2.k AND $1 IS NULL JOIN dev2 AS v ON v.k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT d.k FROM (SELECT k FROM dev2 WHERE k = $1) AS d, (SELECT k FROM dev2 WHERE $1 IS NULL) AS e", args: []any{int64(1)}},
			{sql: "SELECT d.k FROM (SELECT k FROM dev2 WHERE $1 IS NULL) AS d, (SELECT k FROM dev2 WHERE k = $1) AS e", args: []any{int64(1)}, code: "42P08"},
			{sql: "WITH c AS (SELECT k FROM dev2 WHERE k = $1) SELECT k FROM dev WHERE $1 IS NULL", args: []any{int64(1)}},
			{sql: "WITH c AS (SELECT k FROM dev2 WHERE $1 IS NULL) SELECT k FROM c WHERE k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT count(*) FROM dev WHERE k > $1 HAVING $1 IS NULL", args: []any{int64(1)}},
			{sql: "SELECT count(*) FROM dev WHERE $1 IS NULL HAVING count(*) > $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev GROUP BY k HAVING $1 IS NULL ORDER BY k + $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k, count(*) FROM dev GROUP BY k, $1 IS NULL ORDER BY k + $1", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev GROUP BY k, k + $1 ORDER BY $1 IS NULL, k", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev ORDER BY k LIMIT CASE WHEN $1 IS NULL THEN 1 END OFFSET $1", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev ORDER BY k LIMIT $1 OFFSET CASE WHEN $1 IS NULL THEN 1 END", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev WHERE EXISTS (SELECT 1 FROM dev2 WHERE dev2.k = dev.k AND $1 IS NULL) AND k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev WHERE k = $1 AND EXISTS (SELECT 1 FROM dev2 WHERE dev2.k = dev.k AND $1 IS NULL)", args: []any{int64(1)}},
			{sql: "SELECT (SELECT j FROM dev2 WHERE dev2.k = $1), $1 IS NULL FROM dev WHERE k = 1", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev WHERE (CASE WHEN $1 IS NULL THEN 1 END) IN (SELECT k FROM dev2 WHERE k = $1)", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev WHERE k IN (SELECT $1 FROM dev2)", args: []any{"1"}, code: "42883"},
			// Output columns get text at the end of their query, or where ORDER BY, GROUP BY, or
			// DISTINCT reads them.
			{sql: "SELECT $1, $1::bigint", args: []any{"1"}, code: "42P08"},
			{sql: "SELECT $1::bigint, $1", args: []any{int64(1)}},
			{sql: "SELECT $1 FROM dev WHERE k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT $1 FROM dev WHERE s = $1", args: []any{"a"}},
			{sql: "SELECT $1 FROM dev ORDER BY 1 LIMIT $1", args: []any{"1"}, code: "42804"},
			{sql: "SELECT $1 FROM dev GROUP BY 1 HAVING $1 > 0", args: []any{"1"}, code: "42P08"},
			{sql: "SELECT DISTINCT $1 FROM dev WHERE k = $1", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT (SELECT $1), $1::bigint", args: []any{"1"}},
			// Contexts that coerce right after their operand.
			{sql: "SELECT k FROM dev WHERE $1 OR k = $1", args: []any{true}, code: "42883"},
			{sql: "SELECT CASE WHEN $1 THEN $1 END FROM dev", args: []any{true}},
			{sql: "SELECT CASE $1 WHEN 'a' THEN $1 ELSE 1 END", args: []any{"a"}, code: "42804"},
			{sql: "SELECT CASE k WHEN $1 THEN $1 IS NULL END FROM dev ORDER BY k", args: []any{int64(1)}},
			{sql: "SELECT CASE WHEN $1 IS NULL THEN 1 WHEN k = $1 THEN 2 END FROM dev", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT coalesce($1, $1::bigint)", args: []any{int64(1)}},
			{sql: "SELECT coalesce($1 IS NULL, $1::bigint > 0)", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT coalesce($1::bigint > 0, $1 IS NULL)", args: []any{int64(1)}},
			// BETWEEN reads its operand twice, and IN compares the one reference with each item.
			{sql: "SELECT k FROM dev WHERE k BETWEEN $1 AND $1 + 1 ORDER BY k", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev WHERE $1 BETWEEN k AND 5 ORDER BY k", args: []any{int64(1)}},
			{sql: "SELECT 1 WHERE $1 BETWEEN 1 AND NULL", args: []any{"2"}},
			{sql: "SELECT k FROM dev WHERE NOT ($1 BETWEEN k AND f) ORDER BY k", args: []any{int64(1)}},
			{sql: "SELECT $1 BETWEEN $1 AND TRUE", args: []any{"t"}, code: "42883"},
			{sql: "SELECT k FROM dev WHERE k IN ($1, $1 + 1)", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev WHERE $1 IN (k, 5)", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT k FROM dev WHERE k = $1 AND $1 IN (i, 5)", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev WHERE $1 IN (k, 5::bigint)", args: []any{int64(1)}},
			{sql: "SELECT k FROM dev WHERE $1 IN (k, NULL)", args: []any{"1"}, code: "42P08"},
			// A later reference has the type, which its context converts implicitly or not.
			{sql: "SELECT $1::bigint, $1 + 1", args: []any{int64(1)}},
			{sql: "SELECT $1 + 1, $1 + 5000000000", args: []any{int64(1)}},
			{sql: "SELECT $1 + 1, $1 + 5000000000", args: []any{"3000000000"}, code: "22003"},
			{sql: "SELECT $1::float8, $1 = 1", args: []any{1.5}},
			{sql: "SELECT $1::bigint, $1 || 'x'", args: []any{int64(1)}},
			{sql: "SELECT $1::bigint, $1 = 'abc'", args: []any{int64(1)}, code: "22P02"},
			{sql: "SELECT $1::bigint, length($1)", args: []any{int64(1)}, code: "42883"},
			{sql: "SELECT $1::text, abs($1)", args: []any{"-1"}, code: "42883"},
			{sql: "SELECT $1::bytea, $1 = 'abc'", args: []any{"abc"}},
			{sql: "SELECT $1::bigint, $1 = $2", args: []any{int64(1), int64(1)}},
			{sql: "SELECT $1 = $2, $2 + 1", args: []any{"1", "1"}, code: "42883"},
			{sql: "SELECT $1::bigint, coalesce($2, $1), coalesce($3, $2), coalesce($4, $3), coalesce($5, $4), length($5)", args: []any{int64(1), int64(2), int64(3), int64(4), int64(5)}, code: "42883"},
			{sql: "SELECT $1::bigint, coalesce($2, $1), coalesce($3, $2), coalesce($4, $3), coalesce($5, $4), $5 + 0.5::float8", args: []any{int64(1), int64(2), int64(3), int64(4), int64(5)}},
			{sql: "SELECT k FROM dev WHERE k = $1 AND s = $1", args: []any{int64(1)}, code: "42883"},
			{sql: "SELECT k FROM dev WHERE f = $1 AND k = $1", args: []any{1.5}},
			{sql: "SELECT k FROM dev WHERE k = $1 AND $1 + 1 > 0", args: []any{int64(math.MaxInt64)}, code: "22003"},
			{sql: "SELECT k FROM dev WHERE k = $1 AND s = $1::text", args: []any{int64(1)}},
			{sql: "SELECT count($1::bigint), count($1)", args: []any{int64(1)}},
			{sql: "SELECT count($1), $1::bigint", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT count(DISTINCT $1), $1::bigint", args: []any{"1"}},
			{sql: "SELECT +$1, $1 + 1", args: []any{"2"}},
			// Errors of the analysis come in Postgres's order.
			{sql: "SELECT $1, $1::bigint, nope", args: []any{int64(1)}, code: "42703"},
			{sql: "SELECT $1 IS NULL, $1::bigint, 1 / 0", args: []any{int64(1)}, code: "42P08"},
			{sql: "SELECT $1::bigint, $1 = 'abc', nope", args: []any{int64(1)}, code: "22P02"},
			{sql: "SELECT k FROM dev WHERE k = $1 AND s = $1 AND nope = 1", args: []any{int64(1)}, code: "42883"},
			{sql: "SELECT 1 WHERE $1 BETWEEN 1 AND -$2", args: []any{"1", "2"}, code: "42725"},
			{sql: "SELECT 1 WHERE $1 AND '\\x'::bytea IN ('\\x' || $2, CAST($1 AS bytea))", args: []any{nil, ""}, code: "42846"},
			{sql: "SELECT $1 IS NULL, $2 IS NULL, $2::bigint", args: []any{int64(1), int64(2)}, code: "42P08"},
			// Writes: VALUES rows one at a time, and UPDATE's WHERE before its SET, whose
			// expressions are all analyzed before they are coerced to their columns.
			{sql: "INSERT INTO dev2 VALUES ($1, $1)", args: []any{int64(5)}},
			{sql: "INSERT INTO dev VALUES ($1, NULL, $1, NULL, NULL)", args: []any{int64(6)}, code: "42P08"},
			{sql: "INSERT INTO dev VALUES ($1 + 0, NULL, $1, NULL, NULL)", args: []any{int64(6)}},
			{sql: "INSERT INTO dev VALUES ($1, NULL, 'x', NULL, NULL), (8, NULL, $1 || 'y', NULL, NULL)", args: []any{int64(7)}},
			{sql: "INSERT INTO dev VALUES (9, NULL, $1 || 'y', NULL, NULL), ($1, NULL, 'x', NULL, NULL)", args: []any{int64(10)}, code: "42804"},
			{sql: "INSERT INTO dev VALUES (11, NULL, $1, NULL, $1 IS NULL)", args: []any{"z"}, code: "42P08"},
			{sql: "INSERT INTO dev VALUES (12, NULL, 'q', NULL, $1), (13, NULL, CASE WHEN $1 IS NULL THEN 'n' END, NULL, NULL)", args: []any{true}},
			{sql: "UPDATE dev SET s = CASE WHEN $1 IS NULL THEN 'n' END WHERE k = $1", args: []any{int64(6)}},
			{sql: "UPDATE dev SET i = $1 + 100 WHERE $1 IS NULL", args: []any{int64(5)}, code: "42P08"},
			{sql: "UPDATE dev SET s = $1 WHERE k = $1", args: []any{int64(7)}},
			{sql: "UPDATE dev SET s = $1 || '', i = $1 WHERE k = 8", args: []any{int64(80)}, code: "42804"},
			{sql: "UPDATE dev SET i = $1, s = $1 WHERE k = 8", args: []any{int64(90)}, code: "42P08"},
			{sql: "UPDATE dev SET s = 'w' WHERE k = $1 OR $1 IS NULL", args: []any{int64(12)}},
			{sql: "DELETE FROM dev WHERE k = $1 OR $1 IS NULL", args: []any{int64(13)}},
			{sql: "DELETE FROM dev WHERE $1 IS NULL OR k = $1", args: []any{int64(13)}, code: "42P08"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				m, mErr := queryRow(t, b.m, test.sql, test.args)
				pg, pgErr := queryRow(t, b.pg, test.sql, test.args)
				is.Equal(t, test.code, pgdiff.SQLState(pgErr), fmt.Sprint(pgErr))
				if test.code == "" {
					is.NotError(t, mErr)
					is.Equal(t, fmt.Sprint(pg), fmt.Sprint(m))
				} else {
					is.True(t, pgdiff.SameClass(mErr, pgErr), fmt.Sprintf("maragudb %v; postgres %v", mErr, pgErr))
				}
			})
		}
		for _, query := range []string{"SELECT * FROM dev ORDER BY k", "SELECT * FROM dev2 ORDER BY k"} {
			m, mErr := queryRow(t, b.m, query, nil)
			is.NotError(t, mErr)
			pg, pgErr := queryRow(t, b.pg, query, nil)
			is.NotError(t, pgErr)
			is.Equal(t, fmt.Sprint(pg), fmt.Sprint(m), "the writes leave the same rows")
		}
	})
}

// queryRow runs a query with the arguments args and returns its one row, with each value made
// comparable by [canonicalValue].
func queryRow(t *testing.T, db *sql.DB, query string, args []any) ([]any, error) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for _, v := range vals {
			out = append(out, canonicalValue(v))
		}
	}
	return out, rows.Err()
}

// timestampBounds are the ends of timestamptz in Postgres, the start of the year 1, which text
// reads, and times outside the range. Each is within 292,277 years of 2000, where pgx encodes a
// time faithfully: beyond, its microseconds since 2000 overflow 64 bits and wrap around to
// another time, which Postgres may accept.
var timestampBounds = []time.Time{
	time.Date(-4713, 11, 24, 0, 0, 0, 0, time.UTC),
	time.Date(294277, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(-100000, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(294277, 1, 5, 0, 0, 0, 0, time.UTC),
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
