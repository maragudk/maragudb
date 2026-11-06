package exec

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// aggRun is a run of a grouped SELECT: the groups of its input rows, and what failed.
//
// Postgres evaluates the GROUP BY expressions and the arguments of the aggregates on every input
// row, and HAVING, the output, and ORDER BY on every group, but in an order that its plan
// decides: hashed grouping reads every row before it finishes any group, and sorted grouping
// evaluates the GROUP BY expressions of every row first and then finishes each group in turn.
// So when several things fail, which error Postgres reports depends on its plan, and the run
// reports an error only when every failure has the same class, and rejects the statement
// otherwise. A failure on any row or group fails the statement in every plan.
type aggRun struct {
	g     *grouping
	o     output
	order []orderKey
	h     havingConds
	// live are the aggregates that the output, ORDER BY, or HAVING read after folding, which are
	// the ones Postgres computes.
	live []bool
	// minmax is set when Postgres could compute every aggregate by reading only the first row
	// of an index, which is when the statement reads one table without GROUP BY and its
	// aggregates are all MIN and MAX.
	minmax bool

	groups map[string]*groupState
	list   []*groupState
	// failures of the input rows, and whereFailures of the WHERE clause on them.
	failures, whereFailures []error
	// orderDependent is set when a WHERE or HAVING condition fails on a row or group that another
	// one rejects.
	orderDependent error
	// planDependent is set when the result depends on the plan in another way, such as the order
	// in which a float sum adds.
	planDependent error
	// possible are the errors that some plans raise and others do not, such as the overflow of
	// a float sum in some orders of its rows.
	possible []error
}

// groupState is a group of input rows: its first row, its keys, and the states of its
// aggregates.
type groupState struct {
	row    []value.Value
	keys   []value.Value
	states []aggState
	// failed is set when a row of the group failed, so that Postgres never finishes it.
	failed bool
}

func newAggRun(g *grouping, o output, order []orderKey, h havingConds) *aggRun {
	r := &aggRun{g: g, o: o, order: order, h: h, live: make([]bool, len(g.aggs)), groups: map[string]*groupState{}}
	mark := func(e *cexpr) {
		for _, i := range e.aggs {
			r.live[i] = true
		}
	}
	for _, e := range o.items {
		mark(e)
	}
	for _, k := range order {
		if k.expr != nil {
			mark(k.expr)
		}
	}
	for _, c := range h.kept {
		mark(c)
	}
	return r
}

// liveAggs are the aggregates that the run computes.
func (r *aggRun) liveAggs() []*aggregate {
	var out []*aggregate
	for i, a := range r.g.aggs {
		if r.live[i] {
			out = append(out, a)
		}
	}
	return out
}

// onlyMinMax reports whether the statement has aggregates, and all of them are MIN or MAX.
func (r *aggRun) onlyMinMax() bool {
	live := r.liveAggs()
	return len(live) > 0 && !slices.ContainsFunc(live, func(a *aggregate) bool { return a.name != "min" && a.name != "max" })
}

// sortRefs are the columns whose order Postgres can use to group or sort, or to compute a MIN
// or MAX, so that a scan of an index on them is a plan it could pick.
func (r *aggRun) sortRefs() []colRef {
	var out []colRef
	for _, k := range r.g.keys {
		if k.plain && k.ref.cast == value.Unknown {
			out = append(out, k.ref)
		}
	}
	for _, k := range r.order {
		if k.plain {
			out = append(out, k.ref)
		}
	}
	for _, a := range r.liveAggs() {
		if a.argPlain && (a.name == "min" || a.name == "max") {
			out = append(out, a.argRef)
		}
	}
	return out
}

