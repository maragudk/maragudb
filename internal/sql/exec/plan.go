package exec

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// filter is a bound WHERE clause: its top-level conjuncts, which a row must all satisfy.
//
// The filter behaves as Postgres does when it scans the table through its primary key index,
// which it does whenever a conjunct can use the index and sequential scans are off: the index
// conditions select the rows, and Postgres evaluates the other conjuncts only on those. A row
// that an index condition rejects is skipped, whatever the other conjuncts would do on it.
//
// Postgres evaluates the other conjuncts in an order its planner picks, by estimated cost and
// with equalities on constants last, and stops at the first that is not true. When one
// conjunct fails on a row that another conjunct rejects, whether the statement fails therefore
// depends on that order. The filter does not reproduce the order. It evaluates the conjuncts in
// the order they are written, and when the outcome on a row would differ between orders, it
// reports [errOrderDependent] instead, so that a statement it accepts gives the same outcome as
// in Postgres in every order.
type filter struct {
	conds []*cexpr
	// conjuncts are the conditions as written, for messages.
	conjuncts []ast.Expr
	// fallible is set when a conjunct can fail on a row.
	fallible bool
	// index are the conditions that Postgres uses as conditions of the scan of the primary key
	// index, from [binder.indexCondition] and [binder.impliedIndexConditions]. None of them can
	// fail. indexed marks the conds among them.
	index   []*cexpr
	indexed []bool
	// implied are equalities that follow from the equalities among the conjuncts, which
	// Postgres's planner derives through its equivalence classes and may evaluate instead of
	// the written ones. They never change which rows match, but one of them can reject a row on
	// which a conjunct fails, so they are checked when a conjunct fails.
	implied []*cexpr
	// never is set when a conjunct is known false or NULL, so no row can match and no row is
	// read.
	never bool
	// gates are the conjuncts that read no columns but are not constants either, because they
	// contain a stable function. Postgres evaluates them once, when the statement runs, before it
	// reads any row.
	gates []*cexpr
	// keyed are the index conditions that compare a primary key column with a constant, which
	// narrow the scan.
	keyed []keyed
	// unsupported is set when the outcome of the clause depends on the plan Postgres picks. It is
	// reported only after the whole statement is bound and folded, so that an error that
	// Postgres reports for certain comes first.
	unsupported error
}

// errOrderDependent is wrapped, together with [ErrUnsupported], by the error of a statement
// whose outcome on a row depends on the order in which Postgres evaluates its WHERE conditions.
var errOrderDependent = errors.New("order-dependent")

// keyed is a comparison of a primary key column with an expression that reads no columns, as
// seen from the column. When known is set, the expression is a constant whose value is k.
type keyed struct {
	col   int
	op    ast.BinaryOp
	k     value.Value
	known bool
}

// conjuncts of a WHERE clause as Postgres sees them after it simplifies the expression: ANDs
// are flattened, NOT is pushed down through AND, OR, comparisons, LIKE, and IS NULL, BETWEEN
// becomes two comparisons, an IN with one element or with elements that read columns becomes
// comparisons, and a comparison with a boolean constant becomes the other side.
func conjuncts(e ast.Expr) []ast.Expr {
	e = simplify(e)
	if b, ok := e.(*ast.Binary); ok && b.Op == ast.And {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	return []ast.Expr{e}
}

// simplify the top of an expression as Postgres's planner does before it splits a WHERE clause
// into conjuncts. The result is true exactly when e is, but it may be false where e is NULL,
// which is the same in a WHERE clause; so an operand of NOT is never simplified, but negated
// with [negate].
func simplify(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.Between:
		d, _ := desugar(x)
		return simplify(d)
	case *ast.In:
		if len(x.List) == 1 {
			op := ast.Eq
			if x.Not {
				op = ast.Ne
			}
			return simplify(&ast.Binary{Op: op, Left: x.Expr, Right: x.List[0]})
		}
		if d, ok := desugar(x); ok {
			return simplify(d)
		}
	case *ast.Unary:
		if x.Op == ast.Not {
			return negate(x.Expr)
		}
	case *ast.Binary:
		switch x.Op {
		case ast.And, ast.Or:
			return &ast.Binary{Op: x.Op, Left: simplify(x.Left), Right: simplify(x.Right)}
		case ast.Eq, ast.Ne:
			if b, ok := boolLiteral(x.Right); ok {
				return boolEquality(x.Left, b, x.Op == ast.Eq)
			}
			if b, ok := boolLiteral(x.Left); ok {
				return boolEquality(x.Right, b, x.Op == ast.Eq)
			}
			if x.Op == ast.Eq && ast.RenderExpr(x.Left) == ast.RenderExpr(x.Right) {
				// X = X is X IS NOT NULL for a strict operator, where X is not NULL; where it is,
				// the one is NULL and the other false.
				return &ast.IsNull{Expr: x.Left, Not: true}
			}
		}
	}
	return e
}

