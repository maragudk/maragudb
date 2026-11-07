package exec

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// paramOrder is the order in which Postgres analyzes the references to the parameters of a
// statement. Postgres gives a parameter its type at the first reference that a context coerces
// to a type, and every reference that it makes after that has the type; a reference that it made
// before then, which has no type of its own, is coerced later, and fails with 42P08 when its
// context coerces it to another type, or reads it in a context that gives no type, such as IS
// NULL. The order is that of the clauses as Postgres analyzes them, the WITH clause, FROM with
// the conditions of the joins, the output, WHERE, HAVING, ORDER BY, GROUP BY, OFFSET, and LIMIT,
// and within an expression the order of its subexpressions. Positions count the nodes as
// Postgres finishes them, in steps of two, so that a coercion right after a node, before the
// next one starts, has the odd position after it.
type paramOrder struct {
	at int
	// sites are the references to the parameters, and byNumber the references to each
	// parameter in the order of the analysis.
	sites    map[*ast.Param]*paramSite
	byNumber map[int][]*ast.Param
	// pos is the position of every expression of the statement, at which Postgres reports an
	// error of its analysis, and start the position before its first subexpression, so that the
	// expressions in it have the positions after start up to pos.
	pos, start map[ast.Expr]int
	// imprecise are the parameters with a reference whose coercion the order does not place,
	// such as an output column that ORDER BY names by an expression.
	imprecise map[int]bool
	// lists are the references on the left of an IN with a list, which Postgres analyzes once
	// and compares with each item.
	lists map[*ast.Param]bool
	// depth is the number of queries around the query being walked.
	depth int
}

// paramSite is the position at which Postgres makes a reference to a parameter, create, and the
// position at which its context coerces the reference to a type when it has none, coerce, or 0
// when its context does not coerce it.
type paramSite struct {
	create, coerce int
}

// newParamOrder of the references to the parameters of st, or nil when no parameter is
// referenced more than once or on the left of an IN, whose type is then that of its one
// context.
func newParamOrder(st ast.Statement) *paramOrder {
	o := &paramOrder{sites: map[*ast.Param]*paramSite{}, byNumber: map[int][]*ast.Param{}, pos: map[ast.Expr]int{}, start: map[ast.Expr]int{}, imprecise: map[int]bool{}, lists: map[*ast.Param]bool{}}
	switch st := st.(type) {
	case *ast.Select:
		// The statement's own query has no query around.
		o.depth = -1
		o.query(st)
	case *ast.Insert:
		// Postgres analyzes each row of VALUES and then coerces its items to the types of
		// their columns.
		for _, row := range st.Rows {
			for _, e := range row {
				o.expr(e)
			}
			at := o.tick()
			for _, e := range row {
				o.coerce(e, at)
			}
		}
	case *ast.Update:
		// Postgres analyzes WHERE before SET, and every expression of SET before it coerces
		// them to the types of their columns.
		o.clause(st.Where)
		for _, a := range st.Set {
			o.expr(a.Value)
		}
		at := o.tick()
		for _, a := range st.Set {
			o.coerce(a.Value, at)
		}
	case *ast.Delete:
		o.clause(st.Where)
	}
	for _, refs := range o.byNumber {
		if len(refs) > 1 {
			return o
		}
	}
	if len(o.lists) > 0 {
		return o
	}
	return nil
}

// tick finishes a node and returns its position.
func (o *paramOrder) tick() int {
	o.at += 2
	return o.at
}

// coerce records that the context of e coerces it at the position at, when e is a parameter.
func (o *paramOrder) coerce(e ast.Expr, at int) {
	if p, ok := e.(*ast.Param); ok && o.sites[p].coerce == 0 {
		o.sites[p].coerce = at
	}
}

// clause walks the expression of a clause that Postgres coerces to a type right after it
// analyzes it, such as WHERE to boolean and LIMIT to bigint.
func (o *paramOrder) clause(e ast.Expr) {
	if e != nil {
		o.coerce(e, o.expr(e)+1)
	}
}

