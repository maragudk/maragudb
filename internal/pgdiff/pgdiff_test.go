package pgdiff_test

import (
	"database/sql"
	"os"
	"strconv"
	"sync"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
	"maragu.dev/maragudb/internal/sql/ast"
)

func newRunner(t *testing.T) *pgdiff.Runner {
	t.Helper()
	return &pgdiff.Runner{PG: pgdiff.Postgres(t), New: func() (*sql.DB, error) {
		c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(s3fake.New())}, "s3://bucket/diff")
		if err != nil {
			return nil, err
		}
		return sql.OpenDB(c), nil
	}}
}

func TestDifferential(t *testing.T) {
	t.Run("generated statements give the same results on Postgres and maragudb", func(t *testing.T) {
		r := newRunner(t)
		cases := 200
		if s := os.Getenv("MARAGUDB_PGDIFF_CASES"); s != "" {
			var err error
			cases, err = strconv.Atoi(s)
			is.NotError(t, err)
		}
		var total pgdiff.Report
		var mu sync.Mutex
		hegel.Test(t, func(ht *hegel.T) {
			c := pgdiff.Generate(ht)
			rep, err := r.Run(t.Context(), c)
			mu.Lock()
			total.Add(rep)
			mu.Unlock()
			if err != nil {
				var lines []string
				lines = append(lines, ast.Render(c.Create, ast.Maragudb))
				for _, st := range c.Statements {
					lines = append(lines, ast.Render(st, ast.Maragudb))
				}
				for _, l := range lines {
					ht.Note(l)
				}
				ht.Fatalf("%v", err)
			}
		}, hegel.WithTestCases(cases))
		t.Log(total)
		if t.Failed() {
			// Shrinking skews the counts toward degenerate cases.
			return
		}
		// The run must exercise real behavior, or agreement means nothing.
		is.True(t, total.Statements >= cases, "statements")
		is.True(t, total.OK > total.Statements/2, "most statements succeed on both sides")
		is.True(t, total.Errors > 0, "some statements fail on both sides")
		is.True(t, total.Rows > 0, "rows are returned")
		is.True(t, total.Ordered > 0, "some comparisons are ordered")
	})
}
