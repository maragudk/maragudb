package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// lookupTable returns the table, or false when it does not exist.
func (s *Session) lookupTable(ctx context.Context, name string) (*table, bool, error) {
	t, err := s.getTable(ctx, name)
	if errors.Is(err, ErrNoTable) {
		return nil, false, nil
	}
	return t, err == nil, err
}

func (s *Session) createTable(ctx context.Context, st *ast.CreateTable) error {
	if _, exists, err := s.lookupTable(ctx, st.Name); err != nil {
		return err
	} else if exists {
		if st.IfNotExists {
			return nil
		}
		return fmt.Errorf("%w: table %q already exists", ErrTableExists, st.Name)
	}
	t, err := newTable(st)
	if err != nil {
		return err
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.kv.put(catalogKey(st.Name), b)
	return nil
}

func (s *Session) dropTable(ctx context.Context, st *ast.DropTable) error {
	t, exists, err := s.lookupTable(ctx, st.Name)
	if err != nil {
		return err
	}
	if !exists {
		if st.IfExists {
			return nil
		}
		return fmt.Errorf("%w: table %q does not exist", ErrNoTable, st.Name)
	}
	start, end := t.rowRange()
	it := s.kv.scan(ctx, start, end)
	for it.Next() {
		s.kv.del(it.Key())
	}
	if err := it.Err(); err != nil {
		return err
	}
	s.kv.del(catalogKey(st.Name))
	return nil
}

// assignable checks that an expression can be stored in a column, adapting an int constant to a
// float column and a NULL to the column's type.
func assignable(e *cexpr, t *table, col int) (*cexpr, error) {
	c := t.Columns[col]
	switch {
	case e.typ == c.Type:
		return e, nil
	case e.typ == value.Unknown:
		return retypeNull(e, c.Type), nil
	case e.typ == value.Int && c.Type == value.Float && e.literal:
		return constant(value.NewFloat(float64(e.val.I))), nil
	}
	return nil, fmt.Errorf("%w: column %q is of type %v but the expression is of type %v", ErrType, c.Name, c.Type, e.typ)
}

func (s *Session) insert(ctx context.Context, st *ast.Insert, params []value.Value) (Result, error) {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return Result{}, err
	}
	cols := make([]int, 0, len(t.Columns))
	if len(st.Columns) == 0 {
		for i := range t.Columns {
			cols = append(cols, i)
		}
	} else {
		seen := map[int]bool{}
		for _, name := range st.Columns {
			i, ok := t.columnIndex(name)
			if !ok {
				return Result{}, fmt.Errorf("%w: column %q of table %q does not exist", ErrInvalid, name, t.Name)
			}
			if seen[i] {
				return Result{}, fmt.Errorf("%w: column %q specified more than once", ErrInvalid, name)
			}
			seen[i] = true
			cols = append(cols, i)
		}
	}
	b := &binder{params: params}
	for _, exprRow := range st.Rows {
		if len(exprRow) != len(cols) {
			return Result{}, fmt.Errorf("%w: INSERT has %d expressions but %d target columns", ErrInvalid, len(exprRow), len(cols))
		}
		row := make([]value.Value, len(t.Columns))
		for i, c := range t.Columns {
			row[i] = value.NewNull(c.Type)
		}
		for i, e := range exprRow {
			c, err := b.bind(e)
			if err != nil {
				return Result{}, err
			}
			if c, err = assignable(c, t, cols[i]); err != nil {
				return Result{}, err
			}
			if row[cols[i]], err = c.value(nil); err != nil {
				return Result{}, err
			}
		}
		if err := s.writeRow(ctx, t, nil, row); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(st.Rows))}, nil
}

// writeRow checks constraints and writes row, replacing the row at oldKey when given.
func (s *Session) writeRow(ctx context.Context, t *table, oldKey []byte, row []value.Value) error {
	for i, c := range t.Columns {
		if c.NotNull && row[i].Null {
			return fmt.Errorf("%w: column %q of table %q", ErrNotNull, c.Name, t.Name)
		}
		if row[i].Type == value.String && !validString([]byte(row[i].S)) {
			return fmt.Errorf("%w: column %q is not valid UTF-8", ErrInvalid, c.Name)
		}
	}
	key := t.rowKey(row)
	if oldKey == nil || string(key) != string(oldKey) {
		if _, exists, err := s.kv.get(ctx, key); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("%w: table %q already has a row with key %s", ErrDuplicateKey, t.Name, keyDescription(t, row))
		}
		if oldKey != nil {
			s.kv.del(oldKey)
		}
	}
	s.kv.put(key, value.EncodeRow(nil, row))
	return nil
}

