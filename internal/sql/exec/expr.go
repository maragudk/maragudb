package exec

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// cexpr is a bound expression: type-checked, with parameters substituted and constant
// subexpressions folded.
type cexpr struct {
	typ value.Type
	// refs reports whether the expression reads columns.
	refs bool
	// folded reports that the expression is a constant whose value is in val.
	folded bool
	val    value.Value
	eval   func(row []value.Value) (value.Value, error)
	// fallible reports that evaluating the expression on a row can fail: an overflow, a
	// division, a cast of text or a float, a LIKE whose pattern is not constant.
	fallible bool
	// stable reports that the expression contains a function that Postgres marks stable rather
	// than immutable, which among the supported ones are the conversions between timestamptz and
	// text. Postgres does not fold such an expression, so it is evaluated when the statement
	// runs, even when it reads no columns.
	stable bool
	// pruned reports that the expression was bound in a branch that is never evaluated, so that
	// a conversion applied to it later, when it is unified with the live branches, does not
	// report a folding error either.
	pruned bool
	// param is the number of the parameter the expression is, when it is a parameter of a Go
	// type other than string, which has type [value.Unknown] until its context gives it one.
	// Its value is in val, typed by its Go type.
	param int
	// aggs are the aggregates of a grouped SELECT whose results the expression reads, by their
	// index in its [grouping].
	aggs []int
}

// value of the expression for row.
func (e *cexpr) value(row []value.Value) (value.Value, error) {
	if e.folded {
		return e.val, nil
	}
	return e.eval(row)
}

func constant(v value.Value) *cexpr {
	return &cexpr{typ: v.Type, folded: true, val: v}
}

// binder binds expressions against an optional scope of tables and the statement's parameters.
type binder struct {
	// scope of the statement's tables, and ns the namespace that names resolve in. Both are made
	// from table when they are nil.
	scope *scope
	ns    namespace
	// layout is the offset of each entry's columns in the rows that expressions read, or nil for
	// the offsets of the entries, which a row of a join has. An entry at -1 is not in the rows.
	layout []int
	// table is the one table that the rows hold, whose indexes the WHERE clause can use, at the
	// entry of index entry, or nil when the rows are those of a join.
	table  *table
	entry  int
	params []value.Value
	// pruned is positive while binding a branch that can never be evaluated, such as the ELSE of a
	// CASE whose WHEN is known true. Such a branch is still type-checked, but an error in folding
	// it is not reported, as in Postgres.
	pruned int
	// phase of the statement being bound, which orders folding errors. See [binder.folding].
	phase int
	// foldErr is the folding error to report, from the lowest phase, and the first in it.
	foldErr   error
	foldPhase int
	// stableErr rejects an expression that [binder.fold] cannot fold because it is stable and
	// that can fail.
	stableErr error
	// now is the value of now() and its synonyms: the start of the transaction.
	now value.Value
	// agg is how a call of an aggregate binds, or nil where aggregates are not allowed, in the
	// clause that aggClause names.
	agg       *aggScope
	aggClause string
}

// Phases of a statement, in the order in which Postgres folds their constants: the output
// expressions, including those of ORDER BY and GROUP BY, then the conditions of the joins, then
// WHERE, then HAVING, then OFFSET, then LIMIT.
const (
	phaseOutput = iota
	phaseOn
	phaseWhere
	phaseHaving
	phaseOffset
	phaseLimit
)

// folding returns the error that folding constants raised, if any. Postgres checks the whole
// statement before it folds any constant, so a statement that is invalid, or mistyped anywhere,
// reports that before a folding error such as division by zero. The binder therefore records
// folding errors instead of returning them, and the statement reports one only once it has bound
// everything.
func (b *binder) folding() error {
	if b.foldErr != nil {
		return b.foldErr
	}
	return b.stableErr
}

// fold a node whose children are all constants by evaluating it now, which also reports errors
// such as division by zero at bind time, as Postgres does when it folds constants. The node's
// fallibility is its own or its children's.
func (b *binder) fold(e *cexpr, children ...*cexpr) (*cexpr, error) {
	for _, c := range children {
		e.fallible = e.fallible || c.fallible
		e.refs = e.refs || c.refs
		e.stable = e.stable || c.stable
		e.aggs = append(e.aggs, c.aggs...)
	}
	if e.refs {
		return e, nil
	}
	if e.stable {
		// Postgres evaluates a stable expression without columns when the statement runs, but
		// also while it plans, to estimate how many rows a condition matches, depending on where
		// the expression is. When it can fail, whether the statement fails then depends on the
		// plan.
		// It is reported after the statement is bound and folded, so that an error that
		// Postgres reports for certain comes first.
		if e.fallible && b.pruned == 0 && b.stableErr == nil {
			b.stableErr = fmt.Errorf("%w: a conversion between text and timestamptz that reads no columns and can fail is not supported, because whether Postgres evaluates it depends on its plan; write the timestamptz as a quoted literal, such as '2024-01-02 03:04:05+00'", ErrUnsupported)
		}
		return e, nil
	}
	v, err := e.eval(nil)
	if err != nil {
		if b.pruned == 0 && (b.foldErr == nil || b.phase < b.foldPhase) {
			b.foldErr, b.foldPhase = err, b.phase
		}
		// The expression stays unfolded, and binding goes on as if it had folded.
		return e, nil
	}
	return constant(v), nil
}

// names returns the scope and the namespace that names resolve in, which are those of table
// when the binder has none.
func (b *binder) names() (*scope, namespace) {
	if b.scope == nil && b.table != nil {
		b.scope, b.ns = singleScope(b.table, b.table.Name)
	}
	return b.scope, b.ns
}

// resolve a column reference in the binder's namespace.
func (b *binder) resolve(ref *ast.ColumnRef) (colRef, error) {
	sc, ns := b.names()
	if sc == nil {
		if ref.Table != "" {
			return colRef{}, fmt.Errorf("%w: missing FROM-clause entry for table %q", ErrNoTable, ref.Table)
		}
		return colRef{}, fmt.Errorf("%w: column %q does not exist", ErrInvalid, ref.Name)
	}
	return sc.resolve(ns, ref)
}

// offset of the columns of entry i in the rows the binder binds for.
func (b *binder) offset(i int) int {
	if b.layout != nil {
		return b.layout[i]
	}
	return b.scope.entries[i].offset
}

// column reads the column r from a row.
func (b *binder) column(r colRef) *cexpr {
	off := b.offset(r.entry)
	if off < 0 {
		panic(fmt.Sprintf("exec: binding a column of entry %d, which the rows do not hold", r.entry))
	}
	at := off + r.col
	ce := &cexpr{typ: b.scope.entries[r.entry].t.Columns[r.col].Type, refs: true, eval: func(row []value.Value) (value.Value, error) {
		return row[at], nil
	}}
	if r.cast != value.Unknown {
		ce = widen(ce, r.cast)
	}
	return ce
}

