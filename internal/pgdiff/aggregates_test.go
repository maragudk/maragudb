package pgdiff_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

// aggTables are the tables of the aggregate tests, which every test drops first.
var aggTables = []string{"ga", "gb", "gc", "ge", "gf"}

// newAggSession compares statements on fresh tables named in [aggTables], with rows that every
// test of aggregates shares: ga has groups of several rows with NULLs, gb references ga, gc has
// a composite primary key, ge is empty, and gf has floats.
func newAggSession(t *testing.T) *pgdiff.Session {
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
	for _, table := range aggTables {
		_, err := pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+table+" CASCADE")
		is.NotError(t, err)
	}
	s := pgdiff.NewSession(pgConn, mConn)
	expect(t, s,
		outcome{sql: "CREATE TABLE ga (id bigint PRIMARY KEY, g bigint, v bigint, s text, b bytea, k boolean, ts timestamptz)"},
		outcome{sql: "CREATE TABLE gb (id bigint PRIMARY KEY, aid bigint REFERENCES ga, w bigint NOT NULL, s text)"},
		outcome{sql: "CREATE INDEX gb_aid ON gb (aid)"},
		outcome{sql: "CREATE TABLE gc (x bigint, y bigint, z text, PRIMARY KEY (x, y))"},
		outcome{sql: "CREATE TABLE ge (id bigint PRIMARY KEY, v bigint, f double precision)"},
		outcome{sql: "CREATE TABLE gf (id bigint PRIMARY KEY, g bigint, f double precision)"},
		outcome{sql: "INSERT INTO ga VALUES (1, 1, 10, 'a', '\\x01', TRUE, '2024-01-02 03:04:05+00'), (2, 1, 20, 'b', '\\x02', FALSE, '2024-01-03 00:00:00+00'), (3, 2, NULL, NULL, NULL, NULL, NULL), (4, 2, 5, 'B', '\\x00ff', TRUE, '2023-12-31 23:59:59.5+00'), (5, NULL, 7, 'é', '\\x', FALSE, '2024-01-01 00:00:00+00'), (6, 3, 9223372036854775807, 'zz', '\\xff', TRUE, '2024-06-01 00:00:00+00'), (7, 3, 9223372036854775807, 'z', '\\xfe', TRUE, '2024-06-01 00:00:00+00')"},
		outcome{sql: "INSERT INTO gb VALUES (10, 1, 1, 'x'), (11, 1, 2, NULL), (12, 2, 0, 'y'), (13, NULL, 3, 'z'), (14, 4, 4, 'x')"},
		outcome{sql: "INSERT INTO gc VALUES (1, 1, 'p'), (1, 2, 'q'), (2, 1, 'r')"},
		outcome{sql: "INSERT INTO gf VALUES (1, 1, 1.5), (2, 1, 2.25), (3, 2, 0.1), (4, 2, 0.2), (5, 3, 'Infinity'), (6, 3, 1), (7, 4, 'NaN'), (8, 4, 2), (9, 5, '-0'), (10, 5, '-0'), (11, 6, 'Infinity'), (12, 6, '-Infinity'), (13, 7, 1e308), (14, 7, 1e308), (15, 8, 4e15), (16, 8, 4e15), (17, 8, 1)"},
	)
	return s
}

