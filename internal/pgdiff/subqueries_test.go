package pgdiff_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// subqueryTables are the tables of the subquery tests, which every test drops first.
var subqueryTables = []string{"sa", "sb", "sc", "sd"}

// newSubquerySession compares statements on fresh tables named in [subqueryTables]: sa has a
// row with NULLs, sb references sa and has an index on the reference, with a row whose reference
// is NULL and one whose w is 0, sc has the column names of sa, and sd is empty.
func newSubquerySession(t *testing.T) *pgdiff.Session {
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
	for _, table := range subqueryTables {
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table+" CASCADE")
		is.NotError(t, err)
	}
	s := pgdiff.NewSession(pgConn, mConn)
	expect(t, s,
		outcome{sql: "CREATE TABLE sa (id bigint PRIMARY KEY, v bigint, s text, f double precision)"},
		outcome{sql: "CREATE TABLE sb (id bigint PRIMARY KEY, aid bigint REFERENCES sa, w bigint NOT NULL, s text)"},
		outcome{sql: "CREATE INDEX sb_aid ON sb (aid)"},
		outcome{sql: "CREATE TABLE sc (id bigint PRIMARY KEY, v bigint, t text)"},
		outcome{sql: "CREATE TABLE sd (id bigint PRIMARY KEY, v bigint)"},
		outcome{sql: "INSERT INTO sa VALUES (1, 10, 'a', 0.5), (2, 20, 'b', -0.5), (3, NULL, NULL, NULL), (4, 10, 'b', 2)"},
		outcome{sql: "INSERT INTO sb VALUES (10, 1, 1, 'x'), (11, 1, 2, NULL), (12, 2, 0, 'y'), (13, NULL, 3, 'z')"},
		outcome{sql: "INSERT INTO sc VALUES (1, 10, 'p'), (2, NULL, 'q'), (5, 30, 'r')"},
	)
	return s
}

