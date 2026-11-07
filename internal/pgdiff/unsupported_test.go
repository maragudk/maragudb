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

	t.Run("UNS-NUMERIC numeric literals outside double precision and bigint contexts, and functions of numeric", func(t *testing.T) {
		verify.Covers(t, "numeric literals that Postgres keeps as numeric, such as SELECT 1.5 or a literal beyond the bigint range, also next to the numeric results of SUM and AVG and of arithmetic on them, functions of such results, and quoted literals read as numeric are rejected, and Postgres accepts them", "UNS-NUMERIC")
		rejects(t, create,
			example{SQL: "SELECT 1.5"},
			example{SQL: "SELECT 1 + 1.5"},
			example{SQL: "SELECT 9223372036854775808"},
			example{SQL: "SELECT i FROM dev WHERE i = 1.5"},
			example{SQL: "SELECT 1.5 + 1.5"},
			example{SQL: "SELECT sum(j) * 1.5 FROM dev"},
			example{SQL: "SELECT -sum(j) + 9223372036854775808 FROM dev"},
			example{SQL: "SELECT abs(avg(j)) FROM dev"},
			example{SQL: "SELECT abs(sum(j) / 2) FROM dev"},
			example{SQL: "SELECT sum(j) = '5' FROM dev"},
			example{SQL: "SELECT avg(j) * 2 > '5' FROM dev"},
			example{SQL: "SELECT avg(j) > 1.5 FROM dev"},
			example{SQL: "SELECT sum(1.5) FROM dev"},
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
			example{SQL: "EXPLAIN SELECT 1"},
			example{SQL: "SET TIME ZONE 'UTC'"},
			example{SQL: "TRUNCATE dev"},
			example{SQL: "VALUES (1)"},
			example{SQL: "BEGIN ISOLATION LEVEL READ COMMITTED"},
			example{SQL: "SAVEPOINT s", InTx: true},
		)
	})

	t.Run("UNS-CLAUSES other clauses of queries and writes", func(t *testing.T) {
		verify.Covers(t, "DISTINCT ON, set operations, also in a subquery, FOR UPDATE, RETURNING, ON CONFLICT, INSERT ... SELECT, an alias in INSERT, UPDATE ... FROM, and DELETE ... USING are rejected, and Postgres accepts them", "UNS-CLAUSES")
		rejects(t, create,
			example{SQL: "SELECT DISTINCT ON (j) j FROM dev"},
			example{SQL: "SELECT i FROM dev UNION SELECT 1"},
			example{SQL: "SELECT i FROM dev WHERE i IN (SELECT 1 UNION SELECT 2)"},
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
		verify.Covers(t, "RIGHT and FULL joins, LATERAL, an alias for a join or for the columns of USING, VALUES and functions in FROM, names with a schema, a reference to a whole row, and a repeated name in the names of a table's columns are rejected, and Postgres accepts them", "UNS-JOINS")
		rejects(t, create,
			example{SQL: "SELECT * FROM dev a RIGHT JOIN dev b ON a.i = b.j"},
			example{SQL: "SELECT * FROM dev a FULL JOIN dev b ON a.i = b.j"},
			example{SQL: "SELECT * FROM dev a JOIN LATERAL (SELECT a.i) b ON true"},
			example{SQL: "SELECT * FROM (dev a JOIN dev b ON a.i = b.j) AS c"},
			example{SQL: "SELECT * FROM dev a JOIN dev b USING (i) AS c"},
			example{SQL: "SELECT * FROM (VALUES (1)) AS a"},
			example{SQL: "SELECT * FROM generate_series(1, 2)"},
			example{SQL: "SELECT i FROM public.dev"},
			example{SQL: "INSERT INTO public.dev VALUES (9, 1, 'x')"},
			example{SQL: "SELECT public.dev.i FROM dev"},
			example{SQL: "SELECT dev FROM dev"},
			example{SQL: "SELECT dev.* IS NULL FROM dev"},
			example{SQL: "SELECT * FROM dev AS d (a, a)"},
		)
	})

	t.Run("UNS-FUNCTIONS functions other than the supported ones", func(t *testing.T) {
		verify.Covers(t, "functions other than abs, coalesce, length, lower, nullif, upper, now, transaction_timestamp, CURRENT_TIMESTAMP, and the aggregates, including window functions, the other clocks, and CURRENT_TIMESTAMP with a precision, are rejected, and Postgres accepts them", "UNS-FUNCTIONS")
		rejects(t, create,
			example{SQL: "SELECT statement_timestamp()"},
			example{SQL: "SELECT clock_timestamp()"},
			example{SQL: "SELECT CURRENT_TIMESTAMP(3)"},
			example{SQL: "SELECT LOCALTIMESTAMP"},
			example{SQL: "SELECT CURRENT_DATE"},
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
			example{SQL: "SELECT i FROM dev WHERE i = $1 AND j + 0.5::float8 > $1", Args: []any{1.5}},
		)
	})

	t.Run("UNS-PARAMETERS a parameter that ORDER BY or GROUP BY may take for an output column, in contexts of different types", func(t *testing.T) {
		verify.Covers(t, "a parameter that is an output column and that ORDER BY names by the same parameter, or GROUP BY by the output column's alias, where Postgres may take it for the output column, is rejected when its contexts give it different types, and Postgres with pgx accepts it", "UNS-PARAMETERS")
		rejects(t, create,
			example{SQL: "SELECT $1::bigint + 0, $1 FROM dev ORDER BY $1", Args: []any{int64(1)}},
			example{SQL: "SELECT $1::bigint AS b, $1 AS a FROM dev GROUP BY a", Args: []any{int64(1)}},
		)
	})

	t.Run("UNS-PARAMETERS a statement that fails before its analysis reaches references of a parameter that Postgres analyzes first", func(t *testing.T) {
		verify.Covers(t, "a statement whose analysis fails where maragudb has not analyzed every reference of a repeated parameter that Postgres analyzes before that point, or where the failure has no position in Postgres's order, is rejected, because the types of those references could make Postgres fail first, and Postgres fails it too", "UNS-PARAMETERS")
		b := newBoth(t, create)
		query := "INSERT INTO dev (i, j) VALUES (TRUE, 1), ($1, $1 + 1)"
		_, err := b.m.ExecContext(t.Context(), query, int64(5))
		is.Error(t, maragudb.ErrUnsupported, err)
		_, err = b.pg.ExecContext(t.Context(), query, int64(5))
		is.Equal(t, "42804", pgdiff.SQLState(err), fmt.Sprint(err))
	})

	t.Run("UNS-AGGREGATES other aggregates and forms of grouping", func(t *testing.T) {
		verify.Covers(t, "FILTER, ORDER BY in an aggregate, WITHIN GROUP, GROUPING SETS, ROLLUP, CUBE, the empty grouping set, GROUP BY ALL and DISTINCT, and the aggregates other than COUNT, SUM, AVG, MIN, and MAX are rejected, and Postgres accepts them", "UNS-AGGREGATES")
		rejects(t, create,
			example{SQL: "SELECT count(*) FILTER (WHERE j > 0) FROM dev"},
			example{SQL: "SELECT sum(j ORDER BY i) FROM dev"},
			example{SQL: "SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY j) FROM dev"},
			example{SQL: "SELECT j, count(*) FROM dev GROUP BY GROUPING SETS ((j), ())"},
			example{SQL: "SELECT j, count(*) FROM dev GROUP BY ROLLUP (j)"},
			example{SQL: "SELECT j, count(*) FROM dev GROUP BY CUBE (j)"},
			example{SQL: "SELECT count(*) FROM dev GROUP BY ()"},
			example{SQL: "SELECT j FROM dev GROUP BY ALL j"},
			example{SQL: "SELECT j FROM dev GROUP BY DISTINCT j"},
			example{SQL: "SELECT string_agg(s, ',') FROM dev"},
			example{SQL: "SELECT array_agg(j) FROM dev"},
			example{SQL: "SELECT bool_and(j > 0), every(j > 0) FROM dev"},
			example{SQL: "SELECT grouping(j) FROM dev GROUP BY j"},
		)
	})

	t.Run("UNS-FLOAT-SUM a SUM or AVG of double precision whose result depends on the order of the rows", func(t *testing.T) {
		verify.Covers(t, "a SUM or AVG of double precision values whose sum, or whether it overflows, depends on the order in which Postgres adds them is rejected, and Postgres accepts it", "UNS-FLOAT-SUM")
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, f double precision)",
			example{SQL: "SELECT sum(f) FROM dev", Setup: []string{"INSERT INTO dev VALUES (1, 0.1), (2, 0.2), (3, 0.3)"}},
			example{SQL: "SELECT avg(f) FROM dev", Setup: []string{"INSERT INTO dev VALUES (1, 1e16), (2, 1), (3, -1e16)"}},
			example{SQL: "SELECT sum(f) FROM dev", Setup: []string{"INSERT INTO dev VALUES (1, 1e308), (2, -1e308), (3, 1e308)"}},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-AGGREGATE MIN and MAX that Postgres can compute from the first row of an index, with a WHERE condition that fails", func(t *testing.T) {
		verify.Covers(t, "a SELECT on one table without GROUP BY whose aggregates are all MIN and MAX, with a WHERE condition that fails on a row, is rejected, because Postgres can read only the first row of an index that passes the condition, and Postgres accepts it when that row comes first", "UNS-PLAN-DEPENDENT-AGGREGATE")
		rejects(t, create,
			example{SQL: "SELECT max(i) FROM dev WHERE 10 / (j - 1) > 0", Setup: []string{"INSERT INTO dev VALUES (1, 1, 'a'), (2, 3, 'b')"}},
		)
	})

	t.Run("UNS-LIMIT-ORDER LIMIT and OFFSET without a total order", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET without ORDER BY on every primary key column, or on groups without ORDER BY on every GROUP BY expression, are rejected, because which rows Postgres returns depends on its plan, and Postgres accepts them", "UNS-LIMIT-ORDER")
		rejects(t, create,
			example{SQL: "SELECT i FROM dev LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT i FROM dev ORDER BY j OFFSET 1", Setup: []string{rows}},
			example{SQL: "SELECT a.i FROM dev a, dev b ORDER BY a.i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT DISTINCT j, s FROM dev ORDER BY j LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT j, count(*) FROM dev GROUP BY j LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT j, s, count(*) FROM dev GROUP BY j, s ORDER BY j LIMIT 1", Setup: []string{rows}},
		)
	})

	t.Run("UNS-SUBQUERIES other forms of subqueries and of WITH", func(t *testing.T) {
		verify.Covers(t, "ANY, SOME, and ALL, a row on the left of IN, WITH RECURSIVE, MATERIALIZED and NOT MATERIALIZED, a query that writes in WITH, WITH in front of INSERT, UPDATE, or DELETE, a subquery in FROM without an alias, a repeated column name in a subquery in FROM or a CTE, an aggregate of the columns of a query around a subquery, t.* of a table of a query around, and a subquery in the list of an IN, in GROUP BY, or in LIMIT or OFFSET are rejected, and Postgres accepts them", "UNS-SUBQUERIES")
		rejects(t, create,
			example{SQL: "SELECT i FROM dev WHERE i = ANY (SELECT j FROM dev)"},
			example{SQL: "SELECT i FROM dev WHERE i > SOME (SELECT j FROM dev)"},
			example{SQL: "SELECT i FROM dev WHERE i > ALL (SELECT j FROM dev)"},
			example{SQL: "SELECT i FROM dev WHERE (i, j) IN (SELECT j, i FROM dev)"},
			example{SQL: "WITH RECURSIVE x (n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM x WHERE n < 3) SELECT * FROM x"},
			example{SQL: "WITH x AS MATERIALIZED (SELECT 1) SELECT * FROM x"},
			example{SQL: "WITH x AS NOT MATERIALIZED (SELECT 1) SELECT * FROM x"},
			example{SQL: "WITH x AS (INSERT INTO dev VALUES (3, 1, 'c') RETURNING i) SELECT * FROM x"},
			example{SQL: "WITH x AS (SELECT 4 AS i) INSERT INTO dev SELECT i, 1, 'd' FROM x"},
			example{SQL: "WITH x AS (SELECT 1 AS i) UPDATE dev SET j = 1 WHERE i IN (SELECT i FROM x)"},
			example{SQL: "WITH x AS (SELECT 1 AS i) DELETE FROM dev WHERE i IN (SELECT i FROM x)"},
			example{SQL: "SELECT * FROM (SELECT 1)"},
			example{SQL: "SELECT * FROM (SELECT 1 AS a, 2 AS a) x"},
			example{SQL: "WITH c (a, a) AS (SELECT 1, 2) SELECT * FROM c"},
			example{SQL: "SELECT (SELECT sum(dev.j)) FROM dev"},
			example{SQL: "SELECT (SELECT count(*) FROM dev d WHERE d.i < sum(dev.j)) FROM dev"},
			example{SQL: "SELECT EXISTS (SELECT dev.* FROM dev d) FROM dev"},
			example{SQL: "SELECT i FROM dev WHERE i IN ((SELECT 1), 2)"},
			example{SQL: "SELECT count(*) FROM dev GROUP BY (SELECT 1)"},
			example{SQL: "SELECT i FROM dev ORDER BY i LIMIT (SELECT 1)"},
			example{SQL: "SELECT i FROM dev ORDER BY i OFFSET (SELECT 1)"},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-SUBQUERY a subquery that fails where some plan evaluates it and another does not", func(t *testing.T) {
		verify.Covers(t, "a statement whose subquery fails on rows or in places that one plan of Postgres evaluates it on and another does not is rejected: an uncorrelated subquery in a condition, a correlated EXISTS whose conditions fail on rows that no row of the query around reaches, a correlated IN that can fail, an IN whose left side fails on a row for which its subquery has no rows, a subquery in FROM or a CTE that fails, and a condition that can fail on their columns, and Postgres accepts the statement when its plan does not evaluate the subquery there", "UNS-PLAN-DEPENDENT-SUBQUERY")
		setup := []string{rows, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)"}
		rejects(t, create,
			example{SQL: "SELECT * FROM dev2 WHERE j = (SELECT j FROM dev)", Setup: setup},
			example{SQL: "SELECT * FROM dev2 WHERE j IN (SELECT 10 / j FROM dev)", Setup: setup},
			example{SQL: "SELECT * FROM dev2 WHERE EXISTS (SELECT 1 FROM dev WHERE dev.i = dev2.j AND 10 / dev.j > 0)", Setup: setup},
			example{SQL: "SELECT * FROM dev2 WHERE j IN (SELECT dev.i FROM dev WHERE dev.j = dev2.i AND dev.i + 1 > 0)", Setup: setup},
			example{SQL: "SELECT count(*) FROM (SELECT 10 / j AS q FROM dev) x", Setup: setup},
			example{SQL: "WITH x AS (SELECT 10 / j AS q FROM dev) SELECT count(*) FROM x", Setup: setup},
			example{SQL: "SELECT * FROM (SELECT * FROM dev WHERE j <> 0) x WHERE 10 / x.j > 0", Setup: setup},
			example{SQL: "SELECT * FROM dev WHERE 10 / j IN (SELECT i FROM dev2)", Setup: setup},
			example{SQL: "SELECT * FROM dev WHERE 10 / j IN (SELECT dev2.i FROM dev2 WHERE dev2.j = dev.i)", Setup: setup},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-SUBQUERY a subquery whose correlation a NULL conjunct folds away", func(t *testing.T) {
		verify.Covers(t, "a scalar subquery whose references to the query around sit in a WHERE that a NULL conjunct decides, which Postgres folds to false and evaluates as an uncorrelated InitPlan before any row, is rejected when it returns more than one row and no row reaches it, where Postgres fails with 21000", "UNS-PLAN-DEPENDENT-SUBQUERY")
		b := newBoth(t, create)
		b.exec(t, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)")
		b.exec(t, "INSERT INTO dev2 VALUES (1, 1), (2, 2)")
		query := "SELECT 1 FROM dev AS o WHERE (SELECT true FROM dev2 WHERE NULL NOT IN (SELECT x.j FROM dev AS x WHERE x.j = o.j AND NOT NULL))"
		_, err := b.m.ExecContext(t.Context(), query)
		is.Error(t, maragudb.ErrUnsupported, err)
		_, err = b.pg.ExecContext(t.Context(), query)
		is.Equal(t, "21000", pgdiff.SQLState(err), fmt.Sprint(err))
	})

	t.Run("UNS-PLAN-DEPENDENT-JOIN a condition of a join that fails on a row that some plan evaluates it on", func(t *testing.T) {
		verify.Covers(t, "a SELECT on several tables with a condition that fails on a row or a combination of rows that a plan of Postgres could evaluate it on is rejected, because whether Postgres fails depends on its join plan, and Postgres accepts it when its plan does not evaluate the condition there", "UNS-PLAN-DEPENDENT-JOIN")
		rejects(t, create,
			example{SQL: "SELECT * FROM dev JOIN dev2 ON dev.j = dev2.j WHERE 10 / dev.j > 0", Setup: []string{rows, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)"}},
			example{SQL: "SELECT * FROM dev2 LEFT JOIN dev ON dev.j = dev2.j AND 10 / dev.j > 0", Setup: []string{rows, "CREATE TABLE dev2 (i bigint PRIMARY KEY, j bigint)"}},
		)
	})

	t.Run("UNS-DISTINCT-ZERO DISTINCT, GROUP BY, MIN, and MAX of -0 and 0", func(t *testing.T) {
		verify.Covers(t, "a SELECT DISTINCT whose result has -0 and 0 in one group, or numerics of different scales, a GROUP BY whose group has -0 and 0 when the query reads its value, and a MIN, MAX, or SUM(DISTINCT) of -0 and 0 are rejected, because which of them Postgres outputs depends on its plan, and Postgres accepts them", "UNS-DISTINCT-ZERO")
		zeros := []string{"INSERT INTO dev VALUES (1, '-0', 1), (2, 0, 1)"}
		rejects(t, "CREATE TABLE dev (i bigint PRIMARY KEY, f double precision, g bigint)",
			example{SQL: "SELECT DISTINCT f FROM dev", Setup: zeros},
			example{SQL: "SELECT DISTINCT a.f FROM dev a, dev b", Setup: zeros},
			example{SQL: "SELECT f FROM dev GROUP BY f", Setup: zeros},
			example{SQL: "SELECT min(f), max(f) FROM dev", Setup: zeros},
			example{SQL: "SELECT sum(DISTINCT f) FROM dev", Setup: zeros},
			example{SQL: "SELECT DISTINCT CASE WHEN i = 1 THEN sum(g) ELSE avg(g) END FROM dev GROUP BY i", Setup: zeros},
		)
	})

	t.Run("UNS-PLAN-DEPENDENT-LIMIT LIMIT and OFFSET with an expression that can fail", func(t *testing.T) {
		verify.Covers(t, "LIMIT and OFFSET with an output, ORDER BY, or WHERE expression that can fail are rejected, because how many rows Postgres evaluates it on depends on its plan, and Postgres accepts them when no row fails", "UNS-PLAN-DEPENDENT-LIMIT")
		rejects(t, create,
			example{SQL: "SELECT i + j FROM dev ORDER BY i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT i FROM dev WHERE j + 1 > 0 ORDER BY i LIMIT 1", Setup: []string{rows}},
			example{SQL: "SELECT j, sum(i + 1) FROM dev GROUP BY j ORDER BY j LIMIT 1", Setup: []string{rows}},
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
			example{SQL: "SELECT j FROM dev GROUP BY j HAVING j <> 0 AND 10 / j > 1", Setup: []string{rows}},
			example{SQL: "SELECT j FROM dev GROUP BY j HAVING count(*) > 1 AND 10 / (count(*) - 1) > 0", Setup: []string{rows}},
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
