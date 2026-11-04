package pgdiff_test

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/verify"
)

var update = flag.Bool("update", false, "rewrite the ratchet baseline in testdata/ratchet.json")

func TestMain(m *testing.M) {
	verify.Main(m, pgdiff.DropDatabase)
}

func newRunner(t *testing.T) *pgdiff.Runner {
	t.Helper()
	return &pgdiff.Runner{PG: pgdiff.Postgres(t), New: func() (*sql.DB, error) {
		// Maintenance stays off, so that a background compaction cannot land at a wall-clock
		// moment and make a case harder to replay.
		c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(s3fake.New()), Maintenance: new(false)}, "s3://bucket/diff")
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(c), nil
	}}
}

// run cases with hegel and return the total report, failing on the first mismatch.
func run(t *testing.T, r *pgdiff.Runner, opts ...hegel.Option) pgdiff.Report {
	t.Helper()
	var total pgdiff.Report
	var mu sync.Mutex
	hegel.Test(t, func(ht *hegel.T) {
		c := pgdiff.Generate(pgdiff.HegelSource(ht))
		rep, err := r.Run(t.Context(), c)
		mu.Lock()
		total.Add(rep)
		mu.Unlock()
		if err != nil {
			ht.Note(ast.Render(c.Create))
			for _, st := range c.Statements {
				ht.Note(ast.Render(st))
			}
			ht.Fatalf("%v", err)
		}
	}, opts...)
	return total
}

// ratchet is the committed baseline of the generator's executed statements.
type ratchet struct {
	Seed  uint64 `json:"seed"`
	Cases int   `json:"cases"`
	// Executed statements ran on both sides, out of Statements generated; the rest were rejected
	// by maragudb as unsupported.
	Executed   int `json:"executed"`
	Statements int `json:"statements"`
}

func TestDifferential(t *testing.T) {
	t.Run("generated statements give the same results on Postgres and maragudb", func(t *testing.T) {
		verify.Covers(t, "hegel-generated schemas, rows, transactions, and statements, written once as Postgres SQL, give the same results, column names, affected-row counts, and transaction states on both sides, ordered exactly when ORDER BY decides the order and as multisets otherwise, and fail on both sides with the same error class, except for statements that maragudb rejects as unsupported, which Postgres skips", "AC-4.1", "AC-4.2", "AC-4.3")
		r := newRunner(t)
		cases := 200
		if s := os.Getenv("MARAGUDB_PGDIFF_CASES"); s != "" {
			var err error
			cases, err = strconv.Atoi(s)
			is.NotError(t, err)
		}
		total := run(t, r, hegel.WithTestCases(cases))
		t.Log(total)
		if t.Failed() {
			// Shrinking skews the counts toward degenerate cases.
			return
		}
		classMatches := 0
		for _, n := range total.ErrorClasses {
			classMatches += n
		}
		verify.Record(t, verify.Fact{Diff: &verify.Diff{Cases: cases, Statements: total.Statements, Executed: total.Executed, Rejected: total.Rejected, OK: total.OK, Errors: total.Errors, ClassMatches: classMatches, Classes: len(total.ErrorClasses), Ordered: total.Ordered, Transactions: total.Transactions}})
		// The run must exercise real behavior, or agreement means nothing.
		is.True(t, total.Statements >= cases, "statements")
		is.True(t, total.OK > total.Executed/2, "most statements succeed on both sides")
		is.True(t, total.Errors > 0, "some statements fail on both sides")
		is.True(t, len(total.ErrorClasses) >= 3, "failures on both sides are compared by class, for several classes")
		is.True(t, total.Rows > 0, "rows are returned")
		is.True(t, total.Ordered > 0, "some comparisons are ordered")
		is.True(t, total.Transactions > 0, "some statements run in explicit transactions")
		is.True(t, total.Rejected > 0, "some statements are rejected, so the rejections are exercised")
	})

	t.Run("the generator mostly produces SQL that maragudb accepts, and no less than the committed baseline", func(t *testing.T) {
		verify.Covers(t, "with a fixed seed, at least 70% of the generated statements run on both sides, and no fewer than the baseline in testdata/ratchet.json", "AC-4.5")
		path := filepath.Join("testdata", "ratchet.json")
		var base ratchet
		b, err := os.ReadFile(path)
		is.NotError(t, err)
		is.NotError(t, json.Unmarshal(b, &base))
		// The cases come from a seeded PRNG rather than hegel, whose engine does not give the same
		// cases for the same seed in every process.
		r := newRunner(t)
		var total pgdiff.Report
		for i := range base.Cases {
			c := pgdiff.Generate(pgdiff.RandSource(rand.New(rand.NewPCG(base.Seed, uint64(i)))))
			rep, err := r.Run(t.Context(), c)
			total.Add(rep)
			if err != nil {
				var lines []string
				lines = append(lines, ast.Render(c.Create))
				for _, st := range c.Statements {
					lines = append(lines, ast.Render(st))
				}
				t.Fatalf("case %d:\n%s\n%v", i, strings.Join(lines, "\n"), err)
			}
		}
		t.Log(total)
		got := ratchet{Seed: base.Seed, Cases: base.Cases, Executed: total.Executed, Statements: total.Statements}
		if *update {
			b, err := json.MarshalIndent(got, "", "  ")
			is.NotError(t, err)
			is.NotError(t, os.WriteFile(path, append(b, '\n'), 0o644))
			return
		}
		verify.Record(t, verify.Fact{Ratchet: &verify.Ratchet{Cases: got.Cases, Executed: got.Executed, Statements: got.Statements, Baseline: base.Executed}})
		is.True(t, float64(got.Executed) >= 0.7*float64(got.Statements), fmt.Sprintf("%d of %d statements executed", got.Executed, got.Statements))
		is.True(t, got.Executed >= base.Executed, fmt.Sprintf("executed statements dropped from %d to %d; generate more supported SQL, or support more of it", base.Executed, got.Executed))
		if got.Executed > base.Executed {
			t.Logf("executed statements rose from %d to %d; raise the baseline with go test ./internal/pgdiff -run TestDifferential -args -update", base.Executed, got.Executed)
		}
	})
}
