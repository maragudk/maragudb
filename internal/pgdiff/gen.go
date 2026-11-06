package pgdiff

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
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
}

// Table name used by every case.
const tableName = "t"

var allTypes = []value.Type{value.Int, value.Float, value.Bool, value.String, value.Bytes, value.Time}

// gen generates one case. It aims at statements that maragudb accepts, so that most of them
// are compared with Postgres, and now and then draws a form that maragudb rejects with
// [exec.ErrUnsupported], such as a LIMIT whose rows depend on Postgres's plan, so that the
// rejections are exercised too.
type gen struct {
	src   Source
	table *ast.CreateTable
	// cols by type.
	cols map[value.Type][]string
}

// Source of the generator's choices.
type Source interface {
	// Int in [min, max].
	Int(min, max int64) int64
	// Float that is finite.
	Float() float64
	// Text of at most max characters from alphabet, or of any characters but NUL when
	// alphabet is empty.
	Text(alphabet string, max int) string
	// Bytes, at most max of them.
	Bytes(max int) []byte
}

// HegelSource draws the choices from a hegel test case, which shrinks a failing case.
func HegelSource(ht hegel.TestCase) Source {
	return hegelSource{ht: ht}
}

type hegelSource struct {
	ht hegel.TestCase
}

func (h hegelSource) Int(min, max int64) int64 {
	return hegel.Draw(h.ht, hegel.Integers(min, max))
}

func (h hegelSource) Float() float64 {
	return hegel.Draw(h.ht, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false))
}

func (h hegelSource) Text(alphabet string, max int) string {
	if alphabet == "" {
		return hegel.Draw(h.ht, hegel.Text().MaxSize(max).ExcludeCharacters("\x00"))
	}
	return hegel.Draw(h.ht, hegel.Text().Alphabet(alphabet).MaxSize(max))
}

func (h hegelSource) Bytes(max int) []byte {
	return hegel.Draw(h.ht, hegel.Binary(0, max))
}

// RandSource draws the choices from r, which gives the same cases for the same seed on every
// run, unlike hegel.
func RandSource(r *rand.Rand) Source {
	return randSource{r: r}
}

type randSource struct {
	r *rand.Rand
}

func (s randSource) Int(min, max int64) int64 {
	return min + s.r.Int64N(max-min+1)
}

func (s randSource) Float() float64 {
	for {
		if f := math.Float64frombits(s.r.Uint64()); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f
		}
	}
}

