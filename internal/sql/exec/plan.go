package exec

import (
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// access describes how a statement reads a table's rows: a single key, a key range, or
// nothing at all when the WHERE clause can never be true.
type access struct {
	// point is the key of the one row that can match, when set.
	point []byte
	// start and end bound a range scan otherwise.
	start, end []byte
	// empty means no row can match.
	empty bool
}

// plan the access path for a WHERE clause over t. The conjuncts of the WHERE clause that compare
// a primary key column with a constant narrow the scan: equalities on a prefix of the key
// columns, and a range on the column after them. Every row read is still filtered by the whole
// WHERE clause, so the plan only has to be sound, never exact.
func (b *binder) plan(t *table, where ast.Expr) (access, error) {
	start, end := t.rowRange()
	a := access{start: start, end: end}
	if where == nil {
		return a, nil
	}
	type bound struct {
		v         value.Value
		inclusive bool
	}
	eq := map[int]value.Value{}
	lower := map[int]bound{}
	upper := map[int]bound{}
	for _, c := range conjuncts(where) {
		col, op, k, ok, err := b.keyComparison(t, c)
		if err != nil {
			return a, err
		}
		if !ok {
			continue
		}
		if k.Null {
			return access{empty: true}, nil
		}
		switch op {
		case ast.Eq:
			if prev, dup := eq[col]; dup && value.Compare(prev, k) != 0 {
				return access{empty: true}, nil
			}
			eq[col] = k
		case ast.Gt, ast.Ge:
			if prev, has := lower[col]; !has || value.Compare(k, prev.v) > 0 {
				lower[col] = bound{k, op == ast.Ge}
			}
		case ast.Lt, ast.Le:
			if prev, has := upper[col]; !has || value.Compare(k, prev.v) < 0 {
				upper[col] = bound{k, op == ast.Le}
			}
		}
	}
	prefix := start
	for i, col := range t.PrimaryKey {
		if k, ok := eq[col]; ok {
			prefix = value.EncodeKey(prefix, k)
			if i == len(t.PrimaryKey)-1 {
				return access{point: prefix}, nil
			}
			continue
		}
		a.start, a.end = prefix, append(append([]byte{}, prefix...), 0xFF)
		if lo, ok := lower[col]; ok {
			a.start = value.EncodeKey(append([]byte{}, prefix...), lo.v)
		}
		if hi, ok := upper[col]; ok {
			a.end = append(value.EncodeKey(append([]byte{}, prefix...), hi.v), 0xFF)
		}
		return a, nil
	}
	return a, nil
}

func conjuncts(e ast.Expr) []ast.Expr {
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.And {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	if bt, ok := e.(*ast.Between); ok && !bt.Not {
		return []ast.Expr{
			&ast.Binary{Op: ast.Ge, Left: bt.Expr, Right: bt.Low},
			&ast.Binary{Op: ast.Le, Left: bt.Expr, Right: bt.High},
		}
	}
	return []ast.Expr{e}
}

var flipped = map[ast.BinaryOp]ast.BinaryOp{ast.Eq: ast.Eq, ast.Lt: ast.Gt, ast.Le: ast.Ge, ast.Gt: ast.Lt, ast.Ge: ast.Le}

// keyComparison recognizes `column op constant` or `constant op column` on a primary key column
// and returns the column index, the operator as seen from the column, and the constant in the
// column's type.
func (b *binder) keyComparison(t *table, e ast.Expr) (int, ast.BinaryOp, value.Value, bool, error) {
	bin, ok := e.(*ast.Binary)
	if !ok {
		return 0, 0, value.Value{}, false, nil
	}
	op, ok := flipped[bin.Op]
	if !ok {
		return 0, 0, value.Value{}, false, nil
	}
	ref, isRef := bin.Left.(*ast.ColumnRef)
	other := bin.Right
	if isRef {
		op = bin.Op
	} else {
		if ref, isRef = bin.Right.(*ast.ColumnRef); !isRef {
			return 0, 0, value.Value{}, false, nil
		}
		other = bin.Left
	}
	col, ok := t.columnIndex(ref.Name)
	if !ok || !t.isPrimaryKey(col) {
		return 0, 0, value.Value{}, false, nil
	}
	hasRef := false
	ast.Walk(other, func(x ast.Expr) bool {
		if _, ok := x.(*ast.ColumnRef); ok {
			hasRef = true
		}
		return !hasRef
	})
	if hasRef {
		return 0, 0, value.Value{}, false, nil
	}
	// Binding the whole comparison checks the types, so the constant below is known to be
	// comparable with the column once adapted.
	if _, err := b.bind(bin); err != nil {
		return 0, 0, value.Value{}, false, err
	}
	k, err := b.bind(other)
	if err != nil {
		return 0, 0, value.Value{}, false, err
	}
	switch {
	case k.val.Null:
		return col, op, value.NewNull(t.Columns[col].Type), true, nil
	case k.typ == t.Columns[col].Type:
		return col, op, k.val, true, nil
	case k.typ == value.Int && t.Columns[col].Type == value.Float:
		return col, op, value.NewFloat(float64(k.val.I)), true, nil
	}
	return 0, 0, value.Value{}, false, nil
}
