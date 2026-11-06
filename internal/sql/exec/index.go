package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// createIndex runs CREATE INDEX. As in Postgres, it checks the table and the columns before the
// name, so that IF NOT EXISTS skips only a statement that is valid otherwise, and it fails on
// rows that a unique index would have twice.
func (s *Session) createIndex(ctx context.Context, st *ast.CreateIndex) error {
	t, err := s.getTable(ctx, st.Table)
	if err != nil {
		return err
	}
	x := index{Unique: st.Unique, NullsNotDistinct: st.NullsNotDistinct}
	var names []string
	for _, c := range st.Columns {
		i, ok := t.columnIndex(c.Name)
		if !ok {
			return fmt.Errorf("%w: column %q does not exist", ErrInvalid, c.Name)
		}
		col := indexColumn{Column: i, Desc: c.Desc, NullsFirst: c.Desc}
		switch c.Nulls {
		case ast.NullsFirst:
			col.NullsFirst = true
		case ast.NullsLast:
			col.NullsFirst = false
		}
		x.Columns = append(x.Columns, col)
		names = append(names, c.Name)
	}
	if st.Name == "" {
		if x.Name, err = s.chooseName(ctx, t.Name, indexNameAddition(names), "idx"); err != nil {
			return err
		}
	} else {
		if strings.IndexByte(st.Name, 0) >= 0 {
			return fmt.Errorf("%w: index name must not contain a NUL byte", ErrInvalid)
		}
		if _, exists, err := s.relation(ctx, st.Name); err != nil {
			return err
		} else if exists {
			if st.IfNotExists {
				return nil
			}
			return fmt.Errorf("%w: relation %q already exists", ErrTableExists, st.Name)
		}
		x.Name = st.Name
	}
	if err := s.fillIndex(ctx, t, &x); err != nil {
		return err
	}
	t = t.clone()
	t.Indexes = append(t.Indexes, x)
	if err := s.putTable(t); err != nil {
		return err
	}
	return s.putIndexName(x.Name, t.Name)
}

// fillIndex writes the entries of every row of t to the new index x, and fails when x is unique
// and two rows have the same unique entry.
func (s *Session) fillIndex(ctx context.Context, t *table, x *index) error {
	start, end := t.rowRange()
	it := s.kv.scan(ctx, start, end)
	types := t.types()
	for it.Next() {
		row, err := value.DecodeRow(it.Value(), types)
		if err != nil {
			return fmt.Errorf("exec: table %q: %w", t.Name, err)
		}
		key, unique := x.entry(t, row)
		if unique && !s.opts.Mutations.UniqueNoCheck {
			if _, exists, err := s.kv.get(ctx, key); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("%w: could not create unique index %q, because the key %s is duplicated", ErrDuplicateKey, x.Name, x.description(t, row))
			}
		}
		s.kv.put(key, t.keySuffix(it.Key()))
	}
	return it.Err()
}

// dropIndex runs DROP INDEX. The index of a PRIMARY KEY or UNIQUE constraint cannot be dropped
// on its own, as in Postgres.
func (s *Session) dropIndex(ctx context.Context, st *ast.DropIndex) error {
	rel, exists, err := s.relation(ctx, st.Name)
	switch {
	case err != nil:
		return err
	case !exists && st.IfExists:
		return nil
	case !exists:
		return fmt.Errorf("%w: index %q does not exist", ErrNoIndex, st.Name)
	case rel.IndexOf == "":
		return fmt.Errorf("%w: %q is not an index, use DROP TABLE to remove a table", ErrWrongObjectType, st.Name)
	}
	t, err := s.getTable(ctx, rel.IndexOf)
	if err != nil {
		return err
	}
	if st.Name == t.PrimaryKeyIndex {
		return fmt.Errorf("%w: cannot drop index %q because the primary key of table %q requires it", ErrDependentObjects, st.Name, t.Name)
	}
	t = t.clone()
	i := slices.IndexFunc(t.Indexes, func(x index) bool { return x.Name == st.Name })
	if i < 0 {
		return fmt.Errorf("exec: table %q does not have its index %q", t.Name, st.Name)
	}
	if t.Indexes[i].Constraint {
		return fmt.Errorf("%w: cannot drop index %q because a unique constraint of table %q requires it", ErrDependentObjects, st.Name, t.Name)
	}
	if err := s.deleteEntries(ctx, &t.Indexes[i]); err != nil {
		return err
	}
	t.Indexes = slices.Delete(t.Indexes, i, i+1)
	if err := s.putTable(t); err != nil {
		return err
	}
	s.deleteRelation(st.Name)
	return nil
}