func boolLiteral(e ast.Expr) (bool, bool) {
	lit, ok := e.(*ast.Literal)
	if !ok || lit.Value.Null || lit.Value.Type != value.Bool {
		return false, false
	}
	return lit.Value.B, true
}

// boolEquality simplifies x = b and x <> b for a boolean constant b, as Postgres does: x = TRUE
// is x, x = FALSE is NOT x, and <> the other way round.
func boolEquality(x ast.Expr, b, eq bool) ast.Expr {
	if b == eq {
		return simplify(x)
	}
	return negate(x)
}

// negate an expression as Postgres's negate_clause does: through AND and OR with De Morgan's
// laws, by flipping comparison operators, LIKE, and IS NULL, and by dropping a double NOT. Like
// [simplify], the result is true exactly when NOT e is.
func negate(e ast.Expr) ast.Expr {
	switch x := e.(type) {
	case *ast.Literal:
		if b, ok := boolLiteral(x); ok {
			return &ast.Literal{Value: value.NewBool(!b)}
		}
	case *ast.Unary:
		if x.Op == ast.Not {
			return simplify(x.Expr)
		}
	case *ast.Binary:
		switch x.Op {
		case ast.And:
			return &ast.Binary{Op: ast.Or, Left: negate(x.Left), Right: negate(x.Right)}
		case ast.Or:
			return &ast.Binary{Op: ast.And, Left: negate(x.Left), Right: negate(x.Right)}
		case ast.Eq, ast.Ne, ast.Lt, ast.Le, ast.Gt, ast.Ge:
			return simplify(&ast.Binary{Op: negated[x.Op], Left: x.Left, Right: x.Right})
		}
	case *ast.Like:
		return &ast.Like{Expr: x.Expr, Pattern: x.Pattern, Not: !x.Not}
	case *ast.IsNull:
		return &ast.IsNull{Expr: x.Expr, Not: !x.Not}
	case *ast.Between:
		return simplify(&ast.Between{Expr: x.Expr, Low: x.Low, High: x.High, Not: !x.Not})
	case *ast.In:
		// An IN of constants stays an array comparison, whose negation is NOT IN.
		return simplify(&ast.In{Expr: x.Expr, List: x.List, Not: !x.Not})
	}
	return &ast.Unary{Op: ast.Not, Expr: e}
}

// whereConjuncts of a WHERE clause as Postgres sees them after it simplifies and canonicalizes
// the clause: the [conjuncts], each in its [binder.canonicalQual] form, which may split it into
// more conjuncts.
func (b *binder) whereConjuncts(e ast.Expr) []ast.Expr {
	var out []ast.Expr
	for _, c := range conjuncts(e) {
		if canon, ok := b.canonicalQual(c); ok {
			c = canon
		}
		out = append(out, flatten(c, ast.And)...)
	}
	return out
}