// query walks a SELECT, and coerces the parameters that are output columns to text where
// Postgres does: when ORDER BY, GROUP BY, or DISTINCT reads them, and otherwise at the end.
func (o *paramOrder) query(sel *ast.Select) {
	o.depth++
	defer func() { o.depth-- }()
	for _, c := range sel.With {
		o.query(c.Select)
	}
	var from func(e ast.TableExpr)
	from = func(e ast.TableExpr) {
		switch e := e.(type) {
		case *ast.Derived:
			o.query(e.Select)
		case *ast.Join:
			from(e.Left)
			from(e.Right)
			o.clause(e.On)
		}
	}
	for _, f := range sel.From {
		from(f)
	}
	for _, item := range sel.Items {
		if !item.Star {
			o.expr(item.Expr)
		}
	}
	o.clause(sel.Where)
	o.clause(sel.Having)
	// output is the parameter that the output column that e names is, by its position or, in
	// ORDER BY, its alias, or nil.
	output := func(e ast.Expr, alias bool) *ast.Param {
		switch e := e.(type) {
		case *ast.Literal:
			if (e.Value.Type == value.Int4 || e.Value.Type == value.Int) && !e.Value.Null && e.Value.I >= 1 && e.Value.I <= int64(len(sel.Items)) {
				item := sel.Items[e.Value.I-1]
				p, _ := item.Expr.(*ast.Param)
				if !item.Star {
					return p
				}
			}
		case *ast.ColumnRef:
			for _, item := range sel.Items {
				if p, ok := item.Expr.(*ast.Param); ok && e.Table == "" && item.Alias == e.Name && item.Alias != "" {
					if !alias {
						// GROUP BY reads a column of FROM of that name first.
						o.imprecise[p.Index] = true
						return nil
					}
					return p
				}
			}
		case *ast.Param:
			// Postgres takes an expression that equals an output column as that column, which
			// for a parameter depends on the type it has by then.
			for _, item := range sel.Items {
				if p, ok := item.Expr.(*ast.Param); ok && p.Index == e.Index {
					o.imprecise[e.Index] = true
				}
			}
		}
		return nil
	}
	for _, item := range sel.OrderBy {
		at := o.expr(item.Expr) + 1
		o.coerce(item.Expr, at)
		if p := output(item.Expr, true); p != nil {
			o.coerce(p, at)
		}
	}
	for _, e := range sel.GroupBy {
		at := o.expr(e) + 1
		o.coerce(e, at)
		if p := output(e, false); p != nil {
			o.coerce(p, at)
		}
	}
	outputs := func(at int) {
		for _, item := range sel.Items {
			if !item.Star {
				o.coerce(item.Expr, at)
			}
		}
	}
	if sel.Distinct {
		outputs(o.tick())
	}
	o.clause(sel.Offset)
	o.clause(sel.Limit)
	outputs(o.tick())
}

