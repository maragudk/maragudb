// Package ast is the syntax tree of the SQL subset, with a renderer to Postgres SQL, which is
// also the SQL of maragudb.
package ast

import "maragu.dev/maragudb/internal/sql/value"

// Keywords that cannot be unquoted column names: Postgres's reserved words and its
// type_func_name words. Every other keyword is recognized in context and can name a column.
var Keywords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true, "array": true, "as": true, "asc": true,
	"asymmetric": true, "authorization": true, "binary": true, "both": true, "case": true, "cast": true, "check": true,
	"collate": true, "collation": true, "column": true, "concurrently": true, "constraint": true, "create": true,
	"cross": true, "current_catalog": true, "current_date": true, "current_role": true, "current_schema": true,
	"current_time": true, "current_timestamp": true, "current_user": true, "default": true, "deferrable": true,
	"desc": true, "distinct": true, "do": true, "else": true, "end": true, "except": true, "false": true, "fetch": true,
	"for": true, "foreign": true, "freeze": true, "from": true, "full": true, "grant": true, "group": true, "having": true,
	"ilike": true, "in": true, "initially": true, "inner": true, "intersect": true, "into": true, "is": true,
	"isnull": true, "join": true, "lateral": true, "leading": true, "left": true, "like": true, "limit": true,
	"localtime": true, "localtimestamp": true, "natural": true, "not": true, "notnull": true, "null": true,
	"offset": true, "on": true, "only": true, "or": true, "order": true, "outer": true, "overlaps": true, "placing": true,
	"primary": true, "references": true, "returning": true, "right": true, "select": true, "session_user": true,
	"similar": true, "some": true, "symmetric": true, "system_user": true, "table": true, "tablesample": true,
	"then": true, "to": true, "trailing": true, "true": true, "union": true, "unique": true, "user": true, "using": true,
	"variadic": true, "verbose": true, "when": true, "where": true, "window": true, "with": true,
}

// Statement is any SQL statement.
type Statement interface {
	stmt()
}

// CreateTable statement.
type CreateTable struct {
	Name        string
	IfNotExists bool
	Columns     []ColumnDef
	// PrimaryKey columns in key order. Required.
	PrimaryKey []string
	// PrimaryKeyName is the name of the primary key constraint, and of its index, or "" for the
	// name Postgres chooses.
	PrimaryKeyName string
	// Unique constraints, column and table constraints alike, in the order they are written.
	Unique []UniqueConstraint
	// Checks are the CHECK constraints, column and table constraints alike, in the order they
	// are written.
	Checks []CheckConstraint
	// ForeignKeys are the FOREIGN KEY and REFERENCES constraints, column and table constraints
	// alike, in the order they are written.
	ForeignKeys []ForeignKey
}

// CheckConstraint of a [CreateTable].
type CheckConstraint struct {
	// Name of the constraint, or "" for the name Postgres chooses.
	Name string
	Expr Expr
}

// ForeignKey constraint of a [CreateTable]: its columns reference the columns RefColumns of the
// table Table, or its primary key when RefColumns is empty. Every row whose columns have no NULL
// must have a row in Table with the same values, which MATCH SIMPLE means.
type ForeignKey struct {
	// Name of the constraint, or "" for the name Postgres chooses.
	Name       string
	Columns    []string
	Table      string
	RefColumns []string
	// OnDelete and OnUpdate are the referential actions.
	OnDelete, OnUpdate Action
	// SetColumns are the columns that ON DELETE SET NULL or SET DEFAULT sets, or empty for every
	// column of the key.
	SetColumns []string
}

// Action of a [ForeignKey] when a referenced row is deleted or its key updated.
type Action int

// Actions.
const (
	NoAction Action = iota
	Restrict
	Cascade
	SetNull
	SetDefault
)

func (a Action) String() string {
	switch a {
	case Restrict:
		return "RESTRICT"
	case Cascade:
		return "CASCADE"
	case SetNull:
		return "SET NULL"
	case SetDefault:
		return "SET DEFAULT"
	}
	return "NO ACTION"
}

// UniqueConstraint of a [CreateTable].
type UniqueConstraint struct {
	// Name of the constraint and its index, or "" for the name Postgres chooses.
	Name    string
	Columns []string
	// NullsNotDistinct makes NULLs equal to each other, so that two rows with NULL in the same
	// columns are a duplicate.
	NullsNotDistinct bool
}

// CreateIndex statement.
type CreateIndex struct {
	// Name of the index, or "" for the name Postgres chooses.
	Name        string
	IfNotExists bool
	Unique      bool
	Table       string
	Columns     []IndexColumn
	// NullsNotDistinct makes NULLs equal to each other in a unique index.
	NullsNotDistinct bool
}

// IndexColumn of a [CreateIndex].
type IndexColumn struct {
	Name  string
	Desc  bool
	Nulls NullsOrder
}

