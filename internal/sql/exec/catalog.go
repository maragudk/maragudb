package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// Key layout. Tables and indexes share one namespace, as in Postgres. A table's definition,
// with its indexes, lives at `c/<name>`, and each index's name at `c/<index name>` too, pointing
// to its table. A table's rows live at `r/<name>\x00<key>`, where <key> is the order-preserving
// encoding of the primary key columns, and the entries of a secondary index at
// `i/<index name>\x00<entry>`, where <entry> is the index key encoding of the indexed columns,
// followed by the row's <key> unless the entry must be unique, and the entry's value is the
// row's <key>. The name of every constraint is at `k/<name>\x00<table>`. The layout requires that a table or index name never contains a NUL byte, so that
// the prefix of one never covers another; [newTable] and [Session.createIndex] enforce it.

// table definition as stored in the catalog.
type table struct {
	Name    string   `json:"name"`
	Columns []column `json:"columns"`
	// PrimaryKey holds column indexes in key order.
	PrimaryKey []int `json:"primaryKey"`
	// PrimaryKeyIndex is the name of the primary key's index, which Postgres creates in the
	// namespace of tables and indexes.
	PrimaryKeyIndex string `json:"primaryKeyIndex,omitempty"`
	// Indexes of the table besides the primary key, in the order they were created.
	Indexes []index `json:"indexes,omitempty"`
	// IndexOf is set when the catalog entry is the name of an index rather than a table, to the
	// table it belongs to. Such an entry has only its Name besides.
	IndexOf string `json:"indexOf,omitempty"`
	// Checks are the CHECK constraints of the table, in the order they were created.
	Checks []check `json:"checks,omitempty"`
	// ForeignKeys of the table, in the order they were created.
	ForeignKeys []foreignKey `json:"foreignKeys,omitempty"`
	// ReferencedBy are the foreign keys that reference the table, its own among them, in the order
	// they were created.
	ReferencedBy []reference `json:"referencedBy,omitempty"`
	// Constraints are the names of every constraint of the table, NOT NULL constraints among
	// them, as Postgres names those. Each is also at `k/<name>\x00<table>`, because the
	// constraints of all tables share the namespace in which Postgres chooses their names.
	Constraints []string `json:"constraints,omitempty"`
	// derived holds the rows of a subquery in FROM or of a CTE, which is a table only within a
	// statement, without indexes, and whose primary key is every column.
	derived *derivedRows
}

// check is a CHECK constraint, with its expression as SQL text.
type check struct {
	Name string `json:"name"`
	Expr string `json:"expr"`
}

type column struct {
	Name    string     `json:"name"`
	Type    value.Type `json:"type"`
	NotNull bool       `json:"notNull"`
	// Default is the SQL text of the column's DEFAULT expression, or "".
	Default string `json:"default,omitempty"`
}

// index is a secondary index of a table.
type index struct {
	Name    string        `json:"name"`
	Columns []indexColumn `json:"columns"`
	Unique  bool          `json:"unique,omitempty"`
	// NullsNotDistinct makes NULLs equal to each other in a unique index.
	NullsNotDistinct bool `json:"nullsNotDistinct,omitempty"`
	// Constraint reports that a UNIQUE constraint of the table owns the index, so that only
	// dropping the table drops it.
	Constraint bool `json:"constraint,omitempty"`
}

// indexColumn is a column of an index, with its sort order.
type indexColumn struct {
	Column     int  `json:"column"`
	Desc       bool `json:"desc,omitempty"`
	NullsFirst bool `json:"nullsFirst,omitempty"`
}

// entryRange is the key range [start, end) that holds every entry of the index.
func (x *index) entryRange() ([]byte, []byte) {
	return []byte("i/" + x.Name + "\x00"), []byte("i/" + x.Name + "\x01")
}

// entry of a row in the index: its key, and whether it is a unique entry, which no other row may
// have. The value of the entry is the row's primary key encoding.
func (x *index) entry(t *table, row []value.Value) ([]byte, bool) {
	key, _ := x.entryRange()
	hasNull := false
	for _, c := range x.Columns {
		key = value.EncodeIndexKey(key, row[c.Column], c.Desc, c.NullsFirst)
		hasNull = hasNull || row[c.Column].Null
	}
	unique := x.Unique && (!hasNull || x.NullsNotDistinct)
	if !unique {
		key = t.appendKey(key, row)
	}
	return key, unique
}