// forEntry is a binder for the rows of the entry i alone, whose table's indexes its WHERE clause
// can use, in the same namespace.
func (b *binder) forEntry(i int) *binder {
	nb := b.fresh()
	nb.table, nb.entry = b.scope.entries[i].t, i
	nb.layout = make([]int, len(b.scope.entries))
	for j := range nb.layout {
		nb.layout[j] = -1
	}
	nb.layout[i] = 0
	return nb
}

// bindPruned binds a branch that is known never to be evaluated.
func (b *binder) bindPruned(e ast.Expr) (*cexpr, error) {
	b.pruned++
	defer func() { b.pruned-- }()
	ce, err := b.bind(e)
	if ce != nil {
		ce.pruned = true
	}
	return ce, err
}

// castError maps a text input error of the value package to the statement error it is.
func castError(err error) error {
	switch {
	case errors.Is(err, value.ErrOutOfRange):
		return fmt.Errorf("%w: %v", ErrOutOfRange, err)
	case errors.Is(err, value.ErrUnsupportedInput):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	return fmt.Errorf("%w: %v", ErrCast, err)
}

// resolve gives a constant of unknown type, a NULL or a quoted string literal, the type its
// context demands, by reading the string with the type's input function as Postgres does. Other
// expressions are returned as they are.
func resolve(e *cexpr, t value.Type) (*cexpr, error) {
	if e.typ != value.Unknown || !e.folded {
		return e, nil
	}
	if e.val.Null {
		return constant(value.NewNull(t)), nil
	}
	if e.param > 0 {
		v, err := resolveParam(e.param, e.val, t)
		if err != nil {
			return nil, err
		}
		return constant(v), nil
	}
	if t == value.Unknown {
		return constant(value.NewString(e.val.S)), nil
	}
	if t == value.Decimal {
		return nil, fmt.Errorf("%w: a quoted literal of type numeric, the type of SUM and AVG of integers, is not supported; cast the literal, such as '5'::bigint", ErrUnsupported)
	}
	v, err := value.Parse(e.val.S, t)
	if err != nil {
		return nil, castError(err)
	}
	return constant(v), nil
}

// resolveParam converts a parameter of a Go type other than string to the type t that its
// context infers for it, the way a Postgres client library encodes the value for a parameter of
// that type: an int64 is a bigint, an integer if it fits, or a double precision, and every Go
// type is its own SQL type. Any other conversion, which client libraries make in different ways,
// is not supported.
func resolveParam(n int, v value.Value, t value.Type) (value.Value, error) {
	if t == value.Unknown {
		t = value.String
	}
	switch {
	case v.Type == t:
		return v, nil
	case v.Type == value.Int && t == value.Int4:
		if v.I < math.MinInt32 || v.I > math.MaxInt32 {
			return value.Value{}, fmt.Errorf("%w: parameter $%d is %d, which is out of range for type integer", ErrOutOfRange, n, v.I)
		}
		return value.NewInt4(int32(v.I)), nil
	case v.Type == value.Int && t == value.Float:
		return value.NewFloat(float64(v.I)), nil
	case v.Type == value.Int && t == value.Decimal:
		return value.DecimalFromInt(v.I), nil
	}
	// Postgres drivers differ in what they do with such a value: one refuses it, another sends
	// it as text that the server then reads as the type.
	return value.Value{}, fmt.Errorf("%w: parameter $%d is of type %v here, and a Go %s is not converted to it; pass a %s, or cast the parameter, such as $%d::%v", ErrUnsupported, n, t, goTypeName(v.Type), goTypeFor(t), n, v.Type)
}

// goTypeName is the Go type of a parameter of SQL type t.
func goTypeName(t value.Type) string {
	return map[value.Type]string{value.Int: "int64", value.Float: "float64", value.Bool: "bool", value.Bytes: "[]byte", value.Time: "time.Time"}[t]
}

// goTypeFor is the Go type to pass for a parameter of SQL type t.
func goTypeFor(t value.Type) string {
	if t == value.String {
		return "string"
	}
	if t == value.Int4 || t == value.Decimal {
		return "int64"
	}
	return goTypeName(t)
}

// numericTo converts a numeric literal to double precision, bigint, or text, which are the
// casts Postgres has for it among the supported types. Postgres converts with a function call
// that it folds like any other, so a conversion that fails is a folding error, which a pruned
// branch does not report.
func (b *binder) numericTo(e *cexpr, t value.Type) (*cexpr, error) {
	if e.pruned {
		b.pruned++
		defer func() { b.pruned-- }()
	}
	var conv func(s string) (value.Value, error)
	switch t {
	case value.Float:
		conv = func(s string) (value.Value, error) {
			f, err := value.NumericToFloat(s)
			return value.NewFloat(f), err
		}
	case value.Int:
		conv = func(s string) (value.Value, error) {
			i, err := value.NumericToInt(s)
			return value.NewInt(i), err
		}
	case value.String:
		text, err := value.NumericText(e.val.S)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
		}
		return constant(value.NewString(text)), nil
	default:
		return nil, fmt.Errorf("%w: the numeric type is not supported, cast the literal %s to double precision or bigint", ErrUnsupported, e.val.S)
	}
	return b.fold(&cexpr{typ: t, eval: func([]value.Value) (value.Value, error) {
		v, err := conv(e.val.S)
		if err != nil {
			return value.Value{}, castError(err)
		}
		return v, nil
	}}, e)
}

// widen an expression of type integer to bigint, or an integer or bigint to double precision,
// which are the implicit casts Postgres applies between the numeric types. A constant is
// converted now; an expression that reads columns gets a conversion step, which Postgres prices
// as a function call.
func widen(e *cexpr, t value.Type) *cexpr {
	if e.typ == t {
		return e
	}
	conv := func(v value.Value) value.Value {
		switch {
		case v.Null:
			return value.NewNull(t)
		case t == value.Int:
			return value.NewInt(v.I)
		case v.Type == value.Float:
			return v
		}
		return value.NewFloat(float64(v.I))
	}
	if e.folded {
		return constant(conv(e.val))
	}
	return &cexpr{typ: t, refs: e.refs, fallible: e.fallible, stable: e.stable, aggs: e.aggs, eval: func(row []value.Value) (value.Value, error) {
		v, err := e.value(row)
		if err != nil {
			return value.Value{}, err
		}
		return conv(v), nil
	}}
}

// toDecimal converts an integer or bigint to numeric, which is an implicit cast in Postgres.
func toDecimal(e *cexpr) *cexpr {
	conv := func(v value.Value) value.Value {
		if v.Null {
			return value.NewNull(value.Decimal)
		}
		return value.DecimalFromInt(v.I)
	}
	if e.folded {
		return constant(conv(e.val))
	}
	return &cexpr{typ: value.Decimal, refs: e.refs, fallible: e.fallible, stable: e.stable, aggs: e.aggs, eval: func(row []value.Value) (value.Value, error) {
		v, err := e.value(row)
		if err != nil {
			return value.Value{}, err
		}
		return conv(v), nil
	}}
}