func TestSubqueries(t *testing.T) {
	t.Run("evaluates IN, NOT IN, EXISTS, NOT EXISTS, and scalar subqueries as Postgres does, correlated or not, with their NULL rules", func(t *testing.T) {
		verify.Covers(t, "IN, NOT IN with NULLs on either side, EXISTS, NOT EXISTS, and scalar subqueries, which are NULL without a row, give Postgres's rows in WHERE, the output, ORDER BY, HAVING, and the conditions of joins, correlated with the query around or not, at any depth", "INV-PG-SUBSET")
		s := newSubquerySession(t)
		expect(t, s,
			outcome{sql: "SELECT id FROM sa WHERE id IN (SELECT aid FROM sb) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa WHERE id NOT IN (SELECT aid FROM sb) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa WHERE id NOT IN (SELECT aid FROM sb WHERE aid IS NOT NULL) ORDER BY id"},
			outcome{sql: "SELECT id, v IN (SELECT v FROM sc), v NOT IN (SELECT v FROM sc), v IN (SELECT v FROM sd) FROM sa ORDER BY id"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE sb.aid = sa.id) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa WHERE NOT EXISTS (SELECT 1 FROM sb WHERE sb.aid = sa.id) ORDER BY id"},
			outcome{sql: "SELECT id, (SELECT count(*) FROM sb WHERE sb.aid = sa.id) AS n, (SELECT max(w) FROM sb WHERE aid = sa.id) FROM sa ORDER BY id"},
			outcome{sql: "SELECT id, (SELECT s FROM sc WHERE sc.id = sa.id) FROM sa ORDER BY id"},
			outcome{sql: "SELECT (SELECT 1), (SELECT 'a'), (SELECT NULL), (SELECT v FROM sd)"},
			outcome{sql: "SELECT id FROM sa WHERE v > (SELECT avg(v) FROM sa) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa x WHERE v = (SELECT max(v) FROM sa WHERE sa.s = x.s) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE sb.aid = sa.id AND EXISTS (SELECT 1 FROM sc WHERE sc.id = sa.id AND sc.v = sa.v)) ORDER BY id"},
			outcome{sql: "SELECT id FROM sa ORDER BY (SELECT count(*) FROM sb WHERE sb.aid = sa.id), id"},
			outcome{sql: "SELECT s, count(*) FROM sa GROUP BY s HAVING count(*) > (SELECT count(*) FROM sd) ORDER BY s"},
			outcome{sql: "SELECT s, (SELECT count(*) FROM sc WHERE sc.t > sa.s) FROM sa GROUP BY s ORDER BY s"},
			outcome{sql: "SELECT sa.id, sb.id FROM sa JOIN sb ON sb.aid = sa.id AND EXISTS (SELECT 1 FROM sc WHERE sc.id = sa.id) ORDER BY 1, 2"},
			outcome{sql: "SELECT sa.id, sb.id FROM sa LEFT JOIN sb ON sb.aid = sa.id WHERE sb.id IS NULL OR sb.w IN (SELECT v FROM sc) ORDER BY 1, 2"},
			outcome{sql: "SELECT id FROM sa WHERE (SELECT 1 FROM sd) IS NULL ORDER BY id"},
			outcome{sql: "SELECT 1 IN (SELECT 1), NULL IN (SELECT 1), NULL IN (SELECT 1 WHERE false), 2 NOT IN (SELECT NULL::bigint), EXISTS (SELECT NULL)"},
		)
	})

	t.Run("fails a scalar subquery with more than one row with 21000, and a subquery with the wrong number of columns as Postgres does", func(t *testing.T) {
		verify.Covers(t, "a scalar subquery with more than one row fails with 21000 where every plan evaluates it, and a subquery with several columns fails with 42601", "INV-PG-SUBSET", "AC-4.3")
		s := newSubquerySession(t)
		expect(t, s,
			outcome{sql: "SELECT (SELECT id FROM sb)", code: "21000"},
			outcome{sql: "SELECT id, (SELECT id FROM sb WHERE sb.aid = sa.id) FROM sa ORDER BY 1", code: "21000"},
			outcome{sql: "SELECT id, (SELECT id FROM sb WHERE sb.aid = sa.id) FROM sa ORDER BY id", code: "42702"},
			outcome{sql: "SELECT (SELECT id, w FROM sb)", code: "42601"},
			outcome{sql: "SELECT 1 IN (SELECT id, w FROM sb)", code: "42601"},
			outcome{sql: "SELECT (SELECT nope FROM sb)", code: "42703"},
			outcome{sql: "SELECT id FROM sa WHERE id IN (SELECT s FROM sb)", code: "42883"},
		)
	})
}

