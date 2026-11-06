package exec

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// A SELECT is grouped when it has GROUP BY or HAVING, or an aggregate in its output, HAVING, or
// ORDER BY. Its input rows, those of its FROM that its WHERE clause admits, fall into groups
// by the values of the GROUP BY expressions, or into one group without GROUP BY, and its
// output, HAVING, and ORDER BY are evaluated once per group, on a group row: the first input
// row of the group, then the values of the GROUP BY expressions, then the results of the
// aggregates.
//
// Postgres analyzes such a statement in three steps, which the binder follows so that the same
// error comes first: it analyzes every clause on its own, then it finds the GROUP BY items, of
// which a name or position refers to an output column, and only then checks that the output,
// ORDER BY, and HAVING read the columns of the input rows only through a GROUP BY expression,
// through an aggregate, or as a column of a table whose primary key the GROUP BY has.

// grouped reports whether a SELECT is grouped.
func grouped(st *ast.Select) bool {
	if len(st.GroupBy) > 0 || st.Having != nil {
		return true
	}
	for _, item := range st.Items {
		if !item.Star && ast.HasAggregate(item.Expr) {
			return true
		}
	}
	for _, o := range st.OrderBy {
		if ast.HasAggregate(o.Expr) {
			return true
		}
	}
	return false
}

// aggScope is how the binder binds aggregates and columns in a grouped SELECT.
type aggScope struct {
	// g is the grouping whose group rows the expressions read, or nil while the statement is
	// analyzed before its grouping is known, when columns read the input rows and aggregates
	// are only checked.
	g *grouping
	// nested is set while the arguments of an aggregate are bound, which read the input rows,
	// and found is set when they have an aggregate of their own.
	nested bool
	found  *bool
}

// grouping of a grouped SELECT: its GROUP BY expressions and its aggregates.
type grouping struct {
	// width of the input rows, which the group rows begin with.
	width int
	keys  []*groupKey
	// nonVar is set when a key is not a column, so that every subexpression of the output may be
	// one.
	nonVar bool
	// dependent are the entries whose primary key columns are all keys, so that every row of a
	// group has the same values in all their columns.
	dependent map[int]bool
	aggs      []*aggregate
	// input binds the expressions of the input rows, for messages and the text of expressions.
	input *binder
}

// groupKey is a GROUP BY expression.
type groupKey struct {
	expr ast.Expr
	// canon is its [binder.canonical] text, by which other expressions match it.
	canon string
	// ref is the column it is, when plain is set.
	ref   colRef
	plain bool
	// ce is the expression bound in the input rows.
	ce *cexpr
	// used is set when an expression of the output, HAVING, or ORDER BY reads its value.
	used bool
}

// aggregate is a call of an aggregate function in a grouped SELECT.
type aggregate struct {
	name     string
	star     bool
	distinct bool
	// arg is the argument bound in the input rows, or nil for COUNT(*), and argRef the column it
	// is, when argPlain is set.
	arg      *cexpr
	argRef   colRef
	argPlain bool
	// typ is the type of the result.
	typ value.Type
	// key identifies the aggregate, so that equal calls share one.
	key string
}

// slot is an expression that reads the value at index at of the group row.
func slot(typ value.Type, at int) *cexpr {
	return &cexpr{typ: typ, refs: true, eval: func(row []value.Value) (value.Value, error) {
		return row[at], nil
	}}
}

// errGrouping is the error of a column that a grouped SELECT reads outside of the GROUP BY
// expressions and the aggregates.
func (b *binder) errGrouping(r colRef) error {
	e := b.scope.entries[r.entry]
	return fmt.Errorf("%w: column %q must appear in the GROUP BY clause or be used in an aggregate function", ErrGrouping, e.name+"."+e.columns[r.col])
}