// decimalTo converts a numeric to double precision, bigint, or text, which are the casts that
// Postgres has from numeric to the supported types: to double precision through its text, which
// rounds to the nearest double, and to bigint rounded half away from zero, which can fail.
func (b *binder) decimalTo(e *cexpr, t value.Type) (*cexpr, error) {
	var conv func(v value.Value) (value.Value, error)
	switch t {
	case value.Decimal:
		return e, nil
	case value.Float:
		conv = func(v value.Value) (value.Value, error) {
			f, err := value.DecimalToFloat(v.S)
			return value.NewFloat(f), err
		}
	case value.Int:
		conv = func(v value.Value) (value.Value, error) {
			i, err := value.DecimalToInt(v.S)
			return value.NewInt(i), err
		}
	case value.String:
		conv = func(v value.Value) (value.Value, error) { return value.NewString(v.S), nil }
	default:
		return nil, fmt.Errorf("%w: cannot cast type numeric to %v", ErrCast, t)
	}
	if e.folded && e.val.Null {
		return constant(value.NewNull(t)), nil
	}
	return b.fold(&cexpr{typ: t, fallible: t == value.Int, eval: func(row []value.Value) (value.Value, error) {
		v, err := e.value(row)
		if err != nil || v.Null {
			return value.NewNull(t), err
		}
		out, err := conv(v)
		if err != nil {
			return value.Value{}, castError(err)
		}
		return out, nil
	}}, e)
}

// errDecimalArithmetic is the error of arithmetic on numeric, which is not supported.
func errDecimalArithmetic() error {
	return fmt.Errorf("%w: arithmetic on numeric, the type of SUM and AVG of integers, is not supported; cast it to bigint or double precision first, such as SUM(x)::bigint", ErrUnsupported)
}

// numeric reports whether t is one of the numeric types, by Postgres's categories.
func numeric(t value.Type) bool {
	return t == value.Int4 || t == value.Int || t == value.Float || t == value.Numeric || t == value.Decimal
}

// errNoCommonType is returned by [commonType] when two types do not unify.
var errNoCommonType = errors.New("no common type")

// commonType of two operand types under Postgres's rules for its binary operators: an unknown
// takes the other type, integer widens to bigint, both widen to double precision, and a numeric
// literal is double precision next to one. Two unknowns are text. Anything else does not unify,
// which is [errNoCommonType].
func commonType(a, c value.Type) (value.Type, error) {
	switch {
	case a == c && a != value.Unknown && a != value.Numeric:
		return a, nil
	case a == value.Unknown && c == value.Unknown:
		return value.String, nil
	case a == value.Unknown && c == value.Numeric, c == value.Unknown && a == value.Numeric, a == value.Numeric && c == value.Numeric:
		return 0, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
	case a == value.Unknown:
		return c, nil
	case c == value.Unknown:
		return a, nil
	case numeric(a) && numeric(c):
		if a == value.Float || c == value.Float {
			return value.Float, nil
		}
		if a == value.Numeric || c == value.Numeric {
			return 0, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
		}
		if a == value.Decimal || c == value.Decimal {
			return value.Decimal, nil
		}
		return value.Int, nil
	}
	return 0, errNoCommonType
}

// coerce an expression to a type that [commonType] chose for it, with the implicit casts
// Postgres has between the supported types.
func (b *binder) coerce(e *cexpr, t value.Type) (*cexpr, error) {
	switch {
	case e.typ == t:
		return e, nil
	case e.typ == value.Unknown:
		return resolve(e, t)
	case e.typ == value.Numeric:
		return b.numericTo(e, t)
	case e.typ == value.Int4 && t == value.Int, (e.typ == value.Int4 || e.typ == value.Int) && t == value.Float:
		return widen(e, t), nil
	case (e.typ == value.Int4 || e.typ == value.Int) && t == value.Decimal:
		return toDecimal(e), nil
	case e.typ == value.Decimal && t == value.Float:
		return b.decimalTo(e, t)
	}
	return nil, errNoCommonType
}

// unify two operands of the operator sym to one type.
func (b *binder) unify(a, c *cexpr, sym string) (*cexpr, *cexpr, error) {
	t, err := commonType(a.typ, c.typ)
	if errors.Is(err, errNoCommonType) {
		return nil, nil, fmt.Errorf("%w: operator does not exist: %v %s %v", ErrType, a.typ, sym, c.typ)
	}
	if err != nil {
		return nil, nil, err
	}
	if a, err = b.coerce(a, t); err != nil {
		return nil, nil, err
	}
	if c, err = b.coerce(c, t); err != nil {
		return nil, nil, err
	}
	return a, c, nil
}

// unifyAll unifies every expression to one common type, for IN lists, CASE results and COALESCE,
// as Postgres resolves the type of a UNION: the first known type is the candidate, a later type
// that the candidate converts to implicitly replaces it, and unknowns take the result. All
// unknowns make text.
func (b *binder) unifyAll(es []*cexpr, what string) ([]*cexpr, value.Type, error) {
	t := value.Unknown
	for _, e := range es {
		switch {
		case e.typ == value.Unknown:
		case t == value.Unknown:
			t = e.typ
		case t == e.typ:
		case numeric(t) && numeric(e.typ):
			if e.typ == value.Float {
				t = value.Float
			} else if e.typ == value.Int && t == value.Int4 {
				t = value.Int
			} else if e.typ == value.Numeric && t != value.Float {
				return nil, 0, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
			} else if e.typ == value.Decimal && t != value.Float {
				t = value.Decimal
			}
		default:
			return nil, 0, fmt.Errorf("%w: %s %v and %v", ErrType, what, t, e.typ)
		}
	}
	if t == value.Numeric {
		return nil, 0, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
	}
	if t == value.Unknown {
		t = value.String
	}
	out := make([]*cexpr, len(es))
	for i, e := range es {
		var err error
		if out[i], err = b.coerce(e, t); err != nil {
			return nil, 0, err
		}
	}
	return out, t, nil
}

