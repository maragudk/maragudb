package exec_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sql/value"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

// world is a fake store shared by sessions, like several processes on one database.
type world struct {
	t      *testing.T
	clock  *s3fake.VirtualClock
	fake   *s3fake.Client
	decide func(kind s3.Kind, key string) s3fake.Outcome
	seed   uint64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, clock: s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	w.fake = s3fake.New(s3fake.WithClock(w.clock), s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
		if w.decide == nil {
			return s3fake.OutcomeNormal
		}
		return w.decide(kind, key)
	}}))
	return w
}

func (w *world) session(opts exec.Options) (*exec.Session, *engine.DB) {
	w.t.Helper()
	w.seed++
	db, err := engine.Open(w.t.Context(), engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now, Rand: rand.New(rand.NewPCG(w.seed, w.seed))})
	is.NotError(w.t, err)
	return exec.NewSession(db, opts), db
}

func newSession(t *testing.T) *exec.Session {
	t.Helper()
	s, _ := newWorld(t).session(exec.Options{})
	return s
}

// run a statement with parameters as the driver passes them: a Go string is a value of unknown
// type, like a quoted literal, and the other Go types are their SQL types.
func run(ctx context.Context, s *exec.Session, sql string, params ...any) (*exec.Rows, exec.Result, error) {
	st, err := exec.Prepare(sql)
	if err != nil {
		return nil, exec.Result{}, err
	}
	var vals []value.Value
	for _, p := range params {
		switch p := p.(type) {
		case int:
			vals = append(vals, value.NewInt(int64(p)))
		case int64:
			vals = append(vals, value.NewInt(p))
		case float64:
			vals = append(vals, value.NewFloat(p))
		case bool:
			vals = append(vals, value.NewBool(p))
		case string:
			vals = append(vals, value.NewUnknown(p))
		case []byte:
			vals = append(vals, value.NewBytes(p))
		case time.Time:
			vals = append(vals, value.NewTime(p))
		case nil:
			vals = append(vals, value.NewNull(value.Unknown))
		default:
			panic(fmt.Sprintf("unsupported param %T", p))
		}
	}
	return s.Execute(ctx, st, vals)
}

func mustExec(t *testing.T, s *exec.Session, sql string, params ...any) exec.Result {
	t.Helper()
	_, res, err := run(t.Context(), s, sql, params...)
	is.NotError(t, err, sql)
	return res
}

// query runs a SELECT and renders the rows as one line per row, values separated by spaces:
// strings quoted, NULL as NULL, and everything else as Postgres outputs it.
func query(t *testing.T, s *exec.Session, sql string, params ...any) string {
	t.Helper()
	rows, _, err := run(t.Context(), s, sql, params...)
	is.NotError(t, err, sql)
	return render(rows)
}

func render(rows *exec.Rows) string {
	var lines []string
	for _, row := range rows.Rows {
		var parts []string
		for _, v := range row {
			parts = append(parts, v.String())
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

func queryErr(t *testing.T, s *exec.Session, sql string, params ...any) error {
	t.Helper()
	_, _, err := run(t.Context(), s, sql, params...)
	is.True(t, err != nil, "expected an error from "+sql)
	return err
}

func setup(t *testing.T, s *exec.Session) {
	t.Helper()
	mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, name text NOT NULL, f double precision, b boolean, data bytea, at timestamptz)")
	mustExec(t, s, `INSERT INTO t VALUES (1, 'alice', 1.5, TRUE, '\x01', '2024-01-01T00:00:00Z'), (2, 'bob', NULL, FALSE, NULL, NULL), (3, 'carol', -2.5, NULL, '\x', '2024-06-01 12:30:00.5+00')`)
}

func TestSession_DDL(t *testing.T) {
	t.Run("creates and drops tables, with IF EXISTS variants", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)")
		is.Error(t, exec.ErrTableExists, queryErr(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)"))
		mustExec(t, s, "CREATE TABLE IF NOT EXISTS t (id bigint PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		mustExec(t, s, "DROP TABLE t")
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "DROP TABLE t"))
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "SELECT * FROM t"))
		mustExec(t, s, "DROP TABLE IF EXISTS t")
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)")
		is.Equal(t, "", query(t, s, "SELECT * FROM t"), "the old rows are gone")
	})

	t.Run("rejects bad definitions", func(t *testing.T) {
		s := newSession(t)
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint, PRIMARY KEY (nope))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint, id text, PRIMARY KEY (id))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint, PRIMARY KEY (id, id))"))
		is.True(t, strings.Contains(queryErr(t, s, "SELECT 1 + TRUE").Error(), "operator does not exist: integer + boolean"))
	})

	t.Run("primary key columns are not null", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (a bigint, b bigint, PRIMARY KEY (a, b))")
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "INSERT INTO t VALUES (1, NULL)"))
	})

	t.Run("accepts Postgres's unreserved words as column names and rejects its reserved words", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE kw (key bigint PRIMARY KEY, if bigint, exists bigint, set bigint, begin bigint, nulls bigint, first bigint, values bigint, delete bigint)")
		mustExec(t, s, "INSERT INTO kw (key, set, begin, values) VALUES (1, 2, 3, 4)")
		mustExec(t, s, "UPDATE kw SET set = 5, begin = 6 WHERE key = 1")
		is.Equal(t, "1 5 6 4", query(t, s, "SELECT key, set, begin, values FROM kw ORDER BY key"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "CREATE TABLE kw2 (limit bigint PRIMARY KEY)"))
		mustExec(t, s, `CREATE TABLE kw2 ("limit" bigint PRIMARY KEY)`)
	})
}

func TestSession_Insert(t *testing.T) {
	t.Run("inserts rows and reads them back in key order", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Equal(t, `1 "alice" 1.5 true \x01 2024-01-01 00:00:00+00
2 "bob" NULL false NULL NULL
3 "carol" -2.5 NULL \x 2024-06-01 12:30:00.5+00`, query(t, s, "SELECT * FROM t"))
	})

	t.Run("uses the column list and defaults the rest to NULL", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		res := mustExec(t, s, "INSERT INTO t (name, id) VALUES ('dan', 4)")
		is.Equal(t, int64(1), res.RowsAffected)
		is.Equal(t, `4 "dan" NULL NULL NULL NULL`, query(t, s, "SELECT * FROM t WHERE id = 4"))
	})

	t.Run("checks constraints and types", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t (id, name) VALUES (1, 'x')"))
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "INSERT INTO t (id) VALUES (9)"))
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, NULL)"))
		is.Error(t, exec.ErrCast, queryErr(t, s, "INSERT INTO t (id, name) VALUES ('x', 'x')"), "a quoted literal is read as the column's type")
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name) VALUES (TRUE, 'x')"))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name, b) VALUES (9, 'x', 1)"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, nope) VALUES (9, 'x')"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, id) VALUES (9, 9)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, 'x', 1)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, 'x'), (10)"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, 'x', 1), (10, 'y', CAST('z' AS bigint))"), "a row is checked before the next is analyzed")
		is.Error(t, exec.ErrCast, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, 'x'), (CAST('z' AS bigint), 'y', 1)"), "and is analyzed before its length is checked")
		is.Error(t, exec.ErrCast, queryErr(t, s, "UPDATE t SET data = coalesce('a'), id = CAST('x' AS bigint)"), "every SET expression is analyzed before any is converted")
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET data = coalesce('a'), id = 1 WHERE 1"), "and WHERE before any of them")
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "UPDATE t SET id = 1, id = 2"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, name) VALUES (id, 'x')"))
	})

	t.Run("applies Postgres's assignment casts", func(t *testing.T) {
		verify.Covers(t, "an INSERT applies the assignment casts Postgres has: a quoted literal as the column's type, numeric and float literals rounded into bigint, integers widened to double precision, and any value written into text", "INV-TYPES", "INV-PG-SUBSET")
		s := newSession(t)
		setup(t, s)
		mustExec(t, s, "INSERT INTO t (id, name, f, b, data, at) VALUES ('10', 'x', 2, 't', '\\x6869', '2024-01-02 03:04:05+02')")
		is.Equal(t, `10 "x" 2 true \x6869 2024-01-02 01:04:05+00`, query(t, s, "SELECT * FROM t WHERE id = 10"))
		mustExec(t, s, "INSERT INTO t (id, name, f) VALUES (11, 1.5, $1)", 2)
		is.Equal(t, `11 "1.5" 2`, query(t, s, "SELECT id, name, f FROM t WHERE id = 11"), "an integer parameter widens, and a numeric literal is written as text")
		mustExec(t, s, "INSERT INTO t (id, name) VALUES (12.5, TRUE), (13.5, 2.5::float8), (16.5::float8, '2024-01-01'::timestamptz)")
		is.Equal(t, "13 \"true\"\n14 \"2.5\"\n16 \"2024-01-01 00:00:00+00\"", query(t, s, "SELECT id, name FROM t WHERE id >= 13 ORDER BY id"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t (id, name) VALUES (13.5::float8, 'x')"), "a numeric literal rounds half away from zero, a float half to even")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "INSERT INTO t (id, name) VALUES (1e30::float8, 'x')"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "INSERT INTO t (id, name) VALUES (99999999999999999999, 'x')"))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (15, 'x', TRUE)"))
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (15, 'x', $1)", true))
		is.Error(t, exec.ErrCast, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (15, 'x', $1)", "x"), "a string parameter is read like a quoted literal")
		mustExec(t, s, "INSERT INTO t (id, name, f) VALUES (15, 'x', $1)", "1.5")
		is.Equal(t, "1.5", query(t, s, "SELECT f FROM t WHERE id = 15"))
	})

	t.Run("a multi-row insert that fails leaves nothing behind", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t (id, name) VALUES (7, 'x'), (8, 'y'), (8, 'z')"))
		is.Equal(t, "", query(t, s, "SELECT id FROM t WHERE id >= 7"))
	})

	t.Run("binds parameters of every type", func(t *testing.T) {
		verify.Covers(t, "every type binds through a parameter, and a timestamptz keeps microseconds", "INV-TYPES", "INV-PLACEHOLDERS")
		s := newSession(t)
		setup(t, s)
		at := time.Date(2025, 2, 3, 4, 5, 6, 7000, time.FixedZone("x", 3600))
		mustExec(t, s, "INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6)", 5, "eve", 0.25, true, []byte{0xAB}, at)
		is.Equal(t, `5 "eve" 0.25 true \xab 2025-02-03 03:05:06.000007+00`, query(t, s, "SELECT * FROM t WHERE id = $1", 5))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT * FROM t WHERE id = $2", 5))
	})
}