// deleteEntries deletes every entry of the index.
func (s *Session) deleteEntries(ctx context.Context, x *index) error {
	start, end := x.entryRange()
	it := s.kv.scan(ctx, start, end)
	for it.Next() {
		s.kv.del(it.Key())
	}
	return it.Err()
}

// indexConstraint is a PRIMARY KEY or UNIQUE constraint of a new table, which has an index.
type indexConstraint struct {
	name             string
	columns          []int
	nullsNotDistinct bool
}

// indexConstraints of a new table, checked, in the order Postgres creates their indexes: the
// primary key first, then the others in the order they are written, where a constraint on the
// same columns as an earlier one, with the same NULLS DISTINCT, adds no index and gives its name
// to the earlier one if that has none.
func (t *table) indexConstraints(st *ast.CreateTable) ([]indexConstraint, error) {
	cs := []indexConstraint{{name: st.PrimaryKeyName, columns: t.PrimaryKey}}
	for _, u := range st.Unique {
		c := indexConstraint{name: u.Name, nullsNotDistinct: u.NullsNotDistinct}
		for _, name := range u.Columns {
			i, ok := t.columnIndex(name)
			if !ok {
				return nil, fmt.Errorf("%w: column %q named in key does not exist", ErrInvalid, name)
			}
			if slices.Contains(c.columns, i) {
				return nil, fmt.Errorf("%w: column %q appears twice in unique constraint", ErrInvalid, name)
			}
			c.columns = append(c.columns, i)
		}
		same := slices.IndexFunc(cs, func(o indexConstraint) bool {
			return slices.Equal(o.columns, c.columns) && o.nullsNotDistinct == c.nullsNotDistinct
		})
		if same < 0 {
			cs = append(cs, c)
			continue
		}
		if cs[same].name == "" {
			cs[same].name = c.name
		}
	}
	return cs, nil
}

// addIndexes adds the indexes of a new table's constraints from [table.indexConstraints], one at a
// time, so that the name Postgres chooses for one depends on the names of those before it.
func (s *Session) addIndexes(ctx context.Context, t *table, cs []indexConstraint) error {
	for i, c := range cs {
		name := c.name
		var err error
		switch {
		case name != "" && strings.IndexByte(name, 0) >= 0:
			return fmt.Errorf("%w: constraint name must not contain a NUL byte", ErrInvalid)
		case name != "":
			if _, exists, err := s.relation(ctx, name); err != nil {
				return err
			} else if exists {
				return fmt.Errorf("%w: relation %q already exists", ErrTableExists, name)
			}
		case i == 0:
			name, err = s.chooseName(ctx, t.Name, "", "pkey")
		default:
			var names []string
			for _, col := range c.columns {
				names = append(names, t.Columns[col].Name)
			}
			name, err = s.chooseName(ctx, t.Name, indexNameAddition(names), "key")
		}
		if err != nil {
			return err
		}
		if i == 0 {
			t.PrimaryKeyIndex = name
		} else {
			x := index{Name: name, Unique: true, NullsNotDistinct: c.nullsNotDistinct, Constraint: true}
			for _, col := range c.columns {
				x.Columns = append(x.Columns, indexColumn{Column: col})
			}
			t.Indexes = append(t.Indexes, x)
		}
		if err := s.putIndexName(name, t.Name); err != nil {
			return err
		}
		if err := s.putTable(t); err != nil {
			return err
		}
	}
	return nil
}

// clone of the table definition, which can be changed without changing the cached one.
func (t *table) clone() *table {
	c := *t
	c.Columns = slices.Clone(t.Columns)
	c.PrimaryKey = slices.Clone(t.PrimaryKey)
	c.Indexes = slices.Clone(t.Indexes)
	return &c
}

// description of the values of a row in the index's columns, for messages.
func (x *index) description(t *table, row []value.Value) string {
	var parts []string
	for _, c := range x.Columns {
		parts = append(parts, fmt.Sprintf("%s=%v", t.Columns[c.Column].Name, row[c.Column]))
	}
	return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
}

// duplicate is the error of a row whose unique entry in x another row has.
func duplicate(t *table, x *index, row []value.Value) error {
	return fmt.Errorf("%w: duplicate key value violates unique constraint %q: the key %s already exists in table %q", ErrDuplicateKey, x.Name, x.description(t, row), t.Name)
}