// expr walks e and returns its position.
func (o *paramOrder) expr(e ast.Expr) int {
	at, start := 0, o.at
	switch e := e.(type) {
	case *ast.Param:
		at = o.tick()
		if o.sites[e] == nil {
			o.sites[e] = &paramSite{create: at}
			o.byNumber[e.Index] = append(o.byNumber[e.Index], e)
		}
	case *ast.Unary:
		if e.Op == ast.Not {
			o.clause(e.Expr)
			at = o.tick()
			break
		}
		o.expr(e.Expr)
		at = o.tick()
		o.coerce(e.Expr, at)
	case *ast.Binary:
		if e.Op == ast.And || e.Op == ast.Or {
			// Postgres coerces each operand to boolean right after it analyzes it.
			o.clause(e.Left)
			o.clause(e.Right)
			at = o.tick()
			break
		}
		o.expr(e.Left)
		o.expr(e.Right)
		at = o.tick()
		o.coerce(e.Left, at)
		o.coerce(e.Right, at)
	case *ast.IsNull:
		o.expr(e.Expr)
		at = o.tick()
	case *ast.Between:
		// Postgres analyzes x BETWEEN a AND b as x >= a AND x <= b, and so x twice: the second
		// time, a parameter in it has the type that the first comparison gave it. A copy of x
		// for the second comparison, which [desugar] uses, has references of its own, unless x
		// has a subquery, which the binder would then analyze twice.
		if e.Copy == nil && hasParam(e.Expr) && !hasSubquery(e.Expr) {
			e.Copy = ast.Map(e.Expr, func(x ast.Expr) ast.Expr {
				if p, ok := x.(*ast.Param); ok {
					return &ast.Param{Index: p.Index}
				}
				return x
			})
		}
		o.expr(e.Expr)
		o.expr(e.Low)
		ge := o.tick()
		o.coerce(e.Expr, ge)
		o.coerce(e.Low, ge)
		if e.Copy != nil {
			o.expr(e.Copy)
		}
		o.expr(e.High)
		le := o.tick()
		if e.Copy != nil {
			o.coerce(e.Copy, le)
		}
		o.coerce(e.High, le)
		at = o.tick()
	case *ast.In:
		o.expr(e.Expr)
		if p, ok := e.Expr.(*ast.Param); ok && o.compares(e.List) {
			o.lists[p] = true
		}
		for _, x := range e.List {
			o.expr(x)
		}
		at = o.tick()
		o.coerce(e.Expr, at)
		for _, x := range e.List {
			o.coerce(x, at)
		}
	case *ast.Like:
		o.expr(e.Expr)
		o.expr(e.Pattern)
		at = o.tick()
		o.coerce(e.Expr, at)
		o.coerce(e.Pattern, at)
	case *ast.Case:
		// Postgres coerces the operand to text, when it has no type, and each WHEN, right after
		// it analyzes them, and the results once it has analyzed them all.
		o.clause(e.Operand)
		for _, w := range e.Whens {
			o.clause(w.Cond)
			o.expr(w.Result)
		}
		if e.Else != nil {
			o.expr(e.Else)
		}
		at = o.tick()
		for _, w := range e.Whens {
			o.coerce(w.Result, at)
		}
		if e.Else != nil {
			o.coerce(e.Else, at)
		}
	case *ast.Call:
		for _, x := range e.Args {
			o.expr(x)
		}
		at = o.tick()
		for _, x := range e.Args {
			o.coerce(x, at)
		}
	case *ast.Cast:
		o.expr(e.Expr)
		at = o.tick()
		o.coerce(e.Expr, at)
	case *ast.Subquery:
		// Postgres analyzes the query of a subquery before the left side of an IN.
		o.query(e.Select)
		if e.Expr != nil {
			o.expr(e.Expr)
		}
		at = o.tick()
		if e.Expr != nil {
			o.coerce(e.Expr, at)
		}
	default:
		at = o.tick()
	}
	if e != nil {
		o.pos[e] = at
		o.start[e] = start
	}
	return at
}

// compares reports whether Postgres compares the left side of an IN with the items list one at
// a time: when at most one item reads no columns of the query that the IN is in. Two or more
// such items make an array, to whose common type Postgres coerces the left side once. A column
// of a query around, inside a subquery, is not a column of the query, and neither side is
// known for an item with a subquery, so those lists do not count.
func (o *paramOrder) compares(list []ast.Expr) bool {
	constants := 0
	for _, x := range list {
		columns, sub := false, false
		ast.Walk(x, func(y ast.Expr) bool {
			switch y.(type) {
			case *ast.ColumnRef:
				columns = true
			case *ast.Subquery:
				sub = true
			}
			return true
		})
		switch {
		case sub, columns && o.depth > 0:
			return false
		case !columns:
			constants++
		}
	}
	return constants <= 1
}

// hasParam reports whether e has a parameter.
func hasParam(e ast.Expr) bool {
	found := false
	ast.Walk(e, func(x ast.Expr) bool {
		_, p := x.(*ast.Param)
		found = found || p
		return !found
	})
	return found
}

// hasSubquery reports whether e has a subquery.
func hasSubquery(e ast.Expr) bool {
	found := false
	ast.Walk(e, func(x ast.Expr) bool {
		_, sub := x.(*ast.Subquery)
		found = found || sub
		return !found
	})
	return found
}

// located is an error of the analysis of the expression at the position at.
type located struct {
	err error
	at  int
}

func (e *located) Error() string { return e.err.Error() }
func (e *located) Unwrap() error { return e.err }