// canonicalQual rewrites a conjunct as Postgres's canonicalize_qual does with a WHERE clause
// after it folds constants. Through AND and OR, an argument that is a constant decides the
// result or drops out, where NULL counts as FALSE, as it does for a WHERE clause; and the
// conditions that every argument of an OR has in common are taken out of the OR, so that
// (a AND b) OR (a AND c) becomes a AND (b OR c), and (a AND b) OR a becomes a. The result is
// true exactly when e is. It matters because a condition taken out of an OR can be an index
// condition.
//
// The result is the constant FALSE only where folding the constants makes it FALSE. Where it is
// false only because NULL counts as FALSE, as in x AND NULL, the result is NULL instead, which
// the WHERE clause treats the same, except that Postgres folds the constants of the conjuncts
// after a NULL, but not those after a FALSE.
//
// Postgres checks the types of every argument, and then folds the arguments from the left, and
// stops at the first FALSE of an AND and the first TRUE of an OR. When an argument is mistyped,
// or folding an argument before that raises an error, the statement fails with it, so
// canonicalQual reports false, and e must be bound as it is to report the error.
func (b *binder) canonicalQual(e ast.Expr) (ast.Expr, bool) {
	bin, ok := e.(*ast.Binary)
	if !ok || bin.Op != ast.And && bin.Op != ast.Or {
		return e, true
	}
	// decides is the constant that decides the result, and drops the one that drops out; NULL
	// drops out too.
	decides := bin.Op == ast.Or
	var args []ast.Expr
	sawNull := false
	all := flatten(e, bin.Op)
	for _, a := range all {
		a, ok := b.canonicalQual(a)
		if !ok {
			return e, false
		}
		c := b.constantOf(a)
		switch {
		case c == constantFailed:
			return e, false
		case c == constantNull:
			sawNull = true
			continue
		case c == constantTrue && decides, c == constantFalse && !decides:
			if !b.typeChecks(all) {
				return e, false
			}
			return &ast.Literal{Value: value.NewBool(c == constantTrue)}, true
		case c != notConstant:
			continue
		}
		args = append(args, flatten(a, bin.Op)...)
	}
	switch {
	case bin.Op == ast.And && sawNull, bin.Op == ast.Or && len(args) == 0 && sawNull:
		return &ast.Literal{Value: value.NewNull(value.Bool)}, true
	case len(args) == 0:
		return &ast.Literal{Value: value.NewBool(bin.Op == ast.And)}, true
	case bin.Op == ast.And:
		return join(args, ast.And), true
	}
	return b.factorOr(args), true
}

// typeChecks reports whether every expression binds as a boolean, without folding.
func (b *binder) typeChecks(es []ast.Expr) bool {
	for _, e := range es {
		ce, err := b.bindPruned(e)
		if err != nil {
			return false
		}
		if ce, err = resolve(ce, value.Bool); err != nil || ce.typ != value.Bool {
			return false
		}
	}
	return true
}

// constness is what [binder.constantOf] finds an expression to be.
type constness int

const (
	notConstant constness = iota
	constantTrue
	constantFalse
	constantNull
	// constantFailed is an expression whose folding raises an error.
	constantFailed
)

// constantOf binds e to tell whether it folds to a constant, and which. The binding records
// nothing in the binder. An expression that reads columns is not a constant, and is not bound.
func (b *binder) constantOf(e ast.Expr) constness {
	if readsColumns(e) {
		return notConstant
	}
	foldErr, foldPhase, stableErr := b.foldErr, b.foldPhase, b.stableErr
	b.foldErr, b.stableErr = nil, nil
	ce, err := b.bind(e)
	if err == nil {
		ce, err = resolve(ce, value.Bool)
	}
	failed := err != nil || b.foldErr != nil || ce.typ != value.Bool
	b.foldErr, b.foldPhase, b.stableErr = foldErr, foldPhase, stableErr
	switch {
	case failed:
		// Binding the conjunct reports the error.
		return constantFailed
	case !ce.folded:
		return notConstant
	case ce.val.Null:
		return constantNull
	case ce.val.B:
		return constantTrue
	}
	return constantFalse
}

