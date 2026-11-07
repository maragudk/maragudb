package exec

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// createTable runs CREATE TABLE. As in Postgres, IF NOT EXISTS skips a statement whose name is
// taken before anything else, and otherwise the definition is checked before the name, which
// tables and indexes share.
func (s *Session) createTable(ctx context.Context, st *ast.CreateTable) error {
	_, exists, err := s.relation(ctx, st.Name)
	if err != nil {
		return err
	}
	if exists && st.IfNotExists {
		return nil
	}
	t, err := newTable(st)
	if err != nil {
		return err
	}
	cs, err := t.indexConstraints(st)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: relation %q already exists", ErrTableExists, st.Name)
	}
	for i, c := range st.Columns {
		if c.Default == nil {
			continue
		}
		if t.Columns[i].Default, err = s.checkDefault(t, i, c.Default); err != nil {
			return err
		}
	}
	// Postgres creates the constraints in this order, and each takes a name that the ones before
	// it do not have.
	var n names
	if err := s.addChecks(ctx, t, st, &n); err != nil {
		return err
	}
	if err := s.addNotNullNames(ctx, t, &n); err != nil {
		return err
	}
	if err := s.putTable(t); err != nil {
		return err
	}
	if err := s.addIndexes(ctx, t, cs, &n); err != nil {
		return err
	}
	if err := s.addForeignKeys(ctx, t, st, &n); err != nil {
		return err
	}
	t.Constraints = n.list
	for _, name := range n.list {
		s.kv.put(constraintKey(name, t.Name), []byte(t.Name))
	}
	return s.putTable(t)
}

// dropTable runs DROP TABLE, which drops the table's indexes too.
func (s *Session) dropTable(ctx context.Context, st *ast.DropTable) error {
	t, exists, err := s.relation(ctx, st.Name)
	switch {
	case err != nil:
		return err
	case !exists && st.IfExists:
		return nil
	case !exists:
		return fmt.Errorf("%w: table %q does not exist", ErrNoTable, st.Name)
	case t.IndexOf != "":
		return fmt.Errorf("%w: %q is not a table, use DROP INDEX to remove an index", ErrWrongObjectType, st.Name)
	}
	if err := s.dropForeignKeys(ctx, t); err != nil {
		return err
	}
	start, end := t.rowRange()
	it := s.kv.scan(ctx, start, end)
	for it.Next() {
		s.kv.del(it.Key())
	}
	if err := it.Err(); err != nil {
		return err
	}
	for i := range t.Indexes {
		if err := s.deleteEntries(ctx, &t.Indexes[i]); err != nil {
			return err
		}
		s.deleteRelation(t.Indexes[i].Name)
	}
	if t.PrimaryKeyIndex != "" {
		s.deleteRelation(t.PrimaryKeyIndex)
	}
	for _, name := range t.Constraints {
		s.kv.del(constraintKey(name, t.Name))
	}
	s.deleteRelation(st.Name)
	return nil
}

