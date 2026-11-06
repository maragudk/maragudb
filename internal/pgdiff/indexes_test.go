package pgdiff_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// indexTables are the tables of the index tests, which every test drops first, with their
// indexes.
var indexTables = []string{"ix", "ix2", "ix_pkey", "ix_a_key", "ixu", "ixs", "ixo"}

// columns33 are the definitions and names33 the names of 33 columns, one more than an index
// can have.
var columns33, names33 = func() (string, string) {
	var defs, names []string
	for i := range 33 {
		defs = append(defs, fmt.Sprintf("c%d bigint", i))
		names = append(names, fmt.Sprintf("c%d", i))
	}
	return strings.Join(defs, ", "), strings.Join(names, ", ")
}()

// newIndexSession compares statements on fresh tables named in [indexTables].
func newIndexSession(t *testing.T) *pgdiff.Session {
	t.Helper()
	r := newRunner(t)
	m, err := r.New()
	is.NotError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	pgConn, err := r.PG.Conn(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = pgConn.Close() })
	mConn, err := m.Conn(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = mConn.Close() })
	for _, table := range indexTables {
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table)
		is.NotError(t, err)
	}
	return pgdiff.NewSession(pgConn, mConn)
}

// outcome of a statement in an index test: it succeeds, fails on both with the class of code, or
// is rejected by maragudb.
type outcome struct {
	sql      string
	code     string
	rejected bool
}

// expect runs the statements in order and checks each outcome on both databases.
func expect(t *testing.T, s *pgdiff.Session, outcomes ...outcome) {
	t.Helper()
	for _, o := range outcomes {
		var rep pgdiff.Report
		got, err := s.Exec(t.Context(), o.sql, &rep)
		is.NotError(t, err, o.sql)
		if o.rejected {
			is.True(t, !got.Executed, fmt.Sprintf("%s: maragudb rejects it", o.sql))
			continue
		}
		is.True(t, got.Executed, fmt.Sprintf("%s: rejected: %s", o.sql, got.Rejection))
		is.Equal(t, o.code, pgdiff.SQLState(got.Err), fmt.Sprintf("%s: %v", o.sql, got.Err))
	}
}

