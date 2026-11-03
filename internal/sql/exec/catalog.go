package exec

import (
	"context"
	"encoding/json"
	"fmt"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// Key layout. A table's definition lives at `c/<name>`, and its rows at `r/<name>\x00<key>`,
// where <key> is the order-preserving encoding of the primary key columns. Identifiers never
// contain a NUL byte, so the row prefix of one table never covers rows of another.
const (
	catalogPrefix = "c/"
	rowPrefix     = "r/"
)

// table definition as stored in the catalog.
type table struct {
	Name    string   `json:"name"`
	Columns []column `json:"columns"`
	// PrimaryKey holds column indexes in key order.
	PrimaryKey []int `json:"primaryKey"`
}

type column struct {
	Name    string     `json:"name"`
	Type    value.Type `json:"type"`
	NotNull bool       `json:"notNull"`
}

func catalogKey(name string) []byte {
	return []byte(catalogPrefix + name)
}

// rowRange is the key range [start, end) that holds every row of the table.
func (t *table) rowRange() ([]byte, []byte) {
	start := []byte(rowPrefix + t.Name + "\x00")
	end := []byte(rowPrefix + t.Name + "\x01")
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

// getTable from the catalog, through the transaction's cache.
func (s *Session) getTable(ctx context.Context, name string) (*table, error) {
	if t, ok := s.tables[name]; ok {
		if t == nil {
			return nil, fmt.Errorf("%w: table %q does not exist", ErrNoTable, name)
		}
		return t, nil
	}
	b, ok, err := s.kv.get(ctx, catalogKey(name))
	if err != nil {
		return nil, err
	}
	if !ok {
		s.tables[name] = nil
		return nil, fmt.Errorf("%w: table %q does not exist", ErrNoTable, name)
	}
	var t table
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("exec: corrupt definition of table %q: %w", name, err)
	}
	s.tables[name] = &t
	return &t, nil
}