// writeEntries writes the index entries of row, which replaces old when old is given. An entry
// that changes is deleted and the new one written, after a check that no other row has the new
// one when it is a unique entry; an entry that stays is written again only when the row's key
// changes, because the entry's value is the key.
func (s *Session) writeEntries(ctx context.Context, t *table, old, row []value.Value) error {
	pk := t.appendKey(nil, row)
	moved := old != nil && !bytes.Equal(t.appendKey(nil, old), pk)
	for i := range t.Indexes {
		x := &t.Indexes[i]
		key, unique := x.entry(t, row)
		if old != nil {
			oldKey, _ := x.entry(t, old)
			if bytes.Equal(oldKey, key) {
				if moved {
					s.kv.put(key, pk)
				}
				continue
			}
			if !s.opts.Mutations.IndexStaleDelete {
				s.kv.del(oldKey)
			}
		}
		if unique && !s.opts.Mutations.UniqueNoCheck {
			get := s.kv.get
			if s.opts.Mutations.UniqueOwnWrites {
				get = s.kv.tx.Get
			}
			if _, exists, err := get(ctx, key); err != nil {
				return err
			} else if exists {
				return duplicate(t, x, row)
			}
		}
		s.kv.put(key, pk)
	}
	return nil
}

// deleteRowEntries deletes the index entries of a deleted row.
func (s *Session) deleteRowEntries(t *table, row []value.Value) {
	if s.opts.Mutations.IndexStaleDelete {
		return
	}
	for i := range t.Indexes {
		key, _ := t.Indexes[i].entry(t, row)
		s.kv.del(key)
	}
}

// ErrIndexInconsistent is returned by [CheckIndexes] when an index does not hold exactly the
// entries of its table's rows.
var ErrIndexInconsistent = errors.New("index inconsistent with its table")

// CheckIndexes checks, in the transaction's snapshot, that every index of every table holds
// exactly the entries derived from the table's rows, that every unique index has no two rows
// with the same unique entry, and that every index name in the catalog belongs to an index of
// its table. It reads the catalog, every row, and every entry.
func CheckIndexes(ctx context.Context, tx *engine.Tx) error {
	tables := map[string]*table{}
	names := map[string]string{}
	it := tx.Scan(ctx, []byte("c/"), []byte("c0"))
	for it.Next() {
		var t table
		if err := json.Unmarshal(it.Value(), &t); err != nil {
			return fmt.Errorf("exec: corrupt catalog entry %q: %w", it.Key(), err)
		}
		if t.IndexOf != "" {
			names[t.Name] = t.IndexOf
			continue
		}
		tables[t.Name] = &t
	}
	if err := it.Err(); err != nil {
		return err
	}
	for name, of := range names {
		t, ok := tables[of]
		if !ok || name != t.PrimaryKeyIndex && !slices.ContainsFunc(t.Indexes, func(x index) bool { return x.Name == name }) {
			return fmt.Errorf("%w: the catalog names %q an index of %q, which has no such index", ErrIndexInconsistent, name, of)
		}
	}
	want := map[string]string{}
	for _, t := range tables {
		if t.PrimaryKeyIndex != "" && names[t.PrimaryKeyIndex] != t.Name {
			return fmt.Errorf("%w: the primary key index %q of %q is not in the catalog", ErrIndexInconsistent, t.PrimaryKeyIndex, t.Name)
		}
		for i := range t.Indexes {
			if names[t.Indexes[i].Name] != t.Name {
				return fmt.Errorf("%w: the index %q of %q is not in the catalog", ErrIndexInconsistent, t.Indexes[i].Name, t.Name)
			}
		}
		start, end := t.rowRange()
		types := t.types()
		rows := tx.Scan(ctx, start, end)
		for rows.Next() {
			row, err := value.DecodeRow(rows.Value(), types)
			if err != nil {
				return fmt.Errorf("exec: table %q: %w", t.Name, err)
			}
			for i := range t.Indexes {
				x := &t.Indexes[i]
				key, _ := x.entry(t, row)
				if other, ok := want[string(key)]; ok {
					return fmt.Errorf("%w: rows %x and %x of %q have the same entry in the unique index %q", ErrIndexInconsistent, other, t.keySuffix(rows.Key()), t.Name, x.Name)
				}
				want[string(key)] = string(t.keySuffix(rows.Key()))
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	entries := tx.Scan(ctx, []byte("i/"), []byte("i0"))
	n := 0
	for entries.Next() {
		n++
		pk, ok := want[string(entries.Key())]
		if !ok {
			return fmt.Errorf("%w: the entry %q is not derived from any row", ErrIndexInconsistent, entries.Key())
		}
		if pk != string(entries.Value()) {
			return fmt.Errorf("%w: the entry %q points to the row %x, but derives from the row %x", ErrIndexInconsistent, entries.Key(), entries.Value(), pk)
		}
	}
	if err := entries.Err(); err != nil {
		return err
	}
	if n != len(want) {
		return fmt.Errorf("%w: %d entries are missing", ErrIndexInconsistent, len(want)-n)
	}
	return nil
}
