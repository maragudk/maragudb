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
)

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
			vals = append(vals, value.NewString(p))
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

// query runs a SELECT and renders the rows as one line per row, values separated by spaces.
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
	mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY, name string NOT NULL, f float, b bool, data bytes, at time)")
	mustExec(t, s, `INSERT INTO t VALUES (1, 'alice', 1.5, TRUE, X'01', TIME '2024-01-01T00:00:00Z'), (2, 'bob', NULL, FALSE, NULL, NULL), (3, 'carol', -2.5, NULL, X'', TIME '2024-06-01T12:30:00.5Z')`)
}

func TestSession_DDL(t *testing.T) {
	t.Run("creates and drops tables, with IF EXISTS variants", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY)")
		is.Error(t, exec.ErrTableExists, queryErr(t, s, "CREATE TABLE t (id int PRIMARY KEY)"))
		mustExec(t, s, "CREATE TABLE IF NOT EXISTS t (id int PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		mustExec(t, s, "DROP TABLE t")
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "DROP TABLE t"))
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "SELECT * FROM t"))
		mustExec(t, s, "DROP TABLE IF EXISTS t")
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY)")
		is.Equal(t, "", query(t, s, "SELECT * FROM t"), "the old rows are gone")
	})

	t.Run("rejects bad definitions", func(t *testing.T) {
		s := newSession(t)
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id int, PRIMARY KEY (nope))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id int, id string, PRIMARY KEY (id))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id int, PRIMARY KEY (id, id))"))
		is.True(t, strings.Contains(queryErr(t, s, "SELECT 1 + 'a'").Error(), "operator + does not exist for int and string"))
	})

	t.Run("primary key columns are not null", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (a int, b int, PRIMARY KEY (a, b))")
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "INSERT INTO t VALUES (1, NULL)"))
	})
}

func TestSession_Insert(t *testing.T) {
	t.Run("inserts rows and reads them back in key order", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Equal(t, `1 "alice" 1.5 true X'01' 2024-01-01T00:00:00Z
2 "bob" NULL false NULL NULL
3 "carol" -2.5 NULL X'' 2024-06-01T12:30:00.5Z`, query(t, s, "SELECT * FROM t"))
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
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name) VALUES ('9', 'x')"))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (9, 'x', '1.5')"))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9.0, 'x')"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, nope) VALUES (9, 'x')"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, id) VALUES (9, 9)"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9)"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, name) VALUES (9, 'x', 1)"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "INSERT INTO t (id, name) VALUES (id, 'x')"))
	})

	t.Run("adapts an int literal to a float column but not a parameter", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		mustExec(t, s, "INSERT INTO t (id, name, f) VALUES (10, 'x', 2)")
		is.Equal(t, "2.0", query(t, s, "SELECT f FROM t WHERE id = 10"))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (11, 'x', $1)", 2))
		is.Error(t, exec.ErrType, queryErr(t, s, "INSERT INTO t (id, name, f) VALUES (11, 'x', $1 + 1)", 2))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT f + $1 FROM t", 2))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT * FROM t WHERE f = $1", 2))
		mustExec(t, s, "INSERT INTO t (id, name, f) VALUES (11, 'x', $1)", 2.0)
		is.Equal(t, "2.0", query(t, s, "SELECT f FROM t WHERE id = $1 AND f = $2", 11, 2.0))
	})

	t.Run("a multi-row insert that fails leaves nothing behind", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t (id, name) VALUES (7, 'x'), (8, 'y'), (8, 'z')"))
		is.Equal(t, "", query(t, s, "SELECT id FROM t WHERE id >= 7"))
	})

	t.Run("binds parameters of every type", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		at := time.Date(2025, 2, 3, 4, 5, 6, 7, time.FixedZone("x", 3600))
		mustExec(t, s, "INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6)", 5, "eve", 0.25, true, []byte{0xAB}, at)
		is.Equal(t, `5 "eve" 0.25 true X'ab' 2025-02-03T03:05:06.000000007Z`, query(t, s, "SELECT * FROM t WHERE id = $1", 5))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT * FROM t WHERE id = $2", 5))
	})
}