// limitable checks that a grouped SELECT with LIMIT or OFFSET gives the same rows in every plan:
// with GROUP BY, its ORDER BY sorts by every GROUP BY expression, so that the order of the
// groups is fully determined, and nothing can fail, because sorted grouping under a LIMIT stops
// reading rows when it has enough groups. Without GROUP BY, there is one group, which every plan
// reads all rows for.
func (r *aggRun) limitable(st *ast.Select, f filter) error {
	if len(r.g.keys) == 0 {
		return nil
	}
	for _, k := range r.g.keys {
		covered := false
		for i, o := range r.order {
			e := st.OrderBy[i].Expr
			if o.output >= 0 {
				e = r.o.exprs[o.output]
			}
			covered = covered || r.g.input.canonical(e) == k.canon
		}
		if !covered {
			return fmt.Errorf("%w: LIMIT and OFFSET on groups without an ORDER BY on every GROUP BY expression are not supported, because which groups Postgres returns is then not determined; add %s to ORDER BY", ErrUnsupported, ast.RenderExpr(k.expr))
		}
	}
	fails := f.fallible ||
		slices.ContainsFunc(r.g.keys, func(k *groupKey) bool { return k.ce.fallible }) ||
		slices.ContainsFunc(r.liveAggs(), func(a *aggregate) bool {
			return a.arg != nil && a.arg.fallible || a.typ == value.Float && (a.name == "sum" || a.name == "avg")
		}) ||
		slices.ContainsFunc(r.h.kept, func(c *cexpr) bool { return c.fallible }) ||
		slices.ContainsFunc(r.o.items, func(c *cexpr) bool { return c.fallible }) ||
		slices.ContainsFunc(r.order, func(k orderKey) bool { return k.expr != nil && k.expr.fallible })
	if fails {
		return fmt.Errorf("%w: LIMIT and OFFSET on groups with an expression that can fail are not supported, because how many rows Postgres evaluates it on depends on its plan; select the groups first and compute the expression on them", ErrUnsupported)
	}
	return nil
}

// add an input row to its group. Postgres evaluates the GROUP BY expressions of a row before the
// arguments of its aggregates, so those of a row whose GROUP BY expression fails are not
// evaluated.
func (r *aggRun) add(row []value.Value) {
	g := r.g
	keys := make([]value.Value, len(g.keys))
	failed := false
	for i, k := range g.keys {
		v, err := k.ce.value(row)
		if err != nil {
			r.failures = append(r.failures, err)
			failed = true
		}
		keys[i] = v
	}
	if failed {
		return
	}
	var enc []byte
	for _, v := range keys {
		enc = value.EncodeIndexKey(enc, keyValue(v), false, false)
	}
	gs, ok := r.groups[string(enc)]
	if !ok {
		gs = &groupState{row: slices.Clone(row), keys: keys, states: make([]aggState, len(g.aggs))}
		r.groups[string(enc)] = gs
		r.list = append(r.list, gs)
	} else {
		for i, k := range g.keys {
			if k.used && !keys[i].Null && value.Format(keys[i]) != value.Format(gs.keys[i]) {
				r.planDependent = cmpOr(r.planDependent, fmt.Errorf("%w: GROUP BY of -0 and 0 in one group is not supported when the query reads the value of the group, because which of them Postgres reads depends on its plan; add 0 to the expression, which makes -0 0", ErrUnsupported))
			}
		}
	}
	for i, a := range g.aggs {
		if !r.live[i] {
			continue
		}
		if a.star {
			gs.states[i].n++
			continue
		}
		v, err := a.arg.value(row)
		if err != nil {
			r.failures = append(r.failures, err)
			gs.failed = true
			continue
		}
		gs.states[i].add(v, a)
	}
}

// cmpOr returns a, or b when a is nil.
func cmpOr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// keyValue is v as a value that the key encoding has, with equal values encoded equally: an
// integer as a bigint, a numeric as the text of its value, and a literal as its text.
func keyValue(v value.Value) value.Value {
	switch v.Type {
	case value.Int4:
		return value.Value{Type: value.Int, I: v.I, Null: v.Null}
	case value.Decimal:
		if v.Null {
			return value.NewNull(value.String)
		}
		return value.NewString(value.DecimalKey(v.S))
	case value.Unknown, value.Numeric:
		// A constant of COUNT(DISTINCT), whose text is its value.
		return value.Value{Type: value.String, S: v.S, Null: v.Null}
	}
	return v
}

