package pgdiff

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"regexp"
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
	// Parent is a table that Create references with a foreign key, created before it, or nil.
	Parent *ast.CreateTable
	Create *ast.CreateTable
	// NowColumn is the column of Create whose default is now(), whose values differ between the
	// two databases and are compared only with now(), or "".
	NowColumn string
	// Statements run in order after the tables are created. Mutations are followed by a state
	// check on both sides.
	Statements []ast.Statement
	// Args are the values of the parameters of each statement, by the statement's index, or nil
	// for a statement without parameters.
	Args [][]any
}

// Tables used by the cases.
const (
	tableName  = "t"
	parentName = "p"
)

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
	// indexed are the columns of the indexes the case may have, the primary key first, which key
	// conditions compare.
	indexed [][]string
	// indexes are the names of the indexes the case created with a name.
	indexes []string
	// parent is the generator of the table that the table references, which shares the source,
	// or nil.
	parent *gen
	// nowColumn is the column whose default is now(), or "".
	nowColumn string
	// rows are the rows that INSERTs of the table wrote, or tried to.
	rows [][]ast.Expr
	// args are the values of the parameters of the statement being generated, shared with the
	// generator of the other table.
	args *[]any
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
	g := &gen{src: src, cols: map[value.Type][]string{}, args: &[]any{}}
	var parent *ast.CreateTable
	if g.bool() {
		g.parent = &gen{src: src, cols: map[value.Type][]string{}, args: g.args}
		g.parent.parentSchema()
		parent = g.parent.table
	}
	g.schema()
	g.constraints()
	c := Case{Parent: parent, Create: g.table, NowColumn: g.nowColumn}
	add := func(st ast.Statement) {
		// Postgres fails a statement that does not use each of its parameters.
		text := ast.Render(st)
		for i := range *g.args {
			if !regexp.MustCompile(fmt.Sprintf(`\$%d\b`, i+1)).MatchString(text) {
				panic(fmt.Sprintf("pgdiff: the statement does not use its parameter $%d: %s", i+1, text))
			}
		}
		c.Statements = append(c.Statements, st)
		c.Args = append(c.Args, *g.args)
		*g.args = nil
	}
	if g.parent != nil {
		add(g.parent.insert(g.draw(1, 5)))
	}
	add(g.insert(g.draw(0, 6)))
	// Indexes on the rows, so that later statements use them, and so that a unique index on rows
	// that repeat its values fails.
	for range g.draw(0, 2) {
		add(g.createIndex())
	}
	// Two of the kinds of statements are joins, two are grouped, and two have subqueries, so that
	// the others are as frequent as they were before there were any of these.
	n := g.draw(1, 12)
	// A third of the cases run some statements in an explicit transaction, which ends with a
	// COMMIT, a ROLLBACK, or not at all.
	begin, end := -1, -1
	if g.draw(0, 2) == 0 {
		begin = g.draw(0, n-1)
		end = g.draw(begin, n)
	}
	for i := range n + 1 {
		if i == begin {
			add(&ast.Begin{ReadOnly: g.draw(0, 4) == 0})
		}
		if i == end && i > begin {
			if g.bool() {
				add(&ast.Commit{})
			} else {
				add(&ast.Rollback{})
			}
		}
		if i < n {
			add(g.statement())
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
	if g.table != nil {
		t.Name = g.table.Name
	}
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
	// Constraints get names that nothing else in the case has, so that the table is always
	// created.
	names := []string{"k0", "k1", "k2"}
	if g.rarely(6) {
		t.PrimaryKeyName = names[0]
	}
	g.indexed = [][]string{t.PrimaryKey}
	g.table = t
	// Now and then UNIQUE constraints, which may repeat each other or the primary key.
	for range g.draw(0, 2) {
		if g.draw(0, 2) > 0 {
			break
		}
		// A column twice is an error in a constraint, which would fail the CREATE TABLE.
		u := ast.UniqueConstraint{Columns: slices.Compact(g.indexColumns()), NullsNotDistinct: g.rarely(4)}
		if g.rarely(3) {
			u.Name = names[1+len(t.Unique)]
			g.indexes = append(g.indexes, u.Name)
		}
		t.Unique = append(t.Unique, u)
		g.indexed = append(g.indexed, u.Columns)
	}
}

// indexColumns are one or two columns for an index, now and then the same one twice.
func (g *gen) indexColumns() []string {
	names := g.columnNames()
	cols := []string{pick(g, names)}
	if g.bool() {
		cols = append(cols, pick(g, names))
	}
	return cols
}

// columnNames of the table, without the column whose default is now(), whose values the
// statements do not read.
func (g *gen) columnNames() []string {
	var names []string
	for _, c := range g.table.Columns {
		if c.Name != g.nowColumn {
			names = append(names, c.Name)
		}
	}
	return names
}

// parentSchema is a table that the table of the case references: a primary key of one or two
// columns, mostly bigint, so that the referencing values match it often, and now and then a
// unique constraint.
func (g *gen) parentSchema() {
	t := &ast.CreateTable{Name: parentName}
	g.table = t
	for i := range g.draw(1, 3) {
		typ := value.Int
		if i > 0 || g.rarely(4) {
			typ = pick(g, []value.Type{value.Int, value.Bool, value.String})
		}
		name := fmt.Sprint("c", i)
		t.Columns = append(t.Columns, ast.ColumnDef{Name: name, Type: typ, NotNull: g.rarely(4)})
		g.cols[typ] = append(g.cols[typ], name)
	}
	t.PrimaryKey = []string{"c0"}
	if len(t.Columns) > 1 && g.rarely(3) {
		t.PrimaryKey = append(t.PrimaryKey, "c1")
	}
	if len(t.Columns) > 1 && g.rarely(3) {
		u := ast.UniqueConstraint{Columns: []string{t.Columns[len(t.Columns)-1].Name}}
		t.Unique = append(t.Unique, u)
	}
	if g.rarely(5) {
		g.table.Checks = append(g.table.Checks, ast.CheckConstraint{Expr: g.check()})
	}
	g.indexed = [][]string{t.PrimaryKey}
	for _, u := range t.Unique {
		g.indexed = append(g.indexed, u.Columns)
	}
}

// constraints adds defaults, a column whose default is now(), CHECK constraints, and foreign
// keys, to the parent table and to the table itself.
func (g *gen) constraints() {
	t := g.table
	for i, c := range t.Columns {
		if !g.isKey(c.Name) && g.rarely(4) {
			t.Columns[i].Default = g.literal(c.Type)
		}
	}
	if g.rarely(3) {
		g.nowColumn = "ts"
		def := pick(g, []ast.Expr{&ast.Call{Name: "now"}, &ast.Call{Name: "current_timestamp", Keyword: true}, &ast.Call{Name: "transaction_timestamp"}})
		t.Columns = append(t.Columns, ast.ColumnDef{Name: g.nowColumn, Type: value.Time, NotNull: g.rarely(3), Default: def})
	}
	for range g.draw(0, 2) {
		if !g.rarely(3) {
			break
		}
		c := ast.CheckConstraint{Expr: g.check()}
		if g.rarely(4) {
			c.Name = fmt.Sprint("k", 3+len(t.Checks))
		}
		t.Checks = append(t.Checks, c)
	}
	if g.parent != nil {
		if fk, ok := g.foreignKey(g.parent); ok {
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
	}
	if len(t.PrimaryKey) == 1 && g.rarely(3) {
		if fk, ok := g.foreignKey(g); ok {
			t.ForeignKeys = append(t.ForeignKeys, fk)
		}
	}
}

// check is the expression of a CHECK constraint, which mostly cannot fail, because the outcome
// of one that fails depends on its row, and one whose constant part fails is not supported.
func (g *gen) check() ast.Expr {
	return g.expr(value.Bool, scope{columns: true, safe: !g.rarely(4), null: true}, 1)
}

// foreignKey of the table to the primary key of the table of p, or to a unique constraint of it,
// on columns of the same types, which it adds where the table has none, with random actions. It
// reports false when the referenced columns have a type that a foreign key cannot have.
func (g *gen) foreignKey(p *gen) (ast.ForeignKey, bool) {
	fk := ast.ForeignKey{Table: p.table.Name}
	ref := p.table.PrimaryKey
	if len(p.table.Unique) > 0 && g.bool() {
		ref = p.table.Unique[0].Columns
		fk.RefColumns = ref
	}
	for _, r := range ref {
		typ := p.column(r).Type
		if typ == value.Float {
			return fk, false
		}
		var candidates []string
		for _, name := range g.cols[typ] {
			if !slices.Contains(fk.Columns, name) && (p != g || !slices.Contains(ref, name)) {
				candidates = append(candidates, name)
			}
		}
		if len(candidates) == 0 || g.rarely(4) {
			name := fmt.Sprint("f", len(g.table.Columns))
			g.table.Columns = append(g.table.Columns, ast.ColumnDef{Name: name, Type: typ})
			g.cols[typ] = append(g.cols[typ], name)
			candidates = []string{name}
		}
		fk.Columns = append(fk.Columns, pick(g, candidates))
	}
	if p != g && g.rarely(5) {
		fk.RefColumns = ref
	}
	fk.OnDelete = ast.Action(g.draw(0, 4))
	if (fk.OnDelete == ast.SetNull || fk.OnDelete == ast.SetDefault) && len(fk.Columns) > 1 && g.rarely(3) {
		fk.SetColumns = fk.Columns[:1]
	}
	fk.OnUpdate = ast.Action(g.draw(0, 1))
	if g.rarely(6) {
		fk.Name = fmt.Sprint("k", 6+len(g.table.ForeignKeys))
	}
	return fk, true
}

// indexName for a new index, one of a few, so that names clash now and then, also with the
// names Postgres chooses.
func (g *gen) indexName() string {
	name := pick(g, []string{"i0", "i1", "i2", "t_pkey", "t_c0_key", "t_c0_idx", "t"})
	g.indexes = append(g.indexes, name)
	return name
}

// createIndex of one or two columns, with a name or without.
func (g *gen) createIndex() *ast.CreateIndex {
	ci := &ast.CreateIndex{Table: g.table.Name, Unique: g.bool(), NullsNotDistinct: g.rarely(5)}
	if g.rarely(20) {
		ci.Table = pick(g, []string{"nope", "t_pkey"})
	}
	if g.bool() {
		ci.Name = g.indexName()
		ci.IfNotExists = g.rarely(3)
	}
	var names []string
	for _, name := range g.indexColumns() {
		if g.rarely(30) {
			name = "nope"
		}
		ci.Columns = append(ci.Columns, ast.IndexColumn{Name: name, Desc: g.bool(), Nulls: ast.NullsOrder(g.draw(0, 2))})
		names = append(names, name)
	}
	g.indexed = append(g.indexed, names)
	return ci
}

// dropIndex of an index the case named, or of one Postgres may have named.
func (g *gen) dropIndex() *ast.DropIndex {
	names := append([]string{"t_pkey", "t_c0_key", "t_c0_idx", "t_c1_idx", "t", "nope"}, g.indexes...)
	return &ast.DropIndex{Name: pick(g, names), IfExists: g.rarely(3)}
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
	// refs are the tables of a join whose columns the expression references, each qualified by
	// its name. Without them, the expression references the columns of the generator's own
	// table, by their names alone.
	refs []ref
	// safe forbids expressions that can fail at runtime, which maragudb rejects where the rows
	// Postgres evaluates them on depend on its plan, as under a LIMIT.
	safe bool
	// null allows a bare NULL literal here.
	null bool
	// sub allows subqueries, nested sub levels deep at most. The columns of refs are those of
	// the queries around, which a subquery reads by their qualified names.
	sub int
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
	if s.sub > 0 && depth < 3 && g.rarely(4) {
		return g.subquery(t, s, depth)
	}
	if s.columns && g.hasColumn(t, s) && g.draw(0, 2) == 0 {
		return g.columnOfType(t, s)
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
			ot := g.anyTypeIn(s)
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
			return &ast.IsNull{Expr: g.expr(g.anyTypeIn(s), s.withNull(true), depth), Not: g.bool()}
		case 3:
			ot := g.anyTypeIn(s)
			return &ast.Between{Expr: g.expr(ot, s.withNull(true), depth), Low: g.expr(ot, s.withNull(true), depth), High: g.expr(ot, s.withNull(true), depth), Not: g.bool()}
		case 4:
			ot := g.anyTypeIn(s)
			in := &ast.In{Expr: g.expr(ot, s, depth), Not: g.bool()}
			// A subquery in the list of an IN is rejected.
			ls := s
			ls.sub = 0
			for range g.draw(1, 3) {
				in.List = append(in.List, g.expr(ot, ls.withNull(true), depth))
			}
			return in
		case 5:
			pattern := g.src.Text("ab%_", 5)
			if g.bool() && !s.safe {
				pattern = g.src.Text("aA%_\\", 5)
			}
			return &ast.Like{Expr: g.expr(value.String, s.withNull(true), depth), Pattern: &ast.Literal{Value: value.NewUnknown(pattern)}, Not: g.bool()}
		default:
			ot := g.anyTypeIn(s)
			op := pick(g, []ast.BinaryOp{ast.Eq, ast.Ne, ast.Lt, ast.Le, ast.Gt, ast.Ge})
			return &ast.Binary{Op: op, Left: g.expr(ot, s, depth), Right: g.expr(ot, s.withNull(true), depth)}
		}
	case value.String:
		switch g.draw(0, 4) {
		case 0:
			// || writes a value of any type next to text as text.
			rt := t
			if g.rarely(3) {
				rt = g.anyTypeIn(s)
			}
			return &ast.Binary{Op: ast.Concat, Left: g.expr(t, s, depth), Right: g.expr(rt, s.withNull(true), depth)}
		case 1:
			name := pick(g, []string{"lower", "upper"})
			return &ast.Call{Name: name, Args: []ast.Expr{g.expr(t, s.withNull(true), depth)}}
		case 2:
			return &ast.Cast{Expr: g.expr(g.anyTypeIn(s), s.withNull(true), depth), Type: value.String, Style: ast.CastStyle(g.draw(0, 1))}
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

// hasColumn reports whether a column of type t is in the scope.
func (g *gen) hasColumn(t value.Type, s scope) bool {
	if len(s.refs) == 0 {
		return len(g.cols[t]) > 0
	}
	return slices.ContainsFunc(s.refs, func(r ref) bool { return len(r.g.cols[t]) > 0 })
}

// columnOfType t in the scope, which [gen.hasColumn] reports there is.
func (g *gen) columnOfType(t value.Type, s scope) ast.Expr {
	if len(s.refs) == 0 {
		return &ast.ColumnRef{Name: pick(g, g.cols[t])}
	}
	var refs []ref
	for _, r := range s.refs {
		if len(r.g.cols[t]) > 0 {
			refs = append(refs, r)
		}
	}
	r := pick(g, refs)
	return &ast.ColumnRef{Table: r.name, Name: pick(g, r.g.cols[t])}
}

// anyType that has a column when columns are in scope, so comparisons tend to touch data.
func (g *gen) anyType() value.Type {
	return g.anyTypeIn(scope{})
}

// anyTypeIn is [gen.anyType] for the columns of the scope.
func (g *gen) anyTypeIn(s scope) value.Type {
	var withCols []value.Type
	for _, t := range allTypes {
		if g.hasColumn(t, scope{refs: s.refs}) {
			withCols = append(withCols, t)
		}
	}
	if len(withCols) > 0 && g.draw(0, 2) > 0 {
		return withCols[g.draw(0, len(withCols)-1)]
	}
	return pick(g, allTypes)
}

// row of literal values for every column, respecting NOT NULL most of the time, with DEFAULT for
// the column whose default is now() and now and then for another column.
func (g *gen) row() []ast.Expr {
	row := make([]ast.Expr, len(g.table.Columns))
	for i, c := range g.table.Columns {
		if c.Name == g.nowColumn && !g.rarely(8) || c.Default != nil && g.rarely(3) || g.rarely(40) {
			row[i] = &ast.Default{}
			continue
		}
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
	ins := &ast.Insert{Table: g.table.Name}
	seen := map[string]bool{}
	for range max(n, 1) {
		row := g.row()
		g.references(row)
		// Rows mostly get distinct keys, so that inserts mostly succeed.
		key := g.rowKey(row)
		for try := 0; seen[key] && try < 3; try++ {
			row = g.rowKey2(row)
			key = g.rowKey(row)
		}
		seen[key] = true
		g.rows = append(g.rows, row)
		ins.Rows = append(ins.Rows, g.parameterize(row))
	}
	return ins
}

// parameterize now and then a value of a row that is not of the key, by a parameter whose
// value [gen.paramValue] draws. The row keeps its literals for [gen.rows], which later rows copy
// from.
func (g *gen) parameterize(row []ast.Expr) []ast.Expr {
	out := append([]ast.Expr{}, row...)
	for i, c := range g.table.Columns {
		if g.isKey(c.Name) || !g.rarely(6) {
			continue
		}
		if _, ok := row[i].(*ast.Default); ok {
			continue
		}
		out[i] = g.param(g.paramValue(c.Type))
	}
	return out
}

// param of the statement being generated, with the value v.
func (g *gen) param(v any) ast.Expr {
	*g.args = append(*g.args, v)
	return &ast.Param{Index: len(*g.args)}
}

// paramValue for a parameter in the place of a value of type t: mostly a value of the Go type of
// t, sometimes its text as a string, which reads like a quoted literal, or nil, and now and then
// one that Postgres refuses when it reads it, a time outside the range of timestamptz or a string
// that is not valid UTF-8.
func (g *gen) paramValue(t value.Type) any {
	if g.rarely(10) {
		return nil
	}
	if t == value.Time {
		return g.paramTime()
	}
	v := g.value(t)
	if g.rarely(5) {
		return value.Format(v)
	}
	switch t {
	case value.Int:
		return v.I
	case value.Float:
		return v.F
	case value.Bool:
		return v.B
	case value.String:
		if g.rarely(6) {
			return v.S + pick(g, []string{"\xff", "\x00", "\xc3", "\xed\xa0\x80"})
		}
		return v.S
	}
	return v.Raw
}

// timestampBounds are the ends of timestamptz, the start of the year 1, from which text reads
// it, and times outside the range. Each is within 292,277 years of 2000, where pgx encodes a time
// faithfully: beyond, its microseconds since 2000 overflow 64 bits and wrap around to another
// time, which Postgres may accept.
var timestampBounds = []time.Time{
	time.Date(-4713, 11, 24, 0, 0, 0, 0, time.UTC),
	time.Date(294277, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(-100000, 1, 1, 0, 0, 0, 0, time.UTC),
	time.Date(-300, 6, 1, 0, 0, 0, 0, time.UTC),
	time.Date(294277, 1, 5, 0, 0, 0, 0, time.UTC),
}

// paramTime is a time with nanoseconds, which both sides floor to microseconds: mostly in the
// range of the literals, and otherwise at or near one of [timestampBounds], inside or outside.
func (g *gen) paramTime() time.Time {
	if !g.rarely(3) {
		return g.value(value.Time).T.Add(time.Duration(g.draw(0, 999)))
	}
	offset := pick(g, []time.Duration{0, 1, 999, 1000, 1001, time.Second, 24 * time.Hour})
	if g.bool() {
		offset = -offset
	}
	return pick(g, timestampBounds).Add(offset)
}

// references gives the columns of each foreign key of the table, most of the time, the values of
// a row that an earlier INSERT of the referenced table wrote, so that the reference mostly holds.
func (g *gen) references(row []ast.Expr) {
	for _, fk := range g.table.ForeignKeys {
		ref := g
		if fk.Table == parentName {
			ref = g.parent
		}
		if len(ref.rows) == 0 || g.rarely(4) {
			continue
		}
		refColumns := fk.RefColumns
		if len(refColumns) == 0 {
			refColumns = ref.table.PrimaryKey
		}
		from := pick(g, ref.rows)
		for i, c := range fk.Columns {
			row[g.indexOf(c)] = from[ref.indexOf(refColumns[i])]
		}
	}
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
	if g.draw(0, 2) == 0 {
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

// keyCondition on the columns of the primary key or another index, so that index access paths
// are exercised: a comparison of an indexed column with a literal, an IN of literals, or a range
// of two comparisons, and now and then two of them on two indexes.
func (g *gen) keyCondition() ast.Expr {
	if len(g.indexed) > 1 && g.rarely(4) {
		return &ast.Binary{Op: ast.And, Left: g.indexCondition(), Right: g.indexCondition()}
	}
	return g.indexCondition()
}

func (g *gen) indexCondition() ast.Expr {
	cols := pick(g, g.indexed)
	if g.bool() {
		cols = g.table.PrimaryKey
	}
	name := pick(g, cols)
	if g.indexOf(name) < 0 {
		name = pick(g, g.table.PrimaryKey)
	}
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

// indexOf the column, or -1.
func (g *gen) indexOf(name string) int {
	for i, c := range g.table.Columns {
		if c.Name == name {
			return i
		}
	}
	return -1
}

func (g *gen) statement() ast.Statement {
	if g.parent != nil && g.rarely(3) {
		// A write of the referenced table, whose foreign key checks and actions act on this
		// one.
		switch g.draw(0, 2) {
		case 0:
			return g.parent.insert(g.draw(1, 3))
		case 1:
			return g.parent.update()
		default:
			return &ast.Delete{Table: parentName, Where: g.parent.where(scope{columns: true})}
		}
	}
	if g.rarely(8) {
		if len(g.indexes) > 0 && g.rarely(3) || g.rarely(6) {
			return g.dropIndex()
		}
		return g.createIndex()
	}
	switch g.draw(0, 11) {
	case 10:
		return g.subquerySelect()
	case 11:
		return g.subqueryWrite()
	case 0:
		return g.insert(g.draw(1, 3))
	case 1:
		return g.update()
	case 2:
		d := &ast.Delete{Table: g.table.Name, Where: g.where(scope{columns: true})}
		d.Alias = g.aliased(&d.Where)
		return d
	case 3, 4:
		return g.joinSelect()
	case 5, 6:
		return g.groupSelect()
	default:
		return g.selectStmt()
	}
}

// update of one or two columns, mostly not of the key.
func (g *gen) update() *ast.Update {
	u := &ast.Update{Table: g.table.Name}
	var nonKey []string
	for _, name := range g.columnNames() {
		if !g.isKey(name) {
			nonKey = append(nonKey, name)
		}
	}
	targets := nonKey
	// The referenced table updates its key more often, which its foreign keys check.
	if len(nonKey) == 0 || g.rarely(5) || g.table.Name == parentName && g.bool() {
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
		var e ast.Expr = &ast.Default{}
		switch {
		case g.rarely(8):
			e = g.param(g.paramValue(g.column(name).Type))
		case !g.rarely(8):
			e = g.expr(g.column(name).Type, scope{columns: true, null: !g.isKey(name)}, 0)
		}
		u.Set = append(u.Set, ast.Assignment{Column: name, Value: e})
	}
	if g.nowColumn != "" && g.rarely(6) {
		u.Set = append(u.Set, ast.Assignment{Column: g.nowColumn, Value: &ast.Default{}})
	}
	u.Where = g.where(scope{columns: true})
	exprs := []*ast.Expr{&u.Where}
	for i := range u.Set {
		exprs = append(exprs, &u.Set[i].Value)
	}
	u.Alias = g.aliased(exprs...)
	return u
}

func (g *gen) selectStmt() *ast.Select {
	s := &ast.Select{From: []ast.TableExpr{&ast.TableRef{Name: g.table.Name}}}
	limited := g.draw(0, 2) == 0
	sc := scope{columns: true, safe: limited && !g.rarely(8)}
	n := g.draw(1, 3)
	for i := range n {
		if g.draw(0, 3) == 0 {
			if g.nowColumn == "" {
				s.Items = append(s.Items, ast.SelectItem{Star: true})
				continue
			}
			// The values of the column whose default is now() differ between the databases, so
			// they are only compared with now().
			s.Items = append(s.Items, ast.SelectItem{Expr: &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Le, ast.Eq}), Left: &ast.ColumnRef{Name: g.nowColumn}, Right: &ast.Call{Name: "now"}}})
			continue
		}
		if g.rarely(30) {
			s.Items = append(s.Items, ast.SelectItem{Expr: &ast.Binary{Op: ast.Eq, Left: &ast.Call{Name: "now"}, Right: pick(g, []ast.Expr{&ast.Call{Name: "current_timestamp", Keyword: true}, &ast.Call{Name: "transaction_timestamp"}, &ast.Call{Name: "now"}})}})
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
			o.Expr = &ast.ColumnRef{Name: pick(g, g.columnNames())}
		default:
			o.Expr = g.expr(g.anyType(), sc, 1)
		}
		s.OrderBy = append(s.OrderBy, o)
	}
	s.Distinct = !limited && g.rarely(5)
	exprs := []*ast.Expr{&s.Where}
	for i := range s.Items {
		if !s.Items[i].Star {
			exprs = append(exprs, &s.Items[i].Expr)
		}
	}
	for i := range s.OrderBy {
		exprs = append(exprs, &s.OrderBy[i].Expr)
	}
	s.From[0].(*ast.TableRef).Alias = g.aliased(exprs...)
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
// shadows, or by the position of an output column that is the key column itself. A grouped
// SELECT is fully ordered when it has one group at most, without GROUP BY, or when every GROUP
// BY item is a sort key, as the same expression or by the position of an output column that is
// it.
func FullyOrdered(s *ast.Select, pk []string) bool {
	if isGrouped(s) {
		return groupsOrdered(s)
	}
	if len(s.OrderBy) == 0 {
		return false
	}
	for _, k := range pk {
		found := false
		for _, o := range s.OrderBy {
			// A qualified name is never an output column.
			if ref, ok := o.Expr.(*ast.ColumnRef); ok && ref.Name == k && (ref.Table != "" || !shadowed(s, k)) {
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
			// Postgres resolves a name alone in ORDER BY to an output column first. A qualified
			// name sorts by the column, which an output column of the same text before any star
			// shows.
			if e.Table == "" {
				i = slices.Index(columns, e.Name)
				break
			}
			for j, item := range s.Items {
				if item.Star {
					break
				}
				if ref, ok := item.Expr.(*ast.ColumnRef); ok && *ref == *e {
					i = j
					break
				}
			}
		}
		if i < 0 {
			break
		}
		out = append(out, i)
	}
	return out
}

// ref is a table of a join, under the name that qualifies its columns.
type ref struct {
	name string
	g    *gen
}

// qualify every column reference of e that names a column alone with the name of a table.
func qualify(e ast.Expr, name string) ast.Expr {
	if e == nil {
		return nil
	}
	return ast.Map(e, func(x ast.Expr) ast.Expr {
		if c, ok := x.(*ast.ColumnRef); ok && c.Table == "" {
			return &ast.ColumnRef{Table: name, Name: c.Name}
		}
		return x
	})
}

// aliased gives a statement on the table an alias now and then, and then qualifies its column
// references by the alias or by the table's name, now and then the wrong one, or leaves them
// alone. It returns the alias, or "".
func (g *gen) aliased(exprs ...*ast.Expr) string {
	if !g.rarely(3) {
		return ""
	}
	alias := ""
	if g.bool() {
		alias = pick(g, []string{"x", "y", g.table.Name})
	}
	name := cmp.Or(alias, g.table.Name)
	if g.rarely(20) {
		// A reference by the name that the alias hides, which fails on both.
		name = pick(g, []string{g.table.Name, "q"})
	}
	if g.bool() {
		for _, e := range exprs {
			*e = qualify(*e, name)
		}
	}
	return alias
}

// joinSelect is a SELECT on a join of the table with itself, or with the referenced table: a
// list of tables, CROSS JOIN, an inner or left join with ON, USING, or NATURAL, and now and
// then a third table, joined from the right in parentheses or from the left, with conditions
// that compare columns of the tables, the foreign key columns among them, and now and then
// conditions that can fail, which maragudb rejects where their outcome depends on Postgres's
// join plan.
func (g *gen) joinSelect() *ast.Select {
	s := &ast.Select{Distinct: g.rarely(4)}
	refs, other, conds := g.joinFrom(s)
	// Under a LIMIT, an expression that can fail is rejected, so a limited join has none.
	limited := g.rarely(6)
	sc := scope{columns: true, refs: refs, safe: limited}
	hasNow := g.nowColumn != ""
	for i := range g.draw(1, 3) {
		switch {
		case g.draw(0, 5) == 0 && !hasNow:
			s.Items = append(s.Items, ast.SelectItem{Star: true})
		case g.draw(0, 4) == 0 && (!hasNow || other.g != g):
			r := refs[0]
			if hasNow {
				r = other
			}
			if !hasNow && g.bool() {
				r = pick(g, refs)
			}
			s.Items = append(s.Items, ast.SelectItem{Star: true, Table: r.name})
		default:
			item := ast.SelectItem{Expr: g.expr(g.anyTypeIn(sc), sc.withNull(true), 0)}
			if g.bool() {
				item.Alias = fmt.Sprint("a", i)
			}
			s.Items = append(s.Items, item)
		}
	}
	if g.bool() {
		r := pick(g, refs)
		conds = append(conds, qualify(r.g.keyCondition(), r.name))
	}
	if g.rarely(3) {
		// A condition on the tables of the join, which may fail.
		safe := limited || !g.rarely(3)
		conds = append(conds, g.expr(value.Bool, scope{columns: true, refs: refs, safe: safe}, 1))
	}
	if !limited && g.rarely(5) {
		conds = append(conds, g.failing(scope{columns: true, refs: refs}))
	}
	if len(conds) > 0 {
		s.Where = join(conds)
	}
	for range g.draw(0, 2) {
		o := ast.OrderItem{Desc: g.bool(), Nulls: ast.NullsOrder(g.draw(0, 2))}
		switch g.draw(0, 2) {
		case 0:
			o.Expr = &ast.Literal{Value: value.NewInt4(int32(g.draw(1, 3)))}
		case 1:
			r := pick(g, refs)
			o.Expr = &ast.ColumnRef{Table: r.name, Name: pick(g, r.g.columnNames())}
		default:
			o.Expr = g.expr(g.anyTypeIn(sc), sc, 1)
		}
		s.OrderBy = append(s.OrderBy, o)
	}
	// The columns that ORDER BY sorts by are in the output too, so that the harness compares the
	// order, unless a position in ORDER BY would then name another column.
	if !limited && !s.Distinct && !slices.ContainsFunc(s.OrderBy, func(o ast.OrderItem) bool { _, ok := o.Expr.(*ast.Literal); return ok }) {
		for _, o := range s.OrderBy {
			if ref, ok := o.Expr.(*ast.ColumnRef); ok {
				s.Items = append(s.Items, ast.SelectItem{Expr: ref})
			}
		}
	}
	if limited {
		// A total order, which a LIMIT needs, by every key column of every table.
		for _, r := range refs {
			for _, k := range r.g.table.PrimaryKey {
				s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Table: r.name, Name: k}, Desc: g.bool()})
			}
		}
		s.Limit = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 4)))}
	}
	return s
}

// joinFrom sets the FROM of s to a join of the table with itself or with the referenced table,
// and returns the tables, the other table of the first join, and conditions on them for WHERE.
func (g *gen) joinFrom(s *ast.Select) ([]ref, ref, []ast.Expr) {
	refs := []ref{{name: "x", g: g}}
	other := ref{name: "y", g: g}
	if g.parent != nil && g.bool() {
		other = ref{name: parentName, g: g.parent}
		if g.bool() {
			other.name = "z"
		}
	}
	refs = append(refs, other)
	table := func(r ref) ast.TableExpr {
		t := &ast.TableRef{Name: r.g.table.Name}
		if r.name != t.Name {
			t.Alias = r.name
		}
		return t
	}
	kind := pick(g, []ast.JoinKind{ast.JoinInner, ast.JoinInner, ast.JoinLeft, ast.JoinLeft, ast.JoinCross})
	var conds []ast.Expr
	var from ast.TableExpr
	switch g.draw(0, 5) {
	case 0:
		// A list of tables, joined by WHERE.
		s.From = []ast.TableExpr{table(refs[0]), table(refs[1])}
		conds = append(conds, g.joinCondition(refs[0], refs[1]))
	case 1:
		j := &ast.Join{Kind: pick(g, []ast.JoinKind{ast.JoinInner, ast.JoinLeft}), Left: table(refs[0]), Right: table(refs[1])}
		// NATURAL would compare the column whose default is now(), which differs between the
		// databases.
		if g.bool() && g.nowColumn == "" {
			j.Natural = true
		} else {
			j.Using = g.usingColumns(refs[0], refs[1])
		}
		from = j
	default:
		j := &ast.Join{Kind: kind, Left: table(refs[0]), Right: table(refs[1])}
		if kind != ast.JoinCross {
			j.On = g.joinCondition(refs[0], refs[1])
		}
		from = j
	}
	if from != nil && g.rarely(3) {
		// A third table, which a join of two references from the right, in parentheses, or which
		// joins the two from the left.
		third := ref{name: "w", g: g}
		if g.parent != nil && other.g != g.parent && g.bool() {
			third = ref{name: parentName, g: g.parent}
		}
		refs = append(refs, third)
		j := &ast.Join{Kind: pick(g, []ast.JoinKind{ast.JoinInner, ast.JoinLeft})}
		if g.bool() {
			inner := from.(*ast.Join)
			j.Left, j.Right = table(third), inner
			j.On = g.joinCondition(third, pick(g, refs[:2]))
		} else {
			j.Left, j.Right = from, table(third)
			j.On = g.joinCondition(pick(g, refs[:2]), third)
		}
		from = j
	}
	if from != nil {
		s.From = []ast.TableExpr{from}
	}
	return refs, other, conds
}

// join the conditions with AND.
func join(conds []ast.Expr) ast.Expr {
	out := conds[0]
	for _, c := range conds[1:] {
		out = &ast.Binary{Op: ast.And, Left: out, Right: c}
	}
	return out
}

// joinCondition of two tables of a join: mostly equalities of columns of the same type, the
// foreign key columns of the table with the referenced columns when they are the two tables,
// and now and then another condition on both, which may fail.
func (g *gen) joinCondition(l, r ref) ast.Expr {
	var conds []ast.Expr
	if l.g != r.g || g.rarely(3) {
		fkOf := func(c, p ref) bool {
			for _, fk := range c.g.table.ForeignKeys {
				if fk.Table != p.g.table.Name || !g.bool() {
					continue
				}
				refCols := fk.RefColumns
				if len(refCols) == 0 {
					refCols = p.g.table.PrimaryKey
				}
				for i, col := range fk.Columns {
					conds = append(conds, &ast.Binary{Op: ast.Eq, Left: &ast.ColumnRef{Table: c.name, Name: col}, Right: &ast.ColumnRef{Table: p.name, Name: refCols[i]}})
				}
				return true
			}
			return false
		}
		if !fkOf(l, r) {
			fkOf(r, l)
		}
	}
	if len(conds) == 0 || g.rarely(4) {
		var types []value.Type
		for _, t := range allTypes {
			if len(l.g.cols[t]) > 0 && len(r.g.cols[t]) > 0 {
				types = append(types, t)
			}
		}
		if len(types) > 0 {
			t := pick(g, types)
			op := ast.Eq
			if g.rarely(4) {
				op = pick(g, []ast.BinaryOp{ast.Lt, ast.Ne, ast.Ge})
			}
			conds = append(conds, &ast.Binary{Op: op, Left: &ast.ColumnRef{Table: l.name, Name: pick(g, l.g.cols[t])}, Right: &ast.ColumnRef{Table: r.name, Name: pick(g, r.g.cols[t])}})
		}
	}
	if len(conds) == 0 || g.rarely(4) {
		// A condition on both tables, or on one, which may fail.
		conds = append(conds, g.expr(value.Bool, scope{columns: true, refs: []ref{l, r}, safe: !g.rarely(3)}, 1))
	}
	if g.rarely(8) {
		conds = append(conds, g.failing(scope{columns: true, refs: []ref{l, r}}))
	}
	return join(conds)
}

// failing is a condition that fails on some rows and not on others, so that whether a join fails
// depends on the rows and pairs its plan evaluates it on: a division by an integer column, which
// is often 0, or the text of a column read as a bigint.
func (g *gen) failing(s scope) ast.Expr {
	switch {
	case g.hasColumn(value.Int, s) && !g.rarely(4):
		divisor := g.columnOfType(value.Int, s)
		if g.hasColumn(value.Int, s) && g.bool() {
			// A difference of two columns, which is 0 on the pairs where they are equal.
			divisor = &ast.Binary{Op: ast.Sub, Left: divisor, Right: g.columnOfType(value.Int, s)}
		}
		return &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Ge, ast.Lt, ast.Ne}), Left: &ast.Binary{Op: ast.Div, Left: &ast.Literal{Value: value.NewInt4(10)}, Right: divisor}, Right: g.literal(value.Int)}
	case g.hasColumn(value.String, s) && g.bool():
		return &ast.Binary{Op: ast.Gt, Left: &ast.Cast{Expr: g.columnOfType(value.String, s), Type: value.Int}, Right: g.literal(value.Int)}
	}
	return g.expr(value.Bool, s, 1)
}