// groupedExpr binds e as an expression of the group rows when it is a GROUP BY expression, or a
// column, and reports whether it did. Other expressions bind as usual, and their parts come
// back here, as Postgres's check of a grouped query does: a subexpression that equals a GROUP BY
// expression is that expression, an aggregate reads its own arguments, a constant is a
// constant, and a column that is none of these is an error, unless the GROUP BY has the
// primary key of its table.
func (b *binder) groupedExpr(e ast.Expr) (*cexpr, bool, error) {
	g := b.agg.g
	switch e.(type) {
	case *ast.Literal, *ast.Param:
		return nil, false, nil
	}
	if ast.IsAggregate(e) {
		return nil, false, nil
	}
	if g.nonVar {
		canon := g.input.canonical(e)
		for i, k := range g.keys {
			if k.canon == canon {
				k.used = true
				return slot(k.ce.typ, g.width+i), true, nil
			}
		}
	}
	ref, ok := e.(*ast.ColumnRef)
	if !ok {
		return nil, false, nil
	}
	r, err := b.resolve(ref)
	if err != nil {
		return nil, true, err
	}
	for i, k := range g.keys {
		if k.plain && k.ref == r {
			k.used = true
			return slot(k.ce.typ, g.width+i), true, nil
		}
	}
	if g.dependent[r.entry] {
		return b.column(r), true, nil
	}
	return nil, true, b.errGrouping(r)
}

// aggregate binds a call of an aggregate function: its arguments in the input rows, then its
// type, and then whether an aggregate is allowed where it is, in the order in which Postgres
// checks them.
func (b *binder) aggregate(e *ast.Call) (*cexpr, error) {
	saved := b.agg
	found := false
	b.agg = &aggScope{nested: true, found: &found}
	if saved != nil {
		b.agg.g = saved.g
	}
	args := make([]*cexpr, len(e.Args))
	var err error
	for i, a := range e.Args {
		if args[i], err = b.bind(a); err != nil {
			break
		}
	}
	b.agg = saved
	if err != nil {
		return nil, err
	}
	agg := &aggregate{name: e.Name, star: e.Star, distinct: e.Distinct}
	if err := b.aggregateType(agg, e, args); err != nil {
		return nil, err
	}
	if len(e.Args) == 1 {
		if ref, ok := e.Args[0].(*ast.ColumnRef); ok {
			if r, err := b.resolve(ref); err == nil {
				agg.argRef, agg.argPlain = r, true
			}
		}
	}
	switch {
	case found:
		return nil, fmt.Errorf("%w: aggregate function calls cannot be nested", ErrGrouping)
	case b.agg == nil:
		clause := cmp.Or(b.aggClause, "this clause")
		return nil, fmt.Errorf("%w: aggregate functions are not allowed in %s", ErrGrouping, clause)
	case b.agg.nested:
		*b.agg.found = true
		return &cexpr{typ: agg.typ, refs: true}, nil
	case b.agg.g == nil:
		return &cexpr{typ: agg.typ, refs: true}, nil
	}
	g := b.agg.g
	agg.key = ast.RenderExpr(g.input.normalize(e))
	i := slices.IndexFunc(g.aggs, func(a *aggregate) bool { return a.key == agg.key })
	if i < 0 {
		i = len(g.aggs)
		g.aggs = append(g.aggs, agg)
	}
	ce := slot(agg.typ, g.width+len(g.keys)+i)
	ce.aggs = []int{i}
	return ce, nil
}

