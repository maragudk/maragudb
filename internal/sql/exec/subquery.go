package exec

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// A subquery is bound where it is written, so that its errors of analysis come in Postgres's
// order, and evaluated by running its SELECT again whenever its value is needed, with the names
// that its FROM does not have resolved in the statements around it. Postgres plans a subquery
// in several ways: an uncorrelated one as an InitPlan, evaluated lazily at its first use, or
// before a scan whose index condition it is, or not at all; IN and EXISTS as semi-joins, NOT
// EXISTS as an anti-join, IN and NOT IN as hashed SubPlans, and every one of them as a SubPlan
// that stops at the first row that decides its value. Which rows the expressions of a subquery
// are evaluated on therefore depends on the plan, so maragudb evaluates a subquery completely,
// and rejects the statement when its evaluation fails, with [errPlanDependentSubquery]. The one
// error a subquery reports itself is the more than one row of a scalar subquery, which is the
// same in every plan that evaluates it. Where and how often the statement around evaluates the
// subquery follows the plan rules of that statement, for which the subquery is an expression
// that can fail.

// errPlanDependentSubquery is wrapped, together with [ErrUnsupported], by the error of a
// statement whose outcome depends on how Postgres plans a subquery.
var errPlanDependentSubquery = errors.New("plan-dependent subquery")

// maxSubqueryEvals is the number of times a statement can evaluate its subqueries before it is
// rejected.
const maxSubqueryEvals = 20_000

// stmtRun is the execution of one statement, which every binder of the statement shares, also
// those of its subqueries.
type stmtRun struct {
	s   *Session
	ctx context.Context
	// refs are the columns of the queries around each subquery that it reads, by the scope they
	// are in, which its analysis records.
	refs map[*ast.Select][]outerRef
	// evals counts the evaluations of subqueries.
	evals int
	// names are the names of the columns of the scalar subqueries, by their queries.
	names map[*ast.Select]string
}

// outerRef is a column of an enclosing query that a subquery reads.
type outerRef struct {
	scope *scope
	ref   colRef
}

// levelState is shared by the binders of one query, without its subqueries.
type levelState struct {
	// fallible is set when the query binds an expression that can fail, outside of pruned
	// branches.
	fallible bool
}

// subqueryInfo is a subquery as its own query sees it.
type subqueryInfo struct {
	sel *ast.Select
	// row is the row of the query around, during an evaluation.
	row []value.Value
	// correlated is set when the subquery reads a column outside of itself, or a CTE of a query
	// around it, refsParent when it reads a column of the query it is in, and deeper when it
	// reads a column of a query further out.
	correlated, refsParent, deeper bool
	// cte is set when the subquery reads a CTE of a query around it.
	cte bool
	// uses are its references to columns outside of itself, its own and those of the subqueries
	// in it, which [subqueryInfo.live] reads.
	uses []*outerUse
}

// outerUse is a reference to a column of a query around a subquery, through the subqueries hops
// from the innermost out, which dies when folding drops the expression that has it.
type outerUse struct {
	hops []*subqueryInfo
	dead bool
}

// live reports correlated and refsParent as Postgres sees them after it folds the subquery,
// which drops the expressions that a constant decides, such as the left side of AND FALSE, the
// branches of a CASE that never match, and a WHERE clause that is FALSE, with their references
// to the queries around. Postgres decides whether a subquery is correlated from what remains, so
// a subquery whose references all fold away is an InitPlan. A reference to a CTE of a query
// around counts as live.
func (info *subqueryInfo) live() (correlated, refsParent bool) {
	correlated = info.cte
	for _, u := range info.uses {
		if !u.dead {
			correlated = true
			refsParent = refsParent || u.hops[len(u.hops)-1] == info
		}
	}
	return correlated, refsParent
}

// dropped marks the references to the queries around in the expressions es dead, because
// folding drops the expressions.
func dropped(es ...*cexpr) {
	for _, e := range es {
		for _, u := range e.outer {
			u.dead = true
		}
	}
}