// usingColumns are one or two columns that both tables of a join have, which a self-join has
// all of, and a join with the referenced table mostly c0, now and then of different types.
func (g *gen) usingColumns(l, r ref) []string {
	var common []string
	for _, c := range l.g.columnNames() {
		if slices.Contains(r.g.columnNames(), c) {
			common = append(common, c)
		}
	}
	if len(common) == 0 || g.rarely(20) {
		return []string{"nope"}
	}
	cols := []string{pick(g, common)}
	if len(common) > 1 && g.rarely(3) {
		if c := pick(g, common); c != cols[0] {
			cols = append(cols, c)
		}
	}
	return cols
}

// groupSelect is a grouped SELECT on the table or on a join: GROUP BY columns, expressions, or
// the primary key, aggregates in the output, HAVING, and ORDER BY, with DISTINCT inside them,
// and now and then arguments, GROUP BY expressions, and HAVING conditions that can fail, a
// column outside the GROUP BY, and a LIMIT with an ORDER BY on every GROUP BY expression.
func (g *gen) groupSelect() *ast.Select {
	s := &ast.Select{}
	var refs []ref
	var conds []ast.Expr
	if g.rarely(3) {
		refs, _, conds = g.joinFrom(s)
	} else {
		s.From = []ast.TableExpr{&ast.TableRef{Name: g.table.Name}}
		if w := g.where(scope{columns: true}); w != nil {
			conds = append(conds, w)
		}
	}
	limited := g.rarely(6)
	sc := scope{columns: true, refs: refs, safe: limited}
	var keys []ast.Expr
	switch g.draw(0, 4) {
	case 0:
	case 1:
		// The primary key of a table, which allows its other columns.
		r := ref{g: g}
		if len(refs) > 0 {
			r = pick(g, refs)
		}
		for _, k := range r.g.table.PrimaryKey {
			keys = append(keys, &ast.ColumnRef{Table: r.name, Name: k})
		}
	default:
		for range g.draw(1, 2) {
			t := g.anyTypeIn(sc)
			if g.hasColumn(t, sc) && !g.rarely(3) {
				keys = append(keys, g.columnOfType(t, sc))
			} else {
				keys = append(keys, g.expr(t, sc, 2))
			}
		}
	}
	for i := range g.draw(1, 3) {
		var item ast.SelectItem
		switch {
		case len(keys) > 0 && g.draw(0, 2) == 0:
			item.Expr = pick(g, keys)
		case g.rarely(20):
			// A column that is not grouped, which is an error unless the GROUP BY has the primary
			// key of its table.
			item.Expr = g.columnOfTypeOr(g.anyTypeIn(sc), sc)
		default:
			item.Expr, _ = g.aggregate(sc)
		}
		if g.bool() {
			item.Alias = fmt.Sprint("a", i)
		}
		s.Items = append(s.Items, item)
	}
	if len(keys) > 0 && g.rarely(3) {
		// A GROUP BY item by the position of an output column that is a key.
		for i, item := range s.Items {
			if j := slices.IndexFunc(keys, func(k ast.Expr) bool { return k == item.Expr }); j >= 0 && item.Alias == "" {
				keys[j] = &ast.Literal{Value: value.NewInt4(int32(i + 1))}
				break
			}
		}
	}
	s.GroupBy = keys
	if g.rarely(3) {
		s.Having = g.havingCondition(sc, keys)
	}
	if !limited && g.rarely(6) {
		conds = append(conds, g.failing(sc))
	}
	if len(conds) > 0 {
		s.Where = join(conds)
	}
	if limited {
		// The order of the groups is fully determined by an ORDER BY on every GROUP BY item.
		for _, k := range keys {
			s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: k, Desc: g.bool(), Nulls: ast.NullsOrder(g.draw(0, 2))})
		}
		if g.bool() {
			s.Limit = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 3)))}
		} else {
			s.Offset = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 2)))}
		}
	} else {
		for range g.draw(0, 2) {
			o := ast.OrderItem{Desc: g.bool(), Nulls: ast.NullsOrder(g.draw(0, 2))}
			switch {
			case g.bool():
				o.Expr = &ast.Literal{Value: value.NewInt4(int32(g.draw(1, len(s.Items))))}
			case len(keys) > 0 && g.bool():
				o.Expr = pick(g, keys)
			default:
				o.Expr, _ = g.aggregate(sc)
			}
			s.OrderBy = append(s.OrderBy, o)
		}
		s.Distinct = g.rarely(6)
	}
	if len(refs) == 0 {
		exprs := []*ast.Expr{&s.Where, &s.Having}
		for i := range s.Items {
			exprs = append(exprs, &s.Items[i].Expr)
		}
		for i := range s.GroupBy {
			exprs = append(exprs, &s.GroupBy[i])
		}
		for i := range s.OrderBy {
			exprs = append(exprs, &s.OrderBy[i].Expr)
		}
		s.From[0].(*ast.TableRef).Alias = g.aliased(exprs...)
	}
	return s
}