func (b *binder) bind(e ast.Expr) (*cexpr, error) {
	if b.agg != nil && b.agg.g != nil && !b.agg.nested {
		if ce, ok, err := b.groupedExpr(e); ok || err != nil {
			return ce, err
		}
	}
	switch e := e.(type) {
	case *ast.Literal:
		return constant(e.Value), nil
	case *ast.Param:
		if e.Index > len(b.params) {
			return nil, fmt.Errorf("%w: there is no parameter $%d, %d given", ErrInvalid, e.Index, len(b.params))
		}
		// A parameter has no type of its own, as in Postgres, which infers it from the context
		// like the type of a quoted literal. A Go string or nil is exactly such a literal; a
		// value of another Go type converts to the inferred type when it can.
		v := b.params[e.Index-1]
		if v.Type == value.Unknown {
			return constant(v), nil
		}
		return &cexpr{typ: value.Unknown, folded: true, val: v, param: e.Index}, nil
	case *ast.ColumnRef:
		r, err := b.resolve(e)
		if err != nil {
			return nil, err
		}
		return b.column(r), nil
	case *ast.Unary:
		return b.unary(e)
	case *ast.Binary:
		return b.binary(e)
	case *ast.IsNull:
		x, err := b.bind(e.Expr)
		if err != nil {
			return nil, err
		}
		if x, err = resolve(x, value.String); err != nil {
			return nil, err
		}
		if x.typ == value.Numeric {
			return constant(value.NewBool(e.Not)), nil
		}
		not := e.Not
		return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil {
				return value.Value{}, err
			}
			return value.NewBool(v.Null != not), nil
		}}, x)
	case *ast.Between:
		d, _ := desugar(e)
		return b.bind(d)
	case *ast.In:
		if mixedIn(e) && !b.commonInType(e) {
			// Without a common type of the elements that read no columns, Postgres compares every
			// element in turn, in the order they are written.
			return b.bind(inChain(e, e.List))
		}
		if d, ok := desugar(e); ok {
			return b.bind(d)
		}
		return b.in(e)
	case *ast.Like:
		return b.like(e)
	case *ast.Case:
		return b.caseExpr(e)
	case *ast.Call:
		return b.call(e)
	case *ast.Cast:
		x, err := b.bind(e.Expr)
		if err != nil {
			return nil, err
		}
		return b.cast(x, e.Type)
	case *ast.Default:
		return nil, fmt.Errorf("%w: DEFAULT is not allowed in this context", ErrSyntax)
	}
	return nil, fmt.Errorf("exec: unsupported expression %T", e)
}

func (b *binder) unary(e *ast.Unary) (*cexpr, error) {
	x, err := b.bind(e.Expr)
	if err != nil {
		return nil, err
	}
	switch e.Op {
	case ast.Not:
		if x, err = resolve(x, value.Bool); err != nil {
			return nil, err
		}
		if x.typ != value.Bool {
			return nil, fmt.Errorf("%w: argument of NOT must be type boolean, not type %v", ErrType, x.typ)
		}
		return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.Bool), err
			}
			return value.NewBool(!v.B), nil
		}}, x)
	case ast.Neg, ast.Pos:
		sym := "-"
		if e.Op == ast.Pos {
			sym = "+"
		}
		switch x.typ {
		case value.Unknown:
			// Postgres has unary minus and plus for several types, so an unknown operand is
			// ambiguous.
			return nil, fmt.Errorf("%w: operator is not unique: %s unknown", ErrType, sym)
		case value.Numeric:
			return nil, fmt.Errorf("%w: the numeric type is not supported, write the sign as part of the literal or cast the literal to double precision or bigint", ErrUnsupported)
		case value.Decimal:
			return nil, errDecimalArithmetic()
		case value.Int4, value.Int, value.Float:
		default:
			return nil, fmt.Errorf("%w: operator does not exist: %s %v", ErrType, sym, x.typ)
		}
		if e.Op == ast.Pos {
			return b.fold(&cexpr{typ: x.typ, eval: x.value}, x)
		}
		t := x.typ
		return b.fold(&cexpr{typ: t, fallible: t != value.Float, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return v, err
			}
			switch t {
			case value.Float:
				return value.NewFloat(-v.F), nil
			case value.Int4:
				if v.I == math.MinInt32 {
					return value.Value{}, fmt.Errorf("%w: integer out of range", ErrOutOfRange)
				}
				return value.NewInt4(int32(-v.I)), nil
			}
			if v.I == math.MinInt64 {
				return value.Value{}, fmt.Errorf("%w: bigint out of range", ErrOutOfRange)
			}
			return value.NewInt(-v.I), nil
		}}, x)
	}
	return nil, fmt.Errorf("exec: unsupported unary operator %d", e.Op)
}

func (b *binder) binary(e *ast.Binary) (*cexpr, error) {
	switch e.Op {
	case ast.And, ast.Or:
		return b.logical(e)
	}
	l, err := b.bind(e.Left)
	if err != nil {
		return nil, err
	}
	r, err := b.bind(e.Right)
	if err != nil {
		return nil, err
	}
	if e.Op == ast.Concat {
		return b.concat(l, r)
	}
	sym := operatorSymbols[e.Op]
	l, r, err = b.unify(l, r, sym)
	if err != nil {
		return nil, err
	}
	t := l.typ
	var op func(a, c value.Value) (value.Value, error)
	var resultType value.Type
	fallible := false
	switch e.Op {
	case ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Mod:
		if !numeric(t) {
			return nil, fmt.Errorf("%w: operator does not exist: %v %s %v", ErrType, t, sym, t)
		}
		if e.Op == ast.Mod && t == value.Float {
			return nil, fmt.Errorf("%w: operator does not exist: double precision %% double precision", ErrType)
		}
		if t == value.Decimal {
			return nil, errDecimalArithmetic()
		}
		resultType = t
		op = arithmetic(e.Op, t)
		fallible = true
	default:
		resultType = value.Bool
		op = comparison(e.Op)
	}
	if (l.folded && l.val.Null) || (r.folded && r.val.Null) {
		// Operators are strict, so a known NULL operand makes the result NULL without evaluating
		// the other operand, as Postgres folds it.
		return constant(value.NewNull(resultType)), nil
	}
	return b.fold(&cexpr{typ: resultType, fallible: fallible, eval: func(row []value.Value) (value.Value, error) {
		a, err := l.value(row)
		if err != nil {
			return value.Value{}, err
		}
		c, err := r.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if a.Null || c.Null {
			return value.NewNull(resultType), nil
		}
		return op(a, c)
	}}, l, r)
}

// concat binds ||, which Postgres defines for text with text, bytea with bytea, and text with
// any other type, whose value is written as text.
func (b *binder) concat(l, r *cexpr) (*cexpr, error) {
	lt, rt := l.typ, r.typ
	var t value.Type
	var err error
	switch {
	case lt == value.Bytes && rt == value.Bytes, lt == value.Bytes && rt == value.Unknown, lt == value.Unknown && rt == value.Bytes:
		t = value.Bytes
	case lt == value.String || rt == value.String, lt == value.Unknown && rt == value.Unknown, lt == value.Unknown, rt == value.Unknown:
		t = value.String
	default:
		return nil, fmt.Errorf("%w: operator does not exist: %v || %v", ErrType, lt, rt)
	}
	text := func(e *cexpr) (*cexpr, error) {
		switch e.typ {
		case value.Unknown:
			return resolve(e, t)
		case t:
			return e, nil
		case value.Numeric:
			return b.numericTo(e, value.String)
		}
		// Written as text, which Postgres does through the type's output function, in its
		// operators anytextcat and textanycat. Postgres inlines them into a cast to text, which
		// is immutable except from timestamptz.
		if e.folded && e.val.Null {
			return constant(value.NewNull(value.String)), nil
		}
		if e.folded && e.typ != value.Time {
			return constant(value.NewString(value.Format(e.val))), nil
		}
		return &cexpr{typ: value.String, refs: e.refs, fallible: e.fallible, stable: e.stable || e.typ == value.Time, aggs: e.aggs, eval: func(row []value.Value) (value.Value, error) {
			v, err := e.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.String), err
			}
			return value.NewString(value.Format(v)), nil
		}}, nil
	}
	if l, err = text(l); err != nil {
		return nil, err
	}
	if r, err = text(r); err != nil {
		return nil, err
	}
	if (l.folded && l.val.Null) || (r.folded && r.val.Null) {
		return constant(value.NewNull(t)), nil
	}
	return b.fold(&cexpr{typ: t, eval: func(row []value.Value) (value.Value, error) {
		a, err := l.value(row)
		if err != nil {
			return value.Value{}, err
		}
		c, err := r.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if a.Null || c.Null {
			return value.NewNull(t), nil
		}
		if t == value.Bytes {
			return value.NewBytes(append(append([]byte{}, a.Raw...), c.Raw...)), nil
		}
		return value.NewString(a.S + c.S), nil
	}}, l, r)
}