// subqueryResult is the rows of an evaluation of a subquery, or its error.
type subqueryResult struct {
	rows [][]value.Value
	err  error
}

// newRun starts the execution of a statement.
func (s *Session) newRun(ctx context.Context) *stmtRun {
	return &stmtRun{s: s, ctx: ctx, refs: map[*ast.Select][]outerRef{}, names: map[*ast.Select]string{}}
}

// newBinder for a statement with the parameters params.
func (s *Session) newBinder(ctx context.Context, params []value.Value) *binder {
	return &binder{params: params, now: s.now(), run: s.newRun(ctx), level: &levelState{}, top: true}
}

// snapshot of the binder as it is now, in which the names of a subquery bound now resolve
// whenever it is evaluated.
func (b *binder) snapshot() *binder {
	c := *b
	c.top = false
	return &c
}

// inner is a binder for the query of a subquery bound in b, whose names resolve in outer when
// the query does not have them.
func (b *binder) inner(outer *binder, info *subqueryInfo, analyzing bool) *binder {
	return &binder{params: b.params, now: b.now, run: b.run, outer: outer, sub: info, ctes: b.ctes, wide: b.wide, level: &levelState{}, analyzing: analyzing, aggClause: "JOIN conditions"}
}

// visible reports whether the query of b has a column that ref can name: a column of a visible
// entry of that name, when ref is qualified, or a column of that name otherwise.
func (b *binder) visible(ref *ast.ColumnRef) bool {
	sc, ns := b.names()
	if sc == nil {
		return false
	}
	if ref.Table != "" {
		return slices.ContainsFunc(ns, func(it nsItem) bool { return it.rel && sc.entries[it.entry].name == ref.Table })
	}
	for _, it := range ns {
		for _, c := range it.cols {
			if c.name == ref.Name {
				return true
			}
		}
	}
	return false
}

// columnRef binds a column reference: in the query of b, or, as Postgres resolves names, in the
// nearest query around it that has the name. A reference to a column of a query around is the
// value of that column in the row the query around evaluates the subquery on.
func (b *binder) columnRef(e *ast.ColumnRef) (*cexpr, error) {
	if b.outer == nil || b.visible(e) {
		r, err := b.resolve(e)
		if err != nil {
			return nil, err
		}
		return b.column(r), nil
	}
	var hops []*binder
	for cur := b; cur.outer != nil; cur = cur.outer {
		hops = append(hops, cur)
		o := cur.outer
		if !o.visible(e) {
			continue
		}
		oce, err := o.bind(e)
		if err != nil {
			return nil, err
		}
		if r, err := o.resolve(e); err == nil {
			sc, _ := o.names()
			for _, h := range hops {
				if or := (outerRef{scope: sc, ref: r}); !slices.Contains(b.run.refs[h.sub.sel], or) {
					b.run.refs[h.sub.sel] = append(b.run.refs[h.sub.sel], or)
				}
			}
		}
		use := &outerUse{}
		for i, h := range hops {
			h.sub.correlated = true
			if i == len(hops)-1 {
				h.sub.refsParent = true
			} else {
				h.sub.deeper = true
			}
			use.hops = append(use.hops, h.sub)
			h.sub.uses = append(h.sub.uses, use)
		}
		info := cur.sub
		return &cexpr{typ: oce.typ, refs: true, fallible: oce.fallible, outer: []*outerUse{use}, eval: func([]value.Value) (value.Value, error) {
			return oce.value(info.row)
		}}, nil
	}
	// No query has the name. As Postgres does, a name alone that names a table at any level
	// is a reference to its whole row, which is not supported.
	if e.Table == "" {
		for cur := b; cur != nil; cur = cur.outer {
			if sc, ns := cur.names(); sc != nil && slices.ContainsFunc(ns, func(it nsItem) bool { return it.rel && sc.entries[it.entry].name == e.Name }) {
				return nil, fmt.Errorf("%w: a reference to the whole row of %q is not supported, name its columns", ErrUnsupported, e.Name)
			}
		}
	}
	_, err := b.resolve(e)
	return nil, err
}