// aggregateType resolves the function of an aggregate call by the types of its arguments, as
// Postgres does, and sets the aggregate's argument and type.
func (b *binder) aggregateType(agg *aggregate, e *ast.Call, args []*cexpr) error {
	notExist := func() error {
		var types []string
		for _, a := range args {
			types = append(types, a.typ.String())
		}
		return fmt.Errorf("%w: function %s(%s) does not exist", ErrType, e.Name, strings.Join(types, ", "))
	}
	if e.Star {
		if e.Name != "count" {
			return notExist()
		}
		agg.typ = value.Int
		return nil
	}
	if len(args) != 1 {
		return notExist()
	}
	x := args[0]
	if x.param > 0 {
		return fmt.Errorf("%w: parameter $%d as the argument of %s is not supported, because its type is not known; cast it, such as $%d::bigint", ErrUnsupported, x.param, e.Name, x.param)
	}
	if x.typ == value.Numeric && e.Name != "count" {
		return fmt.Errorf("%w: the numeric type is not supported, cast the literal %s to double precision or bigint", ErrUnsupported, x.val.S)
	}
	switch e.Name {
	case "count":
		agg.typ = value.Int
	case "sum", "avg":
		switch x.typ {
		case value.Unknown:
			return fmt.Errorf("%w: function %s(unknown) is not unique", ErrType, e.Name)
		case value.Int4:
			agg.typ = value.Decimal
			if e.Name == "sum" {
				agg.typ = value.Int
			}
		case value.Int, value.Decimal:
			agg.typ = value.Decimal
		case value.Float:
			agg.typ = value.Float
		default:
			return notExist()
		}
	default:
		switch x.typ {
		case value.Unknown:
			// An unknown argument is text, the preferred type of the string category, which
			// Postgres picks first.
			var err error
			if x, err = resolve(x, value.String); err != nil {
				return err
			}
		case value.Bool:
			return notExist()
		}
		agg.typ = x.typ
	}
	agg.arg = x
	return nil
}

// selectGrouped runs a grouped SELECT, whose FROM the binder has bound.
func (s *Session) selectGrouped(ctx context.Context, st *ast.Select, b *binder, nodes []*fromNode) (*Rows, error) {
	var t *table
	if b.scope != nil && len(b.scope.entries) == 1 {
		t = b.scope.entries[0].t
		b.table, b.entry = t, 0
	}
	analysis := func(phase int) *binder {
		ab := b.fresh()
		ab.phase, ab.agg = phase, &aggScope{}
		return ab
	}

	// Every clause on its own, in the order of Postgres's analysis.
	o, err := analysis(phaseOutput).selectList(st)
	if err != nil {
		return nil, err
	}
	b.phase, b.aggClause = phaseWhere, "WHERE"
	if _, err := b.where(st.Where, "WHERE"); err != nil {
		return nil, err
	}
	if st.Having != nil {
		ab := analysis(phaseHaving)
		ce, err := ab.bind(st.Having)
		if err == nil {
			ce, err = resolve(ce, value.Bool)
		}
		if err != nil {
			return nil, err
		}
		if ce.typ != value.Bool {
			return nil, fmt.Errorf("%w: argument of HAVING must be type boolean, not type %v", ErrType, ce.typ)
		}
	}
	// Postgres checks that the ORDER BY of DISTINCT is in the output after GROUP BY.
	undistinct := *st
	undistinct.Distinct = false
	if _, err := analysis(phaseOutput).orderBy(&undistinct, o); err != nil {
		return nil, err
	}
	g, err := b.grouping(st, o)
	if err != nil {
		return nil, err
	}
	if st.Distinct {
		if _, err := analysis(phaseOutput).orderBy(st, o); err != nil {
			return nil, err
		}
	}
	offsetExpr, limitExpr, err := b.limits(st)
	if err != nil {
		return nil, err
	}

	// The check of the grouping, which binds the output, ORDER BY, and HAVING in the group rows,
	// and folds them with the GROUP BY expressions.
	b.phase, b.agg = phaseOutput, &aggScope{g: g}
	if o, err = b.selectList(st); err != nil {
		return nil, err
	}
	order, err := b.orderBy(st, o)
	if err != nil {
		return nil, err
	}
	b.agg, b.aggClause = nil, "GROUP BY"
	for _, k := range g.keys {
		if k.ce, err = b.bind(k.expr); err != nil {
			return nil, err
		}
		if k.ce, err = resolve(k.ce, value.String); err != nil {
			return nil, err
		}
	}
	b.phase, b.agg = phaseHaving, &aggScope{g: g}
	h, err := b.having(st, len(st.GroupBy) > 0)
	if err != nil {
		return nil, err
	}
	b.agg = nil
	if err := b.folding(); err != nil {
		return nil, err
	}

	// The WHERE clause, with the conditions of HAVING that Postgres moves into it.
	where := h.moved
	if st.Where != nil {
		where = append([]ast.Expr{b.normalize(st.Where)}, where...)
	}
	// The aggregates in what moves are in branches that folding drops, as in a COALESCE after a
	// constant, so they bind without being computed.
	wb := b.fresh()
	wb.phase, wb.aggClause, wb.agg = phaseWhere, "WHERE", &aggScope{}
	var f filter
	if len(where) > 0 {
		if f, err = wb.where(join(where, ast.And), "WHERE"); err != nil {
			return nil, err
		}
	}
	if f.unsupported != nil {
		return nil, f.unsupported
	}

	r := newAggRun(g, o, order, h)
	if st.Limit != nil || st.Offset != nil {
		if err := r.limitable(st, f); err != nil {
			return nil, err
		}
	}
	if t == nil && len(nodes) > 0 {
		return s.selectJoin(ctx, st, b, nodes, f, o, order, offsetExpr, limitExpr, r)
	}
	offset, err := limitValue(offsetExpr, "OFFSET")
	if err != nil {
		return nil, err
	}
	limit, err := limitValue(limitExpr, "LIMIT")
	if err != nil {
		return nil, err
	}
	if limit == 0 {
		// Postgres does not read any row under LIMIT 0.
		return &Rows{Columns: o.columns, Rows: [][]value.Value{}}, nil
	}
	if t == nil {
		open, err := f.open()
		if err != nil {
			return nil, err
		}
		if ok, err := f.matches(nil); err != nil {
			return nil, err
		} else if ok && open {
			r.add(nil)
		}
	} else {
		var sortCols []int
		for _, ref := range r.sortRefs() {
			sortCols = append(sortCols, ref.col)
		}
		r.minmax = len(g.keys) == 0 && r.onlyMinMax()
		f.candidates = t.candidates(f, sortCols, b.reads(st, 0))
		f.failures = &r.whereFailures
		err := s.scanRows(ctx, t, t.plan(f, nil), f, func(_ []byte, row []value.Value) (bool, error) {
			r.add(row)
			return true, nil
		})
		switch {
		case errors.Is(err, errOrderDependent):
			r.orderDependent = err
		case err != nil:
			return nil, err
		}
	}
	return r.rows(st.Distinct, offset, limit)
}

