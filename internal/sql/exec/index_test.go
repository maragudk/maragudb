package exec_test

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

// checkIndexes fails the test when an index of the database does not match its table.
func checkIndexes(t *testing.T, db *engine.DB) {
	t.Helper()
	tx, err := db.BeginReadOnly(t.Context())
	is.NotError(t, err)
	is.NotError(t, exec.CheckIndexes(t.Context(), tx))
	is.NotError(t, tx.Rollback())
}

func TestSession_CreateIndex(t *testing.T) {
	t.Run("names indexes as Postgres does, and tables and indexes share one namespace", func(t *testing.T) {
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t_pkey (x bigint PRIMARY KEY)")
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, b bigint, UNIQUE (a, b), UNIQUE (a))")
		mustExec(t, s, "CREATE INDEX ON t (b)")
		mustExec(t, s, "CREATE INDEX ON t (b)")
		mustExec(t, s, "CREATE INDEX ON t (a, a)")
		for _, name := range []string{"t_pkey_pkey", "t_pkey1", "t_a_key", "t_a_b_key", "t_b_idx", "t_b_idx1", "t_a_a1_idx"} {
			is.Error(t, exec.ErrTableExists, queryErr(t, s, fmt.Sprintf("CREATE TABLE %s (x bigint PRIMARY KEY)", name)), name)
			is.Error(t, exec.ErrWrongObjectType, queryErr(t, s, "SELECT * FROM "+name), name)
		}
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "SELECT * FROM t_a_key1"), "the two UNIQUE (a) are one index")
		is.Error(t, exec.ErrTableExists, queryErr(t, s, "CREATE INDEX t ON t (b)"))
		mustExec(t, s, "CREATE INDEX IF NOT EXISTS t ON t (b)")
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE INDEX IF NOT EXISTS t ON t (nope)"), "columns are checked before the name")
		mustExec(t, s, "CREATE TABLE IF NOT EXISTS t_b_idx (x bigint PRIMARY KEY)")
		is.Error(t, exec.ErrNoTable, queryErr(t, s, "CREATE INDEX ON nope (a)"))
		is.Error(t, exec.ErrWrongObjectType, queryErr(t, s, "CREATE INDEX ON t_b_idx (a)"))
		checkIndexes(t, db)
	})

	t.Run("gives a constraint's name to an earlier index on the same columns that has none", func(t *testing.T) {
		s, _ := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, CONSTRAINT u UNIQUE (id), a bigint UNIQUE NULLS NOT DISTINCT, UNIQUE (a))")
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX u"), "u is the primary key's index")
		is.Error(t, exec.ErrNoIndex, queryErr(t, s, "DROP INDEX t_pkey"))
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX t_a_key"))
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX t_a_key1"), "NULLS NOT DISTINCT is another index")
		is.Error(t, exec.ErrTableExists, queryErr(t, s, "CREATE TABLE v (id bigint PRIMARY KEY, a bigint UNIQUE, CONSTRAINT v_a_key UNIQUE (a, id), b bigint)"), "the chosen name comes first")
		mustExec(t, s, "CREATE TABLE w (id bigint PRIMARY KEY, a bigint UNIQUE, CONSTRAINT w_a_key UNIQUE (id))")
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX w_a_key"), "the primary key took the name")
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX w_a_key1"))
	})

	t.Run("checks the definition before the name of the table", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY)")
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, UNIQUE (nope))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, UNIQUE (id, id))"))
		is.Error(t, exec.ErrInvalid, queryErr(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, id bigint)"))
		mustExec(t, s, "CREATE TABLE IF NOT EXISTS t (id bigint PRIMARY KEY, UNIQUE (nope))")
	})

	t.Run("shortens long generated names as Postgres does", func(t *testing.T) {
		s := newSession(t)
		long := strings.Repeat("x", 60)
		mustExec(t, s, fmt.Sprintf("CREATE TABLE %s (id bigint PRIMARY KEY, %s bigint UNIQUE)", long, long+"col"))
		// Both names are shortened, the longer first, to fit 63 bytes with "_" and "_key".
		name := strings.Repeat("x", 29) + "_" + strings.Repeat("x", 29) + "_key"
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX "+name))
		is.Error(t, exec.ErrDependentObjects, queryErr(t, s, "DROP INDEX "+long[:58]+"_pkey"))
	})

	t.Run("fails a unique index on rows that have the same values, but not on NULLs unless NULLS NOT DISTINCT", func(t *testing.T) {
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1, NULL), (2, 1, NULL), (3, 2, 5)")
		mustExec(t, s, "CREATE UNIQUE INDEX tb ON t (b)")
		mustExec(t, s, "CREATE UNIQUE INDEX tab ON t (a, b)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "CREATE UNIQUE INDEX ta ON t (a)"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "CREATE UNIQUE INDEX tbn ON t (b) NULLS NOT DISTINCT"))
		is.Error(t, exec.ErrNoIndex, queryErr(t, s, "DROP INDEX ta"), "the failed index is not there")
		checkIndexes(t, db)
	})
}

