// Package ast is the syntax tree of the SQL dialect, with renderers to that dialect and to
// Postgres.
package ast

import "maragu.dev/maragudb/internal/sql/value"

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

// Literal value. A NULL literal has a Value with Null set and type [value.Unknown].
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

// Cast is `CAST(expr AS type)`.
type Cast struct {
	Expr Expr
	Type value.Type
}

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
