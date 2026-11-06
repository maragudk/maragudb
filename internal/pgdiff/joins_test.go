package pgdiff_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// joinTables are the tables of the join tests, which every test drops first.
var joinTables = []string{"ja", "jb", "jc", "jd", "jf", "jt"}

// newJoinSession compares statements on fresh tables named in [joinTables], with rows that
// every test of joins shares: ja has a row that no row of jb references, jb has a row whose
// reference is missing and one whose reference is NULL, jc shares the column names id and v
// with ja, and jd is empty.
func newJoinSession(t *testing.T) *pgdiff.Session {
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
	for _, table := range joinTables {
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table+" CASCADE")
		is.NotError(t, err)
	}
	s := pgdiff.NewSession(pgConn, mConn)
	expect(t, s,
		outcome{sql: "CREATE TABLE ja (id bigint PRIMARY KEY, v bigint, s text, f double precision)"},
		outcome{sql: "CREATE TABLE jb (id bigint PRIMARY KEY, aid bigint REFERENCES ja, w bigint NOT NULL, s text)"},
		outcome{sql: "CREATE INDEX jb_aid ON jb (aid)"},
		outcome{sql: "CREATE TABLE jc (id bigint PRIMARY KEY, v double precision, t text)"},
		outcome{sql: "CREATE TABLE jd (id bigint PRIMARY KEY, v bigint)"},
		outcome{sql: "INSERT INTO ja VALUES (1, 10, 'a', 0), (2, 20, 'b', -0.5), (3, NULL, NULL, '-0'), (4, 10, 'b', 'NaN')"},
		outcome{sql: "INSERT INTO jb VALUES (10, 1, 1, 'x'), (11, 1, 2, NULL), (12, 2, 0, 'y'), (13, NULL, 3, 'z')"},
		outcome{sql: "INSERT INTO jc VALUES (1, 10, 'p'), (2, 20.5, 'q'), (5, NULL, 'r')"},
	)
	return s
}