// locate err as an error of the analysis of e, unless it is located already, or it is a grouping
// error of a column. An expression that
// the binder rewrote, which has no position of its own, fails right after the last of its
// subexpressions that has one, since Postgres analyzes an expression after its subexpressions.
func (o *paramOrder) locate(e ast.Expr, err error) error {
	var l *located
	if errors.As(err, &l) {
		return err
	}
	if errors.Is(err, errUngrouped) {
		// Postgres checks the columns of a grouped query once it has analyzed all of the
		// query, so the error has no position of its own.
		return err
	}
	at, ok := o.pos[e]
	if !ok {
		at = o.enclosing(e)
	}
	if at > 0 {
		return &located{err: err, at: at}
	}
	return err
}

// enclosing is the position of the smallest expression of the statement that holds every
// expression of e that has a position, or 0. The binder rewrites some expressions, such as IN as
// comparisons of its left side with each item, which Postgres makes once it has analyzed every
// item, so an error of the rewritten expression comes where the expression it was written as
// ends.
func (o *paramOrder) enclosing(e ast.Expr) int {
	lo, hi := math.MaxInt, 0
	ast.Walk(e, func(x ast.Expr) bool {
		if p, ok := o.pos[x]; ok {
			lo, hi = min(lo, p), max(hi, p)
		}
		return true
	})
	if hi == 0 {
		return 0
	}
	best := 0
	for x, p := range o.pos {
		if o.start[x] < lo && p >= hi && (best == 0 || p < best) {
			best = p
		}
	}
	return best
}

// errUngrouped is the error of a column of a grouped query outside its GROUP BY and aggregates.
var errUngrouped = errors.New("ungrouped column")

// errRetype makes a statement analyze again, with the types that [paramLedger.check] found for
// the references that Postgres makes once their parameter has a type.
var errRetype = errors.New("exec: parameter types changed")

// paramLedger is what the analysis of a statement learns of its parameters, as Postgres learns it
// when it parses the statement: the type that the context of each reference gives it, from which
// Postgres infers one type per parameter, in the order of [paramOrder]; and the errors with which
// it refuses their values when it binds them, after the analysis and before it folds constants,
// in the order of their numbers.
//
// The binder reads a reference like a quoted literal, which takes the type of its context. That
// is what Postgres does with a reference that it makes before the parameter has a type. A
// reference that it makes after has the parameter's type, so when its context gives it another
// type, the statement is analyzed again, with that reference bound with the parameter's type.
type paramLedger struct {
	// n is the highest parameter number of the statement, and used the numbers it references.
	n    int
	used map[int]bool
	// order of the references, or nil when no parameter has more than one.
	order *paramOrder
	// retype are the references that are bound with the type their parameter has, and next
	// those of the next analysis, when [paramLedger.check] finds that they differ.
	retype, next map[*ast.Param]value.Type
	refs         map[*ast.Param]*paramRef
	// reached are the references that the analysis bound, with a context or not yet.
	reached map[*ast.Param]bool
	// refused, encode, and convert are the errors of the values that Postgres refuses, that the
	// client library refuses to encode, which it tries for every parameter before it sends
	// any, and that maragudb does not convert to the type, by parameter number.
	refused, encode, convert map[int]error
	// checked is set once the analysis of the statement is complete.
	checked bool
}

// paramRef is what the contexts of a reference to a parameter give it: the types, and untyped
// when it is in a context that gives none, such as IS NULL. A reference that is bound with the
// type of its parameter is pretyped.
type paramRef struct {
	types    []value.Type
	untyped  bool
	pretyped bool
}

// paramUse is a reference to the parameter n, which pre binds with the type its parameter has.
type paramUse struct {
	n      int
	p      *ast.Param
	pre    bool
	ledger *paramLedger
}

// newParamLedger for the statement st, whose references in retype are bound with those types.
func newParamLedger(st *Statement, retype map[*ast.Param]value.Type) *paramLedger {
	return &paramLedger{n: st.nparams, used: st.used, order: st.order, retype: retype, refs: map[*ast.Param]*paramRef{}, reached: map[*ast.Param]bool{}}
}

