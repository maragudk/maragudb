package pgdiff_test

import (
	"database/sql"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/verify"
)

// Each test here is one class of statements in /docs/unsupported.md. It runs examples of the
// class on both databases and asserts that Postgres accepts them and maragudb rejects them with
// [maragudb.ErrUnsupported], so that the document cannot go stale: when either side changes, the
// test fails.

// both is a maragudb and a Postgres database with the same table `dev`.
type both struct {
	m, pg *sql.DB
}

func newBoth(t *testing.T, create string) both {
	t.Helper()
	r := newRunner(t)
	m, err := r.New()
	is.NotError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	b := both{m: m, pg: r.PG}
	for _, q := range []string{"DROP TABLE IF EXISTS dev", "DROP TABLE IF EXISTS dev2"} {
		_, err = b.pg.ExecContext(t.Context(), q)
		is.NotError(t, err)
	}
	if create != "" {
		b.exec(t, create)
	}
	return b
}

// exec a statement on both databases, which must succeed on both.
func (b both) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	for _, db := range []*sql.DB{b.m, b.pg} {
		_, err := db.ExecContext(t.Context(), query, args...)
		is.NotError(t, err, query)
	}
}

// example of a class of unsupported statements: Setup runs on both databases, and then SQL
// with Args must succeed on Postgres and fail with ErrUnsupported on maragudb. With InTx, both
// run in an explicit transaction.
type example struct {
	SQL   string
	Args  []any
	InTx  bool
	Setup []string
}

// rejects asserts every example.
func rejects(t *testing.T, create string, examples ...example) {
	t.Helper()
	for _, e := range examples {
		t.Run(e.SQL, func(t *testing.T) {
			b := newBoth(t, create)
			for _, q := range e.Setup {
				b.exec(t, q)
			}
			var errs []error
			for _, db := range []*sql.DB{b.m, b.pg} {
				conn, err := db.Conn(t.Context())
				is.NotError(t, err)
				if e.InTx {
					_, err = conn.ExecContext(t.Context(), "BEGIN")
					is.NotError(t, err)
				}
				rows, err := conn.QueryContext(t.Context(), e.SQL, e.Args...)
				if err == nil {
					for rows.Next() {
					}
					err = rows.Err()
					_ = rows.Close()
				}
				errs = append(errs, err)
				if e.InTx {
					_, _ = conn.ExecContext(t.Context(), "ROLLBACK")
				}
				_ = conn.Close()
			}
			is.Error(t, maragudb.ErrUnsupported, errs[0], fmt.Sprintf("maragudb rejects %s as unsupported", e.SQL))
			is.NotError(t, errs[1], fmt.Sprintf("postgres accepts %s", e.SQL))
		})
	}
}