func (s *Session) insert(ctx context.Context, st *ast.Insert, params []value.Value) (Result, error) {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return Result{}, err
	}
	cols := make([]int, 0, len(t.Columns))
	if st.DefaultValues {
		// One row in which every column takes its default.
	} else if len(st.Columns) == 0 {
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
	b := s.newBinder(ctx, params)
	b.aggClause = "VALUES"
	var bound [][]*cexpr
	rows := st.Rows
	width := 0
	if st.DefaultValues {
		rows = [][]ast.Expr{{}}
	}
	// defaults are the bound defaults of the columns, bound once, because they read no columns.
	defaults := make([]*cexpr, len(t.Columns))
	defaultOf := func(i int) (*cexpr, error) {
		if defaults[i] == nil {
			var err error
			if defaults[i], err = s.defaultExpr(b, t, i); err != nil {
				return nil, err
			}
		}
		return defaults[i], nil
	}
	// As in Postgres, each row is analyzed, checked against the first row's length and the
	// target columns, and converted to the column types before the next row.
	for _, exprRow := range rows {
		var es []*cexpr
		// isDefault marks the items that are DEFAULT, which are converted already.
		var isDefault []bool
		for i, e := range exprRow {
			_, ok := e.(*ast.Default)
			isDefault = append(isDefault, ok)
			if ok && i >= len(cols) {
				// Too many items, which the check below reports.
				es = append(es, constant(value.NewNull(value.Unknown)))
				continue
			}
			if ok {
				c, err := defaultOf(cols[i])
				if err != nil {
					return Result{}, err
				}
				es = append(es, c)
				continue
			}
			c, err := b.bind(e)
			if err != nil {
				return Result{}, err
			}
			es = append(es, c)
		}
		switch {
		case len(bound) > 0 && len(es) != width:
			return Result{}, fmt.Errorf("%w: VALUES lists must all be the same length", ErrSyntax)
		case len(es) > len(cols):
			return Result{}, fmt.Errorf("%w: INSERT has more expressions than target columns", ErrSyntax)
		case len(es) < len(cols) && len(st.Columns) > 0:
			return Result{}, fmt.Errorf("%w: INSERT has more target columns than expressions", ErrSyntax)
		}
		// Without a column list, the values go to the first columns, and the others take their
		// defaults.
		width = len(es)
		for i := range es {
			if isDefault[i] {
				continue
			}
			var err error
			if es[i], err = b.assignable(es[i], t, cols[i]); err != nil {
				return Result{}, err
			}
		}
		// The columns the statement leaves out take their defaults.
		full := make([]*cexpr, len(t.Columns))
		for i, c := range es {
			full[cols[i]] = c
		}
		for i := range full {
			if full[i] == nil {
				var err error
				if full[i], err = defaultOf(i); err != nil {
					return Result{}, err
				}
			}
		}
		bound = append(bound, full)
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
		for i, c := range es {
			var err error
			if row[i], err = c.value(nil); err != nil {
				return Result{}, err
			}
		}
		if err := s.writeRow(ctx, t, nil, nil, row); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(bound))}, s.firing(ctx)
}

// writeRow checks constraints and writes row and its index entries, replacing old, the row at
// oldKey, when given. As in Postgres, it checks NOT NULL, then the CHECK constraints, then the
// primary key, and then the unique indexes.
func (s *Session) writeRow(ctx context.Context, t *table, oldKey []byte, old, row []value.Value) error {
	if err := s.checkNewRow(t, row, oldKey != nil); err != nil {
		return err
	}
	key := t.rowKey(row)
	wrote := oldKey != nil && s.kv.wrote(oldKey)
	if oldKey == nil || string(key) != string(oldKey) {
		if _, exists, err := s.kv.get(ctx, key); err != nil {
			return err
		} else if exists {
			return keyDuplicate(t, row)
		}
		if oldKey != nil {
			s.kv.del(oldKey)
		}
	}
	if err := s.writeEntries(ctx, t, old, row); err != nil {
		return err
	}
	s.kv.put(key, value.EncodeRow(nil, row))
	s.queue(rowEvent{t: t, oldKey: oldKey, key: key, old: old, row: row, wrote: wrote})
	return nil
}

// checkNewRow checks the row that an insert, or an update when update is set, writes, against
// the NOT NULL constraints and then the CHECK constraints of its table, before the keys.
func (s *Session) checkNewRow(t *table, row []value.Value, update bool) error {
	if err := t.checkRow(row); err != nil {
		return err
	}
	if update && s.opts.Mutations.CheckSkipUpdate {
		return nil
	}
	return s.checkConstraints(t, row)
}

