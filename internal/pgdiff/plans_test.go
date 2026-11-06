package pgdiff_test

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// planTables are the tables of [TestIndexConditions]. In each, the condition that can fail in
// the examples fails on a row that the key conditions do not select, so a statement agrees
// between the databases only when both evaluate it on the selected rows alone.
var planTables = []string{
	"CREATE TABLE plan_i (i bigint PRIMARY KEY, j bigint, s text)",
	"INSERT INTO plan_i VALUES (1, 0, 'a'), (2, 5, 'b'), (3, 9223372036854775807, 'c'), (4, -9223372036854775808, 'd')",
	"CREATE TABLE plan_ab (a bigint, b bigint, j bigint, PRIMARY KEY (a, b))",
	"INSERT INTO plan_ab VALUES (1, 1, 5), (1, 2, 0), (2, 1, 5), (2, 2, 0)",
	"CREATE TABLE plan_k (k boolean PRIMARY KEY, j bigint)",
	"INSERT INTO plan_k VALUES (TRUE, 5), (FALSE, 0)",
	"CREATE TABLE plan_f (f double precision PRIMARY KEY, j bigint)",
	"INSERT INTO plan_f VALUES (1, 0), (2, 5)",
	"CREATE TABLE plan_s (s text PRIMARY KEY, j bigint)",
	"INSERT INTO plan_s VALUES ('a', 0), ('b', 5)",
	"CREATE TABLE plan_ts (ts timestamptz PRIMARY KEY, j bigint)",
	"INSERT INTO plan_ts VALUES ('2024-01-01 00:00:00+00', 0), ('2024-01-02 00:00:00+00', 5)",
}