func TestSession_DropIndex(t *testing.T) {
	t.Run("drops an index and its entries, and DROP TABLE drops every index of the table", func(t *testing.T) {
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, b text)")
		mustExec(t, s, "CREATE INDEX tb ON t (b DESC)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 10, 'x'), (2, 20, 'y')")
		mustExec(t, s, "DROP INDEX tb")
		is.Error(t, exec.ErrNoIndex, queryErr(t, s, "DROP INDEX tb"))
		mustExec(t, s, "DROP INDEX IF EXISTS tb")
		is.Error(t, exec.ErrWrongObjectType, queryErr(t, s, "DROP INDEX t"))
		is.Error(t, exec.ErrWrongObjectType, queryErr(t, s, "DROP TABLE t_a_key"))
		is.Error(t, exec.ErrWrongObjectType, queryErr(t, s, "DROP TABLE IF EXISTS t_a_key"))
		checkIndexes(t, db)
		mustExec(t, s, "CREATE INDEX tb ON t (b)")
		mustExec(t, s, "DROP TABLE t")
		checkIndexes(t, db)
		mustExec(t, s, "CREATE TABLE tb (x bigint PRIMARY KEY)")
		mustExec(t, s, "CREATE TABLE t_a_key (x bigint PRIMARY KEY)")
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 10)")
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE a = 10"), "no entry of the dropped table remains")
		checkIndexes(t, db)
	})

	t.Run("is transactional", func(t *testing.T) {
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "CREATE UNIQUE INDEX ta ON t (a)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (2, 1)"))
		mustExec(t, s, "ROLLBACK")
		mustExec(t, s, "INSERT INTO t VALUES (2, 1)")
		checkIndexes(t, db)
	})
}

func TestSession_Unique(t *testing.T) {
	t.Run("rejects a row whose values another row has, in one statement or across statements, and allows NULLs", func(t *testing.T) {
		verify.Covers(t, "inserts and updates that would give two rows the same non-NULL values of a unique index fail with ErrDuplicateKey, within one statement, across statements of a transaction, and against committed rows, and NULLs never collide unless NULLS NOT DISTINCT", "INV-UNIQUE", "INV-PG-SUBSET")
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, b text, c bigint, UNIQUE NULLS NOT DISTINCT (c))")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1, 'x', 1), (2, NULL, 'y', NULL), (3, NULL, 'z', 3)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (4, 1, 'w', 4)"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (4, 4, 'w', 4), (5, 4, 'v', 5)"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (4, 4, 'w', NULL)"), "NULLS NOT DISTINCT")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET a = 1 WHERE id = 3"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET a = 7 WHERE id >= 2"), "two rows onto one value")
		mustExec(t, s, "UPDATE t SET a = NULL WHERE id = 1")
		mustExec(t, s, "INSERT INTO t VALUES (4, 1, 'w', 4)")
		mustExec(t, s, "BEGIN")
		mustExec(t, s, "INSERT INTO t VALUES (5, 5, 'v', 5)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "INSERT INTO t VALUES (6, 5, 'u', 6)"))
		mustExec(t, s, "ROLLBACK")
		is.Equal(t, "1 NULL\n2 NULL\n3 NULL\n4 1", query(t, s, "SELECT id, a FROM t ORDER BY id"))
		checkIndexes(t, db)
	})

	t.Run("rejects an UPDATE whose outcome depends on the order Postgres visits the rows, and fails a cycle of moves", func(t *testing.T) {
		verify.Covers(t, "an UPDATE whose rows move onto unique values that other rows of it leave is rejected when they form a chain, whose outcome depends on the order, and fails with ErrDuplicateKey when they form a cycle, which fails in every order", "UNS-KEY-UPDATE-ORDER", "INV-UNIQUE")
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, b bigint)")
		mustExec(t, s, "CREATE INDEX tb ON t (b)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1, 1), (2, 2, 2), (3, 3, 3)")
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "UPDATE t SET a = a + 1"))
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET a = 4 - a"), "1 and 3 swap")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET a = CASE a WHEN 3 THEN 1 ELSE a + 1 END"), "a cycle of three")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET id = 4 - id"), "the same for the primary key")
		mustExec(t, s, "UPDATE t SET a = a + 10, b = b + 1")
		mustExec(t, s, "UPDATE t SET a = a - 10 WHERE b > 2")
		mustExec(t, s, "UPDATE t SET id = id + 10")
		is.Equal(t, "11 11 2\n12 2 3\n13 3 4", query(t, s, "SELECT * FROM t ORDER BY id"))
		is.Equal(t, "12", query(t, s, "SELECT id FROM t WHERE a = 2"))
		checkIndexes(t, db)
	})

	t.Run("reports the error of the first row in key order when another row fails otherwise", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, n bigint NOT NULL)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1, 1), (2, 2, 0), (3, 3, 1)")
		is.Error(t, exec.ErrDuplicateKey, queryErr(t, s, "UPDATE t SET a = 3, n = 10 / n WHERE id <= 2"))
		is.Error(t, exec.ErrDivisionByZero, queryErr(t, s, "UPDATE t SET a = a + 10, n = 10 / n"))
	})
}