func TestSubqueryNames(t *testing.T) {
	t.Run("resolves names in the query of a subquery first and then in the queries around it, as Postgres does, with the same errors", func(t *testing.T) {
		verify.Covers(t, "a name in a subquery resolves in its own FROM first and then in the queries around it, from the nearest out, a qualified name by the nearest table of that name, with 42702 for an ambiguous name at the nearest level that has it, 42703 and 42P01 when no level has it, and 42803 for an ungrouped column of a grouped query around", "INV-PG-SUBSET", "AC-4.3")
		s := newSubquerySession(t)
		expect(t, s,
			outcome{sql: "SELECT id, (SELECT v FROM sc WHERE sc.id = sa.id) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT id, (SELECT s FROM sc WHERE sc.id = sa.id) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT id, (SELECT t FROM sc WHERE sc.v = v) FROM sa ORDER BY 1", code: "21000"},
			outcome{sql: "SELECT id, (SELECT max(t) FROM sc WHERE sc.v = f) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT x.id, (SELECT count(*) FROM sa WHERE sa.id < x.id) FROM sa x ORDER BY 1"},
			outcome{sql: "SELECT sa.id, (SELECT count(*) FROM sa x WHERE x.id < sa.id) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT id, (SELECT (SELECT count(*) FROM sd WHERE sd.v = sa.v) FROM sc WHERE sc.id = sa.id) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb, sc WHERE sb.id = id)", code: "42702"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE nope = 1)", code: "42703"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE nope.id = 1)", code: "42P01"},
			outcome{sql: "SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE sa.nope = 1)", code: "42703"},
			outcome{sql: "SELECT id FROM sa x WHERE EXISTS (SELECT 1 FROM sb WHERE sa.id = 1)", code: "42P01"},
			outcome{sql: "SELECT s, (SELECT count(*) FROM sc WHERE sc.v = sa.v) FROM sa GROUP BY s", code: "42803"},
			outcome{sql: "SELECT s, (SELECT count(*) FROM sc WHERE sc.t = sa.s) FROM sa GROUP BY s ORDER BY 1"},
			outcome{sql: "SELECT (SELECT max(sc.v) + sa.v FROM sc) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT (SELECT sum(sc.v + sa.v) FROM sc) FROM sa ORDER BY 1"},
			outcome{sql: "SELECT (SELECT sa FROM sc)", code: "42703"},
			outcome{sql: "SELECT (SELECT id FROM sc), id FROM sa", code: "21000"},
		)
	})
}

func TestSubqueryWrites(t *testing.T) {
	t.Run("evaluates subqueries in UPDATE, DELETE, and INSERT on the snapshot of the statement, also on the table they write", func(t *testing.T) {
		verify.Covers(t, "subqueries in the WHERE of UPDATE and DELETE, in SET, and in INSERT ... VALUES read the rows as the statement began, also of the table it writes, as in Postgres", "INV-PG-SUBSET")
		s := newSubquerySession(t)
		expect(t, s,
			outcome{sql: "UPDATE sa SET v = (SELECT count(*) FROM sb WHERE sb.aid = sa.id)"},
			outcome{sql: "SELECT * FROM sa ORDER BY id"},
			outcome{sql: "UPDATE sa SET v = (SELECT x.v FROM sa x WHERE x.id = sa.id + 1)"},
			outcome{sql: "SELECT * FROM sa ORDER BY id"},
			outcome{sql: "UPDATE sa SET v = (SELECT coalesce(x.v, 0) + sa.id FROM sa x WHERE x.id = 1)"},
			outcome{sql: "SELECT * FROM sa ORDER BY id"},
			outcome{sql: "UPDATE sa SET v = (SELECT max(v) FROM sa) + 1 WHERE id IN (SELECT aid FROM sb)"},
			outcome{sql: "SELECT * FROM sa ORDER BY id"},
			outcome{sql: "INSERT INTO sc VALUES ((SELECT max(id) FROM sc) + 1, (SELECT count(*) FROM sc), 'n'), ((SELECT max(id) FROM sc) + 2, (SELECT count(*) FROM sc), 'm')"},
			outcome{sql: "SELECT * FROM sc ORDER BY id"},
			outcome{sql: "INSERT INTO sc VALUES ((SELECT id FROM sc), 1, 'x')", code: "21000"},
			outcome{sql: "DELETE FROM sb WHERE NOT EXISTS (SELECT 1 FROM sa WHERE sa.id = sb.aid AND sa.v > 1)"},
			outcome{sql: "SELECT * FROM sb ORDER BY id"},
			outcome{sql: "DELETE FROM sc WHERE id NOT IN (SELECT x.id FROM sc x WHERE x.v > 0)"},
			outcome{sql: "SELECT * FROM sc ORDER BY id"},
			outcome{sql: "UPDATE sc SET v = (SELECT id FROM sa)", code: "21000"},
			// Postgres folds the SET expressions in the order of their columns.
			outcome{sql: "UPDATE sc SET t = CAST(CAST('' AS text) AS bigint)::text, v = CAST(CAST('NaN' AS double precision) AS bigint)", code: "22003"},
		)
	})
}

