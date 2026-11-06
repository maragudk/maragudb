package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// output of a SELECT: its columns, and their expressions, bound and as Postgres compares them,
// with the stars expanded.
type output struct {
	columns []Column
	items   []*cexpr
	exprs   []ast.Expr
}

// selectList binds the output of a SELECT.
func (b *binder) selectList(st *ast.Select) (output, error) {
	var o output
	for _, item := range st.Items {
		if item.Star {
			cols, err := b.star(item.Table)
			if err != nil {
				return output{}, err
			}
			for _, c := range cols {
				e := b.column(c.ref)
				if b.agg != nil && b.agg.g != nil {
					// In a grouped SELECT, a column is a GROUP BY expression or of a table whose
					// primary key the GROUP BY has.
					var err error
					if e, err = b.bind(b.refExpr(c)); err != nil {
						return output{}, err
					}
				}
				o.items = append(o.items, e)
				o.exprs = append(o.exprs, b.refExpr(c))
				o.columns = append(o.columns, Column{Name: c.name, Type: e.typ, Nullable: b.nullable(c.ref)})
			}
			continue
		}
		e, err := b.bind(item.Expr)
		if err != nil {
			return output{}, err
		}
		// An output column of unknown type is text, as in Postgres.
		if e, err = resolve(e, value.String); err != nil {
			return output{}, err
		}
		if e.typ == value.Numeric {
			return output{}, fmt.Errorf("%w: the numeric type is not supported, cast the literal %s to double precision or bigint", ErrUnsupported, e.val.S)
		}
		o.items = append(o.items, e)
		o.exprs = append(o.exprs, item.Expr)
		col := Column{Name: item.Alias, Type: e.typ, Nullable: true}
		if col.Name == "" {
			col.Name = OutputName(item.Expr)
		}
		if ref, ok := item.Expr.(*ast.ColumnRef); ok {
			if r, err := b.resolve(ref); err == nil {
				col.Nullable = b.nullable(r)
			}
		}
		o.columns = append(o.columns, col)
	}
	return o, nil
}

// star returns the columns of `*`, every column that a name alone can reference, in order, or
// of `table.*`, every column of the entry of that name.
func (b *binder) star(table string) ([]nsCol, error) {
	sc, ns := b.names()
	if sc == nil {
		if table != "" {
			return nil, fmt.Errorf("%w: missing FROM-clause entry for table %q", ErrNoTable, table)
		}
		return nil, fmt.Errorf("%w: SELECT * with no tables specified is not valid", ErrSyntax)
	}
	if table == "" {
		var cols []nsCol
		for _, it := range ns {
			cols = append(cols, it.cols...)
		}
		return cols, nil
	}
	i := slices.IndexFunc(ns, func(it nsItem) bool { return it.rel && sc.entries[it.entry].name == table })
	if i < 0 {
		// The reference fails as a column of that table would.
		_, err := sc.resolve(ns, &ast.ColumnRef{Table: table, Name: "*"})
		return nil, err
	}
	return sc.item(ns[i].entry).cols, nil
}

// refExpr is the expression of a column that a star expands to, as [binder.normalize] writes a
// reference to it.
func (b *binder) refExpr(c nsCol) ast.Expr {
	if len(b.scope.entries) == 1 {
		return &ast.ColumnRef{Name: c.name}
	}
	return b.scope.expr(c.ref)
}

// nullable reports whether the column r can be NULL: when it is not NOT NULL, or when its entry
// is on the right side of a LEFT JOIN.
func (b *binder) nullable(r colRef) bool {
	e := b.scope.entries[r.entry]
	return !e.t.Columns[r.col].NotNull || e.nullable
}