// rows finishes the groups and returns the rows of the statement.
func (r *aggRun) rows(distinct bool, offset, limit int64) (*Rows, error) {
	g := r.g
	if len(g.keys) == 0 && len(r.list) == 0 {
		// Without GROUP BY there is one group, also of no rows.
		r.list = append(r.list, &groupState{states: make([]aggState, len(g.aggs))})
	}
	if r.minmax && len(r.whereFailures) > 0 {
		return nil, fmt.Errorf("%w: a WHERE condition that fails in a query whose aggregates are all MIN and MAX is not supported, because Postgres may compute them from the first row of an index that passes the condition and read no other; write the condition so that it cannot fail, such as with CASE", ErrUnsupported)
	}
	failures := append(slices.Clone(r.whereFailures), r.failures...)
	var collected []sortable
	width := g.width + len(g.keys) + len(g.aggs)
	for _, gs := range r.list {
		if gs.failed {
			continue
		}
		row := make([]value.Value, width)
		copy(row, gs.row)
		copy(row[g.width:], gs.keys)
		for i, a := range g.aggs {
			at := g.width + len(g.keys) + i
			if !r.live[i] {
				row[at] = value.NewNull(a.typ)
				continue
			}
			v, err := gs.states[i].result(a)
			if err != nil {
				// The statement is rejected unless something fails, and the group goes on with a
				// NULL, which only the errors of its HAVING and output depend on.
				r.planDependent = cmpOr(r.planDependent, err)
				v = value.NewNull(a.typ)
			}
			if gs.states[i].overflows {
				r.possible = append(r.possible, fmt.Errorf("%w: value out of range: overflow", ErrOutOfRange))
			}
			row[at] = v
		}
		if r.h.never {
			continue
		}
		pass, err := r.passes(row)
		if err != nil {
			if errors.Is(err, errOrderDependent) {
				r.orderDependent = cmpOr(r.orderDependent, err)
			} else {
				failures = append(failures, err)
			}
			continue
		}
		if !pass {
			continue
		}
		out := make([]value.Value, len(r.o.items))
		var errs []error
		for i, e := range r.o.items {
			if out[i], err = e.value(row); err != nil {
				errs = append(errs, err)
			}
		}
		keys := make([]value.Value, len(r.order))
		for i, k := range r.order {
			switch {
			case k.output >= 0:
				keys[i] = out[k.output]
			default:
				if keys[i], err = k.expr.value(row); err != nil {
					errs = append(errs, err)
				}
			}
		}
		if len(errs) > 0 {
			failures = append(failures, errs...)
			continue
		}
		collected = append(collected, sortable{out, keys})
	}
	if r.orderDependent != nil {
		return nil, r.orderDependent
	}
	if len(failures) > 0 {
		kind := errorKind(failures[0])
		for _, err := range append(failures[1:], r.possible...) {
			if !errors.Is(err, kind) {
				return nil, fmt.Errorf("%w: %w: rows or groups of this query fail with different errors, %v and %v, and which of them Postgres reports depends on the order of its plan; select the rows first, and compute the expressions on them", ErrUnsupported, errOrderDependent, kind, errorKind(err))
			}
		}
		return nil, failures[0]
	}
	if r.planDependent != nil {
		return nil, r.planDependent
	}
	out, err := finish(collected, distinct, r.order, len(r.order) > 0, offset, limit)
	if err != nil {
		return nil, err
	}
	return &Rows{Columns: r.o.columns, Rows: out}, nil
}

