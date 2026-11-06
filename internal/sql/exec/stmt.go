package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

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
	if err := s.putTable(t); err != nil {
		return err
	}
	return s.addIndexes(ctx, t, cs)
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
	s.deleteRelation(st.Name)
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
		if err := s.writeRow(ctx, t, nil, nil, row); err != nil {
			return Result{}, err
		}
	}
	return Result{RowsAffected: int64(len(st.Rows))}, nil
}

// writeRow checks constraints and writes row and its index entries, replacing old, the row at
// oldKey, when given. As in Postgres, it checks NOT NULL, then the primary key, and then the
// unique indexes.
func (s *Session) writeRow(ctx context.Context, t *table, oldKey []byte, old, row []value.Value) error {
	if err := t.checkRow(row); err != nil {
		return err
	}
	key := t.rowKey(row)
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
	return nil
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
		movesKeys = movesKeys || t.constrains(i)
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
		return Result{RowsAffected: n}, s.updateRestarting(ctx, t, a, f, apply, &n)
	case movesKeys:
		return s.updateKeys(ctx, t, a, f, assign)
	}
	if err := s.scanRows(ctx, t, a, f, apply); err != nil {
		return Result{}, err
	}
	return Result{RowsAffected: n}, nil
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
			m.err = t.checkRow(m.row)
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
	for _, m := range moves {
		pk := t.appendKey(nil, m.row)
		s.kv.put(t.rowKeyOf(pk), value.EncodeRow(nil, m.row))
		for i := range t.Indexes {
			k, _ := t.Indexes[i].entry(t, m.row)
			s.kv.put(k, pk)
		}
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
	f.candidates = t.candidates(f, nil, nil)
	a := t.plan(f, nil)
	var n int64
	err = s.scanRows(ctx, t, a, f, func(key []byte, row []value.Value) (bool, error) {
		s.kv.del(key)
		s.deleteRowEntries(t, row)
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
	// column is the index of the column the item sorts by, when it is a plain column, or -1.
	column int
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
		k := orderKey{output: -1, desc: o.Desc, column: -1}
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
		// The item sorts by a column when it is that column, either directly or through an
		// output column that is a plain reference to it.
		sorted := o.Expr
		if k.output >= 0 {
			sorted = itemExprs[k.output]
		}
		if ref, ok := sorted.(*ast.ColumnRef); ok && t != nil {
			if i, ok := t.columnIndex(ref.Name); ok {
				k.column = i
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
		var sortCols []int
		for _, k := range order {
			if k.column >= 0 {
				sortCols = append(sortCols, k.column)
			}
		}
		f.candidates = t.candidates(f, sortCols, t.reads(st))
		var stream *scan
		if !fallible(f, items, order) {
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
		return &Rows{Columns: columns, Rows: [][]value.Value{}}, nil
	}

	// The rows are sorted after they are read, unless the access reads them in order.
	sorting := len(order) > 0 && !a.ordered
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
		if !sorting && limit >= 0 && int64(len(out)) >= limit {
			return false, nil
		}
		o, err := project(row)
		if err != nil {
			return false, err
		}
		if sorting {
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
	if sorting {
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

// reads are the columns of t that a SELECT reads, by name, in its output, WHERE, and ORDER BY.
func (t *table) reads(st *ast.Select) []int {
	var out []int
	add := func(e ast.Expr) {
		ast.Walk(e, func(x ast.Expr) bool {
			if ref, ok := x.(*ast.ColumnRef); ok {
				if i, ok := t.columnIndex(ref.Name); ok && !slices.Contains(out, i) {
					out = append(out, i)
				}
			}
			return true
		})
	}
	for _, item := range st.Items {
		if item.Star {
			for i := range t.Columns {
				if !slices.Contains(out, i) {
					out = append(out, i)
				}
			}
			continue
		}
		add(item.Expr)
	}
	add(st.Where)
	for _, o := range st.OrderBy {
		add(o.Expr)
	}
	return out
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
func limitable(t *table, order []orderKey, items []*cexpr, f filter) error {
	for _, col := range t.PrimaryKey {
		found := false
		for _, k := range order {
			found = found || k.column == col
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
