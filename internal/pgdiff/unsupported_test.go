package pgdiff_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
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
	for _, q := range []string{"DROP VIEW IF EXISTS dev_view", "DROP TABLE IF EXISTS dev2 CASCADE", "DROP TABLE IF EXISTS dev CASCADE", "DROP SEQUENCE IF EXISTS dev_seq"} {
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

	t.Run("UNS-SEQUENCES sequences, serial types, and identity columns", func(t *testing.T) {
		verify.Covers(t, "sequences, the serial types, and identity columns are rejected with a hint, and Postgres accepts them", "UNS-SEQUENCES")
		rejects(t, create,
			example{SQL: "CREATE TABLE dev2 (a serial PRIMARY KEY)"},
			example{SQL: "CREATE TABLE dev2 (a bigserial PRIMARY KEY)"},
			example{SQL: "CREATE TABLE dev2 (a bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY)"},
			example{SQL: "CREATE SEQUENCE dev_seq"},
		)
	})

	t.Run("UNS-DEFAULT a DEFAULT that can fail", func(t *testing.T) {
		verify.Covers(t, "a DEFAULT whose constant part fails, or that can fail when it is evaluated, is rejected when the table is created, and Postgres accepts the table", "UNS-DEFAULT")
		rejects(t, create,
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint DEFAULT 1 / 0)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint DEFAULT 2147483647 + 1)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b timestamptz DEFAULT CAST(CAST(now() AS text) AS timestamptz))"},
		)
	})

	t.Run("UNS-CHECK other forms of CHECK", func(t *testing.T) {
		verify.Covers(t, "a CHECK with NO INHERIT, NOT VALID, or NOT ENFORCED, or whose constant part fails, is rejected, and Postgres accepts the table", "UNS-CHECK")
		rejects(t, create,
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY CHECK (a > 0) NO INHERIT)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, CHECK (a > 0) NOT VALID)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY CHECK (a > 0) NOT ENFORCED)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY CHECK (1 / 0 > a))"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, CHECK (a > 0 OR 2147483647 + 1 > 0))"},
		)
	})

	t.Run("UNS-FOREIGN-KEY other forms of FOREIGN KEY", func(t *testing.T) {
		verify.Covers(t, "a foreign key with MATCH FULL, ON UPDATE CASCADE, SET NULL, or SET DEFAULT, NOT VALID, NOT ENFORCED, or DEFERRABLE, or on double precision columns, is rejected, and Postgres accepts the table", "UNS-FOREIGN-KEY")
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, f double precision UNIQUE)",
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev MATCH FULL)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev ON UPDATE CASCADE)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev ON UPDATE SET NULL)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev ON UPDATE SET DEFAULT)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint, FOREIGN KEY (b) REFERENCES dev NOT VALID)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev NOT ENFORCED)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev DEFERRABLE)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b double precision REFERENCES dev (f))"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint REFERENCES dev (f))"},
		)
	})

	t.Run("UNS-FOREIGN-KEY-ORDER a DELETE whose foreign key actions depend on their order", func(t *testing.T) {
		verify.Covers(t, "a DELETE whose foreign key actions could write columns that a check reads, or write a table through more than one foreign key, is rejected, and Postgres accepts it when it fires the actions in the right order", "UNS-FOREIGN-KEY-ORDER")
		rejects(t, create,
			example{SQL: "DELETE FROM dev WHERE i = 1", Setup: []string{rows, "CREATE TABLE dev2 (a bigint PRIMARY KEY, y bigint REFERENCES dev ON DELETE CASCADE, x bigint REFERENCES dev)", "INSERT INTO dev2 VALUES (1, 1, 1)"}},
			example{SQL: "DELETE FROM dev WHERE i = 1", Setup: []string{rows, "CREATE TABLE dev2 (a bigint PRIMARY KEY, y bigint REFERENCES dev ON DELETE CASCADE, x bigint REFERENCES dev ON DELETE SET NULL)", "INSERT INTO dev2 VALUES (1, 1, 1)"}},
			example{SQL: "DELETE FROM dev", Setup: []string{rows, "CREATE TABLE dev2 (a bigint PRIMARY KEY, x bigint REFERENCES dev ON DELETE SET NULL, y bigint REFERENCES dev ON DELETE CASCADE, CHECK (x IS NOT NULL))", "INSERT INTO dev2 VALUES (1, 1, 2)"}},
		)
	})

	t.Run("UNS-NO-PRIMARY-KEY tables without a primary key", func(t *testing.T) {
		verify.Covers(t, "a table without a primary key is rejected, and Postgres accepts it", "UNS-NO-PRIMARY-KEY")
		rejects(t, create, example{SQL: "CREATE TABLE dev2 (a bigint)"})
	})

	t.Run("UNS-DDL other schema statements and constraints", func(t *testing.T) {
		verify.Covers(t, "schema statements other than CREATE TABLE, DROP TABLE, CREATE INDEX, and DROP INDEX, constraints other than PRIMARY KEY, UNIQUE, NOT NULL, DEFAULT, CHECK, and FOREIGN KEY, and a named NOT NULL are rejected, and Postgres accepts them", "UNS-DDL")
		rejects(t, create,
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint, EXCLUDE USING btree (b WITH =))"},
			example{SQL: "CREATE TABLE dev2 (a bigint CONSTRAINT dev2_a_nn NOT NULL PRIMARY KEY)"},
			example{SQL: "CREATE TEMP TABLE dev2 (a bigint PRIMARY KEY)"},
			example{SQL: "CREATE VIEW dev_view AS SELECT 1"},
			example{SQL: "ALTER TABLE dev ADD COLUMN k bigint"},
			example{SQL: "DROP TABLE IF EXISTS dev2, dev3"},
			example{SQL: "DROP TABLE IF EXISTS dev2 CASCADE"},
		)
	})

	t.Run("UNS-INDEX other forms of indexes and of their constraints", func(t *testing.T) {
		verify.Covers(t, "indexes on expressions, partial indexes, INCLUDE, USING, CONCURRENTLY, ON ONLY, operator classes, COLLATE, storage parameters, tablespaces, ALTER INDEX, several indexes in one DROP INDEX, and DEFERRABLE and NOT DEFERRABLE constraints are rejected, and Postgres accepts them", "UNS-INDEX")
		rejects(t, create,
			example{SQL: "CREATE INDEX ON dev ((j + 1))"},
			example{SQL: "CREATE INDEX ON dev (lower(s))"},
			example{SQL: "CREATE INDEX ON dev (j) WHERE j > 0"},
			example{SQL: "CREATE UNIQUE INDEX ON dev (j) INCLUDE (s)"},
			example{SQL: "CREATE INDEX ON dev USING btree (j)"},
			example{SQL: "CREATE INDEX ON dev USING hash (j)"},
			example{SQL: "CREATE INDEX CONCURRENTLY ON dev (j)"},
			example{SQL: "CREATE INDEX ON ONLY dev (j)"},
			example{SQL: "CREATE INDEX ON dev (s text_pattern_ops)"},
			example{SQL: `CREATE INDEX ON dev (s COLLATE "C")`},
			example{SQL: "CREATE INDEX ON dev (j) WITH (fillfactor = 70)"},
			example{SQL: "CREATE INDEX ON dev (j) TABLESPACE pg_default"},
			example{SQL: "ALTER INDEX dev_pkey RENAME TO dev_key"},
			example{SQL: "DROP INDEX CONCURRENTLY IF EXISTS dev2"},
			example{SQL: "DROP INDEX IF EXISTS dev2, dev3"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint UNIQUE DEFERRABLE)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY NOT DEFERRABLE)"},
			example{SQL: "CREATE TABLE dev2 (a bigint, b bigint, PRIMARY KEY (a) DEFERRABLE INITIALLY DEFERRED)"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint, UNIQUE (b) INCLUDE (a))"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint UNIQUE WITH (fillfactor = 70))"},
			example{SQL: "CREATE TABLE dev2 (a bigint PRIMARY KEY USING INDEX TABLESPACE pg_default)"},
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
		verify.Covers(t, "DISTINCT ON, GROUP BY, subqueries, set operations, FOR UPDATE, RETURNING, ON CONFLICT, INSERT ... SELECT, an alias in INSERT, UPDATE ... FROM, and DELETE ... USING are rejected, and Postgres accepts them", "UNS-CLAUSES")
		rejects(t, create,
			example{SQL: "SELECT DISTINCT ON (j) j FROM dev"},
			example{SQL: "SELECT j FROM dev GROUP BY j"},
			example{SQL: "SELECT i FROM dev UNION SELECT 1"},
			example{SQL: "SELECT i FROM dev WHERE i IN (SELECT 1)"},
			example{SQL: "SELECT i FROM dev FOR UPDATE"},
			example{SQL: "INSERT INTO dev VALUES (5, 1, 'x') RETURNING i"},
			example{SQL: "INSERT INTO dev SELECT 6, 1, 'x'"},
			example{SQL: "INSERT INTO dev VALUES (7, 1, 'x') ON CONFLICT DO NOTHING"},
			example{SQL: "INSERT INTO dev AS d VALUES (8, 1, 'x')"},
			example{SQL: "UPDATE dev SET j = d.j FROM dev AS d WHERE d.i = dev.i"},
			example{SQL: "DELETE FROM dev USING dev AS d WHERE d.i = dev.i"},
		)
	})

	t.Run("UNS-JOINS other forms of FROM and of names", func(t *testing.T) {
		verify.Covers(t, "RIGHT and FULL joins, LATERAL, an alias for a join or for the columns of USING, subqueries and functions in FROM, names with a schema, a reference to a whole row, and a repeated name in the names of a table's columns are rejected, and Postgres accepts them", "UNS-JOINS")
		rejects(t, create,
			example{SQL: "SELECT * FROM dev a RIGHT JOIN dev b ON a.i = b.j"},
			example{SQL: "SELECT * FROM dev a FULL JOIN dev b ON a.i = b.j"},
			example{SQL: "SELECT * FROM dev a JOIN LATERAL (SELECT a.i) b ON true"},
			example{SQL: "SELECT * FROM (dev a JOIN dev b ON a.i = b.j) AS c"},
			example{SQL: "SELECT * FROM dev a JOIN dev b USING (i) AS c"},
			example{SQL: "SELECT * FROM (SELECT 1) AS a"},
			example{SQL: "SELECT * FROM generate_series(1, 2)"},
			example{SQL: "SELECT i FROM public.dev"},
			example{SQL: "SELECT public.dev.i FROM dev"},
			example{SQL: "SELECT dev FROM dev"},
			example{SQL: "SELECT dev.* IS NULL FROM dev"},
			example{SQL: "SELECT * FROM dev AS d (a, a)"},
		)
	})

	t.Run("UNS-FUNCTIONS functions other than the supported ones", func(t *testing.T) {
		verify.Covers(t, "functions other than abs, coalesce, length, lower, upper, now, transaction_timestamp, and CURRENT_TIMESTAMP, including aggregates, window functions, the other clocks, and CURRENT_TIMESTAMP with a precision, are rejected, and Postgres accepts them", "UNS-FUNCTIONS")
		rejects(t, create,
			example{SQL: "SELECT count(*) FROM dev"},
			example{SQL: "SELECT statement_timestamp()"},
			example{SQL: "SELECT clock_timestamp()"},
			example{SQL: "SELECT CURRENT_TIMESTAMP(3)"},
			example{SQL: "SELECT LOCALTIMESTAMP"},
			example{SQL: "SELECT CURRENT_DATE"},
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
			example{SQL: "SELECT a.i FROM dev a, dev b ORDER BY a.i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT DISTINCT j, s FROM dev ORDER BY j LIMIT 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-JOIN a condition of a join that fails on a row that some plan evaluates it on", func(t *testing.T) {
		verify.Covers(t, "a SELECT on several tables with a condition that fails on a row or a combination of rows that a plan of Postgres could evaluate it on is rejected, because whether Postgres fails depends on its join plan, and Postgres accepts it when its plan does not evaluate the condition there", "UNS-PLAN-DEPENDENT-JOIN")
		rejects(t, create,
			example{SQL: "SELECT * FROM dev JOIN dev2 ON dev.j = dev2.j WHERE 10 / dev.j > 0", Setup: []string{rows, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)"}},
			example{SQL: "SELECT * FROM dev2 LEFT JOIN dev ON dev.j = dev2.j AND 10 / dev.j > 0", Setup: []string{rows, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)"}},
		)
	})

	t.Run("UNS-DISTINCT-ZERO DISTINCT of -0 and 0", func(t *testing.T) {
		verify.Covers(t, "a SELECT DISTINCT whose result has -0 and 0 in one group is rejected, because which of them Postgres outputs depends on its plan, and Postgres accepts it", "UNS-DISTINCT-ZERO")
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, f double precision)",
			example{SQL: "SELECT DISTINCT f FROM dev", Setup: []string{"INSERT INTO dev VALUES (1, '-0'), (2, 0)"}},
			example{SQL: "SELECT DISTINCT a.f FROM dev a, dev b", Setup: []string{"INSERT INTO dev VALUES (1, '-0'), (2, 0)"}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-LIMIT LIMIT and OFFSET with an expression that can fail", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET with an output, ORDER BY, or WHERE expression that can fail are rejected, because how many rows Postgres evaluates it on depends on its plan, and Postgres accepts them when no row fails", "UNS-PLAN-DEPENDENT-LIMIT")
		rejects(t, create,
			example{SQL: "SELECT i + j FROM dev ORDER BY i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT i FROM dev WHERE j + 1 > 0 ORDER BY i LIMIT 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-WHERE a LIKE on an indexed column whose pattern can fail", func(t *testing.T) {
		verify.Covers(t, "a LIKE on a column of the primary key or of another index whose pattern can fail is rejected, because Postgres scans the index for its prefix and evaluates it only on those rows, and Postgres accepts it when no row there fails", "UNS-PLAN-DEPENDENT-WHERE")
		rejects(t, "CREATE TABLE dev (s text PRIMARY KEY, j bigint)",
			example{SQL: "SELECT j FROM dev WHERE s LIKE 'b\\'", Setup: []string{"INSERT INTO dev VALUES ('a', 0), ('b', 5)"}},
			example{SQL: "UPDATE dev SET j = 1 WHERE s LIKE 'b\\' AND j > 0", Setup: []string{"INSERT INTO dev VALUES ('a', 0), ('b', 5)"}},
		)
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, s text UNIQUE, j bigint)",
			example{SQL: "SELECT j FROM dev WHERE s LIKE 'b\\'", Setup: []string{"INSERT INTO dev VALUES (1, 'a', 0), (2, 'b', 5)"}},
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
		verify.Covers(t, "an UPDATE that moves a row onto the primary key, or the values of a unique index, of a row that the same statement moves away is rejected, because Postgres checks the key row by row in the order it visits the rows, and Postgres accepts it when it stores the rows in the right order", "UNS-KEY-UPDATE-ORDER")
		uniqueRows := []string{"CREATE UNIQUE INDEX dev_j ON dev (j)", "INSERT INTO dev VALUES (2, 2, 'b'), (1, 1, 'a')"}
		rejects(t, create,
			example{SQL: "UPDATE dev SET i = i + 1", Setup: []string{rows}},
			example{SQL: "UPDATE dev SET j = j + 1", Setup: uniqueRows},
			example{SQL: "UPDATE dev SET j = j + 1 WHERE j > 0", Setup: uniqueRows},
		)
	})
}

// TestDifferences has one test per remaining difference from Postgres in /docs/unsupported.md.
// Each runs the example of the difference on both databases.
func TestDifferences(t *testing.T) {
	t.Run("DEV-20 Postgres with sequential scans can fail on a row that the primary key does not select", func(t *testing.T) {
		verify.Covers(t, "on a small table with statistics, Postgres in its default configuration scans the whole table and fails on a row that the primary key condition does not select, maragudb evaluates the condition only on the selected rows and succeeds, and Postgres with enable_seqscan=off gives the same result as maragudb", "DEV-20")
		b := newBoth(t, "CREATE TABLE dev (id bigint PRIMARY KEY, b bigint)")
		b.exec(t, "INSERT INTO dev VALUES (1, 100), (2, 50), (9, -9223372036854775808)")
		const update = "UPDATE dev SET b = b - 10 WHERE id IN (1, 2, 3, 4, 5) AND b - 10 >= 0"

		res, err := b.m.ExecContext(t.Context(), update)
		is.NotError(t, err, "maragudb evaluates b - 10 only on the rows 1 and 2")
		n, err := res.RowsAffected()
		is.NotError(t, err)
		is.Equal(t, int64(2), n)

		conn, err := b.pg.Conn(t.Context())
		is.NotError(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		for _, q := range []string{"SET enable_seqscan = on", "ANALYZE dev"} {
			_, err := conn.ExecContext(t.Context(), q)
			is.NotError(t, err, q)
		}
		_, err = conn.ExecContext(t.Context(), update)
		is.Equal(t, "22003", pgdiff.SQLState(err), "Postgres scans the whole table and evaluates b - 10 first, also on row 9")

		_, err = conn.ExecContext(t.Context(), "SET enable_seqscan = off")
		is.NotError(t, err)
		res, err = conn.ExecContext(t.Context(), update)
		is.NotError(t, err, "with sequential scans off, Postgres scans the primary key index")
		n, err = res.RowsAffected()
		is.NotError(t, err)
		is.Equal(t, int64(2), n)

		var states []string
		for _, q := range []querier{b.m, conn} {
			rows, err := q.QueryContext(t.Context(), "SELECT id, b FROM dev ORDER BY id")
			is.NotError(t, err)
			var state []string
			for rows.Next() {
				var id, balance int64
				is.NotError(t, rows.Scan(&id, &balance))
				state = append(state, fmt.Sprint(id, "=", balance))
			}
			is.NotError(t, rows.Err())
			_ = rows.Close()
			states = append(states, strings.Join(state, " "))
		}
		is.Equal(t, "1=90 2=40 9=-9223372036854775808", states[0])
		is.Equal(t, states[0], states[1], "both databases end in the same state")
	})
}

// querier is a database or a connection.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
