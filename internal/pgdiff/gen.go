package pgdiff

import (
	"fmt"
	"math"
	"time"

	"hegel.dev/go/hegel"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sql/value"
)

// Case is one generated scenario: a table, its initial rows, and statements to run on it.
type Case struct {
	Create *ast.CreateTable
	// Statements run in order after the table is created. Mutations are followed by a state
	// check on both sides.
	Statements []ast.Statement
	// ASCII reports whether every string in the case is ASCII, which is when lower and upper
	// are generated, because Postgres with the C locale folds case only in ASCII.
	ASCII bool
}

// Table name used by every case.
const tableName = "t"

var allTypes = []value.Type{value.Int, value.Float, value.Bool, value.String, value.Bytes, value.Time}

// gen generates one case.
type gen struct {
	ht    hegel.TestCase
	table *ast.CreateTable
	ascii bool
	// cols by type.
	cols map[value.Type][]string
}

// Generate a case.
func Generate(ht hegel.TestCase) Case {
	g := &gen{ht: ht, ascii: hegel.Draw(ht, hegel.WeightedBooleans(0.7)), cols: map[value.Type][]string{}}
	g.schema()
	c := Case{Create: g.table, ASCII: g.ascii}
	c.Statements = append(c.Statements, g.insert(hegel.Draw(ht, hegel.Integers(0, 6))))
	for range hegel.Draw(ht, hegel.Integers(1, 6)) {
		c.Statements = append(c.Statements, g.statement())
	}
	return c
}

func (g *gen) draw(min, max int) int {
	return hegel.Draw(g.ht, hegel.Integers(min, max))
}

func (g *gen) bool() bool {
	return hegel.Draw(g.ht, hegel.Booleans())
}

func (g *gen) schema() {
	n := g.draw(1, 5)
	t := &ast.CreateTable{Name: tableName}
	for i := range n {
		typ := hegel.Draw(g.ht, hegel.SampledFrom(allTypes))
		name := fmt.Sprint("c", i)
		t.Columns = append(t.Columns, ast.ColumnDef{Name: name, Type: typ, NotNull: g.draw(0, 4) == 0})
		g.cols[typ] = append(g.cols[typ], name)
	}
	// The primary key is one or two columns, chosen at random.
	first := g.draw(0, n-1)
	t.PrimaryKey = []string{t.Columns[first].Name}
	if n > 1 && g.bool() {
		second := g.draw(0, n-1)
		if second != first {
			t.PrimaryKey = append(t.PrimaryKey, t.Columns[second].Name)
		}
	}
	g.table = t
}

func (g *gen) column(name string) ast.ColumnDef {
	for _, c := range g.table.Columns {
		if c.Name == name {
			return c
		}
	}
	panic(name)
}

func (g *gen) isKey(name string) bool {
	for _, k := range g.table.PrimaryKey {
		if k == name {
			return true
		}
	}
	return false
}

// literal of type t, drawn from a distribution rich in edge cases.
func (g *gen) literal(t value.Type) ast.Expr {
	return &ast.Literal{Value: g.value(t)}
}

