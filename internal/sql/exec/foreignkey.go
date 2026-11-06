package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// foreignKey is a FOREIGN KEY constraint of the table that has it, the child: every row whose
// Columns have no NULL has a row in the table Parent with the same values in RefColumns, which
// is MATCH SIMPLE.
type foreignKey struct {
	Name       string `json:"name"`
	Columns    []int  `json:"columns"`
	Parent     string `json:"parent"`
	RefColumns []int  `json:"refColumns"`
	// Index is the name of the parent's index on RefColumns that the key depends on: its primary
	// key index or a unique index.
	Index    string     `json:"index"`
	OnDelete ast.Action `json:"onDelete,omitempty"`
	OnUpdate ast.Action `json:"onUpdate,omitempty"`
	// SetColumns are the columns that ON DELETE SET NULL or SET DEFAULT sets, or empty for all of
	// Columns.
	SetColumns []int `json:"setColumns,omitempty"`
}

// reference to a table by the foreign key Name of the table Table, which can be the table itself.
type reference struct {
	Table string `json:"table"`
	Name  string `json:"name"`
}

// setColumns of the key's ON DELETE SET NULL or SET DEFAULT.
func (fk *foreignKey) setColumns() []int {
	if len(fk.SetColumns) > 0 {
		return fk.SetColumns
	}
	return fk.Columns
}

// foreignKey of the table by name.
func (t *table) foreignKey(name string) (*foreignKey, bool) {
	for i := range t.ForeignKeys {
		if t.ForeignKeys[i].Name == name {
			return &t.ForeignKeys[i], true
		}
	}
	return nil, false
}

// values of the columns of row, and whether none is NULL.
func values(row []value.Value, columns []int) ([]value.Value, bool) {
	out := make([]value.Value, len(columns))
	complete := true
	for i, c := range columns {
		out[i] = row[c]
		complete = complete && !row[c].Null
	}
	return out, complete
}

// sameValues reports whether two lists of values without NULLs are equal, by their key
// encoding, which is how they compare for the types a foreign key can have.
func sameValues(a, b []value.Value) bool {
	for i := range a {
		if a[i].Null || b[i].Null || !bytes.Equal(value.EncodeKey(nil, a[i]), value.EncodeKey(nil, b[i])) {
			return false
		}
	}
	return true
}

// addForeignKeys checks the FOREIGN KEY constraints of a new table, in the order they are
// written, as Postgres does when it adds them after the table and its indexes: first the name,
// then the referenced table, the columns, the columns of SET NULL and SET DEFAULT, the referenced
// columns and their unique index, and the types. Each one references the table it names, which
// records the reference.
func (s *Session) addForeignKeys(ctx context.Context, t *table, st *ast.CreateTable, n *names) error {
	for _, f := range st.ForeignKeys {
		name := f.Name
		switch {
		case name != "" && strings.IndexByte(name, 0) >= 0:
			return fmt.Errorf("%w: constraint name must not contain a NUL byte", ErrInvalid)
		case name != "":
			if n.has(name) {
				return fmt.Errorf("%w: constraint %q for relation %q already exists", ErrDuplicateObject, name, t.Name)
			}
		default:
			var err error
			if name, err = s.chooseConstraintName(ctx, n, t.Name, foreignKeyNameAddition(f.Columns), "fkey"); err != nil {
				return err
			}
		}
		parent := t
		if f.Table != t.Name {
			var err error
			if parent, err = s.getTable(ctx, f.Table); err != nil {
				return err
			}
		}
		fk := foreignKey{Name: name, Parent: parent.Name, OnDelete: f.OnDelete, OnUpdate: f.OnUpdate}
		columnsOf := func(tt *table, names []string) ([]int, error) {
			var out []int
			for _, c := range names {
				i, ok := tt.columnIndex(c)
				if !ok {
					return nil, fmt.Errorf("%w: column %q referenced in foreign key constraint does not exist", ErrInvalid, c)
				}
				out = append(out, i)
			}
			return out, nil
		}
		var err error
		if fk.Columns, err = columnsOf(t, f.Columns); err != nil {
			return err
		}
		set, err := columnsOf(t, f.SetColumns)
		if err != nil {
			return err
		}
		for _, c := range set {
			if !slices.Contains(fk.Columns, c) {
				return fmt.Errorf("%w: column %q referenced in ON DELETE SET action must be part of foreign key", ErrInvalid, t.Columns[c].Name)
			}
			if !slices.Contains(fk.SetColumns, c) {
				fk.SetColumns = append(fk.SetColumns, c)
			}
		}
		if len(f.RefColumns) == 0 {
			fk.RefColumns, fk.Index = parent.PrimaryKey, parent.PrimaryKeyIndex
		} else {
			if fk.RefColumns, err = columnsOf(parent, f.RefColumns); err != nil {
				return err
			}
			if fk.Index, err = parent.uniqueIndexOn(fk.RefColumns); err != nil {
				return err
			}
		}
		if len(fk.Columns) != len(fk.RefColumns) {
			return fmt.Errorf("%w: number of referencing and referenced columns for foreign key disagree", ErrInvalidForeignKey)
		}
		for i, c := range fk.Columns {
			ct, pt := t.Columns[c].Type, parent.Columns[fk.RefColumns[i]].Type
			switch {
			case ct == pt && ct != value.Float:
			case ct == pt, ct == value.Int && pt == value.Float:
				return fmt.Errorf("%w: a foreign key on double precision columns is not supported, because which values it finds equal depends on how Postgres compares them; reference a bigint or a text column", ErrUnsupported)
			default:
				return fmt.Errorf("%w: foreign key constraint %q cannot be implemented: key columns %q of the referencing table and %q of the referenced table are of incompatible types: %v and %v", ErrType, name, t.Columns[c].Name, parent.Columns[fk.RefColumns[i]].Name, ct, pt)
			}
		}
		n.add(name)
		t.ForeignKeys = append(t.ForeignKeys, fk)
		ref := reference{Table: t.Name, Name: name}
		if parent == t {
			t.ReferencedBy = append(t.ReferencedBy, ref)
			continue
		}
		parent = parent.clone()
		parent.ReferencedBy = append(parent.ReferencedBy, ref)
		if err := s.putTable(parent); err != nil {
			return err
		}
	}
	return nil
}

