package ast

import (
	"fmt"
	"strconv"
	"strings"

	"maragu.dev/maragudb/internal/sql/value"
)

// Render a statement as Postgres SQL.
func Render(s Statement) string {
	var r renderer
	r.stmt(s)
	return r.b.String()
}

// RenderExpr as Postgres SQL.
func RenderExpr(e Expr) string {
	var r renderer
	r.expr(e, 0)
	return r.b.String()
}

// TypeName of a column type in SQL.
func TypeName(t value.Type) string {
	return t.String()
}

type renderer struct {
	b strings.Builder
	// restricted is set while rendering the restricted expression of a DEFAULT, Postgres's
	// b_expr, in which a boolean operator, IS, LIKE, BETWEEN, and IN must be in parentheses,
	// unless they are inside a construct with delimiters of its own, such as a function call.
	restricted bool
}

func (r *renderer) stmt(s Statement) {
	switch s := s.(type) {
	case *CreateTable:
		r.b.WriteString("CREATE TABLE ")
		if s.IfNotExists {
			r.b.WriteString("IF NOT EXISTS ")
		}
		r.ident(s.Name)
		r.b.WriteString(" (")
		for i, c := range s.Columns {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.ident(c.Name)
			r.b.WriteByte(' ')
			r.b.WriteString(TypeName(c.Type))
			if c.NotNull {
				r.b.WriteString(" NOT NULL")
			}
			if c.Default != nil {
				// A DEFAULT takes a restricted expression, in which only a parenthesized
				// expression can have a boolean operator, IS, LIKE, BETWEEN, or IN.
				r.b.WriteString(" DEFAULT ")
				r.restricted = true
				r.expr(c.Default, 0)
				r.restricted = false
			}
		}
		r.b.WriteString(", ")
		if s.PrimaryKeyName != "" {
			r.b.WriteString("CONSTRAINT ")
			r.ident(s.PrimaryKeyName)
			r.b.WriteByte(' ')
		}
		r.b.WriteString("PRIMARY KEY (")
		r.idents(s.PrimaryKey)
		r.b.WriteByte(')')
		for _, u := range s.Unique {
			r.b.WriteString(", ")
			if u.Name != "" {
				r.b.WriteString("CONSTRAINT ")
				r.ident(u.Name)
				r.b.WriteByte(' ')
			}
			r.b.WriteString("UNIQUE ")
			if u.NullsNotDistinct {
				r.b.WriteString("NULLS NOT DISTINCT ")
			}
			r.b.WriteByte('(')
			r.idents(u.Columns)
			r.b.WriteByte(')')
		}
		for _, c := range s.Checks {
			r.b.WriteString(", ")
			r.constraintName(c.Name)
			r.b.WriteString("CHECK (")
			r.expr(c.Expr, 0)
			r.b.WriteByte(')')
		}
		for _, f := range s.ForeignKeys {
			r.b.WriteString(", ")
			r.constraintName(f.Name)
			r.b.WriteString("FOREIGN KEY (")
			r.idents(f.Columns)
			r.b.WriteString(") REFERENCES ")
			r.ident(f.Table)
			if len(f.RefColumns) > 0 {
				r.b.WriteString(" (")
				r.idents(f.RefColumns)
				r.b.WriteByte(')')
			}
			if f.OnDelete != NoAction {
				r.b.WriteString(" ON DELETE ")
				r.b.WriteString(f.OnDelete.String())
				if len(f.SetColumns) > 0 {
					r.b.WriteString(" (")
					r.idents(f.SetColumns)
					r.b.WriteByte(')')
				}
			}
			if f.OnUpdate != NoAction {
				r.b.WriteString(" ON UPDATE ")
				r.b.WriteString(f.OnUpdate.String())
			}
		}
		r.b.WriteByte(')')
	case *DropTable:
		r.b.WriteString("DROP TABLE ")
		if s.IfExists {
			r.b.WriteString("IF EXISTS ")
		}
		r.ident(s.Name)
	case *CreateIndex:
		r.b.WriteString("CREATE ")
		if s.Unique {
			r.b.WriteString("UNIQUE ")
		}
		r.b.WriteString("INDEX ")
		if s.IfNotExists {
			r.b.WriteString("IF NOT EXISTS ")
		}
		if s.Name != "" {
			r.ident(s.Name)
			r.b.WriteByte(' ')
		}
		r.b.WriteString("ON ")
		r.ident(s.Table)
		r.b.WriteString(" (")
		for i, c := range s.Columns {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.ident(c.Name)
			if c.Desc {
				r.b.WriteString(" DESC")
			}
			switch c.Nulls {
			case NullsFirst:
				r.b.WriteString(" NULLS FIRST")
			case NullsLast:
				r.b.WriteString(" NULLS LAST")
			}
		}
		r.b.WriteByte(')')
		if s.NullsNotDistinct {
			r.b.WriteString(" NULLS NOT DISTINCT")
		}
	case *DropIndex:
		r.b.WriteString("DROP INDEX ")
		if s.IfExists {
			r.b.WriteString("IF EXISTS ")
		}
		r.ident(s.Name)
	case *Insert:
		r.b.WriteString("INSERT INTO ")
		r.ident(s.Table)
		if len(s.Columns) > 0 {
			r.b.WriteString(" (")
			r.idents(s.Columns)
			r.b.WriteByte(')')
		}
		if s.DefaultValues {
			r.b.WriteString(" DEFAULT VALUES")
			return
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
		r.ident(s.Table)
		r.b.WriteString(" SET ")
		for i, a := range s.Set {
			if i > 0 {
				r.b.WriteString(", ")
			}
			r.ident(a.Column)
			r.b.WriteString(" = ")
			r.expr(a.Value, 0)
		}
		r.where(s.Where)
	case *Delete:
		r.b.WriteString("DELETE FROM ")
		r.ident(s.Table)
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
				r.ident(item.Alias)
			}
		}
		if s.From != "" {
			r.b.WriteString(" FROM ")
			r.ident(s.From)
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

// constraintName writes CONSTRAINT and the name, when there is one.
func (r *renderer) constraintName(name string) {
	if name != "" {
		r.b.WriteString("CONSTRAINT ")
		r.ident(name)
		r.b.WriteByte(' ')
	}
}

// ident writes an identifier, quoted unless it is a plain lowercase name that is not a keyword.
func (r *renderer) ident(name string) {
	if IsPlainIdentifier(name) {
		r.b.WriteString(name)
		return
	}
	r.b.WriteByte('"')
	r.b.WriteString(strings.ReplaceAll(name, `"`, `""`))
	r.b.WriteByte('"')
}

func (r *renderer) idents(names []string) {
	for i, n := range names {
		if i > 0 {
			r.b.WriteString(", ")
		}
		r.ident(n)
	}
}

// IsPlainIdentifier reports whether name can be written without quotes: lowercase ASCII letters,
// digits, and underscores, not starting with a digit, and not a keyword.
func IsPlainIdentifier(name string) bool {
	if name == "" || Keywords[name] {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
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
	bpCast
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
	case *Cast:
		if e.Style == CastColons {
			return bpCast
		}
	}
	return bpPrimary
}

// restrictable reports whether e can be written without parentheses in a restricted
// expression.
func restrictable(e Expr) bool {
	switch e := e.(type) {
	case *IsNull, *Between, *In, *Like:
		return false
	case *Unary:
		return e.Op != Not
	case *Binary:
		return e.Op != And && e.Op != Or
	}
	return true
}

// expr renders e, parenthesized when its binding power is below minimum.
func (r *renderer) expr(e Expr, minimum int) {
	if bindingPower(e) < minimum || r.restricted && !restrictable(e) {
		restricted := r.restricted
		r.restricted = false
		r.b.WriteByte('(')
		r.expr(e, 0)
		r.b.WriteByte(')')
		r.restricted = restricted
		return
	}
	if r.restricted {
		switch e.(type) {
		case *Case, *Call, *Cast:
			// Their parts have delimiters of their own, except for a cast in :: style.
			if c, ok := e.(*Cast); !ok || c.Style != CastColons {
				r.restricted = false
				defer func() { r.restricted = true }()
			}
		}
	}
	switch e := e.(type) {
	case *Literal:
		r.literal(e.Value)
	case *Param:
		fmt.Fprintf(&r.b, "$%d", e.Index)
	case *ColumnRef:
		r.ident(e.Name)
	case *Unary:
		if e.Op == Not {
			r.b.WriteString("NOT ")
			r.expr(e.Expr, bpNot)
			return
		}
		if e.Op == Pos {
			r.b.WriteByte('+')
		} else {
			r.b.WriteByte('-')
		}
		// A literal is parenthesized so that the sign is not folded into it when parsed again,
		// and a nested sign so that two minus signs do not form a comment.
		if _, ok := e.Expr.(*Literal); ok {
			r.expr(e.Expr, bpPrimary+1)
			return
		}
		if u, ok := e.Expr.(*Unary); ok && u.Op != Not {
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
	case *Default:
		r.b.WriteString("DEFAULT")
	case *Call:
		if e.Keyword {
			r.b.WriteString(strings.ToUpper(e.Name))
			return
		}
		r.ident(e.Name)
		r.b.WriteByte('(')
		r.exprs(e.Args)
		r.b.WriteByte(')')
	case *Cast:
		switch e.Style {
		case CastColons:
			// A negative number is parenthesized so that its sign is not read as an operator
			// applied to the cast when parsed again.
			if lit, ok := e.Expr.(*Literal); ok && isNegativeNumber(lit.Value) {
				r.expr(e.Expr, bpPrimary+1)
			} else {
				r.expr(e.Expr, bpCast)
			}
			r.b.WriteString("::")
			r.b.WriteString(TypeName(e.Type))
		case CastPrefix:
			r.b.WriteString(TypeName(e.Type))
			r.b.WriteByte(' ')
			r.expr(e.Expr, 0)
		default:
			r.b.WriteString("CAST(")
			r.expr(e.Expr, 0)
			r.b.WriteString(" AS ")
			r.b.WriteString(TypeName(e.Type))
			r.b.WriteByte(')')
		}
	default:
		panic(fmt.Sprintf("ast: rendering %T", e))
	}
}

// isNegativeNumber reports whether a literal is a number written with a minus sign.
func isNegativeNumber(v value.Value) bool {
	switch {
	case v.Null:
		return false
	case v.Type == value.Int, v.Type == value.Int4:
		return v.I < 0
	case v.Type == value.Numeric:
		return strings.HasPrefix(v.S, "-")
	}
	return false
}

func (r *renderer) literal(v value.Value) {
	if v.Null {
		r.b.WriteString("NULL")
		return
	}
	switch v.Type {
	case value.Int, value.Int4:
		r.b.WriteString(strconv.FormatInt(v.I, 10))
	case value.Numeric:
		r.b.WriteString(v.S)
	case value.Unknown:
		r.b.WriteString(QuoteString(v.S))
	case value.Bool:
		if v.B {
			r.b.WriteString("TRUE")
		} else {
			r.b.WriteString("FALSE")
		}
	default:
		// A typed value is written as the cast of its text, which is how Postgres reads it.
		r.b.WriteString("CAST(")
		r.b.WriteString(QuoteString(value.Format(v)))
		r.b.WriteString(" AS ")
		r.b.WriteString(TypeName(v.Type))
		r.b.WriteByte(')')
	}
}

// QuoteString as a SQL string literal, doubling single quotes.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