// orderBy binds the ORDER BY of a SELECT with the output o. With DISTINCT, every item must be an
// output column, by its name, its position, or its expression, as in Postgres.
func (b *binder) orderBy(st *ast.Select, o output) ([]orderKey, error) {
	var order []orderKey
	for _, item := range st.OrderBy {
		k := orderKey{output: -1, desc: item.Desc, column: -1}
		switch item.Nulls {
		case ast.NullsFirst:
			k.nullsFirst = true
		case ast.NullsLast:
			k.nullsFirst = false
		default:
			k.nullsFirst = item.Desc
		}
		switch e := item.Expr.(type) {
		case *ast.Literal:
			// An integer constant is an output position. Other constants are an error, as in
			// Postgres, which only takes a literal whose digits fit in 32 bits as a position, so
			// that -2147483648 is not one.
			switch {
			case e.Value.Null, e.Value.Type != value.Int4, e.Value.I == math.MinInt32:
				return nil, fmt.Errorf("%w: non-integer constant in ORDER BY", ErrSyntax)
			default:
				if e.Value.I < 1 || e.Value.I > int64(len(o.columns)) {
					return nil, fmt.Errorf("%w: ORDER BY position %d is not in select list", ErrInvalid, e.Value.I)
				}
				k.output = int(e.Value.I) - 1
			}
		case *ast.ColumnRef:
			if e.Table != "" {
				break
			}
			// A name alone matches an output column first. Several output columns of that name
			// are only ambiguous when they are different expressions, as in Postgres.
			matches := -1
			for i, c := range o.columns {
				if c.Name == e.Name {
					if matches >= 0 && b.canonical(o.exprs[matches]) != b.canonical(o.exprs[i]) {
						return nil, fmt.Errorf("%w: ORDER BY %q is ambiguous", ErrInvalid, e.Name)
					}
					if matches < 0 {
						matches = i
					}
				}
			}
			k.output = matches
		}
		if k.output < 0 {
			var err error
			if k.expr, err = b.bind(item.Expr); err != nil {
				return nil, err
			}
			if k.expr, err = resolve(k.expr, value.String); err != nil {
				return nil, err
			}
			if k.expr.typ == value.Numeric {
				return nil, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
			}
		}
		// The item sorts by a column when it is that column, either directly or through an
		// output column that is a plain reference to it.
		sorted := item.Expr
		if k.output >= 0 {
			sorted = o.exprs[k.output]
		}
		if ref, ok := sorted.(*ast.ColumnRef); ok && b.scope != nil {
			if r, err := b.resolve(ref); err == nil && r.cast == value.Unknown {
				k.ref, k.plain = r, true
				if len(b.scope.entries) == 1 {
					k.column = r.col
				}
			}
		}
		order = append(order, k)
	}
	if !st.Distinct {
		return order, nil
	}
	// Postgres matches an ORDER BY expression to an output column of the same expression, and
	// DISTINCT needs every sort key to be one.
	for i, k := range order {
		if k.output >= 0 {
			continue
		}
		want := b.canonical(st.OrderBy[i].Expr)
		for j, e := range o.exprs {
			if b.canonical(e) == want {
				order[i].output, order[i].expr = j, nil
				break
			}
		}
		if order[i].output < 0 {
			return nil, fmt.Errorf("%w: for SELECT DISTINCT, ORDER BY expressions must appear in select list", ErrInvalid)
		}
	}
	return order, nil
}

// sortable is an output row and its sort keys.
type sortable struct {
	out  []value.Value
	keys []value.Value
}