func (b *binder) logical(e *ast.Binary) (*cexpr, error) {
	l, err := b.bind(e.Left)
	if err != nil {
		return nil, err
	}
	if l, err = resolve(l, value.Bool); err != nil {
		return nil, err
	}
	if l.typ != value.Bool {
		return nil, fmt.Errorf("%w: argument of %s must be type boolean, not type %v", ErrType, logicalName(e.Op), l.typ)
	}
	// Postgres folds AND and OR left to right and stops at a constant that decides the result, so
	// the right side is only type-checked, and an error in folding it is not reported.
	and := e.Op == ast.And
	decided := l.folded && !l.val.Null && l.val.B != and
	var r *cexpr
	if decided {
		r, err = b.bindPruned(e.Right)
	} else {
		r, err = b.bind(e.Right)
	}
	if err != nil {
		return nil, err
	}
	if r, err = resolve(r, value.Bool); err != nil {
		return nil, err
	}
	if r.typ != value.Bool {
		return nil, fmt.Errorf("%w: argument of %s must be type boolean, not type %v", ErrType, logicalName(e.Op), r.typ)
	}
	// A constant that decides the result also decides it on the right, after a left side that is
	// not constant, because Postgres drops the other arguments of an AND with a FALSE constant
	// or an OR with a TRUE one wherever it is.
	if decided || r.folded && !r.val.Null && r.val.B != and {
		return constant(value.NewBool(!and)), nil
	}
	return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
		a, err := l.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if !a.Null && a.B != and {
			return value.NewBool(!and), nil
		}
		c, err := r.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if !c.Null && c.B != and {
			return value.NewBool(!and), nil
		}
		if a.Null || c.Null {
			return value.NewNull(value.Bool), nil
		}
		return value.NewBool(and), nil
	}}, l, r)
}

var operatorSymbols = map[ast.BinaryOp]string{
	ast.Add: "+", ast.Sub: "-", ast.Mul: "*", ast.Div: "/", ast.Mod: "%", ast.Concat: "||",
	ast.Eq: "=", ast.Ne: "<>", ast.Lt: "<", ast.Le: "<=", ast.Gt: ">", ast.Ge: ">=",
}

func logicalName(op ast.BinaryOp) string {
	if op == ast.And {
		return "AND"
	}
	return "OR"
}

func comparison(op ast.BinaryOp) func(a, b value.Value) (value.Value, error) {
	return func(a, b value.Value) (value.Value, error) {
		c := value.Compare(a, b)
		var ok bool
		switch op {
		case ast.Eq:
			ok = c == 0
		case ast.Ne:
			ok = c != 0
		case ast.Lt:
			ok = c < 0
		case ast.Le:
			ok = c <= 0
		case ast.Gt:
			ok = c > 0
		case ast.Ge:
			ok = c >= 0
		}
		return value.NewBool(ok), nil
	}
}

// arithmetic on two non-null values of one numeric type, with the checks Postgres makes: integer
// and bigint overflow and division by zero are errors, and float overflow to infinity and
// underflow to zero are errors unless an operand already was infinite or zero.
func arithmetic(op ast.BinaryOp, t value.Type) func(a, b value.Value) (value.Value, error) {
	return func(a, b value.Value) (value.Value, error) {
		if t == value.Float {
			return floatArithmetic(op, a.F, b.F)
		}
		x, y := a.I, b.I
		var r int64
		switch op {
		case ast.Add:
			r = x + y
			if (r > x) != (y > 0) {
				return value.Value{}, fmt.Errorf("%w: %v out of range", ErrOutOfRange, t)
			}
		case ast.Sub:
			r = x - y
			if (r < x) != (y > 0) {
				return value.Value{}, fmt.Errorf("%w: %v out of range", ErrOutOfRange, t)
			}
		case ast.Mul:
			r = x * y
			if x != 0 && (r/x != y || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64)) {
				return value.Value{}, fmt.Errorf("%w: %v out of range", ErrOutOfRange, t)
			}
		case ast.Div:
			if y == 0 {
				return value.Value{}, ErrDivisionByZero
			}
			if x == math.MinInt64 && y == -1 {
				return value.Value{}, fmt.Errorf("%w: %v out of range", ErrOutOfRange, t)
			}
			r = x / y
		case ast.Mod:
			if y == 0 {
				return value.Value{}, ErrDivisionByZero
			}
			if y == -1 {
				return value.Value{Type: t, I: 0}, nil
			}
			r = x % y
		}
		if t == value.Int4 && (r < math.MinInt32 || r > math.MaxInt32) {
			return value.Value{}, fmt.Errorf("%w: integer out of range", ErrOutOfRange)
		}
		return value.Value{Type: t, I: r}, nil
	}
}

func floatArithmetic(op ast.BinaryOp, x, y float64) (value.Value, error) {
	var r float64
	switch op {
	case ast.Add:
		r = x + y
	case ast.Sub:
		r = x - y
	case ast.Mul:
		r = x * y
		if r == 0 && x != 0 && y != 0 {
			return value.Value{}, fmt.Errorf("%w: value out of range: underflow", ErrOutOfRange)
		}
	case ast.Div:
		if y == 0 && !math.IsNaN(x) {
			return value.Value{}, ErrDivisionByZero
		}
		r = x / y
		if math.IsInf(r, 0) && !math.IsInf(x, 0) {
			return value.Value{}, fmt.Errorf("%w: value out of range: overflow", ErrOutOfRange)
		}
		if r == 0 && x != 0 && !math.IsInf(y, 0) {
			return value.Value{}, fmt.Errorf("%w: value out of range: underflow", ErrOutOfRange)
		}
		return value.NewFloat(r), nil
	}
	if math.IsInf(r, 0) && !math.IsInf(x, 0) && !math.IsInf(y, 0) {
		return value.Value{}, fmt.Errorf("%w: value out of range: overflow", ErrOutOfRange)
	}
	return value.NewFloat(r), nil
}