// factorOr is the OR of args with the conditions that every argument has in common taken out, as
// Postgres's process_duplicate_ors does: each argument is the AND of its conjuncts, those of the
// shortest argument that every argument has are the winners, and the result is the AND of the
// winners and the OR of what is left of each argument, or of the winners alone when nothing is
// left of one argument.
func (b *binder) factorOr(args []ast.Expr) ast.Expr {
	if len(args) == 1 {
		return args[0]
	}
	// Postgres compares the conditions as trees, which their [binder.canonical] text
	// approximates.
	type cond struct {
		e   ast.Expr
		key string
	}
	arms := make([][]cond, len(args))
	ref := 0
	for i, a := range args {
		for _, c := range flatten(a, ast.And) {
			arms[i] = append(arms[i], cond{e: c, key: b.canonical(c)})
		}
		if len(arms[i]) < len(arms[ref]) {
			ref = i
		}
	}
	has := func(cs []cond, key string) bool {
		return slices.ContainsFunc(cs, func(c cond) bool { return c.key == key })
	}
	var winners []cond
	for _, c := range arms[ref] {
		if !has(winners, c.key) && !slices.ContainsFunc(arms, func(arm []cond) bool { return !has(arm, c.key) }) {
			winners = append(winners, c)
		}
	}
	if len(winners) == 0 {
		return join(args, ast.Or)
	}
	exprs := func(cs []cond) []ast.Expr {
		var out []ast.Expr
		for _, c := range cs {
			out = append(out, c.e)
		}
		return out
	}
	var rest []ast.Expr
	for _, arm := range arms {
		left := slices.DeleteFunc(slices.Clone(arm), func(c cond) bool { return has(winners, c.key) })
		if len(left) == 0 {
			return join(exprs(winners), ast.And)
		}
		rest = append(rest, join(exprs(left), ast.And))
	}
	return join(append(exprs(winners), join(rest, ast.Or)), ast.And)
}

// flatten the arguments of nested op, which is AND or OR, into one list.
func flatten(e ast.Expr, op ast.BinaryOp) []ast.Expr {
	if b, ok := e.(*ast.Binary); ok && b.Op == op {
		return append(flatten(b.Left, op), flatten(b.Right, op)...)
	}
	return []ast.Expr{e}
}

// join expressions with op, which is AND or OR, from the left.
func join(es []ast.Expr, op ast.BinaryOp) ast.Expr {
	out := es[0]
	for _, e := range es[1:] {
		out = &ast.Binary{Op: op, Left: out, Right: e}
	}
	return out
}

var negated = map[ast.BinaryOp]ast.BinaryOp{ast.Eq: ast.Ne, ast.Ne: ast.Eq, ast.Lt: ast.Ge, ast.Ge: ast.Lt, ast.Gt: ast.Le, ast.Le: ast.Gt}

// clause is one conjunct of a WHERE clause, bound.
type clause struct {
	expr ast.Expr
	ce   *cexpr
}

// where binds a WHERE clause into a filter, and rejects a clause whose outcome would depend on
// the plan Postgres chooses.
func (b *binder) where(e ast.Expr) (filter, error) {
	if e == nil {
		return filter{}, nil
	}
	var f filter
	var clauses []clause
	pruned := false
	null := false
	for _, c := range b.whereConjuncts(e) {
		var ce *cexpr
		var err error
		if pruned {
			ce, err = b.bindPruned(c)
		} else {
			ce, err = b.bind(c)
		}
		if err != nil {
			return filter{}, err
		}
		if ce, err = resolve(ce, value.Bool); err != nil {
			return filter{}, err
		}
		if ce.typ != value.Bool {
			return filter{}, fmt.Errorf("%w: argument of WHERE must be type boolean, not type %v", ErrType, ce.typ)
		}
		if pruned {
			continue
		}
		if ce.folded && !ce.val.Null {
			if ce.val.B {
				continue
			}
			// A known false conjunct decides the clause; the rest is only type-checked.
			pruned = true
			f.never = true
			continue
		}
		if ce.folded {
			// A known NULL conjunct decides the clause too, because Postgres evaluates a conjunct
			// without columns once before it reads any row. Unlike FALSE, it does not stop the
			// folding of the conjuncts after it.
			null = true
			continue
		}
		if !ce.refs {
			f.gates = append(f.gates, ce)
			continue
		}
		clauses = append(clauses, clause{expr: c, ce: ce})
	}
	if f.never || null {
		return filter{never: true}, nil
	}
	for _, c := range clauses {
		f.conds = append(f.conds, c.ce)
		f.conjuncts = append(f.conjuncts, c.expr)
		f.fallible = f.fallible || c.ce.fallible
		f.indexed = append(f.indexed, b.indexCondition(c))
		if !f.indexed[len(f.indexed)-1] {
			continue
		}
		f.index = append(f.index, c.ce)
		if k, ok, err := b.keyComparison(c.expr); err != nil {
			return filter{}, err
		} else if ok && k.known {
			f.keyed = append(f.keyed, k)
		}
	}
	classes, err := b.equivalences(clauses)
	if err != nil {
		return filter{}, err
	}
	index, keyed, err := b.impliedIndexConditions(classes)
	if err != nil {
		return filter{}, err
	}
	f.index = append(f.index, index...)
	f.keyed = append(f.keyed, keyed...)
	f.unsupported = b.infallibleIndexConditions(clauses)
	if f.fallible {
		if f.implied, err = b.implied(classes); err != nil {
			return filter{}, err
		}
	}
	return f, nil
}

