package ast

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"maragu.dev/maragudb/internal/sql/value"
)

// Dialect to render to.
type Dialect int

// Dialects.
const (
	Maragudb Dialect = iota
	Postgres
)

// Render a statement in the given dialect.
func Render(s Statement, d Dialect) string {
	r := renderer{d: d}
	r.stmt(s)
	return r.b.String()
}

// RenderExpr in the given dialect.
func RenderExpr(e Expr, d Dialect) string {
	r := renderer{d: d}
	r.expr(e, 0)
	return r.b.String()
}

// TypeName of t in the given dialect.
func TypeName(t value.Type, d Dialect) string {
	if d == Postgres {
		switch t {
		case value.Int:
			return "bigint"
		case value.Float:
			return "double precision"
		case value.Bool:
			return "boolean"
		case value.String:
			return "text"
		case value.Bytes:
			return "bytea"
		case value.Time:
			return "timestamptz"
		}
	}
	return t.String()
}

type renderer struct {
	d Dialect
	b strings.Builder
}

func (r *renderer) stmt(s Statement) {
	switch s := s.(type) {
	case *CreateTable:
		r.b.WriteString("CREATE TABLE ")
		if s.IfNotExists {
			r.b.WriteString("IF NOT EXISTS ")
		}
		r.b.WriteString(s.Name)
		r.b.WriteString(" (")
		for i, c := range s.Columns {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.b.WriteString(c.Name)
			r.b.WriteByte(' ')
			r.b.WriteString(TypeName(c.Type, r.d))
			if c.NotNull {
				r.b.WriteString(" NOT NULL")
			}
		}
		r.b.WriteString(", PRIMARY KEY (")
		r.b.WriteString(strings.Join(s.PrimaryKey, ", "))
		r.b.WriteString("))")
	case *DropTable:
		r.b.WriteString("DROP TABLE ")
		if s.IfExists {
			r.b.WriteString("IF EXISTS ")
		}
		r.b.WriteString(s.Name)
	case *Insert:
		r.b.WriteString("INSERT INTO ")
		r.b.WriteString(s.Table)
		if len(s.Columns) > 0 {
			r.b.WriteString(" (")
			r.b.WriteString(strings.Join(s.Columns, ", "))
			r.b.WriteByte(')')
		}
		r.b.WriteString(" VALUES ")
		for i, row := range s.Rows {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.b.WriteByte('(')
			r.exprs(row)
			r.b.WriteByte(')')
		}
	case *Update:
		r.b.WriteString("UPDATE ")
		r.b.WriteString(s.Table)
		r.b.WriteString(" SET ")
		for i, a := range s.Set {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.b.WriteString(a.Column)
			r.b.WriteString(" = ")
			r.expr(a.Value, 0)
		}
		r.where(s.Where)
	case *Delete:
		r.b.WriteString("DELETE FROM ")
		r.b.WriteString(s.Table)
		r.where(s.Where)
	case *Select:
		r.b.WriteString("SELECT ")
		for i, item := range s.Items {
			if i > 0 {
				r.b.WriteString(", ")
			}
			if item.Star {
				r.b.WriteByte('*')
				continue
			}
			r.expr(item.Expr, 0)
			if item.Alias != "" {
				r.b.WriteString(" AS ")
				r.b.WriteString(item.Alias)
			}
		}
		if s.From != "" {
			r.b.WriteString(" FROM ")
			r.b.WriteString(s.From)
		}
		r.where(s.Where)
		if len(s.OrderBy) > 0 {
			r.b.WriteString(" ORDER BY ")
			for i, o := range s.OrderBy {
				if i > 0 {
					r.b.WriteString(", ")
				}
				r.expr(o.Expr, 0)
				if o.Desc {
					r.b.WriteString(" DESC")
				}
				switch o.Nulls {
				case NullsFirst:
					r.b.WriteString(" NULLS FIRST")
				case NullsLast:
					r.b.WriteString(" NULLS LAST")
				}
			}
		}
		if s.Limit != nil {
			r.b.WriteString(" LIMIT ")
			r.expr(s.Limit, 0)
		}
		if s.Offset != nil {
			r.b.WriteString(" OFFSET ")
			r.expr(s.Offset, 0)
		}
	case *Begin:
		r.b.WriteString("BEGIN")
		if s.ReadOnly {
			r.b.WriteString(" READ ONLY")
		}
	case *Commit:
		r.b.WriteString("COMMIT")
	case *Rollback:
		r.b.WriteString("ROLLBACK")
	default:
		panic(fmt.Sprintf("ast: rendering %T", s))
	}
}

