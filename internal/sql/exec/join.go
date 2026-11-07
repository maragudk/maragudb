package exec

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// A SELECT on several tables runs as nested loops over its join tree, in the rows of the join,
// which hold the columns of every entry. Postgres chooses the order of the joins and the method
// of each, a nested loop, a hash join, or a merge join, by cost, and pushes every condition down
// to the lowest place where it can be evaluated, a condition on one table into the scan of that
// table. Which rows and pairs of rows a condition is evaluated on therefore depends on the plan:
// a hash join evaluates the other conditions of a join only on the pairs with equal keys, a
// nested loop with an index scan only on the rows the index finds, a merge join stops at the end
// of either side, and a join with an empty side may never scan the other one. The outcome of a
// statement in which no condition fails is the same in every plan, so maragudb accepts a statement
// only when no condition that can fail fails on any row or combination of rows that some plan
// could evaluate it on, and rejects it otherwise, as [joinPlan.check] describes.

// jcond is a condition of a join query: a conjunct of WHERE or of the condition of a join, bound
// in the rows of the join.
type jcond struct {
	expr   ast.Expr
	ce     *cexpr
	tables tableSet
}

// joinPlan of a SELECT on several tables.
type joinPlan struct {
	s  *Session
	b  *binder
	st *ast.Select
	// top is the tree of the FROM items, joined from the left with cross joins.
	top *fromNode
	// all are the conditions of the statement, and conds those that a join evaluates, by the
	// node they are placed at: the conditions of an inner join, and the condition of a left join,
	// which decides the rows of its right side. post are those that filter the rows of a left
	// join, after it extends them with NULLs.
	all   []*jcond
	conds map[*fromNode][]*jcond
	post  map[*fromNode][]*jcond
	// never are the joins whose condition is never true.
	never map[*fromNode]bool
	// scan are the conjuncts that the scan of each entry evaluates, as written, and filters the
	// filters they make, bound in the rows of the entry.
	scan    [][]ast.Expr
	filters []filter
	where   filter
	// lookups are the equalities between a column of an entry and an expression of the entries
	// before it, by entry and by the entries bound before it.
	lookups map[lookupKey][]lookup
	groups  map[*fromNode]*groupRun
	// nulls are the rows of NULLs of each entry.
	nulls [][]value.Value
}

type lookupKey struct {
	entry int
	bound tableSet
}

// lookup is an equality of the column col of an entry with the expression e, which reads only
// entries that are bound before the entry, converted to the column's type, so that the scan of
// the entry finds the rows of the value of e through an index.
type lookup struct {
	col int
	e   *cexpr
}