func TestUnsupported(t *testing.T) {
	const create = "CREATE TABLE dev (i bigint PRIMARY KEY, j bigint, s text)"
	rows := "INSERT INTO dev VALUES (2, 5, 'b'), (1, 0, 'a')"

	t.Run("UNS-NUMERIC numeric literals outside double precision and bigint contexts", func(t *testing.T) {
		verify.Covers(t, "numeric literals that Postgres keeps as numeric, such as SELECT 1.5 or a literal beyond the bigint range, are rejected, and Postgres accepts them", "UNS-NUMERIC")
		rejects(t, create,
			example{SQL: "SELECT 1.5"},
			example{SQL: "SELECT 1 + 1.5"},
			example{SQL: "SELECT 9223372036854775808"},
			example{SQL: "SELECT i FROM dev WHERE i = 1.5"},
			example{SQL: "SELECT 1.5 + 1.5"},
		)
	})

	t.Run("UNS-TYPES other types", func(t *testing.T) {
		verify.Covers(t, "type names other than the six supported types and their aliases, and type modifiers, are rejected, and Postgres accepts them", "UNS-TYPES")
		rejects(t, create,
			example{SQL: "SELECT 1::integer"},
			example{SQL: "CREATE TABLE dev2 (a integer PRIMARY KEY)"},
			example{SQL: "CREATE TABLE dev2 (a numeric PRIMARY KEY)"},
			example{SQL: "SELECT 'x'::varchar(3)"},
			example{SQL: "SELECT '2024-01-01'::date"},
			example{SQL: "SELECT '2024-01-01'::timestamp"},
			example{SQL: "SELECT '2024-01-01'::timestamptz(3)"},
			example{SQL: "SELECT '{}'::jsonb"},
		)
	})

	t.Run("UNS-TIMESTAMP-INPUT timestamps that are not ISO 8601 or not in the common era", func(t *testing.T) {
		verify.Covers(t, "timestamptz text that is not in an ISO 8601 form, special values, named time zones, and instants before year 1 are rejected, and Postgres accepts them", "UNS-TIMESTAMP-INPUT")
		rejects(t, create,
			example{SQL: "SELECT 'January 2, 2024'::timestamptz"},
			example{SQL: "SELECT 'now'::timestamptz"},
			example{SQL: "SELECT 'infinity'::timestamptz"},
			example{SQL: "SELECT '2024-01-02 03:04:05 Europe/Oslo'::timestamptz"},
			example{SQL: "SELECT '0001-01-01 00:00:00+01'::timestamptz"},
		)
	})

	t.Run("UNS-NO-PRIMARY-KEY tables without a primary key", func(t *testing.T) {
		verify.Covers(t, "a table without a primary key is rejected, and Postgres accepts it", "UNS-NO-PRIMARY-KEY")
		rejects(t, create, example{SQL: "CREATE TABLE dev2 (a bigint)"})
	})

	t.Run("UNS-DDL other schema statements and constraints", func(t *testing.T) {
		verify.Covers(t, "schema statements other than CREATE TABLE and DROP TABLE, and constraints other than PRIMARY KEY and NOT NULL, are rejected, and Postgres accepts them", "UNS-DDL")
		rejects(t, create,
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint UNIQUE)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint DEFAULT 1)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY CHECK (a > 0))"},
			example{SQL: "CREATE TEMP TABLE dev2 (a bigint PRIMARY KEY)"},
			example{SQL: "CREATE INDEX ON dev (j)"},
			example{SQL: "ALTER TABLE dev ADD COLUMN k bigint"},
			example{SQL: "DROP TABLE IF EXISTS dev2, dev3"},
		)
	})

	t.Run("UNS-STATEMENTS other statements and transaction modes", func(t *testing.T) {
		verify.Covers(t, "statements other than CREATE TABLE, DROP TABLE, INSERT, UPDATE, DELETE, SELECT, and the transaction statements, transaction modes other than SERIALIZABLE, and savepoints are rejected, and Postgres accepts them", "UNS-STATEMENTS")
		rejects(t, create,
			example{SQL: "WITH x AS (SELECT 1) SELECT * FROM x"},
			example{SQL: "EXPLAIN SELECT 1"},
			example{SQL: "SET TIME ZONE 'UTC'"},
			example{SQL: "TRUNCATE dev"},
			example{SQL: "VALUES (1)"},
			example{SQL: "BEGIN ISOLATION LEVEL READ COMMITTED"},
			example{SQL: "SAVEPOINT s", InTx: true},
		)
	})

	t.Run("UNS-CLAUSES other clauses of queries and writes", func(t *testing.T) {
		verify.Covers(t, "DISTINCT, GROUP BY, joins, subqueries, set operations, aliases, qualified names, FOR UPDATE, RETURNING, ON CONFLICT, INSERT ... SELECT, and DEFAULT are rejected, and Postgres accepts them", "UNS-CLAUSES")
		rejects(t, create,
			example{SQL: "SELECT DISTINCT j FROM dev"},
			example{SQL: "SELECT j FROM dev GROUP BY j"},
			example{SQL: "SELECT d.i FROM dev d"},
			example{SQL: "SELECT i FROM dev UNION SELECT 1"},
			example{SQL: "SELECT i FROM dev WHERE i IN (SELECT 1)"},
			example{SQL: "SELECT i FROM dev FOR UPDATE"},
			example{SQL: "INSERT INTO dev VALUES (5, 1, 'x') RETURNING i"},
			example{SQL: "INSERT INTO dev SELECT 6, 1, 'x'"},
			example{SQL: "INSERT INTO dev VALUES (7, 1, 'x') ON CONFLICT DO NOTHING"},
			example{SQL: "INSERT INTO dev VALUES (8, DEFAULT, 'x')"},
		)
	})

	t.Run("UNS-FUNCTIONS functions other than the supported five", func(t *testing.T) {
		verify.Covers(t, "functions other than abs, coalesce, length, lower, and upper, including aggregates and window functions, are rejected, and Postgres accepts them", "UNS-FUNCTIONS")
		rejects(t, create,
			example{SQL: "SELECT count(*) FROM dev"},
			example{SQL: "SELECT now()"},
			example{SQL: "SELECT nullif(1, 2)"},
			example{SQL: "SELECT int8pl(1, 2)"},
			example{SQL: "SELECT count(*) OVER () FROM dev"},
		)
	})

	t.Run("UNS-OPERATORS other operators and predicates", func(t *testing.T) {
		verify.Covers(t, "operators and predicates other than the supported ones, such as IS DISTINCT FROM, ILIKE, regular expressions, arrays, row constructors, LIKE ... ESCAPE, LIKE on bytea, COLLATE, and AT TIME ZONE, are rejected, and Postgres accepts them", "UNS-OPERATORS")
		rejects(t, create,
			example{SQL: "SELECT 1 IS DISTINCT FROM 2"},
			example{SQL: "SELECT TRUE IS TRUE"},
			example{SQL: "SELECT 'a' ILIKE 'A'"},
			example{SQL: "SELECT 'a' ~ 'a'"},
			example{SQL: "SELECT 2 ^ 3"},
			example{SQL: "SELECT 5 & 3"},
			example{SQL: "SELECT ARRAY[1]"},
			example{SQL: "SELECT (1, 2)"},
			example{SQL: "SELECT 'a' LIKE 'a' ESCAPE '#'"},
			example{SQL: "SELECT '\\x01'::bytea LIKE '\\x01'"},
			example{SQL: "SELECT 2 BETWEEN SYMMETRIC 3 AND 1"},
			example{SQL: `SELECT 'a' COLLATE "C"`},
			example{SQL: "SELECT '2024-01-01'::timestamptz AT TIME ZONE 'UTC'"},
			example{SQL: "SELECT CAST('\\x0102'::bytea AS bigint)"},
		)
	})

	t.Run("UNS-LITERALS other forms of constants and identifiers", func(t *testing.T) {
		verify.Covers(t, "escape, bit, national, Unicode, and dollar-quoted string constants, strings continued across lines, and non-ASCII unquoted identifiers are rejected, and Postgres accepts them", "UNS-LITERALS")
		rejects(t, create,
			example{SQL: `SELECT E'a\nb'`},
			example{SQL: "SELECT B'101'"},
			example{SQL: "SELECT X'1F'"},
			example{SQL: "SELECT N'a'"},
			example{SQL: "SELECT U&'a'"},
			example{SQL: "SELECT $$a$$"},
			example{SQL: "SELECT 'a'\n'b'"},
			example{SQL: "SELECT 1 AS ä"},
		)
	})

	t.Run("UNS-PARAMETERS parameters whose Go value needs a conversion", func(t *testing.T) {
		verify.Covers(t, "a parameter whose Go value would have to be converted to the type its context infers, such as a float64 used as a bigint, is rejected, and Postgres with pgx accepts it", "UNS-PARAMETERS")
		rejects(t, create,
			example{SQL: "SELECT i FROM dev WHERE i = $1", Args: []any{1.0}},
			example{SQL: "SELECT $1::bigint + j FROM dev", Args: []any{2.0}},
		)
	})

	t.Run("UNS-LIMIT-ORDER LIMIT and OFFSET without a total order", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET without ORDER BY on every primary key column are rejected, because which rows Postgres returns depends on its plan, and Postgres accepts them", "UNS-LIMIT-ORDER")
		rejects(t, create,
			example{SQL: "SELECT i FROM dev LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT i FROM dev ORDER BY j OFFSET 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-LIMIT LIMIT and OFFSET with an expression that can fail", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET with an output, ORDER BY, or WHERE expression that can fail are rejected, because how many rows Postgres evaluates it on depends on its plan, and Postgres accepts them when no row fails", "UNS-PLAN-DEPENDENT-LIMIT")
		rejects(t, create,
			example{SQL: "SELECT i + j FROM dev ORDER BY i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT i FROM dev WHERE j + 1 > 0 ORDER BY i LIMIT 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-WHERE a condition that can fail next to a primary key condition", func(t *testing.T) {
		verify.Covers(t, "a WHERE condition that can fail next to a condition an index scan on the primary key could use, or a LIKE on a key column that can fail, is rejected, and Postgres accepts it when no row fails", "UNS-PLAN-DEPENDENT-WHERE")
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, j bigint, s text)",
			example{SQL: "SELECT i FROM dev WHERE i = 1 AND j + 1 > 0", Setup: []string{rows}},
			example{SQL: "UPDATE dev SET s = 'x' WHERE i > 1 AND 10 / j > 0", Setup: []string{rows}},
		)
	})

	t.Run("UNS-ORDER-DEPENDENT-WHERE a condition that fails on a row that another condition rejects", func(t *testing.T) {
		verify.Covers(t, "a WHERE clause with a condition that fails on a row that another condition rejects is rejected, because whether Postgres fails depends on the order it picks, and Postgres accepts it in the order it picks here", "UNS-ORDER-DEPENDENT-WHERE")
		rejects(t, create,
			example{SQL: "SELECT i FROM dev WHERE j <> 0 AND 10 / j > 1", Setup: []string{rows}},
			example{SQL: "DELETE FROM dev WHERE j <> 0 AND 10 / j > 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-STABLE-CONSTANT a conversion between text and timestamptz that reads no columns and can fail", func(t *testing.T) {
		verify.Covers(t, "a conversion between text and timestamptz without columns that can fail is rejected, because whether Postgres evaluates it while planning depends on the plan, and Postgres accepts it when it does not fail", "UNS-STABLE-CONSTANT")
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, ts timestamptz)",
			example{SQL: "SELECT CAST(CAST('2024-01-02' AS text) AS timestamptz)"},
			example{SQL: "SELECT i FROM dev WHERE ts = CAST(lower('2024-01-02') AS timestamptz)"},
			example{SQL: "SELECT CAST(CAST('2024-01-02'::timestamptz AS text) || 'x' AS bytea)"},
		)
	})

	t.Run("UNS-KEY-UPDATE-ORDER an UPDATE that moves a key onto one it moves away", func(t *testing.T) {
		verify.Covers(t, "an UPDATE that moves a row onto the primary key of a row that the same statement moves away is rejected, because Postgres checks the key row by row in storage order, and Postgres accepts it when it stores the rows in the right order", "UNS-KEY-UPDATE-ORDER")
		rejects(t, create,
			example{SQL: "UPDATE dev SET i = i + 1", Setup: []string{rows}},
		)
	})
}