func TestIndexes(t *testing.T) {
	t.Run("names indexes as Postgres does, in the namespace of tables, with the same errors", func(t *testing.T) {
		verify.Covers(t, "CREATE INDEX, DROP INDEX, and UNIQUE constraints name their indexes as Postgres does, tables and indexes share one namespace, and every clash, missing name, and wrong kind of relation fails with the class of Postgres's error", "INV-PG-SUBSET", "AC-4.3")
		s := newIndexSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE ix_pkey (x bigint PRIMARY KEY)"},
			outcome{sql: "CREATE TABLE ix (id bigint PRIMARY KEY, a bigint UNIQUE, b bigint, UNIQUE (a, b), UNIQUE (a), CONSTRAINT ix_b_key UNIQUE NULLS NOT DISTINCT (b))"},
			outcome{sql: "CREATE INDEX ON ix (b)"},
			outcome{sql: "CREATE INDEX ON ix (b DESC NULLS LAST)"},
			outcome{sql: "CREATE UNIQUE INDEX ON ix (a, a)"},
			outcome{sql: "CREATE INDEX ix_b_idx2 ON ix (a)"},
			outcome{sql: "CREATE INDEX ON ix (b)"},
			outcome{sql: "SELECT * FROM ix_pkey1", code: "42809"},
			outcome{sql: "INSERT INTO ix_a_key VALUES (1)", code: "42809"},
			outcome{sql: "UPDATE ix_a_b_key SET a = 1", code: "42809"},
			outcome{sql: "DELETE FROM ix_b_idx", code: "42809"},
			outcome{sql: "DROP INDEX ix_b_idx1"},
			outcome{sql: "DROP INDEX ix_b_idx3"},
			outcome{sql: "DROP INDEX ix_a_a1_idx"},
			outcome{sql: "DROP INDEX ix_b_idx2"},
			outcome{sql: "DROP INDEX ix_b_idx", code: ""},
			outcome{sql: "DROP INDEX ix_b_idx", code: "42704"},
			outcome{sql: "DROP INDEX IF EXISTS ix_b_idx"},
			outcome{sql: "DROP INDEX ix_pkey1", code: "2BP01"},
			outcome{sql: "DROP INDEX ix_a_key", code: "2BP01"},
			outcome{sql: "DROP INDEX ix_b_key", code: "2BP01"},
			outcome{sql: "DROP INDEX ix", code: "42809"},
			outcome{sql: "DROP TABLE ix_a_key", code: "42809"},
			outcome{sql: "DROP TABLE IF EXISTS ix_a_b_key", code: "42809"},
			outcome{sql: "CREATE TABLE ix_a_key (x bigint PRIMARY KEY)", code: "42P07"},
			outcome{sql: "CREATE TABLE IF NOT EXISTS ix_a_key (x bigint PRIMARY KEY)"},
			outcome{sql: "CREATE INDEX ix ON ix (a)", code: "42P07"},
			outcome{sql: "CREATE INDEX IF NOT EXISTS ix ON ix (a)"},
			outcome{sql: "CREATE INDEX IF NOT EXISTS ix ON ix (nope)", code: "42703"},
			outcome{sql: "CREATE INDEX ON nope (a)", code: "42P01"},
			outcome{sql: "CREATE INDEX ON ix_a_key (a)", code: "42809"},
			outcome{sql: "CREATE TABLE ix2 (id bigint PRIMARY KEY, CONSTRAINT ix_b_idx UNIQUE (id), a bigint UNIQUE, CONSTRAINT ix2_a_key UNIQUE (a, id))", code: "42P07"},
			outcome{sql: "CREATE TABLE ix2 (id bigint PRIMARY KEY, CONSTRAINT ix_b_idx UNIQUE (id), a bigint UNIQUE)"},
			outcome{sql: "DROP INDEX ix_b_idx", code: "2BP01"},
			outcome{sql: "CREATE TABLE ix2 (id bigint PRIMARY KEY, UNIQUE (nope))", code: "42703"},
			outcome{sql: "CREATE TABLE ix2 (id bigint PRIMARY KEY, UNIQUE (id, id))", code: "42701"},
			outcome{sql: "CREATE TABLE ixs (" + columns33 + ", PRIMARY KEY (c0), UNIQUE (" + names33 + "))", code: "54011"},
			outcome{sql: "CREATE TABLE ix (" + columns33 + ", PRIMARY KEY (c0), UNIQUE (" + names33 + "))", code: "42P07"},
			outcome{sql: "CREATE TABLE ixs (" + columns33 + ", PRIMARY KEY (c0), UNIQUE (" + names33 + ", nope))", code: "42703"},
			outcome{sql: "CREATE TABLE ixs (" + columns33 + ", PRIMARY KEY (c0))"},
			outcome{sql: "CREATE INDEX ON ixs (" + names33 + ")", code: "54011"},
			outcome{sql: "CREATE INDEX ON ixs (" + names33 + ", nope)", code: "54011"},
			outcome{sql: "DROP TABLE ixs"},
			outcome{sql: "DROP TABLE ix"},
			outcome{sql: "CREATE TABLE ix (id bigint PRIMARY KEY, a bigint UNIQUE)"},
			outcome{sql: "DROP TABLE ix2"},
			outcome{sql: "BEGIN"},
			outcome{sql: "CREATE INDEX ix2 ON ix (a)"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "CREATE TABLE ix2 (id bigint PRIMARY KEY)"},
			outcome{sql: "BEGIN READ ONLY"},
			outcome{sql: "CREATE INDEX ON ix (a)", code: "25006"},
			outcome{sql: "ROLLBACK"},
		)
	})

	t.Run("enforces UNIQUE as Postgres does, with NULLs distinct or not, and fails a unique index on duplicated rows", func(t *testing.T) {
		verify.Covers(t, "inserts, updates, and CREATE UNIQUE INDEX that would give two rows the same values of a unique index fail with SQLSTATE 23505 on both, NULLs collide only with NULLS NOT DISTINCT, and the rows are the same on both afterwards", "INV-UNIQUE", "INV-PG-SUBSET")
		s := newIndexSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE ixu (id bigint PRIMARY KEY, a bigint UNIQUE, b text, c double precision, UNIQUE NULLS NOT DISTINCT (b, c))"},
			outcome{sql: "INSERT INTO ixu VALUES (1, 1, 'x', 0), (2, NULL, NULL, NULL), (3, NULL, 'x', NULL)"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 1, 'y', 1)", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, 'x', '-0')", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, NULL, NULL)", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, 'z', 1), (5, 4, 'w', 1)", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, 'z', 1), (4, 5, 'w', 1)", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, 'z', 1), (5, NULL, 'z', 'NaN'), (6, NULL, 'z', 'NaN')", code: "23505"},
			outcome{sql: "INSERT INTO ixu VALUES (4, 4, 'z', 1), (5, NULL, 'z', 'NaN')"},
			outcome{sql: "UPDATE ixu SET a = 1 WHERE id = 4", code: "23505"},
			outcome{sql: "UPDATE ixu SET a = 9 WHERE id > 3", code: "23505"},
			outcome{sql: "UPDATE ixu SET c = 'NaN' WHERE id = 4", code: "23505"},
			outcome{sql: "UPDATE ixu SET a = a, b = b"},
			outcome{sql: "UPDATE ixu SET a = NULL WHERE id = 1"},
			outcome{sql: "INSERT INTO ixu VALUES (6, 1, 'v', 1)"},
			outcome{sql: "DELETE FROM ixu WHERE a = 1"},
			outcome{sql: "INSERT INTO ixu VALUES (6, 1, 'v', 1)"},
			outcome{sql: "BEGIN"},
			outcome{sql: "INSERT INTO ixu VALUES (7, 7, 'u', 1)"},
			outcome{sql: "INSERT INTO ixu VALUES (8, 7, 'u', 2)", code: "23505"},
			outcome{sql: "ROLLBACK"},
			outcome{sql: "SELECT * FROM ixu ORDER BY id"},
			outcome{sql: "CREATE TABLE ixs (id bigint PRIMARY KEY, a bigint, b bigint)"},
			outcome{sql: "INSERT INTO ixs VALUES (1, 1, NULL), (2, 1, NULL), (3, 2, 2)"},
			outcome{sql: "CREATE UNIQUE INDEX ON ixs (b)"},
			outcome{sql: "CREATE UNIQUE INDEX ON ixs (a, b)"},
			outcome{sql: "CREATE UNIQUE INDEX ON ixs (a)", code: "23505"},
			outcome{sql: "CREATE UNIQUE INDEX ON ixs (b) NULLS NOT DISTINCT", code: "23505"},
			outcome{sql: "CREATE INDEX ON ixs (a) NULLS NOT DISTINCT"},
			outcome{sql: "INSERT INTO ixs VALUES (4, 1, 3)"},
		)
	})

	t.Run("fails a cycle of unique values in an UPDATE as Postgres does in every order, and rejects a chain", func(t *testing.T) {
		verify.Covers(t, "an UPDATE whose rows swap the values of a unique index or of the primary key fails with SQLSTATE 23505 on both, an UPDATE that moves a chain of rows onto the values the next one leaves is rejected, and one that moves rows onto free values succeeds on both", "UNS-KEY-UPDATE-ORDER", "INV-UNIQUE", "INV-PG-SUBSET")
		s := newIndexSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE ixu (id bigint PRIMARY KEY, a bigint UNIQUE, n bigint)"},
			outcome{sql: "INSERT INTO ixu VALUES (1, 1, 1), (2, 2, 0), (3, 3, 1)"},
			outcome{sql: "UPDATE ixu SET a = 4 - a", code: "23505"},
			outcome{sql: "UPDATE ixu SET a = CASE a WHEN 3 THEN 1 ELSE a + 1 END", code: "23505"},
			outcome{sql: "UPDATE ixu SET id = 4 - id", code: "23505"},
			outcome{sql: "UPDATE ixu SET a = a + 1", rejected: true},
			outcome{sql: "UPDATE ixu SET a = a - 1", rejected: true},
			outcome{sql: "UPDATE ixu SET a = a - 1 WHERE a >= 2", code: "23505"},
			outcome{sql: "UPDATE ixu SET a = a + 10"},
			outcome{sql: "UPDATE ixu SET a = a - 10 WHERE a > 11"},
			outcome{sql: "UPDATE ixu SET a = 3, n = 10 / n WHERE id <= 2", code: "23505"},
			outcome{sql: "SELECT * FROM ixu ORDER BY id"},
		)
	})
}

