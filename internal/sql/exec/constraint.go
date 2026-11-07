package exec

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"maragu.dev/maragudb/internal/engine"
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
		return nil, fmt.Errorf("%w: default of column %q of %q: %w", engine.ErrCorrupt, c.Name, t.Name, err)
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
	b := &binder{now: s.now(), aggClause: "DEFAULT expressions"}
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

// constraintKey is the catalog key that records that table has a constraint called name.
func constraintKey(name, table string) []byte {
	return []byte("k/" + name + "\x00" + table)
}

// constraintNameExists reports whether a constraint of any table has the name, in the
// transaction.
func (s *Session) constraintNameExists(ctx context.Context, name string) (bool, error) {
	it := s.kv.scan(ctx, []byte("k/"+name+"\x00"), []byte("k/"+name+"\x01"))
	exists := it.Next()
	return exists, it.Err()
}

// names of the constraints of a table that CREATE TABLE creates, in the order it creates them.
type names struct {
	list []string
}

func (n *names) has(name string) bool {
	return slices.Contains(n.list, name)
}

func (n *names) add(name string) {
	n.list = append(n.list, name)
}

// chooseConstraintName as Postgres's ChooseConstraintName does: the name of [makeObjectName], or
// with the label numbered from 1, until no constraint of any table, and none that the statement
// chose before, has the name.
func (s *Session) chooseConstraintName(ctx context.Context, n *names, name1, name2, label string) (string, error) {
	return chooseNumbered(name1, name2, label, func(name string) (bool, error) {
		if n.has(name) {
			return true, nil
		}
		return s.constraintNameExists(ctx, name)
	})
}

// chooseIndexConstraintName for the index of a PRIMARY KEY or UNIQUE constraint, as Postgres's
// ChooseRelationName does for a constraint: like [Session.chooseName], and also until no
// constraint has the name.
func (s *Session) chooseIndexConstraintName(ctx context.Context, n *names, name1, name2, label string) (string, error) {
	return chooseNumbered(name1, name2, label, func(name string) (bool, error) {
		if n.has(name) {
			return true, nil
		}
		if _, exists, err := s.relation(ctx, name); err != nil || exists {
			return true, err
		}
		return s.constraintNameExists(ctx, name)
	})
}

// addChecks checks the CHECK constraints of a new table, in the order they are written, and
// names them as Postgres does: an unnamed one after the column it reads, when it reads exactly
// one, and after the table otherwise.
func (s *Session) addChecks(ctx context.Context, t *table, st *ast.CreateTable, n *names) error {
	for _, c := range st.Checks {
		if _, err := s.bindCheck(t, c.Expr); err != nil {
			return err
		}
		name := c.Name
		switch {
		case name != "" && strings.IndexByte(name, 0) >= 0:
			return fmt.Errorf("%w: constraint name must not contain a NUL byte", ErrInvalid)
		case name != "":
			if slices.ContainsFunc(t.Checks, func(o check) bool { return o.Name == name }) {
				return fmt.Errorf("%w: check constraint %q for relation %q already exists", ErrDuplicateObject, name, t.Name)
			}
		default:
			var cols []string
			ast.Walk(c.Expr, func(x ast.Expr) bool {
				if ref, ok := x.(*ast.ColumnRef); ok && !slices.Contains(cols, ref.Name) {
					cols = append(cols, ref.Name)
				}
				return true
			})
			column := ""
			if len(cols) == 1 {
				column = cols[0]
			}
			var err error
			if name, err = s.chooseConstraintName(ctx, n, t.Name, column, "check"); err != nil {
				return err
			}
		}
		n.add(name)
		t.Checks = append(t.Checks, check{Name: name, Expr: ast.RenderExpr(c.Expr)})
	}
	return nil
}

// addNotNullNames names the NOT NULL constraints of a new table as Postgres 18 does, in the order
// of the columns. Nothing refers to these names, but they take their place among the names of
// constraints.
func (s *Session) addNotNullNames(ctx context.Context, t *table, n *names) error {
	for _, c := range t.Columns {
		if !c.NotNull {
			continue
		}
		name, err := s.chooseConstraintName(ctx, n, t.Name, c.Name, "not_null")
		if err != nil {
			return err
		}
		n.add(name)
	}
	return nil
}

// bindCheck binds the expression of a CHECK constraint of t as Postgres checks it when it creates
// the table: a boolean over the table's columns. maragudb also rejects an expression whose
// constant part fails, which Postgres folds only when the first row is checked.
func (s *Session) bindCheck(t *table, e ast.Expr) (*cexpr, error) {
	b := &binder{table: t, now: s.now(), aggClause: "check constraints"}
	c, err := b.bind(e)
	if err != nil {
		return nil, err
	}
	if c, err = resolve(c, value.Bool); err != nil {
		return nil, err
	}
	if c.typ != value.Bool {
		return nil, fmt.Errorf("%w: argument of CHECK must be type boolean, not type %v", ErrType, c.typ)
	}
	if b.foldErr != nil {
		return nil, fmt.Errorf("%w: a CHECK whose constant part fails is not supported: %v", ErrUnsupported, b.foldErr)
	}
	if b.stableErr != nil {
		return nil, fmt.Errorf("%w: a CHECK with a conversion between text and timestamptz that reads no columns and can fail is not supported", ErrUnsupported)
	}
	return c, nil
}

// boundCheck is a CHECK constraint, bound for a transaction.
type boundCheck struct {
	name string
	expr *cexpr
}

// checksOf t, bound, in the order of their names, which is the order Postgres evaluates them in.
func (s *Session) checksOf(t *table) ([]boundCheck, error) {
	if cs, ok := s.checks[t]; ok {
		return cs, nil
	}
	var cs []boundCheck
	for _, c := range t.Checks {
		e, err := parser.ParseExpr(c.Expr)
		if err != nil {
			return nil, fmt.Errorf("%w: check constraint %q of %q: %w", engine.ErrCorrupt, c.Name, t.Name, err)
		}
		bound, err := s.bindCheck(t, e)
		if err != nil {
			return nil, err
		}
		cs = append(cs, boundCheck{name: c.Name, expr: bound})
	}
	slices.SortFunc(cs, func(a, b boundCheck) int { return strings.Compare(a.name, b.name) })
	if s.checks == nil {
		s.checks = map[*table][]boundCheck{}
	}
	s.checks[t] = cs
	return cs, nil
}

// checkConstraints evaluates the CHECK constraints of t on row, in the order of their names, and
// fails at the first that is false or fails itself. NULL passes.
func (s *Session) checkConstraints(t *table, row []value.Value) error {
	cs, err := s.checksOf(t)
	if err != nil {
		return err
	}
	for _, c := range cs {
		v, err := c.expr.value(row)
		if err != nil {
			return err
		}
		if !v.Null && !v.B {
			return fmt.Errorf("%w: new row for relation %q violates check constraint %q", ErrCheck, t.Name, c.name)
		}
	}
	return nil
}