// grouping finds the GROUP BY items of a SELECT with the output o, as Postgres does: an integer
// constant is the position of an output column, a name is a column of FROM or, when FROM has
// none of that name, the name of an output column, and anything else is an expression. An item
// that is an output column must not have an aggregate.
func (b *binder) grouping(st *ast.Select, o output) (*grouping, error) {
	g := &grouping{dependent: map[int]bool{}}
	if b.scope != nil {
		g.width = b.scope.width
	}
	g.input = b.fresh()
	g.input.agg = &aggScope{}
	eb := b.fresh()
	eb.phase, eb.aggClause = phaseOutput, "GROUP BY"
	for _, item := range st.GroupBy {
		output := -1
		switch e := item.(type) {
		case *ast.Literal:
			if e.Value.Null || e.Value.Type != value.Int4 || e.Value.I == math.MinInt32 {
				return nil, fmt.Errorf("%w: non-integer constant in GROUP BY", ErrSyntax)
			}
			if e.Value.I < 1 || e.Value.I > int64(len(o.columns)) {
				return nil, fmt.Errorf("%w: GROUP BY position %d is not in select list", ErrInvalid, e.Value.I)
			}
			output = int(e.Value.I) - 1
		case *ast.ColumnRef:
			if e.Table != "" {
				break
			}
			_, err := b.resolve(e)
			if err == nil {
				break
			}
			if strings.Contains(err.Error(), "ambiguous") {
				return nil, err
			}
			// A name that is no column of FROM is an output column's name. Several output
			// columns of that name are only ambiguous when they are different expressions.
			for i, c := range o.columns {
				if c.Name != e.Name {
					continue
				}
				if output >= 0 && b.canonical(o.exprs[output]) != b.canonical(o.exprs[i]) {
					return nil, fmt.Errorf("%w: GROUP BY %q is ambiguous", ErrInvalid, e.Name)
				}
				if output < 0 {
					output = i
				}
			}
			if output < 0 {
				return nil, err
			}
		}
		expr := item
		if output >= 0 {
			expr = o.exprs[output]
			if ast.HasAggregate(expr) {
				return nil, fmt.Errorf("%w: aggregate functions are not allowed in GROUP BY", ErrGrouping)
			}
		}
		ce, err := eb.bind(expr)
		if err == nil {
			ce, err = resolve(ce, value.String)
		}
		if err != nil {
			return nil, err
		}
		k := &groupKey{expr: b.normalize(expr), canon: g.input.canonical(expr), ce: ce}
		if ref, ok := expr.(*ast.ColumnRef); ok {
			if r, err := b.resolve(ref); err == nil {
				k.ref, k.plain = r, true
			}
		}
		if slices.ContainsFunc(g.keys, func(o *groupKey) bool { return o.canon == k.canon }) {
			continue
		}
		g.keys = append(g.keys, k)
		g.nonVar = g.nonVar || !k.plain || k.ref.cast != value.Unknown
	}
	// A table whose primary key columns are all keys has the same row in every row of a group.
	if b.scope != nil {
		for i, e := range b.scope.entries {
			all := true
			for _, col := range e.t.PrimaryKey {
				all = all && slices.ContainsFunc(g.keys, func(k *groupKey) bool {
					return k.plain && k.ref == colRef{entry: i, col: col}
				})
			}
			g.dependent[i] = all
		}
	}
	return g, nil
}