// foreignKeyNameAddition is the part of a generated foreign key name that names its columns, as
// Postgres's ChooseForeignKeyConstraintNameAddition makes it: the names joined with underscores,
// which stops once it is longer than [maxNameBytes].
func foreignKeyNameAddition(columns []string) string {
	var b strings.Builder
	for _, name := range columns {
		if b.Len() > 0 {
			b.WriteByte('_')
		}
		b.WriteString(clip(name, maxNameBytes))
		if b.Len() > maxNameBytes {
			break
		}
	}
	return b.String()
}

// uniqueIndexOn the columns, in any order, which a foreign key can reference: the first such one
// in the order the indexes were created, the primary key first, as Postgres chooses it.
func (t *table) uniqueIndexOn(columns []int) (string, error) {
	for i, c := range columns {
		if slices.Contains(columns[:i], c) {
			return "", fmt.Errorf("%w: foreign key referenced-columns list must not contain duplicates", ErrInvalidForeignKey)
		}
	}
	sameSet := func(x *index) bool {
		if len(x.Columns) != len(columns) {
			return false
		}
		for _, c := range x.Columns {
			if !slices.Contains(columns, c.Column) {
				return false
			}
		}
		return true
	}
	if pk := t.keyIndex(); sameSet(pk) {
		return t.PrimaryKeyIndex, nil
	}
	for i := range t.Indexes {
		if t.Indexes[i].Unique && sameSet(&t.Indexes[i]) {
			return t.Indexes[i].Name, nil
		}
	}
	return "", fmt.Errorf("%w: there is no unique constraint matching given keys for referenced table %q", ErrInvalidForeignKey, t.Name)
}

// dropForeignKeys checks that no other table references t, which DROP TABLE drops, and removes
// the references of t's own foreign keys from the tables they reference.
func (s *Session) dropForeignKeys(ctx context.Context, t *table) error {
	for _, r := range t.ReferencedBy {
		if r.Table != t.Name {
			return fmt.Errorf("%w: cannot drop table %q because constraint %q on table %q depends on it", ErrDependentObjects, t.Name, r.Name, r.Table)
		}
	}
	for _, fk := range t.ForeignKeys {
		if fk.Parent == t.Name {
			continue
		}
		parent, err := s.getTable(ctx, fk.Parent)
		if err != nil {
			return err
		}
		parent = parent.clone()
		parent.ReferencedBy = slices.DeleteFunc(parent.ReferencedBy, func(r reference) bool { return r.Table == t.Name && r.Name == fk.Name })
		if err := s.putTable(parent); err != nil {
			return err
		}
	}
	return nil
}