func (g *gen) value(t value.Type) value.Value {
	switch t {
	case value.Int:
		switch g.draw(0, 9) {
		case 0:
			return value.NewInt(hegel.Draw(g.ht, hegel.SampledFrom([]int64{math.MinInt64, math.MaxInt64, math.MinInt64 + 1, math.MaxInt64 - 1, 1 << 32, -(1 << 32)})))
		case 1, 2:
			return value.NewInt(hegel.Draw(g.ht, hegel.Integers[int64](-100000, 100000)))
		default:
			return value.NewInt(hegel.Draw(g.ht, hegel.Integers[int64](-5, 5)))
		}
	case value.Float:
		switch g.draw(0, 9) {
		case 0:
			return value.NewFloat(hegel.Draw(g.ht, hegel.SampledFrom([]float64{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, 1e308, 1e-308})))
		case 1, 2:
			return value.NewFloat(hegel.Draw(g.ht, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false)))
		case 3:
			return value.NewFloat(float64(g.draw(-5, 5)))
		default:
			return value.NewFloat(float64(g.draw(-50, 50)) / 4)
		}
	case value.Bool:
		return value.NewBool(g.bool())
	case value.String:
		if g.ascii {
			return value.NewString(hegel.Draw(g.ht, hegel.Text().Alphabet("abcAB _%\\'\"").MaxSize(6)))
		}
		return value.NewString(hegel.Draw(g.ht, hegel.Text().MaxSize(6).ExcludeCharacters("\x00")))
	case value.Bytes:
		return value.NewBytes(hegel.Draw(g.ht, hegel.Binary(0, 4)))
	case value.Time:
		// Microsecond precision, which is what Postgres keeps.
		sec := hegel.Draw(g.ht, hegel.Integers[int64](-2208988800, 4102444800))
		usec := hegel.Draw(g.ht, hegel.Integers[int64](0, 999999))
		if g.draw(0, 3) == 0 {
			usec = 0
		}
		return value.NewTime(time.Unix(sec, usec*1000))
	}
	panic(t)
}

// scope of an expression.
type scope struct {
	// columns may be referenced.
	columns bool
	// safe forbids expressions that can fail at runtime, for queries with LIMIT or OFFSET,
	// where Postgres may evaluate fewer rows than maragudb.
	safe bool
	// null allows a bare NULL literal here.
	null bool
}

func (s scope) withNull(null bool) scope {
	s.null = null
	return s
}

