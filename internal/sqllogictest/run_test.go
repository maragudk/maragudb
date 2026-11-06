package sqllogictest

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m, pgdiff.DropDatabase)
}

func newFileRunner(t *testing.T) *fileRunner {
	t.Helper()
	return &fileRunner{pg: pgdiff.Postgres(t), newMaragudb: func() (*sql.DB, error) {
		c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(s3fake.New())}, "s3://bucket/sqllogictest")
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(c), nil
	}}
}

// runScript runs a test file given as text, and fails on harness errors.
func runScript(t *testing.T, r *fileRunner, script string) fileReport {
	t.Helper()
	rep, err := r.run(t.Context(), 0, "script.test", []byte(script))
	is.NotError(t, err)
	return rep
}

func TestFileRunner_run(t *testing.T) {
	t.Run("runs a file on both databases and counts what its records did", func(t *testing.T) {
		rep := runScript(t, newFileRunner(t), `hash-threshold 8

statement ok
CREATE TABLE t1(a INTEGER, b VARCHAR(8))

statement ok
INSERT INTO t1 VALUES(1, 'x')

statement ok
INSERT INTO t1 VALUES(2, 'y'), (3, 'z')

statement ok
CREATE INDEX t1a ON t1(a)

onlyif mysql
query I rowsort
SELECT a DIV 2 FROM t1
----
0

skipif postgresql
query IT rowsort
SELECT a, b FROM t1 WHERE a > 1
----
these results are never read

query I rowsort
SELECT DISTINCT a FROM t1
----
1

query I nosort
SELECT a FROM t1 ORDER BY a DESC, b
----

statement error
INSERT INTO t1 VALUES(1, 'x')
`)
		is.True(t, rep.Mismatch == nil, fmt.Sprint(rep.Mismatch))
		is.Equal(t, 9, rep.Records)
		is.Equal(t, 1, rep.Dropped, "CREATE INDEX")
		is.Equal(t, 1, rep.OtherEngine, "only for MySQL")
		is.Equal(t, 7, rep.Statements)
		is.Equal(t, 1, rep.Rejected, "DISTINCT")
		is.Equal(t, 6, rep.Executed)
		is.Equal(t, 1, rep.Errors, "the duplicate key")
		is.Equal(t, 1, rep.SetupFailed["23505 duplicate key value violates unique constraint _"])
		is.Equal(t, 0, rep.Skipped)
	})

	t.Run("skips the statements that depend on a table whose creation was rejected or failed, until it is created", func(t *testing.T) {
		rep := runScript(t, newFileRunner(t), `statement ok
CREATE TABLE t1(a INTEGER UNIQUE)

statement ok
INSERT INTO t1 VALUES(1)

query I rowsort
SELECT a FROM t1
----

statement ok
CREATE VIEW v1 AS SELECT a FROM t1

query I rowsort
SELECT a FROM v1
----

statement ok
CREATE TABLE t2(a INTEGER PRIMARY KEY, a INTEGER)

query I rowsort
SELECT * FROM "t2" WHERE a = 1
----

statement ok
CREATE TABLE t1(a INTEGER)

statement ok
INSERT INTO t1 VALUES(NULL)

query I rowsort
SELECT a FROM t1
----
`)
		is.True(t, rep.Mismatch == nil, fmt.Sprint(rep.Mismatch))
		is.Equal(t, 5, rep.Skipped)
		is.Equal(t, 4, rep.SkippedFor["rejected: UNIQUE_is_not_supported"], "also the view on the table, and the query of the view")
		is.Equal(t, 1, rep.SkippedFor["failed on Postgres: 42701 column _ specified more than once"])
		is.Equal(t, 1, rep.SetupFailed["23502 null value in column _ of relation _ violates not-null constraint"], "the primary key of the rewrite makes the column NOT NULL")
		is.Equal(t, 1, rep.Rejected)
		is.Equal(t, 4, rep.Executed)
	})

	t.Run("stops at a halt for this engine, and goes on at a halt for another", func(t *testing.T) {
		rep := runScript(t, newFileRunner(t), `onlyif mssql
halt

query I
SELECT 1
----

skipif sqlite
halt

query I
SELECT 2
----

query I
SELECT 3
----
`)
		is.Equal(t, 1, rep.Executed)
		is.Equal(t, 2, rep.OtherEngine)
	})

	t.Run("reports a difference between the databases with the file and line, and ends the file", func(t *testing.T) {
		// The store of maragudb fails every read once a table exists, so that a query that
		// Postgres answers fails on maragudb.
		var created atomic.Bool
		fake := s3fake.New(s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
			if strings.Contains(key, "/seg/") {
				created.Store(true)
			}
			if created.Load() && (kind == s3.KindGet || kind == s3.KindGetConditional) {
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}}))
		r := newFileRunner(t)
		r.newMaragudb = func() (*sql.DB, error) {
			c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(fake), Maintenance: new(false), MemoryCacheBytes: new(int64(0))}, "s3://bucket/mismatch")
			if err != nil {
				return nil, err
			}
			return sql.OpenDB(c), nil
		}
		rep := runScript(t, r, `statement ok
CREATE TABLE t1(a INTEGER PRIMARY KEY)

query I
SELECT a FROM t1
----

query I
SELECT 1
----
`)
		is.True(t, pgdiff.IsMismatch(rep.Mismatch), fmt.Sprint(rep.Mismatch))
		is.True(t, strings.HasPrefix(rep.Mismatch.Error(), "script.test:4: SELECT a FROM t1"), rep.Mismatch.Error())
		is.Equal(t, 2, rep.Statements, "the file ends at the mismatch")
	})

	t.Run("compares in order only by the real primary key of a table", func(t *testing.T) {
		rep := runScript(t, newFileRunner(t), `statement ok
CREATE TABLE t1(a INTEGER PRIMARY KEY, b INTEGER)

statement ok
INSERT INTO t1 VALUES(1, 5), (2, 5), (3, 4)

statement ok
CREATE TABLE IF NOT EXISTS t1(b INTEGER PRIMARY KEY)

statement ok
BEGIN

statement ok
DROP TABLE t1

statement ok
ROLLBACK

query II nosort
SELECT a, b FROM t1 ORDER BY b
----

query II nosort
SELECT a, b FROM t1 ORDER BY a
----
`)
		is.True(t, rep.Mismatch == nil, fmt.Sprint(rep.Mismatch))
		is.Equal(t, 1, rep.Ordered, "only the ORDER BY of the key is a total order")
		is.Equal(t, 1, rep.SortKeysOrdered)
	})

	t.Run("fails on a file that does not parse", func(t *testing.T) {
		_, err := newFileRunner(t).run(t.Context(), 0, "bad.test", []byte("frobnicate\n"))
		is.True(t, err != nil && strings.Contains(err.Error(), "bad.test: line 1"), fmt.Sprint(err))
	})
}