// desugar rewrites BETWEEN and IN as the comparisons that Postgres rewrites them to before it
// folds constants, so that the same folding and pruning rules apply: x BETWEEN a AND b is
// x >= a AND x <= b, NOT BETWEEN is x < a OR x > b, x IN (a, b) is x = a OR x = b, and NOT IN is
// x <> a AND x <> b. When several elements read no columns, they stay together as one IN, which
// is an array comparison in Postgres, and come first, followed by the other elements in order.
// An IN of only such elements, and at least two of them, is not rewritten.
func desugar(e ast.Expr) (ast.Expr, bool) {
	switch e := e.(type) {
	case *ast.Between:
		if e.Not {
			return &ast.Binary{Op: ast.Or, Left: &ast.Binary{Op: ast.Lt, Left: e.Expr, Right: e.Low}, Right: &ast.Binary{Op: ast.Gt, Left: e.Expr, Right: e.High}}, true
		}
		return &ast.Binary{Op: ast.And, Left: &ast.Binary{Op: ast.Ge, Left: e.Expr, Right: e.Low}, Right: &ast.Binary{Op: ast.Le, Left: e.Expr, Right: e.High}}, true
	case *ast.In:
		var constants, columns []ast.Expr
		for _, item := range e.List {
			if readsColumns(item) {
				columns = append(columns, item)
			} else {
				constants = append(constants, item)
			}
		}
		items := e.List
		var out ast.Expr
		if len(constants) > 1 {
			if len(columns) == 0 {
				return nil, false
			}
			out = &ast.In{Expr: e.Expr, List: constants, Not: e.Not}
			items = columns
		}
		cmp, join := ast.Eq, ast.Or
		if e.Not {
			cmp, join = ast.Ne, ast.And
		}
		for _, item := range items {
			c := &ast.Binary{Op: cmp, Left: e.Expr, Right: item}
			if out == nil {
				out = c
			} else {
				out = &ast.Binary{Op: join, Left: out, Right: c}
			}
		}
		return out, true
	}
	return nil, false
}

// mixedIn reports whether an IN has several elements that read no columns and some that do,
// which [desugar] splits into an array comparison and comparisons.
func mixedIn(e *ast.In) bool {
	constants := 0
	for _, item := range e.List {
		if !readsColumns(item) {
			constants++
		}
	}
	return constants > 1 && constants < len(e.List)
}

// commonInType reports whether the left side and the elements that read no columns of an IN
// have a common type, so that Postgres compares them as an array. It reports true when binding
// them fails otherwise, so that binding the IN reports the error.
func (b *binder) commonInType(e *ast.In) bool {
	x, err := b.bindPruned(e.Expr)
	if err != nil {
		return true
	}
	es := []*cexpr{x}
	for _, item := range e.List {
		if readsColumns(item) {
			continue
		}
		c, err := b.bindPruned(item)
		if err != nil {
			return true
		}
		es = append(es, c)
	}
	_, _, err = b.unifyAll(es, "IN mixes")
	return !errors.Is(err, ErrType)
}

// inChain is the comparisons of an IN with the items, joined with OR, or for NOT IN the
// negated comparisons joined with AND.
func inChain(e *ast.In, items []ast.Expr) ast.Expr {
	cmp, join := ast.Eq, ast.Or
	if e.Not {
		cmp, join = ast.Ne, ast.And
	}
	var chain ast.Expr
	for _, item := range items {
		var c ast.Expr = &ast.Binary{Op: cmp, Left: e.Expr, Right: item}
		if chain != nil {
			c = &ast.Binary{Op: join, Left: chain, Right: c}
		}
		chain = c
	}
	return chain
}

// readsColumns reports whether e references a column.
func readsColumns(e ast.Expr) bool {
	found := false
	ast.Walk(e, func(x ast.Expr) bool {
		if _, ok := x.(*ast.ColumnRef); ok {
			found = true
		}
		return !found
	})
	return found
}

// in binds an IN whose elements are all constants, which is an array comparison in Postgres.
func (b *binder) in(e *ast.In) (*cexpr, error) {
	x, err := b.bind(e.Expr)
	if err != nil {
		return nil, err
	}
	es := []*cexpr{x}
	for _, item := range e.List {
		c, err := b.bind(item)
		if err != nil {
			return nil, err
		}
		es = append(es, c)
	}
	es, _, err = b.unifyAll(es, "IN mixes")
	if errors.Is(err, ErrType) {
		// Without a common type there is no array, and Postgres compares the elements one at a
		// time instead, each with its own types.
		return b.bind(inChain(e, e.List))
	}
	if err != nil {
		return nil, err
	}
	x, list := es[0], es[1:]
	if x.folded && x.val.Null {
		// The array comparison is strict, so a known NULL makes it NULL without evaluating the
		// elements.
		return constant(value.NewNull(value.Bool)), nil
	}
	not := e.Not
	// Postgres prices the comparison with an array of n elements at n/2 operator calls. An
	// element that is not folded is a constant whose folding failed in a pruned branch; it is
	// evaluated like a column.
	return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil {
			return value.Value{}, err
		}
		sawNull := false
		for _, c := range list {
			item, err := c.value(row)
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
	}}, es...)
}

func (b *binder) like(e *ast.Like) (*cexpr, error) {
	x, err := b.bind(e.Expr)
	if err != nil {
		return nil, err
	}
	p, err := b.bind(e.Pattern)
	if err != nil {
		return nil, err
	}
	if x.typ == value.Bytes || p.typ == value.Bytes {
		return nil, fmt.Errorf("%w: LIKE on bytea is not supported, compare text", ErrUnsupported)
	}
	if x, err = resolve(x, value.String); err != nil {
		return nil, err
	}
	if p, err = resolve(p, value.String); err != nil {
		return nil, err
	}
	if x.typ != value.String || p.typ != value.String {
		return nil, fmt.Errorf("%w: operator does not exist: %v ~~ %v", ErrType, x.typ, p.typ)
	}
	if (x.folded && x.val.Null) || (p.folded && p.val.Null) {
		return constant(value.NewNull(value.Bool)), nil
	}
	not := e.Not
	// A constant pattern can only fail when it ends in its escape character, and then only on a
	// row whose value is long enough for the matching to reach it.
	return b.fold(&cexpr{typ: value.Bool, fallible: !p.folded || endsInEscape(p.val.S), eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil {
			return value.Value{}, err
		}
		pat, err := p.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if v.Null || pat.Null {
			return value.NewNull(value.Bool), nil
		}
		ok, err := like(v.S, pat.S)
		if err != nil {
			return value.Value{}, err
		}
		return value.NewBool(ok != not), nil
	}}, x, p)
}

// endsInEscape reports whether a LIKE pattern ends in an unescaped backslash.
func endsInEscape(pattern string) bool {
	escaped := false
	for _, r := range pattern {
		if escaped {
			escaped = false
			continue
		}
		escaped = r == '\\'
	}
	return escaped
}