func TestSession_Select(t *testing.T) {
	t.Run("evaluates expressions like Postgres", func(t *testing.T) {
		verify.Covers(t, "expressions evaluate as in Postgres: integer literals are 32-bit, numeric literals adapt to double precision, quoted literals take the type their context demands, || writes any type next to text, and casts use Postgres's text formats", "INV-PG-SUBSET", "INV-TYPES")
		s := newSession(t)
		tests := []struct {
			sql  string
			want string
		}{
			{"SELECT 1 + 2 * 3 - 4 / 2 % 3", "5"},
			{"SELECT 7 / 2, -7 / 2, 7 % 3, -7 % 3", "3 -3 1 -1"},
			{"SELECT 1.5::float8 + 1, 2 * 2.5::float8, 1 / 4.0::float8, 1.5 + 1.5::float8", "2.5 5 0.25 3"},
			{"SELECT -(-5), -5, abs(-3), abs(-2.5::float8), abs(0.0::float8), +5, +(-5)", "5 -5 3 2.5 0 5 -5"},
			{"SELECT 'a' || 'b' || 'c', 'a' || NULL, '\\x01'::bytea || '\\x02', 1 || '2', 'a' || 1, TRUE || 'x', 1.5::float8 || 'x', '\\x01'::bytea || 'x'::text, 'x' || '\\x01'::bytea", `"abc" NULL \x0102 "12" "a1" "truex" "1.5x" "\\x01x" \x7801`},
			{"SELECT length('héllo'), length('\\x0102'::bytea), lower('ÄBC'), upper('äbc'), lower('ΑΒΓ')", `5 2 "Äbc" "äBC" "ΑΒΓ"`},
			{"SELECT 1 = 1, 1 <> 1, 1 < 2, 2 <= 2, 3 > 2, 2 >= 3", "true false true true true false"},
			{"SELECT 'a' < 'b', 'B' < 'a', 'abc' < 'abd', '' < 'a'", "true true true true"},
			{"SELECT '\\x01'::bytea < '\\x0102', '\\x02'::bytea > '\\x0199', TRUE > FALSE", "true true true"},
			{"SELECT '2024-01-01T00:00:00Z'::timestamptz < '2024-01-01 00:00:00.000001+00', timestamptz '2024-01-01 02:00:00+02' = '2024-01-01'", "true true"},
			{"SELECT TRUE AND NULL, FALSE AND NULL, TRUE OR NULL, FALSE OR NULL, NOT NULL, NULL AND NULL, 'true' AND 't', NOT 'f'", "NULL false true NULL NULL NULL true true"},
			{"SELECT NULL = NULL, NULL IS NULL, NULL IS NOT NULL, 1 IS NULL, 1 = NULL, 'a' IS NULL", "NULL true false false NULL false"},
			{"SELECT 2 BETWEEN 1 AND 3, 0 BETWEEN 1 AND 3, 2 NOT BETWEEN 1 AND 3, NULL BETWEEN 1 AND 3, 5 BETWEEN 1 AND NULL, 0 BETWEEN 1 AND NULL", "true false false NULL NULL false"},
			{"SELECT 1 IN (1, 2), 3 IN (1, 2), 3 IN (1, NULL), 1 IN (1, NULL), NULL IN (1), 3 NOT IN (1, 2), 3 NOT IN (1, NULL), 1 IN ('1'), '1' IN (1, 2), 'a' IN ('a')", "true false NULL true NULL true NULL true true true"},
			{"SELECT 'abc' LIKE 'a%', 'abc' LIKE 'a_c', 'abc' LIKE 'a_', 'a%c' LIKE 'a\\%c', 'abc' LIKE 'a\\%c', 'abc' NOT LIKE '%b%', 'héllo' LIKE 'h_llo', '' LIKE '%', '' LIKE '_', NULL LIKE 'a'", "true true false true false false true true false NULL"},
			{"SELECT CASE WHEN 1 > 2 THEN 'a' WHEN 2 > 1 THEN 'b' ELSE 'c' END, CASE WHEN NULL THEN 1 ELSE 2 END, CASE WHEN FALSE THEN 1 END, CASE WHEN 't' THEN 1 END", `"b" 2 NULL 1`},
			{"SELECT CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' END, CASE NULL WHEN NULL THEN 'null' ELSE 'other' END, CASE 'a' WHEN 'a' THEN 1 END, CASE 1 WHEN '1' THEN 1 END", `"two" "other" 1 1`},
			{"SELECT coalesce(NULL, 1), coalesce(NULL, NULL), coalesce(1, 2), coalesce(NULL, 1.5::float8, 2), coalesce('1', 2), coalesce(1, 2::bigint)", "1 NULL 1 1.5 1 1"},
			{"SELECT -abs(NULL), abs(NULL) + 1, length(NULL) + 1, lower(NULL) || 'a', abs('-1')", "NULL NULL NULL NULL 1"},
			{"SELECT CAST(1 AS double precision), CAST(2.5::float8 AS bigint), CAST(3.5::float8 AS bigint), CAST(-2.5::float8 AS bigint), CAST(2.4::float8 AS bigint), 2.5::bigint, 3.5::bigint, -2.5::bigint, (-2.5)::bigint", "1 2 4 -2 2 3 4 -3 -3"},
			{"SELECT CAST(42 AS text), CAST(TRUE AS text), CAST(1.5::float8 AS text), CAST('\\x6869'::bytea AS text), CAST('2024-01-01T00:00:00.25Z'::timestamptz AS text), 2::bigint::text", `"42" "true" "1.5" "\\x6869" "2024-01-01 00:00:00.25+00" "2"`},
			{"SELECT CAST(' 42 ' AS bigint), CAST('1.5e3' AS double precision), CAST('t' AS boolean), CAST('NO' AS bool), CAST('on' AS boolean), CAST('hi' AS bytea), CAST('2024-01-01 00:00:00Z' AS timestamptz), '0x10'::bigint, '0x10'::float8", `42 1500 true false true \x6869 2024-01-01 00:00:00+00 16 16`},
			{"SELECT CAST('NaN' AS float8) > CAST('Infinity' AS float8), CAST('NaN' AS double precision) = CAST('nan' AS double precision), CAST('-inf' AS float8) < -1e308::float8", "true true true"},
			{"SELECT 'Infinity'::float8 - 'Infinity'::float8, 'Infinity'::float8 * 0, 'nan'::float8 / 0", "NaN NaN NaN"},
			{"SELECT CAST('NaN'::float8 AS text), CAST(1e20::float8 AS text), CAST(1e14::float8 AS text), CAST(0.0001::float8 AS text), CAST(0.00001::float8 AS text), CAST(-0.0::float8 AS text), (-0.0)::float8::text, '-0'::float8::text, -(0.0::float8) || ''", `"NaN" "1e+20" "100000000000000" "0.0001" "1e-05" "-0" "0" "-0" "-0"`},
			{"SELECT CAST(NULL AS bigint) IS NULL, CAST(NULL AS text), NULL::bigint", "true NULL NULL"},
			{"SELECT 1 AS one, 2 two, 'a', NULL", `1 2 "a" NULL`},
			{"SELECT 1 WHERE FALSE", ""},
			{"SELECT 1 WHERE NULL", ""},
			{"SELECT 1 WHERE 'true'", "1"},
			{"SELECT 9223372036854775807, -9223372036854775808, 9223372036854775807 % -1, -9223372036854775808 % -1", "9223372036854775807 -9223372036854775808 0 0"},
			{"SELECT 1e308::float8 * 0.5, 1e-308::float8 * 0.0, 0.0::float8 / 1.5", "5e+307 0 0"},
			{"SELECT 2147483647 + 0, -2147483647 - 1, 2147483647::bigint + 1, 2000000000 * 2::bigint", "2147483647 -2147483648 2147483648 4000000000"},
			{"SELECT 5 % -1, (-2147483647 - 1) % -1, abs(-2147483647)", "0 0 2147483647"},
			{"SELECT 1 LIMIT '1'", "1"},
			{"SELECT 1.5::text, 1e5 || 'x', 01.50::text, .5::text, (-0.0)::text, 1.5e1::text, 1e-3 || '', 1.::text, 000.0::text, 1.0e-2::text, 'a' || 9223372036854775808, 1.5E+2::text", `"1.5" "100000x" "1.50" "0.5" "0.0" "15" "0.001" "1" "0.0" "0.010" "a9223372036854775808" "150"`},
			{"SELECT CASE WHEN FALSE THEN 1e400::float8 END, CASE WHEN FALSE THEN 9223372036854775808::bigint END", "NULL NULL"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				is.Equal(t, test.want, query(t, s, test.sql))
			})
		}
	})

	t.Run("reports evaluation errors like Postgres", func(t *testing.T) {
		verify.Covers(t, "integer and bigint overflow in +, -, *, /, unary minus, and abs is ErrOutOfRange, at 32 bits for integer literals, among the other evaluation errors", "INV-INT-OVERFLOW")
		s := newSession(t)
		tests := []struct {
			sql  string
			want error
		}{
			{"SELECT 1 / 0", exec.ErrDivisionByZero},
			{"SELECT 1 % 0", exec.ErrDivisionByZero},
			{"SELECT 1.0::float8 / 0.0", exec.ErrDivisionByZero},
			{"SELECT 1::bigint / 0", exec.ErrDivisionByZero},
			{"SELECT 9223372036854775807 + 1", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 - 1", exec.ErrOutOfRange},
			{"SELECT 4611686018427387904 * 2", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 * -1", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 / -1", exec.ErrOutOfRange},
			{"SELECT -(-9223372036854775807 - 1)", exec.ErrOutOfRange},
			{"SELECT abs(-9223372036854775808)", exec.ErrOutOfRange},
			{"SELECT 2147483647 + 1", exec.ErrOutOfRange},
			{"SELECT -2147483647 - 2", exec.ErrOutOfRange},
			{"SELECT 2000000000 * 2", exec.ErrOutOfRange},
			{"SELECT 2147483647 * 2147483647", exec.ErrOutOfRange},
			{"SELECT (-2147483647 - 1) / -1", exec.ErrOutOfRange},
			{"SELECT -(-2147483647 - 1)", exec.ErrOutOfRange},
			{"SELECT abs(-2147483647 - 1)", exec.ErrOutOfRange},
			{"SELECT length('a') + 2147483647", exec.ErrOutOfRange},
			{"SELECT 1e308::float8 * 10", exec.ErrOutOfRange},
			{"SELECT 1e308::float8 + 1e308::float8", exec.ErrOutOfRange},
			{"SELECT 1e-308::float8 * 1e-308::float8", exec.ErrOutOfRange},
			{"SELECT 1e-308::float8 / 1e308::float8", exec.ErrOutOfRange},
			{"SELECT CAST(1e19::float8 AS bigint)", exec.ErrOutOfRange},
			{"SELECT CAST(CAST('NaN' AS float8) AS bigint)", exec.ErrOutOfRange},
			{"SELECT CAST('99999999999999999999' AS bigint)", exec.ErrOutOfRange},
			{"SELECT CAST('1e400' AS float8)", exec.ErrOutOfRange},
			{"SELECT 9223372036854775807.5::bigint", exec.ErrOutOfRange},
			{"SELECT 1 LIMIT 1e30", exec.ErrOutOfRange},
			{"SELECT CAST('x' AS bigint)", exec.ErrCast},
			{"SELECT CAST('1.5' AS bigint)", exec.ErrCast},
			{"SELECT CAST('x' AS double precision)", exec.ErrCast},
			{"SELECT CAST('1_000' AS float8)", exec.ErrCast},
			{"SELECT CAST('o' AS boolean)", exec.ErrCast},
			{"SELECT CAST('' AS boolean)", exec.ErrCast},
			{"SELECT CAST('2024-13-01' AS timestamptz)", exec.ErrCast},
			{"SELECT CAST('a\\' AS bytea)", exec.ErrCast},
			{"SELECT CAST(1 AS boolean)", exec.ErrCast},
			{"SELECT CAST(TRUE AS bigint)", exec.ErrCast},
			{"SELECT CAST(1 AS timestamptz)", exec.ErrCast},
			{"SELECT 1 + 'a'", exec.ErrCast},
			{"SELECT '1.5' + 1", exec.ErrCast},
			{"SELECT 1 WHERE 'x'", exec.ErrCast},
			{"SELECT 1 LIMIT 'x'", exec.ErrCast},
			{"SELECT coalesce('a', 1)", exec.ErrCast},
			{"SELECT CASE WHEN FALSE THEN 'a' ELSE 1 END", exec.ErrCast},
			{"SELECT 1 + TRUE", exec.ErrType},
			{"SELECT 1.5::float8 % 1.0::float8", exec.ErrType},
			{"SELECT -'a'", exec.ErrType},
			{"SELECT -NULL", exec.ErrType},
			{"SELECT +NULL", exec.ErrType},
			{"SELECT '1' + '2'", exec.ErrType},
			{"SELECT NULL + NULL", exec.ErrType},
			{"SELECT 1 || 2", exec.ErrType},
			{"SELECT NOT 1", exec.ErrType},
			{"SELECT 1 AND TRUE", exec.ErrType},
			{"SELECT 1 BETWEEN TRUE AND FALSE", exec.ErrType},
			{"SELECT 1 IN (TRUE)", exec.ErrType},
			{"SELECT 1 LIKE 'a'", exec.ErrType},
			{"SELECT CASE WHEN 1 THEN 1 END", exec.ErrType},
			{"SELECT TRUE AND CASE WHEN TRUE THEN NULL END", exec.ErrType},
			{"SELECT TRUE AND coalesce(NULL, NULL)", exec.ErrType},
			{"SELECT 1 + coalesce(CASE WHEN FALSE THEN FALSE END)", exec.ErrType},
			{"SELECT 1 + CASE WHEN FALSE THEN NULL END", exec.ErrType},
			{"SELECT CASE WHEN TRUE THEN 1 ELSE TRUE END", exec.ErrType},
			{"SELECT CASE 1 WHEN TRUE THEN 1 END", exec.ErrType},
			{"SELECT coalesce(1, TRUE)", exec.ErrType},
			{"SELECT length(1)", exec.ErrType},
			{"SELECT lower(1)", exec.ErrType},
			{"SELECT abs(TRUE)", exec.ErrType},
			{"SELECT 1 LIMIT TRUE", exec.ErrType},
			{"SELECT 1 OFFSET '1'::text", exec.ErrType},
			{"SELECT 'abc' LIKE 'a' || '\\'", exec.ErrInvalid},
			{"SELECT 'ab' LIKE 'a\\'", exec.ErrInvalid},
			{"SELECT 'x' LIKE '%\\'", exec.ErrInvalid},
			{"SELECT length()", exec.ErrInvalid},
			{"SELECT x", exec.ErrInvalid},
			{"SELECT 1 LIMIT -1", exec.ErrInvalid},
			{"SELECT 1 ORDER BY 2", exec.ErrInvalid},
			{"SELECT 1 ORDER BY 0", exec.ErrInvalid},
			{"SELECT 1 ORDER BY 2147483647", exec.ErrInvalid},
			{"SELECT 1 ORDER BY -2147483647", exec.ErrInvalid},
			{"SELECT 1 AS a, 2 AS a ORDER BY a", exec.ErrInvalid},
			{"SELECT coalesce()", exec.ErrSyntax},
			{"SELECT *", exec.ErrSyntax},
			{"SELECT 1 WHERE 1", exec.ErrType},
			{"SELECT 1 ORDER BY FALSE", exec.ErrSyntax},
			{"SELECT 1 ORDER BY 'a'", exec.ErrSyntax},
			{"SELECT 1 ORDER BY NULL", exec.ErrSyntax},
			{"SELECT 1 ORDER BY 1.5", exec.ErrSyntax},
			{"SELECT 1 ORDER BY 2147483648", exec.ErrSyntax},
			{"SELECT 1 ORDER BY -2147483648", exec.ErrSyntax},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				is.Error(t, test.want, queryErr(t, s, test.sql))
			})
		}
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT FALSE AND 1"), "a pruned branch is still type-checked")
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN FALSE THEN 1 ELSE TRUE END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN TRUE THEN TRUE END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT coalesce(NULL, 1, TRUE)"))
	})

	t.Run("rejects what Postgres accepts only with ErrUnsupported", func(t *testing.T) {
		verify.Covers(t, "the numeric type, bit strings, non-ISO timestamps, LIKE on bytea, and the other rejections of accepted Postgres SQL are ErrUnsupported with a hint", "INV-UNSUPPORTED")
		s := newSession(t)
		for _, sql := range []string{
			"SELECT 1.5", "SELECT 1 + 1.5", "SELECT 1.5 + 1.5", "SELECT -(1.5)", "SELECT abs(1.5)", "SELECT 1 = 1.5", "SELECT '1' + 1.5",
			"SELECT 9223372036854775808", "SELECT 9223372036854775808 + 1", "SELECT -(-9223372036854775808)", "SELECT 1.5::boolean",
			"SELECT 'January 2, 2024'::timestamptz", "SELECT 'now'::timestamptz", "SELECT '0001-01-01 00:00:00+01'::timestamptz",
			"SELECT '\\x01'::bytea LIKE '\\x01'", "SELECT 1::integer", "SELECT count(1)", "SELECT now()", "SELECT nope(1)", "SELECT 1::nope",
		} {
			is.Error(t, exec.ErrUnsupported, queryErr(t, s, sql), sql)
		}
		is.Equal(t, "3", query(t, s, "SELECT 1.5::float8 + 1.5"), "a numeric literal adapts to double precision")
		is.Equal(t, "true", query(t, s, "SELECT 1.5::float8 = 1.5"))
		is.Equal(t, "false", query(t, s, "SELECT 'a' LIKE 'a\\\\'"), "an escaped backslash is not a trailing escape")
		is.Equal(t, "false", query(t, s, "SELECT 'a' LIKE 'a\\'"), "the trailing escape is only an error when matching reaches it")
		is.Equal(t, "false", query(t, s, "SELECT '' LIKE '%\\'"))
	})

	t.Run("folds constants before reading rows, like Postgres, except behind a false branch", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE e (id bigint PRIMARY KEY, b boolean)")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT * FROM e WHERE 1 / 0 = 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT * FROM e WHERE id = 1 AND 1 / 0 = 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT CASE WHEN b THEN 1 / 0 ELSE 0 END FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT coalesce(id, 1 / 0) FROM e"))
		is.Equal(t, "", query(t, s, "SELECT * FROM e WHERE FALSE AND 1 / 0 = 1"))
		is.Equal(t, "", query(t, s, "SELECT * FROM e WHERE FALSE AND id = 1 / 0"), "the planner does not bind pruned key comparisons")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT * FROM e WHERE NULL AND 1 / 0 = 1"), "but folding goes on after it")
		is.Equal(t, int64(0), mustExec(t, s, "DELETE FROM e WHERE FALSE AND id = 1 / 0").RowsAffected)
		is.Equal(t, "", query(t, s, "SELECT 1 WHERE FALSE AND 1 IN (1 / 0)"), "a pruned IN element never compares")
		is.Equal(t, "true", query(t, s, "SELECT TRUE OR 1 IN (1 / 0)"))
		is.Equal(t, "true", query(t, s, "SELECT coalesce(TRUE, 1 IN (1 / 0))"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN FALSE THEN 1 / 0 ELSE 1 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN TRUE THEN 1 ELSE 1 / 0 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN 1 / 0 = 1 THEN 2 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE 1 WHEN 1 THEN 1 WHEN 1 / 0 THEN 2 END"))
		is.Error(t, exec.ErrCast, queryErr(t, s, "SELECT CASE 1 WHEN 1 THEN 1 WHEN 'a' THEN 2 END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN 1 THEN 2 END"))
		is.Equal(t, "1", query(t, s, "SELECT coalesce(1, 1 / 0)"))
		is.Equal(t, "true", query(t, s, "SELECT TRUE OR 1 / 0 = 1"))
		mustExec(t, s, "INSERT INTO e VALUES (-9223372036854775808, TRUE)")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT * FROM e WHERE -id > 0"))
		is.Equal(t, int64(0), mustExec(t, s, "UPDATE e SET b = NULL WHERE -id > 0 AND NULL").RowsAffected, "a NULL conjunct reads no rows")
		is.Equal(t, "", query(t, s, "SELECT * FROM e WHERE NULL AND -id > 0"))
		is.Equal(t, "true", query(t, s, "SELECT -id > 0 OR TRUE FROM e"), "a deciding constant on the right drops the left side")
		is.Equal(t, "false", query(t, s, "SELECT -id > 0 AND FALSE FROM e"))
		is.Equal(t, "true", query(t, s, "SELECT (-id > 0 OR -id < 0) OR (NULL OR TRUE) FROM e"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT -id > 0 OR FALSE FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / 0 = 1 OR TRUE"), "but the left side is still folded")
		is.Equal(t, "NULL", query(t, s, "SELECT -id IN (NULL) FROM e"), "a single NULL element is a comparison with NULL")
		is.Equal(t, "NULL", query(t, s, "SELECT -id NOT IN (NULL) FROM e"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT -id IN (NULL, NULL) FROM e"), "two are an array")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT -id IN (NULL, id) FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / 0 IN (NULL)"))
		is.Equal(t, "NULL", query(t, s, "SELECT NULL + -id FROM e"), "a strict operator with a known NULL does not evaluate the other side")
		is.Equal(t, "NULL", query(t, s, "SELECT -id * CAST(NULL AS bigint) FROM e"))
		is.Equal(t, "NULL", query(t, s, "SELECT CAST(id AS text) LIKE NULL FROM e"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE NULL WHEN id THEN -id ELSE 1 END FROM e"), "an operand of unknown type is text")
		is.Equal(t, "1", query(t, s, "SELECT CASE NULL::bigint WHEN id THEN -id ELSE 1 END FROM e"))
		is.Equal(t, "1", query(t, s, "SELECT CASE id WHEN NULL THEN -id ELSE 1 END FROM e"))
		is.Equal(t, "NULL", query(t, s, "SELECT CASE id WHEN NULL THEN id END = -id FROM e"), "a CASE with no live WHEN and no ELSE is a NULL constant")
		is.Equal(t, "NULL", query(t, s, "SELECT CASE WHEN FALSE THEN id END + -id FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT NULL + 1 / 0 FROM e"), "but a constant operand is still folded first")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL BETWEEN NULL AND -id FROM e"), "a known NULL value folds BETWEEN")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL BETWEEN -id AND 1 FROM e"))
		mustExec(t, s, "INSERT INTO e VALUES (5, NULL)")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT b BETWEEN TRUE AND (1 / (id - 5) > 0) FROM e WHERE id = 5"), "a NULL column still evaluates the bounds")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM e WHERE id + 0 = 5 AND 1 / (id - 5) > 0"), "a true conjunct continues")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM e WHERE (b OR 1 / (id - 5) > 0) AND id + 0 = 5"), "but not inside OR")
		mustExec(t, s, "DELETE FROM e WHERE id = 5")
		is.Equal(t, "false", query(t, s, "SELECT id NOT IN (id, -id) FROM e"), "IN stops at the first matching column element")
		is.Equal(t, "true", query(t, s, "SELECT id IN (1, id, -id) FROM e"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT id IN (1, -id, id) FROM e"))
		is.Equal(t, "NULL", query(t, s, "SELECT NULL IN (id, -id) FROM e"), "a NULL left side makes every comparison NULL without evaluating the elements")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL IN (id, -id, 1, 2) FROM e"), "also with an array")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL NOT IN (1, 2, -id) FROM e"))
		is.Equal(t, "false", query(t, s, "SELECT 1 IN (1.5::float8, id) FROM e"), "each comparison adapts its own literal")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT id FROM e WHERE id NOT IN (NULL, NULL, -id)"), "the array of constants is a conjunct of its own, which rejects the row on which the other fails")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT id NOT IN (NULL, NULL, -id) FROM e"), "but NULL does not stop AND inside an expression")
		is.Equal(t, "NULL", query(t, s, "SELECT TRUE AND coalesce(CASE WHEN FALSE THEN FALSE END)"), "a pruned branch still types the CASE")
		is.Equal(t, "false", query(t, s, "SELECT 0 BETWEEN 1 AND 1 / 0"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 2 BETWEEN 1 AND 1 / 0"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT NULL BETWEEN 1 AND 1 / 0"))
	})

	t.Run("accepts a WHERE clause only when its outcome is the same in every order and plan Postgres could pick", func(t *testing.T) {
		verify.Covers(t, "a WHERE clause whose outcome on a row depends on the order Postgres evaluates its conditions in, including the equalities its equivalence classes imply, is ErrUnsupported, and so is a condition that can fail next to a primary key condition that an index scan could use; every other WHERE clause fails exactly when Postgres fails in every order", "INV-PG-SUBSET", "UNS-ORDER-DEPENDENT-WHERE", "UNS-PLAN-DEPENDENT-WHERE")
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE w (id bigint PRIMARY KEY, n bigint, m bigint, s text)")
		mustExec(t, s, "INSERT INTO w VALUES (1, 1, 0, 'a'), (2, 7, 5, 'b\\'), (3, 9223372036854775806, 5, 'c')")
		for _, sql := range []string{
			// A condition fails on a row that another rejects.
			"SELECT id FROM w WHERE n = 2 AND 1 / m = 1",
			"SELECT id FROM w WHERE 1 / m = 1 AND n = 2",
			"SELECT id FROM w WHERE m <> 0 AND 1 / m = 1",
			"SELECT id FROM w WHERE s LIKE 'a%' AND n * 2 = 0",
			"SELECT id FROM w WHERE 1 / m = 0 AND id + 0 > 1",
			"DELETE FROM w WHERE n = 2 AND 1 / m = 1",
			"UPDATE w SET n = 0 WHERE NOT (n <> 2 OR 1 / m = 1)",
			// The equivalence class of n = 1 / m and 1 / m = 7 implies n = 7, which rejects the row
			// where 1 / m fails.
			"SELECT id FROM w WHERE n = 1 / m AND 1 / m = 7",
			// An index scan would evaluate the condition that can fail on fewer rows.
			"SELECT id FROM w WHERE id = 2 AND 1 / m = 1",
			"SELECT id FROM w WHERE id > 1 AND 1 / m = 0",
			"SELECT id FROM w WHERE NOT (id <= 1) AND 1 / m = 0",
			"SELECT id FROM w WHERE id IN (2, 3) AND 1 / m = 0",
			"SELECT id FROM w WHERE (id = 1 OR id = 2) AND 1 / m = 1",
			"SELECT id FROM w WHERE id = n AND n = 2 AND 1 / m = 1",
			"SELECT id FROM w WHERE s LIKE s AND id > 1",
			"UPDATE w SET n = 1 WHERE s LIKE s AND id > 1",
			"DELETE FROM w WHERE s LIKE s AND id > 1",
			"SELECT m FROM w WHERE s LIKE 'b\\' AND id = 1",
		} {
			err := queryErr(t, s, sql)
			is.Error(t, exec.ErrUnsupported, err, sql)
			is.True(t, strings.Contains(err.Error(), "write CASE WHEN"), "the message says what to write instead")
		}
		mustExec(t, s, "CREATE TABLE wk (s text PRIMARY KEY, m bigint)")
		mustExec(t, s, "INSERT INTO wk VALUES ('a', 0), ('b', 5)")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT s FROM wk WHERE s LIKE 'b\\'"), "a LIKE that can fail is an index condition itself")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT s FROM wk WHERE s LIKE s AND s LIKE 'b%'"))
		is.Equal(t, "a\nb", strings.ReplaceAll(query(t, s, "SELECT s FROM wk WHERE s LIKE s"), `"`, ""), "a LIKE whose pattern is not a constant is no index condition")

		is.Equal(t, "", query(t, s, "SELECT id FROM w WHERE CASE WHEN m <> 0 THEN 1 / m = 1 END"), "CASE fixes the order")
		is.Equal(t, "2", query(t, s, "SELECT id FROM w WHERE id = 2 AND n = 7"), "conditions that cannot fail combine freely")
		is.Equal(t, "2", query(t, s, "SELECT id FROM w WHERE n = 7 AND 1 / (m + 1) = 0"), "a row on which no condition fails is fine in every order")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM w WHERE 1 / m = 1 AND n = 1"), "a row that every other condition accepts fails in every order")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM w WHERE n = 1 AND m = 0 AND 1 / m = 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM w WHERE n = 1 / m AND 1 / m = 1"), "the implied n = 1 holds on the failing row")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM w WHERE id + 0 >= 1 AND 1 / m = 0"), "a key column in an expression is no index condition")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT id FROM w WHERE n * 2 = 0"), "a single condition is evaluated on every row")
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT id FROM w WHERE s LIKE 'b\\'"), "a trailing escape on a column that is not a key is evaluated like any other condition")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT id FROM w WHERE m <> 0 AND 1 / m >= 0 AND n * 2 > 0"), "a row that fails in every order wins over an earlier row whose outcome depends on the order")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM w WHERE id = 1 AND 1 / 0 = 1"), "a folding error comes before the rejection")
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT nope FROM w WHERE id = 1 AND 1 / m = 1"), "and so does a mistake")
		is.Equal(t, int64(0), mustExec(t, s, "DELETE FROM w WHERE n = 2 AND 1 / m = 1 AND FALSE").RowsAffected, "a known false conjunct reads no rows")
		is.Equal(t, "1\n2\n3", query(t, s, "SELECT id FROM w"), "nothing was deleted or updated")
	})

	t.Run("does not fold the stable conversions between timestamptz and text, as Postgres does not, and rejects one that can fail", func(t *testing.T) {
		verify.Covers(t, "a conversion between timestamptz and text is evaluated when the statement runs, so a CASE does not prune on it and a WHERE condition on it is evaluated once before rows are read, and one without columns that can fail is ErrUnsupported", "INV-PG-SUBSET", "UNS-STABLE-CONSTANT")
		s := newSession(t)
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT CASE WHEN CAST('2024-01-01'::timestamptz AS text) = 'x' THEN 1 ELSE 1 / 0 END"), "the WHEN is not known, so the ELSE is folded")
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN CAST(1 AS text) = '1' THEN 1 ELSE 1 / 0 END"), "other conversions to text fold")
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN 'a' || 1 = 'a1' THEN 1 ELSE 1 / 0 END"), "and so does || with them")
		is.Equal(t, `"2024-01-01 00:00:00+00true"`, query(t, s, "SELECT CAST('2024-01-01'::timestamptz AS text) || CAST(TRUE AS text)"))
		is.Equal(t, "NULL", query(t, s, "SELECT CASE WHEN FALSE THEN CAST(CAST('x' AS text) AS timestamptz) END"), "a pruned conversion is never evaluated")
		for _, sql := range []string{
			"SELECT CAST(CAST('2024-01-02' AS text) AS timestamptz)",
			"SELECT CAST(CAST('2024-01-02'::timestamptz AS text) AS bigint)",
			"SELECT CAST('2024-01-02'::timestamptz AS text) LIKE 'a\\'",
		} {
			is.Error(t, exec.ErrUnsupported, queryErr(t, s, sql), sql)
		}
		mustExec(t, s, "CREATE TABLE e (id bigint PRIMARY KEY, s text)")
		is.Equal(t, "", query(t, s, "SELECT id FROM e WHERE CAST('2024-01-01'::timestamptz AS text) = '2024-01-01 00:00:00+00'"))
		mustExec(t, s, "INSERT INTO e VALUES (2, '2024-01-01'), (1, '2024-13-01')")
		is.Equal(t, "1\n2", query(t, s, "SELECT id FROM e WHERE CAST('2024-01-01'::timestamptz AS text) = '2024-01-01 00:00:00+00'"))
		is.Equal(t, "", query(t, s, "SELECT id FROM e WHERE CAST('2024-01-01'::timestamptz AS text) = 'x'"))
		is.Error(t, exec.ErrCast, queryErr(t, s, "SELECT CAST(s AS timestamptz) FROM e"), "a conversion that reads columns is evaluated on every row")
		is.Equal(t, "", query(t, s, "SELECT id FROM e ORDER BY id LIMIT 0"), "LIMIT 0 reads no rows")
	})

	t.Run("follows Postgres in the details of analysis", func(t *testing.T) {
		verify.Covers(t, "an IN whose constant elements have no common type compares them one at a time, a CASE operand of unknown type is text, +1 in ORDER BY is no position, ORDER BY a name is ambiguous only between different expressions, and a read-only transaction reports analysis errors before it refuses a write", "INV-PG-SUBSET")
		s := newSession(t)
		is.Equal(t, "true", query(t, s, "SELECT '2024-01-01 00:00:00+00' IN (coalesce('x'), CAST('2024-01-01 00:00:00+00' AS timestamptz))"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE 'x' WHEN '\\x'::bytea THEN 1 END"))
		mustExec(t, s, "CREATE TABLE e (id bigint PRIMARY KEY, b boolean, c text)")
		mustExec(t, s, "INSERT INTO e VALUES (2, TRUE, 'b'), (1, FALSE, 'a')")
		is.Equal(t, "2\n1", query(t, s, "SELECT id FROM e ORDER BY +1, id DESC"))
		is.Equal(t, `"a" "a"`+"\n"+`"b" "b"`, query(t, s, "SELECT CAST(c AS text), c FROM e ORDER BY c"), "a cast to the column's own type is the column")
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT lower(c) AS c, c FROM e ORDER BY c"))
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE e SET b = 1"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "DELETE FROM nope"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "INSERT INTO e VALUES (1 / 0, TRUE, 'x')"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "UPDATE e SET b = TRUE"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "CREATE TABLE e (id bigint PRIMARY KEY)"), "DDL is refused before it is analyzed")
		mustExec(t, s, "ROLLBACK")
	})

	t.Run("gives parameters the type of their context", func(t *testing.T) {
		verify.Covers(t, "a parameter of a Go type other than string converts to the type its context infers, overflows like an integer in an integer context, and is ErrUnsupported where it cannot convert", "INV-PLACEHOLDERS", "UNS-PARAMETERS")
		s := newSession(t)
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT $1 + 2147483647", 1))
		is.Equal(t, "2147483648", query(t, s, "SELECT $1::bigint + 2147483647", 1))
		is.Equal(t, "2.5", query(t, s, "SELECT $1 + 0.5::float8", 2))
		is.Equal(t, `"5x"`, query(t, s, "SELECT $1 || 'x'", "5"))
		is.Equal(t, "6", query(t, s, "SELECT $1 + 1", "5"), "a string is read like a quoted literal")
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT $1 + 1", int64(1)<<40))
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT $1", 1), "an int64 with no context would be text")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT $1 + 1", 1.5))
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT $1 AND TRUE", 1))
		is.Equal(t, "true", query(t, s, "SELECT $1 AND TRUE", true))
		is.Equal(t, "NULL", query(t, s, "SELECT $1 + 1", nil))
	})

	t.Run("reports a mistake anywhere in the statement before a folding error, and folding errors in Postgres's order", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE e (id bigint PRIMARY KEY, j bigint)")
		mustExec(t, s, "INSERT INTO e VALUES (1, 1)")
		tests := []struct {
			sql  string
			want error
		}{
			// Checking the whole statement comes first.
			{"SELECT 1 WHERE 1 / 0 = 1 ORDER BY 'a'", exec.ErrSyntax},
			{"SELECT 1 / 0 ORDER BY 2", exec.ErrInvalid},
			{"SELECT 1 / 0 FROM e WHERE nope = 1", exec.ErrInvalid},
			{"SELECT 1 / 0 FROM e WHERE j = TRUE", exec.ErrType},
			{"SELECT 1 / 0 LIMIT TRUE", exec.ErrType},
			{"SELECT id FROM e LIMIT 1 / 0 OFFSET id", exec.ErrInvalid},
			{"DELETE FROM e WHERE 1 / 0 = 1 AND j = TRUE", exec.ErrType},
			{"UPDATE e SET j = 1 / 0 WHERE j = TRUE", exec.ErrType},
			{"INSERT INTO e VALUES (3, 1 / 0), (4, TRUE)", exec.ErrType},
			// Then folding: output expressions, ORDER BY among them, then WHERE, OFFSET, and LIMIT.
			{"SELECT 1 / 0 WHERE 9223372036854775807 + 1 = 1", exec.ErrDivisionByZero},
			{"SELECT 9223372036854775807 + 1, 1 / 0", exec.ErrOutOfRange},
			{"SELECT 1 WHERE 1 / 0 = 1 ORDER BY 9223372036854775807 + 1", exec.ErrOutOfRange},
			{"SELECT 1 WHERE 9223372036854775807 + 1 = 1 AND 1 / 0 = 1", exec.ErrOutOfRange},
			{"SELECT 1 LIMIT 1 / 0 OFFSET 9223372036854775807 + 1", exec.ErrOutOfRange},
			{"UPDATE e SET j = 1 / 0 WHERE 9223372036854775807 + 1 = 1", exec.ErrDivisionByZero},
			{"SELECT 1 / 0 + 1.5::float8", exec.ErrDivisionByZero},
			// Then running: a negative LIMIT, and constraints, only once every value is known.
			{"SELECT 1 LIMIT -1 OFFSET 1 / 0", exec.ErrDivisionByZero},
			{"INSERT INTO e VALUES (1, 1), (2, 1 / 0)", exec.ErrDivisionByZero},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				is.Error(t, test.want, queryErr(t, s, test.sql))
			})
		}
		is.Equal(t, "1 1", query(t, s, "SELECT * FROM e"), "nothing was written")
	})

	t.Run("filters, orders, limits, and offsets", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		tests := []struct {
			sql  string
			want string
		}{
			{"SELECT id FROM t WHERE id = 2", "2"},
			{"SELECT id FROM t WHERE 2 = id", "2"},
			{"SELECT id FROM t WHERE id = $1", "3"},
			{"SELECT id FROM t WHERE id = '2'", "2"},
			{"SELECT id FROM t WHERE id = 2.0::float8", "2"},
			{"SELECT id FROM t WHERE id = NULL", ""},
			{"SELECT id FROM t WHERE id = 1 AND id = 2", ""},
			{"SELECT id FROM t WHERE id = 2 AND id = 2", "2"},
			{"SELECT id FROM t WHERE id > 1", "2\n3"},
			{"SELECT id FROM t WHERE id >= 2 AND id < 3", "2"},
			{"SELECT id FROM t WHERE id BETWEEN 2 AND 3", "2\n3"},
			{"SELECT id FROM t WHERE 1 < id AND 3 > id", "2"},
			{"SELECT id FROM t WHERE id > 5 AND id < 2", ""},
			{"SELECT id FROM t WHERE id = 1 OR id = 3", "1\n3"},
			{"SELECT id FROM t WHERE id IN (3, 1)", "1\n3"},
			{"SELECT id FROM t WHERE name = 'bob'", "2"},
			{"SELECT id FROM t WHERE f IS NULL", "2"},
			{"SELECT id FROM t WHERE f > 0", "1"},
			{"SELECT id FROM t WHERE f > 0 OR f IS NULL", "1\n2"},
			{"SELECT id FROM t WHERE NOT b", "2"},
			{"SELECT id FROM t WHERE b IS NULL", "3"},
			{"SELECT id FROM t WHERE name LIKE '%o%'", "2\n3"},
			{"SELECT id FROM t WHERE at > '2024-01-01T00:00:00Z'", "3"},
			{"SELECT id FROM t WHERE at > timestamptz '2024-01-01 00:00:00+00'", "3"},
			{"SELECT id FROM t WHERE data = '\\x'", "3"},
			{"SELECT id, name FROM t ORDER BY name DESC", `3 "carol"
2 "bob"
1 "alice"`},
			{"SELECT id FROM t ORDER BY f", "3\n1\n2"},
			{"SELECT id FROM t ORDER BY f DESC", "2\n1\n3"},
			{"SELECT id FROM t ORDER BY f NULLS FIRST", "2\n3\n1"},
			{"SELECT id FROM t ORDER BY f DESC NULLS LAST", "1\n3\n2"},
			{"SELECT id FROM t ORDER BY b, id DESC", "2\n1\n3"},
			{"SELECT id FROM t ORDER BY b IS NULL, id", "1\n2\n3"},
			{"SELECT id FROM t ORDER BY 1 DESC", "3\n2\n1"},
			{"SELECT id FROM t ORDER BY '2024-01-01T00:00:00Z'::timestamptz, id DESC", "3\n2\n1"},
			{"SELECT id FROM t ORDER BY '\\x01'::bytea, id DESC", "3\n2\n1"},
			{"SELECT id AS x FROM t ORDER BY x DESC", "3\n2\n1"},
			{"SELECT -id AS id FROM t ORDER BY id", "-3\n-2\n-1"},
			{"SELECT id FROM t ORDER BY -id", "3\n2\n1"},
			{"SELECT id, id FROM t ORDER BY id DESC", "3 3\n2 2\n1 1"},
			{"SELECT *, * FROM t ORDER BY id DESC LIMIT 1", `3 "carol" -2.5 NULL \x 2024-06-01 12:30:00.5+00 3 "carol" -2.5 NULL \x 2024-06-01 12:30:00.5+00`},
			{"SELECT id FROM t ORDER BY id DESC LIMIT 2", "3\n2"},
			{"SELECT id FROM t ORDER BY id LIMIT 1 OFFSET 1", "2"},
			{"SELECT id FROM t ORDER BY id LIMIT 0", ""},
			{"SELECT id FROM t ORDER BY id LIMIT NULL", "1\n2\n3"},
			{"SELECT id FROM t ORDER BY id LIMIT ALL OFFSET 2", "3"},
			{"SELECT id FROM t ORDER BY id LIMIT $1 OFFSET $2", "2"},
			{"SELECT id FROM t ORDER BY id LIMIT 1.5", "1\n2"},
			{"SELECT id FROM t ORDER BY id LIMIT 1.5::float8 OFFSET 0.5", "2\n3"},
			{"SELECT id FROM t ORDER BY id LIMIT 0.5::float8", ""},
			{"SELECT id FROM t ORDER BY f, id LIMIT 2", "3\n1"},
			{"SELECT id, id * 2 AS double, length(name), CAST(id AS text) || name FROM t WHERE id = 1", `1 2 5 "1alice"`},
			{"SELECT f + 1, f * 2, -f FROM t WHERE id = 1", "2.5 3 -1.5"},
			{"SELECT id FROM t WHERE id = 2 ORDER BY 1 / (id - 1), id LIMIT 1", "2"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				var params []any
				if strings.Contains(test.sql, "$2") {
					params = []any{1, 1}
				} else if strings.Contains(test.sql, "$1") {
					params = []any{3}
				}
				is.Equal(t, test.want, query(t, s, test.sql, params...))
			})
		}
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT id AS a, name AS a FROM t ORDER BY a"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / (id - 1) FROM t ORDER BY id DESC"), "ORDER BY evaluates every row")
	})

	t.Run("rejects LIMIT and OFFSET whose rows or errors would depend on Postgres's plan", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET need an ORDER BY on every primary key column and no expression that can fail, else ErrUnsupported", "UNS-LIMIT-ORDER", "UNS-PLAN-DEPENDENT-LIMIT")
		s := newSession(t)
		setup(t, s)
		for _, sql := range []string{
			"SELECT id FROM t LIMIT 2",
			"SELECT id FROM t OFFSET 2",
			"SELECT id FROM t ORDER BY name LIMIT 2",
			"SELECT -id AS id FROM t ORDER BY id LIMIT 2",
			"SELECT 1 / (id - 1) FROM t ORDER BY id LIMIT 1",
			"SELECT id FROM t WHERE 1 / (id - 1) > 0 ORDER BY id LIMIT 1",
			"SELECT id FROM t WHERE f / 2 > 0 ORDER BY id OFFSET 1",
			"SELECT CAST(name AS bigint) FROM t ORDER BY id LIMIT 1",
		} {
			is.Error(t, exec.ErrUnsupported, queryErr(t, s, sql), sql)
		}
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE id = 1 ORDER BY id LIMIT 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM t ORDER BY 1 / (id - 1), id LIMIT 1 OFFSET 1"), "an ORDER BY expression is evaluated on every row in every plan")
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE name LIKE 'a%' ORDER BY id LIMIT 1"))
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE f = 1.5 ORDER BY id LIMIT 1"))
		mustExec(t, s, "CREATE TABLE c (a text, b bigint, PRIMARY KEY (a, b))")
		mustExec(t, s, "INSERT INTO c VALUES ('x', 1), ('x', 2)")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT b FROM c ORDER BY a LIMIT 1"))
		is.Equal(t, "2", query(t, s, "SELECT b FROM c ORDER BY a, b DESC LIMIT 1"))
		is.Equal(t, "1", query(t, s, "SELECT b AS x FROM c ORDER BY x, a LIMIT 1"), "an output column that is a key column counts")
	})

	t.Run("names and types output columns", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		rows, _, err := run(t.Context(), s, "SELECT id, name AS n, f + 1, length(name), CASE WHEN TRUE THEN 1 END, CAST(f AS bigint), NULL, 1, CASE WHEN FALSE THEN 1 ELSE id END, CASE WHEN FALSE THEN 1 ELSE CAST(2 AS bigint) END, CAST(1 AS double precision), CASE WHEN TRUE THEN 1 ELSE coalesce(id) END, 'a', 1::bigint, timestamptz '2024-01-01', '1.5'::float8, TRUE::text, 2147483648 FROM t")
		is.NotError(t, err)
		is.Equal(t, fmt.Sprint([]exec.Column{
			{Name: "id", Type: value.Int}, {Name: "n", Type: value.String}, {Name: "?column?", Type: value.Float, Nullable: true},
			{Name: "length", Type: value.Int4, Nullable: true}, {Name: "case", Type: value.Int4, Nullable: true}, {Name: "f", Type: value.Int, Nullable: true},
			{Name: "?column?", Type: value.String, Nullable: true}, {Name: "?column?", Type: value.Int4, Nullable: true},
			{Name: "id", Type: value.Int, Nullable: true}, {Name: "case", Type: value.Int, Nullable: true}, {Name: "float8", Type: value.Float, Nullable: true}, {Name: "coalesce", Type: value.Int, Nullable: true},
			{Name: "?column?", Type: value.String, Nullable: true}, {Name: "int8", Type: value.Int, Nullable: true}, {Name: "timestamptz", Type: value.Time, Nullable: true},
			{Name: "float8", Type: value.Float, Nullable: true}, {Name: "text", Type: value.String, Nullable: true}, {Name: "?column?", Type: value.Int, Nullable: true},
		}), fmt.Sprint(rows.Columns))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT *, CASE WHEN FALSE THEN id ELSE id END FROM t ORDER BY id"), "ambiguous, as in Postgres")
	})

	t.Run("uses composite primary keys for ranges", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE c (a text, b bigint, v bigint, PRIMARY KEY (a, b))")
		mustExec(t, s, "INSERT INTO c VALUES ('x', 1, 1), ('x', 2, 2), ('x\u0000', 1, 3), ('y', 1, 4), ('', 0, 5)")
		is.Equal(t, "1\n2", query(t, s, "SELECT v FROM c WHERE a = 'x'"))
		is.Equal(t, "2", query(t, s, "SELECT v FROM c WHERE a = 'x' AND b = 2"))
		is.Equal(t, "2", query(t, s, "SELECT v FROM c WHERE a = 'x' AND b > 1"))
		is.Equal(t, "1\n2\n3\n4", query(t, s, "SELECT v FROM c WHERE a >= 'x'"))
		is.Equal(t, "3", query(t, s, "SELECT v FROM c WHERE a = 'x\u0000'"))
		is.Equal(t, "5\n1\n2\n3\n4", query(t, s, "SELECT v FROM c"))
		is.Equal(t, "5\n1\n3\n4", query(t, s, "SELECT v FROM c WHERE b = 1 OR v = 5"), "equality on a non-prefix key column is a filter")
	})
}

