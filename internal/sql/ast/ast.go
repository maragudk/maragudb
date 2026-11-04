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
}

// ColumnDef in a [CreateTable].
type ColumnDef struct {
	Name    string
	Type    value.Type
	NotNull bool
}

// DropTable statement.
type DropTable struct {
	Name     string
	IfExists bool
}

// Insert statement with one or more VALUES rows. An empty Columns list means every column in
// table order.
type Insert struct {
	Table   string
	Columns []string
	Rows    [][]Expr
}

// Update statement.
type Update struct {
	Table string
	Set   []Assignment
	Where Expr
}

// Assignment in an [Update].
type Assignment struct {
	Column string
	Value  Expr
}

// Delete statement.
type Delete struct {
	Table string
	Where Expr
}

// Select statement. An empty From selects a single row without a table.
type Select struct {
	Items   []SelectItem
	From    string
	Where   Expr
	OrderBy []OrderItem
	Limit   Expr
	Offset  Expr
}

// SelectItem is a `*` or an expression with an optional alias.
type SelectItem struct {
	Star  bool
	Expr  Expr
	Alias string
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

// ColumnRef by name.
type ColumnRef struct {
	Name string
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

// Call of a scalar function by lowercase name.
type Call struct {
	Name string
	Args []Expr
}

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
		return fn(&Call{Name: e.Name, Args: list(e.Args)})
	case *Cast:
		return fn(&Cast{Expr: m(e.Expr), Type: e.Type, Style: e.Style})
	}
	return fn(e)
}