// DropIndex statement.
type DropIndex struct {
	Name     string
	IfExists bool
}

// ColumnDef in a [CreateTable].
type ColumnDef struct {
	Name    string
	Type    value.Type
	NotNull bool
	// Default is the expression of the column's DEFAULT, or nil.
	Default Expr
}

// DropTable statement.
type DropTable struct {
	Name     string
	IfExists bool
}

// Insert statement with one or more VALUES rows, in which an item can be [Default]. An empty
// Columns list means every column in table order. With DefaultValues, the statement is INSERT
// ... DEFAULT VALUES, which has no columns and no rows.
type Insert struct {
	Table         string
	Columns       []string
	Rows          [][]Expr
	DefaultValues bool
}

// Update statement. Alias, when set, is the name that references the table in the statement.
type Update struct {
	Table string
	Alias string
	Set   []Assignment
	Where Expr
}

// Assignment in an [Update]. Its Value can be [Default]. A Qualifier is the first part of a
// target written as two names, such as x.c, which Postgres reads as the field c of the column x.
type Assignment struct {
	Qualifier string
	Column    string
	Value     Expr
}

// Delete statement. Alias, when set, is the name that references the table in the statement.
type Delete struct {
	Table string
	Alias string
	Where Expr
}

// Select statement. An empty From selects a single row without a table. Several items in From
// are joined as with CROSS JOIN, and the WHERE clause filters their rows. GroupBy and Having
// group the rows, as do aggregates in Items, Having, or OrderBy. With are the queries of its
// WITH clause, which its FROM, and the FROM of its subqueries, can read by name.
type Select struct {
	With     []CTE
	Distinct bool
	Items    []SelectItem
	From     []TableExpr
	Where    Expr
	GroupBy  []Expr
	Having   Expr
	OrderBy  []OrderItem
	Limit    Expr
	Offset   Expr
}

// SelectItem is a `*`, a `table.*` when Table is set, or an expression with an optional alias.
type SelectItem struct {
	Star  bool
	Table string
	Expr  Expr
	Alias string
}

// CTE is a query of a WITH clause, with names for its first columns.
type CTE struct {
	Name    string
	Columns []string
	Select  *Select
}

// TableExpr is an item of FROM: a [TableRef], a [Derived], or a [Join].
type TableExpr interface {
	tableExpr()
}

// TableRef is a table in FROM, with an optional alias, which is then the only name that
// references it, and optional names for its first columns.
type TableRef struct {
	Name    string
	Alias   string
	Columns []string
}

// Derived is a subquery in FROM, under its alias, with optional names for its first columns.
type Derived struct {
	Select  *Select
	Alias   string
	Columns []string
}

// JoinKind of a [Join].
type JoinKind int

// Join kinds.
const (
	JoinInner JoinKind = iota
	JoinLeft
	JoinCross
)

// Join of two FROM items. An inner or left join has a condition: On, the columns of Using, or
// Natural, which is Using with the columns that both sides have.
type Join struct {
	Kind        JoinKind
	Natural     bool
	Left, Right TableExpr
	On          Expr
	Using       []string
}

func (*TableRef) tableExpr() {}
func (*Derived) tableExpr()  {}
func (*Join) tableExpr()     {}

// FromTable is the table of a SELECT that reads exactly one table, or "".
func FromTable(s *Select) string {
	if len(s.From) != 1 {
		return ""
	}
	if t, ok := s.From[0].(*TableRef); ok {
		return t.Name
	}
	return ""
}

// Tables of a FROM item, in the order they are written. A [Derived] has none.
func Tables(e TableExpr) []*TableRef {
	switch e := e.(type) {
	case *TableRef:
		return []*TableRef{e}
	case *Join:
		return append(Tables(e.Left), Tables(e.Right)...)
	}
	return nil
}

// OrderItem in ORDER BY.
type OrderItem struct {
	Expr  Expr
	Desc  bool
	Nulls NullsOrder
}

// NullsOrder in ORDER BY.
type NullsOrder int

// Null orders. The default follows Postgres: last when ascending, first when descending.
const (
	NullsDefault NullsOrder = iota
	NullsFirst
	NullsLast
)

// Begin statement.
type Begin struct {
	ReadOnly bool
}

// Commit statement.
type Commit struct{}

// Rollback statement.
type Rollback struct{}

func (*CreateTable) stmt() {}
func (*DropTable) stmt()   {}
func (*CreateIndex) stmt() {}
func (*DropIndex) stmt()   {}
func (*Insert) stmt()      {}
func (*Update) stmt()      {}
func (*Delete) stmt()      {}
func (*Select) stmt()      {}
func (*Begin) stmt()       {}
func (*Commit) stmt()      {}
func (*Rollback) stmt()    {}