// aggregate is an expression with an aggregate of the columns of the scope: COUNT(*), or COUNT,
// SUM, AVG, MIN, or MAX of a column or an expression, with DISTINCT now and then, often in an
// expression of its own. It returns the expression and its type, where the numeric result of
// SUM and AVG of bigint is [value.Numeric].
func (g *gen) aggregate(s scope) (ast.Expr, value.Type) {
	name := pick(g, []string{"count", "count", "sum", "sum", "avg", "min", "max"})
	call := &ast.Call{Name: name, Distinct: g.rarely(4)}
	var t value.Type
	switch name {
	case "count":
		if g.bool() {
			return &ast.Call{Name: "count", Star: true}, value.Int
		}
		call.Args = []ast.Expr{g.expr(g.anyTypeIn(s), s, 2)}
		t = value.Int
	case "sum", "avg":
		at := pick(g, []value.Type{value.Int, value.Float})
		if !g.hasColumn(at, s) {
			at = value.Int
		}
		var arg ast.Expr
		switch {
		case !s.safe && at == value.Int && g.rarely(6):
			// An argument that fails on some rows.
			arg = &ast.Binary{Op: ast.Div, Left: &ast.Literal{Value: value.NewInt4(10)}, Right: g.columnOfTypeOr(value.Int, s)}
		case g.hasColumn(at, s) && !g.rarely(4):
			arg = g.columnOfType(at, s)
		default:
			arg = g.expr(at, s, 2)
		}
		call.Args = []ast.Expr{arg}
		t = at
		if at == value.Int {
			t = value.Numeric
		}
	default:
		var types []value.Type
		for _, ty := range allTypes {
			if ty != value.Bool && g.hasColumn(ty, s) {
				types = append(types, ty)
			}
		}
		t = value.Int
		if len(types) > 0 {
			t = pick(g, types)
		}
		if g.hasColumn(t, s) && !g.rarely(4) {
			call.Args = []ast.Expr{g.columnOfType(t, s)}
		} else {
			call.Args = []ast.Expr{g.expr(t, s, 2)}
		}
	}
	var e ast.Expr = call
	switch g.draw(0, 7) {
	case 0:
		if t == value.Numeric || t == value.Int {
			return &ast.Call{Name: "coalesce", Args: []ast.Expr{e, &ast.Literal{Value: value.NewInt4(int32(g.draw(-2, 2)))}}}, t
		}
	case 1:
		if t == value.Numeric || t == value.Float || t == value.Int {
			return &ast.Cast{Expr: e, Type: pick(g, []value.Type{value.Float, value.Int, value.String}), Style: ast.CastColons}, value.Float
		}
	case 2:
		lt := t
		if t == value.Numeric {
			lt = value.Int
		}
		return &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Lt, ast.Ge, ast.Eq, ast.Ne}), Left: e, Right: g.literal(lt)}, value.Bool
	case 3:
		if t == value.Int && !s.safe {
			return &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Add, ast.Mul, ast.Div}), Left: e, Right: g.literal(value.Int)}, value.Int
		}
	case 4, 5:
		if t == value.Numeric {
			return g.numericArithmetic(e, s)
		}
	}
	return e, t
}