// use records a reference to a parameter.
func (l *paramLedger) use(p *ast.Param) *paramUse {
	l.reached[p] = true
	_, pre := l.retype[p]
	return &paramUse{n: p.Index, p: p, pre: pre, ledger: l}
}

func (u *paramUse) ref() *paramRef {
	r := u.ledger.refs[u.p]
	if r == nil {
		r = &paramRef{}
		u.ledger.refs[u.p] = r
	}
	return r
}

// typed records that a context gives the reference the type t, which is text for an unknown.
func (u *paramUse) typed(t value.Type) {
	if t == value.Unknown {
		t = value.String
	}
	r := u.ref()
	r.pretyped = r.pretyped || u.pre
	if !slices.Contains(r.types, t) {
		r.types = append(r.types, t)
	}
}

// untyped records that a context gives the reference no type, as IS NULL and count() do.
func (u *paramUse) untyped() {
	u.ref().untyped = true
}

// refuse records the error err for the value of parameter n, in the map m.
func (l *paramLedger) refuse(m *map[int]error, n int, err error) {
	if *m == nil {
		*m = map[int]error{}
	}
	if _, ok := (*m)[n]; !ok {
		(*m)[n] = err
	}
}

// typing is the outcome of the analysis of the parameters, as [paramLedger.typing] finds it.
type typing struct {
	// next are the references to bind with the type of their parameter, and changed reports
	// that they differ from those of the analysis.
	next    map[*ast.Param]value.Type
	changed bool
	// inconsistent is the error of the first reference, at the position at, that Postgres
	// makes before its parameter has a type and coerces to another type, and ambiguous that of
	// a reference that it makes before then in a context that gives none, which Postgres
	// reports once it has analyzed the statement.
	inconsistent, ambiguous error
	at                      int
	// unsupported is the error of a parameter whose type depends on an order that
	// [paramOrder] does not place.
	unsupported error
	// unbound is the first position of a reference that the analysis did not reach, or 0.
	unbound int
	// typed are the parameters that get a type, and loose those typed without their order.
	typed, loose map[int]bool
}

// settled reports whether the references to bind with the type of their parameter are those of
// retype, among the parameters typed in their order and the references that Postgres makes up to
// the position limit in the order o.
func (t typing) settled(retype map[*ast.Param]value.Type, o *paramOrder, limit int) bool {
	for p, typ := range t.next {
		if retype[p] != typ {
			return false
		}
	}
	for p := range retype {
		if _, ok := t.next[p]; !ok && !t.loose[p.Index] && o.sites[p].create <= limit {
			return false
		}
	}
	return true
}