// member of an equivalence class: one side of an equality conjunct, bound in the type the
// equality compares in.
type member struct {
	expr ast.Expr
	ce   *cexpr
	// key identifies the member by its text and the type it is compared in, so that a column
	// compared as a bigint and the same column cast to double precision are different members.
	key string
}

// equivalences groups the sides of the equality conjuncts into classes of expressions that are
// equal on every matching row, as Postgres's planner does.
func (b *binder) equivalences(clauses []clause) ([][]*member, error) {
	var classes [][]*member
	classOf := map[string]int{}
	for _, c := range clauses {
		bin, ok := c.expr.(*ast.Binary)
		if !ok || bin.Op != ast.Eq {
			continue
		}
		lce, err := b.bindPruned(bin.Left)
		if err != nil {
			return nil, err
		}
		rce, err := b.bindPruned(bin.Right)
		if err != nil {
			return nil, err
		}
		t, err := commonType(lce.typ, rce.typ)
		if err != nil {
			// The conjunct bound, so its sides unify; this is a defensive check only.
			continue
		}
		var sides []*member
		for i, ce := range []*cexpr{lce, rce} {
			if ce, err = b.coerce(ce, t); err != nil {
				return nil, err
			}
			side := []ast.Expr{bin.Left, bin.Right}[i]
			sides = append(sides, &member{expr: side, ce: ce, key: ast.RenderExpr(side) + "@" + t.String()})
		}
		target := -1
		for _, m := range sides {
			if i, ok := classOf[m.key]; ok {
				target = i
				break
			}
		}
		if target < 0 {
			target = len(classes)
			classes = append(classes, nil)
		}
		for _, m := range sides {
			i, ok := classOf[m.key]
			switch {
			case !ok:
				classes[target] = append(classes[target], m)
				classOf[m.key] = target
			case i != target:
				// Merge the other class into the target, keeping member order.
				for _, om := range classes[i] {
					classOf[om.key] = target
				}
				classes[target] = append(classes[target], classes[i]...)
				classes[i] = nil
			}
		}
	}
	return slices.DeleteFunc(classes, func(c []*member) bool { return c == nil }), nil
}

// implied binds an equality between every two members of every class, which is a superset of
// the clauses Postgres derives from the classes.
func (b *binder) implied(classes [][]*member) ([]*cexpr, error) {
	var out []*cexpr
	for _, class := range classes {
		for i, m := range class {
			for _, o := range class[i+1:] {
				ce, err := b.bindPruned(&ast.Binary{Op: ast.Eq, Left: m.expr, Right: o.expr})
				if err != nil {
					return nil, err
				}
				out = append(out, ce)
			}
		}
	}
	return out, nil
}

// infallibleIndexConditions checks that no conjunct that Postgres could take as a condition of
// the scan of the primary key index can fail. Such a condition is evaluated on the rows of the
// index, and in a form that Postgres derives from it: for a LIKE with a fixed prefix, a range of
// the prefix, or an equality when the pattern has no wildcard. Which rows it fails on then
// depends on that form, and the LIKE itself is evaluated on fewer rows than [filter.matches]
// would evaluate it on.
func (b *binder) infallibleIndexConditions(clauses []clause) error {
	for _, c := range clauses {
		if c.ce.fallible && b.indexable(c.expr) {
			return fmt.Errorf("%w: whether %s fails depends on the plan Postgres picks, because an index scan evaluates it only on the rows the primary key index finds; write the pattern without a trailing escape character, or compare the column as an expression", ErrUnsupported, ast.RenderExpr(c.expr))
		}
	}
	return nil
}