// numericArithmetic is arithmetic on e, a numeric result of SUM or AVG: a sign, or +, -, *, /,
// or % with an integer or bigint literal, which is often 0, with another aggregate, or now and
// then with a double precision, or a numeric literal, which maragudb rejects. It returns the
// expression and its type. In a safe scope it is only a sign, which cannot fail.
func (g *gen) numericArithmetic(e ast.Expr, s scope) (ast.Expr, value.Type) {
	if s.safe || g.rarely(4) {
		return &ast.Unary{Op: pick(g, []ast.UnaryOp{ast.Neg, ast.Neg, ast.Pos}), Expr: e}, value.Numeric
	}
	t := value.Numeric
	var other ast.Expr
	switch g.draw(0, 9) {
	case 0, 1, 2:
		other = &ast.Literal{Value: value.NewInt4(int32(g.draw(-3, 100)))}
	case 3, 4:
		other = g.literal(value.Int)
	case 5, 6:
		other = &ast.Call{Name: pick(g, []string{"sum", "avg"}), Args: []ast.Expr{g.columnOfTypeOr(value.Int, s)}}
	case 7:
		other = &ast.Call{Name: "count", Star: true}
	case 8:
		other, t = g.literal(value.Float), value.Float
		if lit, ok := other.(*ast.Literal); ok && lit.Value.Type == value.Numeric {
			// A double precision, which g.literal writes as a numeric literal now and then.
			other = &ast.Cast{Expr: lit, Type: value.Float, Style: ast.CastColons}
		}
	default:
		other = &ast.Literal{Value: value.NewNumeric(pick(g, []string{"1.5", "0.25", "1e3"}))}
	}
	op := pick(g, []ast.BinaryOp{ast.Add, ast.Sub, ast.Mul, ast.Div, ast.Div, ast.Mod})
	if t == value.Float && op == ast.Mod {
		op = ast.Mul
	}
	if g.bool() {
		return &ast.Binary{Op: op, Left: e, Right: other}, t
	}
	return &ast.Binary{Op: op, Left: other, Right: e}, t
}