func TestFileRunner_postgres(t *testing.T) {
	t.Run("gives each worker a connection in its own schema with sequential scans off", func(t *testing.T) {
		r := newFileRunner(t)
		for worker := range 3 {
			conn, err := r.postgres(t.Context(), worker)
			is.NotError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			var schema, seqscan string
			is.NotError(t, conn.QueryRowContext(t.Context(), "SELECT current_schema(), current_setting('enable_seqscan')").Scan(&schema, &seqscan))
			is.Equal(t, fmt.Sprintf("sqllogictest_%d", worker), schema)
			is.Equal(t, "off", seqscan)
		}
	})
}

// TestRegressions has the statements of the corpus that found differences from Postgres, which
// are fixed. They run on both databases, through the rewrites and the comparison of the
// corpus.
func TestRegressions(t *testing.T) {
	t.Run("NOT of a comparison of an expression with itself is NULL where the expression is", func(t *testing.T) {
		verify.Covers(t, "the statements of the sqllogictest corpus that found differences from Postgres give the same results on both sides", "INV-PG-SUBSET")
		rep := runScript(t, newFileRunner(t), `statement ok
CREATE TABLE tab0(col0 INTEGER PRIMARY KEY, col1 INTEGER, col2 INTEGER)

statement ok
INSERT INTO tab0 VALUES(3,26,54)

statement ok
INSERT INTO tab0 VALUES(64,10,NULL)

query I rowsort
SELECT 31 AS col0 FROM tab0 WHERE NOT ( NULL ) = NULL
----

query III rowsort
SELECT ALL * FROM tab0 WHERE NOT NULL = ( NULL )
----

query I rowsort
SELECT col0 FROM tab0 WHERE NOT col2 = col2
----

query I rowsort
SELECT col0 FROM tab0 WHERE NOT (col2 IN (col2))
----

query I rowsort
SELECT col0 FROM tab0 WHERE NOT (col2 IN (col2, col1 + 100))
----

query I rowsort
SELECT col0 FROM tab0 WHERE (col2 = col2) = false
----

query I rowsort
SELECT col0 FROM tab0 WHERE NOT (NOT (NOT col2 = col2))
----

query I rowsort
SELECT col0 FROM tab0 WHERE col2 = col2
----
`)
		is.True(t, rep.Mismatch == nil, fmt.Sprint(rep.Mismatch))
		is.Equal(t, rep.Statements, rep.Executed, "every statement runs on both sides")
		is.Equal(t, 0, len(rep.SetupFailed), "the setup runs")
	})
}

var update = flag.Bool("update", false, "rewrite the baseline in testdata/baseline.json for the files that run")

// subset of the corpus that `make test` runs: a file of each kind of the index and random
// directories, the select files with tables of their own, and the evidence files.
var subset = []string{
	"evidence/*.test",
	"index/between/10/slt_good_0.test",
	"index/delete/10/slt_good_0.test",
	"index/in/10/slt_good_0.test",
	"index/orderby/10/slt_good_0.test",
	"index/random/10/slt_good_0.test",
	"random/expr/slt_good_0.test",
	"random/select/slt_good_0.test",
	"select1.test",
}