// outerOnly reports whether the arguments of an aggregate in a subquery read columns of the
// queries around and none of its own, which makes it an aggregate of a query around, or have a
// subquery and none of its own columns.
func (b *binder) outerOnly(args []ast.Expr) bool {
	if b.outer == nil {
		return false
	}
	own, other := false, false
	for _, a := range args {
		ast.Walk(a, func(x ast.Expr) bool {
			switch x := x.(type) {
			case *ast.ColumnRef:
				if b.visible(x) {
					own = true
				} else {
					other = true
				}
			case *ast.Subquery:
				other = true
			}
			return true
		})
	}
	return other && !own
}

// subqueryRefs calls fn with the columns of the query of b that the subqueries in e read.
func (b *binder) subqueryRefs(e ast.Expr, fn func(r colRef)) {
	if b.run == nil {
		return
	}
	sc, _ := b.names()
	ast.SubqueriesOf(e, func(sel *ast.Select) {
		for _, r := range b.run.refs[sel] {
			if r.scope == sc {
				fn(r.ref)
			}
		}
	})
}

// subquery binds a subquery in an expression.
func (b *binder) subquery(e *ast.Subquery) (*cexpr, error) {
	if b.run == nil {
		return nil, fmt.Errorf("%w: a subquery in %s is not supported", ErrUnsupported, cmp.Or(b.aggClause, "this expression"))
	}
	outer := b.snapshot()
	info := &subqueryInfo{sel: e.Select}
	ib := b.inner(outer, info, true)
	res, err := b.run.s.query(b.run.ctx, e.Select, ib)
	if err != nil {
		return nil, err
	}
	innerFallible := ib.level.fallible
	cols := res.Columns
	// selfFallible is set when the evaluation of the subquery itself can fail.
	selfFallible := innerFallible || e.Kind == ast.SubScalar && !atMostOneRow(e.Select)
	sel := e.Select
	if e.Kind == ast.SubExists {
		sel = simplifyExists(sel)
	}
	evaluate := b.evaluator(e.Select, sel, outer, info.correlated)
	var ce *cexpr
	switch e.Kind {
	case ast.SubScalar:
		if len(cols) != 1 {
			return nil, fmt.Errorf("%w: subquery must return only one column", ErrSyntax)
		}
		typ := cols[0].Type
		b.run.names[e.Select] = cols[0].Name
		ce = &cexpr{typ: typ, refs: info.refsParent, sublink: true, fallible: innerFallible || !atMostOneRow(e.Select), eval: func(row []value.Value) (value.Value, error) {
			rows, err := evaluate(row)
			switch {
			case err != nil:
				return value.Value{}, err
			case len(rows) == 0:
				return value.NewNull(typ), nil
			case len(rows) > 1:
				return value.Value{}, fmt.Errorf("%w: more than one row returned by a subquery used as an expression", ErrCardinality)
			}
			return rows[0][0], nil
		}}
	case ast.SubExists:
		ce = &cexpr{typ: value.Bool, refs: info.refsParent, sublink: true, fallible: innerFallible, eval: func(row []value.Value) (value.Value, error) {
			rows, err := evaluate(row)
			if err != nil {
				return value.Value{}, err
			}
			return value.NewBool(len(rows) > 0), nil
		}}
	case ast.SubIn:
		var convert bool
		if ce, convert, err = b.inSubquery(e, cols, innerFallible, info, evaluate); err != nil {
			return nil, err
		}
		// The conversion of the subquery's values to the type of the comparison is part of
		// its evaluation, which a semi-join makes on the subquery's rows.
		selfFallible = selfFallible || convert
	}
	// The subquery's references reach the expressions around it, whose folding can drop them.
	ce.outer = append(ce.outer, info.uses...)
	if ce.fallible && b.pruned == 0 && b.level != nil {
		b.level.fallible = true
	}
	if b.pruned > 0 {
		return ce, nil
	}
	// Postgres can evaluate an uncorrelated subquery in WHERE, a join condition, or HAVING before
	// it reads any row, as an index condition or a condition without columns, or never, when no
	// row reaches it, and can build the hash table of a semi-join before it reads the other side.
	// A semi-join or an anti-join evaluates the conditions of a correlated EXISTS on its own rows
	// also when no row of the query around reaches them, and the output of a correlated IN on
	// rows that no row of the query around matches.
	// A statement with a parameter value that Postgres refuses fails with that error before any
	// subquery is evaluated.
	refused := b.run.s.params.refusal() != nil
	if selfFallible && !refused && (b.phase == phaseWhere || b.phase == phaseOn || b.phase == phaseHaving) {
		liveCorrelated, liveRefsParent := info.live()
		switch {
		case !info.correlated:
			rows, err := evaluate(nil)
			if err == nil && e.Kind == ast.SubScalar && len(rows) > 1 {
				err = fmt.Errorf("%w: more than one row returned by a subquery used as an expression", ErrCardinality)
			}
			if err != nil {
				return nil, planDependent(err)
			}
		case e.Kind == ast.SubScalar && !liveCorrelated:
			// Postgres folds the subquery before it decides whether it is correlated, so one
			// whose references to the queries around all fold away is evaluated as an
			// uncorrelated one. The references read NULL, which the folded subquery never
			// reads.
			rows, err := b.nullRun(e.Select, sel, outer)
			if err != nil {
				return nil, planDependent(err)
			}
			if len(rows) > 1 && !b.analyzing {
				// Postgres fails with 21000 before it reads any row, or at the first row that
				// reaches the subquery, where maragudb, which evaluates it on that row, fails
				// with it too. When no row reaches it, the outcome depends on the plan. Inside
				// a subquery that is only analyzed, this is decided again when the subquery is
				// evaluated, and never when it is not.
				b.run.s.uncertain = cmp.Or(b.run.s.uncertain, planDependent(fmt.Errorf("%w: more than one row returned by a subquery used as an expression", ErrCardinality)))
			}
		case !info.refsParent, e.Kind == ast.SubScalar && !liveRefsParent:
			// A subquery that reads only the columns of queries further out is a constant of
			// the query it is in, which Postgres can evaluate before any row, as an uncorrelated
			// one.
			return nil, fmt.Errorf("%w: %w: a subquery that can fail in a condition, and reads columns only of a query two levels out, is not supported, because Postgres can evaluate it before it reads any row; write the subquery so that it cannot fail", ErrUnsupported, errPlanDependentSubquery)
		case b.phase == phaseHaving:
			// Postgres merges no subquery of HAVING into a join.
		case e.Kind == ast.SubIn, e.Kind == ast.SubExists && info.deeper:
			// Postgres 18 also merges a correlated IN into a semi-join, which evaluates the
			// output of the subquery on its rows as the key of the join.
			return nil, fmt.Errorf("%w: %w: a correlated IN or EXISTS whose subquery can fail is not supported in a condition, because Postgres can evaluate it as a semi-join, on rows of the subquery that no row of the query around evaluates it on; write the subquery so that it cannot fail", ErrUnsupported, errPlanDependentSubquery)
		case e.Kind == ast.SubExists:
			if _, err := b.nullRun(e.Select, sel, outer); err != nil {
				return nil, err
			}
		}
	}
	return ce, nil
}