func (b *binder) caseExpr(e *ast.Case) (*cexpr, error) {
	var operand *cexpr
	var err error
	if e.Operand != nil {
		if operand, err = b.bind(e.Operand); err != nil {
			return nil, err
		}
		// An operand of unknown type is text, as in Postgres, before it meets the WHEN values.
		if operand, err = resolve(operand, value.String); err != nil {
			return nil, err
		}
	}
	type when struct {
		// operand as unified with cond, which may have adapted a constant operand's type.
		operand, cond, result *cexpr
	}
	var whens []when
	var results []*cexpr
	var all []*cexpr
	if operand != nil {
		all = append(all, operand)
	}
	// A WHEN that is known false is dropped, and a WHEN that is known true ends the CASE, so
	// later results are never bound and folded, as in Postgres.
	decided := false
	for _, w := range e.Whens {
		if decided {
			// Everything after a WHEN known true is only type-checked.
			cond, err := b.bindPruned(w.Cond)
			if err != nil {
				return nil, err
			}
			if operand != nil {
				if _, _, err = b.unify(operand, cond, "="); err != nil {
					return nil, err
				}
			} else if cond, err = resolve(cond, value.Bool); err != nil {
				return nil, err
			} else if cond.typ != value.Bool {
				return nil, fmt.Errorf("%w: argument of CASE/WHEN must be type boolean, not type %v", ErrType, cond.typ)
			}
			result, err := b.bindPruned(w.Result)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
			continue
		}
		cond, err := b.bind(w.Cond)
		if err != nil {
			return nil, err
		}
		var known, matched bool
		o := operand
		if operand != nil {
			if o, cond, err = b.unify(operand, cond, "="); err != nil {
				return nil, err
			}
			// A NULL operand or value never matches, and the comparison is strict, so the WHEN
			// is known not to match when either is a known NULL.
			if (o.folded && cond.folded) || (o.folded && o.val.Null) || (cond.folded && cond.val.Null) {
				known = true
				matched = o.folded && cond.folded && !o.val.Null && !cond.val.Null && value.Compare(o.val, cond.val) == 0
			}
		} else {
			if cond, err = resolve(cond, value.Bool); err != nil {
				return nil, err
			}
			if cond.typ != value.Bool {
				return nil, fmt.Errorf("%w: argument of CASE/WHEN must be type boolean, not type %v", ErrType, cond.typ)
			}
			if cond.folded {
				known = true
				matched = !cond.val.Null && cond.val.B
			}
		}
		if known && !matched {
			// The branch is only type-checked.
			result, err := b.bindPruned(w.Result)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
			continue
		}
		result, err := b.bind(w.Result)
		if err != nil {
			return nil, err
		}
		whens = append(whens, when{o, cond, result})
		results = append(results, result)
		all = append(all, cond, result)
		if known {
			decided = true
		}
	}
	var elseExpr *cexpr
	if e.Else != nil {
		if decided {
			elseExpr, err = b.bindPruned(e.Else)
		} else {
			elseExpr, err = b.bind(e.Else)
		}
		if err != nil {
			return nil, err
		}
		results = append(results, elseExpr)
		if !decided {
			all = append(all, elseExpr)
		}
	}
	if len(results) == 0 {
		return constant(value.NewNull(value.Unknown)), nil
	}
	resultsBefore := results
	results, t, err := b.unifyAll(results, "CASE types")
	if err != nil {
		return nil, err
	}
	// The unified results are matched back to the live WHENs by identity, because pruned
	// results are in the list too.
	unified := map[*cexpr]*cexpr{}
	for i, r := range results {
		unified[resultsBefore[i]] = r
	}
	for i := range whens {
		whens[i].result = unified[whens[i].result]
	}
	if elseExpr != nil {
		if decided {
			elseExpr = nil
		} else {
			elseExpr = unified[elseExpr]
		}
	}
	if len(whens) == 0 {
		// Every WHEN is known not to match, so the CASE is its ELSE, or a NULL constant.
		if elseExpr != nil {
			return elseExpr, nil
		}
		return constant(value.NewNull(t)), nil
	}
	return b.fold(&cexpr{typ: t, eval: func(row []value.Value) (value.Value, error) {
		for _, w := range whens {
			c, err := w.cond.value(row)
			if err != nil {
				return value.Value{}, err
			}
			var matched bool
			if w.operand != nil {
				o, err := w.operand.value(row)
				if err != nil {
					return value.Value{}, err
				}
				matched = !o.Null && !c.Null && value.Compare(o, c) == 0
			} else {
				matched = !c.Null && c.B
			}
			if matched {
				return w.result.value(row)
			}
		}
		if elseExpr != nil {
			return elseExpr.value(row)
		}
		return value.NewNull(t), nil
	}}, all...)
}

