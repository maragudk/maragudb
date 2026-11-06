package exec

import (
	"fmt"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
	"maragu.dev/maragudb/internal/sql/value"
)

// now is the value of now() in the current transaction: its start, at microsecond precision.
func (s *Session) now() value.Value {
	return value.NewTime(s.tx.StartedAt())
}

// defaultExpr binds the default of column i of t, converted to the column's type, or a NULL of
// the column's type when it has none. [Session.createTable] has checked that it binds and never
// fails.
func (s *Session) defaultExpr(b *binder, t *table, i int) (*cexpr, error) {
	c := t.Columns[i]
	if c.Default == "" {
		return constant(value.NewNull(c.Type)), nil
	}
	e, err := parser.ParseExpr(c.Default)
	if err != nil {
		return nil, fmt.Errorf("exec: corrupt default of column %q of %q: %w", c.Name, t.Name, err)
	}
	bound, err := b.bind(e)
	if err != nil {
		return nil, err
	}
	return b.assignable(bound, t, i)
}

// checkDefault checks the DEFAULT of column i of a new table as Postgres does when it creates the
// table: it reads no columns and no parameters, and it converts to the column's type by
// assignment. maragudb also rejects a default that can fail when a statement uses it. It returns
// the default as SQL text, for the catalog.
func (s *Session) checkDefault(t *table, i int, e ast.Expr) (string, error) {
	var err error
	ast.Walk(e, func(x ast.Expr) bool {
		if _, ok := x.(*ast.ColumnRef); ok && err == nil {
			err = fmt.Errorf("%w: cannot use column reference in DEFAULT expression", ErrUnsupported)
		}
		return err == nil
	})
	if err != nil {
		return "", err
	}
	b := &binder{now: s.now()}
	bound, err := b.bind(e)
	if err != nil {
		return "", err
	}
	if bound, err = b.assignable(bound, t, i); err != nil {
		return "", err
	}
	// Postgres folds the constants of a default only in the statement that uses it, and fails
	// that statement then, in an order among the statement's own errors that maragudb does not
	// reproduce.
	if b.foldErr != nil {
		return "", fmt.Errorf("%w: a DEFAULT that fails when it is evaluated is not supported: %v", ErrUnsupported, b.foldErr)
	}
	if b.stableErr != nil || bound.fallible && !bound.folded {
		return "", fmt.Errorf("%w: a DEFAULT that can fail when it is evaluated is not supported, because whether Postgres evaluates it while it plans depends on the plan", ErrUnsupported)
	}
	return ast.RenderExpr(e), nil
}