func TestIndexConditions(t *testing.T) {
	t.Run("evaluates the other conditions only on the rows that the primary key conditions select, as Postgres does with an index scan", func(t *testing.T) {
		verify.Covers(t, "with sequential scans off, Postgres evaluates the other WHERE conditions only on the rows that the conditions on the primary key index select: comparisons and IN of constants on any key column, a boolean key column, and key equalities implied by equalities with a constant; maragudb accepts such statements and gives the same results", "INV-PG-SUBSET", "AC-4.3")
		s := newPlanSession(t)
		for _, e := range []struct {
			sql  string
			fail bool
		}{
			{sql: "SELECT i FROM plan_i WHERE i = 2 AND 10 / j > 1"},
			{sql: "SELECT i FROM plan_i WHERE 10 / j > 1 AND i = 2"},
			{sql: "SELECT i FROM plan_i WHERE 2 = i AND j + 1 > 0"},
			{sql: "SELECT i FROM plan_i WHERE i IN (2, 3) AND 10 / j >= 0"},
			{sql: "SELECT i FROM plan_i WHERE i IN (2, 5, NULL) AND 10 / j >= 0"},
			{sql: "SELECT i FROM plan_i WHERE i > 1 AND i < 3 AND j + 1 > 0"},
			{sql: "SELECT i FROM plan_i WHERE i BETWEEN 2 AND 3 AND 10 / j >= 0"},
			{sql: "SELECT i FROM plan_i WHERE NOT (i <> 2) AND 10 / j > 0"},
			{sql: "SELECT i FROM plan_i WHERE i >= 2 AND i IN (1, 2, 3) AND j - 1 > 0 AND 10 / j >= 0"},
			{sql: "SELECT i FROM plan_i WHERE i = '2' AND 10 / j > 0"},
			{sql: "SELECT i FROM plan_i WHERE i = 1 + 1 AND 10 / j > 0"},
			{sql: "SELECT i FROM plan_i WHERE i = j - 3 AND j - 3 = 2 AND 10 / j > 0"},
			{sql: "SELECT i FROM plan_i WHERE i = 2 AND i = 3 AND 10 / j > 0"},
			{sql: "SELECT i FROM plan_i WHERE i > 3 AND i < 2 AND 10 / j > 0"},
			{sql: "SELECT i, 10 / j FROM plan_i WHERE i IN (2, 3) AND j <> 0 ORDER BY i"},
			{sql: "SELECT i FROM plan_i WHERE i = 1 AND 10 / j > 0", fail: true},
			{sql: "SELECT i FROM plan_i WHERE i >= 3 AND j + 1 > 0", fail: true},
			{sql: "SELECT a, b FROM plan_ab WHERE a = 1 AND b = 1 AND 10 / j > 0"},
			{sql: "SELECT a, b FROM plan_ab WHERE a = 2 AND b < 2 AND 10 / j > 0"},
			{sql: "SELECT a, b FROM plan_ab WHERE b = 1 AND 10 / j > 0"},
			{sql: "SELECT a, b FROM plan_ab WHERE a > 1 AND b = 1 AND 10 / j > 0"},
			{sql: "SELECT a, b FROM plan_ab WHERE a IN (1, 2) AND b IN (1, 3) AND 10 / j > 0 ORDER BY a, b"},
			{sql: "SELECT a, b FROM plan_ab WHERE a = b AND b = 1 AND 10 / j > 0"},
			{sql: "SELECT a, b FROM plan_ab WHERE b = 2 AND 10 / j > 0", fail: true},
			{sql: "SELECT j FROM plan_k WHERE k AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_k WHERE k = TRUE AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_k WHERE k > FALSE AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_k WHERE NOT k AND 10 / (j - 5) < 0"},
			{sql: "SELECT j FROM plan_f WHERE f = 2 AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_f WHERE f IN (2, 3) AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_f WHERE f > 1.5 AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_s WHERE s = 'b' AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_s WHERE s >= 'b' AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_ts WHERE ts = '2024-01-02 00:00:00+00' AND 10 / j > 0"},
			{sql: "SELECT j FROM plan_ts WHERE ts > '2024-01-01 12:00:00+00' AND 10 / j > 0"},
			{sql: "UPDATE plan_i SET j = j - 5 WHERE i = 2 AND j - 5 >= 0"},
			{sql: "UPDATE plan_i SET j = j + 5 WHERE i IN (1, 2) AND 10 / (j + 1) >= 0"},
			{sql: "DELETE FROM plan_ab WHERE a = 2 AND b = 1 AND 10 / j > 0"},
			{sql: "BEGIN"},
			{sql: "UPDATE plan_i SET j = 0 WHERE i = 1 AND j + 1 > 0"},
			{sql: "DELETE FROM plan_i WHERE i > 1 AND i < 4 AND j - 1 > 0"},
			{sql: "COMMIT"},
			{sql: "SELECT * FROM plan_i ORDER BY i"},
			{sql: "SELECT * FROM plan_ab ORDER BY a, b"},
		} {
			var rep pgdiff.Report
			o, err := s.Exec(t.Context(), e.sql, &rep)
			is.NotError(t, err, e.sql)
			is.True(t, o.Executed, fmt.Sprintf("%s: rejected: %s", e.sql, o.Rejection))
			is.Equal(t, e.fail, o.Err != nil, fmt.Sprintf("%s: fails on both: %v", e.sql, o.Err))
		}
	})

	t.Run("takes the index conditions out of an OR as Postgres does when it drops constants and common conditions from the OR", func(t *testing.T) {
		verify.Covers(t, "in a WHERE clause, Postgres drops the arguments of an OR that are FALSE or NULL constants and takes the conditions common to every argument out of the OR, which can make them index conditions; maragudb does the same and gives the same results", "INV-PG-SUBSET", "AC-4.3")
		s := newPlanSession(t)
		for _, sql := range []string{
			"SELECT i FROM plan_i WHERE NULL OR (10 / j > 1 AND i = 2)",
			"SELECT i FROM plan_i WHERE FALSE OR 1 = 2 OR (10 / j > 1 AND i = 2)",
			"SELECT i FROM plan_i WHERE (NULL AND 10 / j > 0) OR (10 / j > 1 AND i = 2)",
			"SELECT i FROM plan_i WHERE NOT (NULL AND i <> 2) AND 10 / j > 1",
			"SELECT i FROM plan_i WHERE (10 / j > 1 AND i = 2) OR (j + 1 > 0 AND i = 2)",
			"SELECT i FROM plan_i WHERE (10 / j > 1 AND i = 2) OR (i = 2 AND j + 1 > 0 AND i = 2)",
			"SELECT i FROM plan_i WHERE (10 / j > 1 AND i = 2) OR i = 2",
			"SELECT i FROM plan_i WHERE (i BETWEEN 2 AND 3 AND 10 / j >= 0) OR (i >= 2 AND i <= 3 AND j + 1 > 0)",
			"SELECT i FROM plan_i WHERE (i = 2 AND i = 2 AND 10 / j > 1) OR (i = 2 AND i = 2 AND j + 1 > 0)",
			"SELECT i FROM plan_i WHERE ((10 / j > 1 AND i = 2) OR (j + 1 > 0 AND i = 2)) AND (NULL OR j - 1 > 0)",
			"UPDATE plan_i SET j = j WHERE NULL OR (j - 1 > 0 AND i IN (2, 3))",
			"SELECT i FROM plan_i WHERE TRUE OR CAST(CAST('x' AS text) AS bigint) = NULL",
			"SELECT i FROM plan_i WHERE (FALSE AND CAST(CAST('x' AS text) AS bigint) = 1) OR i = 2",
		} {
			var rep pgdiff.Report
			o, err := s.Exec(t.Context(), sql, &rep)
			is.NotError(t, err, sql)
			is.True(t, o.Executed, fmt.Sprintf("%s: rejected: %s", sql, o.Rejection))
			is.True(t, o.Err == nil, fmt.Sprintf("%s: %v", sql, o.Err))
		}
		// Postgres folds every argument before the one that decides, and reports its error.
		for _, sql := range []string{
			"SELECT i FROM plan_i WHERE CAST(CAST('x' AS text) AS bigint) = NULL OR NULL",
			"SELECT i FROM plan_i WHERE (NULL AND CAST(CAST('x' AS text) AS bigint) = 1) OR i = 2",
			"SELECT i FROM plan_i WHERE CAST(CAST('x' AS text) AS bigint) = NULL OR TRUE",
			// An AND with a NULL argument is false in a WHERE clause, but not a constant, so the
			// constants of the next conjunct are folded too.
			"SELECT i FROM plan_i WHERE ((j = 1 AND NULL) OR NULL) AND 1 / 0 = 1",
			"SELECT i FROM plan_i WHERE ((j = 1 AND NULL) OR FALSE) AND 1 / 0 = 1",
			// And it checks the types of every argument first.
			"SELECT i FROM plan_i WHERE CAST('\\x' AS bytea) IN ('\\x', s || coalesce('\\x'))",
			"SELECT i FROM plan_i WHERE i OR TRUE",
			"SELECT i FROM plan_i WHERE CAST(NULL AS bigint) OR TRUE",
			"SELECT i FROM plan_i WHERE FALSE AND (TRUE OR j)",
		} {
			var rep pgdiff.Report
			o, err := s.Exec(t.Context(), sql, &rep)
			is.NotError(t, err, sql)
			is.True(t, o.Executed && o.Err != nil, fmt.Sprintf("%s fails on both", sql))
		}
	})
}

// newPlanSession compares statements on the tables in [planTables].
func newPlanSession(t *testing.T) *pgdiff.Session {
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
	for _, table := range []string{"plan_i", "plan_ab", "plan_k", "plan_f", "plan_s", "plan_ts"} {
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table)
		is.NotError(t, err)
	}
	s := pgdiff.NewSession(pgConn, mConn)
	for _, q := range planTables {
		var rep pgdiff.Report
		o, err := s.Exec(t.Context(), q, &rep)
		is.NotError(t, err, q)
		is.True(t, o.Executed && o.Err == nil, q)
	}
	return s
}