// columnOfTypeOr is a column of type t, or a literal of it when the scope has none.
func (g *gen) columnOfTypeOr(t value.Type, s scope) ast.Expr {
	if g.hasColumn(t, s) {
		return g.columnOfType(t, s)
	}
	return g.literal(t)
}

// havingCondition is a condition of HAVING: a comparison of an aggregate, a condition on a
// GROUP BY expression, which Postgres moves into WHERE, or now and then one that fails on some
// groups.
func (g *gen) havingCondition(s scope, keys []ast.Expr) ast.Expr {
	switch {
	case !s.safe && g.rarely(5):
		divisor := ast.Expr(&ast.Binary{Op: ast.Sub, Left: &ast.Call{Name: "count", Star: true}, Right: &ast.Literal{Value: value.NewInt4(int32(g.draw(1, 2)))}})
		if ref, ok := pickColumnKey(g, keys); ok && g.bool() {
			divisor = ref
		}
		return &ast.Binary{Op: ast.Gt, Left: &ast.Binary{Op: ast.Div, Left: &ast.Literal{Value: value.NewInt4(10)}, Right: divisor}, Right: &ast.Literal{Value: value.NewInt4(0)}}
	case len(keys) > 0 && g.rarely(3):
		if k, ok := keys[0].(*ast.ColumnRef); ok {
			return &ast.IsNull{Expr: k, Not: g.bool()}
		}
	}
	e, t := g.aggregate(s)
	if t == value.Bool {
		return e
	}
	if t == value.Numeric {
		t = value.Int
	}
	return &ast.Binary{Op: pick(g, []ast.BinaryOp{ast.Gt, ast.Le, ast.Ne}), Left: e, Right: g.literal(t)}
}