func TestCTEs(t *testing.T) {
	t.Run("evaluates subqueries in FROM and CTEs as Postgres does, with their names and errors", func(t *testing.T) {
		verify.Covers(t, "subqueries in FROM with an alias and names for their columns, and non-recursive CTEs, several of them, a later one reading an earlier one, with names for their columns, read several times or never, and in subqueries, give Postgres's rows, names, and errors, such as 42712 for a CTE named twice and 42P10 for too many column names", "INV-PG-SUBSET", "AC-4.3")
		s := newSubquerySession(t)
		expect(t, s,
			outcome{sql: "SELECT * FROM (SELECT id, v FROM sa WHERE v > 5) AS x ORDER BY id"},
			outcome{sql: "SELECT x.a, b FROM (SELECT id, v FROM sa) x (a, b) WHERE x.b = 10 ORDER BY 1"},
			outcome{sql: "SELECT x.k, count(*) FROM (SELECT s AS k FROM sa) x GROUP BY x.k ORDER BY 1"},
			outcome{sql: "SELECT x.id, sb.id FROM (SELECT id FROM sa) x JOIN sb ON sb.aid = x.id ORDER BY 1, 2"},
			outcome{sql: "SELECT * FROM (SELECT s, count(*) AS n FROM sa GROUP BY s) x WHERE x.n > 1 ORDER BY 1"},
			outcome{sql: "SELECT * FROM (SELECT 1 AS a, 'x' AS b) x"},
			outcome{sql: "SELECT * FROM (SELECT 1 AS a, 2 AS a) x", rejected: true},
			outcome{sql: "SELECT * FROM (SELECT 1, 2) x (a, a)", rejected: true},
			outcome{sql: "WITH c (a, a) AS (SELECT 1, 2) SELECT * FROM c", rejected: true},
			outcome{sql: "SELECT * FROM (SELECT 1) x (a, b)", code: "42P10"},
			outcome{sql: "SELECT * FROM (SELECT id FROM sa) x, (SELECT id FROM sb) x", code: "42712"},
			outcome{sql: "SELECT * FROM (SELECT id FROM sa) x WHERE id > (SELECT min(id) FROM x)", code: "42P01"},
			outcome{sql: "SELECT * FROM sa x WHERE x.id IN (SELECT y.id FROM (SELECT id FROM sb WHERE sb.aid = x.id) y) ORDER BY 1"},
			outcome{sql: "WITH c AS (SELECT id, v FROM sa WHERE v IS NOT NULL) SELECT * FROM c ORDER BY id"},
			outcome{sql: "WITH c (a, b) AS (SELECT id, v FROM sa), d AS (SELECT a FROM c WHERE b = 10) SELECT * FROM d ORDER BY a"},
			outcome{sql: "WITH c AS (SELECT id FROM sa) SELECT * FROM c x, c y WHERE x.id = y.id ORDER BY 1"},
			outcome{sql: "WITH c AS (SELECT id FROM sa) SELECT 1"},
			outcome{sql: "WITH c AS (SELECT 1 / 0) SELECT 1", rejected: true},
			outcome{sql: "WITH sb AS (SELECT 5 AS id) SELECT * FROM sb"},
			outcome{sql: "WITH c AS (SELECT id FROM sa) SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM c WHERE c.id = sa.id + 1) ORDER BY 1"},
			outcome{sql: "SELECT (WITH c AS (SELECT sa.v AS x) SELECT x FROM c) FROM sa ORDER BY 1"},
			outcome{sql: "WITH c AS (SELECT 1), c AS (SELECT 2) SELECT 1", code: "42712"},
			outcome{sql: "WITH c (a, b) AS (SELECT 1) SELECT 1", code: "42P10"},
			outcome{sql: "WITH c AS (SELECT * FROM d), d AS (SELECT 1) SELECT * FROM c", code: "42P01"},
			outcome{sql: "WITH c AS (SELECT nope) SELECT 1", code: "42703"},
		)
	})
}