// covers reports whether the index has column i.
func (x *index) covers(i int) bool {
	for _, c := range x.Columns {
		if c.Column == i {
			return true
		}
	}
	return false
}

// keyIndex is the primary key as an index, which sorts by the key columns in ascending order.
func (t *table) keyIndex() *index {
	x := &index{Name: t.PrimaryKeyIndex, Unique: true}
	for _, i := range t.PrimaryKey {
		x.Columns = append(x.Columns, indexColumn{Column: i})
	}
	return x
}

// constrains reports whether column i is in the primary key or a unique index, so that an UPDATE
// that assigns it can collide with another row.
func (t *table) constrains(i int) bool {
	if t.isPrimaryKey(i) {
		return true
	}
	for _, x := range t.Indexes {
		if x.Unique && x.covers(i) {
			return true
		}
	}
	return false
}

// indexed reports whether column i is in the primary key or any index.
func (t *table) indexed(i int) bool {
	if t.derived != nil {
		return false
	}
	if t.isPrimaryKey(i) {
		return true
	}
	for _, x := range t.Indexes {
		if x.covers(i) {
			return true
		}
	}
	return false
}

// rowKeyOf the row whose primary key encoding is key, as an index entry holds it.
func (t *table) rowKeyOf(key []byte) []byte {
	start, _ := t.rowRange()
	return append(start, key...)
}

// keySuffix is the primary key encoding of a row key.
func (t *table) keySuffix(rowKey []byte) []byte {
	start, _ := t.rowRange()
	return rowKey[len(start):]
}

func catalogKey(name string) []byte {
	return []byte("c/" + name)
}

// rowRange is the key range [start, end) that holds every row of the table.
func (t *table) rowRange() ([]byte, []byte) {
	start := []byte("r/" + t.Name + "\x00")
	end := []byte("r/" + t.Name + "\x01")
	return start, end
}

// rowKey of a row.
func (t *table) rowKey(row []value.Value) []byte {
	start, _ := t.rowRange()
	return t.appendKey(start, row)
}

// appendKey appends the encoded primary key of row to dst.
func (t *table) appendKey(dst []byte, row []value.Value) []byte {
	for _, i := range t.PrimaryKey {
		dst = value.EncodeKey(dst, row[i])
	}
	return dst
}

func (t *table) columnIndex(name string) (int, bool) {
	for i, c := range t.Columns {
		if c.Name == name {
			return i, true
		}
	}
	return 0, false
}

func (t *table) types() []value.Type {
	types := make([]value.Type, len(t.Columns))
	for i, c := range t.Columns {
		types[i] = c.Type
	}
	return types
}

func (t *table) isPrimaryKey(i int) bool {
	for _, k := range t.PrimaryKey {
		if k == i {
			return true
		}
	}
	return false
}

// newTable validates a CREATE TABLE statement.
func newTable(s *ast.CreateTable) (*table, error) {
	if strings.IndexByte(s.Name, 0) >= 0 {
		return nil, fmt.Errorf("%w: table name must not contain a NUL byte", ErrInvalid)
	}
	t := &table{Name: s.Name}
	seen := map[string]bool{}
	for _, c := range s.Columns {
		if seen[c.Name] {
			return nil, fmt.Errorf("%w: column %q specified more than once", ErrInvalid, c.Name)
		}
		seen[c.Name] = true
		t.Columns = append(t.Columns, column{Name: c.Name, Type: c.Type, NotNull: c.NotNull})
	}
	inKey := map[string]bool{}
	for _, name := range s.PrimaryKey {
		i, ok := t.columnIndex(name)
		if !ok {
			return nil, fmt.Errorf("%w: column %q named in PRIMARY KEY does not exist", ErrInvalid, name)
		}
		if inKey[name] {
			return nil, fmt.Errorf("%w: column %q appears twice in PRIMARY KEY", ErrInvalid, name)
		}
		inKey[name] = true
		t.Columns[i].NotNull = true
		t.PrimaryKey = append(t.PrimaryKey, i)
	}
	return t, nil
}