func TestSession_Select(t *testing.T) {
	t.Run("evaluates expressions like Postgres", func(t *testing.T) {
		s := newSession(t)
		tests := []struct {
			sql  string
			want string
		}{
			{"SELECT 1 + 2 * 3 - 4 / 2 % 3", "5"},
			{"SELECT 7 / 2, -7 / 2, 7 % 3, -7 % 3", "3 -3 1 -1"},
			{"SELECT 1.5 + 1, 2 * 2.5, 1 / 4.0", "2.5 5.0 0.25"},
			{"SELECT -(-5), -5, abs(-3), abs(-2.5), abs(0.0)", "5 -5 3 2.5 0.0"},
			{"SELECT 'a' || 'b' || 'c', 'a' || NULL, X'01' || X'02'", `"abc" NULL X'0102'`},
			{"SELECT length('héllo'), length(X'0102'), lower('ÄBC'), upper('äbc')", `5 2 "äbc" "ÄBC"`},
			{"SELECT 1 = 1, 1 <> 1, 1 < 2, 2 <= 2, 3 > 2, 2 >= 3", "true false true true true false"},
			{"SELECT 'a' < 'b', 'B' < 'a', 'abc' < 'abd', '' < 'a'", "true true true true"},
			{"SELECT X'01' < X'0102', X'02' > X'0199', TRUE > FALSE", "true true true"},
			{"SELECT TIME '2024-01-01T00:00:00Z' < TIME '2024-01-01T00:00:00.000000001Z'", "true"},
			{"SELECT TRUE AND NULL, FALSE AND NULL, TRUE OR NULL, FALSE OR NULL, NOT NULL, NULL AND NULL", "NULL false true NULL NULL NULL"},
			{"SELECT NULL = NULL, NULL IS NULL, NULL IS NOT NULL, 1 IS NULL, 1 = NULL", "NULL true false false NULL"},
			{"SELECT 2 BETWEEN 1 AND 3, 0 BETWEEN 1 AND 3, 2 NOT BETWEEN 1 AND 3, NULL BETWEEN 1 AND 3, 5 BETWEEN 1 AND NULL, 0 BETWEEN 1 AND NULL", "true false false NULL NULL false"},
			{"SELECT 1 IN (1, 2), 3 IN (1, 2), 3 IN (1, NULL), 1 IN (1, NULL), NULL IN (1), 3 NOT IN (1, 2), 3 NOT IN (1, NULL)", "true false NULL true NULL true NULL"},
			{"SELECT 'abc' LIKE 'a%', 'abc' LIKE 'a_c', 'abc' LIKE 'a_', 'a%c' LIKE 'a\\%c', 'abc' LIKE 'a\\%c', 'abc' NOT LIKE '%b%', 'héllo' LIKE 'h_llo', '' LIKE '%', '' LIKE '_', NULL LIKE 'a'", "true true false true false false true true false NULL"},
			{"SELECT CASE WHEN 1 > 2 THEN 'a' WHEN 2 > 1 THEN 'b' ELSE 'c' END, CASE WHEN NULL THEN 1 ELSE 2 END, CASE WHEN FALSE THEN 1 END", `"b" 2 NULL`},
			{"SELECT CASE 2 WHEN 1 THEN 'one' WHEN 2 THEN 'two' END, CASE NULL WHEN NULL THEN 'null' ELSE 'other' END", `"two" "other"`},
			{"SELECT coalesce(NULL, 1), coalesce(NULL, NULL), coalesce(1, 2), coalesce(NULL, 1.5, 2)", "1 NULL 1 1.5"},
			{"SELECT -abs(NULL), abs(NULL) + 1, length(NULL) + 1, lower(NULL) || 'a'", "NULL NULL NULL NULL"},
			{"SELECT CAST(1 AS float), CAST(2.5 AS int), CAST(3.5 AS int), CAST(-2.5 AS int), CAST(2.4 AS int)", "1.0 2 4 -2 2"},
			{"SELECT CAST(42 AS string), CAST(TRUE AS string), CAST(1.5 AS string), CAST(X'6869' AS string), CAST(TIME '2024-01-01T00:00:00.25Z' AS string)", `"42" "true" "1.5" "hi" "2024-01-01T00:00:00.25Z"`},
			{"SELECT CAST(' 42 ' AS int), CAST('1.5e3' AS float), CAST('t' AS bool), CAST('NO' AS bool), CAST('on' AS bool), CAST('hi' AS bytes), CAST('2024-01-01 00:00:00Z' AS time)", `42 1500.0 true false true X'6869' 2024-01-01T00:00:00Z`},
			{"SELECT CAST('NaN' AS float) > CAST('Infinity' AS float), CAST('NaN' AS float) = CAST('nan' AS float), CAST('-inf' AS float) < -1e308", "true true true"},
			{"SELECT CAST('Infinity' AS float) - CAST('Infinity' AS float), CAST('Infinity' AS float) * 0", "NaN NaN"},
			{"SELECT CAST(CAST('NaN' AS float) AS string), CAST(1e20 AS string), CAST(1e14 AS string), CAST(0.0001 AS string), CAST(0.00001 AS string), CAST(-0.0 AS string)", `"NaN" "1e+20" "100000000000000" "0.0001" "1e-05" "-0"`},
			{"SELECT CAST(NULL AS int) IS NULL, CAST(NULL AS string)", "true NULL"},
			{"SELECT 1 AS one, 2 two", "1 2"},
			{"SELECT 1 WHERE FALSE", ""},
			{"SELECT 1 WHERE NULL", ""},
			{"SELECT 9223372036854775807, -9223372036854775808, 9223372036854775807 % -1, -9223372036854775808 % -1", "9223372036854775807 -9223372036854775808 0 0"},
			{"SELECT 1e308 * 0.5, 1e-308 * 0.0, 0.0 / 1.5", "5e+307 0.0 0.0"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				is.Equal(t, test.want, query(t, s, test.sql))
			})
		}
	})

	t.Run("reports evaluation errors like Postgres", func(t *testing.T) {
		s := newSession(t)
		tests := []struct {
			sql  string
			want error
		}{
			{"SELECT 1 / 0", exec.ErrDivisionByZero},
			{"SELECT 1 % 0", exec.ErrDivisionByZero},
			{"SELECT 1.0 / 0.0", exec.ErrDivisionByZero},
			{"SELECT 9223372036854775807 + 1", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 - 1", exec.ErrOutOfRange},
			{"SELECT 4611686018427387904 * 2", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 * -1", exec.ErrOutOfRange},
			{"SELECT -9223372036854775808 / -1", exec.ErrOutOfRange},
			{"SELECT -(-9223372036854775808)", exec.ErrOutOfRange},
			{"SELECT abs(-9223372036854775808)", exec.ErrOutOfRange},
			{"SELECT 1e308 * 10", exec.ErrOutOfRange},
			{"SELECT 1e308 + 1e308", exec.ErrOutOfRange},
			{"SELECT 1e-308 * 1e-308", exec.ErrOutOfRange},
			{"SELECT 1e-308 / 1e308", exec.ErrOutOfRange},
			{"SELECT CAST(1e19 AS int)", exec.ErrOutOfRange},
			{"SELECT CAST(CAST('NaN' AS float) AS int)", exec.ErrOutOfRange},
			{"SELECT CAST('99999999999999999999' AS int)", exec.ErrOutOfRange},
			{"SELECT CAST('x' AS int)", exec.ErrCast},
			{"SELECT CAST('1.5' AS int)", exec.ErrCast},
			{"SELECT CAST('x' AS float)", exec.ErrCast},
			{"SELECT CAST('1_000' AS float)", exec.ErrCast},
			{"SELECT CAST('0x1p-2' AS float)", exec.ErrCast},
			{"SELECT CAST('0x10' AS int)", exec.ErrCast},
			{"SELECT CAST('1_000' AS int)", exec.ErrCast},
			{"SELECT CAST('o' AS bool)", exec.ErrCast},
			{"SELECT CAST('' AS bool)", exec.ErrCast},
			{"SELECT CAST('yesterday' AS time)", exec.ErrCast},
			{"SELECT CAST(1 AS bool)", exec.ErrCast},
			{"SELECT CAST(TRUE AS int)", exec.ErrCast},
			{"SELECT CAST(1 AS time)", exec.ErrCast},
			{"SELECT CAST(X'ff' AS string) IS NULL", exec.ErrCast},
			{"SELECT 1 + 'a'", exec.ErrType},
			{"SELECT 1 + 1.5 WHERE 'a' = 1", exec.ErrType},
			{"SELECT 'a' || 1", exec.ErrType},
			{"SELECT 1.5 % 1.0", exec.ErrType},
			{"SELECT -'a'", exec.ErrType},
			{"SELECT -NULL", exec.ErrType},
			{"SELECT 'abc' LIKE 'a\\'", exec.ErrInvalid},
			{"SELECT 'x' LIKE '%\\'", exec.ErrInvalid},
			{"SELECT NOT 1", exec.ErrType},
			{"SELECT 1 AND TRUE", exec.ErrType},
			{"SELECT 1 BETWEEN 'a' AND 'b'", exec.ErrType},
			{"SELECT 1 IN ('a')", exec.ErrType},
			{"SELECT 1 LIKE 'a'", exec.ErrType},
			{"SELECT CASE WHEN 1 THEN 1 END", exec.ErrType},
			{"SELECT TRUE AND CASE WHEN TRUE THEN NULL END", exec.ErrType},
			{"SELECT TRUE AND coalesce(NULL, NULL)", exec.ErrType},
			{"SELECT 1 + coalesce(CASE WHEN FALSE THEN FALSE END)", exec.ErrType},
			{"SELECT 1 + CASE WHEN FALSE THEN NULL END", exec.ErrType},
			{"SELECT CASE WHEN TRUE THEN 1 ELSE 'a' END", exec.ErrType},
			{"SELECT CASE 1 WHEN 'a' THEN 1 END", exec.ErrType},
			{"SELECT coalesce(1, 'a')", exec.ErrType},
			{"SELECT length(1)", exec.ErrType},
			{"SELECT lower(1)", exec.ErrType},
			{"SELECT abs('a')", exec.ErrType},
			{"SELECT 'abc' LIKE 'a\\'", exec.ErrInvalid},
			{"SELECT nope(1)", exec.ErrInvalid},
			{"SELECT length()", exec.ErrInvalid},
			{"SELECT coalesce()", exec.ErrInvalid},
			{"SELECT x", exec.ErrInvalid},
			{"SELECT *", exec.ErrInvalid},
			{"SELECT 1 WHERE 1", exec.ErrType},
			{"SELECT 1 LIMIT -1", exec.ErrInvalid},
			{"SELECT 1 OFFSET 'a'", exec.ErrType},
			{"SELECT 1 ORDER BY 2", exec.ErrInvalid},
			{"SELECT 1 ORDER BY 0", exec.ErrInvalid},
			{"SELECT 1 ORDER BY FALSE", exec.ErrInvalid},
			{"SELECT 1 ORDER BY 'a'", exec.ErrInvalid},
			{"SELECT 1 ORDER BY NULL", exec.ErrInvalid},
			{"SELECT 1 AS a, 2 AS a ORDER BY a", exec.ErrInvalid},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				is.Error(t, test.want, queryErr(t, s, test.sql))
			})
		}
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT FALSE AND 1"), "a pruned branch is still type-checked")
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN FALSE THEN 1 ELSE 'a' END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN TRUE THEN 'a' END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT coalesce(NULL, 1, 'a')"))
	})

	t.Run("folds constants before reading rows, like Postgres, except behind a false branch", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE e (id int PRIMARY KEY, b bool)")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT * FROM e WHERE 1 / 0 = 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT * FROM e WHERE id = 1 AND 1 / 0 = 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT CASE WHEN b THEN 1 / 0 ELSE 0 END FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT coalesce(id, 1 / 0) FROM e"))
		is.Equal(t, "", query(t, s, "SELECT * FROM e WHERE FALSE AND 1 / 0 = 1"))
		is.Equal(t, "", query(t, s, "SELECT * FROM e WHERE FALSE AND id = 1 / 0"), "the planner does not bind pruned key comparisons")
		is.Equal(t, int64(0), mustExec(t, s, "DELETE FROM e WHERE FALSE AND id = 1 / 0").RowsAffected)
		is.Equal(t, "", query(t, s, "SELECT 1 WHERE FALSE AND 1 IN (1 / 0)"), "a pruned IN element never compares")
		is.Equal(t, "true", query(t, s, "SELECT TRUE OR 1 IN (1 / 0)"))
		is.Equal(t, "true", query(t, s, "SELECT coalesce(TRUE, 1 IN (1 / 0))"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN FALSE THEN 1 / 0 ELSE 1 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN TRUE THEN 1 ELSE 1 / 0 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN 1 / 0 = 1 THEN 2 END"))
		is.Equal(t, "1", query(t, s, "SELECT CASE 1 WHEN 1 THEN 1 WHEN 1 / 0 THEN 2 END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE 1 WHEN 1 THEN 1 WHEN 'a' THEN 2 END"))
		is.Error(t, exec.ErrType, queryErr(t, s, "SELECT CASE WHEN TRUE THEN 1 WHEN 1 THEN 2 END"))
		is.Equal(t, "1", query(t, s, "SELECT coalesce(1, 1 / 0)"))
		is.Equal(t, "true", query(t, s, "SELECT TRUE OR 1 / 0 = 1"))
		mustExec(t, s, "INSERT INTO e VALUES (-9223372036854775808, TRUE)")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL + -id FROM e"), "a strict operator with a known NULL does not evaluate the other side")
		is.Equal(t, "NULL", query(t, s, "SELECT -id * CAST(NULL AS int) FROM e"))
		is.Equal(t, "NULL", query(t, s, "SELECT CAST(id AS string) LIKE NULL FROM e"))
		is.Equal(t, "1", query(t, s, "SELECT CASE NULL WHEN id THEN -id ELSE 1 END FROM e"))
		is.Equal(t, "1", query(t, s, "SELECT CASE id WHEN NULL THEN -id ELSE 1 END FROM e"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT NULL + 1 / 0 FROM e"), "but a constant operand is still folded first")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL BETWEEN NULL AND -id FROM e"), "a known NULL value folds BETWEEN")
		is.Equal(t, "NULL", query(t, s, "SELECT NULL BETWEEN -id AND 1 FROM e"))
		mustExec(t, s, "INSERT INTO e VALUES (5, NULL)")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT b BETWEEN TRUE AND (1 / (id - 5) > 0) FROM e WHERE id = 5"), "a NULL column still evaluates the bounds")
		is.Equal(t, "", query(t, s, "SELECT id FROM e WHERE b AND 1 / (id - 5) > 0 AND id = 5"), "a WHERE conjunct that is NULL stops the row's evaluation")
		is.Equal(t, "", query(t, s, "SELECT id FROM e WHERE b BETWEEN TRUE AND (1 / (id - 5) > 0) AND id = 5"), "BETWEEN at the top level splits into conjuncts")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM e WHERE id = 5 AND 1 / (id - 5) > 0"), "a true conjunct continues")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT id FROM e WHERE (b OR 1 / (id - 5) > 0) AND id = 5"), "but not inside OR")
		mustExec(t, s, "DELETE FROM e WHERE id = 5")
		is.Equal(t, "false", query(t, s, "SELECT id NOT IN (id, -id) FROM e"), "IN stops at the first matching column element")
		is.Equal(t, "true", query(t, s, "SELECT id IN (1, id, -id) FROM e"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT id IN (1, -id, id) FROM e"))
		is.Error(t, exec.ErrOutOfRange, queryErr(t, s, "SELECT NULL IN (id, -id) FROM e"), "a NULL left side still evaluates every element")
		is.Equal(t, "NULL", query(t, s, "SELECT TRUE AND coalesce(CASE WHEN FALSE THEN FALSE END)"), "a pruned branch still types the CASE")
		is.Equal(t, "false", query(t, s, "SELECT 0 BETWEEN 1 AND 1 / 0"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 2 BETWEEN 1 AND 1 / 0"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT NULL BETWEEN 1 AND 1 / 0"))
		is.Equal(t, "false", query(t, s, "SELECT 'a' LIKE 'a\\'"), "the trailing escape is only an error when matching reaches it")
		is.Equal(t, "false", query(t, s, "SELECT '' LIKE '\\'"))
		is.Equal(t, "false", query(t, s, "SELECT '' LIKE '%\\'"))
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
			{"SELECT id FROM t WHERE at > TIME '2024-01-01T00:00:00Z'", "3"},
			{"SELECT id FROM t WHERE data = X''", "3"},
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
			{"SELECT id FROM t ORDER BY TIME '2024-01-01T00:00:00Z', id DESC", "3\n2\n1"},
			{"SELECT id FROM t ORDER BY X'01', id DESC", "3\n2\n1"},
			{"SELECT id AS x FROM t ORDER BY x DESC", "3\n2\n1"},
			{"SELECT -id AS id FROM t ORDER BY id", "-3\n-2\n-1"},
			{"SELECT id FROM t ORDER BY -id", "3\n2\n1"},
			{"SELECT id, id FROM t ORDER BY id DESC", "3 3\n2 2\n1 1"},
			{"SELECT *, * FROM t ORDER BY id DESC LIMIT 1", `3 "carol" -2.5 NULL X'' 2024-06-01T12:30:00.5Z 3 "carol" -2.5 NULL X'' 2024-06-01T12:30:00.5Z`},
			{"SELECT id FROM t ORDER BY id DESC LIMIT 2", "3\n2"},
			{"SELECT id FROM t ORDER BY id LIMIT 1 OFFSET 1", "2"},
			{"SELECT id FROM t LIMIT 2", "1\n2"},
			{"SELECT id FROM t OFFSET 2", "3"},
			{"SELECT id FROM t LIMIT 0", ""},
			{"SELECT id FROM t LIMIT NULL", "1\n2\n3"},
			{"SELECT id FROM t LIMIT $1 OFFSET $2", "2"},
			{"SELECT id, id * 2 AS double, length(name), CAST(id AS string) || name FROM t WHERE id = 1", `1 2 5 "1alice"`},
			{"SELECT f + 1, f * 2, -f FROM t WHERE id = 1", "2.5 3.0 -1.5"},
			{"SELECT 1 / (id - 1) FROM t WHERE id > 1 LIMIT 1", "1"},
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
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / (id - 1) FROM t LIMIT 1"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / (id - 1) FROM t LIMIT 1 OFFSET 1"), "OFFSET evaluates the skipped rows")
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "SELECT 1 / (id - 1) FROM t ORDER BY id DESC LIMIT 1"), "ORDER BY evaluates every row")
	})

	t.Run("names and types output columns", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		rows, _, err := run(t.Context(), s, "SELECT id, name AS n, f + 1, length(name), CASE WHEN TRUE THEN 1 END, CAST(f AS int), NULL, 1, CASE WHEN FALSE THEN 1 ELSE id END, CASE WHEN FALSE THEN 1 ELSE CAST(2 AS int) END, CAST(1 AS float), CASE WHEN TRUE THEN 1 ELSE coalesce(id) END FROM t")
		is.NotError(t, err)
		is.Equal(t, fmt.Sprint([]exec.Column{
			{Name: "id", Type: value.Int}, {Name: "n", Type: value.String}, {Name: "?column?", Type: value.Float, Nullable: true},
			{Name: "length", Type: value.Int, Nullable: true}, {Name: "case", Type: value.Int, Nullable: true}, {Name: "f", Type: value.Int, Nullable: true},
			{Name: "?column?", Type: value.Unknown, Nullable: true}, {Name: "?column?", Type: value.Int, Nullable: true},
			{Name: "id", Type: value.Int, Nullable: true}, {Name: "case", Type: value.Int, Nullable: true}, {Name: "float", Type: value.Float, Nullable: true}, {Name: "coalesce", Type: value.Int, Nullable: true},
		}), fmt.Sprint(rows.Columns))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "SELECT *, CASE WHEN FALSE THEN id ELSE id END FROM t ORDER BY id"), "ambiguous, as in Postgres")
	})

	t.Run("uses composite primary keys for ranges", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE c (a string, b int, v int, PRIMARY KEY (a, b))")
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
2 "BOB" 1.0
3 "CAROL" -1.5`, query(t, s, "SELECT id, name, f FROM t"))
		res = mustExec(t, s, "UPDATE t SET f = NULL WHERE id = 99")
		is.Equal(t, int64(0), res.RowsAffected)
		mustExec(t, s, "UPDATE t SET f = $1, b = $2 WHERE id = $3", 2.0, nil, 1)
		is.Equal(t, "2.0 NULL", query(t, s, "SELECT f, b FROM t WHERE id = 1"))
	})

	t.Run("evaluates every assignment against the old row", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE p (id int PRIMARY KEY, a int, b int)")
		mustExec(t, s, "INSERT INTO p VALUES (1, 1, 2)")
		mustExec(t, s, "UPDATE p SET a = b, b = a")
		is.Equal(t, "1 2 1", query(t, s, "SELECT * FROM p"))
	})

	t.Run("moves rows when the primary key changes and does not revisit them", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE h (id int PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO h VALUES (1), (2), (3)")
		res := mustExec(t, s, "UPDATE h SET id = id + 10 WHERE id < 100")
		is.Equal(t, int64(3), res.RowsAffected)
		is.Equal(t, "11\n12\n13", query(t, s, "SELECT id FROM h"))
		mustExec(t, s, "UPDATE h SET id = id - 1")
		is.Equal(t, "10\n11\n12", query(t, s, "SELECT id FROM h"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE h SET id = 11 WHERE id = 10"))
		is.Equal(t, "10\n11\n12", query(t, s, "SELECT id FROM h"), "a failed update changes nothing")
	})

	t.Run("checks constraints and types", func(t *testing.T) {
		s := newSession(t)
		setup(t, s)
		is.Error(t, exec.ErrNotNull, queryErr(t, s, "UPDATE t SET name = NULL WHERE id = 1"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET name = 1"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET f = id"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "UPDATE t SET nope = 1"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "UPDATE t SET f = 1, f = 2"))
		is.Error(t, exec.ErrType, queryErr(t, s, "UPDATE t SET f = 1 WHERE name"))
		mustExec(t, s, "UPDATE t SET f = 2")
		is.Equal(t, "2.0\n2.0\n2.0", query(t, s, "SELECT f FROM t"), "an int literal adapts to the float column")
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
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY, n int)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, s, "UPDATE t SET n = n + 1")
		is.Equal(t, "1 2", query(t, s, "SELECT * FROM t"))
		mustExec(t, s, "ROLLBACK")
		is.True(t, !s.InTx())
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "SELECT * FROM t"))
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY, n int)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, s, "COMMIT")
		is.Equal(t, "1 1", query(t, s, "SELECT * FROM t"))
	})

	t.Run("a failed statement in a transaction leaves earlier statements intact", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY)")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (1)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (2), (1)"))
		mustExec(t, s, "COMMIT")
		is.Equal(t, "1", query(t, s, "SELECT id FROM t"))
	})

	t.Run("rejects nested BEGIN and COMMIT or ROLLBACK without a transaction", func(t *testing.T) {
		s := newSession(t)
		is.Error(t, exec.ErrNoTx, queryErr(t, s, "COMMIT"))
		is.Error(t, exec.ErrNoTx, queryErr(t, s, "ROLLBACK"))
		mustExec(t, s, "BEGIN")
		is.Error(t, exec.ErrTxInProgress, queryErr(t, s, "BEGIN"))
		mustExec(t, s, "COMMIT")
	})

	t.Run("read-only transactions reject writes and DDL", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY)")
		mustExec(t, s, "BEGIN READ ONLY")
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "INSERT INTO t VALUES (1)"))
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "UPDATE t SET id = 2"))
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "DELETE FROM t"))
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "CREATE TABLE u (id int PRIMARY KEY)"))
		is.Error(t, exec.ErrReadOnlyTx, queryErr(t, s, "DROP TABLE t"))
		is.Equal(t, "", query(t, s, "SELECT * FROM t"))
		mustExec(t, s, "COMMIT")
	})

	t.Run("a transaction sees a snapshot, and an explicit transaction gets ErrConflict while autocommit retries", func(t *testing.T) {
		w := newWorld(t)
		a, _ := w.session(exec.Options{})
		b, _ := w.session(exec.Options{})
		mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
		w := newWorld(t)
		a, _ := w.session(exec.Options{})
		b, _ := w.session(exec.Options{})
		mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
		w := newWorld(t)
		s, _ := w.session(exec.Options{Mutations: exec.Mutations{UpdateSeesOwnWrites: true}})
		mustExec(t, s, "CREATE TABLE h (id int PRIMARY KEY)")
		mustExec(t, s, "INSERT INTO h VALUES (1), (2), (3)")
		mustExec(t, s, "UPDATE h SET id = id + 10 WHERE id < 100")
		is.Equal(t, "101\n102\n103", query(t, s, "SELECT id FROM h"), "the mutant moves every row until it leaves the range")
	})

	t.Run("FreshSnapshotPerStatement lets a transaction see a commit made after it began", func(t *testing.T) {
		for _, mutated := range []bool{false, true} {
			w := newWorld(t)
			a, _ := w.session(exec.Options{Mutations: exec.Mutations{FreshSnapshotPerStatement: mutated}})
			b, _ := w.session(exec.Options{})
			mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
		for _, mutated := range []bool{false, true} {
			w := newWorld(t)
			s, _ := w.session(exec.Options{Mutations: exec.Mutations{RetryAfterUnknownCommit: mutated}})
			mustExec(t, s, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