// havingConds of a grouped SELECT, bound in the group rows. As Postgres does after it folds
// constants, the conjuncts of HAVING without an aggregate move into WHERE, where they filter
// the input rows, and without GROUP BY they also stay in HAVING.
type havingConds struct {
	kept      []*cexpr
	conjuncts []ast.Expr
	moved     []ast.Expr
	// never is set when a conjunct is FALSE or NULL, so that no group passes.
	never bool
}

// having binds the HAVING of a SELECT in the group rows.
func (b *binder) having(st *ast.Select, groupBy bool) (havingConds, error) {
	var h havingConds
	if st.Having == nil {
		return h, nil
	}
	pruned := false
	for _, c := range b.whereConjuncts(b.normalize(st.Having)) {
		var ce *cexpr
		var err error
		if pruned {
			ce, err = b.bindPruned(c)
		} else {
			ce, err = b.bind(c)
		}
		if err == nil {
			ce, err = resolve(ce, value.Bool)
		}
		if err != nil {
			return havingConds{}, err
		}
		if ce.typ != value.Bool {
			return havingConds{}, fmt.Errorf("%w: argument of HAVING must be type boolean, not type %v", ErrType, ce.typ)
		}
		switch {
		case pruned:
			continue
		case ce.folded && !ce.val.Null && ce.val.B:
			continue
		case ce.folded:
			// A FALSE stops the folding of the conjuncts after it, and a NULL does not.
			pruned = !ce.val.Null
			h.never = true
			h.moved = append(h.moved, &ast.Literal{Value: value.NewBool(false)})
			continue
		case len(ce.aggs) > 0:
			h.kept = append(h.kept, ce)
			h.conjuncts = append(h.conjuncts, c)
			continue
		}
		h.moved = append(h.moved, c)
		if !groupBy {
			h.kept = append(h.kept, ce)
			h.conjuncts = append(h.conjuncts, c)
		}
	}
	return h, nil
}