func TestSession_Update(t *testing.T) {
	t.Run("updates matching rows from their old values", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		res := mustExec(t, s, "UPDATE t SET f = coalesce(f, 0) + 1, name = upper(name) WHERE id > 1")
		is.Equal(t, int64(2), res.RowsAffected)
		is.Equal(t, `1 "alice" 1.5
2 "BOB" 1
3 "CAROL" -1.5`, query(t, s, "SELECT id, name, f FROM t"))
		res = mustExec(t, s, "UPDATE t SET f = NULL WHERE id = 99")
		is.Equal(t, int64(0), res.RowsAffected)
		mustExec(t, s, "UPDATE t SET f = $1, b = $2 WHERE id = $3", 2.0, nil, 1)
		is.Equal(t, "2 NULL", query(t, s, "SELECT f, b FROM t WHERE id = 1"))
	})

	t.Run("evaluates every assignment against the old row", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE p (id bigint PRIMARY KEY, a bigint, b bigint)")
		mustExec(t, s, "INSERT INTO p VALUES (1, 1, 2)")
		mustExec(t, s, "UPDATE p SET a = b, b = a")
		is.Equal(t, "1 2 1", query(t, s, "SELECT * FROM p"))
	})

	t.Run("moves rows when the primary key changes, and rejects a move whose outcome depends on row order", func(t *testing.T) {
		verify.Covers(t, "an UPDATE of primary key columns succeeds or fails as in Postgres in every row order, and a row that moves onto a key another row moves away from is ErrUnsupported", "UNS-KEY-UPDATE-ORDER", "INV-PG-SUBSET")
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE h (id bigint PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO h VALUES (1), (2), (3)")
		res := mustExec(t, s, "UPDATE h SET id = id + 10 WHERE id < 100")
		is.Equal(t, int64(3), res.RowsAffected)
		is.Equal(t, "11\n12\n13", query(t, s, "SELECT id FROM h"))
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "UPDATE h SET id = id - 1"), "12 moves onto 11, which moves away")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "UPDATE h SET id = id + 1"))
		is.Equal(t, "11\n12\n13", query(t, s, "SELECT id FROM h"), "a rejected update changes nothing")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE h SET id = 11 WHERE id = 13"), "a move onto a key that stays is a duplicate")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE h SET id = 5"), "two rows onto one key is a duplicate")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE h SET id = 11 WHERE id > 11"))
		is.Equal(t, int64(3), mustExec(t, s, "UPDATE h SET id = id").RowsAffected, "a row that stays is no move")
		is.Equal(t, int64(2), mustExec(t, s, "UPDATE h SET id = id * 2 WHERE id > 11").RowsAffected)
		is.Equal(t, "11\n24\n26", query(t, s, "SELECT id FROM h"))
		is.Equal(t, int64(1), mustExec(t, s, "UPDATE h SET id = 12 WHERE id = 11").RowsAffected)
		is.Equal(t, "12\n24\n26", query(t, s, "SELECT id FROM h"))
		mustExec(t, s, "CREATE TABLE k (a bigint, b bigint, PRIMARY KEY (a, b))")
		mustExec(t, s, "INSERT INTO k VALUES (1, 1), (1, 2)")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "UPDATE k SET b = 3 - b"), "the rows swap keys")
		is.Equal(t, int64(2), mustExec(t, s, "UPDATE k SET a = 2").RowsAffected)
		is.Equal(t, "2 1\n2 2", query(t, s, "SELECT * FROM k"))
	})

	t.Run("checks constraints and types", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "UPDATE t SET name = NULL WHERE id = 1"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET b = 1"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET f = b"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "UPDATE t SET nope = 1"))
		is.Error(t, exec.ErrSyntax, queryErr(t, s, "UPDATE t SET f = 1, f = 2"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET f = 1 WHERE name"))
		mustExec(t, s, "UPDATE t SET f = 2, name = id")
		is.Equal(t, "2 \"1\"\n2 \"2\"\n2 \"3\"", query(t, s, "SELECT f, name FROM t"), "an integer widens to the float column and is written into the text column")
	})
}