// planDependent is the error of a statement whose subquery fails with err in some plan of
// Postgres and not in another.
func planDependent(err error) error {
	if errors.Is(err, ErrUnsupported) {
		return err
	}
	return fmt.Errorf("%w: %w: a subquery fails with %v, and whether Postgres evaluates it on the rows where it fails depends on its plan; write the subquery so that it cannot fail", ErrUnsupported, errPlanDependentSubquery, err)
}

// nullRun evaluates the query of a correlated subquery with NULL for every column of the query
// around, which makes the correlated conditions of an EXISTS reject every row, so that the
// conditions that can fail are evaluated on every row that its scans could read, and returns its
// rows.
func (b *binder) nullRun(orig, sel *ast.Select, outer *binder) ([][]value.Value, error) {
	width := 0
	if sc, _ := b.names(); sc != nil {
		width = sc.width
	}
	row := make([]value.Value, width)
	for i := range row {
		row[i] = value.Value{Null: true}
	}
	info := &subqueryInfo{sel: orig, row: row}
	b.run.evals++
	if b.run.evals > maxSubqueryEvals {
		return nil, errTooManyEvals()
	}
	b.run.s.snapshotReads++
	defer func() { b.run.s.snapshotReads-- }()
	res, err := b.run.s.query(b.run.ctx, sel, b.inner(outer, info, false))
	if err != nil {
		return nil, planDependent(err)
	}
	return res.Rows, nil
}