// typing of the parameters from the references that the analysis recorded, as Postgres types
// them up to the position limit, which is past the end of the statement when its analysis did
// not fail: the references that it makes after limit, and the coercions after it, do not happen.
func (l *paramLedger) typing(limit int) typing {
	t := typing{next: map[*ast.Param]value.Type{}, typed: map[int]bool{}, loose: map[int]bool{}}
	complete := limit == math.MaxInt
	byNumber := map[int][]*ast.Param{}
	for p := range l.refs {
		if l.order == nil || l.order.sites[p] == nil || l.order.sites[p].create <= limit {
			byNumber[p.Index] = append(byNumber[p.Index], p)
		}
	}
	for n := 1; n <= l.n; n++ {
		precise := l.order != nil && !l.order.imprecise[n]
		var sites []*ast.Param
		if l.order != nil {
			for _, p := range l.order.byNumber[n] {
				if l.order.sites[p].create <= limit {
					sites = append(sites, p)
				}
			}
			for _, p := range byNumber[n] {
				precise = precise && l.order.sites[p] != nil
			}
		}
		// pending are the references that a failed analysis reached and that no context gave a
		// type yet, such as the operand of an operator that failed.
		pending := map[*ast.Param]bool{}
		for _, p := range sites {
			r, s := l.refs[p], l.order.sites[p]
			switch {
			case r == nil && !complete && l.reached[p]:
				pending[p] = true
			case r == nil:
				if t.unbound == 0 || s.create < t.unbound {
					t.unbound = s.create
				}
				precise = false
			case r.untyped && len(r.types) > 0, len(r.types) > 0 && !r.pretyped && s.coerce == 0:
				precise = false
			}
		}
		// The first reference that a context coerces gives the parameter its type. A reference
		// in several contexts, such as the left side of an IN, which Postgres compares with each
		// item, is placed only once the parameter has a type.
		var first *ast.Param
		if precise {
			for _, p := range sites {
				s := l.order.sites[p]
				if r := l.refs[p]; r != nil && len(r.types) > 0 && !r.pretyped && s.coerce < limit && (first == nil || s.coerce < l.order.sites[first].coerce) {
					first = p
				}
			}
		}
		if first != nil {
			for _, p := range sites {
				if r, s := l.refs[p], l.order.sites[p]; r != nil && len(r.types) > 1 && !r.pretyped && s.create < l.order.sites[first].coerce && s.coerce < limit {
					if !l.order.lists[p] {
						precise = false
						continue
					}
					// Postgres coerces the one reference to the type of each comparison.
					if t.inconsistent == nil || s.coerce < t.at {
						t.inconsistent = fmt.Errorf("%w: inconsistent types deduced for parameter $%d, %v and %v", ErrType, n, r.types[0], r.types[1])
						t.at = s.coerce
					}
				}
			}
		}
		if !precise || first == nil && complete && len(byNumber[n]) > 0 && !l.untypedOnly(byNumber[n]) {
			t.loose[n] = true
			l.conservative(n, byNumber[n], complete, &t)
			continue
		}
		if first == nil {
			continue
		}
		typ, typedAt := l.refs[first].types[0], l.order.sites[first].coerce
		t.typed[n] = true
		for _, p := range sites {
			r, s := l.refs[p], l.order.sites[p]
			after := s.create > typedAt
			switch {
			case pending[p]:
				if after {
					t.next[p] = typ
				}
			case r.untyped:
				if !after && t.ambiguous == nil {
					t.ambiguous = fmt.Errorf("%w: could not determine data type of parameter $%d, which a context that gives it no type, such as IS NULL, reads before Postgres gives it the type %v", ErrType, n, typ)
				}
			case after:
				if r.pretyped || len(r.types) > 1 || r.types[0] != typ {
					t.next[p] = typ
				}
			case s.coerce >= limit:
				// Postgres has not coerced the reference yet.
			case !r.pretyped && r.types[0] != typ && (t.inconsistent == nil || s.coerce < t.at):
				t.inconsistent = fmt.Errorf("%w: inconsistent types deduced for parameter $%d, %v and %v", ErrType, n, typ, r.types[0])
				t.at = s.coerce
			}
		}
	}
	t.changed = !maps.Equal(t.next, l.retype)
	return t
}

// untypedOnly reports whether every reference of refs is in a context that gives it no type.
func (l *paramLedger) untypedOnly(refs []*ast.Param) bool {
	for _, p := range refs {
		if r := l.refs[p]; !r.untyped || len(r.types) > 0 {
			return false
		}
	}
	return true
}

// conservative types the parameter n from its references refs without their order: the
// parameter has a type when every context gives it the same one, and is not supported when
// they give it different types, or a type and none, because Postgres's outcome then depends on
// their order, or, once the analysis is complete, none and the statement has a reference.
func (l *paramLedger) conservative(n int, refs []*ast.Param, complete bool, t *typing) {
	var types []value.Type
	untyped := false
	for _, p := range refs {
		r := l.refs[p]
		untyped = untyped || r.untyped
		for _, typ := range r.types {
			if !slices.Contains(types, typ) {
				types = append(types, typ)
			}
		}
	}
	t.typed[n] = len(types) > 0
	if t.unsupported != nil {
		return
	}
	switch {
	case len(types) > 1:
		t.unsupported = fmt.Errorf("%w: parameter $%d in contexts of the types %v and %v is not supported here, because Postgres gives a parameter the type of its first context, in an order that maragudb does not follow for this statement; cast the parameter, such as $%d::%v, or pass the value twice", ErrUnsupported, n, types[0], types[1], n, types[0])
	case len(types) == 1 && untyped:
		t.unsupported = fmt.Errorf("%w: parameter $%d in IS NULL or count() and in a context of type %v is not supported here, because whether Postgres determines its type depends on their order, which maragudb does not follow for this statement; cast the parameter, such as $%d::%v", ErrUnsupported, n, types[0], n, types[0])
	case len(types) == 0 && l.used[n] && !untyped && complete:
		t.unsupported = fmt.Errorf("%w: parameter $%d in a context whose type maragudb does not determine as Postgres does is not supported; cast the parameter, such as $%d::text", ErrUnsupported, n, n)
	}
}