func TestSubqueryPlans(t *testing.T) {
	t.Run("rejects a subquery whose evaluation fails where some plan evaluates it and another does not, and accepts it otherwise", func(t *testing.T) {
		verify.Covers(t, "a subquery that can fail is evaluated on every row and in every place that a plan of Postgres could evaluate it, an uncorrelated one in a condition before any row, also one whose references to the query around fold away, the conditions of a correlated EXISTS also without a row of the query around, and a subquery in FROM or a CTE before the statement reads any row, and the statement is rejected when the evaluation fails, while a scalar subquery with more than one row fails with 21000 where every plan evaluates it", "INV-PG-SUBSET", "UNS-PLAN-DEPENDENT-SUBQUERY")
		s := newSubquerySession(t)
		for _, e := range []struct {
			sql  string
			want planOutcome
		}{
			{"SELECT id FROM sa WHERE id IN (SELECT aid FROM sb WHERE 10 / w > 0)", dependsOnPlan},
			{"SELECT id FROM sa WHERE id IN (SELECT aid FROM sb WHERE 10 / (w + 1) > 0)", succeeds},
			{"SELECT id FROM sa WHERE v NOT IN (SELECT 10 / w FROM sb)", dependsOnPlan},
			{"SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE sb.aid = sa.id AND 10 / sb.w > 0)", dependsOnPlan},
			{"SELECT id FROM sd WHERE EXISTS (SELECT 1 FROM sb WHERE sb.aid = sd.id AND 10 / sb.w > 0)", dependsOnPlan},
			{"SELECT id FROM sa WHERE EXISTS (SELECT 1 FROM sb WHERE sb.aid = sa.id AND 10 / (sb.w + 1) > 0)", succeeds},
			{"SELECT id FROM sa WHERE id IN (SELECT aid FROM sb WHERE sb.w = sa.v AND 10 / (sb.w + 1) > 0)", dependsOnPlan},
			{"SELECT id FROM sa WHERE id = (SELECT aid FROM sb)", dependsOnPlan},
			{"SELECT id FROM sd WHERE v = (SELECT aid FROM sb)", dependsOnPlan},
			{"SELECT (SELECT aid FROM sb) FROM sd", succeeds},
			{"SELECT id, (SELECT aid FROM sb) FROM sa", fails},
			{"SELECT (SELECT 10 / w FROM sb WHERE id = 12)", dependsOnPlan},
			{"SELECT id FROM sa WHERE v > 10 AND (SELECT count(*) FROM sb WHERE sb.aid = sa.id) > 0", succeeds},
			{"SELECT id FROM sa WHERE (SELECT sb.id FROM sb WHERE sb.aid = sa.id) > 0", fails},
			{"SELECT id FROM sa WHERE id <> 1 AND (SELECT sb.id FROM sb WHERE sb.aid = sa.id) > 0", dependsOnPlan},
			{"SELECT id FROM sa WHERE id > 1 AND (SELECT sb.id FROM sb WHERE sb.aid = sa.id) > 0", succeeds},
			{"SELECT sa.id FROM sa JOIN sc ON sa.id = sc.id WHERE (SELECT sb.id FROM sb WHERE sb.aid = sa.id) > 0", dependsOnPlan},
			{"SELECT sa.id FROM sa JOIN sc ON sa.id = sc.id WHERE (SELECT count(*) FROM sb WHERE sb.aid = sa.id) > 0", succeeds},
			{"SELECT * FROM (SELECT id, 10 / w AS q FROM sb WHERE w <> 0) x WHERE q > 1", dependsOnPlan},
			{"SELECT q FROM (SELECT id, 10 / w AS q FROM sb WHERE w <> 0) x", succeeds},
			{"SELECT * FROM (SELECT id FROM sb WHERE 10 / w > 0) x", dependsOnPlan},
			{"SELECT * FROM (SELECT * FROM sb) x WHERE 10 / x.w > 0", dependsOnPlan},
			{"SELECT * FROM (SELECT * FROM sb) x WHERE x.w > 0", succeeds},
			{"WITH c AS (SELECT 10 / w AS q FROM sb) SELECT count(*) FROM c, c c2", dependsOnPlan},
			{"WITH c AS (SELECT 10 / w AS q FROM sb) SELECT count(*) FROM c", dependsOnPlan},
			{"WITH c AS (SELECT 10 / w AS q FROM sb) SELECT 1", succeeds},
			{"SELECT EXISTS (SELECT 10 / w FROM sb)", succeeds},
			{"SELECT EXISTS (SELECT count(10 / w) FROM sb)", dependsOnPlan},
			{"UPDATE sa SET v = 1 WHERE id = 1 AND EXISTS (SELECT 1 FROM sb WHERE 10 / w > 0)", dependsOnPlan},
			{"SELECT id, (SELECT count(*) FROM sb WHERE sb.aid = sa.id) FROM sa ORDER BY id LIMIT 2", succeeds},
			{"SELECT id, (SELECT sb.id FROM sb WHERE sb.aid = sa.id) FROM sa ORDER BY 1 LIMIT 1", dependsOnPlan},
			// A subquery whose references to the query around fold away is uncorrelated, and
			// evaluated before any row, also of an empty table.
			{"SELECT id FROM sd WHERE (SELECT max(10 / w) FROM sb WHERE sb.aid = sd.id) > 0", succeeds},
			{"SELECT id FROM sd WHERE (SELECT max(10 / w) FROM sb WHERE sb.aid = sd.id OR TRUE) > 0", dependsOnPlan},
			{"SELECT id FROM sd WHERE (SELECT max(10 / w) FROM sb WHERE sb.aid = sd.id AND CAST('off' AS boolean)) > 0", succeeds},
			{"SELECT id FROM sd WHERE (SELECT max(CASE WHEN false THEN sd.v ELSE 10 / w END) FROM sb) > 0", dependsOnPlan},
			{"SELECT id FROM sd WHERE (SELECT max(coalesce(1, sd.v) + 10 / w) FROM sb) > 0", dependsOnPlan},
			{"SELECT id FROM sd WHERE (SELECT max(10 / w) FROM sb WHERE sd.v > 0 AND (SELECT 1 FROM sc WHERE sc.id = sd.id AND false) IS NULL) > 0", succeeds},
			{"SELECT id FROM sd WHERE (SELECT CASE WHEN (SELECT FALSE FROM sc WHERE sc.id = sd.id AND CAST('off' AS boolean)) THEN 1 END FROM sb WHERE 10 / w > 0) IS NULL", dependsOnPlan},
			{"DELETE FROM sd WHERE (SELECT CASE WHEN (SELECT FALSE FROM sc WHERE sc.id = sd.id AND CAST('off' AS boolean)) THEN 1 END FROM sb WHERE 10 / w > 0) IS NULL", dependsOnPlan},
			{"SELECT id FROM sa WHERE (SELECT max(w) FROM sb WHERE sb.aid = sa.id OR TRUE) > 1 ORDER BY id", succeeds},
			{"SELECT id FROM sa WHERE (SELECT max(w) + count(*) FROM sb WHERE sb.aid = sa.id AND FALSE) IS NULL ORDER BY id", succeeds},
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

	t.Run("Postgres evaluates a subquery lazily, before a scan, as a semi-join, or not at all, and stops at the row that decides it", func(t *testing.T) {
		verify.Covers(t, "EXPLAIN and the outcomes on Postgres 18 with sequential scans off show that an uncorrelated subquery in an index condition is evaluated before the scan, also of an empty table, and in a filter only when a row reaches it, that IN, also correlated, and EXISTS become semi-joins and NOT EXISTS an anti-join, which evaluate a condition of the query around on rows that the subquery rejects, that NOT IN is a hashed SubPlan, that EXISTS drops the output of its query, that a subquery in FROM and a CTE read once are merged into the query around and their unread outputs never evaluated, that a CTE read twice is evaluated as a whole, that an unread CTE is never evaluated, that a scalar subquery stops at its second row, and that a subquery whose references to the query around fold away, also in a subquery in it, is an uncorrelated InitPlan, evaluated before the scan of an empty table", "INV-PG-SUBSET")
		b := newBoth(t, "")
		for _, q := range []string{
			"DROP TABLE IF EXISTS dev_e, dev_two, dev_t, dev_u",
			"CREATE TABLE dev_e (id bigint PRIMARY KEY, v bigint)",
			"CREATE TABLE dev_two (id bigint PRIMARY KEY, x bigint, w bigint)",
			"INSERT INTO dev_two VALUES (1, 1, 1), (2, 2, 1), (3, 3, 0)",
			"CREATE TABLE dev_t (id bigint PRIMARY KEY, a bigint, w bigint)",
			"CREATE INDEX dev_t_a ON dev_t (a)",
			"INSERT INTO dev_t VALUES (1, 1, 1), (2, 2, 0), (3, 3, 1)",
			"INSERT INTO dev_t SELECT g, g, 1 FROM generate_series(4, 1000) g",
			"CREATE TABLE dev_u (id bigint PRIMARY KEY, b bigint, w bigint)",
			"CREATE INDEX dev_u_b ON dev_u (b)",
			"INSERT INTO dev_u VALUES (1, 1, 1), (2, 5, 0)",
			"ANALYZE dev_e, dev_two, dev_t, dev_u",
		} {
			_, err := b.pg.ExecContext(t.Context(), q)
			is.NotError(t, err, q)
		}
		t.Cleanup(func() { _, _ = b.pg.ExecContext(t.Context(), "DROP TABLE IF EXISTS dev_e, dev_two, dev_t, dev_u") })
		conn, err := b.pg.Conn(t.Context())
		is.NotError(t, err)
		defer func() { _ = conn.Close() }()
		run := func(sql string) (string, error) {
			t.Helper()
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
		plan, err := run("SELECT * FROM dev_e WHERE id = (SELECT x FROM dev_two)")
		is.True(t, strings.Contains(plan, "Index Cond: (id = (InitPlan 1).col1)"), plan)
		is.Equal(t, "21000", pgdiff.SQLState(err), "the index scan evaluates the subquery before it reads the empty table")
		plan, err = run("SELECT * FROM dev_e WHERE v = (SELECT x FROM dev_two)")
		is.True(t, strings.Contains(plan, "Filter: (v = (InitPlan 1).col1)"), plan)
		is.NotError(t, err, "a filter evaluates the subquery only when a row reaches it")

		plan, err = run("SELECT id FROM dev_e WHERE (SELECT max(10 / w) FROM dev_two WHERE dev_two.id = dev_e.id OR TRUE) > 0")
		is.True(t, strings.Contains(plan, "One-Time Filter") && strings.Contains(plan, "InitPlan") && !strings.Contains(plan, "SubPlan"), "folding drops the reference, and the subquery is uncorrelated:\n"+plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "the uncorrelated subquery is evaluated before the scan of the empty table")
		plan, err = run("SELECT id FROM dev_e WHERE (SELECT CASE WHEN (SELECT FALSE FROM dev_two x WHERE x.id = dev_e.id AND CAST('off' AS boolean)) THEN 1 END FROM dev_two WHERE id = 3 AND 10 / w > 0) IS NULL")
		is.True(t, strings.Contains(plan, "One-Time Filter") && !strings.Contains(plan, "SubPlan"), "also through a subquery in it:\n"+plan)
		is.Equal(t, "22012", pgdiff.SQLState(err))
		plan, err = run("SELECT id FROM dev_e WHERE (SELECT max(10 / w) FROM dev_two WHERE dev_two.id = dev_e.id) > 0")
		is.True(t, strings.Contains(plan, "SubPlan"), plan)
		is.NotError(t, err, "a correlated subquery is evaluated only on rows")

		plan, _ = run("SELECT * FROM dev_t WHERE a IN (SELECT b FROM dev_u)")
		is.True(t, strings.Contains(plan, "Semi Join"), plan)
		plan, _ = run("SELECT * FROM dev_t WHERE a IN (SELECT b FROM dev_u WHERE dev_u.w = dev_t.w)")
		is.True(t, strings.Contains(plan, "Semi Join") && !strings.Contains(plan, "SubPlan"), "Postgres 18 merges a correlated IN into a semi-join:\n"+plan)
		plan, _ = run("SELECT * FROM dev_t WHERE EXISTS (SELECT 1 FROM dev_u WHERE dev_u.b = dev_t.a)")
		is.True(t, strings.Contains(plan, "Semi Join"), plan)
		plan, _ = run("SELECT * FROM dev_t WHERE NOT EXISTS (SELECT 1 FROM dev_u WHERE dev_u.b = dev_t.a)")
		is.True(t, strings.Contains(plan, "Anti Join"), plan)
		plan, _ = run("SELECT * FROM dev_t WHERE a NOT IN (SELECT b FROM dev_u)")
		is.True(t, strings.Contains(plan, "hashed SubPlan"), plan)
		plan, _ = run("SELECT id FROM dev_t WHERE EXISTS (SELECT 1 FROM dev_u WHERE dev_u.w = 0)")
		is.True(t, strings.Contains(plan, "One-Time Filter: (InitPlan 1).col1"), "an uncorrelated EXISTS is evaluated once, before the scan:\n"+plan)

		plan, err = run("SELECT * FROM dev_t WHERE a IN (SELECT b FROM dev_u WHERE dev_u.id = 1) AND 10 / w > 0")
		is.True(t, strings.Contains(plan, "Join") && strings.Contains(plan, "Filter: ((10 / w) > 0)"), plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "the join evaluates the division on rows that the IN rejects")
		_, err = run("SELECT * FROM dev_t WHERE a = 1 AND 10 / w > 0")
		is.NotError(t, err, "the row that the IN accepts does not fail")

		_, err = run("SELECT EXISTS (SELECT 10 / w FROM dev_two)")
		is.NotError(t, err, "EXISTS drops the output of its query")
		_, err = run("SELECT EXISTS (SELECT count(10 / w) FROM dev_two)")
		is.Equal(t, "22012", pgdiff.SQLState(err), "but not of a query with aggregates")
		_, err = run("SELECT 1 FROM (SELECT 10 / w AS q FROM dev_two) s")
		is.NotError(t, err, "an output of a subquery in FROM that nothing reads is never evaluated")
		_, err = run("SELECT q FROM (SELECT 10 / w AS q FROM dev_two) s")
		is.Equal(t, "22012", pgdiff.SQLState(err))
		_, err = run("WITH c AS (SELECT 10 / w AS q FROM dev_two) SELECT count(*) FROM c")
		is.NotError(t, err, "a CTE read once is merged into the query")
		plan, err = run("WITH c AS (SELECT 10 / w AS q FROM dev_two) SELECT count(*) FROM c, c c2")
		is.True(t, strings.Contains(plan, "CTE Scan on c"), plan)
		is.Equal(t, "22012", pgdiff.SQLState(err), "a CTE read twice is evaluated as a whole")
		_, err = run("WITH c AS (SELECT 1 / 0) SELECT 1")
		is.NotError(t, err, "an unread CTE is never evaluated, nor folded")
		_, err = run("SELECT (SELECT 10 / w FROM dev_two ORDER BY id)")
		is.Equal(t, "21000", pgdiff.SQLState(err), "a scalar subquery stops at its second row, before the row that fails")
	})
}