// inSubquery binds `x [NOT] IN (subquery)`, whose subquery has the columns cols: true when x
// equals a value of the subquery, false when the subquery has no rows or x differs from every
// value, and NULL otherwise, as the comparison is NULL with a NULL. Postgres analyzes the
// subquery before x, and does not evaluate x when the subquery has no rows.
func (b *binder) inSubquery(e *ast.Subquery, cols []Column, innerFallible bool, info *subqueryInfo, evaluate func([]value.Value) ([][]value.Value, error)) (*cexpr, bool, error) {
	left, err := b.bind(e.Expr)
	if err != nil {
		return nil, false, err
	}
	if len(cols) != 1 {
		if len(cols) > 1 {
			return nil, false, fmt.Errorf("%w: subquery has too many columns", ErrSyntax)
		}
		return nil, false, fmt.Errorf("%w: subquery has too few columns", ErrSyntax)
	}
	in := &cexpr{typ: cols[0].Type, refs: true, eval: func(row []value.Value) (value.Value, error) {
		return row[0], nil
	}}
	if left, in, err = b.unify(left, in, "="); err != nil {
		return nil, false, err
	}
	not := e.Not
	// In a condition, Postgres can turn an IN into a semi-join, whose hash join computes the left
	// side as its key on every row of the query around, also when the subquery has no rows, where
	// a subplan never computes it. A NOT IN stays a subplan. An IN under OR or NOT does too, which
	// the binder does not tell apart here, so such an IN is rejected as well.
	condition := left.fallible && !e.Not && (b.phase == phaseWhere || b.phase == phaseOn)
	return &cexpr{typ: value.Bool, refs: left.refs || info.refsParent, fallible: innerFallible || left.fallible || in.fallible, stable: left.stable, sublink: true, aggs: left.aggs, eval: func(row []value.Value) (value.Value, error) {
		rows, err := evaluate(row)
		if err != nil {
			return value.Value{}, err
		}
		if len(rows) == 0 {
			if condition {
				if _, err := left.value(row); err != nil {
					return value.Value{}, fmt.Errorf("%w: %w: the left side of an IN with a subquery fails with %v on a row for which the subquery has no rows, and whether Postgres evaluates it there depends on its plan; write the left side so that it cannot fail", ErrUnsupported, errPlanDependentSubquery, err)
				}
			}
			return value.NewBool(not), nil
		}
		v, err := left.value(row)
		if err != nil {
			return value.Value{}, err
		}
		sawNull := false
		for _, r := range rows {
			item, err := in.value(r)
			if err != nil {
				return value.Value{}, err
			}
			if item.Null {
				sawNull = true
				continue
			}
			if !v.Null && value.Compare(v, item) == 0 {
				return value.NewBool(!not), nil
			}
		}
		if v.Null || sawNull {
			return value.NewNull(value.Bool), nil
		}
		return value.NewBool(not), nil
	}}, in.fallible, nil
}