// selectJoin runs a SELECT on several tables, whose rows go into the groups of gr when it is
// grouped.
func (s *Session) selectJoin(ctx context.Context, st *ast.Select, b *binder, nodes []*fromNode, where filter, o output, order []orderKey, offsetExpr, limitExpr *cexpr, gr *aggRun) (*Rows, error) {
	p := &joinPlan{s: s, b: b, st: st, where: where, conds: map[*fromNode][]*jcond{}, post: map[*fromNode][]*jcond{}, never: map[*fromNode]bool{}, lookups: map[lookupKey][]lookup{}, groups: map[*fromNode]*groupRun{}}
	for _, e := range b.scope.entries {
		p.nulls = append(p.nulls, nullRow(e.t))
	}
	p.top = nodes[0]
	for _, n := range nodes[1:] {
		p.top = &fromNode{entry: -1, kind: ast.JoinCross, left: p.top, right: n, tables: p.top.tables | n.tables}
	}
	p.scan = make([][]ast.Expr, len(b.scope.entries))
	if err := p.place(); err != nil {
		return nil, err
	}
	if st.Limit != nil || st.Offset != nil {
		if gr != nil {
			if slices.ContainsFunc(p.all, func(c *jcond) bool { return c.ce.fallible }) {
				return nil, errLimitFallible()
			}
		} else if err := p.limitable(order, o, st.Distinct); err != nil {
			return nil, err
		}
	}
	p.filters = make([]filter, len(b.scope.entries))
	for i := range b.scope.entries {
		f, err := b.forEntry(i).filterOf(p.scan[i], "WHERE")
		if err != nil {
			return nil, err
		}
		if f.unsupported != nil {
			return nil, f.unsupported
		}
		p.filters[i] = f
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
		return &Rows{Columns: o.columns, Rows: [][]value.Value{}}, nil
	}
	open, err := where.open()
	if err != nil {
		return nil, err
	}
	// Postgres evaluates the output on every row of the join, and fails at the first row that
	// fails, which depends on the order of its plan. The class of the error is the same in every
	// order only when every row that fails fails with it.
	var collected []sortable
	var first error
	if open && !where.never {
		var sorts []colRef
		if gr != nil {
			sorts = gr.sortRefs()
		} else {
			for _, k := range order {
				if k.plain {
					sorts = append(sorts, k.ref)
				}
			}
		}
		if err := p.candidates(ctx, sorts); err != nil {
			return nil, err
		}
		if err := p.check(ctx); err != nil {
			return nil, err
		}
		r := &joinRun{p: p, ctx: ctx, row: make([]value.Value, b.scope.width)}
		err := r.node(p.top, 0, nil, func() error {
			if gr != nil {
				gr.add(r.row)
				return nil
			}
			out, err := project(o.items, r.row)
			var keys []value.Value
			if err == nil {
				keys, err = sortKeys(order, out, r.row)
			}
			switch {
			case err == nil:
				collected = append(collected, sortable{out, keys})
			case first == nil:
				first = err
			case !errors.Is(err, errorKind(first)):
				return fmt.Errorf("%w: %w: rows of this join fail with different errors, %v and %v, and which of them Postgres reports depends on the order of its plan; select the rows first, and compute the expressions on them", ErrUnsupported, errOrderDependent, errorKind(first), errorKind(err))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if gr != nil {
		return gr.rows(st.Distinct, offset, limit)
	}
	if first != nil {
		return nil, first
	}
	out, err := finish(collected, st.Distinct, order, len(order) > 0, offset, limit)
	if err != nil {
		return nil, err
	}
	return &Rows{Columns: o.columns, Rows: out}, nil
}

// place the conditions of the statement at the lowest node of the join tree whose rows they
// can filter without changing the result, as Postgres does: a conjunct of WHERE, or of the
// condition of an inner join, into either side of an inner join that has all its tables, and
// into the left side of a left join; and a conjunct of the condition of a left join that reads
// only its right side into that side. A conjunct placed at an entry is evaluated by the scan of
// the entry.
func (p *joinPlan) place() error {
	attach := func(n *fromNode, c *jcond) {
		switch {
		case n.entry >= 0:
			p.scan[n.entry] = append(p.scan[n.entry], c.expr)
		case n.kind == ast.JoinLeft:
			p.post[n] = append(p.post[n], c)
		default:
			p.conds[n] = append(p.conds[n], c)
		}
	}
	conds := func(ns namespace, f filter) []*jcond {
		saved := p.b.ns
		p.b.ns = ns
		defer func() { p.b.ns = saved }()
		var out []*jcond
		for i, ce := range f.conds {
			out = append(out, &jcond{expr: f.conjuncts[i], ce: ce, tables: p.b.tablesOf(f.conjuncts[i])})
		}
		for _, g := range f.gates {
			out = append(out, &jcond{ce: g})
		}
		return out
	}
	for _, c := range conds(p.b.ns, p.where) {
		if c.expr == nil {
			// The gates of WHERE are evaluated once, before any row is read.
			continue
		}
		p.all = append(p.all, c)
		attach(descend(p.top, c.tables), c)
	}
	var walk func(n *fromNode)
	walk = func(n *fromNode) {
		if n.entry >= 0 {
			return
		}
		walk(n.left)
		walk(n.right)
		if !n.hasOn {
			return
		}
		if n.on.never {
			p.never[n] = true
		}
		for _, c := range conds(n.onNS, n.on) {
			p.all = append(p.all, c)
			switch {
			case c.expr == nil:
				p.conds[n] = append(p.conds[n], c)
			case n.kind == ast.JoinLeft && c.tables != 0 && c.tables&^n.right.tables == 0:
				attach(descend(n.right, c.tables), c)
			case n.kind == ast.JoinLeft:
				p.conds[n] = append(p.conds[n], c)
			default:
				attach(descend(n, c.tables), c)
			}
		}
	}
	walk(p.top)
	return nil
}

// descend from n to the lowest node that has every table of tables, through the sides of inner
// joins and the left sides of left joins, below which a condition on those tables filters the
// same rows.
func descend(n *fromNode, tables tableSet) *fromNode {
	for n.entry < 0 && tables != 0 {
		switch {
		case tables&^n.left.tables == 0:
			n = n.left
		case n.kind != ast.JoinLeft && tables&^n.right.tables == 0:
			n = n.right
		default:
			return n
		}
	}
	return n
}

// limitable checks that a SELECT on several tables with LIMIT or OFFSET gives the same rows in
// every plan: the ORDER BY sorts by every primary key column of every table, or with DISTINCT by
// every output column, and nothing can fail, as [limitable] requires of one table.
func (p *joinPlan) limitable(order []orderKey, o output, distinct bool) error {
	for _, e := range p.b.scope.entries {
		if err := e.t.derivedLimit(); err != nil {
			return err
		}
	}
	if !distinct || !sortsByEveryOutput(order, len(o.columns)) {
		for i, e := range p.b.scope.entries {
			for _, col := range e.t.PrimaryKey {
				if !slices.ContainsFunc(order, func(k orderKey) bool { return k.plain && k.ref.entry == i && k.ref.col == col }) {
					return fmt.Errorf("%w: LIMIT and OFFSET without an ORDER BY on every primary key column are not supported, because which rows Postgres returns is then not determined; add %s.%s to ORDER BY", ErrUnsupported, e.name, e.columns[col])
				}
			}
		}
	}
	fails := slices.ContainsFunc(o.items, func(e *cexpr) bool { return e.fallible }) ||
		slices.ContainsFunc(order, func(k orderKey) bool { return k.expr != nil && k.expr.fallible })
	if fails {
		return fmt.Errorf("%w: LIMIT and OFFSET with an output expression that can fail are not supported, because how many rows Postgres evaluates it on depends on its plan; select the rows first and compute the expression on them", ErrUnsupported)
	}
	if slices.ContainsFunc(p.all, func(c *jcond) bool { return c.ce.fallible }) {
		return errLimitFallible()
	}
	return nil
}

// errLimitFallible is the error of a LIMIT or OFFSET with a condition that can fail.
func errLimitFallible() error {
	return fmt.Errorf("%w: LIMIT and OFFSET with a WHERE condition that can fail are not supported, because how many rows Postgres evaluates it on depends on its plan; select the rows first and filter them", ErrUnsupported)
}

// candidates sets the scans of each entry that Postgres could pick, which decide the rows that
// some plan reads, as for a statement on one table, see [table.candidates]. Besides the
// conditions that its scan evaluates, a scan of the entry can serve a condition of a join, a
// merge join, or the order of the rows, so every index on a column that any condition reads, or
// on one of the columns sorts that the rows can be sorted by, is a scan Postgres could pick.
func (p *joinPlan) candidates(ctx context.Context, sorts []colRef) error {
	for i, e := range p.b.scope.entries {
		f := p.filters[i]
		touched := map[int]bool{}
		for col := range f.touched {
			touched[col] = true
		}
		for _, c := range p.all {
			p.columnsOf(c.expr, i, func(col int) { touched[col] = true })
		}
		var sortCols []int
		for _, ref := range sorts {
			if ref.entry == i {
				sortCols = append(sortCols, ref.col)
			}
		}
		f.touched = touched
		p.filters[i].candidates = e.t.candidates(f, sortCols, p.reads(i))
	}
	return nil
}

// columnsOf calls fn with the columns of entry i that e reads. The conditions are normalized, so
// their references resolve by the names of the entries in any namespace of the statement.
func (p *joinPlan) columnsOf(e ast.Expr, i int, fn func(col int)) {
	p.b.columnsOf(p.b.ns, e, i, fn)
}

// reads are the columns of entry i that the statement reads anywhere.
func (p *joinPlan) reads(i int) []int {
	out := p.b.reads(p.st, i)
	for _, c := range p.all {
		p.columnsOf(c.expr, i, func(col int) {
			if !slices.Contains(out, col) {
				out = append(out, col)
			}
		})
	}
	return out
}

// maxChecks is the number of rows and combinations of rows on which [joinPlan.check] evaluates
// the conditions of a statement before it gives up.
const maxChecks = 100_000

// check evaluates every condition that can fail on every row and combination of rows that a plan
// of Postgres could evaluate it on, and rejects the statement when it fails on one, because then
// whether the statement fails depends on the plan. A condition on several entries can be
// evaluated on any combination of the rows that the scans of those entries could read, with
// NULLs for an entry that a LEFT JOIN extends. Postgres can also evaluate a part of a condition
// alone: each side of a comparison, as the key of a hash join or a merge join, or as the value
// of an index scan in a nested loop, and, from an OR whose arms each have conditions on one
// entry, the OR of those conditions, which it adds to the scan of the entry. Those are evaluated
// on the rows of their own entries too. The rows of each entry are read once, so a statement on
// large tables whose conditions can fail is rejected when the combinations are too many.
func (p *joinPlan) check(ctx context.Context) error {
	type check struct {
		ce     *cexpr
		tables tableSet
		expr   ast.Expr
	}
	var checks []check
	add := func(e ast.Expr, ce *cexpr) {
		if ce.fallible && ce.refs {
			checks = append(checks, check{ce: ce, tables: p.b.tablesOf(e), expr: e})
		}
	}
	for _, c := range p.all {
		if c.expr == nil || !c.ce.fallible {
			continue
		}
		checks = append(checks, check{ce: c.ce, tables: c.tables, expr: c.expr})
		if bin, ok := c.expr.(*ast.Binary); ok && comparisonOps[bin.Op] {
			for _, side := range []ast.Expr{bin.Left, bin.Right} {
				if ce, err := p.b.fresh().bindPruned(side); err == nil {
					add(side, ce)
				}
			}
		}
		for _, e := range p.extracted(c.expr) {
			if ce, err := p.b.fresh().bindPruned(e); err == nil {
				add(e, ce)
			}
		}
	}
	if len(checks) == 0 {
		return nil
	}
	rows := map[int][][]value.Value{}
	domain := func(i int) ([][]value.Value, error) {
		if d, ok := rows[i]; ok {
			return d, nil
		}
		var d [][]value.Value
		e := p.b.scope.entries[i]
		f := p.filters[i]
		f.conds, f.conjuncts, f.implied, f.fallible = nil, nil, nil, true
		err := p.s.scanRows(ctx, e.t, e.t.plan(f, nil), f, func(_ []byte, row []value.Value) (bool, error) {
			d = append(d, row)
			return len(d) <= maxChecks, nil
		})
		if err != nil {
			return nil, err
		}
		if e.nullable {
			d = append(d, p.nulls[i])
		}
		rows[i] = d
		return d, nil
	}
	total := 0
	row := make([]value.Value, p.b.scope.width)
	for _, c := range checks {
		members := c.tables.members()
		var domains [][][]value.Value
		n := 1
		for _, i := range members {
			d, err := domain(i)
			if err != nil {
				return err
			}
			domains = append(domains, d)
			n *= len(d)
			if n > maxChecks {
				break
			}
		}
		if total += n; n > maxChecks || total > maxChecks {
			return fmt.Errorf("%w: a condition of a join that can fail, %s, on more than %d rows and combinations of rows is not supported, because maragudb checks that it fails on none of those that a plan of Postgres could evaluate it on; filter the tables more, or write the condition so that it cannot fail", ErrUnsupported, ast.RenderExpr(c.expr), maxChecks)
		}
		var combine func(k int) error
		combine = func(k int) error {
			if k == len(members) {
				if _, err := c.ce.value(row); err != nil {
					return fmt.Errorf("%w: whether %s fails depends on the join plan Postgres picks, because it fails on a row that one plan evaluates it on and another plan does not; write the condition so that it cannot fail, such as with CASE", ErrUnsupported, ast.RenderExpr(c.expr))
				}
				return nil
			}
			e := p.b.scope.entries[members[k]]
			for _, r := range domains[k] {
				copy(row[e.offset:], r)
				if err := combine(k + 1); err != nil {
					return err
				}
			}
			return nil
		}
		if err := combine(0); err != nil {
			return err
		}
	}
	return nil
}

var comparisonOps = map[ast.BinaryOp]bool{ast.Eq: true, ast.Ne: true, ast.Lt: true, ast.Le: true, ast.Gt: true, ast.Ge: true}

// extracted are the conditions that Postgres extracts from an OR on several entries for the scan
// of one of them: for each entry that every arm of the OR has conditions on alone, the OR of
// those conditions.
func (p *joinPlan) extracted(e ast.Expr) []ast.Expr {
	bin, ok := e.(*ast.Binary)
	if !ok || bin.Op != ast.Or {
		return nil
	}
	arms := flatten(e, ast.Or)
	var out []ast.Expr
	for _, i := range p.b.tablesOf(e).members() {
		var parts []ast.Expr
		for _, arm := range arms {
			var mine []ast.Expr
			for _, c := range flatten(arm, ast.And) {
				if p.b.tablesOf(c) == tableSet(0).with(i) {
					mine = append(mine, c)
				}
			}
			if len(mine) == 0 {
				parts = nil
				break
			}
			parts = append(parts, join(mine, ast.And))
		}
		if len(parts) > 0 {
			out = append(out, join(parts, ast.Or))
		}
	}
	return out
}

// nullRow of a table, with a NULL of its type in every column.
func nullRow(t *table) []value.Value {
	row := make([]value.Value, len(t.Columns))
	for i, c := range t.Columns {
		row[i] = value.NewNull(c.Type)
	}
	return row
}

// fresh is a binder of the same names and rows, whose folding errors are its own.
func (b *binder) fresh() *binder {
	return &binder{scope: b.scope, ns: b.ns, layout: b.layout, table: b.table, entry: b.entry, params: b.params, now: b.now, phase: b.phase, aggClause: b.aggClause,
		run: b.run, outer: b.outer, sub: b.sub, ctes: b.ctes, level: b.level, wide: b.wide, analyzing: b.analyzing}
}

// joinRun is a run of a join plan, which fills the row of the join as it goes.
type joinRun struct {
	p   *joinPlan
	ctx context.Context
	row []value.Value
}

// group of a node: the items of the tree of inner and cross joins at it, which are entries and
// left joins, the conditions of those joins, and whether one of them is never true.
func (p *joinPlan) group(n *fromNode) ([]*fromNode, []*jcond, bool) {
	if n.entry >= 0 || n.kind == ast.JoinLeft {
		return []*fromNode{n}, nil, false
	}
	li, lc, ln := p.group(n.left)
	ri, rc, rn := p.group(n.right)
	return append(li, ri...), append(append(append([]*jcond{}, lc...), rc...), p.conds[n]...), ln || rn || p.never[n]
}

// groupRun is the group of a node with the conditions of an enclosing left join, and the order
// of its items, which are the same on every run of the node.
type groupRun struct {
	order []*fromNode
	conds []*jcond
	never bool
}

// groupAt returns the [groupRun] of n with the conditions extra, made on the first run.
func (p *joinPlan) groupAt(n *fromNode, bound tableSet, extra []*jcond) *groupRun {
	if g, ok := p.groups[n]; ok {
		return g
	}
	items, conds, never := p.group(n)
	conds = append(conds, extra...)
	g := &groupRun{order: p.order(items, conds, bound), conds: conds, never: never}
	p.groups[n] = g
	return g
}

// node runs the joins at n, with the entries of bound already in the row, and calls emit for
// every row they produce. The conditions extra, of an enclosing left join, must hold too.
func (r *joinRun) node(n *fromNode, bound tableSet, extra []*jcond, emit func() error) error {
	g := r.p.groupAt(n, bound, extra)
	if g.never {
		return nil
	}
	order, conds := g.order, g.conds
	var step func(k int, bound tableSet) error
	step = func(k int, bound tableSet) error {
		if k == len(order) {
			return emit()
		}
		it := order[k]
		next := bound | it.tables
		var ready []*jcond
		for _, c := range conds {
			if c.tables&^next == 0 && (c.tables&^bound != 0 || k == 0) {
				ready = append(ready, c)
			}
		}
		return r.item(it, bound, conds, func() error {
			for _, c := range ready {
				v, err := c.ce.value(r.row)
				if err != nil {
					return err
				}
				if v.Null || !v.B {
					return nil
				}
			}
			return step(k+1, next)
		})
	}
	return step(0, bound)
}

// item runs an entry or a left join, with the entries of bound in the row.
func (r *joinRun) item(n *fromNode, bound tableSet, conds []*jcond, emit func() error) error {
	if n.entry >= 0 {
		return r.scan(n.entry, bound, conds, emit)
	}
	if post := r.p.post[n]; len(post) > 0 {
		next := emit
		emit = func() error {
			for _, c := range post {
				v, err := c.ce.value(r.row)
				if err != nil {
					return err
				}
				if v.Null || !v.B {
					return nil
				}
			}
			return next()
		}
	}
	return r.node(n.left, bound, nil, func() error {
		matched := false
		if !r.p.never[n] {
			err := r.node(n.right, bound|n.left.tables, r.p.conds[n], func() error {
				matched = true
				return emit()
			})
			if err != nil {
				return err
			}
		}
		if matched {
			return nil
		}
		for _, i := range n.right.members {
			copy(r.row[r.p.b.scope.entries[i].offset:], r.p.nulls[i])
		}
		return emit()
	})
}

// scan the rows of entry i that its filter admits, with the values of its lookups, into the row.
func (r *joinRun) scan(i int, bound tableSet, conds []*jcond, emit func() error) error {
	e := r.p.b.scope.entries[i]
	f := r.p.filters[i]
	f.keyed = slices.Clone(f.keyed)
	for _, lk := range r.p.lookupsFor(i, bound, conds) {
		v, err := lk.e.value(r.row)
		if err != nil {
			return err
		}
		if v.Null {
			// An equality with NULL is never true.
			return nil
		}
		f.keyed = append(f.keyed, keyed{col: lk.col, op: ast.Eq, k: v, known: true})
	}
	// The narrowest access, whatever can fail: the filter skips the rows that no plan of
	// Postgres reads before it evaluates anything on them, and [joinPlan.check] made sure that
	// nothing fails on the others.
	narrow := f
	narrow.fallible = false
	return r.p.s.scanRows(r.ctx, e.t, e.t.plan(narrow, nil), f, func(_ []byte, row []value.Value) (bool, error) {
		copy(r.row[e.offset:], row)
		return true, emit()
	})
}

// lookupsFor are the equalities of conds between an indexed column of entry i and an expression
// of the entries of bound alone, which narrow the scan of the entry.
func (p *joinPlan) lookupsFor(i int, bound tableSet, conds []*jcond) []lookup {
	key := lookupKey{entry: i, bound: bound}
	if l, ok := p.lookups[key]; ok {
		return l
	}
	t := p.b.scope.entries[i].t
	out := []lookup{}
	for _, c := range conds {
		bin, ok := c.expr.(*ast.Binary)
		if !ok || bin.Op != ast.Eq || !c.tables.has(i) {
			continue
		}
		for _, sides := range [][2]ast.Expr{{bin.Left, bin.Right}, {bin.Right, bin.Left}} {
			ref, ok := sides[0].(*ast.ColumnRef)
			if !ok {
				continue
			}
			r, err := p.b.scope.resolve(p.b.ns, ref)
			other := p.b.tablesOf(sides[1])
			if err != nil || r.entry != i || r.cast != value.Unknown || !t.indexed(r.col) || other == 0 || other&^bound != 0 {
				continue
			}
			ce, err := p.b.fresh().bindPruned(sides[1])
			if err != nil {
				continue
			}
			// The comparison is in the column's type only when the other side converts to it;
			// otherwise Postgres compares the column converted, which no index has.
			if ce, err = p.b.coerce(ce, t.Columns[r.col].Type); err != nil {
				continue
			}
			out = append(out, lookup{col: r.col, e: ce})
		}
	}
	p.lookups[key] = out
	return out
}

// order of the items of a group: from the first, each time the next item that a condition links
// to the items before it, preferring an entry that a lookup finds, and the next in FROM order when
// none is linked, which keeps the loops of a long chain of equalities short. The rows the group
// produces are the same in every order.
func (p *joinPlan) order(items []*fromNode, conds []*jcond, bound tableSet) []*fromNode {
	rest := slices.Clone(items)
	var out []*fromNode
	have := bound
	for len(rest) > 0 {
		pick := -1
		for j, it := range rest {
			if it.entry >= 0 && len(p.lookupsFor(it.entry, have, conds)) > 0 {
				pick = j
				break
			}
		}
		if pick < 0 {
			for j, it := range rest {
				if slices.ContainsFunc(conds, func(c *jcond) bool {
					return c.tables&it.tables != 0 && c.tables&have != 0 && c.tables&^(have|it.tables) == 0
				}) {
					pick = j
					break
				}
			}
		}
		if pick < 0 {
			pick = 0
		}
		out = append(out, rest[pick])
		have |= rest[pick].tables
		rest = slices.Delete(rest, pick, pick+1)
	}
	return out
}