func TestSession_Delete(t *testing.T) {
	t.Run("deletes matching rows", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		res := mustExec(t, s, "DELETE FROM t WHERE id = 2")
		is.Equal(t, int64(1), res.RowsAffected)
		is.Equal(t, "1\n3", query(t, s, "SELECT id FROM t"))
		res = mustExec(t, s, "DELETE FROM t WHERE f < 0")
		is.Equal(t, int64(1), res.RowsAffected)
		res = mustExec(t, s, "DELETE FROM t")
		is.Equal(t, int64(1), res.RowsAffected)
		is.Equal(t, "", query(t, s, "SELECT id FROM t"))
		mustExec(t, s, "INSERT INTO t (id, name) VALUES (1, 'again')")
		is.Equal(t, "1", query(t, s, "SELECT id FROM t"))
	})
}

func TestSession_Transactions(t *testing.T) {
	t.Run("statements see earlier writes of their transaction, and rollback discards everything including DDL", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "BEGIN")
		is.True(t, s.InTx())
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, s, "UPDATE t SET n = n + 1")
		is.Equal(t, "1 2", query(t, s, "SELECT * FROM t"))
		mustExec(t, s, "ROLLBACK")
		is.True(t, !s.InTx())
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "SELECT * FROM t"))
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, s, "COMMIT")
		is.Equal(t, "1 1", query(t, s, "SELECT * FROM t"))
	})

	t.Run("a failed statement aborts the transaction until it ends, as in Postgres", func(t *testing.T) {
		verify.Covers(t, "after a failed statement every statement of the transaction fails with ErrTxAborted, COMMIT rolls it back, and ROLLBACK ends it", "INV-PG-SUBSET")
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (2), (1)"))
		is.Error(t, exec.ErrTxAborted, queryErr(t, s, "SELECT id FROM t"))
		is.Error(t, exec.ErrTxAborted, queryErr(t, s, "INSERT INTO t VALUES (3)"))
		is.Error(t, exec.ErrTxAborted, queryErr(t, s, "SELECT 1"))
		mustExec(t, s, "BEGIN")
		is.True(t, s.InTx(), "BEGIN inside a transaction only warns")
		mustExec(t, s, "COMMIT")
		is.True(t, !s.InTx())
		is.Equal(t, "", query(t, s, "SELECT id FROM t"), "COMMIT of an aborted transaction rolls it back")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / 0"))
		is.Error(t, exec.ErrTxAborted, s.Commit(t.Context()), "the driver's commit reports the rollback")
		is.True(t, !s.InTx())
		is.Equal(t, "", query(t, s, "SELECT id FROM t"))
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT 1.5"), "a rejection aborts the transaction too")
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		mustExec(t, s, "COMMIT")
		is.Equal(t, "1", query(t, s, "SELECT id FROM t"))
	})

	t.Run("COMMIT and ROLLBACK without a transaction only warn, and the driver's calls are strict", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "COMMIT")
		mustExec(t, s, "ROLLBACK")
		is.Error(t, exec.ErrNoTx, s.Commit(t.Context()))
		is.Error(t, exec.ErrNoTx, s.Rollback())
		mustExec(t, s, "BEGIN")
		is.Error(t, exec.ErrTxInProgress, s.Begin(t.Context(), false))
		mustExec(t, s, "COMMIT")
	})

	t.Run("read-only transactions reject writes and DDL", func(t *testing.T) {
		verify.Covers(t, "a read-only transaction rejects INSERT, UPDATE, DELETE, and DDL", "INV-READ-ONLY")
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "INSERT INTO t VALUES (1)"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "UPDATE t SET id = 2"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "DELETE FROM t"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "CREATE TABLE u (id bigint PRIMARY KEY)"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "DROP TABLE t"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Equal(t, "", query(t, s, "SELECT * FROM t"))
		mustExec(t, s, "COMMIT")
	})

	t.Run("a transaction sees a snapshot, and an explicit transaction gets ErrConflict while autocommit retries", func(t *testing.T) {
		verify.Covers(t, "an explicit transaction that loses gets ErrConflict, while the same statement in autocommit retries and succeeds", "INV-RETRYABLE-ERROR", "INV-AUTOCOMMIT-RETRY")
		w := newWorld(t)
		a, _ := w.session(exec.Options{})
		b, _ := w.session(exec.Options{})
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")

		mustExec(t, a, "BEGIN")
		is.Equal(t, "0", query(t, a, "SELECT n FROM t"))
		mustExec(t, b, "UPDATE t SET n = 10")
		is.Equal(t, "0", query(t, a, "SELECT n FROM t"), "the snapshot predates b's commit")
		mustExec(t, a, "UPDATE t SET n = n + 1")
		_, _, err := run(t.Context(), a, "COMMIT")
		is.Error(t, engine.ErrConflict, err)
		is.True(t, !a.InTx(), "the session left the transaction")
		is.Equal(t, "10", query(t, a, "SELECT n FROM t"))

		// Autocommit from a stale cached manifest: b's commit made a's manifest stale, but
		// a refreshes it when the statement begins, so there is no conflict to retry.
		mustExec(t, a, "UPDATE t SET n = n + 1")
		is.Equal(t, "11", query(t, b, "SELECT n FROM t"))

		// A read-only transaction never conflicts.
		mustExec(t, a, "BEGIN READ ONLY")
		is.Equal(t, "11", query(t, a, "SELECT n FROM t"))
		mustExec(t, b, "UPDATE t SET n = n + 1")
		is.Equal(t, "11", query(t, a, "SELECT n FROM t"))
		mustExec(t, a, "COMMIT")
	})

	t.Run("autocommit retries a conflict caused by a concurrent commit during the statement", func(t *testing.T) {
		verify.Covers(t, "an autocommit statement that conflicts with a commit made during it retries and applies once", "INV-AUTOCOMMIT-RETRY")
		w := newWorld(t)
		a, _ := w.session(exec.Options{})
		b, _ := w.session(exec.Options{})
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")
		// b commits between a's freshness check and a's manifest write: a's first PUT-conditional
		// on the manifest fails with a real precondition failure, and a retries the statement.
		interfered := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && strings.HasSuffix(key, "manifest") && !interfered {
				interfered = true
				w.decide = nil
				mustExec(t, b, "UPDATE t SET n = n + 100")
			}
			return s3fake.OutcomeNormal
		}
		mustExec(t, a, "UPDATE t SET n = n + 1")
		is.True(t, interfered)
		is.Equal(t, "101", query(t, b, "SELECT n FROM t"), "both updates applied, in order")
	})
}

