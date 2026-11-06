package exec

import (
	"context"
	"fmt"
	"slices"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// entry is a table in the FROM of a statement, under the name that references it: its alias, or
// the table's own name.
type entry struct {
	t    *table
	name string
	// columns are the names of the table's columns, after the names that an alias gives them.
	columns []string
	// offset of the entry's columns in the rows of a join, which hold the columns of every entry
	// in the order the entries are written.
	offset int
	// nullable is set when the entry is on the right side of a LEFT JOIN, which can extend a row
	// with NULLs in its place.
	nullable bool
}

// tableSet is a set of entries, by their index.
type tableSet uint64

func (s tableSet) has(i int) bool      { return s&(1<<i) != 0 }
func (s tableSet) with(i int) tableSet { return s | 1<<i }

// members of the set, in order.
func (s tableSet) members() []int {
	var out []int
	for i := range 64 {
		if s.has(i) {
			out = append(out, i)
		}
	}
	return out
}

// maxEntries is the number of tables a statement can read, which is the size of a [tableSet].
const maxEntries = 64

// colRef is what a column name resolves to: a column of an entry, converted to cast when cast
// is not [value.Unknown], which is the column that USING merges from columns of two types.
type colRef struct {
	entry, col int
	cast       value.Type
}

// nsCol is a column of a namespace item, by the name that references it.
type nsCol struct {
	name string
	ref  colRef
}

// nsItem is an item of a namespace, as Postgres's parser has them: an entry whose name can
// qualify a column, when rel is set, and the columns that a name alone can reference, which are
// those of the entry, or the columns of a join, whose items then have only their names visible.
type nsItem struct {
	// entry is the index of the entry, or -1 for the columns of a join.
	entry int
	rel   bool
	cols  []nsCol
}

// namespace that the names in a clause resolve in.
type namespace []nsItem

// scope of a statement: the entries of its FROM, which every namespace of the statement draws
// from.
type scope struct {
	entries []*entry
	// width of a row that holds the columns of every entry.
	width int
}

// singleScope is the scope of a statement on the one table t, referenced by name.
func singleScope(t *table, name string) (*scope, namespace) {
	e := &entry{t: t, name: name}
	for _, c := range t.Columns {
		e.columns = append(e.columns, c.Name)
	}
	sc := &scope{entries: []*entry{e}, width: len(t.Columns)}
	return sc, namespace{sc.item(0)}
}

// item of the entry i, with its name and its columns visible.
func (sc *scope) item(i int) nsItem {
	e := sc.entries[i]
	it := nsItem{entry: i, rel: true}
	for j, name := range e.columns {
		it.cols = append(it.cols, nsCol{name: name, ref: colRef{entry: i, col: j}})
	}
	return it
}

// typeOf the column that a reference resolves to.
func (sc *scope) typeOf(r colRef) value.Type {
	if r.cast != value.Unknown {
		return r.cast
	}
	return sc.entries[r.entry].t.Columns[r.col].Type
}

// resolve a column reference as Postgres does: a qualified name by the visible entry of that
// name, and a name alone by every item whose columns are visible, so that a name that two items
// have, or one item twice, is ambiguous. A name alone that is no column but names an entry is a
// reference to the whole row, which is not supported.
func (sc *scope) resolve(ns namespace, ref *ast.ColumnRef) (colRef, error) {
	var found []colRef
	if ref.Table != "" {
		i := slices.IndexFunc(ns, func(it nsItem) bool { return it.rel && sc.entries[it.entry].name == ref.Table })
		if i < 0 {
			if slices.ContainsFunc(sc.entries, func(e *entry) bool { return e.name == ref.Table || e.t.Name == ref.Table }) {
				return colRef{}, fmt.Errorf("%w: invalid reference to FROM-clause entry for table %q", ErrNoTable, ref.Table)
			}
			return colRef{}, fmt.Errorf("%w: missing FROM-clause entry for table %q", ErrNoTable, ref.Table)
		}
		e := sc.entries[ns[i].entry]
		for j, name := range e.columns {
			if name == ref.Name {
				found = append(found, colRef{entry: ns[i].entry, col: j})
			}
		}
		switch len(found) {
		case 0:
			return colRef{}, fmt.Errorf("%w: column %s.%s does not exist", ErrInvalid, ref.Table, ref.Name)
		case 1:
			return found[0], nil
		}
		return colRef{}, fmt.Errorf("%w: column reference %q is ambiguous", ErrInvalid, ref.Table+"."+ref.Name)
	}
	for _, it := range ns {
		for _, c := range it.cols {
			if c.name == ref.Name {
				found = append(found, c.ref)
			}
		}
	}
	switch len(found) {
	case 0:
		if slices.ContainsFunc(ns, func(it nsItem) bool { return it.rel && sc.entries[it.entry].name == ref.Name }) {
			return colRef{}, fmt.Errorf("%w: a reference to the whole row of %q is not supported, name its columns", ErrUnsupported, ref.Name)
		}
		return colRef{}, fmt.Errorf("%w: column %q does not exist", ErrInvalid, ref.Name)
	case 1:
		return found[0], nil
	}
	return colRef{}, fmt.Errorf("%w: column reference %q is ambiguous", ErrInvalid, ref.Name)
}

// expr is an expression that resolves to the column r in a namespace where the entries of the
// statement are visible by name.
func (sc *scope) expr(r colRef) ast.Expr {
	e := sc.entries[r.entry]
	var x ast.Expr = &ast.ColumnRef{Table: e.name, Name: e.columns[r.col]}
	if r.cast != value.Unknown {
		x = &ast.Cast{Expr: x, Type: r.cast}
	}
	return x
}

// fromNode is an item of FROM, as a tree of joins of the statement's entries.
type fromNode struct {
	// entry is the index of the entry of a table, or -1 for a join.
	entry int
	kind  ast.JoinKind
	// left and right are the sides of a join.
	left, right *fromNode
	// on is the condition of an inner or left join, bound in the rows of the join, with on.never
	// set when it is never true. A cross join has none.
	on     filter
	hasOn  bool
	onNS   namespace
	ns     namespace
	cols   []nsCol
	tables tableSet
	// members are the entries of tables, in order.
	members []int
}

// from builds the scope of a FROM clause, and binds the conditions of its joins, in the order
// Postgres analyzes them: each item from the left, a join's sides before its condition, and
// the names of every item checked against those before it. It returns the items and the
// namespace of the whole clause.
func (b *binder) from(ctx context.Context, s *Session, items []ast.TableExpr) ([]*fromNode, error) {
	b.scope = &scope{}
	var nodes []*fromNode
	for _, item := range items {
		n, err := b.fromItem(ctx, s, item)
		if err != nil {
			return nil, err
		}
		if err := b.checkNames(b.ns, n.ns); err != nil {
			return nil, err
		}
		b.ns = append(b.ns, n.ns...)
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// checkNames reports an error when an entry of r has the name of one of l, which Postgres
// checks whenever it joins two namespaces.
func (b *binder) checkNames(l, r namespace) error {
	for _, x := range r {
		if !x.rel {
			continue
		}
		for _, y := range l {
			if y.rel && b.scope.entries[x.entry].name == b.scope.entries[y.entry].name {
				return fmt.Errorf("%w: table name %q specified more than once", ErrInvalid, b.scope.entries[x.entry].name)
			}
		}
	}
	return nil
}

func (b *binder) fromItem(ctx context.Context, s *Session, item ast.TableExpr) (*fromNode, error) {
	switch item := item.(type) {
	case *ast.TableRef:
		t, err := s.getTable(ctx, item.Name)
		if err != nil {
			return nil, err
		}
		if len(item.Columns) > len(t.Columns) {
			name := item.Alias
			if name == "" {
				name = item.Name
			}
			return nil, fmt.Errorf("%w: table %q has %d columns available but %d columns specified", ErrInvalid, name, len(t.Columns), len(item.Columns))
		}
		if len(b.scope.entries) == maxEntries {
			return nil, fmt.Errorf("%w: more than %d tables in one statement are not supported", ErrUnsupported, maxEntries)
		}
		e := &entry{t: t, name: item.Name, offset: b.scope.width}
		if item.Alias != "" {
			e.name = item.Alias
		}
		for i, c := range t.Columns {
			name := c.Name
			if i < len(item.Columns) {
				name = item.Columns[i]
			}
			e.columns = append(e.columns, name)
		}
		b.scope.entries = append(b.scope.entries, e)
		b.scope.width += len(t.Columns)
		i := len(b.scope.entries) - 1
		it := b.scope.item(i)
		return &fromNode{entry: i, ns: namespace{it}, cols: it.cols, tables: tableSet(0).with(i), members: []int{i}}, nil
	case *ast.Join:
		l, err := b.fromItem(ctx, s, item.Left)
		if err != nil {
			return nil, err
		}
		r, err := b.fromItem(ctx, s, item.Right)
		if err != nil {
			return nil, err
		}
		if err := b.checkNames(l.ns, r.ns); err != nil {
			return nil, err
		}
		n := &fromNode{entry: -1, kind: item.Kind, left: l, right: r, tables: l.tables | r.tables, members: append(slices.Clone(l.members), r.members...)}
		n.onNS = append(append(namespace{}, l.ns...), r.ns...)
		if item.Kind == ast.JoinLeft {
			for _, i := range r.tables.members() {
				b.scope.entries[i].nullable = true
			}
		}
		using := item.Using
		if item.Natural {
			for _, c := range l.cols {
				if slices.ContainsFunc(r.cols, func(rc nsCol) bool { return rc.name == c.name }) && !slices.Contains(using, c.name) {
					using = append(using, c.name)
				}
			}
		}
		merged, cond, err := b.using(l, r, using)
		if err != nil {
			return nil, err
		}
		n.cols = merged
		for _, c := range l.cols {
			if !slices.Contains(using, c.name) {
				n.cols = append(n.cols, c)
			}
		}
		for _, c := range r.cols {
			if !slices.Contains(using, c.name) {
				n.cols = append(n.cols, c)
			}
		}
		for _, it := range n.onNS {
			n.ns = append(n.ns, nsItem{entry: it.entry, rel: it.rel})
		}
		n.ns = append(n.ns, nsItem{entry: -1, cols: n.cols})
		on := item.On
		if cond != nil {
			on = cond
		}
		if item.Kind != ast.JoinCross && on != nil {
			saved, phase := b.ns, b.phase
			b.ns, b.phase = n.onNS, phaseOn
			n.on, err = b.where(on, "JOIN/ON")
			b.ns, b.phase = saved, phase
			if err != nil {
				return nil, err
			}
			n.hasOn = true
		}
		return n, nil
	}
	return nil, fmt.Errorf("exec: unsupported FROM item %T", item)
}

// using resolves the columns of USING, or of NATURAL, in the columns of both sides of a join, as
// Postgres does, and returns the merged columns and the equality of their sides, which is the
// condition of the join, or nil for no columns. A merged column is that of the left side,
// converted to the common type of both sides.
func (b *binder) using(l, r *fromNode, names []string) ([]nsCol, ast.Expr, error) {
	var merged []nsCol
	var cond ast.Expr
	find := func(cols []nsCol, name, side string) (colRef, error) {
		var found []colRef
		for _, c := range cols {
			if c.name == name {
				found = append(found, c.ref)
			}
		}
		switch len(found) {
		case 0:
			return colRef{}, fmt.Errorf("%w: column %q specified in USING clause does not exist in %s table", ErrInvalid, name, side)
		case 1:
			return found[0], nil
		}
		return colRef{}, fmt.Errorf("%w: common column name %q appears more than once in %s table", ErrInvalid, name, side)
	}
	// Postgres finds every column on both sides before it compares their types.
	var pairs [][2]colRef
	for i, name := range names {
		if slices.Contains(names[:i], name) {
			return nil, nil, fmt.Errorf("%w: column name %q appears more than once in USING clause", ErrInvalid, name)
		}
		lc, err := find(l.cols, name, "left")
		if err != nil {
			return nil, nil, err
		}
		rc, err := find(r.cols, name, "right")
		if err != nil {
			return nil, nil, err
		}
		pairs = append(pairs, [2]colRef{lc, rc})
	}
	for i, p := range pairs {
		lc, rc := p[0], p[1]
		lt, rt := b.scope.typeOf(lc), b.scope.typeOf(rc)
		t, err := commonType(lt, rt)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: operator does not exist: %v = %v", ErrType, lt, rt)
		}
		eq := &ast.Binary{Op: ast.Eq, Left: b.scope.expr(lc), Right: b.scope.expr(rc)}
		if cond == nil {
			cond = eq
		} else {
			cond = &ast.Binary{Op: ast.And, Left: cond, Right: eq}
		}
		m := lc
		if t != lt {
			m.cast = t
		}
		merged = append(merged, nsCol{name: names[i], ref: m})
	}
	return merged, cond, nil
}

// normalize the column references of e that resolve to the form that every reference to the same
// column has, as Postgres compares expressions after it resolves their names: the column's name
// alone in a statement on one table, and qualified by the name of its entry otherwise, with the
// conversion of a column that USING merges from two types. Such a reference resolves the same in
// every namespace of the statement. A reference that does not resolve stays as it is.
func (b *binder) normalize(e ast.Expr) ast.Expr {
	sc, _ := b.names()
	if sc == nil || e == nil {
		return e
	}
	return ast.Map(e, func(x ast.Expr) ast.Expr {
		ref, ok := x.(*ast.ColumnRef)
		if !ok {
			return x
		}
		r, err := b.resolve(ref)
		if err != nil {
			return x
		}
		if len(sc.entries) == 1 {
			return &ast.ColumnRef{Name: sc.entries[r.entry].columns[r.col]}
		}
		return sc.expr(r)
	})
}

// tablesOf e: the entries whose columns its references resolve to. A reference that does not
// resolve is left out.
func (b *binder) tablesOf(e ast.Expr) tableSet {
	var set tableSet
	ast.Walk(e, func(x ast.Expr) bool {
		if ref, ok := x.(*ast.ColumnRef); ok {
			if r, err := b.resolve(ref); err == nil {
				set = set.with(r.entry)
			}
		}
		return true
	})
	return set
}