// expr of type t.
func (g *gen) expr(t value.Type, s scope, depth int) ast.Expr {
	if s.null && g.draw(0, 11) == 0 {
		return &ast.Literal{Value: value.NewNull(value.Unknown)}
	}
	s = s.withNull(false)
	if s.columns && len(g.cols[t]) > 0 && g.draw(0, 2) == 0 {
		return &ast.ColumnRef{Name: hegel.Draw(g.ht, hegel.SampledFrom(g.cols[t]))}
	}
	if depth >= 3 || g.draw(0, 3) == 0 {
		return g.literal(t)
	}
	depth++
	// Forms shared by every type. At least one result or argument is not a bare NULL, because
	// Postgres types an all-NULL CASE or COALESCE as text and then coerces it loosely.
	switch g.draw(0, 9) {
	case 0:
		c := &ast.Case{}
		if g.bool() {
			ot := g.anyType()
			c.Operand = g.expr(ot, s, depth)
			for i := range g.draw(1, 2) {
				c.Whens = append(c.Whens, ast.When{Cond: g.expr(ot, s.withNull(true), depth), Result: g.expr(t, s.withNull(i > 0), depth)})
			}
		} else {
			for i := range g.draw(1, 2) {
				c.Whens = append(c.Whens, ast.When{Cond: g.expr(value.Bool, s, depth), Result: g.expr(t, s.withNull(i > 0), depth)})
			}
		}
		if g.bool() {
			c.Else = g.expr(t, s.withNull(true), depth)
		}
		return c
	case 1:
		n := g.draw(1, 3)
		var args []ast.Expr
		for i := range n {
			args = append(args, g.expr(t, s.withNull(i < n-1), depth))
		}
		return &ast.Call{Name: "coalesce", Args: args}
	}
	switch t {
	case value.Int:
		switch g.draw(0, 7) {
		case 0:
			return &ast.Call{Name: "length", Args: []ast.Expr{g.expr(hegel.Draw(g.ht, hegel.SampledFrom([]value.Type{value.String, value.Bytes})), s.withNull(true), depth)}}
		case 1:
			if s.safe {
				return g.literal(t)
			}
			// Never a bare NULL: Postgres types abs(NULL) as double precision.
			return &ast.Call{Name: "abs", Args: []ast.Expr{g.expr(t, s, depth)}}
		case 2:
			if s.safe {
				return g.literal(t)
			}
			return &ast.Unary{Op: ast.Neg, Expr: g.expr(t, s, depth)}
		case 3:
			if s.safe {
				return g.literal(t)
			}
			return &ast.Cast{Expr: g.expr(value.Float, s.withNull(true), depth), Type: value.Int}
		case 4:
			// A string that came from an int reads back as that int.
			return &ast.Cast{Expr: &ast.Cast{Expr: g.expr(value.Int, s.withNull(true), depth), Type: value.String}, Type: value.Int}
		default:
			if s.safe {
				return g.literal(t)
			}
			op := hegel.Draw(g.ht, hegel.SampledFrom([]ast.BinaryOp{ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Mod}))
			return &ast.Binary{Op: op, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		}
	case value.Float:
		switch g.draw(0, 6) {
		case 0:
			return &ast.Call{Name: "abs", Args: []ast.Expr{g.expr(t, s.withNull(true), depth)}}
		case 1:
			return &ast.Unary{Op: ast.Neg, Expr: g.expr(t, s, depth)}
		case 2:
			return &ast.Cast{Expr: g.expr(value.Int, s.withNull(true), depth), Type: value.Float}
		case 3:
			return &ast.Cast{Expr: &ast.Cast{Expr: g.expr(value.Float, s.withNull(true), depth), Type: value.String}, Type: value.Float}
		case 4:
			// An integer literal adapts to a float.
			return &ast.Binary{Op: ast.Add, Left: g.expr(t, s, depth), Right: g.literal(value.Int)}
		default:
			if s.safe {
				return g.literal(t)
			}
			op := hegel.Draw(g.ht, hegel.SampledFrom([]ast.BinaryOp{ast.Add, ast.Sub, ast.Mul, ast.Div}))
			return &ast.Binary{Op: op, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		}
	case value.Bool:
		switch g.draw(0, 7) {
		case 0:
			return &ast.Unary{Op: ast.Not, Expr: g.expr(t, s.withNull(true), depth)}
		case 1:
			op := hegel.Draw(g.ht, hegel.SampledFrom([]ast.BinaryOp{ast.And, ast.Or}))
			return &ast.Binary{Op: op, Left: g.expr(t, s.withNull(true), depth), Right: g.expr(t, s.withNull(true), depth)}
		case 2:
			return &ast.IsNull{Expr: g.expr(g.anyType(), s.withNull(true), depth), Not: g.bool()}
		case 3:
			ot := g.anyType()
			return &ast.Between{Expr: g.expr(ot, s.withNull(true), depth), Low: g.expr(ot, s.withNull(true), depth), High: g.expr(ot, s.withNull(true), depth), Not: g.bool()}
		case 4:
			ot := g.anyType()
			in := &ast.In{Expr: g.expr(ot, s, depth), Not: g.bool()}
			for range g.draw(1, 3) {
				in.List = append(in.List, g.expr(ot, s.withNull(true), depth))
			}
			return in
		case 5:
			pattern := hegel.Draw(g.ht, hegel.Text().Alphabet("ab%_").MaxSize(5))
			if g.bool() {
				pattern = hegel.Draw(g.ht, hegel.Text().Alphabet("aA%_\\").MaxSize(5))
			}
			return &ast.Like{Expr: g.expr(value.String, s.withNull(true), depth), Pattern: &ast.Literal{Value: value.NewString(pattern)}, Not: g.bool()}
		default:
			ot := g.anyType()
			op := hegel.Draw(g.ht, hegel.SampledFrom([]ast.BinaryOp{ast.Eq, ast.Ne, ast.Lt, ast.Le, ast.Gt, ast.Ge}))
			return &ast.Binary{Op: op, Left: g.expr(ot, s, depth), Right: g.expr(ot, s.withNull(true), depth)}
		}
	case value.String:
		switch g.draw(0, 4) {
		case 0:
			return &ast.Binary{Op: ast.Concat, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		case 1:
			if !g.ascii {
				return g.literal(t)
			}
			name := hegel.Draw(g.ht, hegel.SampledFrom([]string{"lower", "upper"}))
			return &ast.Call{Name: name, Args: []ast.Expr{g.expr(t, s.withNull(true), depth)}}
		case 2:
			from := hegel.Draw(g.ht, hegel.SampledFrom([]value.Type{value.Int, value.Float, value.Bool}))
			return &ast.Cast{Expr: g.expr(from, s.withNull(true), depth), Type: value.String}
		default:
			return g.literal(t)
		}
	case value.Bytes:
		if g.bool() {
			return &ast.Binary{Op: ast.Concat, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		}
		return g.literal(t)
	case value.Time:
		return g.literal(t)
	}
	panic(t)
}

// anyType that has a column when columns are in scope, so comparisons tend to touch data.
func (g *gen) anyType() value.Type {
	var withCols []value.Type
	for _, t := range allTypes {
		if len(g.cols[t]) > 0 {
			withCols = append(withCols, t)
		}
	}
	if len(withCols) > 0 && g.draw(0, 2) > 0 {
		return withCols[g.draw(0, len(withCols)-1)]
	}
	return hegel.Draw(g.ht, hegel.SampledFrom(allTypes))
}

// row of literal values for every column, respecting NOT NULL most of the time.
func (g *gen) row() []ast.Expr {
	row := make([]ast.Expr, len(g.table.Columns))
	for i, c := range g.table.Columns {
		if !c.NotNull && !g.isKey(c.Name) && g.draw(0, 4) == 0 {
			row[i] = &ast.Literal{Value: value.NewNull(value.Unknown)}
			continue
		}
		if g.draw(0, 79) == 0 {
			// A NULL into a NOT NULL column or a key column is an error on both sides.
			row[i] = &ast.Literal{Value: value.NewNull(value.Unknown)}
			continue
		}
		row[i] = g.literal(c.Type)
	}
	return row
}

func (g *gen) insert(n int) *ast.Insert {
	ins := &ast.Insert{Table: tableName}
	seen := map[string]bool{}
	for range max(n, 1) {
		row := g.row()
		// Rows mostly get distinct keys, so that inserts mostly succeed.
		key := g.rowKey(row)
		for try := 0; seen[key] && try < 3; try++ {
			row = g.rowKey2(row)
			key = g.rowKey(row)
		}
		seen[key] = true
		ins.Rows = append(ins.Rows, row)
	}
	return ins
}

// rowKey renders the key columns of a row of literals.
func (g *gen) rowKey(row []ast.Expr) string {
	var parts []string
	for i, c := range g.table.Columns {
		if g.isKey(c.Name) {
			parts = append(parts, ast.RenderExpr(row[i], ast.Maragudb))
		}
	}
	return fmt.Sprint(parts)
}

// rowKey2 redraws the key columns of a row.
func (g *gen) rowKey2(row []ast.Expr) []ast.Expr {
	out := append([]ast.Expr{}, row...)
	for i, c := range g.table.Columns {
		if g.isKey(c.Name) {
			out[i] = g.literal(c.Type)
		}
	}
	return out
}

func (g *gen) where(s scope) ast.Expr {
	if g.draw(0, 3) == 0 {
		return nil
	}
	if g.draw(0, 2) == 0 && len(g.table.PrimaryKey) > 0 {
		// A comparison on a key column, so that key access paths are exercised.
		name := hegel.Draw(g.ht, hegel.SampledFrom(g.table.PrimaryKey))
		op := hegel.Draw(g.ht, hegel.SampledFrom([]ast.BinaryOp{ast.Eq, ast.Eq, ast.Lt, ast.Le, ast.Gt, ast.Ge}))
		var e ast.Expr = &ast.Binary{Op: op, Left: &ast.ColumnRef{Name: name}, Right: g.literal(g.column(name).Type)}
		if g.bool() {
			e = &ast.Binary{Op: ast.And, Left: e, Right: g.expr(value.Bool, s, 1)}
		}
		return e
	}
	return g.expr(value.Bool, s, 0)
}

func (g *gen) statement() ast.Statement {
	switch g.draw(0, 5) {
	case 0:
		return g.insert(g.draw(1, 3))
	case 1:
		u := &ast.Update{Table: tableName}
		var nonKey []string
		for _, c := range g.table.Columns {
			if !g.isKey(c.Name) {
				nonKey = append(nonKey, c.Name)
			}
		}
		if len(nonKey) == 0 {
			return g.insert(1)
		}
		seen := map[string]bool{}
		for range g.draw(1, 2) {
			name := hegel.Draw(g.ht, hegel.SampledFrom(nonKey))
			if seen[name] {
				continue
			}
			seen[name] = true
			u.Set = append(u.Set, ast.Assignment{Column: name, Value: g.expr(g.column(name).Type, scope{columns: true, null: true}, 0)})
		}
		u.Where = g.where(scope{columns: true})
		return u
	case 2:
		return &ast.Delete{Table: tableName, Where: g.where(scope{columns: true})}
	default:
		return g.selectStmt()
	}
}

func (g *gen) selectStmt() *ast.Select {
	s := &ast.Select{From: tableName}
	limited := g.draw(0, 2) == 0
	sc := scope{columns: true, safe: limited}
	n := g.draw(1, 3)
	for i := range n {
		if g.draw(0, 3) == 0 {
			s.Items = append(s.Items, ast.SelectItem{Star: true})
			continue
		}
		item := ast.SelectItem{Expr: g.expr(g.anyType(), sc.withNull(true), 0)}
		if g.bool() {
			item.Alias = fmt.Sprint("a", i)
		}
		s.Items = append(s.Items, item)
	}
	s.Where = g.where(sc)
	for range g.draw(0, 2) {
		o := ast.OrderItem{Desc: g.bool(), Nulls: ast.NullsOrder(g.draw(0, 2))}
		switch g.draw(0, 2) {
		case 0:
			o.Expr = &ast.Literal{Value: value.NewInt(int64(g.draw(1, g.outputs(s))))}
		case 1:
			o.Expr = &ast.ColumnRef{Name: g.table.Columns[g.draw(0, len(g.table.Columns)-1)].Name}
		default:
			o.Expr = g.expr(g.anyType(), sc, 1)
		}
		s.OrderBy = append(s.OrderBy, o)
	}
	if limited {
		// The order must be fully determined, so every key column is a sort key. An output
		// column named like a key column would shadow it in ORDER BY, so then there is no LIMIT.
		for _, k := range g.table.PrimaryKey {
			if shadowed(s, k) {
				return s
			}
		}
		for _, k := range g.table.PrimaryKey {
			s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Name: k}, Desc: g.bool()})
		}
		if g.bool() {
			s.Limit = &ast.Literal{Value: value.NewInt(int64(g.draw(0, 4)))}
		}
		if g.bool() {
			s.Offset = &ast.Literal{Value: value.NewInt(int64(g.draw(0, 3)))}
		}
		if s.Limit == nil && s.Offset == nil {
			s.Limit = &ast.Literal{Value: value.NewInt(2)}
		}
	}
	return s
}

// outputs counts the output columns of a SELECT with its stars expanded against the table.
func (g *gen) outputs(s *ast.Select) int {
	n := 0
	for _, item := range s.Items {
		if item.Star {
			n += len(g.table.Columns)
		} else {
			n++
		}
	}
	return n
}

// shadowed reports whether an expression item of s is output under the name of column k, so
// that ORDER BY k would sort by that expression instead of the column.
func shadowed(s *ast.Select, k string) bool {
	for _, item := range s.Items {
		if item.Star {
			continue
		}
		name := item.Alias
		if name == "" {
			name = exec.OutputName(item.Expr)
		}
		if name == k {
			return true
		}
	}
	return false
}

// FullyOrdered reports whether the ORDER BY of s determines the row order completely, which
// is when every primary key column is a sort key referenced by name and not shadowed by an
// output column of the same name.
func FullyOrdered(s *ast.Select, pk []string) bool {
	if len(s.OrderBy) == 0 {
		return false
	}
	for _, k := range pk {
		if shadowed(s, k) {
			return false
		}
		found := false
		for _, o := range s.OrderBy {
			if ref, ok := o.Expr.(*ast.ColumnRef); ok && ref.Name == k {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