// keyDuplicate is the error of a row whose primary key another row has.
func keyDuplicate(t *table, row []value.Value) error {
	return fmt.Errorf("%w: duplicate key value violates unique constraint %q: table %q already has a row with key %s", ErrDuplicateKey, t.PrimaryKeyIndex, t.Name, keyDescription(t, row))
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
// filter admit, in key order, or in the order of an index's entries for an ordered access,
// until fn returns false.
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
	match := func(key []byte, row []value.Value) (bool, error) {
		ok, err := f.matches(row)
		if errors.Is(err, errOrderDependent) {
			if orderDependent == nil {
				orderDependent = err
			}
			return true, nil
		}
		if err != nil && f.failures != nil {
			*f.failures = append(*f.failures, err)
			return true, nil
		}
		if err != nil || !ok {
			return err == nil, err
		}
		return fn(key, row)
	}
	if t.derived != nil {
		rows, err := t.derived.get()
		if err != nil {
			return err
		}
		for _, row := range rows {
			if more, err := match(nil, row); err != nil || !more {
				return err
			}
		}
		return orderDependent
	}
	visit := func(key, val []byte) (bool, error) {
		if s.opts.Mutations.PanicInScan {
			panic(fmt.Sprintf("exec: a scan of %q panics", t.Name))
		}
		row, err := value.DecodeRow(val, types)
		if err != nil {
			return false, fmt.Errorf("%w: table %q: %w", engine.ErrCorrupt, t.Name, err)
		}
		return match(key, row)
	}
	if a.point != nil {
		get := s.kv.get
		if s.snapshotReads > 0 {
			get = s.kv.tx.Get
		}
		val, ok, err := get(ctx, a.point)
		if err != nil || !ok {
			return err
		}
		if _, err = visit(a.point, val); err != nil {
			return err
		}
		return orderDependent
	}
	// The rows that an index finds are read from the transaction, like the rows of a scan, and
	// not from the statement's own writes.
	fetch := func(key []byte, fromIndex bool) (bool, error) {
		val, ok, err := s.kv.tx.Get(ctx, key)
		switch {
		case err != nil:
			return false, err
		case !ok && fromIndex:
			return false, fmt.Errorf("exec: %w: an entry of an index of %q points to the missing row %x", ErrIndexInconsistent, t.Name, key)
		case !ok:
			return true, nil
		}
		return visit(key, val)
	}
	if a.index == nil && a.union == nil || a.ordered {
		// A range of rows, or of the entries of an index, in order.
		it := s.kv.scan(ctx, a.start, a.end)
		for it.Next() {
			var more bool
			var err error
			if a.index == nil {
				more, err = visit(it.Key(), it.Value())
			} else {
				more, err = fetch(t.rowKeyOf(it.Value()), true)
			}
			if err != nil || !more {
				return err
			}
		}
		if err := it.Err(); err != nil {
			return err
		}
		return orderDependent
	}
	keys, err := s.rowKeys(ctx, t, a)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if more, err := fetch([]byte(k.key), k.fromIndex); err != nil || !more {
			return err
		}
	}
	return orderDependent
}

// rowKey of a row that an access reads, and whether an index entry points to it.
type rowKey struct {
	key       string
	fromIndex bool
}

// rowKeys that an access reads, in key order and each once.
func (s *Session) rowKeys(ctx context.Context, t *table, a access) ([]rowKey, error) {
	fromIndex := map[string]bool{}
	var add func(a access) error
	add = func(a access) error {
		switch {
		case a.empty:
		case a.union != nil:
			for _, u := range a.union {
				if err := add(u); err != nil {
					return err
				}
			}
		case a.point != nil:
			fromIndex[string(a.point)] = false
		default:
			it := s.kv.scan(ctx, a.start, a.end)
			for it.Next() {
				key := string(it.Key())
				if a.index != nil {
					key = string(t.rowKeyOf(it.Value()))
				}
				prev, seen := fromIndex[key]
				fromIndex[key] = a.index != nil && (!seen || prev)
			}
			return it.Err()
		}
		return nil
	}
	if err := add(a); err != nil {
		return nil, err
	}
	keys := make([]rowKey, 0, len(fromIndex))
	for k, f := range fromIndex {
		keys = append(keys, rowKey{key: k, fromIndex: f})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].key < keys[j].key })
	return keys, nil
}