// passes reports whether a group row passes the conditions that HAVING keeps. Postgres
// evaluates them in an order its planner picks and stops at the first that is not true, so a
// condition that fails on a group that another condition rejects is [errOrderDependent].
func (r *aggRun) passes(row []value.Value) (bool, error) {
	var failure error
	pass := true
	rejecting := -1
	for i, c := range r.h.kept {
		v, err := c.value(row)
		switch {
		case err != nil:
			failure = cmpOr(failure, err)
		case v.Null || !v.B:
			pass = false
			if rejecting < 0 {
				rejecting = i
			}
		}
	}
	if failure != nil && !pass {
		return false, fmt.Errorf("%w: %w: whether this query fails depends on the order in which Postgres evaluates the conditions of its HAVING clause, because one fails on a group that %s rejects; combine them into one condition with CASE", ErrUnsupported, errOrderDependent, ast.RenderExpr(r.h.conjuncts[rejecting]))
	}
	return pass, failure
}

// aggState is the state of an aggregate in a group.
type aggState struct {
	// n counts the rows, or the values that are not NULL.
	n int64
	// sum of integers.
	sum *big.Int
	// floats are the values of a float SUM or AVG.
	floats []float64
	// best is the value of a MIN or MAX so far.
	best value.Value
	// zeros are the signs of the zero floats among the values: 1 for 0 and 2 for -0.
	zeros int
	// seen are the values of a DISTINCT aggregate, by their key.
	seen map[string]bool
	// overflows is set by [aggState.result] when the sum of floats can overflow in some order of
	// the rows.
	overflows bool
}

// add a value of the argument.
func (s *aggState) add(v value.Value, a *aggregate) {
	if v.Null {
		return
	}
	if v.Type == value.Float && v.F == 0 {
		if math.Signbit(v.F) {
			s.zeros |= 2
		} else {
			s.zeros |= 1
		}
	}
	if a.distinct {
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		k := string(value.EncodeIndexKey(nil, keyValue(v), false, false))
		if s.seen[k] {
			return
		}
		s.seen[k] = true
	}
	s.n++
	switch a.name {
	case "sum", "avg":
		if v.Type == value.Float {
			s.floats = append(s.floats, v.F)
			return
		}
		if s.sum == nil {
			s.sum = new(big.Int)
		}
		s.sum.Add(s.sum, big.NewInt(v.I))
	case "min", "max":
		if s.n == 1 {
			s.best = v
			return
		}
		c := value.Compare(v, s.best)
		if a.name == "min" && c < 0 || a.name == "max" && c > 0 {
			s.best = v
		}
	}
}

// result of the aggregate, and an error when it depends on the plan.
func (s *aggState) result(a *aggregate) (value.Value, error) {
	if a.name == "count" {
		return value.NewInt(s.n), nil
	}
	if s.n == 0 {
		return value.NewNull(a.typ), nil
	}
	switch a.name {
	case "sum", "avg":
		switch {
		case a.typ == value.Float:
			return s.floatResult(a)
		case a.name == "avg":
			return value.DecimalDiv(s.sum, big.NewInt(s.n)), nil
		case a.typ == value.Int:
			return value.NewInt(s.sum.Int64()), nil
		}
		return value.DecimalFromBig(s.sum), nil
	}
	// Postgres's float8smaller and float8larger keep the later of two equal values, so which zero
	// is the result depends on the order of the rows when both are among them.
	if s.best.Type == value.Float && s.best.F == 0 && s.zeros == 3 {
		return value.Value{}, fmt.Errorf("%w: %s of -0 and 0 is not supported, because which of them Postgres returns depends on its plan; add 0 to the argument, which makes -0 0", ErrUnsupported, a.name)
	}
	return s.best, nil
}