// evaluator of a subquery whose SELECT is orig, which runs sel, either orig or the form of it
// that EXISTS needs, in the binder outer around it. An uncorrelated subquery has the same rows in
// the whole statement, and a correlated one the same rows for the same row of the query around,
// so each is evaluated once for each.
func (b *binder) evaluator(orig, sel *ast.Select, outer *binder, correlated bool) func(row []value.Value) ([][]value.Value, error) {
	run := b.run
	memo := map[string]subqueryResult{}
	return func(row []value.Value) ([][]value.Value, error) {
		var key string
		if correlated {
			key = memoKey(row)
			if r, ok := memo[key]; ok {
				return r.rows, r.err
			}
		} else if r, ok := run.s.subqueries[orig]; ok {
			return r.rows, r.err
		}
		run.evals++
		if run.evals > maxSubqueryEvals {
			return nil, errTooManyEvals()
		}
		info := &subqueryInfo{sel: orig, row: row}
		run.s.snapshotReads++
		res, err := run.s.query(run.ctx, sel, b.inner(outer, info, false))
		run.s.snapshotReads--
		var r subqueryResult
		if err != nil {
			r.err = planDependent(err)
		} else {
			r.rows = res.Rows
		}
		if correlated {
			memo[key] = r
		} else {
			if run.s.subqueries == nil {
				run.s.subqueries = map[*ast.Select]subqueryResult{}
			}
			run.s.subqueries[orig] = r
		}
		return r.rows, r.err
	}
}

// errTooManyEvals is the error of a statement that evaluates its subqueries more than
// [maxSubqueryEvals] times.
func errTooManyEvals() error {
	return fmt.Errorf("%w: a statement that evaluates its subqueries more than %d times is not supported, because maragudb evaluates them on every row that a plan of Postgres could evaluate them on; filter the rows more", ErrUnsupported, maxSubqueryEvals)
}

// memoKey is a text that two rows have exactly when their values are the same, also in their
// type and in how they print.
func memoKey(row []value.Value) string {
	var b strings.Builder
	for _, v := range row {
		if v.Null {
			b.WriteString("n|")
			continue
		}
		s := value.Format(v)
		fmt.Fprintf(&b, "%d:%d:%s|", v.Type, len(s), s)
	}
	return b.String()
}

// simplifyExists is the query that Postgres evaluates for EXISTS, as its simplify_EXISTS_query
// makes it: without its output, GROUP BY, DISTINCT, ORDER BY, and a LIMIT that is a positive
// constant or NULL, none of which changes whether the query has a row, unless it has aggregates,
// HAVING, OFFSET, or another LIMIT.
func simplifyExists(s *ast.Select) *ast.Select {
	if s.Having != nil || s.Offset != nil {
		return s
	}
	for _, item := range s.Items {
		if !item.Star && ast.HasAggregate(item.Expr) {
			return s
		}
	}
	for _, o := range s.OrderBy {
		if ast.HasAggregate(o.Expr) {
			return s
		}
	}
	if s.Limit != nil {
		lit, ok := s.Limit.(*ast.Literal)
		if !ok || !lit.Value.Null && (lit.Value.Type != value.Int4 && lit.Value.Type != value.Int || lit.Value.I <= 0) {
			return s
		}
	}
	c := *s
	c.Items = []ast.SelectItem{{Expr: &ast.Literal{Value: value.NewInt4(1)}}}
	c.GroupBy, c.Distinct, c.OrderBy, c.Limit = nil, false, nil, nil
	return &c
}

// atMostOneRow reports whether a query has one row at most whatever its tables hold: a grouped
// query without GROUP BY, or one with a LIMIT of 0 or 1.
func atMostOneRow(s *ast.Select) bool {
	if lit, ok := s.Limit.(*ast.Literal); ok && !lit.Value.Null && (lit.Value.Type == value.Int4 || lit.Value.Type == value.Int) && lit.Value.I <= 1 {
		return true
	}
	return len(s.GroupBy) == 0 && grouped(s)
}