// pickColumnKey is a GROUP BY item that is a column, if there is one.
func pickColumnKey(g *gen, keys []ast.Expr) (ast.Expr, bool) {
	for _, k := range keys {
		if _, ok := k.(*ast.ColumnRef); ok {
			return k, true
		}
	}
	return nil, false
}

// isGrouped reports whether a SELECT is grouped: by GROUP BY, HAVING, or an aggregate in its
// output or ORDER BY.
func isGrouped(s *ast.Select) bool {
	if len(s.GroupBy) > 0 || s.Having != nil {
		return true
	}
	for _, item := range s.Items {
		if !item.Star && ast.HasAggregate(item.Expr) {
			return true
		}
	}
	return slices.ContainsFunc(s.OrderBy, func(o ast.OrderItem) bool { return ast.HasAggregate(o.Expr) })
}

// groupsOrdered reports whether the groups of a grouped SELECT are in one order in every plan.
func groupsOrdered(s *ast.Select) bool {
	if len(s.GroupBy) == 0 {
		return true
	}
	// output is the text of the expression of output column i, or "".
	output := func(i int) string {
		if i < 0 || i >= len(s.Items) || starBefore(s, i) {
			return ""
		}
		return ast.RenderExpr(s.Items[i].Expr)
	}
	alias := func(name string) int {
		return slices.IndexFunc(s.Items, func(item ast.SelectItem) bool { return !item.Star && item.Alias == name })
	}
	var sorted []string
	for _, o := range s.OrderBy {
		text := ast.RenderExpr(o.Expr)
		if p, ok := orderPosition(o.Expr); ok {
			text = output(p)
		} else if ref, ok := o.Expr.(*ast.ColumnRef); ok && ref.Table == "" && alias(ref.Name) >= 0 {
			text = output(alias(ref.Name))
		}
		sorted = append(sorted, text)
	}
	for _, k := range s.GroupBy {
		text := ast.RenderExpr(k)
		if p, ok := orderPosition(k); ok {
			text = output(p)
		} else if ref, ok := k.(*ast.ColumnRef); ok && ref.Table == "" && alias(ref.Name) >= 0 {
			// A name in GROUP BY is a column of FROM before it is an output column's.
			return false
		}
		if text == "" || !slices.Contains(sorted, text) {
			return false
		}
	}
	return true
}

