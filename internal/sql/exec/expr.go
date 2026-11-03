package exec

import (
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
	// literal reports that the expression is built from literals only, so it is not a parameter.
	// Only such a constant adapts its type, as an integer literal does next to a float.
	literal bool
	val     value.Value
	eval    func(row []value.Value) (value.Value, error)
}

// value of the expression for row.
func (e *cexpr) value(row []value.Value) (value.Value, error) {
	if e.folded {
		return e.val, nil
	}
	return e.eval(row)
}

func constant(v value.Value) *cexpr {
	return &cexpr{typ: v.Type, folded: true, literal: true, val: v}
}

// param is a constant from a parameter, which never adapts its type.
func param(v value.Value) *cexpr {
	return &cexpr{typ: v.Type, folded: true, val: v}
}

// binder binds expressions against an optional table scope and the statement's parameters.
type binder struct {
	table  *table
	params []value.Value
	// pruned is positive while binding a branch that can never be evaluated, such as the ELSE of a
	// CASE whose WHEN is known true. Such a branch is still type-checked, but an error in folding
	// it is not reported, as in Postgres.
	pruned int
}

// fold a node whose children are all constants by evaluating it now, which also reports errors
// such as division by zero at bind time, as Postgres does when it folds constants.
func (b *binder) fold(e *cexpr, children ...*cexpr) (*cexpr, error) {
	literal := true
	for _, c := range children {
		if c.refs {
			e.refs = true
			return e, nil
		}
		literal = literal && c.literal
	}
	v, err := e.eval(nil)
	if err != nil {
		if b.pruned > 0 {
			return e, nil
		}
		return nil, err
	}
	c := constant(v)
	c.literal = literal
	return c, nil
}

// bindPruned binds a branch that is known never to be evaluated.
func (b *binder) bindPruned(e ast.Expr) (*cexpr, error) {
	b.pruned++
	defer func() { b.pruned-- }()
	return b.bind(e)
}

// unify two operand types for a binary operation. A NULL of unknown type takes the other type,
// and an int constant becomes a float when the other side is a float, which is how an integer
// literal adapts to a float column. Anything else is an error: there is no implicit coercion.
func unify(a, b *cexpr, what string) (*cexpr, *cexpr, error) {
	switch {
	case a.typ == b.typ:
		return a, b, nil
	case a.typ == value.Unknown:
		return retypeNull(a, b.typ), b, nil
	case b.typ == value.Unknown:
		return a, retypeNull(b, a.typ), nil
	case a.typ == value.Int && b.typ == value.Float && a.literal:
		return constant(value.NewFloat(float64(a.val.I))), b, nil
	case b.typ == value.Int && a.typ == value.Float && b.literal:
		return a, constant(value.NewFloat(float64(b.val.I))), nil
	}
	return nil, nil, fmt.Errorf("%w: %s %v and %v", ErrType, what, a.typ, b.typ)
}

func retypeNull(e *cexpr, t value.Type) *cexpr {
	if !e.folded {
		return e
	}
	return constant(value.NewNull(t))
}

// unifyAll unifies every expression to one common type, for IN lists, CASE results and COALESCE.
func unifyAll(es []*cexpr, what string) ([]*cexpr, value.Type, error) {
	t := value.Unknown
	for _, e := range es {
		switch {
		case e.typ == value.Unknown:
		case e.typ == value.Int && e.literal && t == value.Float:
		case t == value.Unknown, t == value.Int && e.typ == value.Float:
			t = e.typ
		}
	}
	out := make([]*cexpr, len(es))
	for i, e := range es {
		switch {
		case e.typ == t:
			out[i] = e
		case e.typ == value.Unknown:
			out[i] = retypeNull(e, t)
		case e.typ == value.Int && t == value.Float && e.literal:
			out[i] = constant(value.NewFloat(float64(e.val.I)))
		default:
			return nil, 0, fmt.Errorf("%w: %s %v and %v", ErrType, what, t, e.typ)
		}
	}
	return out, t, nil
}