func (s randSource) Text(alphabet string, max int) string {
	runes := []rune(alphabet)
	var b strings.Builder
	for range s.r.IntN(max + 1) {
		switch {
		case len(runes) > 0:
			b.WriteRune(runes[s.r.IntN(len(runes))])
		case s.r.IntN(2) == 0:
			b.WriteRune(rune(1 + s.r.IntN(127)))
		default:
			r := rune(0x80 + s.r.IntN(0x2000))
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (s randSource) Bytes(max int) []byte {
	out := make([]byte, s.r.IntN(max+1))
	for i := range out {
		out[i] = byte(s.r.UintN(256))
	}
	return out
}

// Generate a case.
func Generate(src Source) Case {
	g := &gen{src: src, cols: map[value.Type][]string{}}
	g.schema()
	c := Case{Create: g.table}
	c.Statements = append(c.Statements, g.insert(g.draw(0, 6)))
	n := g.draw(1, 6)
	// A third of the cases run some statements in an explicit transaction, which ends with a
	// COMMIT, a ROLLBACK, or not at all.
	begin, end := -1, -1
	if g.draw(0, 2) == 0 {
		begin = g.draw(0, n-1)
		end = g.draw(begin, n)
	}
	for i := range n + 1 {
		if i == begin {
			c.Statements = append(c.Statements, &ast.Begin{ReadOnly: g.draw(0, 4) == 0})
		}
		if i == end && i > begin {
			if g.bool() {
				c.Statements = append(c.Statements, &ast.Commit{})
			} else {
				c.Statements = append(c.Statements, &ast.Rollback{})
			}
		}
		if i < n {
			c.Statements = append(c.Statements, g.statement())
		}
	}
	return c
}

func (g *gen) draw(min, max int) int {
	return int(g.src.Int(int64(min), int64(max)))
}

func (g *gen) int64n(min, max int64) int64 {
	return g.src.Int(min, max)
}

func (g *gen) bool() bool {
	return g.src.Int(0, 1) == 1
}

// pick one of xs.
func pick[T any](g *gen, xs []T) T {
	return xs[g.draw(0, len(xs)-1)]
}

// rarely is true once in n draws.
func (g *gen) rarely(n int) bool {
	return g.draw(0, n-1) == 0
}

func (g *gen) schema() {
	n := g.draw(1, 5)
	t := &ast.CreateTable{Name: tableName}
	for i := range n {
		typ := pick(g, allTypes)
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

// literal of type t, written in one of the ways Postgres reads as that type: an integer as its
// digits, which is an integer or a bigint by its size, or cast to bigint; a float as the cast
// of its text, as a numeric literal, or as a numeric literal cast to double precision; and the
// other types as a quoted literal, which takes its type from the context, or as its cast.
func (g *gen) literal(t value.Type) ast.Expr {
	v := g.value(t)
	switch t {
	case value.Int:
		lit := &ast.Literal{Value: v}
		if g.rarely(4) {
			return &ast.Cast{Expr: lit, Type: value.Int, Style: ast.CastStyle(g.draw(0, 1))}
		}
		return lit
	case value.Float:
		text := value.FormatFloat(v.F)
		finite := !math.IsNaN(v.F) && !math.IsInf(v.F, 0) && !strings.HasPrefix(text, "-")
		switch {
		case finite && g.rarely(5):
			// A numeric literal, which is double precision only next to one.
			return &ast.Literal{Value: value.NewNumeric(numericText(text))}
		case finite && g.rarely(4):
			return &ast.Cast{Expr: &ast.Literal{Value: value.NewNumeric(numericText(text))}, Type: value.Float, Style: ast.CastColons}
		}
		return g.typedText(text, t)
	case value.Bool:
		if g.rarely(8) {
			return g.typedText(pick(g, []string{"t", "true", "no", "off", "1", " yes "}), t)
		}
		return &ast.Literal{Value: v}
	}
	text := value.Format(v)
	if t == value.Time && g.rarely(3) {
		// The ISO 8601 forms that Postgres reads, with a T, a zone of its own, or more digits than
		// microseconds, which Postgres rounds.
		at := v.T.Add(time.Duration(g.draw(0, 999))).In(time.FixedZone("", 3600*g.draw(-12, 14)))
		text = at.Format(pick(g, []string{"2006-01-02T15:04:05.999999Z07:00", "2006-01-02 15:04:05.999999999-0700", "2006-01-02 15:04:05.999999-07", "2006-01-02 15:04:05"}))
	}
	if t != value.String && g.rarely(3) || t == value.String && g.rarely(6) {
		return g.typedText(text, t)
	}
	return &ast.Literal{Value: value.NewUnknown(text)}
}

// numericText writes the text of a float as a numeric literal, with a decimal point.
func numericText(s string) string {
	if !strings.ContainsAny(s, ".e") {
		s += ".0"
	}
	return s
}

// typedText is a quoted literal cast to type t.
func (g *gen) typedText(text string, t value.Type) ast.Expr {
	return &ast.Cast{Expr: &ast.Literal{Value: value.NewUnknown(text)}, Type: t, Style: ast.CastStyle(g.draw(0, 2))}
}

func (g *gen) value(t value.Type) value.Value {
	switch t {
	case value.Int:
		switch g.draw(0, 9) {
		case 0:
			return value.NewInt(pick(g, []int64{math.MinInt64, math.MaxInt64, math.MinInt64 + 1, math.MaxInt64 - 1, 1 << 32, -(1 << 32), math.MaxInt32, math.MinInt32, math.MaxInt32 + 1}))
		case 1, 2:
			return value.NewInt(g.int64n(-100000, 100000))
		default:
			return value.NewInt(g.int64n(-5, 5))
		}
	case value.Float:
		switch g.draw(0, 9) {
		case 0:
			return value.NewFloat(pick(g, []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.Copysign(0, -1), math.MaxFloat64, -math.MaxFloat64, math.SmallestNonzeroFloat64, 1e308, 1e-308, 1e15, 1e-5, 123456789012345678}))
		case 1, 2:
			return value.NewFloat(g.src.Float())
		case 3:
			return value.NewFloat(float64(g.draw(-5, 5)))
		default:
			return value.NewFloat(float64(g.draw(-50, 50)) / 4)
		}
	case value.Bool:
		return value.NewBool(g.bool())
	case value.String:
		if g.rarely(3) {
			return value.NewString(g.src.Text("", 6))
		}
		return value.NewString(g.src.Text("abcAB _%\\'\"ÄéΣ", 6))
	case value.Bytes:
		return value.NewBytes(g.src.Bytes(4))
	case value.Time:
		sec := g.int64n(-2208988800, 4102444800)
		nsec := g.int64n(0, 999999999)
		if g.draw(0, 3) == 0 {
			nsec = 0
		}
		return value.NewTime(time.Unix(sec, nsec))
	}
	panic(t)
}

// scope of an expression.
type scope struct {
	// columns may be referenced.
	columns bool
	// safe forbids expressions that can fail at runtime, which maragudb rejects where the rows
	// Postgres evaluates them on depend on its plan, as under a LIMIT.
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
		return &ast.ColumnRef{Name: pick(g, g.cols[t])}
	}
	if depth >= 3 || g.draw(0, 3) == 0 {
		return g.literal(t)
	}
	depth++
	// Forms shared by every type.
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
			return &ast.Call{Name: "length", Args: []ast.Expr{g.expr(pick(g, []value.Type{value.String, value.Bytes}), s.withNull(true), depth)}}
		case 1:
			if s.safe {
				return g.literal(t)
			}
			return &ast.Call{Name: "abs", Args: []ast.Expr{g.expr(t, s.withNull(g.rarely(4)), depth)}}
		case 2:
			if s.safe {
				return g.literal(t)
			}
			return &ast.Unary{Op: pick(g, []ast.UnaryOp{ast.Neg, ast.Neg, ast.Pos}), Expr: g.expr(t, s, depth)}
		case 3:
			if s.safe {
				return g.literal(t)
			}
			return &ast.Cast{Expr: g.expr(value.Float, s.withNull(true), depth), Type: value.Int, Style: ast.CastStyle(g.draw(0, 1))}
		case 4:
			if s.safe {
				return g.literal(t)
			}
			// A string that came from a bigint reads back as that bigint, and other text mostly
			// fails.
			return &ast.Cast{Expr: g.expr(value.String, s.withNull(true), depth), Type: value.Int}
		default:
			if s.safe {
				return g.literal(t)
			}
			op := pick(g, []ast.BinaryOp{ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Mod})
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
			if s.safe {
				return g.literal(t)
			}
			return &ast.Cast{Expr: &ast.Cast{Expr: g.expr(value.Float, s.withNull(true), depth), Type: value.String}, Type: value.Float}
		case 4:
			// An integer adapts to a double precision.
			return &ast.Binary{Op: ast.Add, Left: g.expr(t, s, depth), Right: g.literal(value.Int)}
		default:
			if s.safe {
				return g.literal(t)
			}
			op := pick(g, []ast.BinaryOp{ast.Add, ast.Sub, ast.Mul, ast.Div})
			return &ast.Binary{Op: op, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		}
	case value.Bool:
		switch g.draw(0, 7) {
		case 0:
			return &ast.Unary{Op: ast.Not, Expr: g.expr(t, s.withNull(true), depth)}
		case 1:
			op := pick(g, []ast.BinaryOp{ast.And, ast.Or})
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
			pattern := g.src.Text("ab%_", 5)
			if g.bool() && !s.safe {
				pattern = g.src.Text("aA%_\\", 5)
			}
			return &ast.Like{Expr: g.expr(value.String, s.withNull(true), depth), Pattern: &ast.Literal{Value: value.NewUnknown(pattern)}, Not: g.bool()}
		default:
			ot := g.anyType()
			op := pick(g, []ast.BinaryOp{ast.Eq, ast.Ne, ast.Lt, ast.Le, ast.Gt, ast.Ge})
			return &ast.Binary{Op: op, Left: g.expr(ot, s, depth), Right: g.expr(ot, s.withNull(true), depth)}
		}
	case value.String:
		switch g.draw(0, 4) {
		case 0:
			// || writes a value of any type next to text as text.
			rt := t
			if g.rarely(3) {
				rt = g.anyType()
			}
			return &ast.Binary{Op: ast.Concat, Left: g.expr(t, s, depth), Right: g.expr(rt, s.withNull(true), depth)}
		case 1:
			name := pick(g, []string{"lower", "upper"})
			return &ast.Call{Name: name, Args: []ast.Expr{g.expr(t, s.withNull(true), depth)}}
		case 2:
			return &ast.Cast{Expr: g.expr(g.anyType(), s.withNull(true), depth), Type: value.String, Style: ast.CastStyle(g.draw(0, 1))}
		default:
			return g.literal(t)
		}
	case value.Bytes:
		switch g.draw(0, 2) {
		case 0:
			return &ast.Binary{Op: ast.Concat, Left: g.expr(t, s, depth), Right: g.expr(t, s.withNull(true), depth)}
		case 1:
			if !s.safe {
				// Text that is a bytea in hex or escape format, or not.
				return &ast.Cast{Expr: g.expr(value.String, s.withNull(true), depth), Type: value.Bytes}
			}
		}
		return g.literal(t)
	case value.Time:
		if g.rarely(3) && !s.safe {
			return &ast.Cast{Expr: &ast.Cast{Expr: g.expr(value.Time, s.withNull(true), depth), Type: value.String}, Type: value.Time}
		}
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
	return pick(g, allTypes)
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
			parts = append(parts, ast.RenderExpr(row[i]))
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

// where draws a WHERE clause, or none. Several conditions where one can fail make an outcome
// that can depend on the order in which Postgres evaluates them, which maragudb rejects, so
// they are mostly combined only with conditions that cannot fail.
func (g *gen) where(s scope) ast.Expr {
	if g.draw(0, 3) == 0 {
		return nil
	}
	mayFail := !s.safe
	if !g.rarely(6) {
		s.safe = true
	}
	if g.draw(0, 2) == 0 && len(g.table.PrimaryKey) > 0 {
		e := g.keyCondition()
		if g.bool() {
			if mayFail && g.bool() {
				// A condition that can fail next to the key condition, which both evaluate only on
				// the rows that the key condition selects.
				s.safe = false
			}
			e = &ast.Binary{Op: ast.And, Left: e, Right: g.expr(value.Bool, s, 1)}
		}
		return e
	}
	if g.bool() {
		// A single condition is evaluated on every row in every plan, so it may fail.
		s.safe = false
	}
	return g.expr(value.Bool, s, 0)
}

// keyCondition on the primary key columns, so that key access paths are exercised: a
// comparison of a key column with a literal, an IN of literals, or a range of two comparisons.
func (g *gen) keyCondition() ast.Expr {
	name := pick(g, g.table.PrimaryKey)
	col := &ast.ColumnRef{Name: name}
	typ := g.column(name).Type
	switch g.draw(0, 3) {
	case 0:
		in := &ast.In{Expr: col}
		for range g.draw(2, 3) {
			in.List = append(in.List, g.literal(typ))
		}
		return in
	case 1:
		return &ast.Binary{Op: ast.And,
			Left:  &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Gt, ast.Ge}), Left: col, Right: g.literal(typ)},
			Right: &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Lt, ast.Le}), Left: col, Right: g.literal(typ)},
		}
	}
	op := pick(g, []ast.BinaryOp{ast.Eq, ast.Eq, ast.Lt, ast.Le, ast.Gt, ast.Ge})
	return &ast.Binary{Op: op, Left: col, Right: g.literal(typ)}
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
		targets := nonKey
		if len(nonKey) == 0 || g.rarely(5) {
			// An update of key columns, whose outcome may depend on the order of the rows.
			targets = g.table.PrimaryKey
		}
		seen := map[string]bool{}
		for range g.draw(1, 2) {
			name := pick(g, targets)
			if seen[name] {
				continue
			}
			seen[name] = true
			u.Set = append(u.Set, ast.Assignment{Column: name, Value: g.expr(g.column(name).Type, scope{columns: true, null: !g.isKey(name)}, 0)})
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
	sc := scope{columns: true, safe: limited && !g.rarely(8)}
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
			o.Expr = &ast.Literal{Value: value.NewInt4(int32(g.draw(1, g.outputs(s))))}
		case 1:
			o.Expr = &ast.ColumnRef{Name: g.table.Columns[g.draw(0, len(g.table.Columns)-1)].Name}
		default:
			o.Expr = g.expr(g.anyType(), sc, 1)
		}
		s.OrderBy = append(s.OrderBy, o)
	}
	if limited {
		// The order must be fully determined, so every key column is a sort key, except now and
		// then. An output column named like a key column would shadow it in ORDER BY, so then
		// there is no LIMIT.
		for _, k := range g.table.PrimaryKey {
			if shadowed(s, k) {
				return s
			}
		}
		if !g.rarely(10) {
			for _, k := range g.table.PrimaryKey {
				s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Name: k}, Desc: g.bool()})
			}
		}
		if g.bool() {
			s.Limit = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 4)))}
		}
		if g.bool() {
			s.Offset = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 3)))}
		}
		if s.Limit == nil && s.Offset == nil {
			s.Limit = &ast.Literal{Value: value.NewInt4(2)}
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
		if ref, ok := item.Expr.(*ast.ColumnRef); ok && ref.Name == k {
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
// is when every primary key column is a sort key, referenced by a name that no output column
// shadows, or by the position of an output column that is the key column itself.
func FullyOrdered(s *ast.Select, pk []string) bool {
	if len(s.OrderBy) == 0 {
		return false
	}
	for _, k := range pk {
		found := false
		for _, o := range s.OrderBy {
			if ref, ok := o.Expr.(*ast.ColumnRef); ok && ref.Name == k && !shadowed(s, k) {
				found = true
			}
			if i, ok := orderPosition(o.Expr); ok && i < len(s.Items) && !starBefore(s, i) {
				if ref, ok := s.Items[i].Expr.(*ast.ColumnRef); ok && ref.Name == k {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// orderPosition is the index of the output column that an ORDER BY expression refers to by
// its position, as Postgres reads it: an integer literal, of which -2147483648 is none.
func orderPosition(e ast.Expr) (int, bool) {
	lit, ok := e.(*ast.Literal)
	if !ok || lit.Value.Null || lit.Value.Type != value.Int4 || lit.Value.I < 1 {
		return 0, false
	}
	return int(lit.Value.I) - 1, true
}

// starBefore reports whether an item of s up to item i is a *, after which the positions of the
// items are not the positions of the output columns.
func starBefore(s *ast.Select, i int) bool {
	for _, item := range s.Items[:i+1] {
		if item.Star {
			return true
		}
	}
	return false
}

// sortedColumns are the output columns, in order, that the ORDER BY of s sorts by first: the
// sort keys up to the first one that is not an output column, by position or by name, given the
// names of the output columns. Rows that ORDER BY sorts are in the same order of these columns
// in every plan, however the ties between them are broken.
func sortedColumns(s *ast.Select, columns []string) []int {
	var out []int
	for _, o := range s.OrderBy {
		i := -1
		switch e := o.Expr.(type) {
		case *ast.Literal:
			if p, ok := orderPosition(e); ok && p < len(columns) {
				i = p
			}
		case *ast.ColumnRef:
			// Postgres resolves a name in ORDER BY to an output column first.
			i = slices.Index(columns, e.Name)
		}
		if i < 0 {
			break
		}
		out = append(out, i)
	}
	return out
}