func (s *Session) update(ctx context.Context, st *ast.Update, params []value.Value) (Result, error) {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return Result{}, err
	}
	b := s.newBinder(ctx, params)
	b.table, b.aggClause = t, "UPDATE"
	b.scope, b.ns = singleScope(t, cmp.Or(st.Alias, t.Name))
	type assignment struct {
		col int
		e   *cexpr
		// foldErr and stableErr are what folding the expression raised.
		foldErr, stableErr error
	}
	// Postgres analyzes WHERE first, then every SET expression, and then converts each to its
	// column's type. It folds the SET expressions before WHERE, in the order of their columns,
	// into which its rewriter sorts them.
	b.phase = phaseWhere
	f, err := b.where(st.Where, "WHERE")
	if err != nil {
		return Result{}, err
	}
	b.phase = phaseOutput
	values := make([]*cexpr, len(st.Set))
	binders := make([]*binder, len(st.Set))
	for i, a := range st.Set {
		binders[i] = b.fresh()
		if _, ok := a.Value.(*ast.Default); ok {
			// Bound with its column below.
			continue
		}
		if values[i], err = binders[i].bind(a.Value); err != nil {
			return Result{}, err
		}
	}
	var sets []assignment
	movesKeys := false
	for j, a := range st.Set {
		if a.Qualifier != "" {
			// Postgres reads x.c as the field c of the column x, and no column has fields.
			if i, ok := t.columnIndex(a.Qualifier); ok {
				return Result{}, fmt.Errorf("%w: cannot assign to field %q of column %q because its type %v is not a composite type", ErrType, a.Column, a.Qualifier, t.Columns[i].Type)
			}
			return Result{}, fmt.Errorf("%w: column %q of relation %q does not exist", ErrInvalid, a.Qualifier, t.Name)
		}
		i, ok := t.columnIndex(a.Column)
		if !ok {
			return Result{}, fmt.Errorf("%w: column %q of relation %q does not exist", ErrInvalid, a.Column, t.Name)
		}
		var e *cexpr
		if values[j] == nil {
			e, err = s.defaultExpr(b, t, i)
		} else {
			e, err = binders[j].assignable(values[j], t, i)
		}
		if err != nil {
			return Result{}, err
		}
		sets = append(sets, assignment{col: i, e: e, foldErr: binders[j].foldErr, stableErr: binders[j].stableErr})
		movesKeys = movesKeys || t.constrains(i)
	}
	seen := map[int]bool{}
	for _, set := range sets {
		if seen[set.col] {
			return Result{}, fmt.Errorf("%w: multiple assignments to same column %q", ErrSyntax, t.Columns[set.col].Name)
		}
		seen[set.col] = true
	}
	byColumn := slices.Clone(sets)
	slices.SortStableFunc(byColumn, func(x, y assignment) int { return cmp.Compare(x.col, y.col) })
	for _, set := range byColumn {
		if set.foldErr != nil && (b.foldErr == nil || b.foldPhase > phaseOutput) {
			b.foldErr, b.foldPhase = set.foldErr, phaseOutput
		}
		b.stableErr = cmp.Or(b.stableErr, set.stableErr)
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
	f.candidates = t.candidates(f, nil, nil)
	a := t.plan(f, nil)
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
		if err := s.writeRow(ctx, t, key, old, row); err != nil {
			return false, err
		}
		n++
		return true, nil
	}
	switch {
	case s.opts.Mutations.UpdateSeesOwnWrites:
		if err := s.updateRestarting(ctx, t, a, f, apply, &n); err != nil {
			return Result{}, err
		}
		return Result{RowsAffected: n}, s.firing(ctx)
	case movesKeys:
		res, err := s.updateKeys(ctx, t, a, f, assign)
		if err != nil {
			return Result{}, err
		}
		return res, s.firing(ctx)
	}
	if err := s.scanRows(ctx, t, a, f, apply); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, s.firing(ctx)
}