// indexDependent reports the foreign key that depends on the index of t, if any, which DROP
// INDEX cannot drop then.
func (s *Session) indexDependent(ctx context.Context, t *table, name string) (reference, bool, error) {
	for _, r := range t.ReferencedBy {
		child := t
		if r.Table != t.Name {
			var err error
			if child, err = s.getTable(ctx, r.Table); err != nil {
				return reference{}, false, err
			}
		}
		if fk, ok := child.foreignKey(r.Name); ok && fk.Index == name {
			return r, true, nil
		}
	}
	return reference{}, false, nil
}

// rowEvent is a row that a statement inserted, updated, or deleted, whose foreign keys are
// checked or whose referencing rows are acted on when the statement ends, as Postgres's
// referential triggers do.
type rowEvent struct {
	t *table
	// old is the row before an update or a delete, at oldKey, and row the row after an insert or
	// an update, at key.
	oldKey, key []byte
	old, row    []value.Value
	// wrote reports whether the transaction had written the old row, in which case Postgres
	// checks the foreign keys of an updated row even when they did not change.
	wrote bool
}

// queue an event for the end of the statement, when the table has foreign keys or is
// referenced.
func (s *Session) queue(ev rowEvent) {
	if len(ev.t.ForeignKeys) == 0 && len(ev.t.ReferencedBy) == 0 {
		return
	}
	ev.oldKey, ev.key = slices.Clone(ev.oldKey), slices.Clone(ev.key)
	s.events = append(s.events, ev)
}

