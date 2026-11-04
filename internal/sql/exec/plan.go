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
// Postgres evaluates the conjuncts of a WHERE clause in an order its planner picks, by
// estimated cost and with equalities on constants last, and stops at the first that is not
// true. When one conjunct fails on a row that another conjunct rejects, whether the statement
// fails therefore depends on that order. The filter does not reproduce the order. It evaluates
// the conjuncts in the order they are written, and when the outcome on a row would differ
// between orders, it reports [errOrderDependent] instead, so that a statement it accepts gives
// the same outcome as in Postgres in every order.
type filter struct {
	conds []*cexpr
	// conjuncts are the conditions as written, for messages.
	conjuncts []ast.Expr
	// fallible is set when a conjunct can fail on a row.
	fallible bool
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
	// keyed are the conjuncts that compare a primary key column with a constant, which narrow
	// the scan.
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
// into conjuncts.
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
			return negate(simplify(x.Expr))
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
				// X = X is X IS NOT NULL for a strict operator.
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
	x = simplify(x)
	if b == eq {
		return x
	}
	return negate(x)
}

// negate an expression as Postgres's negate_clause does: through AND and OR with De Morgan's
// laws, by flipping comparison operators, LIKE, and IS NULL, and by dropping a double NOT.
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
	case *ast.Between, *ast.In:
		if simplified := simplify(x); simplified != e {
			return negate(simplified)
		}
		// An IN of constants stays an array comparison, whose negation is NOT IN.
		if in, ok := x.(*ast.In); ok {
			return &ast.In{Expr: in.Expr, List: in.List, Not: !in.Not}
		}
	}
	return &ast.Unary{Op: ast.Not, Expr: e}
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
	for _, c := range conjuncts(e) {
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
	f.unsupported = b.planIndependent(clauses, classes)
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

// planIndependent checks that the outcome of a WHERE clause cannot depend on whether Postgres
// scans the table or uses the primary key index. An index scan evaluates the other conditions
// only on the rows that the conditions on primary key columns find, so a condition that can fail
// on a row may never see that row in one plan and fail on it in another. The conditions on key
// columns include those that Postgres derives from equalities, such as id = 5 from id = n and
// n = 5.
func (b *binder) planIndependent(clauses []clause, classes [][]*member) error {
	var keyCond string
	for _, c := range clauses {
		if b.indexable(c.expr) {
			if c.ce.fallible {
				return fmt.Errorf("%w: whether %s fails depends on the plan Postgres picks, because an index scan evaluates it only on the rows the primary key index finds; write the pattern without a trailing escape character, or compare the column as an expression", ErrUnsupported, ast.RenderExpr(c.expr))
			}
			if keyCond == "" {
				keyCond = ast.RenderExpr(c.expr)
			}
		}
	}
	if keyCond == "" {
		keyCond = b.impliedKeyCondition(classes)
	}
	if keyCond == "" {
		return nil
	}
	for _, c := range clauses {
		if c.ce.fallible {
			return fmt.Errorf("%w: whether %s fails depends on the plan Postgres picks, because an index scan evaluates it only on the rows that the primary key condition %s finds, and a sequential scan on every row; write CASE WHEN %s THEN %s END",
				ErrUnsupported, ast.RenderExpr(c.expr), keyCond, keyCond, ast.RenderExpr(c.expr))
		}
	}
	return nil
}

// impliedKeyCondition returns an equality between a primary key column and a constant that
// follows from the equivalence classes, or "" when there is none.
func (b *binder) impliedKeyCondition(classes [][]*member) string {
	for _, class := range classes {
		var col, constant *member
		for _, m := range class {
			if i, ok := b.keyColumn(m.expr); ok && m.ce.typ == b.table.Columns[i].Type && col == nil {
				col = m
			}
			if !m.ce.refs && constant == nil {
				constant = m
			}
		}
		if col != nil && constant != nil {
			return ast.RenderExpr(&ast.Binary{Op: ast.Eq, Left: col.expr, Right: constant.expr})
		}
	}
	return ""
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
		col, ok := b.keyColumn(x.Expr)
		if !ok || x.Not {
			return false
		}
		for _, item := range x.List {
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

// matches reports whether a row satisfies every conjunct. When a conjunct fails on the row, the
// error is the statement's only when every other conjunct holds or fails too, because Postgres
// then fails on the row in every order; when another conjunct, or an equality that the
// conjuncts imply, rejects the row, Postgres may evaluate that one first and skip the row, so
// the error is [errOrderDependent]. Likewise, a row that a conjunct rejects is
// [errOrderDependent] when a later conjunct would fail on it.
func (f filter) matches(row []value.Value) (bool, error) {
	if f.never {
		return false, nil
	}
	for i, c := range f.conds {
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
