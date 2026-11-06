package pgdiff

import (
	"math"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
)

func parseSelect(t *testing.T, sql string) *ast.Select {
	t.Helper()
	st, err := parser.Parse(sql)
	is.NotError(t, err, sql)
	sel, ok := st.(*ast.Select)
	is.True(t, ok, sql)
	return sel
}

func TestFullyOrdered(t *testing.T) {
	tests := []struct {
		sql  string
		want bool
	}{
		{"SELECT a, b FROM t ORDER BY a", true},
		{"SELECT b FROM t ORDER BY 1", false},
		{"SELECT a, b FROM t ORDER BY 1", true},
		{"SELECT b, a FROM t ORDER BY 2 DESC", true},
		{"SELECT b AS x, a AS y FROM t ORDER BY 2", true},
		{"SELECT *, a FROM t ORDER BY 2", false},
		{"SELECT b AS a FROM t ORDER BY a", false},
		{"SELECT b FROM t ORDER BY +1", false},
		{"SELECT a + 0 FROM t ORDER BY 1", false},
		{"SELECT b FROM t", false},
	}
	for _, test := range tests {
		t.Run(test.sql, func(t *testing.T) {
			is.Equal(t, test.want, FullyOrdered(parseSelect(t, test.sql), []string{"a"}))
		})
	}
}

func TestSortedColumns(t *testing.T) {
	tests := []struct {
		sql     string
		columns []string
		want    []int
	}{
		{"SELECT a, b FROM t ORDER BY b, 1", []string{"a", "b"}, []int{1, 0}},
		{"SELECT * FROM t ORDER BY 2", []string{"a", "b", "c"}, []int{1}},
		{"SELECT a + 1 AS x FROM t ORDER BY x DESC", []string{"x"}, []int{0}},
		{"SELECT a FROM t ORDER BY a, c, 1", []string{"a"}, []int{0}},
		{"SELECT a FROM t ORDER BY c, a", []string{"a"}, nil},
		{"SELECT a FROM t ORDER BY a + 1", []string{"a"}, nil},
		{"SELECT a FROM t ORDER BY 2", []string{"a"}, nil},
	}
	for _, test := range tests {
		t.Run(test.sql, func(t *testing.T) {
			is.EqualSlice(t, test.want, sortedColumns(parseSelect(t, test.sql), test.columns))
		})
	}
}

func TestSortKeys(t *testing.T) {
	t.Run("renders the chosen columns, with -0 and 0 alike", func(t *testing.T) {
		rows := [][]any{{int64(1), math.Copysign(0, -1), "x"}, {int64(2), 0.0, nil}}
		got := sortKeys(rows, []int{2, 1})
		is.EqualSlice(t, []string{`"x" 0(0)`, `NULL 0(0)`}, got)
	})
}