func (r *renderer) where(e Expr) {
	if e != nil {
		r.b.WriteString(" WHERE ")
		r.expr(e, 0)
	}
}

func (r *renderer) exprs(es []Expr) {
	for i, e := range es {
		if i > 0 {
			r.b.WriteString(", ")
		}
		r.expr(e, 0)
	}
}

// Binding powers, tighter is higher, following the Postgres operator precedence table.
const (
	bpOr = iota + 1
	bpAnd
	bpNot
	bpIs
	bpCmp
	bpLike // BETWEEN, IN, LIKE
	bpConcat
	bpAdd
	bpMul
	bpUnary
	bpPrimary
)

var binaryOps = map[BinaryOp]struct {
	sym string
	bp  int
}{
	Add: {"+", bpAdd}, Sub: {"-", bpAdd}, Mul: {"*", bpMul}, Div: {"/", bpMul}, Mod: {"%", bpMul},
	Concat: {"||", bpConcat},
	Eq:     {"=", bpCmp}, Ne: {"<>", bpCmp}, Lt: {"<", bpCmp}, Le: {"<=", bpCmp}, Gt: {">", bpCmp}, Ge: {">=", bpCmp},
	And: {"AND", bpAnd}, Or: {"OR", bpOr},
}

func bindingPower(e Expr) int {
	switch e := e.(type) {
	case *Unary:
		if e.Op == Not {
			return bpNot
		}
		return bpUnary
	case *Binary:
		return binaryOps[e.Op].bp
	case *IsNull:
		return bpIs
	case *Between, *In, *Like:
		return bpLike
	}
	return bpPrimary
}

