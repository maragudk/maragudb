package pgdiff_test

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
	"maragu.dev/maragudb/internal/verify"
)

func TestRunner_Run(t *testing.T) {
	create := &ast.CreateTable{Name: "t", Columns: []ast.ColumnDef{{Name: "c0", Type: value.Int, NotNull: true}}, PrimaryKey: []string{"c0"}}
	insert := &ast.Insert{Table: "t", Rows: [][]ast.Expr{
		{&ast.Literal{Value: value.NewInt(0)}},
		{&ast.Literal{Value: value.NewInt(math.MinInt64)}},
	}}
	c0 := &ast.ColumnRef{Name: "c0"}

	t.Run("accepts different errors that agree once Postgres visits the rows in key order", func(t *testing.T) {
		r := newRunner(t)
		sel := &ast.Select{From: "t", Items: []ast.SelectItem{
			{Expr: &ast.Binary{Op: ast.Div, Left: c0, Right: c0}},
			{Expr: &ast.Call{Name: "abs", Args: []ast.Expr{c0}}},
		}}
		rep, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{insert, sel}})
		is.NotError(t, err)
		is.Equal(t, 1, rep.Reordered)
		is.Equal(t, 1, rep.ErrorClasses["out of range/22003"])
	})

	t.Run("also inside a transaction, which stays aborted on both sides", func(t *testing.T) {
		r := newRunner(t)
		sel := &ast.Select{From: "t", Items: []ast.SelectItem{
			{Expr: &ast.Binary{Op: ast.Div, Left: c0, Right: c0}},
			{Expr: &ast.Call{Name: "abs", Args: []ast.Expr{c0}}},
		}}
		rep, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{&ast.Begin{}, insert, sel, insert, &ast.Commit{}, sel}})
		is.NotError(t, err)
		is.Equal(t, 1, rep.Reordered)
		is.Equal(t, 1, rep.ErrorClasses["transaction aborted/25P02"], "the insert after the failure is ignored")
		is.Equal(t, 1, rep.Transactions)
		is.Equal(t, 6, rep.OK+rep.Errors, "every statement runs on both sides, and the last finds the table empty")
	})

	t.Run("also inside a read-only transaction, whose rows are the committed ones", func(t *testing.T) {
		r := newRunner(t)
		sel := &ast.Select{From: "t", Items: []ast.SelectItem{
			{Expr: &ast.Binary{Op: ast.Div, Left: c0, Right: c0}},
			{Expr: &ast.Call{Name: "abs", Args: []ast.Expr{c0}}},
		}}
		rep, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{insert, &ast.Begin{ReadOnly: true}, sel, sel, &ast.Commit{}, sel}})
		is.NotError(t, err)
		is.Equal(t, 2, rep.Reordered, "the first and the last select")
		is.Equal(t, 2, rep.ErrorClasses["out of range/22003"])
		is.Equal(t, 1, rep.ErrorClasses["transaction aborted/25P02"], "the transaction stays aborted")
	})

	t.Run("reports a transaction that is aborted on Postgres only as a mismatch, without a rerun", func(t *testing.T) {
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
		_, err = pgConn.ExecContext(t.Context(), "DROP TABLE IF EXISTS aborted")
		is.NotError(t, err)
		s := pgdiff.NewSession(pgConn, mConn)
		for _, q := range []string{"CREATE TABLE aborted (c0 bigint PRIMARY KEY)", "INSERT INTO aborted VALUES (0), (-9223372036854775808)", "BEGIN READ ONLY"} {
			_, err := s.Exec(t.Context(), q, &pgdiff.Report{})
			is.NotError(t, err, q)
		}
		_, err = pgConn.ExecContext(t.Context(), "SELECT 1 / 0")
		is.Equal(t, "22012", pgdiff.SQLState(err), "only the Postgres transaction is aborted")
		_, err = s.Exec(t.Context(), "SELECT c0 / c0, abs(c0) FROM aborted", &pgdiff.Report{})
		is.True(t, pgdiff.IsMismatch(err), fmt.Sprint(err))
		is.True(t, err != nil && strings.Contains(err.Error(), "postgres SQLSTATE 25P02, maragudb out of range"), fmt.Sprint(err))
	})

	t.Run("skips a rejected statement on Postgres, and aborts the transaction there instead", func(t *testing.T) {
		verify.Covers(t, "a statement that maragudb rejects as unsupported is counted as rejected and not run on Postgres, and inside a transaction both transactions end up aborted", "AC-4.5")
		r := newRunner(t)
		unsupported := &ast.Select{From: "t", Items: []ast.SelectItem{{Expr: c0}}, Limit: &ast.Literal{Value: value.NewInt4(1)}}
		rep, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{insert, unsupported, &ast.Begin{}, unsupported, insert, &ast.Rollback{}}})
		is.NotError(t, err)
		is.Equal(t, 2, rep.Rejected)
		is.Equal(t, 4, rep.Executed)
		is.Equal(t, 1, rep.ErrorClasses["transaction aborted/25P02"], "the insert after the rejection fails on both")
		is.Equal(t, 1, len(rep.RejectedFor))
	})

	t.Run("reports errors of different classes as a mismatch", func(t *testing.T) {
		verify.Covers(t, "a statement that fails on both sides for different reasons is a mismatch", "AC-4.3")
		// The store of maragudb fails every read once the table exists, that is after CREATE TABLE
		// wrote the first segment, so that a duplicate key, which Postgres reports, is a store
		// error on maragudb.
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
		r := &pgdiff.Runner{PG: pgdiff.Postgres(t), New: func() (*sql.DB, error) {
			c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(fake), Maintenance: new(false), MemoryCacheBytes: new(int64(0))}, "s3://bucket/mismatch")
			if err != nil {
				return nil, err
			}
			return sql.OpenDB(c), nil
		}}
		dup := &ast.Insert{Table: "t", Rows: [][]ast.Expr{{&ast.Literal{Value: value.NewInt4(1)}}, {&ast.Literal{Value: value.NewInt4(1)}}}}
		_, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{dup}})
		is.True(t, pgdiff.IsMismatch(err), fmt.Sprint(err))
		is.True(t, err != nil && strings.Contains(err.Error(), "both failed, for different reasons: postgres SQLSTATE 23505, maragudb unclassified error"), fmt.Sprint(err))
	})
}