// indexCondition reports whether Postgres 18, with sequential scans off, always uses a conjunct
// as a condition of its scan of the primary key index, so that it evaluates the other conjuncts
// only on the rows the conjunct accepts. That is the case for a conjunct that cannot fail and is
// one of:
//   - a comparison with =, <, <=, >, or >= of any primary key column with an expression that
//     reads no columns, compared in the column's own type, such as id = 1 or 5 < id;
//   - an IN of such expressions on a key column, which is an array comparison, such as
//     id IN (1, 2);
//   - a boolean key column, or NOT of one, which Postgres takes as k = true and k = false.
//
// Postgres puts every such conjunct into the conditions of every scan it considers on the index,
// a plain index scan, an index-only scan, or a bitmap scan, and with sequential scans off it
// always picks one of those over the sequential scan. Other conjuncts that can use the index are
// not always conditions of the scan, because the scan Postgres picks depends on costs: an OR of
// key conditions is a bitmap OR, or for equalities on one column an array comparison, but may
// also be checked on every row of an index scan in key order; and a LIKE is checked on the rows
// of the range of its prefix. Those are evaluated like any other conjunct, which is never less
// strict than Postgres, because a row that they reject is [errOrderDependent] when another
// conjunct fails on it.
func (b *binder) indexCondition(c clause) bool {
	if c.ce.fallible {
		return false
	}
	switch x := c.expr.(type) {
	case *ast.Binary:
		_, ok, err := b.keyComparison(x)
		return err == nil && ok
	case *ast.In:
		return b.keyIn(x)
	case *ast.ColumnRef:
		return b.booleanKey(x)
	case *ast.Unary:
		return x.Op == ast.Not && b.booleanKey(x.Expr)
	}
	return false
}

// booleanKey reports whether e is a primary key column of type boolean.
func (b *binder) booleanKey(e ast.Expr) bool {
	col, ok := b.keyColumn(e)
	return ok && b.table.Columns[col].Type == value.Bool
}

// impliedIndexConditions returns an equality between a primary key column and a constant for
// every key column in an equivalence class that also has a constant, such as id = 5 from
// id = n and n = 5. Postgres derives these equalities from the class, and takes them as
// conditions of the scan of the primary key index. It also returns those whose constant is
// known, to narrow the scan.
func (b *binder) impliedIndexConditions(classes [][]*member) ([]*cexpr, []keyed, error) {
	var index []*cexpr
	var known []keyed
	for _, class := range classes {
		var constant *member
		for _, m := range class {
			if !m.ce.refs && !m.ce.fallible {
				constant = m
				break
			}
		}
		if constant == nil {
			continue
		}
		for _, m := range class {
			col, ok := b.keyColumn(m.expr)
			if !ok || m.ce.typ != b.table.Columns[col].Type {
				continue
			}
			ce, err := b.bindPruned(&ast.Binary{Op: ast.Eq, Left: m.expr, Right: constant.expr})
			if err != nil {
				return nil, nil, err
			}
			if ce.fallible {
				continue
			}
			index = append(index, ce)
			if constant.ce.folded {
				known = append(known, keyed{col: col, op: ast.Eq, k: constant.ce.val, known: true})
			}
		}
	}
	return index, known, nil
}

// indexable reports whether Postgres could evaluate a conjunct as a condition on the primary
// key index: a comparison of a key column with a constant in the column's own type, an IN of
// constants on one, a LIKE with a fixed prefix or no wildcards on a text one, or an OR of such.
func (b *binder) indexable(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Binary:
		if x.Op == ast.Or {
			return b.indexable(x.Left) && b.indexable(x.Right)
		}
		k, ok, err := b.keyComparison(e)
		return err == nil && ok && (!k.known || !k.k.Null)
	case *ast.In:
		return b.keyIn(x)
	case *ast.Like:
		col, ok := b.keyColumn(x.Expr)
		if !ok || b.table.Columns[col].Type != value.String {
			return false
		}
		// Postgres takes the fixed prefix of a constant pattern, which a parameter is too, as an
		// index condition.
		p, err := b.bindPruned(x.Pattern)
		if err != nil || !p.folded || p.val.Null || p.val.Type != value.Unknown && p.val.Type != value.String {
			return false
		}
		prefix, exact := likePrefix(p.val.S)
		return exact || prefix != ""
	}
	return false
}