func TestAggregates(t *testing.T) {
	t.Run("computes COUNT, SUM, AVG, MIN, and MAX as Postgres does, with numeric results for SUM and AVG of integers", func(t *testing.T) {
		verify.Covers(t, "COUNT(*), COUNT(x), SUM, AVG, MIN, and MAX, also with DISTINCT, over bigint, integer, double precision, text, bytea, timestamptz, and NULLs give Postgres's values and types: SUM of integer is bigint, SUM of bigint and AVG of integers are numeric with Postgres's scale, and MIN and MAX of text compare by byte order", "INV-PG-SUBSET", "INV-TYPES")
		s := newAggSession(t)
		expect(t, s,
			outcome{sql: "SELECT count(*), count(v), count(DISTINCT g), count(s), count(ALL k) FROM ga"},
			outcome{sql: "SELECT sum(v), avg(v), min(v), max(v) FROM ga WHERE id < 6"},
			outcome{sql: "SELECT sum(v), avg(v), sum(DISTINCT v), avg(DISTINCT v) FROM ga"},
			outcome{sql: "SELECT g, sum(v), avg(v), count(*) FROM ga GROUP BY g ORDER BY g"},
			outcome{sql: "SELECT min(s), max(s), min(b), max(b), min(ts), max(ts) FROM ga"},
			outcome{sql: "SELECT g, min(s), max(s) FROM ga GROUP BY g"},
			outcome{sql: "SELECT sum(length(s)), avg(length(s)), min(length(s)), max(1), sum(1), avg(1), count(1) FROM ga"},
			outcome{sql: "SELECT avg(id), avg(id * 3), avg(-id), sum(id) FROM ga"},
			outcome{sql: "SELECT count(*), sum(v), avg(v), min(v), max(f) FROM ge"},
			outcome{sql: "SELECT count(*) FROM ge GROUP BY v"},
			outcome{sql: "SELECT count(*)"},
			outcome{sql: "SELECT count(*) WHERE false"},
			outcome{sql: "SELECT sum(1), min('a'), max(NULL), count(NULL), count(1.5)"},
			outcome{sql: "SELECT count(DISTINCT 1.5), count(DISTINCT 'a'), count(DISTINCT NULL), min(DISTINCT 'b') FROM ga"},
			outcome{sql: "SELECT min(k) FROM ga", code: "42883"},
			outcome{sql: "SELECT sum(s) FROM ga", code: "42883"},
			outcome{sql: "SELECT avg(ts) FROM ga", code: "42883"},
			outcome{sql: "SELECT sum('1')", code: "42725"},
			outcome{sql: "SELECT avg(NULL)", code: "42725"},
			outcome{sql: "SELECT count(v, s) FROM ga", code: "42883"},
			outcome{sql: "SELECT sum(*) FROM ga", code: "42883"},
			outcome{sql: "SELECT lower(*) FROM ga", code: "42883"},
			outcome{sql: "SELECT lower(DISTINCT s) FROM ga", code: "42809"},
			outcome{sql: "SELECT abs(DISTINCT s) FROM ga", code: "42883"},
			outcome{sql: "SELECT count() FROM ga", code: "42809"},
			outcome{sql: "SELECT now(*)", code: "42809"},
			outcome{sql: "SELECT sum(1.5)", rejected: true},
			outcome{sql: "SELECT sum(v) + 1 FROM ga", rejected: true},
			outcome{sql: "SELECT -sum(v) FROM ga", rejected: true},
			outcome{sql: "SELECT sum(v) = '1' FROM ga", rejected: true},
		)
	})

	t.Run("compares, sorts, casts, and outputs numeric results as Postgres does", func(t *testing.T) {
		verify.Covers(t, "the numeric results of SUM and AVG of integers compare with each other, with integers, and with floats through Postgres's implicit casts, sort, take part in DISTINCT, IS NULL, CASE, COALESCE, NULLIF, and ||, and cast to bigint with rounding half away from zero and overflow errors, to double precision, and to text, while arithmetic on them is rejected", "INV-PG-SUBSET", "INV-TYPES")
		s := newAggSession(t)
		expect(t, s,
			outcome{sql: "SELECT g, avg(v) > 10, avg(v) = 15, sum(v) >= 30::bigint, avg(v) < 7.5::float8, sum(v) IN (30, 5), sum(v) BETWEEN 1 AND 100 FROM ga GROUP BY g"},
			outcome{sql: "SELECT g FROM ga GROUP BY g HAVING avg(v) > 6 ORDER BY g"},
			outcome{sql: "SELECT g, avg(v) FROM ga GROUP BY g ORDER BY avg(v) DESC NULLS LAST, g"},
			outcome{sql: "SELECT g, avg(v) FROM ga GROUP BY g ORDER BY 2, 1"},
			outcome{sql: "SELECT avg(v)::bigint, avg(v)::float8, avg(v)::text, sum(v)::text, avg(v) IS NULL FROM ga GROUP BY g"},
			outcome{sql: "SELECT sum(v)::bigint FROM ga", code: "22003"},
			outcome{sql: "SELECT avg(v)::bigint FROM ga WHERE g = 3"},
			outcome{sql: "SELECT coalesce(sum(v), 0), coalesce(avg(v), sum(v)), nullif(sum(v), 30), CASE WHEN g = 1 THEN sum(v) ELSE 0 END, 'x' || sum(v) FROM ga GROUP BY g"},
			outcome{sql: "SELECT coalesce(avg(v), 0.5::float8) FROM ga GROUP BY g"},
			outcome{sql: "SELECT DISTINCT avg(v) FROM ga GROUP BY g"},
			outcome{sql: "SELECT avg(v)::boolean FROM ga", code: "42846"},
			outcome{sql: "SELECT length(sum(v)) FROM ga", code: "42883"},
			outcome{sql: "SELECT sum(v) LIKE '1%' FROM ga", code: "42883"},
			outcome{sql: "SELECT avg(v) * 2 FROM ga", rejected: true},
			outcome{sql: "SELECT abs(avg(v)) FROM ga", rejected: true},
			outcome{sql: "SELECT avg(v) = 1.5 FROM ga", rejected: true},
		)
	})
}