// planOutcome is a statement of [TestIndexPlans]: it succeeds on both, fails on both, or
// maragudb rejects it because whether it fails depends on the index Postgres picks.
type planOutcome int

const (
	succeeds planOutcome = iota
	fails
	dependsOnPlan
)

func TestIndexPlans(t *testing.T) {
	t.Run("evaluates the other conditions only on the rows that every index scan Postgres could pick skips, and otherwise on every row", func(t *testing.T) {
		verify.Covers(t, "with several indexes, a condition that can fail is evaluated on the rows that any index scan Postgres could pick reads: those of each index's certain conditions, and every row when an index without them could serve a condition or ORDER BY; maragudb gives Postgres's result when every such scan agrees, and rejects the statement otherwise", "INV-PG-SUBSET", "AC-4.3", "UNS-ORDER-DEPENDENT-WHERE")
		s := newIndexSession(t)
		expect(t, s,
			outcome{sql: "CREATE TABLE ixo (id bigint PRIMARY KEY, a bigint, b text, k boolean, j bigint)"},
			outcome{sql: "CREATE INDEX ixo_a ON ixo (a)"},
			outcome{sql: "CREATE UNIQUE INDEX ixo_bk ON ixo (b, k)"},
			outcome{sql: "INSERT INTO ixo VALUES (1, 1, 'a', TRUE, 1), (2, 2, 'b', FALSE, 0), (3, 3, 'c', TRUE, 0), (4, 4, 'd', FALSE, 5)"},
		)
		for _, e := range []struct {
			sql  string
			want planOutcome
		}{
			{"SELECT id FROM ixo WHERE a = 1 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE a IN (1, 4) AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE a >= 4 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE b = 'a' AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE k AND b < 'c' AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE NOT k AND b > 'c' AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE a = j + 1 AND j + 1 = 1 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE a = j + 2 AND j + 2 = 2 AND 10 / j > 0", fails},
			{"SELECT id FROM ixo WHERE id = 1 AND a = 1 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE id <= 1 AND a <= 1 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE id = 1 AND a = 4 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE id = 1 AND a = 2 AND 10 / j > 0", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id IN (1, 4) AND b = 'b' AND 10 / j > 0", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id = 1 AND 10 / j > 0 ORDER BY a", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id = 1 AND 10 / j > 0 ORDER BY j", succeeds},
			{"SELECT id FROM ixo WHERE id = 1 AND a IS NOT NULL AND 10 / j > 0", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id = 1 AND (a = 2 OR a = 3) AND 10 / j > 0", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id = 1 AND b LIKE 'b%' AND 10 / j > 0", dependsOnPlan},
			{"SELECT id FROM ixo WHERE id = 1 AND a + 0 = 2 AND 10 / j > 0", succeeds},
			{"SELECT id FROM ixo WHERE a = 2 AND 10 / j > 0", fails},
			{"SELECT id FROM ixo WHERE a = 2 AND b = 'b' AND 10 / j > 0", fails},
			{"UPDATE ixo SET j = j - 1 WHERE a = 4 AND j - 1 >= 0", succeeds},
			{"UPDATE ixo SET j = 10 / j WHERE b = 'a' AND k", succeeds},
			{"UPDATE ixo SET j = j WHERE id = 4 AND a = 2 AND 10 / j > 0", dependsOnPlan},
			{"DELETE FROM ixo WHERE a = 4 AND 10 / (j - 5) < 0", succeeds},
			{"SELECT * FROM ixo ORDER BY id", succeeds},
		} {
			var rep pgdiff.Report
			o, err := s.Exec(t.Context(), e.sql, &rep)
			is.NotError(t, err, e.sql)
			switch e.want {
			case dependsOnPlan:
				is.True(t, !o.Executed && strings.HasPrefix(o.Rejection, "whether"), fmt.Sprintf("%s: rejected as plan-dependent, got %+v", e.sql, o))
			default:
				is.True(t, o.Executed, fmt.Sprintf("%s: rejected: %s", e.sql, o.Rejection))
				is.Equal(t, e.want == fails, o.Err != nil, fmt.Sprintf("%s: fails on both: %v", e.sql, o.Err))
			}
		}
	})

	t.Run("Postgres takes the certain conditions on an indexed column as conditions of the scan of the index, and picks between indexes", func(t *testing.T) {
		verify.Covers(t, "EXPLAIN on Postgres 18 with sequential scans off shows that comparisons, IN, and boolean conditions on any column of a secondary index are conditions of its scan, that it picks between the primary key and another index with conditions, and that it scans a whole index in order for ORDER BY", "INV-PG-SUBSET")
		b := newBoth(t, "CREATE TABLE dev (i bigint PRIMARY KEY, a bigint, s text, k boolean, j bigint)")
		b.exec(t, "CREATE INDEX dev_ask ON dev (a, s, k)")
		explain := func(sql string) string {
			t.Helper()
			conn, err := b.pg.Conn(t.Context())
			is.NotError(t, err)
			defer func() { _ = conn.Close() }()
			rows, err := conn.QueryContext(t.Context(), "EXPLAIN (COSTS OFF) "+sql)
			is.NotError(t, err)
			defer func() { _ = rows.Close() }()
			var lines []string
			for rows.Next() {
				var line string
				is.NotError(t, rows.Scan(&line))
				lines = append(lines, strings.TrimSpace(line))
			}
			return strings.Join(lines, "\n")
		}
		for cond, want := range map[string]string{
			"a = 1":           "(a = 1)",
			"1 < a":           "(a > 1)",
			"a IN (1, 2)":     "(a = ANY ('{1,2}'::bigint[]))",
			"s >= 'x'":        "(s >= 'x'::text)",
			"k":               "(k = true)",
			"NOT k":           "(k = false)",
			"a = j AND j = 3": "(a = 3)",
		} {
			plan := explain("SELECT i FROM dev WHERE " + cond + " AND 10 / j > 0")
			is.True(t, strings.Contains(plan, "dev_ask") && strings.Contains(plan, "Cond: "+want) || strings.Contains(plan, "Cond: ("+want), fmt.Sprintf("%s:\n%s", cond, plan))
		}
		is.True(t, strings.Contains(explain("SELECT i FROM dev WHERE a + 0 = 1 AND i = 1"), "Index Cond: (i = 1)"))
		for _, q := range []string{"INSERT INTO dev SELECT n, n, 'x', TRUE, n FROM generate_series(1, 2000) n", "ANALYZE dev"} {
			_, err := b.pg.ExecContext(t.Context(), q)
			is.NotError(t, err)
		}
		is.True(t, strings.Contains(explain("SELECT i FROM dev WHERE i > 0 AND a = 5"), "dev_ask"), "the secondary index when it is the narrower")
		is.True(t, strings.Contains(explain("SELECT i FROM dev WHERE i = 5 AND a > 0"), "dev_pkey"), "the primary key when it is the narrower")
		plan := explain("SELECT i FROM dev WHERE i < 1000 AND 10 / j > 0 ORDER BY a LIMIT 1")
		is.True(t, strings.Contains(plan, "Index Scan using dev_ask") && !strings.Contains(plan, "Index Cond"), "a whole index in order, with the key condition as a filter:\n"+plan)
	})
}