// baseline is the committed number of statements of every file that ran on both sides.
type baseline struct {
	// Commit of the corpus that the counts are for.
	Commit string `json:"commit"`
	// Executed maps every test file to its number of statements that ran on both sides.
	Executed map[string]int `json:"executed"`
}

const baselinePath = "testdata/baseline.json"

func TestCorpus(t *testing.T) {
	t.Run("the statements of the corpus give the same results on Postgres and maragudb, and no file executes fewer of them than its baseline", func(t *testing.T) {
		verify.Covers(t, "the SQLite sqllogictest corpus, rewritten once and run as the same text on both sides, gives the same results, column names, affected-row counts, and error classes on Postgres and maragudb, except for statements that maragudb rejects as unsupported, and every file executes at least its committed baseline of statements on both sides", "AC-4.6")
		c, err := loadCorpus(t.Context())
		is.NotError(t, err)
		name, globs := "make test subset", strings.Join(subset, ",")
		switch {
		case os.Getenv("MARAGUDB_SQLLOGICTEST_FILES") != "":
			globs = os.Getenv("MARAGUDB_SQLLOGICTEST_FILES")
			name = "files " + globs
		case os.Getenv("MARAGUDB_SQLLOGICTEST") == "full":
			name, globs = "full corpus", "**"
		}
		files, err := c.match(globs)
		is.NotError(t, err)

		var base baseline
		b, err := os.ReadFile(baselinePath)
		is.NotError(t, err)
		is.NotError(t, json.Unmarshal(b, &base))
		if base.Commit != commit && !*update {
			t.Fatalf("the baseline is for corpus commit %s, not %s; rewrite it with make sqllogictest-ratchet", base.Commit, commit)
		}

		start := time.Now()
		reports, err := newFileRunner(t).runFiles(t.Context(), c, files, runtime.GOMAXPROCS(0))
		is.NotError(t, err)
		total := newFileReport()
		fact := verify.Corpus{Name: name, Files: len(files)}
		for _, f := range files {
			rep := reports[f]
			total.add(rep)
			want, ok := base.Executed[f]
			fact.Baseline += want
			switch {
			case rep.Mismatch != nil:
				fact.Mismatches++
				t.Errorf("%v", rep.Mismatch)
			case *update:
			case !ok:
				t.Errorf("%s has no baseline; add it with make sqllogictest-ratchet SQLLOGICTEST_FILES=%s", f, f)
			case rep.Executed < want:
				fact.BelowBaseline++
				t.Errorf("%s: executed statements dropped from %d to %d; support more SQL, or explain the drop and lower the baseline with make sqllogictest-ratchet", f, want, rep.Executed)
			case rep.Executed > want:
				t.Logf("%s: executed statements rose from %d to %d; raise the baseline with make sqllogictest-ratchet", f, want, rep.Executed)
			}
		}
		t.Logf("%s, %d files in %v: %v", name, len(files), time.Since(start).Round(time.Millisecond), total.Report)
		t.Logf("%d records, %d for other engines, %d dropped by the rewrites, %d skipped", total.Records, total.OtherEngine, total.Dropped, total.Skipped)
		t.Logf("rejected for: %s", topCounts(total.RejectedFor, 30))
		t.Logf("setup failed on Postgres for: %s", topCounts(total.SetupFailed, 30))
		t.Logf("skipped for: %s", topCounts(total.SkippedFor, 30))

		if *update {
			if t.Failed() {
				return
			}
			if base.Executed == nil || base.Commit != commit {
				base = baseline{Commit: commit, Executed: map[string]int{}}
			}
			for _, f := range files {
				base.Executed[f] = reports[f].Executed
			}
			b, err := json.MarshalIndent(base, "", "  ")
			is.NotError(t, err)
			is.NotError(t, os.WriteFile(baselinePath, append(b, '\n'), 0o644))
			return
		}

		fact.Statements, fact.Executed, fact.Rejected, fact.Skipped = total.Statements, total.Executed, total.Rejected, total.Skipped
		for _, n := range total.SetupFailed {
			fact.SetupFailed += n
		}
		fact.RejectedFor, fact.SetupFailedFor, fact.SkippedFor = total.RejectedFor, total.SetupFailed, total.SkippedFor
		verify.Record(t, verify.Fact{Corpus: &fact})

		// The run must exercise real behavior, or agreement means nothing.
		is.True(t, total.Executed > 0, "statements run on both sides")
		is.True(t, total.Rows > 0, "rows are returned")
		if name != "make test subset" {
			return
		}
		is.True(t, total.Executed > total.Statements/2, "most statements run on both sides")
		is.True(t, total.Errors > 0, "some statements fail on both sides")
		is.True(t, total.Rejected > 0, "some statements are rejected")
		is.True(t, total.Ordered > 0, "some comparisons are ordered")
		is.True(t, total.SortKeysOrdered > 0, "some comparisons check the order of the sort keys")
		is.True(t, total.Skipped > 0, "some statements depend on a table that could not be created")
		is.True(t, total.Dropped > 0, "the rewrites drop some statements")
	})
}
