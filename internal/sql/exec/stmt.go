package exec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

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
		return fmt.Errorf("%w: relation %q already exists", ErrTableExists, st.Name)
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
				return Result{}, fmt.Errorf("%w: column %q of relation %q does not exist", ErrInvalid, name, t.Name)
			}
			if seen[i] {
				return Result{}, fmt.Errorf("%w: column %q specified more than once", ErrInvalid, name)
			}
			seen[i] = true
			cols = append(cols, i)
		}
	}
	// Every row is bound and folded before any is evaluated, as in Postgres.
	b := &binder{params: params}
	var bound [][]*cexpr
	// As in Postgres, each row is analyzed, checked against the first row's length and the
	// target columns, and converted to the column types before the next row.
	for _, exprRow := range st.Rows {
		var es []*cexpr
		for _, e := range exprRow {
			c, err := b.bind(e)
			if err != nil {
				return Result{}, err
			}
			es = append(es, c)
		}
		switch {
		case len(bound) > 0 && len(es) != len(bound[0]):
			return Result{}, fmt.Errorf("%w: VALUES lists must all be the same length", ErrSyntax)
		case len(es) > len(cols):
			return Result{}, fmt.Errorf("%w: INSERT has more expressions than target columns", ErrSyntax)
		case len(es) < len(cols):
			return Result{}, fmt.Errorf("%w: INSERT has more target columns than expressions", ErrSyntax)
		}
		for i := range es {
			var err error
			if es[i], err = b.assignable(es[i], t, cols[i]); err != nil {
				return Result{}, err
			}
		}
		bound = append(bound, es)
	}
	if err := b.folding(); err != nil {
		return Result{}, err
	}
	if err := s.writable(st); err != nil {
		return Result{}, err
	}
	// Postgres evaluates and inserts the rows one at a time, which matters for the expressions
	// it does not fold.
	for _, es := range bound {
		row := make([]value.Value, len(t.Columns))
		for i, c := range t.Columns {
			row[i] = value.NewNull(c.Type)
		}
		for i, c := range es {
			var err error
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
	if err := t.checkRow(row); err != nil {
		return err
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

// checkRow checks the NOT NULL constraints and that strings are valid UTF-8.
func (t *table) checkRow(row []value.Value) error {
	for i, c := range t.Columns {
		if c.NotNull && row[i].Null {
			return fmt.Errorf("%w: null value in column %q of relation %q violates not-null constraint", ErrNotNull, c.Name, t.Name)
		}
		if row[i].Type == value.String && !utf8.ValidString(row[i].S) {
			return fmt.Errorf("%w: column %q is not valid UTF-8", ErrInvalid, c.Name)
		}
	}
	return nil
}

func keyDescription(t *table, row []value.Value) string {
	var parts []string
	for _, i := range t.PrimaryKey {
		parts = append(parts, fmt.Sprintf("%s=%v", t.Columns[i].Name, row[i]))
	}
	return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
}

// scanRows calls fn with the key and decoded row of every row of t that the access path and
// filter admit, in key order, until fn returns false.
//
// A row whose outcome depends on the order in which Postgres evaluates the WHERE conditions
// does not stop the scan at once: a later row may fail in every order, and then the statement
// fails in Postgres for certain, with that row's error as one of the possible ones. Only when no
// row fails for certain is the statement [errOrderDependent].
func (s *Session) scanRows(ctx context.Context, t *table, a access, f filter, fn func(key []byte, row []value.Value) (bool, error)) error {
	if a.empty {
		return nil
	}
	if ok, err := f.open(); err != nil || !ok {
		return err
	}
	types := t.types()
	var orderDependent error
	visit := func(key, val []byte) (bool, error) {
		row, err := value.DecodeRow(val, types)
		if err != nil {
			return false, fmt.Errorf("exec: table %q: %w", t.Name, err)
		}
		ok, err := f.matches(row)
		if errors.Is(err, errOrderDependent) {
			if orderDependent == nil {
				orderDependent = err
			}
			return true, nil
		}
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
		if _, err = visit(a.point, val); err != nil {
			return err
		}
		return orderDependent
	}
	it := s.kv.scan(ctx, a.start, a.end)
	for it.Next() {
		if more, err := visit(it.Key(), it.Value()); err != nil || !more {
			return err
		}
	}
	if err := it.Err(); err != nil {
		return err
	}
	return orderDependent
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
	// Postgres analyzes WHERE first, then every SET expression, and then converts each to its
	// column's type. It folds the SET expressions before WHERE.
	b.phase = phaseWhere
	f, err := b.where(st.Where)
	if err != nil {
		return Result{}, err
	}
	b.phase = phaseOutput
	values := make([]*cexpr, len(st.Set))
	for i, a := range st.Set {
		if values[i], err = b.bind(a.Value); err != nil {
			return Result{}, err
		}
	}
	var sets []assignment
	movesKeys := false
	for j, a := range st.Set {
		i, ok := t.columnIndex(a.Column)
		if !ok {
			return Result{}, fmt.Errorf("%w: column %q of relation %q does not exist", ErrInvalid, a.Column, t.Name)
		}
		e, err := b.assignable(values[j], t, i)
		if err != nil {
			return Result{}, err
		}
		sets = append(sets, assignment{i, e})
		movesKeys = movesKeys || t.isPrimaryKey(i)
	}
	seen := map[int]bool{}
	for _, set := range sets {
		if seen[set.col] {
			return Result{}, fmt.Errorf("%w: multiple assignments to same column %q", ErrSyntax, t.Columns[set.col].Name)
		}
		seen[set.col] = true
	}
	// The planner needs the folded constants.
	if err := b.folding(); err != nil {
		return Result{}, err
	}
	if f.unsupported != nil {
		return Result{}, f.unsupported
	}
	if err := s.writable(st); err != nil {
		return Result{}, err
	}
	a := t.plan(f)
	var n int64
	assign := func(old []value.Value) ([]value.Value, error) {
		row := append([]value.Value{}, old...)
		for _, set := range sets {
			var err error
			if row[set.col], err = set.e.value(old); err != nil {
				return nil, err
			}
		}
		return row, nil
	}
	apply := func(key []byte, old []value.Value) (bool, error) {
		row, err := assign(old)
		if err != nil {
			return false, err
		}
		if err := s.writeRow(ctx, t, key, row); err != nil {
			return false, err
		}
		n++
		return true, nil
	}
	switch {
	case s.opts.Mutations.UpdateSeesOwnWrites:
		return Result{RowsAffected: n}, s.updateRestarting(ctx, t, a, f, apply, &n)
	case movesKeys:
		return s.updateKeys(ctx, t, a, f, assign)
	}
	if err := s.scanRows(ctx, t, a, f, apply); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, nil
}

// updateKeys runs an UPDATE that assigns primary key columns. Postgres checks the primary key
// of each row as it writes it, in the order it stores the rows, so when a row moves onto the key
// of a row that the same statement moves away, whether the statement fails depends on that
// order. Such a statement is rejected; every other outcome is the same in every order: a move
// onto a key that stays is a duplicate, two rows moving onto one key are a duplicate, and
// anything else succeeds.
func (s *Session) updateKeys(ctx context.Context, t *table, a access, f filter, assign func([]value.Value) ([]value.Value, error)) (Result, error) {
	type move struct {
		oldKey, newKey []byte
		row            []value.Value
	}
	var moves []move
	err := s.scanRows(ctx, t, a, f, func(key []byte, old []value.Value) (bool, error) {
		row, err := assign(old)
		if err != nil {
			return false, err
		}
		if err := t.checkRow(row); err != nil {
			return false, err
		}
		moves = append(moves, move{oldKey: append([]byte{}, key...), newKey: t.rowKey(row), row: row})
		return true, nil
	})
	if err != nil {
		return Result{}, err
	}
	vacated := map[string]bool{}
	for _, m := range moves {
		if string(m.newKey) != string(m.oldKey) {
			vacated[string(m.oldKey)] = true
		}
	}
	taken := map[string]bool{}
	for _, m := range moves {
		k := string(m.newKey)
		if taken[k] {
			return Result{}, fmt.Errorf("%w: table %q already has a row with key %s", ErrDuplicateKey, t.Name, keyDescription(t, m.row))
		}
		taken[k] = true
		if k == string(m.oldKey) {
			continue
		}
		if vacated[k] {
			return Result{}, fmt.Errorf("%w: whether this UPDATE fails in Postgres depends on the order it stores the rows in, because a row moves onto the key %s of a row that the statement moves away; update the rows in two statements", ErrUnsupported, keyDescription(t, m.row))
		}
		if _, exists, err := s.kv.get(ctx, m.newKey); err != nil {
			return Result{}, err
		} else if exists {
			return Result{}, fmt.Errorf("%w: table %q already has a row with key %s", ErrDuplicateKey, t.Name, keyDescription(t, m.row))
		}
	}
	for _, m := range moves {
		if string(m.newKey) != string(m.oldKey) {
			s.kv.del(m.oldKey)
		}
	}
	for _, m := range moves {
		s.kv.put(m.newKey, value.EncodeRow(nil, m.row))
	}
	return Result{RowsAffected: int64(len(moves))}, nil
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
	b := &binder{table: t, params: params, phase: phaseWhere}
	f, err := b.where(st.Where)
	if err != nil {
		return Result{}, err
	}
	// The planner needs the folded constants.
	if err := b.folding(); err != nil {
		return Result{}, err
	}
	if f.unsupported != nil {
		return Result{}, f.unsupported
	}
	if err := s.writable(st); err != nil {
		return Result{}, err
	}
	a := t.plan(f)
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
	// keyColumn is the index of the primary key column the item sorts by, or -1.
	keyColumn int
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
				return nil, fmt.Errorf("%w: SELECT * with no tables specified is not valid", ErrSyntax)
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
		// An output column of unknown type is text, as in Postgres.
		if e, err = resolve(e, value.String); err != nil {
			return nil, err
		}
		if e.typ == value.Numeric {
			return nil, fmt.Errorf("%w: the numeric type is not supported, cast the literal %s to double precision or bigint", ErrUnsupported, e.val.S)
		}
		items = append(items, e)
		itemExprs = append(itemExprs, item.Expr)
		col := Column{Name: item.Alias, Type: e.typ, Nullable: true}
		if col.Name == "" {
			col.Name = OutputName(item.Expr)
		}
		if ref, ok := item.Expr.(*ast.ColumnRef); ok {
			i, _ := t.columnIndex(ref.Name)
			col.Nullable = !t.Columns[i].NotNull
		}
		columns = append(columns, col)
	}

	b.phase = phaseWhere
	f, err := b.where(st.Where)
	if err != nil {
		return nil, err
	}

	// ORDER BY is checked after WHERE, but its expressions are output expressions, which
	// Postgres folds before WHERE.
	b.phase = phaseOutput
	var order []orderKey
	for _, o := range st.OrderBy {
		k := orderKey{output: -1, desc: o.Desc, keyColumn: -1}
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
			// An integer constant is an output position. Other constants are an error, as in
			// Postgres, which only takes a literal whose digits fit in 32 bits as a position, so
			// that -2147483648 is not one.
			switch {
			case e.Value.Null, e.Value.Type != value.Int4, e.Value.I == math.MinInt32:
				return nil, fmt.Errorf("%w: non-integer constant in ORDER BY", ErrSyntax)
			default:
				if e.Value.I < 1 || e.Value.I > int64(len(columns)) {
					return nil, fmt.Errorf("%w: ORDER BY position %d is not in select list", ErrInvalid, e.Value.I)
				}
				k.output = int(e.Value.I) - 1
			}
		case *ast.ColumnRef:
			// A name matches an output column first. Several output columns of that name are
			// only ambiguous when they are different expressions, as in Postgres.
			matches := -1
			for i, c := range columns {
				if c.Name == e.Name {
					if matches >= 0 && b.canonical(itemExprs[matches]) != b.canonical(itemExprs[i]) {
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
			if k.expr, err = resolve(k.expr, value.String); err != nil {
				return nil, err
			}
			if k.expr.typ == value.Numeric {
				return nil, fmt.Errorf("%w: the numeric type is not supported, cast the literal to double precision or bigint", ErrUnsupported)
			}
		}
		// The item sorts by a primary key column when it is that column, either directly or
		// through an output column that is a plain reference to it.
		sorted := o.Expr
		if k.output >= 0 {
			sorted = itemExprs[k.output]
		}
		if ref, ok := sorted.(*ast.ColumnRef); ok && t != nil {
			if i, ok := t.columnIndex(ref.Name); ok && t.isPrimaryKey(i) {
				k.keyColumn = i
			}
		}
		order = append(order, k)
	}

	// OFFSET is checked and folded before LIMIT, as in Postgres.
	b.phase = phaseOffset
	offsetExpr, err := b.limit(st.Offset, "OFFSET")
	if err != nil {
		return nil, err
	}
	b.phase = phaseLimit
	limitExpr, err := b.limit(st.Limit, "LIMIT")
	if err != nil {
		return nil, err
	}
	if err := b.folding(); err != nil {
		return nil, err
	}
	if f.unsupported != nil {
		return nil, f.unsupported
	}
	if (st.Limit != nil || st.Offset != nil) && t != nil {
		if err := limitable(t, order, items, f); err != nil {
			return nil, err
		}
	}
	// The planner needs the folded constants.
	var a access
	if t != nil {
		a = t.plan(f)
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
		return &Rows{Columns: columns, Rows: [][]value.Value{}}, nil
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

// limitable checks that a SELECT with LIMIT or OFFSET gives the same rows and the same errors
// in every plan Postgres could choose: the ORDER BY must sort by every primary key column, so
// that the row order is fully determined, and no output expression or WHERE condition may be
// one that can fail, because an index scan under a LIMIT evaluates them on fewer rows than a
// sequential scan and a sort.
func limitable(t *table, order []orderKey, items []*cexpr, f filter) error {
	for _, col := range t.PrimaryKey {
		found := false
		for _, k := range order {
			found = found || k.keyColumn == col
		}
		if !found {
			return fmt.Errorf("%w: LIMIT and OFFSET without an ORDER BY on every primary key column are not supported, because which rows Postgres returns is then not determined; add %s to ORDER BY", ErrUnsupported, t.Columns[col].Name)
		}
	}
	for _, e := range items {
		if e.fallible {
			return fmt.Errorf("%w: LIMIT and OFFSET with an output expression that can fail are not supported, because how many rows Postgres evaluates it on depends on its plan; select the rows first and compute the expression on them", ErrUnsupported)
		}
	}
	for _, c := range f.conds {
		if c.fallible {
			return fmt.Errorf("%w: LIMIT and OFFSET with a WHERE condition that can fail are not supported, because how many rows Postgres evaluates it on depends on its plan; select the rows first and filter them", ErrUnsupported)
		}
	}
	return nil
}

// limit binds a LIMIT or OFFSET expression, which must be a bigint that reads no columns, or
// convert to one as an assignment does. It returns nil when e is.
func (b *binder) limit(e ast.Expr, what string) (*cexpr, error) {
	if e == nil {
		return nil, nil
	}
	c, err := b.bind(e)
	if err != nil {
		return nil, err
	}
	if c.refs {
		return nil, fmt.Errorf("%w: argument of %s must not contain variables", ErrInvalid, what)
	}
	switch c.typ {
	case value.Unknown:
		return resolve(c, value.Int)
	case value.Numeric:
		return b.numericTo(c, value.Int)
	case value.Int4:
		return widen(c, value.Int), nil
	case value.Float:
		return b.cast(c, value.Int)
	case value.Int:
		return c, nil
	}
	return nil, fmt.Errorf("%w: argument of %s must be type bigint, not type %v", ErrType, what, c.typ)
}

// limitValue of a bound LIMIT or OFFSET expression whose folding succeeded. It returns -1 when
// the expression is absent or NULL.
func limitValue(c *cexpr, what string) (int64, error) {
	if c == nil {
		return -1, nil
	}
	// A stable expression is evaluated now, when the statement runs.
	v, err := c.value(nil)
	if err != nil || v.Null {
		return -1, err
	}
	if v.I < 0 {
		return 0, fmt.Errorf("%w: %s must not be negative", ErrInvalid, what)
	}
	return v.I, nil
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

// OutputName of a SELECT item without an alias, following Postgres: a column or function keeps
// its name, a CASE takes the name of its ELSE when that is a column or function and is "case"
// otherwise, a cast keeps the name of what it casts or takes the type's name, and anything else
// is "?column?".
func OutputName(e ast.Expr) string {
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
		return e.Type.OutputName(), 1
	}
	return "?column?", 0
}

// canonical text of an expression, for telling whether two expressions are the same one as
// Postgres's analysis sees them: casts are written in one style, and a cast to the type the
// expression already has is dropped, because Postgres drops it too.
func (b *binder) canonical(e ast.Expr) string {
	return ast.RenderExpr(ast.Map(e, func(x ast.Expr) ast.Expr {
		c, ok := x.(*ast.Cast)
		if !ok {
			return x
		}
		if inner, err := b.bindPruned(c.Expr); err == nil && inner.typ == c.Type {
			return c.Expr
		}
		return &ast.Cast{Expr: c.Expr, Type: c.Type}
	}))
}