// keyIn reports whether e is an IN of expressions that read no columns on a primary key column,
// compared in the column's own type, which Postgres compares with an array.
func (b *binder) keyIn(e *ast.In) bool {
	col, ok := b.keyColumn(e.Expr)
	if !ok || e.Not {
		return false
	}
	for _, item := range e.List {
		if readsColumns(item) {
			return false
		}
		ce, err := b.bindPruned(item)
		if err != nil {
			return false
		}
		if _, err := b.coerce(ce, b.table.Columns[col].Type); err != nil {
			return false
		}
	}
	return true
}

// keyColumn returns the index of the primary key column that e names, if it does.
func (b *binder) keyColumn(e ast.Expr) (int, bool) {
	ref, ok := e.(*ast.ColumnRef)
	if !ok || b.table == nil {
		return 0, false
	}
	col, ok := b.table.columnIndex(ref.Name)
	if !ok || !b.table.isPrimaryKey(col) {
		return 0, false
	}
	return col, true
}

// likePrefix is the fixed prefix of a LIKE pattern, up to its first wildcard, and whether the
// pattern has no wildcard at all, as Postgres extracts it for an index condition.
func likePrefix(pattern string) (string, bool) {
	var prefix strings.Builder
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '%', '_':
			return prefix.String(), false
		case '\\':
			i++
			if i >= len(runes) {
				return prefix.String(), true
			}
		}
		prefix.WriteRune(runes[i])
	}
	return prefix.String(), true
}

// keyComparison recognizes `column op constant` or `constant op column` on a primary key column,
// compared in the column's own type, and returns the column index, the operator as seen from the
// column, and the constant.
func (b *binder) keyComparison(e ast.Expr) (keyed, bool, error) {
	bin, ok := e.(*ast.Binary)
	if !ok {
		return keyed{}, false, nil
	}
	// The operator as seen from the column when the column is on the right.
	flipped := map[ast.BinaryOp]ast.BinaryOp{ast.Eq: ast.Eq, ast.Lt: ast.Gt, ast.Le: ast.Ge, ast.Gt: ast.Lt, ast.Ge: ast.Le}
	op, ok := flipped[bin.Op]
	if !ok {
		return keyed{}, false, nil
	}
	colExpr, other := bin.Left, bin.Right
	if _, isRef := bin.Left.(*ast.ColumnRef); isRef {
		op = bin.Op
	} else {
		colExpr, other = bin.Right, bin.Left
	}
	col, ok := b.keyColumn(colExpr)
	if !ok || readsColumns(other) {
		return keyed{}, false, nil
	}
	k, err := b.bindPruned(other)
	if err != nil {
		return keyed{}, false, err
	}
	colType := b.table.Columns[col].Type
	if k, err = b.coerce(k, colType); err != nil {
		// The comparison is not in the column's type, such as a bigint column compared with a
		// float, where Postgres casts the column.
		return keyed{}, false, nil
	}
	// The comparison is an index condition with any expression that reads no columns, but its
	// value is only known now when it is a constant.
	return keyed{col: col, op: op, k: k.val, known: k.folded}, true, nil
}

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

// plan the access path for a bound WHERE clause over t. The conjuncts that compare a primary
// key column with a constant narrow the scan: equalities on a prefix of the key columns, and a
// range on the column after them. Every row read is still filtered by the whole WHERE clause,
// so the plan only has to be sound, never exact.
func (t *table) plan(f filter) access {
	start, end := t.rowRange()
	a := access{start: start, end: end}
	if f.never {
		return access{empty: true}
	}
	type bound struct {
		v         value.Value
		inclusive bool
	}
	eq := map[int]value.Value{}
	lower := map[int]bound{}
	upper := map[int]bound{}
	for _, k := range f.keyed {
		if k.k.Null {
			return access{empty: true}
		}
		switch k.op {
		case ast.Eq:
			if prev, dup := eq[k.col]; dup && value.Compare(prev, k.k) != 0 {
				return access{empty: true}
			}
			eq[k.col] = k.k
		case ast.Gt, ast.Ge:
			if prev, has := lower[k.col]; !has || value.Compare(k.k, prev.v) > 0 {
				lower[k.col] = bound{k.k, k.op == ast.Ge}
			}
		case ast.Lt, ast.Le:
			if prev, has := upper[k.col]; !has || value.Compare(k.k, prev.v) < 0 {
				upper[k.col] = bound{k.k, k.op == ast.Le}
			}
		}
	}
	prefix := start
	for i, col := range t.PrimaryKey {
		if k, ok := eq[col]; ok {
			prefix = value.EncodeKey(prefix, k)
			if i == len(t.PrimaryKey)-1 {
				return access{point: prefix}
			}
			continue
		}
		a.start, a.end = prefix, append(slices.Clone(prefix), 0xFF)
		if lo, ok := lower[col]; ok {
			a.start = value.EncodeKey(slices.Clone(prefix), lo.v)
		}
		if hi, ok := upper[col]; ok {
			a.end = append(value.EncodeKey(slices.Clone(prefix), hi.v), 0xFF)
		}
		return a
	}
	return a
}