// relation from the catalog, through the transaction's cache: a table, or an index whose
// IndexOf names its table. It returns false when no table or index has the name.
func (s *Session) relation(ctx context.Context, name string) (*table, bool, error) {
	if t, ok := s.tables[name]; ok {
		return t, t != nil, nil
	}
	b, ok, err := s.kv.get(ctx, catalogKey(name))
	if err != nil {
		return nil, false, err
	}
	if !ok {
		s.tables[name] = nil
		return nil, false, nil
	}
	var t table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, false, fmt.Errorf("exec: corrupt definition of relation %q: %w", name, err)
	}
	s.tables[name] = &t
	return &t, true, nil
}

// getTable from the catalog, through the transaction's cache.
func (s *Session) getTable(ctx context.Context, name string) (*table, error) {
	t, ok, err := s.relation(ctx, name)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("%w: table %q does not exist", ErrNoTable, name)
	case t.IndexOf != "":
		return nil, fmt.Errorf("%w: cannot open relation %q: this operation is not supported for indexes", ErrWrongObjectType, name)
	}
	return t, nil
}

// putTable writes a table's definition to the catalog.
func (s *Session) putTable(t *table) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.kv.put(catalogKey(t.Name), b)
	s.tables[t.Name] = t
	return nil
}

// putIndexName claims an index name in the catalog for the table.
func (s *Session) putIndexName(name, tableName string) error {
	t := &table{Name: name, IndexOf: tableName}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.kv.put(catalogKey(name), b)
	s.tables[name] = t
	return nil
}

// deleteRelation removes a name from the catalog.
func (s *Session) deleteRelation(name string) {
	s.kv.del(catalogKey(name))
	s.tables[name] = nil
}

// chooseName of an index as Postgres's ChooseRelationName does: the name of [makeObjectName], or
// with the label numbered from 1 until no table or index has the name.
func (s *Session) chooseName(ctx context.Context, name1, name2, label string) (string, error) {
	return chooseNumbered(name1, name2, label, func(name string) (bool, error) {
		_, exists, err := s.relation(ctx, name)
		return exists, err
	})
}

// chooseNumbered is the name of [makeObjectName], or with the label numbered from 1, until taken
// reports that the name is free.
func chooseNumbered(name1, name2, label string, taken func(name string) (bool, error)) (string, error) {
	for pass := 0; ; pass++ {
		l := label
		if pass > 0 {
			l = fmt.Sprint(label, pass)
		}
		name := makeObjectName(name1, name2, l)
		if t, err := taken(name); err != nil || !t {
			return name, err
		}
	}
}

// maxNameBytes is the longest name that Postgres keeps, NAMEDATALEN - 1.
const maxNameBytes = 63

// makeObjectName joins name1, name2 if not empty, and label with underscores, and shortens the
// longer of the names, one byte at a time, until the result fits in [maxNameBytes], as Postgres
// does. It never cuts a character in two.
func makeObjectName(name1, name2, label string) string {
	overhead := len(label) + 1
	if name2 != "" {
		overhead++
	}
	n1, n2 := len(name1), len(name2)
	for n1+n2 > maxNameBytes-overhead {
		if n1 > n2 {
			n1--
		} else {
			n2--
		}
	}
	name := clip(name1, n1)
	if name2 != "" {
		name += "_" + clip(name2, n2)
	}
	return name + "_" + label
}

// clip s to at most n bytes without cutting a character in two.
func clip(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// indexNameAddition is the part of a generated index name that names the columns, as Postgres's
// ChooseIndexColumnNames and ChooseIndexNameAddition make it: the column names joined with
// underscores, where a name that repeats an earlier one gets the lowest number that makes it
// differ, and which stops once it is longer than [maxNameBytes].
func indexNameAddition(columns []string) string {
	var names []string
	for _, orig := range columns {
		name := orig
		for i := 1; slices.Contains(names, name); i++ {
			suffix := fmt.Sprint(i)
			name = clip(orig, maxNameBytes-len(suffix)) + suffix
		}
		names = append(names, name)
	}
	var b strings.Builder
	for _, name := range names {
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