func TestMutations(t *testing.T) {
	t.Run("UpdateSeesOwnWrites revisits moved rows, which the Halloween test catches", func(t *testing.T) {
		verify.Covers(t, "with the mutant, an UPDATE that moves keys revisits rows it already moved", "MUT-UPDATE-SEES-OWN-WRITES")
		w := newWorld(t)
		s, _ := w.session(exec.Options{Mutations: exec.Mutations{UpdateSeesOwnWrites: true}})
		mustExec(t, s, "CREATE TABLE h (id bigint PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO h VALUES (1), (2), (3)")
		mustExec(t, s, "UPDATE h SET id = id + 10 WHERE id < 100")
		is.Equal(t, "101\n102\n103", query(t, s, "SELECT id FROM h"), "the mutant moves every row until it leaves the range")
	})

	t.Run("FreshSnapshotPerStatement lets a transaction see a commit made after it began", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a transaction sees a commit made after it began", "MUT-FRESH-SNAPSHOT")
		for _, mutated := range []bool{false, true} {
			w := newWorld(t)
			a, _ := w.session(exec.Options{Mutations: exec.Mutations{FreshSnapshotPerStatement: mutated}})
			b, _ := w.session(exec.Options{})
			mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
			mustExec(t, a, "INSERT INTO t VALUES (1, 0)")
			mustExec(t, a, "BEGIN READ ONLY")
			is.Equal(t, "0", query(t, a, "SELECT n FROM t"))
			mustExec(t, b, "UPDATE t SET n = 1")
			want := "0"
			if mutated {
				want = "1"
			}
			is.Equal(t, want, query(t, a, "SELECT n FROM t"))
			mustExec(t, a, "COMMIT")
		}
	})

	t.Run("RetryAfterUnknownCommit applies a statement twice", func(t *testing.T) {
		verify.Covers(t, "with the mutant, an autocommit UPDATE whose outcome was unknown is applied twice", "MUT-RETRY-UNKNOWN")
		for _, mutated := range []bool{false, true} {
			w := newWorld(t)
			s, _ := w.session(exec.Options{Mutations: exec.Mutations{RetryAfterUnknownCommit: mutated}})
			mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
			mustExec(t, s, "INSERT INTO t VALUES (1, 0)")
			// The manifest write lands but its acknowledgement is lost, and then the store
			// stops answering reads until the commit has given up resolving the outcome.
			puts, failedGets := 0, 0
			w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
				if !strings.HasSuffix(key, "manifest") {
					return s3fake.OutcomeNormal
				}
				switch kind {
				case s3.KindPutConditional:
					puts++
					if puts == 1 {
						return s3fake.OutcomeLostAck
					}
				case s3.KindGet, s3.KindGetConditional:
					if puts >= 1 && failedGets < 8 {
						failedGets++
						return s3fake.OutcomeError
					}
				}
				return s3fake.OutcomeNormal
			}
			_, _, err := run(t.Context(), s, "UPDATE t SET n = n + 1")
			w.decide = nil
			if mutated {
				is.NotError(t, err, "the mutant retried until the store answered")
				is.Equal(t, "2", query(t, s, "SELECT n FROM t"), "and applied the update twice")
			} else {
				is.Error(t, engine.ErrCommitUnknown, err)
				is.Equal(t, "1", query(t, s, "SELECT n FROM t"))
			}
		}
	})
}

func TestPrepare(t *testing.T) {
	t.Run("counts parameters", func(t *testing.T) {
		st, err := exec.Prepare("SELECT $1, $3 FROM t WHERE $2 = 1 ORDER BY $1 LIMIT $3")
		is.NotError(t, err)
		is.Equal(t, 3, st.NumParams())
		st, err = exec.Prepare("INSERT INTO t VALUES ($2), ($1)")
		is.NotError(t, err)
		is.Equal(t, 2, st.NumParams())
	})

	t.Run("reports syntax errors", func(t *testing.T) {
		_, err := exec.Prepare("SELEC 1")
		is.True(t, errors.Is(err, exec.ErrSyntax))
	})
}