// updateKeys runs an UPDATE that assigns columns of the primary key or of a unique index, each a
// constraint whose keys no two rows may share. Postgres checks the constraints of each row as it
// writes it, in the order it visits the rows, so when a row moves onto the key of a row that the
// same statement moves away, whether the statement fails depends on that order. Such a
// statement is rejected; every other outcome is the same in every order: a move onto a key
// that stays is a duplicate, two rows moving onto one key are a duplicate, and anything else
// succeeds. When a row's new values fail, or the WHERE clause fails on a row, the statement fails
// in every order, and the error is the one Postgres reports when it visits the rows in key order,
// which can be a duplicate key of a row before.
func (s *Session) updateKeys(ctx context.Context, t *table, a access, f filter, assign func([]value.Value) ([]value.Value, error)) (Result, error) {
	type move struct {
		oldKey   []byte
		old, row []value.Value
		err      error
	}
	var moves []move
	failed := false
	err := s.scanRows(ctx, t, a, f, func(key []byte, old []value.Value) (bool, error) {
		m := move{oldKey: append([]byte{}, key...), old: old}
		if m.row, m.err = assign(old); m.err == nil {
			m.err = s.checkNewRow(t, m.row, true)
		}
		failed = failed || m.err != nil
		moves = append(moves, m)
		return true, nil
	})
	switch {
	case err != nil && !errors.Is(err, errOrderDependent) && len(moves) > 0:
		// The scan stopped at a row that fails; the rows before it are visited first.
		moves = append(moves, move{err: err})
		failed = true
	case err != nil && (!failed || !errors.Is(err, errOrderDependent)):
		return Result{}, err
	}
	cs := t.keyConstraints(s.opts.Mutations.UniqueNoCheck)
	matched := map[string]bool{}
	for _, m := range moves {
		matched[string(m.oldKey)] = true
	}
	if failed {
		// Visit the rows in key order as Postgres would, checking each new key against the keys
		// of the rows not visited yet and the new keys of those visited.
		live := make([]map[string]bool, len(cs))
		for i, c := range cs {
			live[i] = map[string]bool{}
			for _, m := range moves {
				// A row holds its old key until it is visited, also when it then fails.
				if m.old != nil {
					if k, ok := c.key(t, m.old); ok {
						live[i][k] = true
					}
				}
			}
		}
		for _, m := range moves {
			if m.err != nil {
				return Result{}, m.err
			}
			for i, c := range cs {
				k, ok := c.key(t, m.row)
				old, oldOK := c.key(t, m.old)
				if ok && oldOK && k == old {
					continue
				}
				if ok {
					holder, err := s.keyHolder(ctx, t, c, k)
					if err != nil {
						return Result{}, err
					}
					if live[i][k] || holder != "" && !matched[holder] {
						return Result{}, c.duplicate(t, m.row)
					}
					live[i][k] = true
				}
				if oldOK {
					delete(live[i], old)
				}
			}
		}
		panic("exec: a failed UPDATE row went unreported")
	}
	// A duplicate in every order comes before an outcome that depends on the order. A row that
	// moves onto the key of a row that moves away needs that row visited first, so rows whose
	// moves form a cycle are a duplicate in every order, and a chain of moves succeeds only in
	// the right order.
	var orderDependent error
	for _, c := range cs {
		// leaving maps the old key of every row that leaves it to the row.
		leaving := map[string]int{}
		for i, m := range moves {
			old, oldOK := c.key(t, m.old)
			if k, ok := c.key(t, m.row); oldOK && (!ok || k != old) {
				leaving[old] = i
			}
		}
		taken := map[string]bool{}
		// next is the row whose old key a row moves onto.
		next := map[int]int{}
		for i, m := range moves {
			k, ok := c.key(t, m.row)
			if !ok {
				continue
			}
			if taken[k] {
				return Result{}, c.duplicate(t, m.row)
			}
			taken[k] = true
			if old, oldOK := c.key(t, m.old); oldOK && k == old {
				continue
			}
			if j, ok := leaving[k]; ok {
				next[i] = j
				continue
			}
			if holder, err := s.keyHolder(ctx, t, c, k); err != nil {
				return Result{}, err
			} else if holder != "" {
				return Result{}, c.duplicate(t, m.row)
			}
		}
		if i, ok := cycle(next); ok {
			return Result{}, c.duplicate(t, moves[i].row)
		}
		if len(next) > 0 && orderDependent == nil {
			for i := range moves {
				if _, ok := next[i]; ok {
					orderDependent = fmt.Errorf("%w: whether this UPDATE fails in Postgres depends on the order it visits the rows in, because a row moves onto the key %s of a row that the statement moves away; update the rows in two statements", ErrUnsupported, c.description(t, moves[i].row))
					break
				}
			}
		}
	}
	if orderDependent != nil {
		return Result{}, orderDependent
	}
	wrote := make([]bool, len(moves))
	for i, m := range moves {
		wrote[i] = s.kv.wrote(m.oldKey)
	}
	for _, m := range moves {
		if string(t.rowKey(m.row)) != string(m.oldKey) {
			s.kv.del(m.oldKey)
		}
		if s.opts.Mutations.IndexStaleDelete {
			continue
		}
		for i := range t.Indexes {
			old, _ := t.Indexes[i].entry(t, m.old)
			if k, _ := t.Indexes[i].entry(t, m.row); !bytes.Equal(k, old) {
				s.kv.del(old)
			}
		}
	}
	for j, m := range moves {
		pk := t.appendKey(nil, m.row)
		key := t.rowKeyOf(pk)
		s.kv.put(key, value.EncodeRow(nil, m.row))
		for i := range t.Indexes {
			k, _ := t.Indexes[i].entry(t, m.row)
			s.kv.put(k, pk)
		}
		s.queue(rowEvent{t: t, oldKey: m.oldKey, key: key, old: m.old, row: m.row, wrote: wrote[j]})
	}
	return Result{RowsAffected: int64(len(moves))}, nil
}