// expr renders e, parenthesized when its binding power is below minimum.
func (r *renderer) expr(e Expr, minimum int) {
	if bindingPower(e) < minimum {
		r.b.WriteByte('(')
		r.expr(e, 0)
		r.b.WriteByte(')')
		return
	}
	switch e := e.(type) {
	case *Literal:
		r.literal(e.Value)
	case *Param:
		fmt.Fprintf(&r.b, "$%d", e.Index)
	case *ColumnRef:
		r.b.WriteString(e.Name)
	case *Unary:
		if e.Op == Not {
			r.b.WriteString("NOT ")
			r.expr(e.Expr, bpNot)
			return
		}
		r.b.WriteByte('-')
		// A literal is parenthesized so that the sign is not folded into it when parsed again,
		// and a nested negation so that the two signs do not form a comment.
		if _, ok := e.Expr.(*Literal); ok {
			r.expr(e.Expr, bpPrimary+1)
			return
		}
		if u, ok := e.Expr.(*Unary); ok && u.Op == Neg {
			r.expr(e.Expr, bpPrimary+1)
			return
		}
		r.expr(e.Expr, bpUnary)
	case *Binary:
		op := binaryOps[e.Op]
		leftMin := op.bp
		if op.bp == bpCmp {
			// Comparisons do not associate.
			leftMin = op.bp + 1
		}
		r.expr(e.Left, leftMin)
		r.b.WriteByte(' ')
		r.b.WriteString(op.sym)
		r.b.WriteByte(' ')
		r.expr(e.Right, op.bp+1)
	case *IsNull:
		r.expr(e.Expr, bpIs+1)
		if e.Not {
			r.b.WriteString(" IS NOT NULL")
		} else {
			r.b.WriteString(" IS NULL")
		}
	case *Between:
		r.expr(e.Expr, bpLike+1)
		if e.Not {
			r.b.WriteString(" NOT")
		}
		r.b.WriteString(" BETWEEN ")
		r.expr(e.Low, bpConcat)
		r.b.WriteString(" AND ")
		r.expr(e.High, bpConcat)
	case *In:
		r.expr(e.Expr, bpLike+1)
		if e.Not {
			r.b.WriteString(" NOT")
		}
		r.b.WriteString(" IN (")
		r.exprs(e.List)
		r.b.WriteByte(')')
	case *Like:
		r.expr(e.Expr, bpLike+1)
		if e.Not {
			r.b.WriteString(" NOT")
		}
		r.b.WriteString(" LIKE ")
		r.expr(e.Pattern, bpLike+1)
	case *Case:
		r.b.WriteString("CASE")
		if e.Operand != nil {
			r.b.WriteByte(' ')
			r.expr(e.Operand, 0)
		}
		for _, w := range e.Whens {
			r.b.WriteString(" WHEN ")
			r.expr(w.Cond, 0)
			r.b.WriteString(" THEN ")
			r.expr(w.Result, 0)
		}
		if e.Else != nil {
			r.b.WriteString(" ELSE ")
			r.expr(e.Else, 0)
		}
		r.b.WriteString(" END")
	case *Call:
		r.b.WriteString(e.Name)
		r.b.WriteByte('(')
		r.exprs(e.Args)
		r.b.WriteByte(')')
	case *Cast:
		r.b.WriteString("CAST(")
		r.expr(e.Expr, 0)
		r.b.WriteString(" AS ")
		r.b.WriteString(TypeName(e.Type, r.d))
		r.b.WriteByte(')')
	default:
		panic(fmt.Sprintf("ast: rendering %T", e))
	}
}

func (r *renderer) literal(v value.Value) {
	if v.Null {
		r.b.WriteString("NULL")
		return
	}
	switch v.Type {
	case value.Int:
		if r.d == Postgres {
			fmt.Fprintf(&r.b, "(%d)::bigint", v.I)
		} else {
			r.b.WriteString(strconv.FormatInt(v.I, 10))
		}
	case value.Float:
		if math.IsNaN(v.F) || math.IsInf(v.F, 0) {
			if r.d == Postgres {
				fmt.Fprintf(&r.b, "'%s'::float8", value.FormatFloat(v.F))
			} else {
				fmt.Fprintf(&r.b, "CAST('%s' AS float)", value.FormatFloat(v.F))
			}
			return
		}
		s := value.FormatFloat(v.F)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		if r.d == Postgres {
			fmt.Fprintf(&r.b, "(%s)::float8", s)
		} else {
			r.b.WriteString(s)
		}
	case value.Bool:
		if v.B {
			r.b.WriteString("TRUE")
		} else {
			r.b.WriteString("FALSE")
		}
	case value.String:
		r.b.WriteString(QuoteString(v.S))
	case value.Bytes:
		if r.d == Postgres {
			fmt.Fprintf(&r.b, "'\\x%x'::bytea", v.Raw)
		} else {
			fmt.Fprintf(&r.b, "X'%x'", v.Raw)
		}
	case value.Time:
		s := v.T.UTC().Format(time.RFC3339Nano)
		if r.d == Postgres {
			fmt.Fprintf(&r.b, "'%s'::timestamptz", s)
		} else {
			fmt.Fprintf(&r.b, "TIME '%s'", s)
		}
	default:
		panic(fmt.Sprintf("ast: rendering literal of type %v", v.Type))
	}
}

// QuoteString as a SQL string literal, doubling single quotes.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