// subquery of type t in the scope s: a scalar subquery, or for a boolean also EXISTS, NOT
// EXISTS, IN, or NOT IN, on the table or the referenced table under a name of its own, whose
// conditions now and then compare its columns with those of the queries around, which makes it
// correlated, and whose expressions may have subqueries of their own. A scalar subquery is an
// aggregate, which has one row, or a column, which can have several, and fails then.
func (g *gen) subquery(t value.Type, s scope, depth int) ast.Expr {
	src := g
	if g.parent != nil && g.bool() {
		src = g.parent
	}
	in := ref{name: fmt.Sprintf("s%d%d", s.sub, depth), g: src}
	sel := &ast.Select{From: []ast.TableExpr{&ast.TableRef{Name: src.table.Name, Alias: in.name}}}
	// The expressions of a subquery mostly cannot fail, because maragudb rejects one that fails
	// where only some plans of Postgres evaluate it.
	inner := scope{columns: true, refs: append([]ref{in}, s.refs...), safe: s.safe || !g.rarely(4), sub: s.sub - 1}
	own := scope{columns: true, refs: []ref{in}, safe: inner.safe}
	var conds []ast.Expr
	if len(s.refs) > 0 && !g.rarely(4) {
		// A correlation: a column of the subquery compared with a column of a query around.
		for _, ct := range allTypes {
			if len(src.cols[ct]) > 0 && g.hasColumn(ct, scope{refs: s.refs}) && g.bool() {
				op := pick(g, []ast.BinaryOp{ast.Eq, ast.Eq, ast.Lt, ast.Ne})
				conds = append(conds, &ast.Binary{Op: op, Left: g.columnOfType(ct, scope{refs: []ref{in}}), Right: g.columnOfType(ct, scope{refs: s.refs})})
				break
			}
		}
	}
	if g.bool() {
		conds = append(conds, g.expr(value.Bool, inner, depth+1))
	}
	if !s.safe && g.rarely(10) {
		conds = append(conds, g.failing(scope{columns: true, refs: []ref{in}}))
	}
	if len(conds) > 0 {
		sel.Where = join(conds)
	}
	column := func(ct value.Type) ast.Expr {
		if len(src.cols[ct]) > 0 && !g.rarely(5) {
			return g.columnOfType(ct, scope{refs: []ref{in}})
		}
		return g.expr(ct, inner, depth+1)
	}
	// An argument of an aggregate reads a column of the subquery, which makes it an aggregate of
	// the subquery.
	argument := func(ct value.Type) ast.Expr {
		if len(src.cols[ct]) > 0 {
			return g.columnOfType(ct, scope{refs: []ref{in}})
		}
		return g.expr(ct, own, depth+1)
	}
	if t == value.Bool && g.draw(0, 2) > 0 {
		if g.bool() {
			// EXISTS, whose output Postgres drops.
			switch g.draw(0, 2) {
			case 0:
				sel.Items = []ast.SelectItem{{Star: true}}
			case 1:
				sel.Items = []ast.SelectItem{{Expr: &ast.Literal{Value: value.NewInt4(1)}}}
			default:
				sel.Items = []ast.SelectItem{{Expr: column(g.anyTypeIn(scope{refs: []ref{in}}))}}
			}
			var e ast.Expr = &ast.Subquery{Kind: ast.SubExists, Select: sel}
			if g.bool() {
				e = &ast.Unary{Op: ast.Not, Expr: e}
			}
			return e
		}
		ot := g.anyTypeIn(scope{refs: []ref{in}})
		sel.Items = []ast.SelectItem{{Expr: column(ot)}}
		if g.rarely(4) {
			sel.Distinct = true
		}
		left := g.expr(ot, scope{columns: true, refs: s.refs, safe: s.safe, sub: s.sub - 1, null: true}, depth+1)
		return &ast.Subquery{Kind: ast.SubIn, Select: sel, Expr: left, Not: g.bool()}
	}
	switch {
	case t == value.Int && g.bool():
		if g.bool() {
			sel.Items = []ast.SelectItem{{Expr: &ast.Call{Name: "count", Star: true}}}
		} else {
			sel.Items = []ast.SelectItem{{Expr: &ast.Call{Name: "count", Args: []ast.Expr{argument(g.anyTypeIn(scope{refs: []ref{in}}))}}}}
		}
	case t != value.Bool && !g.rarely(3):
		sel.Items = []ast.SelectItem{{Expr: &ast.Call{Name: pick(g, []string{"min", "max"}), Args: []ast.Expr{argument(t)}}}}
	case s.safe || !g.rarely(3):
		// One row at most, which cannot fail.
		sel.Items = []ast.SelectItem{{Expr: column(t)}}
		sel.Limit = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 1)))}
		for _, k := range src.table.PrimaryKey {
			sel.OrderBy = append(sel.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Table: in.name, Name: k}, Desc: g.bool()})
		}
	default:
		sel.Items = []ast.SelectItem{{Expr: column(t)}}
	}
	return &ast.Subquery{Kind: ast.SubScalar, Select: sel}
}