func (b *binder) bind(e ast.Expr) (*cexpr, error) {
	switch e := e.(type) {
	case *ast.Literal:
		return constant(e.Value), nil
	case *ast.Param:
		if e.Index > len(b.params) {
			return nil, fmt.Errorf("%w: parameter $%d is missing, %d given", ErrInvalid, e.Index, len(b.params))
		}
		return param(b.params[e.Index-1]), nil
	case *ast.ColumnRef:
		if b.table == nil {
			return nil, fmt.Errorf("%w: column %q does not exist", ErrInvalid, e.Name)
		}
		i, ok := b.table.columnIndex(e.Name)
		if !ok {
			return nil, fmt.Errorf("%w: column %q does not exist", ErrInvalid, e.Name)
		}
		return &cexpr{typ: b.table.Columns[i].Type, refs: true, eval: func(row []value.Value) (value.Value, error) {
			return row[i], nil
		}}, nil
	case *ast.Unary:
		return b.unary(e)
	case *ast.Binary:
		return b.binary(e)
	case *ast.IsNull:
		x, err := b.bind(e.Expr)
		if err != nil {
			return nil, err
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
		return b.between(e)
	case *ast.In:
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
		if x.typ != value.Bool && x.typ != value.Unknown {
			return nil, fmt.Errorf("%w: argument of NOT must be bool, not %v", ErrType, x.typ)
		}
		return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.Bool), err
			}
			return value.NewBool(!v.B), nil
		}}, x)
	case ast.Neg:
		if x.typ != value.Int && x.typ != value.Float {
			return nil, fmt.Errorf("%w: operator - does not exist for %v", ErrType, x.typ)
		}
		return b.fold(&cexpr{typ: x.typ, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return v, err
			}
			if v.Type == value.Float {
				return value.NewFloat(-v.F), nil
			}
			if v.I == math.MinInt64 {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
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
	sym := ast.RenderExpr(&ast.Binary{Op: e.Op, Left: &ast.ColumnRef{Name: ""}, Right: &ast.ColumnRef{Name: ""}}, ast.Maragudb)
	sym = strings.TrimSpace(sym)
	l, r, err = unify(l, r, "operator "+sym+" does not exist for")
	if err != nil {
		return nil, err
	}
	t := l.typ
	if resultType, ok := binaryResultType(e.Op, t); ok && ((l.folded && l.val.Null) || (r.folded && r.val.Null)) {
		// Operators are strict, so a known NULL operand makes the result NULL without evaluating
		// the other operand, as Postgres folds it.
		return constant(value.NewNull(resultType)), nil
	}
	var op func(a, c value.Value) (value.Value, error)
	var resultType value.Type
	switch e.Op {
	case ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Mod:
		if t != value.Int && t != value.Float && t != value.Unknown {
			return nil, fmt.Errorf("%w: operator %s does not exist for %v and %v", ErrType, sym, t, t)
		}
		if e.Op == ast.Mod && t == value.Float {
			return nil, fmt.Errorf("%w: operator %% does not exist for float and float", ErrType)
		}
		resultType = t
		op = arithmetic(e.Op)
	case ast.Concat:
		if t != value.String && t != value.Bytes && t != value.Unknown {
			return nil, fmt.Errorf("%w: operator || does not exist for %v and %v", ErrType, t, t)
		}
		resultType = t
		op = func(a, c value.Value) (value.Value, error) {
			if a.Type == value.Bytes {
				return value.NewBytes(append(append([]byte{}, a.Raw...), c.Raw...)), nil
			}
			return value.NewString(a.S + c.S), nil
		}
	default:
		resultType = value.Bool
		op = comparison(e.Op)
	}
	return b.fold(&cexpr{typ: resultType, eval: func(row []value.Value) (value.Value, error) {
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

func (b *binder) logical(e *ast.Binary) (*cexpr, error) {
	l, err := b.bind(e.Left)
	if err != nil {
		return nil, err
	}
	if l.typ != value.Bool && l.typ != value.Unknown {
		return nil, fmt.Errorf("%w: argument of %s must be bool, not %v", ErrType, logicalName(e.Op), l.typ)
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
	if r.typ != value.Bool && r.typ != value.Unknown {
		return nil, fmt.Errorf("%w: argument of %s must be bool, not %v", ErrType, logicalName(e.Op), r.typ)
	}
	if decided {
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

// binaryResultType of a non-logical operator on operands of type t, when the operator exists.
func binaryResultType(op ast.BinaryOp, t value.Type) (value.Type, bool) {
	switch op {
	case ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Mod:
		return t, t == value.Int || t == value.Float || (t == value.Unknown && op != ast.Mod)
	case ast.Concat:
		return t, t == value.String || t == value.Bytes || t == value.Unknown
	}
	return value.Bool, true
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

// arithmetic on two non-null values of the same numeric type, with the checks Postgres makes:
// int overflow and division by zero are errors, and float overflow to infinity and underflow to
// zero are errors unless an operand already was infinite or zero.
func arithmetic(op ast.BinaryOp) func(a, b value.Value) (value.Value, error) {
	return func(a, b value.Value) (value.Value, error) {
		if a.Type == value.Float {
			return floatArithmetic(op, a.F, b.F)
		}
		x, y := a.I, b.I
		var r int64
		switch op {
		case ast.Add:
			r = x + y
			if (r > x) != (y > 0) {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
		case ast.Sub:
			r = x - y
			if (r < x) != (y > 0) {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
		case ast.Mul:
			r = x * y
			if x != 0 && (r/x != y || (x == -1 && y == math.MinInt64) || (y == -1 && x == math.MinInt64)) {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
		case ast.Div:
			if y == 0 {
				return value.Value{}, ErrDivisionByZero
			}
			if x == math.MinInt64 && y == -1 {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
			r = x / y
		case ast.Mod:
			if y == 0 {
				return value.Value{}, ErrDivisionByZero
			}
			if y == -1 {
				return value.NewInt(0), nil
			}
			r = x % y
		}
		return value.NewInt(r), nil
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

func (b *binder) between(e *ast.Between) (*cexpr, error) {
	x, err := b.bind(e.Expr)
	if err != nil {
		return nil, err
	}
	low, err := b.bind(e.Low)
	if err != nil {
		return nil, err
	}
	// Postgres rewrites x BETWEEN low AND high as x >= low AND x <= high, so when the first
	// comparison is known false the high bound is only type-checked.
	var high *cexpr
	if xl, lowl, err := unify(x, low, "BETWEEN mixes"); err == nil && xl.folded && lowl.folded && !xl.val.Null && !lowl.val.Null && value.Compare(xl.val, lowl.val) < 0 {
		high, err = b.bindPruned(e.High)
		if err != nil {
			return nil, err
		}
	} else if high, err = b.bind(e.High); err != nil {
		return nil, err
	}
	es, _, err := unifyAll([]*cexpr{x, low, high}, "BETWEEN mixes")
	if err != nil {
		return nil, err
	}
	x, low, high = es[0], es[1], es[2]
	not := e.Not
	// The high bound is only evaluated when the low comparison is not false.
	return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil {
			return value.Value{}, err
		}
		lo, err := low.value(row)
		if err != nil {
			return value.Value{}, err
		}
		var result value.Value
		switch {
		case !v.Null && !lo.Null && value.Compare(v, lo) < 0:
			result = value.NewBool(false)
		default:
			hi, err := high.value(row)
			if err != nil {
				return value.Value{}, err
			}
			switch {
			case !v.Null && !hi.Null && value.Compare(v, hi) > 0:
				result = value.NewBool(false)
			case v.Null || lo.Null || hi.Null:
				result = value.NewNull(value.Bool)
			default:
				result = value.NewBool(true)
			}
		}
		if not && !result.Null {
			result.B = !result.B
		}
		return result, nil
	}}, x, low, high)
}

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
	es, _, err = unifyAll(es, "IN mixes")
	if err != nil {
		return nil, err
	}
	x, list := es[0], es[1:]
	not := e.Not
	return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
		v, err := x.value(row)
		if err != nil {
			return value.Value{}, err
		}
		// The whole list is evaluated first, as Postgres evaluates the array of = ANY.
		items := make([]value.Value, len(list))
		for i, item := range list {
			if items[i], err = item.value(row); err != nil {
				return value.Value{}, err
			}
		}
		if v.Null {
			return value.NewNull(value.Bool), nil
		}
		sawNull := false
		for _, item := range items {
			if item.Null {
				sawNull = true
				continue
			}
			if value.Compare(v, item) == 0 {
				return value.NewBool(!not), nil
			}
		}
		if sawNull {
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
	for _, c := range []*cexpr{x, p} {
		if c.typ != value.String && c.typ != value.Unknown {
			return nil, fmt.Errorf("%w: LIKE needs strings, not %v", ErrType, c.typ)
		}
	}
	if (x.folded && x.val.Null) || (p.folded && p.val.Null) {
		return constant(value.NewNull(value.Bool)), nil
	}
	not := e.Not
	return b.fold(&cexpr{typ: value.Bool, eval: func(row []value.Value) (value.Value, error) {
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

func (b *binder) caseExpr(e *ast.Case) (*cexpr, error) {
	var operand *cexpr
	var err error
	if e.Operand != nil {
		if operand, err = b.bind(e.Operand); err != nil {
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
				if _, _, err = unify(operand, cond, "CASE mixes"); err != nil {
					return nil, err
				}
			} else if cond.typ != value.Bool && cond.typ != value.Unknown {
				return nil, fmt.Errorf("%w: argument of CASE WHEN must be bool, not %v", ErrType, cond.typ)
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
			if o, cond, err = unify(operand, cond, "CASE mixes"); err != nil {
				return nil, err
			}
			// A NULL operand or value never matches, and the comparison is strict, so the WHEN
			// is known not to match when either is a known NULL.
			if (o.folded && cond.folded) || (o.folded && o.val.Null) || (cond.folded && cond.val.Null) {
				known = true
				matched = o.folded && cond.folded && !o.val.Null && !cond.val.Null && value.Compare(o.val, cond.val) == 0
			}
		} else {
			if cond.typ != value.Bool && cond.typ != value.Unknown {
				return nil, fmt.Errorf("%w: argument of CASE WHEN must be bool, not %v", ErrType, cond.typ)
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
	results, t, err := unifyAll(results, "CASE results mix")
	if err != nil {
		return nil, err
	}
	if t == value.Unknown {
		// Results that are all untyped NULLs make a string, as Postgres resolves unknown to text.
		t = value.String
		for i := range results {
			results[i] = retypeNull(results[i], t)
		}
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
			return fmt.Errorf("%w: function %s takes %d argument(s), %d given", ErrInvalid, e.Name, n, len(args))
		}
		return nil
	}
	switch e.Name {
	case "coalesce":
		if len(e.Args) == 0 {
			return nil, fmt.Errorf("%w: coalesce needs at least one argument", ErrInvalid)
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
		unifiedAll, t, err := unifyAll(all, "coalesce mixes")
		if err != nil {
			return nil, err
		}
		if t == value.Unknown {
			// Arguments that are all untyped NULLs make a string, as Postgres resolves unknown to text.
			t = value.String
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
		x := args[0]
		if x.typ != value.String && x.typ != value.Bytes && x.typ != value.Unknown {
			return nil, fmt.Errorf("%w: length takes a string or bytes, not %v", ErrType, x.typ)
		}
		return b.fold(&cexpr{typ: value.Int, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return value.NewNull(value.Int), err
			}
			if v.Type == value.Bytes {
				return value.NewInt(int64(len(v.Raw))), nil
			}
			return value.NewInt(int64(utf8.RuneCountInString(v.S))), nil
		}}, x)
	case "lower", "upper":
		if err := arity(1); err != nil {
			return nil, err
		}
		x := args[0]
		if x.typ != value.String && x.typ != value.Unknown {
			return nil, fmt.Errorf("%w: %s takes a string, not %v", ErrType, e.Name, x.typ)
		}
		f := strings.ToLower
		if e.Name == "upper" {
			f = strings.ToUpper
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
		if x.typ != value.Int && x.typ != value.Float && x.typ != value.Unknown {
			return nil, fmt.Errorf("%w: abs takes an int or float, not %v", ErrType, x.typ)
		}
		if x.typ == value.Unknown {
			// Postgres resolves abs(NULL) to its preferred numeric type, double precision.
			return constant(value.NewNull(value.Float)), nil
		}
		return b.fold(&cexpr{typ: x.typ, eval: func(row []value.Value) (value.Value, error) {
			v, err := x.value(row)
			if err != nil || v.Null {
				return v, err
			}
			if v.Type == value.Float {
				return value.NewFloat(math.Abs(v.F)), nil
			}
			if v.I == math.MinInt64 {
				return value.Value{}, fmt.Errorf("%w: int out of range", ErrOutOfRange)
			}
			if v.I < 0 {
				return value.NewInt(-v.I), nil
			}
			return v, nil
		}}, x)
	}
	return nil, fmt.Errorf("%w: function %s does not exist", ErrInvalid, e.Name)
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