// cycle returns a node on a cycle of next, a graph in which every node has at most one
// successor and at most one predecessor, if there is a cycle.
func cycle(next map[int]int) (int, bool) {
	const (
		unvisited = iota
		onPath
		done
	)
	state := map[int]int{}
	for _, start := range slices.Sorted(maps.Keys(next)) {
		var path []int
		i := start
		for state[i] == unvisited {
			state[i] = onPath
			path = append(path, i)
			j, ok := next[i]
			if !ok {
				break
			}
			i = j
		}
		if state[i] == onPath && len(path) > 0 {
			if _, ok := next[path[len(path)-1]]; ok {
				return i, true
			}
		}
		for _, p := range path {
			state[p] = done
		}
	}
	return 0, false
}

// keyConstraint is the primary key, when index is nil, or a unique index of a table.
type keyConstraint struct {
	index *index
}

// keyConstraints of a table: the primary key, and the unique indexes unless skipUnique.
func (t *table) keyConstraints(skipUnique bool) []keyConstraint {
	cs := []keyConstraint{{}}
	for i := range t.Indexes {
		if t.Indexes[i].Unique && !skipUnique {
			cs = append(cs, keyConstraint{index: &t.Indexes[i]})
		}
	}
	return cs
}

// key of a row under the constraint, which no other row may have, or false when the row has a
// NULL in a unique index whose NULLs are distinct, so that it never collides.
func (c keyConstraint) key(t *table, row []value.Value) (string, bool) {
	if c.index == nil {
		return string(t.rowKey(row)), true
	}
	k, unique := c.index.entry(t, row)
	return string(k), unique
}

func (c keyConstraint) duplicate(t *table, row []value.Value) error {
	if c.index == nil {
		return keyDuplicate(t, row)
	}
	return duplicate(t, c.index, row)
}