func (b *binder) call(e *ast.Call) (*cexpr, error) {
	if ast.Aggregates[e.Name] {
		return b.aggregate(e)
	}
	if e.Distinct {
		// The arguments are analyzed first, as in Postgres.
		for _, a := range e.Args {
			if _, err := b.bind(a); err != nil {
				return nil, err
			}
		}
		return nil, fmt.Errorf("%w: DISTINCT specified, but %s is not an aggregate function", ErrWrongObjectType, e.Name)
	}
	if e.Star {
		return nil, fmt.Errorf("%w: function %s() does not exist", ErrType, e.Name)
	}
	if e.Name == "nullif" {
		return b.nullif(e)
	}
	var args []*cexpr
	if e.Name != "coalesce" {
		args = make([]*cexpr, len(e.Args))
		for i, a := range e.Args {
			var err error
			if args[i], err = b.bind(a); err != nil {
				return nil, err
			}
		}
	}
	arity := func(n int) error {
		if len(args) != n {
			var types []string
			for _, a := range args {
				types = append(types, a.typ.String())
			}
			return fmt.Errorf("%w: function %s(%s) does not exist", ErrInvalid, e.Name, strings.Join(types, ", "))
		}
		return nil
	}
	switch e.Name {
	case "now", "transaction_timestamp", "current_timestamp":
		if err := arity(0); err != nil {
			return nil, err
		}
		// The start of the transaction, which Postgres marks stable, so that it is evaluated when
		// the statement runs, and never folded.
		now := b.now
		return &cexpr{typ: value.Time, stable: true, eval: func([]value.Value) (value.Value, error) {
			return now, nil
		}}, nil
	case "coalesce":
		if len(e.Args) == 0 {
			return nil, fmt.Errorf("%w: COALESCE needs at least one argument", ErrSyntax)
		}
		// Known NULLs are dropped, and a known value ends the list, so later arguments are only
		// type-checked, as in Postgres.
		var kept, all []*cexpr
		decided := false
		for _, arg := range e.Args {
			var a *cexpr
			var err error
			if decided {
				a, err = b.bindPruned(arg)
			} else {
				a, err = b.bind(arg)
			}
			if err != nil {
				return nil, err
			}
			all = append(all, a)
			if decided || (a.folded && a.val.Null) {
				continue
			}
			kept = append(kept, a)
			if a.folded {
				decided = true
			}
		}
		unifiedAll, t, err := b.unifyAll(all, "COALESCE types")
		if err != nil {
			return nil, err
		}
		if len(kept) == 0 {
			return constant(value.NewNull(t)), nil
		}
		unified := map[*cexpr]*cexpr{}
		for i, a := range all {
			unified[a] = unifiedAll[i]
		}
		for i := range kept {
			kept[i] = unified[kept[i]]
		}
		return b.fold(&cexpr{typ: t, eval: func(row []value.Value) (value.Value, error) {
			for _, a := range kept {
				v, err := a.value(row)
				if err != nil || !v.Null {
					return v, err
				}
			}
			return value.NewNull(t), nil
		}}, kept...)
	case "length":
		if err := arity(1); err != nil {
			return nil, err
		}
		x, err := resolve(args[0], value.String)
		if err != nil {
			return nil, err
		}
		if x.typ != value.String && x.typ != value.Bytes {
			return nil, fmt.Errorf("%w: function length(%v) does not exist", ErrType, x.typ)
		}
		return b.fold(&cexpr{typ: value.Int4, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.Int4), err
			}
			if v.Type == value.Bytes {
				return value.NewInt4(int32(len(v.Raw))), nil
			}
			return value.NewInt4(int32(utf8.RuneCountInString(v.S))), nil
		}}, x)
	case "lower", "upper":
		if err := arity(1); err != nil {
			return nil, err
		}
		x, err := resolve(args[0], value.String)
		if err != nil {
			return nil, err
		}
		if x.typ != value.String {
			return nil, fmt.Errorf("%w: function %s(%v) does not exist", ErrType, e.Name, x.typ)
		}
		f := asciiLower
		if e.Name == "upper" {
			f = asciiUpper
		}
		return b.fold(&cexpr{typ: value.String, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.String), err
			}
			return value.NewString(f(v.S)), nil
		}}, x)
	case "abs":
		if err := arity(1); err != nil {
			return nil, err
		}
		x := args[0]
		var err error
		switch x.typ {
		case value.Unknown:
			// Postgres resolves an unknown argument to its preferred numeric type, double
			// precision.
			if x, err = resolve(x, value.Float); err != nil {
				return nil, err
			}
		case value.Numeric:
			return nil, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
		case value.Decimal:
			return nil, errDecimalArithmetic()
		case value.Int4, value.Int, value.Float:
		default:
			return nil, fmt.Errorf("%w: function abs(%v) does not exist", ErrType, x.typ)
		}
		t := x.typ
		return b.fold(&cexpr{typ: t, fallible: t != value.Float, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return v, err
			}
			switch {
			case v.Type == value.Float:
				return value.NewFloat(math.Abs(v.F)), nil
			case t == value.Int4 && v.I == math.MinInt32:
				return value.Value{}, fmt.Errorf("%w: integer out of range", ErrOutOfRange)
			case t == value.Int && v.I == math.MinInt64:
				return value.Value{}, fmt.Errorf("%w: bigint out of range", ErrOutOfRange)
			case v.I < 0:
				return value.Value{Type: t, I: -v.I}, nil
			}
			return v, nil
		}}, x)
	}
	return nil, fmt.Errorf("%w: function %s does not exist", ErrInvalid, e.Name)
}

// nullif binds NULLIF(a, b), which is NULL when a = b and a otherwise. As in Postgres, its type
// is that of the left operand of the equality, which a cross-type equality of integers and
// bigints leaves as it is, and which the other implicit casts convert to the type of b.
func (b *binder) nullif(e *ast.Call) (*cexpr, error) {
	l, err := b.bind(e.Args[0])
	if err != nil {
		return nil, err
	}
	r, err := b.bind(e.Args[1])
	if err != nil {
		return nil, err
	}
	cl, cr, err := b.unify(l, r, "=")
	if err != nil {
		return nil, err
	}
	result := cl
	if l.typ == value.Int4 && cl.typ == value.Int {
		// Postgres compares integer with bigint without converting the integer.
		result = l
	}
	if cl.folded && cl.val.Null {
		return constant(value.NewNull(result.typ)), nil
	}
	t := result.typ
	return b.fold(&cexpr{typ: t, eval: func(row []value.Value) (value.Value, error) {
		v, err := result.value(row)
		if err != nil {
			return value.Value{}, err
		}
		x, err := cl.value(row)
		if err != nil {
			return value.Value{}, err
		}
		y, err := cr.value(row)
		if err != nil {
			return value.Value{}, err
		}
		if !x.Null && !y.Null && value.Compare(x, y) == 0 {
			return value.NewNull(t), nil
		}
		return v, nil
	}}, result, cl, cr)
}

// asciiLower folds ASCII letters only, as Postgres does with the C locale.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' {
			return r - ('a' - 'A')
		}
		return r
	}, s)
}

// like matches s against a LIKE pattern, where _ matches one character, % matches any sequence,
// and a backslash escapes the next character. It is a port of the Postgres matcher, so that an
// escape character at the end of the pattern is an error only when matching reaches it.
func like(s, pattern string) (bool, error) {
	m, err := likeMatch([]rune(s), []rune(pattern))
	return m == likeTrue, err
}

const (
	likeFalse = iota
	likeTrue
	// likeAbort means no later start position can match either.
	likeAbort
)

func likeMatch(t, p []rune) (int, error) {
	for len(t) > 0 && len(p) > 0 {
		switch p[0] {
		case '\\':
			p = p[1:]
			if len(p) == 0 {
				return 0, fmt.Errorf("%w: LIKE pattern must not end with escape character", ErrInvalid)
			}
			if p[0] != t[0] {
				return likeFalse, nil
			}
		case '%':
			p = p[1:]
		skip:
			for len(p) > 0 {
				switch p[0] {
				case '%':
					p = p[1:]
				case '_':
					if len(t) == 0 {
						return likeAbort, nil
					}
					t, p = t[1:], p[1:]
				default:
					break skip
				}
			}
			if len(p) == 0 {
				return likeTrue, nil
			}
			first := p[0]
			if first == '\\' {
				if len(p) < 2 {
					return 0, fmt.Errorf("%w: LIKE pattern must not end with escape character", ErrInvalid)
				}
				first = p[1]
			}
			for len(t) > 0 {
				if t[0] == first {
					m, err := likeMatch(t, p)
					if err != nil || m != likeFalse {
						return m, err
					}
				}
				t = t[1:]
			}
			return likeAbort, nil
		case '_':
		default:
			if p[0] != t[0] {
				return likeFalse, nil
			}
		}
		t, p = t[1:], p[1:]
	}
	if len(t) > 0 {
		return likeFalse, nil
	}
	for len(p) > 0 && p[0] == '%' {
		p = p[1:]
	}
	if len(p) == 0 {
		return likeTrue, nil
	}
	return likeAbort, nil
}