// check reports, once the statement is analyzed, the errors of its parameters' types in the
// order of Postgres: 42P08 for a reference that Postgres makes before its parameter has a type
// and then coerces to another type or reads in a context that gives none; then 42P18 for a
// parameter that the statement skips, or whose contexts give it no type. It returns
// [errRetype] when a reference that Postgres makes after its parameter has a type has to be
// analyzed with that type.
func (l *paramLedger) check() error {
	if l == nil || l.checked {
		return nil
	}
	l.checked = true
	t := l.typing(math.MaxInt)
	switch {
	case t.changed:
		l.next = t.next
		return errRetype
	case t.unsupported != nil:
		return t.unsupported
	case t.inconsistent != nil:
		return t.inconsistent
	case t.ambiguous != nil:
		return t.ambiguous
	}
	for n := 1; n <= l.n; n++ {
		if !t.typed[n] {
			return fmt.Errorf("%w: could not determine data type of parameter $%d; cast it, such as $%d::bigint", ErrType, n, n)
		}
	}
	return nil
}

// failed returns the error of a statement whose analysis failed with err before it was
// complete, or [errRetype] when a reference that Postgres makes after its parameter has a type
// has to be analyzed with that type first. Postgres reports 42P08 for a reference that it
// coerces to another type than its parameter has where it meets it, so that comes first when it
// is before the error's position. When the analysis did not reach a reference that Postgres
// analyzes before the error, or the error has no position, the order of the two is not known.
func (l *paramLedger) failed(err error) error {
	if l == nil || l.checked || l.order == nil || errors.Is(err, errRetype) {
		return err
	}
	l.checked = true
	at := -1
	var loc *located
	if errors.As(err, &loc) {
		at = loc.at
	}
	limit := math.MaxInt
	if at >= 0 {
		limit = at
	}
	t := l.typing(limit)
	switch {
	case !t.settled(l.retype, l.order, limit):
		l.next = t.next
		return errRetype
	case errors.Is(err, ErrUnsupported):
		return err
	}
	switch {
	case at >= 0 && t.inconsistent != nil && t.at < at:
		return t.inconsistent
	case at >= 0 && !l.unboundBefore(at) && t.unsupported == nil:
		return err
	case at < 0 && t.inconsistent == nil && t.unsupported == nil && t.unbound == 0:
		return err
	}
	return fmt.Errorf("%w: the statement fails with %w, and the types that Postgres gives its parameters, which depend on the order in which it analyzes the statement, may make it fail with another error first; cast the parameters, such as $1::bigint", ErrUnsupported, err)
}

// unboundBefore reports whether a parameter with more than one reference that Postgres makes
// before the position at has one that the analysis did not reach. A parameter with one such
// reference cannot fail before at, whatever its type.
func (l *paramLedger) unboundBefore(at int) bool {
	for _, sites := range l.order.byNumber {
		before, unbound := 0, false
		for _, p := range sites {
			if l.order.sites[p].create <= at {
				before++
				unbound = unbound || !l.reached[p]
			}
		}
		if before > 1 && unbound {
			return true
		}
	}
	return false
}

// refusal is the error of the first parameter, by number, whose value maragudb does not convert
// to its type, or else that the client library refuses to encode, or else that Postgres
// refuses.
func (l *paramLedger) refusal() error {
	if l == nil {
		return nil
	}
	for _, m := range []map[int]error{l.convert, l.encode, l.refused} {
		for n := 1; n <= l.n; n++ {
			if err, ok := m[n]; ok {
				return err
			}
		}
	}
	return nil
}