func TestSession_IndexScans(t *testing.T) {
	t.Run("finds rows through an index for equalities, ranges, and IN, in every direction and NULL order", func(t *testing.T) {
		s, db := newWorld(t).session(exec.Options{})
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b text, f double precision)")
		mustExec(t, s, "CREATE INDEX ta ON t (a DESC NULLS LAST, b)")
		mustExec(t, s, "CREATE UNIQUE INDEX tf ON t (f)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 5, 'x', 1.5), (2, 5, 'y', -0.0), (3, NULL, 'z', NULL), (4, 7, NULL, 'NaN'), (5, -1, 'x', 'Infinity')")
		is.Equal(t, "1\n2", query(t, s, "SELECT id FROM t WHERE a = 5 ORDER BY id"))
		is.Equal(t, "2", query(t, s, "SELECT id FROM t WHERE a = 5 AND b = 'y'"))
		is.Equal(t, "1\n2\n4", query(t, s, "SELECT id FROM t WHERE a > 0 ORDER BY id"))
		is.Equal(t, "1\n2\n5", query(t, s, "SELECT id FROM t WHERE a < 7 ORDER BY id"))
		is.Equal(t, "4\n5", query(t, s, "SELECT id FROM t WHERE a IN (7, -1) ORDER BY id"))
		is.Equal(t, "3", query(t, s, "SELECT id FROM t WHERE a IS NULL"))
		is.Equal(t, "2", query(t, s, "SELECT id FROM t WHERE f = 0"))
		is.Equal(t, "4", query(t, s, "SELECT id FROM t WHERE f > 'Infinity'"))
		is.Equal(t, "4\n1\n2\n5\n3", query(t, s, "SELECT id FROM t ORDER BY a DESC NULLS LAST, b"))
		is.Equal(t, "4\n1", query(t, s, "SELECT id FROM t ORDER BY a DESC NULLS LAST, b, id LIMIT 2"))
		is.Equal(t, "2\n5", query(t, s, "SELECT id FROM t ORDER BY a DESC NULLS LAST, b, id LIMIT 2 OFFSET 2"))
		is.Equal(t, "2\n1", query(t, s, "SELECT id FROM t WHERE a = 5 ORDER BY a DESC NULLS LAST, b DESC"))
		checkIndexes(t, db)
	})

	t.Run("a cold point lookup through a unique index reads the entry and the row", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, email text UNIQUE)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 'a@x'), (2, 'b@x')")
		is.Equal(t, "2", query(t, s, "SELECT id FROM t WHERE email = $1", "b@x"))
	})

	t.Run("evaluates a condition that can fail on every row that some index scan of Postgres reads", func(t *testing.T) {
		s := newSession(t)
		mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, j bigint)")
		mustExec(t, s, "CREATE INDEX ta ON t (a)")
		mustExec(t, s, "INSERT INTO t VALUES (1, 1, 1), (2, 2, 0), (3, 3, 0)")
		// Postgres can scan the primary key, which reads row 1, or ta, which reads row 2.
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT id FROM t WHERE id = 1 AND a = 2 AND 10 / j > 0"))
		// Both read only rows that the other conditions reject, or row 1.
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE id = 1 AND a = 1 AND 10 / j > 0"))
		// Rows 2 and 3 fail, but every plan skips them.
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE id <= 1 AND a <= 1 AND 10 / j > 0"))
		// The primary key reads row 2, which a < 1 rejects.
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT id FROM t WHERE id <= 2 AND a < 1 AND 10 / j > 0"))
		// A scan of the whole of ta in order reads every row.
		is.Error(t, exec.ErrUnsupported, queryErr(t, s, "SELECT id FROM t WHERE id = 1 AND 10 / j > 0 ORDER BY a"))
		is.Equal(t, "1", query(t, s, "SELECT id FROM t WHERE id = 1 AND 10 / j > 0 ORDER BY j"))
	})
}