func keyDescription(t *table, row []value.Value) string {
	var parts []string
	for _, i := range t.PrimaryKey {
		parts = append(parts, fmt.Sprintf("%s=%v", t.Columns[i].Name, row[i]))
	}
	return fmt.Sprintf("(%s)", joinStrings(parts, ", "))
}

func joinStrings(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// filter is a bound WHERE clause.
type filter struct {
	cond *cexpr
}

func (b *binder) where(e ast.Expr) (filter, error) {
	if e == nil {
		return filter{}, nil
	}
	c, err := b.bind(e)
	if err != nil {
		return filter{}, err
	}
	if c.typ != value.Bool && c.typ != value.Unknown {
		return filter{}, fmt.Errorf("%w: argument of WHERE must be bool, not %v", ErrType, c.typ)
	}
	return filter{cond: c}, nil
}

func (f filter) matches(row []value.Value) (bool, error) {
	if f.cond == nil {
		return true, nil
	}
	v, err := f.cond.value(row)
	if err != nil {
		return false, err
	}
	return !v.Null && v.B, nil
}

// scanRows calls fn with the key and decoded row of every row of t that the access path and
// filter admit, in key order, until fn returns false.
func (s *Session) scanRows(ctx context.Context, t *table, a access, f filter, fn func(key []byte, row []value.Value) (bool, error)) error {
	if a.empty {
		return nil
	}
	types := t.types()
	visit := func(key, val []byte) (bool, error) {
		row, err := value.DecodeRow(val, types)
		if err != nil {
			return false, fmt.Errorf("exec: table %q: %w", t.Name, err)
		}
		ok, err := f.matches(row)
		if err != nil || !ok {
			return err == nil, err
		}
		return fn(key, row)
	}
	if a.point != nil {
		val, ok, err := s.kv.get(ctx, a.point)
		if err != nil || !ok {
			return err
		}
		_, err = visit(a.point, val)
		return err
	}
	it := s.kv.scan(ctx, a.start, a.end)
	for it.Next() {
		if more, err := visit(it.Key(), it.Value()); err != nil || !more {
			return err
		}
	}
	return it.Err()
}

func (s *Session) update(ctx context.Context, st *ast.Update, params []value.Value) (Result, error) {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return Result{}, err
	}
	b := &binder{table: t, params: params}
	type assignment struct {
		col int
		e   *cexpr
	}
	var sets []assignment
	seen := map[int]bool{}
	for _, a := range st.Set {
		i, ok := t.columnIndex(a.Column)
		if !ok {
			return Result{}, fmt.Errorf("%w: column %q of table %q does not exist", ErrInvalid, a.Column, t.Name)
		}
		if seen[i] {
			return Result{}, fmt.Errorf("%w: column %q specified more than once", ErrInvalid, a.Column)
		}
		seen[i] = true
		e, err := b.bind(a.Value)
		if err != nil {
			return Result{}, err
		}
		if e, err = assignable(e, t, i); err != nil {
			return Result{}, err
		}
		sets = append(sets, assignment{i, e})
	}
	f, err := b.where(st.Where)
	if err != nil {
		return Result{}, err
	}
	a, err := b.plan(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	var n int64
	apply := func(key []byte, old []value.Value) (bool, error) {
		row := append([]value.Value{}, old...)
		for _, set := range sets {
			var err error
			if row[set.col], err = set.e.value(old); err != nil {
				return false, err
			}
		}
		if err := s.writeRow(ctx, t, key, row); err != nil {
			return false, err
		}
		n++
		return true, nil
	}
	if s.opts.Mutations.UpdateSeesOwnWrites {
		return Result{RowsAffected: n}, s.updateRestarting(ctx, t, a, f, apply, &n)
	}
	if err := s.scanRows(ctx, t, a, f, apply); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, nil
}

// updateRestarting is the mutated UPDATE: it applies each write to the transaction at once and
// restarts the scan after the key it just visited, so it can see and revisit its own writes.
func (s *Session) updateRestarting(ctx context.Context, t *table, a access, f filter, apply func([]byte, []value.Value) (bool, error), n *int64) error {
	if a.point != nil {
		return s.scanRows(ctx, t, a, f, apply)
	}
	for {
		var last []byte
		err := s.scanRows(ctx, t, a, f, func(key []byte, row []value.Value) (bool, error) {
			last = append([]byte{}, key...)
			_, err := apply(key, row)
			return false, err
		})
		if err != nil {
			return err
		}
		if last == nil {
			return nil
		}
		if err := s.kv.apply(); err != nil {
			return err
		}
		a.start = append(last, 0)
	}
}

func (s *Session) delete(ctx context.Context, st *ast.Delete, params []value.Value) (Result, error) {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return Result{}, err
	}
	b := &binder{table: t, params: params}
	f, err := b.where(st.Where)
	if err != nil {
		return Result{}, err
	}
	a, err := b.plan(t, st.Where)
	if err != nil {
		return Result{}, err
	}
	var n int64
	err = s.scanRows(ctx, t, a, f, func(key []byte, _ []value.Value) (bool, error) {
		s.kv.del(key)
		n++
		return true, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, nil
}

// orderKey is one ORDER BY item, bound either to an output column or to an expression over the
// input row.
type orderKey struct {
	output int
	expr   *cexpr
	desc   bool
	// nullsFirst after applying the Postgres default: last when ascending, first when descending.
	nullsFirst bool
}

func (s *Session) selectStmt(ctx context.Context, st *ast.Select, params []value.Value) (*Rows, error) {
	var t *table
	if st.From != "" {
		var err error
		if t, err = s.getTable(ctx, st.From); err != nil {
			return nil, err
		}
	}
	b := &binder{table: t, params: params}

	var columns []Column
	var items []*cexpr
	// itemExprs are the output expressions with stars expanded.
	var itemExprs []ast.Expr
	for _, item := range st.Items {
		if item.Star {
			if t == nil {
				return nil, fmt.Errorf("%w: SELECT * needs a FROM clause", ErrInvalid)
			}
			for i, c := range t.Columns {
				ref := &ast.ColumnRef{Name: c.Name}
				e, _ := b.bind(ref)
				items = append(items, e)
				itemExprs = append(itemExprs, ref)
				columns = append(columns, Column{Name: c.Name, Type: c.Type, Nullable: !t.Columns[i].NotNull})
			}
			continue
		}
		e, err := b.bind(item.Expr)
		if err != nil {
			return nil, err
		}
		items = append(items, e)
		itemExprs = append(itemExprs, item.Expr)
		col := Column{Name: item.Alias, Type: e.typ, Nullable: true}
		if col.Name == "" {
			col.Name = outputName(item.Expr)
		}
		if ref, ok := item.Expr.(*ast.ColumnRef); ok {
			i, _ := t.columnIndex(ref.Name)
			col.Nullable = !t.Columns[i].NotNull
		}
		columns = append(columns, col)
	}

	f, err := b.where(st.Where)
	if err != nil {
		return nil, err
	}
	var a access
	if t != nil {
		if a, err = b.plan(t, st.Where); err != nil {
			return nil, err
		}
	}

	var order []orderKey
	for _, o := range st.OrderBy {
		k := orderKey{output: -1, desc: o.Desc}
		switch o.Nulls {
		case ast.NullsFirst:
			k.nullsFirst = true
		case ast.NullsLast:
			k.nullsFirst = false
		default:
			k.nullsFirst = o.Desc
		}
		switch e := o.Expr.(type) {
		case *ast.Literal:
			// An int constant is an output position. Other untyped constants are an error, as in
			// Postgres, while bytes and time literals are typed and sort as constants.
			switch {
			case e.Value.Null, e.Value.Type == value.Float, e.Value.Type == value.String, e.Value.Type == value.Bool:
				return nil, fmt.Errorf("%w: non-integer constant in ORDER BY", ErrInvalid)
			case e.Value.Type == value.Int:
				if e.Value.I < 1 || e.Value.I > int64(len(columns)) {
					return nil, fmt.Errorf("%w: ORDER BY position %d is not in the select list", ErrInvalid, e.Value.I)
				}
				k.output = int(e.Value.I) - 1
			}
		case *ast.ColumnRef:
			// A name matches an output column first. Several output columns of that name are
			// only ambiguous when they are different expressions, as in Postgres.
			matches := -1
			for i, c := range columns {
				if c.Name == e.Name {
					if matches >= 0 && ast.RenderExpr(itemExprs[matches], ast.Maragudb) != ast.RenderExpr(itemExprs[i], ast.Maragudb) {
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
			if k.expr, err = b.bind(o.Expr); err != nil {
				return nil, err
			}
		}
		order = append(order, k)
	}

	limit, err := s.limit(st.Limit, params, "LIMIT")
	if err != nil {
		return nil, err
	}
	offset, err := s.limit(st.Offset, params, "OFFSET")
	if err != nil {
		return nil, err
	}

	type sortable struct {
		out  []value.Value
		keys []value.Value
	}
	var collected []sortable
	var out [][]value.Value
	var skipped int64
	project := func(row []value.Value) ([]value.Value, error) {
		o := make([]value.Value, len(items))
		for i, e := range items {
			var err error
			if o[i], err = e.value(row); err != nil {
				return nil, err
			}
		}
		return o, nil
	}
	visit := func(_ []byte, row []value.Value) (bool, error) {
		if len(order) == 0 && limit >= 0 && int64(len(out)) >= limit {
			return false, nil
		}
		o, err := project(row)
		if err != nil {
			return false, err
		}
		if len(order) > 0 {
			keys := make([]value.Value, len(order))
			for i, k := range order {
				if k.output >= 0 {
					keys[i] = o[k.output]
					continue
				}
				if keys[i], err = k.expr.value(row); err != nil {
					return false, err
				}
			}
			collected = append(collected, sortable{o, keys})
			return true, nil
		}
		if offset >= 0 && skipped < offset {
			skipped++
			return true, nil
		}
		out = append(out, o)
		return limit < 0 || int64(len(out)) < limit, nil
	}
	if t == nil {
		if ok, err := f.matches(nil); err != nil {
			return nil, err
		} else if ok {
			if _, err := visit(nil, nil); err != nil {
				return nil, err
			}
		}
	} else if err := s.scanRows(ctx, t, a, f, visit); err != nil {
		return nil, err
	}
	if len(order) > 0 {
		sort.SliceStable(collected, func(i, j int) bool {
			return compareKeys(order, collected[i].keys, collected[j].keys) < 0
		})
		for _, c := range collected {
			out = append(out, c.out)
		}
		if offset > 0 {
			out = out[min(offset, int64(len(out))):]
		}
		if limit >= 0 && int64(len(out)) > limit {
			out = out[:limit]
		}
	}
	if out == nil {
		out = [][]value.Value{}
	}
	return &Rows{Columns: columns, Rows: out}, nil
}

// limit evaluates a LIMIT or OFFSET expression. It returns -1 when absent or NULL.
func (s *Session) limit(e ast.Expr, params []value.Value, what string) (int64, error) {
	if e == nil {
		return -1, nil
	}
	b := &binder{params: params}
	c, err := b.bind(e)
	if err != nil {
		return 0, err
	}
	if c.typ != value.Int && c.typ != value.Unknown {
		return 0, fmt.Errorf("%w: argument of %s must be int, not %v", ErrType, what, c.typ)
	}
	if c.val.Null {
		return -1, nil
	}
	if c.val.I < 0 {
		return 0, fmt.Errorf("%w: %s must not be negative", ErrInvalid, what)
	}
	return c.val.I, nil
}

func compareKeys(order []orderKey, a, b []value.Value) int {
	for i, k := range order {
		x, y := a[i], b[i]
		var c int
		switch {
		case x.Null && y.Null:
			continue
		case x.Null:
			c = 1
			if k.nullsFirst {
				c = -1
			}
			return c
		case y.Null:
			c = -1
			if k.nullsFirst {
				c = 1
			}
			return c
		}
		c = value.Compare(x, y)
		if k.desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

// outputName of an expression without an alias, following Postgres: a column or function keeps
// its name, a CASE takes the name of its ELSE when that is a column or function and is "case"
// otherwise, and a cast keeps the name of what it casts or takes the type's name.
func outputName(e ast.Expr) string {
	name, _ := outputNameStrength(e)
	return name
}

// outputNameStrength is the name and how good it is: 2 for a column or function name, 1 for a
// generic name, and 0 for none.
func outputNameStrength(e ast.Expr) (string, int) {
	switch e := e.(type) {
	case *ast.ColumnRef:
		return e.Name, 2
	case *ast.Call:
		return e.Name, 2
	case *ast.Case:
		if e.Else != nil {
			if name, strength := outputNameStrength(e.Else); strength > 1 {
				return name, strength
			}
		}
		return "case", 1
	case *ast.Cast:
		if name, strength := outputNameStrength(e.Expr); strength > 1 {
			return name, strength
		}
		return e.Type.String(), 1
	}
	return "?column?", 0
}