// subquerySelect is a SELECT whose expressions have subqueries, on the table under the name o,
// or on a subquery in FROM or a CTE of that name that selects rows of the table, now and then
// grouped, with every key column in ORDER BY, so that the order is mostly known.
func (g *gen) subquerySelect() *ast.Select {
	o := ref{name: "o", g: g}
	s := &ast.Select{}
	limited := g.rarely(6)
	sc := scope{columns: true, refs: []ref{o}, safe: limited, sub: 2}
	body := func() *ast.Select {
		b := &ast.Select{Items: []ast.SelectItem{{Star: true}}, From: []ast.TableExpr{&ast.TableRef{Name: g.table.Name}}}
		if g.bool() {
			b.Where = g.expr(value.Bool, scope{columns: true, safe: !g.rarely(8), sub: 1}, 1)
		}
		return b
	}
	switch g.draw(0, 5) {
	case 0:
		if limited {
			break
		}
		s.From = []ast.TableExpr{&ast.Derived{Select: body(), Alias: o.name}}
	case 1:
		if limited {
			break
		}
		s.With = []ast.CTE{{Name: o.name, Select: body()}}
		s.From = []ast.TableExpr{&ast.TableRef{Name: o.name}}
		if g.rarely(4) {
			// The CTE read twice.
			s.From = append(s.From, &ast.TableRef{Name: o.name, Alias: "o2"})
		}
	}
	if s.From == nil {
		s.From = []ast.TableExpr{&ast.TableRef{Name: g.table.Name, Alias: o.name}}
	}
	if g.rarely(4) && !limited {
		// Grouped by a column, with subqueries in the output and in HAVING that read the key.
		t := g.anyTypeIn(sc)
		key := g.columnOfTypeOr(t, sc)
		s.GroupBy = []ast.Expr{key}
		ks := scope{columns: false, sub: 1}
		s.Items = []ast.SelectItem{{Expr: key}, {Expr: &ast.Call{Name: "count", Star: true}}}
		if kref, ok := key.(*ast.ColumnRef); ok {
			// The key is the only column of the query around that a subquery may read.
			kg := &gen{src: g.src, cols: map[value.Type][]string{t: {kref.Name}}, table: g.table, parent: g.parent}
			ks = scope{columns: true, refs: []ref{{name: o.name, g: kg}}, sub: 1}
		}
		s.Items = append(s.Items, ast.SelectItem{Expr: g.subquery(g.anyType(), ks, 1)})
		if g.bool() {
			s.Having = g.subquery(value.Bool, ks, 1)
		}
		s.OrderBy = []ast.OrderItem{{Expr: key}}
		return s
	}
	for i := range g.draw(1, 3) {
		item := ast.SelectItem{Expr: g.expr(g.anyType(), sc.withNull(true), 0)}
		if g.bool() {
			item.Alias = fmt.Sprint("a", i)
		}
		s.Items = append(s.Items, item)
	}
	if !g.rarely(4) {
		where := g.subquery(value.Bool, sc, 0)
		if g.bool() {
			where = &ast.Binary{Op: ast.And, Left: qualify(g.keyCondition(), o.name), Right: where}
		}
		s.Where = where
	}
	if g.rarely(4) {
		s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: g.subquery(g.anyType(), sc, 1)})
	}
	for _, k := range g.table.PrimaryKey {
		s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: &ast.ColumnRef{Table: o.name, Name: k}, Desc: g.bool()})
	}
	if limited {
		s.Limit = &ast.Literal{Value: value.NewInt4(int32(g.draw(0, 3)))}
	}
	return s
}

// subqueryWrite is an UPDATE or DELETE of the table under the name o, whose WHERE and SET have
// subqueries, which may read the table itself, or an INSERT with a subquery in VALUES.
func (g *gen) subqueryWrite() ast.Statement {
	o := ref{name: "o", g: g}
	sc := scope{columns: true, refs: []ref{o}, sub: 2}
	switch g.draw(0, 2) {
	case 0:
		ins := g.insert(g.draw(1, 2))
		for _, row := range ins.Rows {
			i := g.draw(0, len(row)-1)
			switch row[i].(type) {
			case *ast.Default, *ast.Param:
				// A parameter stays, because its value is the statement's.
			default:
				if !g.isKey(g.table.Columns[i].Name) {
					row[i] = g.subquery(g.table.Columns[i].Type, scope{sub: 1}, 1)
				}
			}
		}
		return ins
	case 1:
		return &ast.Delete{Table: g.table.Name, Alias: o.name, Where: g.subquery(value.Bool, sc, 0)}
	}
	u := &ast.Update{Table: g.table.Name, Alias: o.name}
	var nonKey []string
	for _, name := range g.columnNames() {
		if !g.isKey(name) && name != g.nowColumn {
			nonKey = append(nonKey, name)
		}
	}
	if len(nonKey) == 0 {
		return &ast.Delete{Table: g.table.Name, Alias: o.name, Where: g.subquery(value.Bool, sc, 0)}
	}
	name := pick(g, nonKey)
	u.Set = []ast.Assignment{{Column: name, Value: g.subquery(g.column(name).Type, sc, 0)}}
	if g.bool() {
		u.Where = g.subquery(value.Bool, sc, 0)
	}
	return u
}
