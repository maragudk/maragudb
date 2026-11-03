package pgdiff_test

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
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

	t.Run("reports errors of different classes as a mismatch", func(t *testing.T) {
		r := newRunner(t)
		// maragudb rejects a string literal for an int column as a type error, where Postgres
		// reports invalid input. See deviation 17.
		bad := &ast.Insert{Table: "t", Rows: [][]ast.Expr{{&ast.Literal{Value: value.NewString("a")}}}}
		_, err := r.Run(t.Context(), pgdiff.Case{Create: create, Statements: []ast.Statement{bad}})
		is.True(t, pgdiff.IsMismatch(err), "a mismatch")
		is.True(t, err != nil && strings.Contains(err.Error(), "both failed, for different reasons: postgres SQLSTATE 22P02, maragudb type"), fmt.Sprint(err))
	})
}