// Expr is any expression.
type Expr interface {
	expr()
}

// Literal value as the parser produces it: an integer of type [value.Int4] or [value.Int], a
// numeric literal of type [value.Numeric], a quoted string of type [value.Unknown], a boolean,
// or a NULL with type [value.Unknown]. A literal of another type renders as a cast of its text.
type Literal struct {
	Value value.Value
}

// Param is a positional parameter `$n`, numbered from 1.
type Param struct {
	Index int
}

// ColumnRef by name, qualified by the name of a table in FROM when Table is set.
type ColumnRef struct {
	Table string
	Name  string
}

// UnaryOp of a [Unary].
type UnaryOp int

// Unary operators.
const (
	Neg UnaryOp = iota
	Not
	Pos
)

// Unary expression.
type Unary struct {
	Op   UnaryOp
	Expr Expr
}

// BinaryOp of a [Binary].
type BinaryOp int

// Binary operators.
const (
	Add BinaryOp = iota
	Sub
	Mul
	Div
	Mod
	Concat
	Eq
	Ne
	Lt
	Le
	Gt
	Ge
	And
	Or
)

// Binary expression.
type Binary struct {
	Op          BinaryOp
	Left, Right Expr
}

// IsNull is `expr IS [NOT] NULL`.
type IsNull struct {
	Expr Expr
	Not  bool
}

// Between is `expr [NOT] BETWEEN low AND high`.
type Between struct {
	Expr, Low, High Expr
	Not             bool
}

// In is `expr [NOT] IN (list)`.
type In struct {
	Expr Expr
	List []Expr
	Not  bool
}

// Like is `expr [NOT] LIKE pattern`.
type Like struct {
	Expr, Pattern Expr
	Not           bool
}

// Case expression, in the simple form when Operand is set and the searched form otherwise.
type Case struct {
	Operand Expr
	Whens   []When
	Else    Expr
}

// When clause of a [Case].
type When struct {
	Cond, Result Expr
}

// Call of a function by lowercase name. With Keyword, it is a function that SQL writes as a
// keyword without parentheses, such as CURRENT_TIMESTAMP, and has no arguments. Star is the *
// of COUNT(*), which has no arguments, and Distinct the DISTINCT of an aggregate's arguments.
type Call struct {
	Name     string
	Args     []Expr
	Keyword  bool
	Star     bool
	Distinct bool
}

// Aggregates are the names of the aggregate functions, whose calls group the rows of a SELECT.
var Aggregates = map[string]bool{"count": true, "sum": true, "avg": true, "min": true, "max": true}

// IsAggregate reports whether e is a call of an aggregate function.
func IsAggregate(e Expr) bool {
	c, ok := e.(*Call)
	return ok && !c.Keyword && Aggregates[c.Name]
}

// HasAggregate reports whether e has a call of an aggregate function.
func HasAggregate(e Expr) bool {
	found := false
	Walk(e, func(x Expr) bool {
		found = found || IsAggregate(x)
		return !found
	})
	return found
}

// SubqueryKind of a [Subquery].
type SubqueryKind int

// Subquery kinds.
const (
	// SubScalar is a subquery as an expression, whose value is that of its one row and one
	// column, or NULL without a row.
	SubScalar SubqueryKind = iota
	// SubExists is EXISTS, which is true when the subquery has a row.
	SubExists
	// SubIn is `expr [NOT] IN (subquery)`.
	SubIn
)

// Subquery in an expression. Its Select reads the tables of its own FROM, and its names can
// also reference the columns of the statements around it. Walk and Map do not descend into it.
type Subquery struct {
	Kind   SubqueryKind
	Select *Select
	// Expr is the left side of an IN, and Not makes it NOT IN.
	Expr Expr
	Not  bool
}

// Default is the DEFAULT keyword in the VALUES of an INSERT or the SET of an UPDATE, which
// stands for the default value of the column.
type Default struct{}

// Cast is `CAST(expr AS type)`, `expr::type`, or, for a quoted string, `type 'string'`.
type Cast struct {
	Expr  Expr
	Type  value.Type
	Style CastStyle
}

// CastStyle is the syntax a [Cast] was written in.
type CastStyle int

// Cast styles.
const (
	CastExplicit CastStyle = iota
	CastColons
	CastPrefix
)

func (*Literal) expr()   {}
func (*Param) expr()     {}
func (*ColumnRef) expr() {}
func (*Unary) expr()     {}
func (*Binary) expr()    {}
func (*IsNull) expr()    {}
func (*Between) expr()   {}
func (*In) expr()        {}
func (*Like) expr()      {}
func (*Case) expr()      {}
func (*Call) expr()      {}
func (*Cast) expr()      {}
func (*Default) expr()   {}
func (*Subquery) expr()  {}