// fire the queued events in order, as Postgres fires its referential triggers at the end of a
// statement. For each row, it acts on the rows that reference the old row, by the foreign keys in
// the order they were created, and then checks the row's own foreign keys, in that order, as
// Postgres orders the triggers by name. An action is a statement of its own, whose events fire
// when it ends, before the next event of the statement that caused it.
func (s *Session) fire(ctx context.Context) error {
	events := s.events
	s.events = nil
	for _, ev := range events {
		if ev.old != nil {
			for _, r := range ev.t.ReferencedBy {
				if err := s.act(ctx, ev, r); err != nil {
					return err
				}
			}
		}
		if ev.row != nil {
			for i := range ev.t.ForeignKeys {
				if err := s.checkReference(ctx, ev, &ev.t.ForeignKeys[i]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// act on the rows that reference the old row of an update or a delete through the foreign key
// r: check that none does, or delete them, or set their keys to NULL or their defaults.
func (s *Session) act(ctx context.Context, ev rowEvent, r reference) error {
	child, err := s.getTable(ctx, r.Table)
	if err != nil {
		return err
	}
	fk, ok := child.foreignKey(r.Name)
	if !ok {
		return fmt.Errorf("exec: table %q does not have its foreign key %q", child.Name, r.Name)
	}
	key, complete := values(ev.old, fk.RefColumns)
	if !complete || s.opts.Mutations.ForeignKeyNoParentCheck {
		// A row with a NULL in the key cannot be referenced.
		return nil
	}
	action := fk.OnDelete
	if ev.row != nil {
		if now, _ := values(ev.row, fk.RefColumns); sameValues(key, now) {
			return nil
		}
		action = fk.OnUpdate
	}
	// restrict fails when rows still reference the key, unless, for NO ACTION, another row now has
	// the key.
	restrict := func(noAction bool) error {
		if noAction {
			if exists, err := s.parentExists(ctx, ev.t, fk, key); err != nil || exists {
				return err
			}
		}
		rows, err := s.referencing(ctx, child, fk, key, true)
		if err != nil || len(rows) == 0 {
			return err
		}
		if !noAction {
			return fmt.Errorf("%w: update or delete on table %q violates RESTRICT setting of foreign key constraint %q on table %q: key %s is referenced from table %q", ErrRestrict, ev.t.Name, fk.Name, child.Name, describeValues(ev.t, fk.RefColumns, key), child.Name)
		}
		return fmt.Errorf("%w: update or delete on table %q violates foreign key constraint %q on table %q: key %s is still referenced from table %q", ErrForeignKey, ev.t.Name, fk.Name, child.Name, describeValues(ev.t, fk.RefColumns, key), child.Name)
	}
	switch action {
	case ast.NoAction, ast.Restrict:
		return restrict(action == ast.NoAction)
	}
	rows, err := s.referencing(ctx, child, fk, key, false)
	if err != nil {
		return err
	}
	b := &binder{now: s.now()}
	for _, row := range rows {
		if action == ast.Cascade {
			s.kv.del(row.key)
			s.deleteRowEntries(child, row.row)
			s.queue(rowEvent{t: child, oldKey: row.key, old: row.row})
			if s.cascaded != nil {
				s.cascaded[child.Name] = true
			}
			continue
		}
		next := slices.Clone(row.row)
		for _, c := range fk.setColumns() {
			next[c] = value.NewNull(child.Columns[c].Type)
			if action == ast.SetDefault {
				e, err := s.defaultExpr(b, child, c)
				if err != nil {
					return err
				}
				if next[c], err = e.value(nil); err != nil {
					return err
				}
			}
		}
		if err := s.writeRow(ctx, child, row.key, row.row, next); err != nil {
			return err
		}
	}
	if err := s.fire(ctx); err != nil {
		return err
	}
	if action == ast.SetDefault {
		// A row whose default is the old key still references it, and the check of its foreign
		// key saw no change, so Postgres checks the old key as NO ACTION does.
		return restrict(true)
	}
	return nil
}

// checkReference checks that the row of an insert or update has a parent row through the foreign
// key fk, unless the key has a NULL, did not change in a row that the transaction had not
// written, or the row is no longer the row of the event, because the statement changed it again.
func (s *Session) checkReference(ctx context.Context, ev rowEvent, fk *foreignKey) error {
	key, complete := values(ev.row, fk.Columns)
	if !complete || s.opts.Mutations.ForeignKeyNoChildCheck {
		return nil
	}
	if ev.old != nil && !ev.wrote {
		if old, _ := values(ev.old, fk.Columns); sameValues(old, key) {
			return nil
		}
	}
	current, ok, err := s.kv.get(ctx, ev.key)
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(current, value.EncodeRow(nil, ev.row)) {
		return nil
	}
	parent := ev.t
	if fk.Parent != ev.t.Name {
		if parent, err = s.getTable(ctx, fk.Parent); err != nil {
			return err
		}
	}
	exists, err := s.parentExists(ctx, parent, fk, key)
	if err != nil || exists {
		return err
	}
	return fmt.Errorf("%w: insert or update on table %q violates foreign key constraint %q: key %s is not present in table %q", ErrForeignKey, ev.t.Name, fk.Name, describeValues(ev.t, fk.Columns, key), parent.Name)
}

// describeValues of columns of t, for messages.
func describeValues(t *table, columns []int, vals []value.Value) string {
	var names, vs []string
	for i, c := range columns {
		names = append(names, t.Columns[c].Name)
		vs = append(vs, vals[i].String())
	}
	return fmt.Sprintf("(%s)=(%s)", strings.Join(names, ", "), strings.Join(vs, ", "))
}

// parentExists reports whether parent has a row whose RefColumns of fk are key, which has no
// NULL, through the index that fk depends on: a point read of the row, or of its unique entry.
func (s *Session) parentExists(ctx context.Context, parent *table, fk *foreignKey, key []value.Value) (bool, error) {
	at := func(col int) value.Value {
		return key[slices.Index(fk.RefColumns, col)]
	}
	var k []byte
	if fk.Index == parent.PrimaryKeyIndex {
		k, _ = parent.rowRange()
		for _, c := range parent.PrimaryKey {
			k = value.EncodeKey(k, at(c))
		}
	} else {
		i := slices.IndexFunc(parent.Indexes, func(x index) bool { return x.Name == fk.Index })
		if i < 0 {
			return false, fmt.Errorf("exec: table %q does not have the index %q of foreign key %q", parent.Name, fk.Index, fk.Name)
		}
		x := &parent.Indexes[i]
		k, _ = x.entryRange()
		for _, c := range x.Columns {
			k = value.EncodeIndexKey(k, at(c.Column), c.Desc, c.NullsFirst)
		}
	}
	get := s.kv.get
	if s.opts.Mutations.ForeignKeyOwnWrites {
		get = s.kv.tx.Get
	}
	_, ok, err := get(ctx, k)
	return ok, err
}

// childRow is a row that references a key, at its row key.
type childRow struct {
	key []byte
	row []value.Value
}

// referencing returns the rows of child whose columns of fk are key, in key order, as the
// statement sees them. It reads an index whose first columns are those of fk where child has
// one, and every row otherwise. With one, it stops at the first row.
func (s *Session) referencing(ctx context.Context, child *table, fk *foreignKey, key []value.Value, one bool) ([]childRow, error) {
	txOnly := s.opts.Mutations.ForeignKeyOwnWrites
	types := child.types()
	matches := func(row []value.Value) bool {
		vals, _ := values(row, fk.Columns)
		return sameValues(vals, key)
	}
	at := func(col int) value.Value {
		return key[slices.Index(fk.Columns, col)]
	}
	leads := func(x *index) bool {
		if len(x.Columns) < len(fk.Columns) {
			return false
		}
		for _, c := range x.Columns[:len(fk.Columns)] {
			if !slices.Contains(fk.Columns, c.Column) {
				return false
			}
		}
		return true
	}
	var out []childRow
	decode := func(k, v []byte) error {
		row, err := value.DecodeRow(v, types)
		if err != nil {
			return fmt.Errorf("exec: table %q: %w", child.Name, err)
		}
		if matches(row) {
			out = append(out, childRow{key: k, row: row})
		}
		return nil
	}
	// The primary key is an index too, but only its leading columns make a prefix.
	if pk := child.keyIndex(); leads(pk) {
		start, _ := child.rowRange()
		for _, c := range pk.Columns[:len(fk.Columns)] {
			start = value.EncodeKey(start, at(c.Column))
		}
		pairs, err := s.kv.scanLatest(ctx, start, prefixEnd(start), txOnly)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			if err := decode(p.key, p.value); err != nil {
				return nil, err
			}
			if one && len(out) > 0 {
				break
			}
		}
		return out, nil
	}
	for i := range child.Indexes {
		x := &child.Indexes[i]
		if !leads(x) {
			continue
		}
		start, _ := x.entryRange()
		for _, c := range x.Columns[:len(fk.Columns)] {
			start = value.EncodeIndexKey(start, at(c.Column), c.Desc, c.NullsFirst)
		}
		pairs, err := s.kv.scanLatest(ctx, start, prefixEnd(start), txOnly)
		if err != nil {
			return nil, err
		}
		for _, p := range pairs {
			k := child.rowKeyOf(p.value)
			get := s.kv.get
			if txOnly {
				get = s.kv.tx.Get
			}
			v, ok, err := get(ctx, k)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("exec: %w: an entry of an index of %q points to the missing row %x", ErrIndexInconsistent, child.Name, k)
			}
			if err := decode(k, v); err != nil {
				return nil, err
			}
			if one && len(out) > 0 {
				break
			}
		}
		slices.SortFunc(out, func(a, b childRow) int { return bytes.Compare(a.key, b.key) })
		return out, nil
	}
	start, end := child.rowRange()
	pairs, err := s.kv.scanLatest(ctx, start, end, txOnly)
	if err != nil {
		return nil, err
	}
	for _, p := range pairs {
		if err := decode(p.key, p.value); err != nil {
			return nil, err
		}
		if one && len(out) > 0 {
			break
		}
	}
	return out, nil
}

// prefixEnd is the first key after every key that starts with prefix.
func prefixEnd(prefix []byte) []byte {
	end := slices.Clone(prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// firing ends a statement that wrote rows: it fires the statement's events, and counts the
// states the firing reached.
func (s *Session) firing(ctx context.Context) error {
	s.cascaded = map[string]bool{}
	defer func() { s.cascaded = nil }()
	if err := s.fire(ctx); err != nil {
		return err
	}
	multi := len(s.cascaded) >= 2
	assert.Sometimes(multi, "a cascade deletes rows in more than one table", nil)
	if multi {
		reach.Hit(ctx, reach.ForeignKeyCascadeTables)
	}
	return nil
}

// errOrderDependentActions is the error of a DELETE whose foreign key actions could give another
// outcome in another order.
var errOrderDependentActions = errors.New("order-dependent foreign key actions")

// checkActionOrder rejects a DELETE from t when the foreign key actions it can fire could give
// another outcome in another order. Postgres fires the actions and checks of each deleted row in
// the order it visits the rows, and those of one row in the order of its trigger names, which
// follows the creation order of the foreign keys only as long as their object identifiers have
// the same number of digits. The outcome is the same in every order when no check reads columns
// that an action writes, and every table that actions write is written by a single foreign key,
// or only deleted from. The checks are those of NO ACTION and RESTRICT, which read the
// referencing columns of the referencing table, NO ACTION also whether the referenced key exists
// again, and those of the foreign keys of a row that SET NULL or SET DEFAULT updates, which read
// the referenced columns.
func (s *Session) checkActionOrder(ctx context.Context, t *table) error {
	type read struct {
		table  string
		cols   []int
		delete bool
		// own is the foreign key whose SET DEFAULT checks again the key it acted on. Its own
		// writes cannot change the outcome, because every row it sets to the old key had it.
		own string
	}
	var reads []read
	deleted := map[string]bool{}
	set := map[string]map[int]bool{}
	writers := map[string]map[string]ast.Action{}
	write := func(child string, fk *foreignKey, a ast.Action) {
		if writers[child] == nil {
			writers[child] = map[string]ast.Action{}
		}
		writers[child][fk.Name] = a
	}
	resolve := func(x *table, r reference) (*table, *foreignKey, error) {
		child := x
		if r.Table != x.Name {
			var err error
			if child, err = s.getTable(ctx, r.Table); err != nil {
				return nil, nil, err
			}
		}
		fk, ok := child.foreignKey(r.Name)
		if !ok {
			return nil, nil, fmt.Errorf("exec: table %q does not have its foreign key %q", child.Name, r.Name)
		}
		return child, fk, nil
	}
	visitedDelete := map[string]bool{}
	visitedSet := map[string]map[int]bool{}
	var onDelete func(x *table) error
	var onSet func(c *table, cols []int) error
	onDelete = func(x *table) error {
		if visitedDelete[x.Name] {
			return nil
		}
		visitedDelete[x.Name] = true
		for _, r := range x.ReferencedBy {
			child, fk, err := resolve(x, r)
			if err != nil {
				return err
			}
			switch fk.OnDelete {
			case ast.NoAction:
				reads = append(reads, read{table: child.Name, cols: fk.Columns, delete: true}, read{table: x.Name, cols: fk.RefColumns})
			case ast.Restrict:
				reads = append(reads, read{table: child.Name, cols: fk.Columns, delete: true})
			case ast.Cascade:
				write(child.Name, fk, ast.Cascade)
				deleted[child.Name] = true
				if err := onDelete(child); err != nil {
					return err
				}
			default:
				write(child.Name, fk, fk.OnDelete)
				if fk.OnDelete == ast.SetDefault {
					reads = append(reads, read{table: child.Name, cols: fk.Columns, delete: true, own: fk.Name}, read{table: x.Name, cols: fk.RefColumns})
				}
				if set[child.Name] == nil {
					set[child.Name] = map[int]bool{}
				}
				for _, c := range fk.setColumns() {
					set[child.Name][c] = true
				}
				if err := onSet(child, fk.setColumns()); err != nil {
					return err
				}
			}
		}
		return nil
	}
	onSet = func(c *table, cols []int) error {
		if visitedSet[c.Name] == nil {
			visitedSet[c.Name] = map[int]bool{}
		}
		fresh := false
		for _, col := range cols {
			fresh = fresh || !visitedSet[c.Name][col]
			visitedSet[c.Name][col] = true
		}
		if !fresh {
			return nil
		}
		for _, r := range c.ReferencedBy {
			child, fk, err := resolve(c, r)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(fk.RefColumns, func(col int) bool { return slices.Contains(cols, col) }) {
				continue
			}
			// The ON UPDATE action is NO ACTION or RESTRICT.
			reads = append(reads, read{table: child.Name, cols: fk.Columns, delete: true})
			if fk.OnUpdate == ast.NoAction {
				reads = append(reads, read{table: c.Name, cols: fk.RefColumns})
			}
		}
		// A row that the transaction wrote before has all its foreign keys checked.
		for _, fk := range c.ForeignKeys {
			reads = append(reads, read{table: fk.Parent, cols: fk.RefColumns, delete: true})
		}
		return nil
	}
	if err := onDelete(t); err != nil {
		return err
	}
	unsupported := fmt.Errorf("%w: %w: a DELETE from %q whose foreign key actions could give another outcome in another order is not supported, because Postgres fires them in an order that depends on the physical order of the rows and on its object identifiers; give the tables that the actions change no other foreign key that acts on them or checks them", ErrUnsupported, errOrderDependentActions, t.Name)
	for _, ws := range writers {
		if len(ws) < 2 {
			continue
		}
		for _, a := range ws {
			if a != ast.Cascade {
				return unsupported
			}
		}
	}
	for _, r := range reads {
		if r.own != "" && len(writers[r.table]) == 1 && writers[r.table][r.own] == ast.SetDefault {
			continue
		}
		if r.delete && deleted[r.table] || slices.ContainsFunc(r.cols, func(c int) bool { return set[r.table][c] }) {
			return unsupported
		}
	}
	return nil
}

// Errors of [CheckConstraints].
var (
	// ErrDanglingReference is a row whose foreign key has no NULL and no row in the referenced
	// table.
	ErrDanglingReference = errors.New("a row references a missing row")
	// ErrCheckViolated is a row for which a CHECK constraint of its table is false.
	ErrCheckViolated = errors.New("a row violates a check constraint")
)

// CheckConstraints checks, in the transaction's snapshot, that every row of every table has a
// row in the referenced table for each foreign key whose columns have no NULL, which wraps
// [ErrDanglingReference] otherwise, and that no CHECK constraint is false for any row, which
// wraps [ErrCheckViolated] otherwise. It reads the catalog and every row.
func CheckConstraints(ctx context.Context, tx *engine.Tx) error {
	s := &Session{tx: tx, tables: map[string]*table{}, kv: kvstore{tx: tx}}
	var names []string
	it := tx.Scan(ctx, []byte("c/"), []byte("c0"))
	for it.Next() {
		names = append(names, strings.TrimPrefix(string(it.Key()), "c/"))
	}
	if err := it.Err(); err != nil {
		return err
	}
	rows := map[string][][]value.Value{}
	tables := map[string]*table{}
	for _, name := range names {
		t, ok, err := s.relation(ctx, name)
		if err != nil {
			return err
		}
		if !ok || t.IndexOf != "" {
			continue
		}
		tables[name] = t
		start, end := t.rowRange()
		types := t.types()
		it := tx.Scan(ctx, start, end)
		for it.Next() {
			row, err := value.DecodeRow(it.Value(), types)
			if err != nil {
				return fmt.Errorf("exec: table %q: %w", t.Name, err)
			}
			rows[name] = append(rows[name], row)
		}
		if err := it.Err(); err != nil {
			return err
		}
	}
	for _, name := range names {
		t, ok := tables[name]
		if !ok {
			continue
		}
		checks, err := s.checksOf(t)
		if err != nil {
			return err
		}
		for _, fk := range t.ForeignKeys {
			parent, ok := tables[fk.Parent]
			if !ok {
				return fmt.Errorf("%w: the foreign key %q of %q references the missing table %q", ErrDanglingReference, fk.Name, t.Name, fk.Parent)
			}
			keys := map[string]bool{}
			for _, row := range rows[parent.Name] {
				if vals, ok := values(row, fk.RefColumns); ok {
					keys[string(encodeValues(vals))] = true
				}
			}
			for _, row := range rows[name] {
				if vals, ok := values(row, fk.Columns); ok && !keys[string(encodeValues(vals))] {
					return fmt.Errorf("%w: the row %v of %q has the key %s of foreign key %q, which %q does not have", ErrDanglingReference, row, t.Name, describeValues(t, fk.Columns, vals), fk.Name, parent.Name)
				}
			}
		}
		for _, row := range rows[name] {
			for _, c := range checks {
				v, err := c.expr.value(row)
				if err != nil {
					return fmt.Errorf("%w: the check constraint %q of %q fails on the row %v: %v", ErrCheckViolated, c.name, t.Name, row, err)
				}
				if !v.Null && !v.B {
					return fmt.Errorf("%w: the row %v of %q violates the check constraint %q", ErrCheckViolated, row, t.Name, c.name)
				}
			}
		}
	}
	return nil
}

// encodeValues of a key without NULLs, so that equal keys encode equally.
func encodeValues(vals []value.Value) []byte {
	var out []byte
	for _, v := range vals {
		out = value.EncodeKey(out, v)
	}
	return out
}