func TestGroupBy(t *testing.T) {
	t.Run("groups by expressions, output positions, and output names, with Postgres's resolution and errors", func(t *testing.T) {
		verify.Covers(t, "GROUP BY of expressions, output positions, and names, where a name is a column of FROM before it is an output column's, HAVING with and without GROUP BY, aggregates in ORDER BY, and the grouping errors of Postgres: an ungrouped column (42803) unless the GROUP BY has the primary key of its table, nested aggregates and aggregates in WHERE, JOIN conditions, GROUP BY, LIMIT, VALUES, and UPDATE (42803), positions out of range (42P10), constants (42601), and ambiguous names (42702)", "INV-PG-SUBSET", "AC-4.3")
		s := newAggSession(t)
		expect(t, s,
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY 1 ORDER BY 1"},
			outcome{sql: "SELECT g AS n, count(*) FROM ga GROUP BY n ORDER BY n"},
			outcome{sql: "SELECT v AS g, count(*) FROM ga GROUP BY g", code: "42803"},
			outcome{sql: "SELECT g + 1, count(*) FROM ga GROUP BY g + 1"},
			outcome{sql: "SELECT (g + 1) * 2 FROM ga GROUP BY g + 1"},
			outcome{sql: "SELECT g + 1 FROM ga GROUP BY g"},
			outcome{sql: "SELECT g FROM ga GROUP BY g + 1", code: "42803"},
			outcome{sql: "SELECT 1 + g FROM ga GROUP BY g + 1", code: "42803"},
			outcome{sql: "SELECT v FROM ga GROUP BY g", code: "42803"},
			outcome{sql: "SELECT * FROM ga GROUP BY id"},
			outcome{sql: "SELECT ga.*, count(*) FROM ga GROUP BY ga.id"},
			outcome{sql: "SELECT x.s FROM ga x GROUP BY x.id"},
			outcome{sql: "SELECT z FROM gc GROUP BY x", code: "42803"},
			outcome{sql: "SELECT z FROM gc GROUP BY x, y"},
			outcome{sql: "SELECT z FROM gc GROUP BY y, x, z"},
			outcome{sql: "SELECT g, s FROM ga GROUP BY g, s ORDER BY g, s"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g, g"},
			outcome{sql: "SELECT s FROM ga GROUP BY 2", code: "42P10"},
			outcome{sql: "SELECT s FROM ga GROUP BY 0", code: "42P10"},
			outcome{sql: "SELECT s FROM ga GROUP BY 'x'", code: "42601"},
			outcome{sql: "SELECT s FROM ga GROUP BY NULL", code: "42601"},
			outcome{sql: "SELECT s FROM ga GROUP BY 1.5", code: "42601"},
			outcome{sql: "SELECT count(*) FROM ga GROUP BY 1", code: "42803"},
			outcome{sql: "SELECT g FROM ga GROUP BY count(*)", code: "42803"},
			outcome{sql: "SELECT g AS x, v AS x FROM ga GROUP BY x", code: "42702"},
			outcome{sql: "SELECT g AS x, g AS x FROM ga GROUP BY x"},
			outcome{sql: "SELECT g FROM ga GROUP BY nope", code: "42703"},
			outcome{sql: "SELECT count(count(*)) FROM ga", code: "42803"},
			outcome{sql: "SELECT g FROM ga WHERE count(*) > 1", code: "42803"},
			outcome{sql: "SELECT count(*) FROM ga LIMIT count(*)", code: "42803"},
			outcome{sql: "SELECT 1 FROM ga JOIN gb ON count(*) > 0", code: "42803"},
			outcome{sql: "INSERT INTO ge VALUES (count(*), 1)", code: "42803"},
			outcome{sql: "UPDATE ge SET v = count(*)", code: "42803"},
			outcome{sql: "DELETE FROM ge WHERE count(*) > 0", code: "42803"},
			outcome{sql: "SELECT g FROM ga GROUP BY g HAVING count(*) > 1"},
			outcome{sql: "SELECT g FROM ga GROUP BY g HAVING g > 1"},
			outcome{sql: "SELECT g FROM ga GROUP BY g HAVING v > 1", code: "42803"},
			outcome{sql: "SELECT count(*) FROM ga HAVING count(*) > 100"},
			outcome{sql: "SELECT count(*) FROM ga HAVING true"},
			outcome{sql: "SELECT 1 FROM ga HAVING count(*) > 1"},
			outcome{sql: "SELECT g FROM ga HAVING count(*) > 1", code: "42803"},
			outcome{sql: "SELECT count(*) FROM ga HAVING 1", code: "42804"},
			outcome{sql: "SELECT g FROM ga GROUP BY g ORDER BY count(*), g"},
			outcome{sql: "SELECT g FROM ga GROUP BY g ORDER BY v", code: "42803"},
			outcome{sql: "SELECT g FROM ga ORDER BY count(*)", code: "42803"},
			outcome{sql: "SELECT sum(v) FROM ga GROUP BY g ORDER BY sum(v)"},
			outcome{sql: "SELECT DISTINCT g FROM ga GROUP BY g, v"},
			outcome{sql: "SELECT DISTINCT g FROM ga GROUP BY FALSE, g ORDER BY v", code: "42601"},
			outcome{sql: "SELECT DISTINCT g FROM ga GROUP BY g, v ORDER BY v", code: "42P10"},
			outcome{sql: "SELECT DISTINCT count(*) FROM ga GROUP BY g ORDER BY count(*)"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g ORDER BY g LIMIT 2"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g ORDER BY 1 NULLS FIRST OFFSET 1"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g LIMIT 2", rejected: true},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g, s ORDER BY g LIMIT 2", rejected: true},
			outcome{sql: "SELECT count(*) FROM ga LIMIT 1"},
			outcome{sql: "SELECT g, count(*) FROM ga GROUP BY g ORDER BY g LIMIT 0"},
			outcome{sql: "SELECT g FROM ga GROUP BY ROLLUP (g)", rejected: true},
			outcome{sql: "SELECT count(*) FILTER (WHERE g > 1) FROM ga", rejected: true},
			outcome{sql: "SELECT string_agg(s, ',') FROM ga", rejected: true},
		)
	})

	t.Run("groups the rows of joins as Postgres does", func(t *testing.T) {
		verify.Covers(t, "GROUP BY and aggregates over inner and left joins and USING give Postgres's groups, and the GROUP BY of a table's primary key allows its other columns but not those of the other tables", "INV-PG-SUBSET")
		s := newAggSession(t)
		expect(t, s,
			outcome{sql: "SELECT ga.id, ga.s, count(gb.id), sum(gb.w) FROM ga LEFT JOIN gb ON ga.id = gb.aid GROUP BY ga.id ORDER BY ga.id"},
			outcome{sql: "SELECT ga.g, count(*), avg(gb.w) FROM ga JOIN gb ON ga.id = gb.aid GROUP BY ga.g"},
			outcome{sql: "SELECT gb.s, count(*) FROM ga JOIN gb ON ga.id = gb.aid GROUP BY ga.id", code: "42803"},
			outcome{sql: "SELECT id, count(*) FROM ga JOIN gb USING (id) GROUP BY id"},
			outcome{sql: "SELECT s, count(*) FROM ga JOIN gb USING (s) GROUP BY s"},
			outcome{sql: "SELECT ga.s FROM ga JOIN gb USING (s) GROUP BY s"},
			outcome{sql: "SELECT ga.s, gb.s FROM ga JOIN gb USING (s) GROUP BY s", code: "42803"},
			outcome{sql: "SELECT ga.s FROM ga LEFT JOIN gb USING (s) GROUP BY gb.s", code: "42803"},
			outcome{sql: "SELECT x.g, y.g, count(*) FROM ga x JOIN ga y ON x.id < y.id GROUP BY x.g, y.g"},
			outcome{sql: "SELECT count(*), count(gb.id), min(gb.s) FROM ga LEFT JOIN gb ON ga.id = gb.aid WHERE gb.id IS NULL"},
			outcome{sql: "SELECT ga.g, count(*) FROM ga, gb WHERE ga.id = gb.aid GROUP BY ga.g HAVING count(*) > 1"},
		)
	})
}

func TestAggregatePlans(t *testing.T) {
	t.Run("rejects an aggregate whose result or error depends on the plan, and accepts it otherwise", func(t *testing.T) {
		verify.Covers(t, "a SUM or AVG of double precision is accepted only when every order of adding gives the same result, with NaN and infinities, and its overflow is an error only when every order overflows; errors in the arguments of aggregates, in the GROUP BY expressions, and in HAVING are reported when every failing row or group fails with the same class; a HAVING condition without aggregates is a WHERE condition; and MIN and MAX that Postgres can compute from the first row of an index reject a WHERE condition that fails", "INV-PG-SUBSET", "UNS-PLAN-DEPENDENT-AGGREGATE")
		s := newAggSession(t)
		for _, e := range []struct {
			sql  string
			want planOutcome
		}{
			{"SELECT sum(f), avg(f) FROM gf WHERE g = 1", succeeds},
			{"SELECT sum(f) FROM gf WHERE g = 2", dependsOnPlan},
			{"SELECT avg(f) FROM gf WHERE g = 2", dependsOnPlan},
			{"SELECT sum(f), avg(f) FROM gf WHERE g = 3", succeeds},
			{"SELECT sum(f), avg(f) FROM gf WHERE g = 4", succeeds},
			{"SELECT sum(f), avg(f), min(f), max(f) FROM gf WHERE g = 5", succeeds},
			{"SELECT sum(f), avg(f) FROM gf WHERE g = 6", succeeds},
			{"SELECT sum(f) FROM gf WHERE g = 7", dependsOnPlan},
			{"SELECT sum(f), avg(f) FROM gf WHERE g = 8", succeeds},
			{"SELECT g, sum(f) FROM gf WHERE g IN (1, 3, 5, 8) GROUP BY g", succeeds},
			{"SELECT min(f), max(f) FROM gf WHERE g = 5 OR id = 3", succeeds},
			{"SELECT min(f + 0), max(f) FROM gf WHERE g IN (1, 5)", succeeds},
			{"SELECT sum(10 / (g - 2)) FROM gf", fails},
			{"SELECT count(*) FROM gf GROUP BY 10 / (g - 2)", fails},
			{"SELECT g FROM gf GROUP BY g HAVING sum(10 / (g - 2)) > 0", fails},
			{"SELECT g FROM gf GROUP BY g HAVING 10 / (g - 2) > 0", fails},
			{"SELECT g FROM gf GROUP BY g HAVING count(*) / (g - 2) > 0", fails},
			{"SELECT g FROM gf GROUP BY g HAVING count(*) > 1 AND 10 / (count(*) - 1) > 0", succeeds},
			{"SELECT g FROM gf GROUP BY g HAVING count(*) > 2 AND 10 / (count(*) - 2) > 0", dependsOnPlan},
			{"SELECT g FROM gf WHERE g > 2 GROUP BY g HAVING 10 / (g - 2) > 0", dependsOnPlan},
			{"SELECT g, 10 / (count(*) - 2) FROM gf GROUP BY g", fails},
			{"SELECT sum(10 / (g - 2)), sum(CAST(f AS bigint)) FROM gf", dependsOnPlan},
			{"SELECT CASE WHEN false THEN sum(10 / (g - 2)) END FROM gf", succeeds},
			{"SELECT sum(10 / (g - 2)) = NULL FROM gf", succeeds},
			{"SELECT count(*) FROM gf HAVING false AND sum(10 / (g - 2)) > 0", succeeds},
			{"SELECT g FROM gf GROUP BY g HAVING coalesce(-65, g, count(*) / 0) <= g + g", succeeds},
			{"SELECT max(id) FROM gf WHERE 10 / (g - 8) > 0", dependsOnPlan},
			{"SELECT count(*) FROM gf WHERE 10 / (g - 8) > 0", fails},
			{"SELECT g, sum(10 / g) FROM gf GROUP BY g ORDER BY g LIMIT 1", dependsOnPlan},
			{"SELECT DISTINCT sum(f) FROM gf WHERE g IN (1, 5) GROUP BY g", succeeds},
			{"SELECT f, count(*) FROM gf WHERE g = 5 OR id = 1 GROUP BY f", succeeds},
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
	t.Run("Postgres moves HAVING into WHERE, stops sorted grouping under LIMIT, computes MIN and MAX from an index, and adds floats in the order of its plan", func(t *testing.T) {
		verify.Covers(t, "EXPLAIN and the outcomes on Postgres 18 with sequential scans off show that a HAVING condition without aggregates is a condition of the scan, that sorted grouping under a LIMIT stops reading rows and succeeds where the whole query fails, that MIN and MAX read the first row of an index that passes WHERE and succeed where COUNT fails, that grouping is hashed or sorted by enable_hashagg and enable_sort, and that the SUM of the same floats depends on the order of the rows", "INV-PG-SUBSET", "UNS-FLOAT-SUM", "UNS-PLAN-DEPENDENT-AGGREGATE", "UNS-PLAN-DEPENDENT-LIMIT")
		b := newBoth(t, "")
		for _, q := range []string{
			"DROP TABLE IF EXISTS dev_w, dev_z",
			"CREATE TABLE dev (id bigint PRIMARY KEY, g bigint, x bigint)",
			"CREATE INDEX dev_g ON dev (g)",
			"INSERT INTO dev VALUES (1, 1, 1), (2, 1, 2), (3, 2, 0)",
			"CREATE TABLE dev_w (id bigint PRIMARY KEY, f double precision)",
			"INSERT INTO dev_w VALUES (1, 1), (2, 1e16), (3, -1e16)",
			"CREATE TABLE dev_z (id bigint PRIMARY KEY, f double precision)",
			"INSERT INTO dev_z VALUES (1, 1e16), (2, -1e16), (3, 1)",
			"ANALYZE dev",
		} {
			_, err := b.pg.ExecContext(t.Context(), q)
			is.NotError(t, err, q)
		}
		conn, err := b.pg.Conn(t.Context())
		is.NotError(t, err)
		defer func() { _ = conn.Close() }()
		run := func(sql string, settings ...string) (string, error) {
			t.Helper()
			for _, s := range []string{"enable_hashagg", "enable_sort"} {
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

		plan, _ := run("SELECT g FROM dev GROUP BY g HAVING g > 1 AND count(*) > 0")
		is.True(t, strings.Contains(plan, "Index Cond: (g > 1)") && strings.Contains(plan, "Filter: (count(*) > 0)"), "the condition without aggregates goes into the scan:\n"+plan)

		plan, err = run("SELECT g, sum(10 / x) FROM dev GROUP BY g ORDER BY g LIMIT 1")
		is.True(t, strings.Contains(plan, "Limit") && strings.Contains(plan, "GroupAggregate") && strings.Contains(plan, "Index Scan using dev_g"), plan)
		is.NotError(t, err, "sorted grouping stops after the first group")
		_, err = run("SELECT g, sum(10 / x) FROM dev GROUP BY g ORDER BY g")
		is.Equal(t, "22012", pgdiff.SQLState(err), "the second group divides by zero")

		plan, err = run("SELECT min(id) FROM dev WHERE 10 / x > 0")
		is.True(t, strings.Contains(plan, "InitPlan") && strings.Contains(plan, "Limit") && strings.Contains(plan, "Index Scan using dev_pkey"), plan)
		is.NotError(t, err, "MIN reads the first row of the index that passes WHERE")
		_, err = run("SELECT min(id), count(*) FROM dev WHERE 10 / x > 0")
		is.Equal(t, "22012", pgdiff.SQLState(err), "with COUNT, Postgres reads every row")

		plan, _ = run("SELECT x, count(*) FROM dev GROUP BY x", "enable_hashagg")
		is.True(t, strings.Contains(plan, "GroupAggregate") && strings.Contains(plan, "Sort"), plan)
		plan, _ = run("SELECT x, count(*) FROM dev GROUP BY x", "enable_sort")
		is.True(t, strings.Contains(plan, "HashAggregate"), plan)

		var w, z float64
		is.NotError(t, conn.QueryRowContext(t.Context(), "SELECT sum(f) FROM dev_w").Scan(&w))
		is.NotError(t, conn.QueryRowContext(t.Context(), "SELECT sum(f) FROM dev_z").Scan(&z))
		is.Equal(t, 0.0, w, "1 + 1e16 rounds to 1e16")
		is.Equal(t, 1.0, z, "1e16 - 1e16 is exact")
		_, err = run("SELECT sum(f) FROM (VALUES (1e308::float8), (1e308)) AS v (f)")
		is.Equal(t, "22003", pgdiff.SQLState(err), "a SUM that overflows is out of range")
		_, err = run("SELECT avg(f) FROM (VALUES (1e200::float8), (-1e200)) AS v (f)")
		is.Equal(t, "22003", pgdiff.SQLState(err), "the squares of an AVG overflow too, although the sum does not")
		_, err = run("SELECT sum(f) FROM (VALUES ('Infinity'::float8), (1e308), (1e308)) AS v (f)")
		is.NotError(t, err, "after an infinity, the sum does not overflow")
	})
}