// matches reports whether a row satisfies every conjunct. A row that an index condition rejects
// does not match, and no other conjunct is evaluated on it, because Postgres never reads it.
// When a conjunct fails on the row, the error is the statement's only when every other
// conjunct holds or fails too, because Postgres then fails on the row in every order; when
// another conjunct, or an equality that the conjuncts imply, rejects the row, Postgres may
// evaluate that one first and skip the row, so the error is [errOrderDependent]. Likewise, a
// row that a conjunct rejects is [errOrderDependent] when a later conjunct would fail on it.
func (f filter) matches(row []value.Value) (bool, error) {
	if f.never {
		return false, nil
	}
	if f.fallible {
		// Without a conjunct that can fail, the index conditions are evaluated with the rest, and
		// with one, they are evaluated first and only once.
		for _, c := range f.index {
			v, err := c.value(row)
			if err != nil {
				return false, err
			}
			if v.Null || !v.B {
				return false, nil
			}
		}
	}
	for i, c := range f.conds {
		if f.fallible && f.indexed[i] {
			continue
		}
		v, err := c.value(row)
		if err != nil {
			return false, f.failed(row, i, err)
		}
		if v.Null || !v.B {
			if f.fallible {
				for j := i + 1; j < len(f.conds); j++ {
					if f.conds[j].fallible {
						if _, err := f.conds[j].value(row); err != nil {
							return false, f.orderDependent(j, f.conjuncts[i])
						}
					}
				}
			}
			return false, nil
		}
	}
	return true, nil
}

// open evaluates the gates when the statement runs, and reports whether any row can match.
// When one gate fails and another is false or NULL, the outcome depends on the order in which
// Postgres evaluates them, which is [errOrderDependent].
func (f filter) open() (bool, error) {
	if f.never {
		return false, nil
	}
	var failure error
	open := true
	for _, g := range f.gates {
		v, err := g.value(nil)
		switch {
		case err != nil:
			failure = cmp.Or(failure, err)
		case v.Null || !v.B:
			open = false
		}
	}
	if failure != nil && !open {
		return false, fmt.Errorf("%w: %w: whether this statement fails depends on the order in which Postgres evaluates the conditions of its WHERE clause that read no columns; combine them into one condition with CASE", ErrUnsupported, errOrderDependent)
	}
	return open, failure
}

// failed returns the error of a row on which conjunct i failed with err.
func (f filter) failed(row []value.Value, i int, err error) error {
	for j := i + 1; j < len(f.conds); j++ {
		if v, cerr := f.conds[j].value(row); cerr == nil && (v.Null || !v.B) {
			return f.orderDependent(i, f.conjuncts[j])
		}
	}
	for _, c := range f.implied {
		if v, cerr := c.value(row); cerr == nil && (v.Null || !v.B) {
			return f.orderDependent(i, nil)
		}
	}
	return err
}

func (f filter) orderDependent(failing int, rejecting ast.Expr) error {
	failed := ast.RenderExpr(f.conjuncts[failing])
	if rejecting == nil {
		return fmt.Errorf("%w: %w: whether this statement fails depends on the order in which Postgres evaluates its WHERE conditions, because %s fails on a row that the equalities of the WHERE clause reject; write CASE WHEN <the equalities> THEN %s END", ErrUnsupported, errOrderDependent, failed, failed)
	}
	other := ast.RenderExpr(rejecting)
	return fmt.Errorf("%w: %w: whether this statement fails depends on the order in which Postgres evaluates its WHERE conditions, because %s fails on a row that %s rejects; write CASE WHEN %s THEN %s END", ErrUnsupported, errOrderDependent, failed, other, other, failed)
}