// floatResult of a SUM or AVG of floats. Postgres adds the values in the order its plan reads
// them, with float8pl for SUM and float8_accum for AVG, and floating-point addition depends on
// that order. The result is the one of every order only when every value and every sum of some
// of them is exact, which [orderFreeSum] checks; and when no order can overflow, which raises an
// error.
func (s *aggState) floatResult(a *aggregate) (value.Value, error) {
	sum, ok, overflows := orderFreeSum(s.floats, a.name == "avg")
	s.overflows = overflows
	if !ok {
		return value.Value{}, fmt.Errorf("%w: %s of these double precision values is not supported, because its result depends on the order in which Postgres adds them, which its plan decides; sum integers, or cast the values to bigint", ErrUnsupported, a.name)
	}
	if a.distinct && a.name == "sum" && s.zeros == 3 && sum == 0 {
		// DISTINCT keeps one of -0 and 0, which one depends on the plan.
		return value.Value{}, fmt.Errorf("%w: sum(DISTINCT) of -0 and 0 is not supported, because which of them Postgres keeps depends on its plan; add 0 to the argument, which makes -0 0", ErrUnsupported)
	}
	if a.name == "avg" {
		return value.NewFloat(sum / float64(s.n)), nil
	}
	return value.NewFloat(sum), nil
}

// orderFreeSum of xs, as Postgres's float SUM or, with avg, the sum of its AVG computes it in
// every order of the values. It reports false when the order can change the sum or whether
// computing it fails, and also whether some order can overflow.
//
// A sum of finite values is the same in every order when every value and every sum of some of
// them is exact: when they are all multiples of the smallest power of two among their lowest
// set bits, and the sum of their magnitudes in that unit is at most 2^53. Postgres then fails in
// no order. NaN and the infinities make the sum NaN or infinite in every order, but only when
// the finite values cannot overflow, which Postgres reports as an error. AVG also adds the
// squares of the deviations, which can overflow and fail on their own.
func orderFreeSum(xs []float64, avg bool) (float64, bool, bool) {
	var nan, posInf, negInf bool
	var finite []float64
	var abs, largest float64
	for _, x := range xs {
		switch {
		case math.IsNaN(x):
			nan = true
		case math.IsInf(x, 1):
			posInf = true
		case math.IsInf(x, -1):
			negInf = true
		default:
			finite = append(finite, x)
			abs += math.Abs(x)
			largest = max(largest, math.Abs(x))
		}
	}
	// A bound on every partial sum and, for AVG, on the sum of squares, with room for rounding.
	n := float64(len(xs))
	overflows := abs >= 0x1p1020 || math.IsInf(abs, 0)
	if avg {
		sxx := n * (largest*n + abs) * (largest*n + abs)
		overflows = overflows || sxx >= 0x1p1000 || math.IsInf(sxx, 0)
	}
	if overflows {
		return 0, false, true
	}
	if nan || posInf && negInf {
		return math.NaN(), true, false
	}
	if posInf {
		return math.Inf(1), true, false
	}
	if negInf {
		return math.Inf(-1), true, false
	}
	if !exactSum(finite) {
		return 0, false, false
	}
	// Exact, so the order of this loop gives the sum of every order. SUM starts at the first
	// value and AVG at 0, which differ only for a sum of -0s.
	var sum float64
	for i, x := range finite {
		if i == 0 && !avg {
			sum = x
			continue
		}
		sum += x
	}
	return sum, true, false
}

// exactSum reports whether every sum of some of the finite values xs is exact: in the unit of the
// smallest lowest set bit among them, the magnitudes add up to at most 2^53.
func exactSum(xs []float64) bool {
	type part struct {
		mant uint64
		exp  int
	}
	var parts []part
	unit := math.MaxInt
	for _, x := range xs {
		if x == 0 {
			continue
		}
		frac, exp := math.Frexp(math.Abs(x))
		mant := uint64(frac * (1 << 53))
		exp -= 53
		for mant&1 == 0 {
			mant >>= 1
			exp++
		}
		parts = append(parts, part{mant, exp})
		unit = min(unit, exp)
	}
	const limit = uint64(1) << 53
	var total uint64
	for _, p := range parts {
		shift := p.exp - unit
		if shift > 53 || p.mant > limit>>shift {
			return false
		}
		total += p.mant << shift
		if total > limit {
			return false
		}
	}
	return true
}