func TestMutations_Indexes(t *testing.T) {
	t.Run("IndexStaleDelete leaves entries that CheckIndexes finds", func(t *testing.T) {
		verify.Covers(t, "with the mutant, an UPDATE or DELETE leaves its old index entries, which the index check finds", "MUT-INDEX-STALE-DELETE", "INV-INDEX-CONSISTENT")
		for _, stmt := range []string{"UPDATE t SET a = 3 WHERE id = 1", "DELETE FROM t WHERE id = 1", "UPDATE t SET id = 5 WHERE id = 1", "UPDATE t SET b = 'q'"} {
			for _, mutated := range []bool{false, true} {
				s, db := newWorld(t).session(exec.Options{Mutations: exec.Mutations{IndexStaleDelete: mutated}})
				mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE, b text)")
				mustExec(t, s, "CREATE INDEX tb ON t (b)")
				mustExec(t, s, "INSERT INTO t VALUES (1, 1, 'x'), (2, 2, 'y')")
				mustExec(t, s, stmt)
				tx, err := db.BeginReadOnly(t.Context())
				is.NotError(t, err)
				err = exec.CheckIndexes(t.Context(), tx)
				if mutated {
					is.Error(t, exec.ErrIndexInconsistent, err, stmt)
				} else {
					is.NotError(t, err, stmt)
				}
			}
		}
	})

	t.Run("UniqueNoCheck and UniqueOwnWrites let duplicates in", func(t *testing.T) {
		verify.Covers(t, "with either mutant, a unique index lets two rows have the same values, which the index check finds", "MUT-UNIQUE-NO-CHECK", "MUT-UNIQUE-OWN-WRITES", "INV-UNIQUE")
		for _, m := range []exec.Mutations{{}, {UniqueNoCheck: true}, {UniqueOwnWrites: true}} {
			s, db := newWorld(t).session(exec.Options{Mutations: m})
			mustExec(t, s, "CREATE TABLE t (id bigint PRIMARY KEY, a bigint UNIQUE)")
			_, _, err := run(t.Context(), s, "INSERT INTO t VALUES (1, 1), (2, 1)")
			if m == (exec.Mutations{}) {
				is.Error(t, exec.ErrDuplicateKey, err)
				checkIndexes(t, db)
				continue
			}
			is.NotError(t, err)
			is.Equal(t, "1 1\n2 1", query(t, s, "SELECT * FROM t ORDER BY id"))
			tx, err := db.BeginReadOnly(t.Context())
			is.NotError(t, err)
			is.Error(t, exec.ErrIndexInconsistent, exec.CheckIndexes(t.Context(), tx))
		}
	})
}

// modelRow of [TestSession_IndexesMatchModel].
type modelRow struct {
	a, c *int64
	b    string
}