func TestJoins(t *testing.T) {
	t.Run("resolves aliases and qualified names as Postgres does, with the same errors", func(t *testing.T) {
		verify.Covers(t, "table aliases with and without AS, names for the columns of a table, qualified names, t.*, and a name alone resolve as in Postgres, and an ambiguous name (42702), a missing or hidden FROM entry (42P01), a table named twice (42712), a missing column (42703), and too many column names (42P10) fail with the class of Postgres's error", "INV-PG-SUBSET", "AC-4.3")
		s := newJoinSession(t)
		expect(t, s,
			outcome{sql: "SELECT x.id, x.v FROM ja AS x ORDER BY x.id"},
			outcome{sql: "SELECT x.id, v FROM ja x WHERE x.v = 10 ORDER BY id"},
			outcome{sql: "SELECT ja.id, ja.s FROM ja ORDER BY ja.id"},
			outcome{sql: "SELECT x.* FROM ja x ORDER BY 1"},
			outcome{sql: "SELECT x.*, x.id AS k FROM ja x ORDER BY k"},
			outcome{sql: "SELECT * FROM ja AS x (p, q) ORDER BY p"},
			outcome{sql: "SELECT x.p, q, x.s FROM ja AS x (p, q) WHERE x.q = 10 ORDER BY x.p"},
			outcome{sql: "SELECT x.id FROM ja AS x (p, q)", code: "42703"},
			outcome{sql: "SELECT * FROM ja AS x (a, b, c, d, e)", code: "42P10"},
			outcome{sql: "SELECT ja.id FROM ja x", code: "42P01"},
			outcome{sql: "SELECT q.id FROM ja", code: "42P01"},
			outcome{sql: "SELECT q.* FROM ja", code: "42P01"},
			outcome{sql: "SELECT ja.nope FROM ja", code: "42703"},
			outcome{sql: "SELECT id FROM ja, jb", code: "42702"},
			outcome{sql: "SELECT * FROM ja, ja", code: "42712"},
			outcome{sql: "SELECT * FROM ja x, jb x", code: "42712"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON true JOIN jb ON true", code: "42712"},
			outcome{sql: "SELECT * FROM ja, nope", code: "42P01"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON nope = 1 JOIN nope ON true", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN nope ON nope = 1", code: "42P01"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON jc.id = 1", code: "42P01"},
			outcome{sql: "SELECT * FROM ja, jb JOIN jc ON ja.id = jc.id", code: "42P01"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON ja.id = jc.id, jc", code: "42P01"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON ja.id = jb.aid AND jb.nope = 1", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON ja.v", code: "42804"},
			outcome{sql: "SELECT x.s || y.s AS st, x.id FROM ja x, ja y WHERE x.id = y.id ORDER BY x.id"},
			outcome{sql: "SELECT ja FROM ja", rejected: true},
			outcome{sql: "SELECT nope FROM ja JOIN jb ON 1 / 0 = 1", code: "42703"},
			outcome{sql: "SELECT 1 / 0 FROM ja JOIN jb ON 1 / 0 = 1", code: "22012"},
			outcome{sql: "SELECT 1 FROM ja JOIN jb ON 2147483647 + 1 = 1 WHERE 1 / 0 = 1", code: "22003"},
			outcome{sql: "UPDATE ja AS x SET v = x.v + 1 WHERE x.id = 1"},
			outcome{sql: "UPDATE ja x SET v = v - 1 WHERE x.id = 1"},
			outcome{sql: "UPDATE ja AS x SET v = 1 WHERE ja.id = 1", code: "42P01"},
			outcome{sql: "UPDATE ja AS x SET x.v = 1 WHERE x.id = 1", code: "42703"},
			outcome{sql: "UPDATE ja SET v.v = 1 WHERE id = 1", code: "42804"},
			outcome{sql: "UPDATE ja SET ja.v = 1 WHERE id = 1", code: "42703"},
			outcome{sql: "DELETE FROM jb AS x WHERE x.id = 13"},
			outcome{sql: "DELETE FROM jb x WHERE jb.id = 13", code: "42P01"},
			outcome{sql: "SELECT * FROM jb ORDER BY id"},
		)
	})

	t.Run("joins as Postgres does", func(t *testing.T) {
		verify.Covers(t, "several tables in FROM, CROSS JOIN, [INNER] JOIN with ON and USING, LEFT [OUTER] JOIN with ON and USING, NATURAL joins, self-joins, and parenthesized and nested joins give Postgres's rows and column names, with the columns that USING merges once, of the common type, and those of a LEFT JOIN's right side NULL where it has no row", "INV-PG-SUBSET", "AC-4.2")
		s := newJoinSession(t)
		expect(t, s,
			outcome{sql: "SELECT * FROM ja, jb"},
			outcome{sql: "SELECT * FROM ja CROSS JOIN jd"},
			outcome{sql: "SELECT * FROM ja, jb WHERE ja.id = jb.aid"},
			outcome{sql: "SELECT ja.id, jb.id FROM ja JOIN jb ON ja.id = jb.aid ORDER BY jb.id"},
			outcome{sql: "SELECT ja.id, jb.id FROM ja INNER JOIN jb ON ja.id = jb.aid AND jb.w > 0 WHERE ja.v = 10"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid ORDER BY ja.id, jb.id"},
			outcome{sql: "SELECT * FROM ja LEFT OUTER JOIN jb ON ja.id = jb.aid AND jb.w > 1"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid WHERE jb.id IS NULL"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid AND ja.v = 10"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON false"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jd ON true"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON NULL"},
			outcome{sql: "SELECT * FROM ja JOIN jc USING (id)"},
			outcome{sql: "SELECT * FROM ja JOIN jc USING (id, v)"},
			outcome{sql: "SELECT id, ja.v, jc.v FROM ja LEFT JOIN jc USING (id) ORDER BY id"},
			outcome{sql: "SELECT id, v FROM ja LEFT JOIN jc USING (id)", code: "42702"},
			outcome{sql: "SELECT * FROM ja NATURAL JOIN jc"},
			outcome{sql: "SELECT * FROM ja NATURAL LEFT JOIN jc"},
			outcome{sql: "SELECT * FROM ja NATURAL JOIN jb"},
			outcome{sql: "SELECT * FROM jb NATURAL JOIN jd"},
			outcome{sql: "SELECT * FROM ja JOIN jb USING (nope)", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN jb USING (id, id)", code: "42701"},
			outcome{sql: "SELECT * FROM ja JOIN jb USING (w)", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN jc USING (s)", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN jc USING (s, nope)", code: "42703"},
			outcome{sql: "SELECT * FROM ja JOIN jb USING (s) JOIN jc USING (id)", code: "42702"},
			outcome{sql: "SELECT * FROM ja JOIN jb USING (s) JOIN jc USING (v)"},
			outcome{sql: "SELECT * FROM (ja JOIN jb ON true) JOIN jc USING (id)", code: "42702"},
			outcome{sql: "SELECT * FROM jc JOIN jb USING (id) JOIN ja USING (s)"},
			outcome{sql: "SELECT * FROM jc JOIN jb USING (id) JOIN ja USING (t)", code: "42703"},
			outcome{sql: "SELECT * FROM ja x JOIN jc y USING (v)"},
			outcome{sql: "SELECT * FROM jb JOIN jc ON jb.s = jc.t JOIN ja USING (id)", code: "42702"},
			outcome{sql: "SELECT * FROM jb JOIN jc ON jb.w = jc.id JOIN ja ON ja.id = jb.aid"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON jb.s = ja.v", code: "42883"},
			outcome{sql: "SELECT * FROM jc JOIN jb USING (s)", code: "42703"},
			outcome{sql: "SELECT * FROM jb JOIN ja USING (s)"},
			outcome{sql: "SELECT * FROM ja x JOIN ja y ON x.id < y.id ORDER BY x.id, y.id"},
			outcome{sql: "SELECT x.id, y.id, z.id FROM ja x JOIN ja y ON x.v = y.v JOIN ja z ON y.id = z.id ORDER BY 1, 2, 3"},
			outcome{sql: "SELECT * FROM ja JOIN (jb LEFT JOIN jc ON jb.w = jc.id) ON ja.id = jb.aid"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN (jb JOIN jc ON jb.id = jc.id + 9) ON ja.id = jb.aid"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb JOIN jc ON jb.w = jc.id ON ja.id = jb.aid"},
			outcome{sql: "SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid LEFT JOIN jc ON jb.w = jc.id ORDER BY ja.id, jb.id"},
			outcome{sql: "SELECT * FROM ja, jb LEFT JOIN jc ON jb.w = jc.id WHERE ja.id = jb.aid OR jc.id IS NULL"},
			outcome{sql: "SELECT ja.*, jb.w FROM ja JOIN jb ON ja.id = jb.aid"},
			outcome{sql: "SELECT ja.id + jb.w, jb.s || ja.s, ja.v = jb.w AS eq FROM ja JOIN jb ON ja.id = jb.aid"},
			outcome{sql: "SELECT * FROM ja JOIN jb ON ja.id = jb.aid JOIN jd ON jd.id = ja.id"},
			outcome{sql: "SELECT * FROM jd LEFT JOIN ja ON true"},
			outcome{sql: "SELECT jb.id, ja.s FROM jb LEFT JOIN ja ON ja.id = jb.aid ORDER BY jb.id, ja.id LIMIT 2"},
			outcome{sql: "SELECT jb.id, ja.s FROM jb LEFT JOIN ja ON ja.id = jb.aid ORDER BY jb.id, ja.id OFFSET 1"},
			outcome{sql: "SELECT jb.id FROM jb LEFT JOIN ja ON ja.id = jb.aid ORDER BY jb.id LIMIT 2", rejected: true},
			outcome{sql: "SELECT jb.w + 1 FROM jb JOIN ja ON ja.id = jb.aid ORDER BY jb.id, ja.id LIMIT 2", rejected: true},
			outcome{sql: "SELECT jb.id FROM jb JOIN ja ON ja.id = jb.aid WHERE jb.w + 1 > 0 ORDER BY jb.id, ja.id LIMIT 2", rejected: true},
			outcome{sql: "SELECT * FROM ja, jb, jc, jd WHERE ja.id = jb.aid AND jb.w = jc.id AND jc.id = jd.id"},
		)
	})

	t.Run("names the output columns as Postgres does, and resolves ORDER BY against them", func(t *testing.T) {
		verify.Covers(t, "the output columns of a join are named as in Postgres, with repeated names, and ORDER BY a name alone is the output column of that name, ambiguous only when two of them are different expressions", "INV-PG-SUBSET")
		s := newJoinSession(t)
		expect(t, s,
			outcome{sql: "SELECT ja.id, jb.id, ja.id + 1, jb.s FROM ja JOIN jb ON ja.id = jb.aid"},
			outcome{sql: "SELECT ja.v AS id, jb.id FROM ja, jb ORDER BY id", code: "42702"},
			outcome{sql: "SELECT ja.id, jb.id FROM ja, jb ORDER BY id", code: "42702"},
			outcome{sql: "SELECT ja.id AS k, jb.id AS k FROM ja, jb ORDER BY k", code: "42702"},
			outcome{sql: "SELECT ja.id AS k, ja.id AS k FROM ja, jb ORDER BY k"},
			outcome{sql: "SELECT ja.id AS k, id AS k FROM ja ORDER BY k"},
			outcome{sql: "SELECT ja.id AS k, id AS k FROM ja JOIN jc USING (id) ORDER BY k"},
			outcome{sql: "SELECT ja.v FROM ja, jb ORDER BY id", code: "42702"},
			outcome{sql: "SELECT ja.id FROM ja, jb ORDER BY jb.id, ja.id"},
			outcome{sql: "SELECT ja.v AS w FROM ja JOIN jb ON ja.id = jb.aid ORDER BY w, jb.w"},
		)
	})

	t.Run("removes duplicate rows with DISTINCT as Postgres does", func(t *testing.T) {
		verify.Covers(t, "SELECT DISTINCT keeps one row of every group of rows that are equal as Postgres compares them, NULLs equal to each other and NaN equal to NaN, sorts by ORDER BY items that must be output columns (42P10 otherwise), and rejects a group of -0 and 0, whose output depends on the plan", "INV-PG-SUBSET", "UNS-DISTINCT-ZERO")
		s := newJoinSession(t)
		expect(t, s,
			outcome{sql: "SELECT DISTINCT v FROM ja"},
			outcome{sql: "SELECT DISTINCT v, s FROM ja ORDER BY v, s"},
			outcome{sql: "SELECT DISTINCT s FROM ja ORDER BY s DESC NULLS LAST"},
			outcome{sql: "SELECT DISTINCT * FROM ja ORDER BY id"},
			outcome{sql: "SELECT DISTINCT x.v FROM ja x ORDER BY v"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY ja.v"},
			outcome{sql: "SELECT DISTINCT v + 1 AS n FROM ja ORDER BY v + 1"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY 1"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY id", code: "42P10"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY v + 0", code: "42P10"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY 1 + 0", code: "42P10"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY nope", code: "42703"},
			outcome{sql: "SELECT DISTINCT ja.v FROM ja JOIN jb ON ja.id = jb.aid"},
			outcome{sql: "SELECT DISTINCT jb.aid, ja.s FROM jb LEFT JOIN ja ON ja.id = jb.aid"},
			outcome{sql: "SELECT DISTINCT 10 / jb.w FROM jb", code: "22012"},
			outcome{sql: "SELECT DISTINCT f FROM ja WHERE id IN (1, 4)"},
			outcome{sql: "SELECT DISTINCT f FROM ja WHERE id IN (2, 4)"},
			outcome{sql: "SELECT DISTINCT f FROM ja", rejected: true},
			outcome{sql: "SELECT DISTINCT f + 0 FROM ja"},
			outcome{sql: "SELECT DISTINCT v FROM ja ORDER BY v LIMIT 2"},
			outcome{sql: "SELECT DISTINCT v, id FROM ja ORDER BY id LIMIT 2"},
			outcome{sql: "SELECT DISTINCT v FROM ja LIMIT 2", rejected: true},
			outcome{sql: "SELECT DISTINCT 1 FROM ja"},
			outcome{sql: "SELECT DISTINCT 1"},
		)
	})
}

func TestJoinPlans(t *testing.T) {
	t.Run("rejects a join whose conditions fail on a row or pair that some plan evaluates them on, and accepts it otherwise", func(t *testing.T) {
		verify.Covers(t, "a condition of a join that can fail, in WHERE or ON, on one table or several, is evaluated on every row and combination of rows that a plan of Postgres could evaluate it on, also on its sides alone and with NULLs for the right side of a LEFT JOIN, and the statement is rejected when it fails on any, while an output expression that fails is reported when every failing row fails with the same class", "INV-PG-SUBSET", "UNS-PLAN-DEPENDENT-JOIN")
		s := newJoinSession(t)
		for _, e := range []struct {
			sql  string
			want planOutcome
		}{
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid WHERE 10 / jb.w > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid WHERE 10 / (jb.w + 1) > 0", succeeds},
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid WHERE jb.id = 10 AND 10 / jb.w > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jc ON ja.v = jc.v WHERE jc.id = 1 AND 10 / (jc.id - 2) > 0", succeeds},
			{"SELECT * FROM ja JOIN jc ON ja.v = jc.v WHERE 10 / (jc.id - 2) > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid AND ja.v / jb.w > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid AND 10 / (ja.v - 20) > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jb ON ja.id = jb.aid AND CASE WHEN jb.w <> 0 THEN ja.v / jb.w > 0 END", succeeds},
			{"SELECT * FROM ja JOIN jd ON ja.v = jd.v WHERE 10 / (ja.v - 10) > 0", dependsOnPlan},
			{"SELECT * FROM ja JOIN jd ON ja.v = jd.v WHERE 10 / (ja.v - 30) > 0", succeeds},
			{"SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid AND 10 / jb.w > 0", dependsOnPlan},
			{"SELECT * FROM ja LEFT JOIN jc ON ja.v = jc.v AND 10 / (jc.id - 2) > 0 AND jc.id = 1", succeeds},
			{"SELECT * FROM ja LEFT JOIN jc ON ja.v = jc.v AND 10 / (jc.id - 2) > 0", dependsOnPlan},
			{"SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid WHERE coalesce(10 / jb.w, 1) > 0", dependsOnPlan},
			{"SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid WHERE 1 / coalesce(jb.w, 1) > 0", dependsOnPlan},
			{"SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid WHERE 1 / coalesce(jb.w + 1, 0) > 0", dependsOnPlan},
			{"SELECT * FROM ja LEFT JOIN jb ON ja.id = jb.aid WHERE 1 / coalesce(jb.w + 1, 1) > 0", succeeds},
			{"SELECT * FROM ja JOIN jb ON ja.v + 1 = jb.w * 1", succeeds},
			{"SELECT * FROM ja JOIN jb ON ja.v = 10 / jb.w", dependsOnPlan},
			{"SELECT * FROM ja JOIN jb ON (ja.id = 1 AND jb.w = 1) OR (ja.id = 2 AND 10 / jb.w > 0)", dependsOnPlan},
			{"SELECT ja.v / jb.w FROM ja JOIN jb ON ja.id = jb.aid", fails},
			{"SELECT 10 / ja.f::bigint FROM ja JOIN jb ON ja.id = jb.aid", fails},
			{"SELECT ja.id, 10 / (jb.w - 1) FROM ja JOIN jb ON ja.id = jb.aid ORDER BY 10 / jb.w", fails},
			{"SELECT ja.id, 10 / (jb.w - 1) FROM ja JOIN jb ON ja.id = jb.aid ORDER BY jb.w * 9223372036854775807", dependsOnPlan},
			{"SELECT ja.id FROM ja JOIN jb ON ja.id = jb.aid ORDER BY 10 / (jb.w - 2)", fails},
			{"SELECT CAST(jb.s AS bigint) FROM ja JOIN jb ON ja.id = jb.aid", fails},
			{"SELECT CAST(jb.s AS bigint), 10 / jb.w FROM ja JOIN jb ON ja.id = jb.aid", fails},
			{"SELECT 10 / jb.w, CAST(jb.s AS bigint) FROM ja JOIN jb ON ja.id = jb.aid", dependsOnPlan},
			{"SELECT * FROM ja, jb WHERE ja.id = jb.aid AND jb.w * 9223372036854775807 > 0", dependsOnPlan},
			{"SELECT * FROM ja, jb WHERE ja.id = jb.aid AND jb.w * 9223372036854775807 > 0 AND jb.w < 2", dependsOnPlan},
			{"SELECT * FROM ja, jc WHERE ja.v = jc.v AND jc.id * 9223372036854775807 > 0 AND jc.id = 1", succeeds},
		} {
			var rep pgdiff.Report
			o, err := s.Exec(t.Context(), e.sql, &rep)
			is.NotError(t, err, e.sql)
			switch e.want {
			case dependsOnPlan:
				is.True(t, !o.Executed, fmt.Sprintf("%s: rejected as plan-dependent, got %+v", e.sql, o))
			default:
				is.True(t, o.Executed, fmt.Sprintf("%s: rejected: %s", e.sql, o.Rejection))
				is.Equal(t, e.want == fails, o.Err != nil, fmt.Sprintf("%s: fails on both: %v", e.sql, o.Err))
			}
		}
	})

	t.Run("Postgres pushes conditions down, picks the join method by cost, and evaluates conditions on rows and pairs that depend on it", func(t *testing.T) {
		verify.Covers(t, "EXPLAIN and the outcomes on Postgres 18 with sequential scans off show that a condition on one table goes into the scan of that table, also on the right side of a LEFT JOIN, that a strict WHERE condition on that side makes the join inner, that equalities derive conditions on other tables, that the join method follows enable_nestloop, enable_hashjoin, and enable_mergejoin, and that the same statement fails with one method and succeeds with another, also when one side is empty", "INV-PG-SUBSET")
		b := newBoth(t, "")
		for _, q := range []string{
			"DROP TABLE IF EXISTS dev_b, dev_e",
			"CREATE TABLE dev (id bigint PRIMARY KEY, x bigint, z bigint)",
			"CREATE TABLE dev_b (id bigint PRIMARY KEY, aid bigint, y bigint, w bigint)",
			"CREATE TABLE dev_e (id bigint PRIMARY KEY, v bigint)",
			"CREATE INDEX dev_b_aid ON dev_b (aid)",
			"INSERT INTO dev VALUES (1, 1, 1), (2, 2, 1)",
			"INSERT INTO dev_b VALUES (10, 1, 1, 1), (11, 9, 5, 0)",
		} {
			_, err := b.pg.ExecContext(t.Context(), q)
			is.NotError(t, err, q)
		}
		conn, err := b.pg.Conn(t.Context())
		is.NotError(t, err)
		defer func() { _ = conn.Close() }()
		run := func(sql string, settings ...string) (string, error) {
			t.Helper()
			for _, s := range []string{"enable_nestloop", "enable_hashjoin", "enable_mergejoin"} {
				_, err := conn.ExecContext(t.Context(), "RESET "+s)
				is.NotError(t, err)
			}
			for _, s := range settings {
				_, err := conn.ExecContext(t.Context(), "SET "+s+" = off")
				is.NotError(t, err)
			}
			rows, err := conn.QueryContext(t.Context(), "EXPLAIN (COSTS OFF) "+sql)
			is.NotError(t, err)
			var lines []string
			for rows.Next() {
				var line string
				is.NotError(t, rows.Scan(&line))
				lines = append(lines, strings.TrimSpace(line))
			}
			is.NotError(t, rows.Close())
			_, qerr := conn.ExecContext(t.Context(), sql)
			return strings.Join(lines, "\n"), qerr
		}
		failing := "SELECT * FROM dev JOIN dev_b ON dev.id = dev_b.aid WHERE 10 / dev_b.w > 0"
		plan, err := run(failing)
		is.True(t, strings.Contains(plan, "Filter: ((10 / w) > 0)") && strings.Contains(plan, "Merge Join"), "the condition is in the scan of dev_b:\n"+plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "a merge join reads every row of dev_b")
		plan, err = run(failing, "enable_mergejoin", "enable_hashjoin")
		is.True(t, strings.Contains(plan, "Nested Loop"), plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "a nested loop over dev_b reads every row of dev_b")

		// The equality costs more than the division, so that a nested loop evaluates the division
		// first.
		filter := "SELECT * FROM dev JOIN dev_b ON dev.x + 0 = dev_b.y + 0 AND dev.z / dev_b.w > 0"
		plan, err = run(filter, "enable_mergejoin", "enable_nestloop")
		is.True(t, strings.Contains(plan, "Hash Join") && strings.Contains(plan, "Join Filter: ((dev.z / dev_b.w) > 0)"), plan)
		is.NotError(t, err, "a hash join evaluates the join filter only on the pairs with equal keys")
		plan, err = run(filter, "enable_mergejoin", "enable_hashjoin")
		is.True(t, strings.Contains(plan, "Nested Loop") && strings.Contains(plan, "Join Filter"), plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "a nested loop evaluates the join filter on every pair:\n"+plan)

		empty := "SELECT * FROM dev JOIN dev_e ON dev.x = dev_e.v WHERE 10 / (dev.z - 1) > 0"
		_, err = run(empty)
		is.NotError(t, err, "with an empty side, Postgres need not scan the other")
		_, err = run("SELECT * FROM dev WHERE 10 / (dev.z - 1) > 0")
		is.Equal(t, "22012", pgdiff.SQLState(err), "the condition fails on every row of dev")

		plan, _ = run("SELECT * FROM dev LEFT JOIN dev_b ON dev.id = dev_b.aid AND 10 / dev_b.y > 0")
		is.True(t, strings.Contains(plan, "Left Join") && strings.Contains(plan, "Filter: ((10 / y) > 0)"), "a condition on the right side goes into its scan:\n"+plan)
		plan, _ = run("SELECT * FROM dev LEFT JOIN dev_b ON dev.id = dev_b.aid WHERE dev_b.y + 1 > 0")
		is.True(t, !strings.Contains(plan, "Left") && strings.Contains(plan, "Filter: ((y + 1) > 0)"), "a strict condition on the right side makes the join inner:\n"+plan)
		plan, _ = run("SELECT * FROM dev LEFT JOIN dev_b ON dev.id = dev_b.aid WHERE dev_b.y IS NULL")
		is.True(t, strings.Contains(plan, "Left Join"), "IS NULL keeps the join a left join:\n"+plan)
		plan, _ = run("SELECT * FROM dev JOIN dev_b ON dev.id = dev_b.aid WHERE dev.id = 1")
		is.True(t, strings.Contains(plan, "(aid = 1)"), "the equalities derive a condition on dev_b:\n"+plan)
	})
}