// Walk calls fn on e and then on every subexpression, depth first, while fn returns true.
func Walk(e Expr, fn func(Expr) bool) {
	if e == nil || !fn(e) {
		return
	}
	switch e := e.(type) {
	case *Unary:
		Walk(e.Expr, fn)
	case *Binary:
		Walk(e.Left, fn)
		Walk(e.Right, fn)
	case *IsNull:
		Walk(e.Expr, fn)
	case *Between:
		Walk(e.Expr, fn)
		Walk(e.Low, fn)
		Walk(e.High, fn)
	case *In:
		Walk(e.Expr, fn)
		for _, x := range e.List {
			Walk(x, fn)
		}
	case *Like:
		Walk(e.Expr, fn)
		Walk(e.Pattern, fn)
	case *Case:
		Walk(e.Operand, fn)
		for _, w := range e.Whens {
			Walk(w.Cond, fn)
			Walk(w.Result, fn)
		}
		Walk(e.Else, fn)
	case *Call:
		for _, x := range e.Args {
			Walk(x, fn)
		}
	case *Cast:
		Walk(e.Expr, fn)
	case *Subquery:
		Walk(e.Expr, fn)
	}
}

// Map rebuilds e bottom up, replacing every subexpression x, after its own subexpressions, with
// fn(x). It does not change e.
func Map(e Expr, fn func(Expr) Expr) Expr {
	if e == nil {
		return nil
	}
	m := func(x Expr) Expr { return Map(x, fn) }
	list := func(xs []Expr) []Expr {
		if xs == nil {
			return nil
		}
		out := make([]Expr, len(xs))
		for i, x := range xs {
			out[i] = m(x)
		}
		return out
	}
	switch e := e.(type) {
	case *Unary:
		return fn(&Unary{Op: e.Op, Expr: m(e.Expr)})
	case *Binary:
		return fn(&Binary{Op: e.Op, Left: m(e.Left), Right: m(e.Right)})
	case *IsNull:
		return fn(&IsNull{Expr: m(e.Expr), Not: e.Not})
	case *Between:
		return fn(&Between{Expr: m(e.Expr), Low: m(e.Low), High: m(e.High), Not: e.Not})
	case *In:
		return fn(&In{Expr: m(e.Expr), List: list(e.List), Not: e.Not})
	case *Like:
		return fn(&Like{Expr: m(e.Expr), Pattern: m(e.Pattern), Not: e.Not})
	case *Case:
		c := &Case{Operand: m(e.Operand), Else: m(e.Else)}
		for _, w := range e.Whens {
			c.Whens = append(c.Whens, When{Cond: m(w.Cond), Result: m(w.Result)})
		}
		return fn(c)
	case *Call:
		return fn(&Call{Name: e.Name, Args: list(e.Args), Keyword: e.Keyword, Star: e.Star, Distinct: e.Distinct})
	case *Cast:
		return fn(&Cast{Expr: m(e.Expr), Type: e.Type, Style: e.Style})
	case *Subquery:
		if e.Expr == nil {
			return fn(e)
		}
		return fn(&Subquery{Kind: e.Kind, Select: e.Select, Expr: m(e.Expr), Not: e.Not})
	}
	return fn(e)
}

// Selects calls fn on every SELECT of s, s first, then those of its WITH clause, of its
// derived tables, and of the subqueries of its expressions, depth first.
func Selects(s *Select, fn func(*Select)) {
	fn(s)
	for _, c := range s.With {
		Selects(c.Select, fn)
	}
	var from func(e TableExpr)
	from = func(e TableExpr) {
		switch e := e.(type) {
		case *Derived:
			Selects(e.Select, fn)
		case *Join:
			from(e.Left)
			from(e.Right)
			SubqueriesOf(e.On, func(sub *Select) { Selects(sub, fn) })
		}
	}
	for _, f := range s.From {
		from(f)
	}
	for _, e := range SelectExprs(s) {
		SubqueriesOf(e, func(sub *Select) { Selects(sub, fn) })
	}
}

// SelectExprs are the expressions of the clauses of s itself, without the conditions of its
// joins.
func SelectExprs(s *Select) []Expr {
	var out []Expr
	add := func(e Expr) {
		if e != nil {
			out = append(out, e)
		}
	}
	for _, item := range s.Items {
		add(item.Expr)
	}
	add(s.Where)
	for _, e := range s.GroupBy {
		add(e)
	}
	add(s.Having)
	for _, o := range s.OrderBy {
		add(o.Expr)
	}
	add(s.Limit)
	add(s.Offset)
	return out
}

// SubqueriesOf calls fn on the SELECT of every subquery in e that is not inside another one.
func SubqueriesOf(e Expr, fn func(*Select)) {
	Walk(e, func(x Expr) bool {
		if sub, ok := x.(*Subquery); ok {
			fn(sub.Select)
		}
		return true
	})
}
