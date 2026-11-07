package exec

import (
	"context"
	"fmt"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// A subquery in FROM and a CTE are tables of the statement, whose rows maragudb evaluates once,
// before the statement reads any row, and the first time for a CTE that only another CTE reads.
// Postgres merges such a query into the query around when it can, as it inlines a CTE that is
// read once, and then evaluates the conditions of both on the rows of its tables, in an order
// and through scans that it picks by cost; it pushes the conditions on the columns of the others
// into them. So maragudb rejects a condition of the query around that can fail and reads their
// columns, and one that reads a column of a query that can fail, and evaluates the conditions of
// such a query without any index conditions, on every row of its tables. A query that fails when
// it is evaluated is rejected, because Postgres may never evaluate it, or only some of its rows.

// derivedRows are the rows of a subquery in FROM or of a CTE, evaluated when first needed.
type derivedRows struct {
	load func() ([][]value.Value, error)
	done bool
	rows [][]value.Value
	err  error
	// fallible is set when the query binds an expression that can fail.
	fallible bool
}

// get the rows, evaluating them the first time.
func (d *derivedRows) get() ([][]value.Value, error) {
	if !d.done {
		d.rows, d.err = d.load()
		d.done = true
	}
	return d.rows, d.err
}

// cteScope is a CTE that the queries of a statement can read by name, linked to those defined
// before it, of its own WITH clause and of the queries around.
type cteScope struct {
	name string
	def  *table
	// level of the query whose WITH clause defines it.
	level  *levelState
	parent *cteScope
}

// find the CTE of a name, the nearest one.
func (c *cteScope) find(name string) *cteScope {
	for ; c != nil; c = c.parent {
		if c.name == name {
			return c
		}
	}
	return nil
}

// with binds the queries of a WITH clause in order, each of which reads those before it, as
// Postgres analyzes them before the FROM of their query.
func (b *binder) with(ctx context.Context, ctes []ast.CTE) error {
	seen := map[string]bool{}
	for _, c := range ctes {
		if seen[c.Name] {
			return fmt.Errorf("%w: WITH query name %q specified more than once", ErrInvalid, c.Name)
		}
		seen[c.Name] = true
		t, err := b.derived(ctx, c.Select, c.Name, c.Columns, func(have, want int) error {
			return fmt.Errorf("%w: WITH query %q has %d columns available but %d columns specified", ErrInvalid, c.Name, have, want)
		})
		if err != nil {
			return err
		}
		b.ctes = &cteScope{name: c.Name, def: t, level: b.level, parent: b.ctes}
	}
	return nil
}

// derived binds the query sel of a subquery in FROM or of a CTE, with names for its first
// columns, and returns it as a table of that name, whose rows it evaluates when first needed. Its
// names resolve in the queries around the query of b, but not in b's own FROM, as Postgres has
// no LATERAL here.
func (b *binder) derived(ctx context.Context, sel *ast.Select, name string, names []string, tooMany func(have, want int) error) (*table, error) {
	if b.run == nil {
		return nil, fmt.Errorf("%w: a subquery is not supported here", ErrUnsupported)
	}
	params, now, run, outer, sub, ctes, wide := b.params, b.now, b.run, b.outer, b.sub, b.ctes, b.wide
	fresh := func(analyzing bool, wide bool) *binder {
		return &binder{params: params, now: now, run: run, outer: outer, sub: sub, ctes: ctes, wide: wide, level: &levelState{}, analyzing: analyzing, aggClause: "JOIN conditions"}
	}
	ab := fresh(true, wide)
	res, err := b.run.s.query(ctx, sel, ab)
	if err != nil {
		return nil, err
	}
	if len(names) > len(res.Columns) {
		return nil, tooMany(len(res.Columns), len(names))
	}
	t := &table{Name: name}
	for i, c := range res.Columns {
		col := column{Name: c.Name, Type: c.Type}
		if i < len(names) {
			col.Name = names[i]
		}
		t.Columns = append(t.Columns, col)
		t.PrimaryKey = append(t.PrimaryKey, i)
	}
	fallible := ab.level.fallible
	s := b.run.s
	t.derived = &derivedRows{fallible: fallible, load: func() ([][]value.Value, error) {
		s.snapshotReads++
		defer func() { s.snapshotReads-- }()
		res, err := s.query(ctx, sel, fresh(false, wide || fallible))
		if err != nil {
			return nil, planDependent(err)
		}
		return res.Rows, nil
	}}
	return t, nil
}

// derivedLimit rejects a LIMIT or OFFSET on the rows of a subquery in FROM or a CTE with a
// column of double precision or numeric, whose rows can be equal by every column and still print
// differently, as -0 and 0 do, so that no ORDER BY determines which of them Postgres returns.
// Without such columns, the ORDER BY on every column that the primary key of the table requires
// leaves only rows that are the same.
func (t *table) derivedLimit() error {
	if t.derived == nil {
		return nil
	}
	for _, c := range t.Columns {
		if c.Type == value.Float || c.Type == value.Decimal {
			return fmt.Errorf("%w: LIMIT and OFFSET on the rows of a subquery or CTE %q with a column of type %v are not supported, because equal values that print differently, such as -0 and 0, make the rows Postgres returns depend on its plan", ErrUnsupported, t.Name, c.Type)
		}
	}
	return nil
}

// materialize evaluates the rows of the subqueries in FROM and the CTEs that the query of b
// reads, before it reads any row, because Postgres may read them first.
func (b *binder) materialize() error {
	if b.scope == nil {
		return nil
	}
	for _, e := range b.scope.entries {
		if e.t.derived != nil {
			if _, err := e.t.derived.get(); err != nil {
				return err
			}
		}
	}
	return nil
}

// derivedConditions rejects a condition that reads a column of a subquery in FROM or a CTE when
// it can fail, or when the query can, because Postgres may evaluate it on rows that the query
// does not have, after it merges the query into the one around. See [binder.derived].
func (b *binder) derivedConditions(clauses []clause) error {
	sc, _ := b.names()
	if sc == nil {
		return nil
	}
	// fails reports whether e can fail on the columns of a subquery in FROM or a CTE: when it can
	// fail and reads one, or reads one whose query can fail. The sides of a comparison are
	// judged on their own, because only the side that reads the columns is evaluated on them.
	var fails func(e ast.Expr, ce *cexpr) (int, bool)
	fails = func(e ast.Expr, ce *cexpr) (int, bool) {
		if bin, ok := e.(*ast.Binary); ok && comparisonOps[bin.Op] && ce.fallible {
			for _, side := range []ast.Expr{bin.Left, bin.Right} {
				sce, err := b.fresh().bindPruned(side)
				if err != nil {
					return 0, false
				}
				if i, ok := fails(side, sce); ok {
					return i, true
				}
			}
			// The comparison itself cannot fail.
			ce = &cexpr{}
		}
		for _, i := range b.tablesOf(e).members() {
			if d := sc.entries[i].t.derived; d != nil && (ce.fallible || d.fallible) {
				return i, true
			}
		}
		return 0, false
	}
	for _, c := range clauses {
		if i, ok := fails(c.expr, c.ce); ok {
			return fmt.Errorf("%w: %w: the condition %s reads the subquery or CTE %q and it or the subquery can fail, and Postgres may evaluate it on rows that are not in the subquery after it merges the subquery into the query around; write the condition so that it cannot fail, or filter inside the subquery", ErrUnsupported, errPlanDependentSubquery, ast.RenderExpr(c.expr), sc.entries[i].name)
		}
	}
	return nil
}