func (c keyConstraint) description(t *table, row []value.Value) string {
	if c.index == nil {
		return keyDescription(t, row)
	}
	return c.index.description(t, row)
}

// keyHolder is the row key of the row that has the key of the constraint in the transaction, or
// "" when no row has it.
func (s *Session) keyHolder(ctx context.Context, t *table, c keyConstraint, key string) (string, error) {
	v, exists, err := s.kv.get(ctx, []byte(key))
	switch {
	case err != nil || !exists:
		return "", err
	case c.index == nil:
		return key, nil
	}
	return string(t.rowKeyOf(v)), nil
}

// updateRestarting is the mutated UPDATE: it applies each write to the transaction at once and
// restarts the scan after the key it just visited, so it can see and revisit its own writes.
func (s *Session) updateRestarting(ctx context.Context, t *table, a access, f filter, apply func([]byte, []value.Value) (bool, error), n *int64) error {
	if a.point != nil {
		return s.scanRows(ctx, t, a, f, apply)
	}
	if a.index != nil || a.union != nil {
		// Restarting needs a range of keys; the filter still selects the rows.
		start, end := t.rowRange()
		a = access{start: start, end: end}
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
	b := s.newBinder(ctx, params)
	b.table, b.phase, b.aggClause = t, phaseWhere, "WHERE"
	b.scope, b.ns = singleScope(t, cmp.Or(st.Alias, t.Name))
	f, err := b.where(st.Where, "WHERE")
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
	if err := s.checkActionOrder(ctx, t); err != nil {
		return Result{}, err
	}
	f.candidates = t.candidates(f, nil, nil)
	a := t.plan(f, nil)
	var n int64
	err = s.scanRows(ctx, t, a, f, func(key []byte, row []value.Value) (bool, error) {
		s.kv.del(key)
		s.deleteRowEntries(t, row)
		s.queue(rowEvent{t: t, oldKey: key, old: row})
		n++
		return true, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, s.firing(ctx)
}

// orderKey is one ORDER BY item, bound either to an output column or to an expression over the
// input row.
type orderKey struct {
	output int
	expr   *cexpr
	desc   bool
	// nullsFirst after applying the Postgres default: last when ascending, first when descending.
	nullsFirst bool
	// column is the index of the column the item sorts by, when it is a plain column of the one
	// table of the statement, or -1.
	column int
	// ref is the column the item sorts by, of any entry, when plain is set.
	ref   colRef
	plain bool
}

// fallible reports whether evaluating a SELECT on a row can fail, in its WHERE clause, its
// output, or its ORDER BY, so that which row fails first depends on the order of the rows.
func fallible(f filter, items []*cexpr, order []orderKey) bool {
	if f.fallible {
		return true
	}
	for _, e := range items {
		if e.fallible {
			return true
		}
	}
	for _, k := range order {
		if k.expr != nil && k.expr.fallible {
			return true
		}
	}
	return false
}

// limitable checks that a SELECT with LIMIT or OFFSET gives the same rows and the same errors
// in every plan Postgres could choose: the ORDER BY must sort by every primary key column, so
// that the row order is fully determined, and no output expression or WHERE condition may be
// one that can fail, because an index scan under a LIMIT evaluates them on fewer rows than a
// sequential scan and a sort.
func limitable(t *table, order []orderKey, items []*cexpr, f filter, distinct bool, outputs int) error {
	if err := t.derivedLimit(); err != nil {
		return err
	}
	if !distinct || !sortsByEveryOutput(order, outputs) {
		for _, col := range t.PrimaryKey {
			found := false
			for _, k := range order {
				found = found || k.column == col
			}
			if !found {
				return fmt.Errorf("%w: LIMIT and OFFSET without an ORDER BY on every primary key column are not supported, because which rows Postgres returns is then not determined; add %s to ORDER BY", ErrUnsupported, t.Columns[col].Name)
			}
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

// sortsByEveryOutput reports whether the ORDER BY sorts by every output column, which determines
// the order of the rows of a SELECT DISTINCT completely, because no two of them are equal.
func sortsByEveryOutput(order []orderKey, outputs int) bool {
	for i := range outputs {
		if !slices.ContainsFunc(order, func(k orderKey) bool { return k.output == i }) {
			return false
		}
	}
	return true
}

// limit binds a LIMIT or OFFSET expression, which must be a bigint that reads no columns, or
// convert to one as an assignment does. It returns nil when e is.
func (b *binder) limit(e ast.Expr, what string) (*cexpr, error) {
	if e == nil {
		return nil, nil
	}
	found := false
	ast.SubqueriesOf(e, func(*ast.Select) { found = true })
	if found {
		return nil, fmt.Errorf("%w: a subquery in %s is not supported, write a constant or a parameter", ErrUnsupported, what)
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
	name, _ := outputNameStrength(e, nil)
	return name
}

// outputNameStrength is the name and how good it is: 2 for a column or function name, 1 for a
// generic name, and 0 for none. A scalar subquery has the name of its column, which names has
// when its query is bound, and which is otherwise the name of its first output expression.
func outputNameStrength(e ast.Expr, names map[*ast.Select]string) (string, int) {
	switch e := e.(type) {
	case *ast.ColumnRef:
		return e.Name, 2
	case *ast.Call:
		return e.Name, 2
	case *ast.Case:
		if e.Else != nil {
			if name, strength := outputNameStrength(e.Else, names); strength > 1 {
				return name, strength
			}
		}
		return "case", 1
	case *ast.Cast:
		if name, strength := outputNameStrength(e.Expr, names); strength > 1 {
			return name, strength
		}
		return e.Type.OutputName(), 1
	case *ast.Subquery:
		switch e.Kind {
		case ast.SubExists:
			return "exists", 2
		case ast.SubScalar:
			if name, ok := names[e.Select]; ok {
				return name, 2
			}
			if len(e.Select.Items) > 0 && !e.Select.Items[0].Star {
				if e.Select.Items[0].Alias != "" {
					return e.Select.Items[0].Alias, 2
				}
				name, _ := outputNameStrength(e.Select.Items[0].Expr, names)
				return name, 2
			}
		}
	}
	return "?column?", 0
}

// canonical text of an expression, for telling whether two expressions are the same one as
// Postgres's analysis sees them: casts are written in one style, and a cast to the type the
// expression already has is dropped, because Postgres drops it too. A quoted literal cast to a
// type is written as the value it reads into, and so is a constant of type boolean or bigint,
// because Postgres reads each of them into a constant of that type and value: FALSE and
// 'no'::boolean are one expression there, and so are 9223372036854775807 and its quoted text cast
// to bigint. An integer constant has no other spelling, because integer is no type name here.
func (b *binder) canonical(e ast.Expr) string {
	constant := func(v value.Value) ast.Expr {
		return &ast.Cast{Expr: &ast.Literal{Value: value.NewUnknown(value.Format(v))}, Type: v.Type}
	}
	return ast.RenderExpr(ast.Map(b.normalize(e), func(x ast.Expr) ast.Expr {
		if lit, ok := x.(*ast.Literal); ok && !lit.Value.Null && (lit.Value.Type == value.Bool || lit.Value.Type == value.Int) {
			return constant(lit.Value)
		}
		c, ok := x.(*ast.Cast)
		if !ok {
			return x
		}
		if lit, ok := c.Expr.(*ast.Literal); ok && lit.Value.Type == value.Unknown && !lit.Value.Null {
			if v, err := value.Parse(lit.Value.S, c.Type); err == nil {
				return constant(v)
			}
		}
		if inner, err := b.bindPruned(c.Expr); err == nil && inner.typ == c.Type {
			return c.Expr
		}
		return &ast.Cast{Expr: c.Expr, Type: c.Type}
	}))
}