// sortKeys of an output row o of the row: the output columns or the expressions of the ORDER BY.
func sortKeys(order []orderKey, o, row []value.Value) ([]value.Value, error) {
	keys := make([]value.Value, len(order))
	for i, k := range order {
		if k.output >= 0 {
			keys[i] = o[k.output]
			continue
		}
		var err error
		if keys[i], err = k.expr.value(row); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// finish the rows of a SELECT: drop those that DISTINCT finds equal to an earlier one, sort them
// when sorting is set, and apply OFFSET and LIMIT, where -1 means none.
func finish(rows []sortable, distinct bool, order []orderKey, sorting bool, offset, limit int64) ([][]value.Value, error) {
	if distinct {
		var err error
		if rows, err = distinctRows(rows); err != nil {
			return nil, err
		}
	}
	if sorting {
		sort.SliceStable(rows, func(i, j int) bool {
			return compareKeys(order, rows[i].keys, rows[j].keys) < 0
		})
	}
	out := make([][]value.Value, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.out)
	}
	if offset > 0 {
		out = out[min(offset, int64(len(out))):]
	}
	if limit >= 0 && int64(len(out)) > limit {
		out = out[:limit]
	}
	return out, nil
}

// distinctRows keeps the first of every group of rows whose output columns are equal by
// Postgres's equality of their types, in which NULLs are equal for DISTINCT, and -0 and 0, and
// all NaNs, are equal double precision values. Which row of a group Postgres outputs depends on
// its plan, so a group whose rows print differently, which only -0 and 0 do, and numerics of
// different scales, is not supported.
func distinctRows(rows []sortable) ([]sortable, error) {
	seen := map[string]int{}
	var out []sortable
	var key []byte
	for _, r := range rows {
		key = key[:0]
		for _, v := range r.out {
			// The key encoding has the column types, of which integer and numeric are not.
			key = value.EncodeIndexKey(key, keyValue(v), false, false)
		}
		i, dup := seen[string(key)]
		if !dup {
			seen[string(key)] = len(out)
			out = append(out, r)
			continue
		}
		for j, v := range r.out {
			w := out[i].out[j]
			if v.Null || value.Format(v) == value.Format(w) {
				continue
			}
			if v.Type == value.Float {
				return nil, fmt.Errorf("%w: SELECT DISTINCT of -0 and 0 in one column is not supported, because which of them Postgres outputs depends on its plan; add 0 to the column, which makes -0 0", ErrUnsupported)
			}
			return nil, fmt.Errorf("%w: SELECT DISTINCT of equal numerics of different scales in one column, such as %s and %s, is not supported, because which of them Postgres outputs depends on its plan; cast the column to double precision", ErrUnsupported, value.Format(w), value.Format(v))
		}
	}
	return out, nil
}

// selectStmt runs a SELECT.
func (s *Session) selectStmt(ctx context.Context, st *ast.Select, params []value.Value) (*Rows, error) {
	b := &binder{params: params, now: s.now(), aggClause: "JOIN conditions"}
	var nodes []*fromNode
	if len(st.From) > 0 {
		var err error
		if nodes, err = b.from(ctx, s, st.From); err != nil {
			return nil, err
		}
		if err := b.checkColumnAliases(st.From); err != nil {
			return nil, err
		}
	}
	if grouped(st) {
		return s.selectGrouped(ctx, st, b, nodes)
	}
	var t *table
	if b.scope != nil && len(b.scope.entries) == 1 {
		t = b.scope.entries[0].t
		b.table, b.entry = t, 0
	}
	b.phase = phaseOutput
	o, err := b.selectList(st)
	if err != nil {
		return nil, err
	}
	b.phase, b.aggClause = phaseWhere, "WHERE"
	f, err := b.where(st.Where, "WHERE")
	if err != nil {
		return nil, err
	}
	// ORDER BY is checked after WHERE, but its expressions are output expressions, which
	// Postgres folds before WHERE.
	b.phase = phaseOutput
	order, err := b.orderBy(st, o)
	if err != nil {
		return nil, err
	}
	offsetExpr, limitExpr, err := b.limits(st)
	if err != nil {
		return nil, err
	}
	if err := b.folding(); err != nil {
		return nil, err
	}
	if f.unsupported != nil {
		return nil, f.unsupported
	}
	if t == nil && len(nodes) > 0 {
		return s.selectJoin(ctx, st, b, nodes, f, o, order, offsetExpr, limitExpr, nil)
	}
	if (st.Limit != nil || st.Offset != nil) && t != nil {
		if err := limitable(t, order, o.items, f, st.Distinct, len(o.columns)); err != nil {
			return nil, err
		}
	}
	// The planner needs the folded constants.
	var a access
	if t != nil {
		var sortCols []int
		for _, k := range order {
			if k.column >= 0 {
				sortCols = append(sortCols, k.column)
			}
		}
		f.candidates = t.candidates(f, sortCols, b.reads(st, 0))
		var stream *scan
		if !fallible(f, o.items, order) && !st.Distinct {
			stream = t.orderedScan(order)
		}
		a = t.plan(f, stream)
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

	// The rows are collected and finished after they are read, unless the access reads them in
	// order and nothing drops rows.
	collecting := len(order) > 0 && !a.ordered || st.Distinct
	var collected []sortable
	var out [][]value.Value
	var skipped int64
	visit := func(_ []byte, row []value.Value) (bool, error) {
		if !collecting && limit >= 0 && int64(len(out)) >= limit {
			return false, nil
		}
		r, err := project(o.items, row)
		if err != nil {
			return false, err
		}
		if collecting {
			keys, err := sortKeys(order, r, row)
			if err != nil {
				return false, err
			}
			collected = append(collected, sortable{r, keys})
			return true, nil
		}
		if offset >= 0 && skipped < offset {
			skipped++
			return true, nil
		}
		out = append(out, r)
		return limit < 0 || int64(len(out)) < limit, nil
	}
	if t == nil {
		open, err := f.open()
		if err != nil {
			return nil, err
		}
		if ok, err := f.matches(nil); err != nil {
			return nil, err
		} else if ok && open {
			if _, err := visit(nil, nil); err != nil {
				return nil, err
			}
		}
	} else if err := s.scanRows(ctx, t, a, f, visit); err != nil {
		return nil, err
	}
	if collecting {
		if out, err = finish(collected, st.Distinct, order, len(order) > 0 && !a.ordered, offset, limit); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = [][]value.Value{}
	}
	return &Rows{Columns: o.columns, Rows: out}, nil
}

// limits binds the OFFSET and the LIMIT of a SELECT, in that order, as Postgres checks and
// folds them.
func (b *binder) limits(st *ast.Select) (*cexpr, *cexpr, error) {
	b.phase, b.aggClause = phaseOffset, "OFFSET"
	offset, err := b.limit(st.Offset, "OFFSET")
	if err != nil {
		return nil, nil, err
	}
	b.phase, b.aggClause = phaseLimit, "LIMIT"
	limit, err := b.limit(st.Limit, "LIMIT")
	if err != nil {
		return nil, nil, err
	}
	return offset, limit, nil
}

// project the output expressions on a row.
func project(items []*cexpr, row []value.Value) ([]value.Value, error) {
	o := make([]value.Value, len(items))
	for i, e := range items {
		var err error
		if o[i], err = e.value(row); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// checkColumnAliases rejects names for the columns of a table that repeat a name, which Postgres
// accepts and then reports as ambiguous wherever the name is used.
func (b *binder) checkColumnAliases(items []ast.TableExpr) error {
	for _, item := range items {
		for _, t := range ast.Tables(item) {
			for i, c := range t.Columns {
				if slices.Contains(t.Columns[:i], c) {
					return fmt.Errorf("%w: the column name %q twice in the names of the columns of a table is not supported, give every column its own name", ErrUnsupported, c)
				}
			}
		}
	}
	return nil
}

// reads are the columns of entry i that a SELECT reads, in its output, WHERE, GROUP BY, HAVING,
// ORDER BY, and the conditions of its joins.
func (b *binder) reads(st *ast.Select, i int) []int {
	var out []int
	add := func(col int) {
		if !slices.Contains(out, col) {
			out = append(out, col)
		}
	}
	walk := func(ns namespace, e ast.Expr) { b.columnsOf(ns, e, i, add) }
	for _, item := range st.Items {
		if !item.Star {
			walk(b.ns, item.Expr)
			continue
		}
		cols, _ := b.star(item.Table)
		for _, c := range cols {
			if c.ref.entry == i {
				add(c.ref.col)
			}
		}
	}
	walk(b.ns, st.Where)
	for _, e := range st.GroupBy {
		walk(b.ns, e)
	}
	walk(b.ns, st.Having)
	for _, o := range st.OrderBy {
		walk(b.ns, o.Expr)
	}
	return out
}

// columnsOf calls fn with the columns of entry i that e reads, resolved in the namespace ns.
func (b *binder) columnsOf(ns namespace, e ast.Expr, i int, fn func(col int)) {
	ast.Walk(e, func(x ast.Expr) bool {
		if ref, ok := x.(*ast.ColumnRef); ok {
			if r, err := b.scope.resolve(ns, ref); err == nil && r.entry == i {
				fn(r.col)
			}
		}
		return true
	})
}

// errorKinds are the errors whose class Postgres's SQLSTATE codes tell apart, which a statement
// that fails reports with the same class as Postgres.
var errorKinds = []error{ErrDivisionByZero, ErrOutOfRange, ErrCast, ErrType, ErrInvalid, ErrNotNull, ErrDuplicateKey, ErrCheck, ErrForeignKey, ErrRestrict}

// errorKind is the one of [errorKinds] that err is, or err.
func errorKind(err error) error {
	for _, k := range errorKinds {
		if errors.Is(err, k) {
			return k
		}
	}
	return err
}