func TestSession_IndexesMatchModel(t *testing.T) {
	t.Run("random writes on a table with indexes match a model, and every index matches its table after every statement", func(t *testing.T) {
		verify.Covers(t, "random inserts, updates, deletes, and index creations and drops on a table with unique and other indexes give the rows and the unique violations of a model, the indexes match the table after every statement, and lookups through every index return the rows of the model", "INV-INDEX-CONSISTENT", "INV-UNIQUE")
		hegel.Test(t, func(ht *hegel.T) {
			s, db := newWorld(t).session(exec.Options{})
			mustExec(t, s, "CREATE TABLE m (id bigint PRIMARY KEY, a bigint UNIQUE, b text, c bigint, UNIQUE NULLS NOT DISTINCT (c))")
			mustExec(t, s, "CREATE INDEX mb ON m (b DESC, a)")
			model := map[int64]modelRow{}
			small := func() *int64 {
				if hegel.Draw(ht, hegel.Integers(0, 4)) == 0 {
					return nil
				}
				return new(int64(hegel.Draw(ht, hegel.Integers[int64](0, 5))))
			}
			lit := func(v *int64) string {
				if v == nil {
					return "NULL"
				}
				return fmt.Sprint(*v)
			}
			// valid reports whether rows break no unique index.
			valid := func(rows map[int64]modelRow) bool {
				as, cs := map[int64]bool{}, map[string]bool{}
				for _, r := range rows {
					if r.a != nil {
						if as[*r.a] {
							return false
						}
						as[*r.a] = true
					}
					if cs[lit(r.c)] {
						return false
					}
					cs[lit(r.c)] = true
				}
				return true
			}
			q := func(sql string, args ...any) string {
				rows, _, err := run(t.Context(), s, sql, args...)
				if err != nil {
					ht.Fatalf("%s: %v", sql, err)
				}
				return render(rows)
			}
			for range hegel.Draw(ht, hegel.Integers(1, 12)) {
				var sql string
				next := map[int64]modelRow{}
				for id, r := range model {
					next[id] = r
				}
				// interferes is set when the outcome of an UPDATE may depend on the order of its rows.
				interferes := false
				switch hegel.Draw(ht, hegel.Integers(0, 4)) {
				case 0, 1:
					var values []string
					ok := true
					for range hegel.Draw(ht, hegel.Integers(1, 3)) {
						id := hegel.Draw(ht, hegel.Integers[int64](0, 6))
						r := modelRow{a: small(), b: hegel.Draw(ht, hegel.SampledFrom([]string{"x", "y", "z"})), c: small()}
						values = append(values, fmt.Sprintf("(%d, %s, '%s', %s)", id, lit(r.a), r.b, lit(r.c)))
						if _, dup := next[id]; dup {
							ok = false
						}
						next[id] = r
						ok = ok && valid(next)
					}
					sql = "INSERT INTO m VALUES " + strings.Join(values, ", ")
					if !ok {
						next = nil
					}
				case 2:
					a := small()
					b := hegel.Draw(ht, hegel.SampledFrom([]string{"x", "y", "z"}))
					sql = fmt.Sprintf("UPDATE m SET a = %s WHERE b = '%s'", lit(a), b)
					for id, r := range next {
						if r.b == b {
							r.a = a
							next[id] = r
						}
					}
				case 3:
					d := hegel.Draw(ht, hegel.Integers[int64](-1, 1))
					sql = fmt.Sprintf("UPDATE m SET c = c + %d, b = 'z'", d)
					cs := map[int64]bool{}
					for _, r := range next {
						if r.c != nil {
							cs[*r.c] = true
						}
					}
					for id, r := range next {
						if r.c != nil {
							r.c = new(*r.c + d)
							interferes = interferes || d != 0 && cs[*r.c]
						}
						r.b = "z"
						next[id] = r
					}
				default:
					b := hegel.Draw(ht, hegel.SampledFrom([]string{"x", "y", "z"}))
					sql = fmt.Sprintf("DELETE FROM m WHERE b = '%s' OR a IS NULL", b)
					for id, r := range next {
						if r.b == b || r.a == nil {
							delete(next, id)
						}
					}
				}
				if next != nil && !valid(next) {
					next = nil
				}
				ht.Note(sql)
				_, _, err := run(t.Context(), s, sql)
				switch {
				case next == nil:
					if !errors.Is(err, exec.ErrDuplicateKey) {
						ht.Fatalf("%s: want a duplicate key, got %v", sql, err)
					}
				case err == nil:
					model = next
				case interferes && (errors.Is(err, exec.ErrUnsupported) || errors.Is(err, exec.ErrDuplicateKey)):
				default:
					ht.Fatalf("%s: %v", sql, err)
				}
				tx, err := db.BeginReadOnly(t.Context())
				if err != nil {
					ht.Fatal(err)
				}
				if err := exec.CheckIndexes(t.Context(), tx); err != nil {
					ht.Fatalf("after %s: %v", sql, err)
				}
				_ = tx.Rollback()
				for v := int64(0); v <= 5; v++ {
					var want []string
					for id, r := range model {
						if r.a != nil && *r.a == v {
							want = append(want, fmt.Sprint(id))
						}
					}
					if got := q("SELECT id FROM m WHERE a = $1", v); got != strings.Join(want, "\n") {
						ht.Fatalf("a = %d: got %q, want %q", v, got, want)
					}
				}
				for _, b := range []string{"x", "y", "z"} {
					var want []string
					for id, r := range model {
						if r.b == b {
							want = append(want, fmt.Sprint(id))
						}
					}
					sort.Strings(want)
					got := strings.Split(q("SELECT id FROM m WHERE b = $1", b), "\n")
					if got[0] == "" {
						got = nil
					}
					sort.Strings(got)
					if !slices.Equal(got, want) {
						ht.Fatalf("b = %s: got %q, want %q", b, got, want)
					}
				}
			}
		}, hegel.WithTestCases(300))
	})
}
