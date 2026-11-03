// Package parser parses the SQL dialect into an [ast.Statement].
package parser

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

// ErrSyntax is wrapped by every parse error.
var ErrSyntax = errors.New("syntax error")

// Parse one statement, with an optional trailing semicolon.
func Parse(sql string) (ast.Statement, error) {
	p := &parser{lex: lexer{src: sql}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	s, err := p.statement()
	if err != nil {
		return nil, err
	}
	if p.tok.kind == tokOp && p.tok.text == ";" {
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if p.tok.kind != tokEOF {
		return nil, p.errorf("unexpected %v", p.tok)
	}
	return s, nil
}

// ParseExpr parses a single expression.
func ParseExpr(sql string) (ast.Expr, error) {
	p := &parser{lex: lexer{src: sql}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	e, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		return nil, p.errorf("unexpected %v", p.tok)
	}
	return e, nil
}

// ParseTime parses a time literal: RFC 3339 with a T or a space between date and time.
func ParseTime(s string) (time.Time, error) {
	if len(s) > 10 && s[10] == ' ' {
		s = s[:10] + "T" + s[11:]
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid time %q, want RFC 3339 such as 2006-01-02T15:04:05Z", s)
	}
	return t.UTC(), nil
}

type parser struct {
	lex lexer
	tok token
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w at position %d: %s", ErrSyntax, p.tok.pos+1, fmt.Sprintf(format, args...))
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) isKeyword(kw string) bool {
	return p.tok.kind == tokKeyword && p.tok.text == kw
}

// isWord reports whether the current token is the keyword or identifier w, for words that are
// keywords only in context.
func (p *parser) isWord(w string) bool {
	return (p.tok.kind == tokKeyword || p.tok.kind == tokIdent) && p.tok.text == w
}

func (p *parser) isOp(op string) bool {
	return p.tok.kind == tokOp && p.tok.text == op
}

// accept the keyword or contextual word and advance when present.
func (p *parser) accept(w string) (bool, error) {
	if !p.isWord(w) {
		return false, nil
	}
	return true, p.advance()
}

func (p *parser) expectWord(w string) error {
	if !p.isWord(w) {
		return p.errorf("expected %s, got %v", strings.ToUpper(w), p.tok)
	}
	return p.advance()
}

func (p *parser) expectOp(op string) error {
	if !p.isOp(op) {
		return p.errorf("expected %q, got %v", op, p.tok)
	}
	return p.advance()
}

func (p *parser) ident() (string, error) {
	if p.tok.kind != tokIdent {
		return "", p.errorf("expected identifier, got %v", p.tok)
	}
	name := p.tok.text
	return name, p.advance()
}

func (p *parser) statement() (ast.Statement, error) {
	switch {
	case p.isKeyword("create"):
		return p.createTable()
	case p.isKeyword("drop"):
		return p.dropTable()
	case p.isKeyword("insert"):
		return p.insert()
	case p.isKeyword("update"):
		return p.update()
	case p.isKeyword("delete"):
		return p.delete()
	case p.isKeyword("select"):
		return p.selectStmt()
	case p.isKeyword("begin"), p.isKeyword("start"):
		return p.begin()
	case p.isKeyword("commit"), p.isKeyword("end"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if _, err := p.accept("transaction"); err != nil {
			return nil, err
		}
		return &ast.Commit{}, nil
	case p.isKeyword("rollback"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if _, err := p.accept("transaction"); err != nil {
			return nil, err
		}
		return &ast.Rollback{}, nil
	}
	return nil, p.errorf("expected a statement, got %v", p.tok)
}

func (p *parser) begin() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if _, err := p.accept("transaction"); err != nil {
		return nil, err
	}
	s := &ast.Begin{}
	if ok, err := p.accept("read"); err != nil {
		return nil, err
	} else if ok {
		switch {
		case p.isWord("only"):
			s.ReadOnly = true
		case p.isWord("write"):
		default:
			return nil, p.errorf("expected ONLY or WRITE, got %v", p.tok)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (p *parser) createTable() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectWord("table"); err != nil {
		return nil, err
	}
	s := &ast.CreateTable{}
	if ok, err := p.accept("if"); err != nil {
		return nil, err
	} else if ok {
		if err := p.expectWord("not"); err != nil {
			return nil, err
		}
		if err := p.expectWord("exists"); err != nil {
			return nil, err
		}
		s.IfNotExists = true
	}
	var err error
	if s.Name, err = p.ident(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		if ok, err := p.accept("primary"); err != nil {
			return nil, err
		} else if ok {
			if s.PrimaryKey != nil {
				return nil, p.errorf("multiple primary keys for table %q", s.Name)
			}
			if err := p.expectWord("key"); err != nil {
				return nil, err
			}
			if err := p.expectOp("("); err != nil {
				return nil, err
			}
			for {
				col, err := p.ident()
				if err != nil {
					return nil, err
				}
				s.PrimaryKey = append(s.PrimaryKey, col)
				if p.isOp(",") {
					if err := p.advance(); err != nil {
						return nil, err
					}
					continue
				}
				break
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
		} else {
			col, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			if col.pk {
				if s.PrimaryKey != nil {
					return nil, p.errorf("multiple primary keys for table %q", s.Name)
				}
				s.PrimaryKey = []string{col.def.Name}
			}
			s.Columns = append(s.Columns, col.def)
		}
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	if len(s.Columns) == 0 {
		return nil, p.errorf("table %q needs at least one column", s.Name)
	}
	if s.PrimaryKey == nil {
		return nil, p.errorf("table %q needs a PRIMARY KEY", s.Name)
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return s, nil
}

type columnDef struct {
	def ast.ColumnDef
	pk  bool
}

func (p *parser) columnDef() (columnDef, error) {
	var c columnDef
	var err error
	if c.def.Name, err = p.ident(); err != nil {
		return c, err
	}
	if c.def.Type, err = p.typeName(); err != nil {
		return c, err
	}
	for {
		switch {
		case p.isKeyword("not"):
			if err := p.advance(); err != nil {
				return c, err
			}
			if err := p.expectWord("null"); err != nil {
				return c, err
			}
			c.def.NotNull = true
		case p.isKeyword("null"):
			if err := p.advance(); err != nil {
				return c, err
			}
		case p.isKeyword("primary"):
			if err := p.advance(); err != nil {
				return c, err
			}
			if err := p.expectWord("key"); err != nil {
				return c, err
			}
			c.pk = true
			c.def.NotNull = true
		default:
			return c, nil
		}
	}
}

func (p *parser) typeName() (value.Type, error) {
	if p.tok.kind != tokIdent {
		return 0, p.errorf("expected a type, got %v", p.tok)
	}
	t, ok := value.ParseType(p.tok.text)
	if !ok {
		return 0, p.errorf("unknown type %q, want int, float, bool, string, bytes, or time", p.tok.text)
	}
	return t, p.advance()
}

func (p *parser) dropTable() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectWord("table"); err != nil {
		return nil, err
	}
	s := &ast.DropTable{}
	if ok, err := p.accept("if"); err != nil {
		return nil, err
	} else if ok {
		if err := p.expectWord("exists"); err != nil {
			return nil, err
		}
		s.IfExists = true
	}
	var err error
	s.Name, err = p.ident()
	return s, err
}

func (p *parser) insert() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectWord("into"); err != nil {
		return nil, err
	}
	s := &ast.Insert{}
	var err error
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.isOp("(") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		for {
			col, err := p.ident()
			if err != nil {
				return nil, err
			}
			s.Columns = append(s.Columns, col)
			if p.isOp(",") {
				if err := p.advance(); err != nil {
					return nil, err
				}
				continue
			}
			break
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
	}
	if err := p.expectWord("values"); err != nil {
		return nil, err
	}
	for {
		if err := p.expectOp("("); err != nil {
			return nil, err
		}
		row, err := p.exprList()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		s.Rows = append(s.Rows, row)
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	return s, nil
}

func (p *parser) exprList() ([]ast.Expr, error) {
	var list []ast.Expr
	for {
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		list = append(list, e)
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		return list, nil
	}
}

func (p *parser) update() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	s := &ast.Update{}
	var err error
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if err := p.expectWord("set"); err != nil {
		return nil, err
	}
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		e, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		s.Set = append(s.Set, ast.Assignment{Column: col, Value: e})
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) delete() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectWord("from"); err != nil {
		return nil, err
	}
	s := &ast.Delete{}
	var err error
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	return s, nil
}

func (p *parser) where() (ast.Expr, error) {
	if ok, err := p.accept("where"); err != nil || !ok {
		return nil, err
	}
	return p.expr(0)
}

func (p *parser) selectStmt() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	s := &ast.Select{}
	for {
		var item ast.SelectItem
		if p.isOp("*") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			item.Star = true
		} else {
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			item.Expr = e
			if ok, err := p.accept("as"); err != nil {
				return nil, err
			} else if ok || p.tok.kind == tokIdent {
				if item.Alias, err = p.ident(); err != nil {
					return nil, err
				}
			}
		}
		s.Items = append(s.Items, item)
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	if ok, err := p.accept("from"); err != nil {
		return nil, err
	} else if ok {
		if s.From, err = p.ident(); err != nil {
			return nil, err
		}
	}
	var err error
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	if ok, err := p.accept("order"); err != nil {
		return nil, err
	} else if ok {
		if err := p.expectWord("by"); err != nil {
			return nil, err
		}
		for {
			var o ast.OrderItem
			if o.Expr, err = p.expr(0); err != nil {
				return nil, err
			}
			if ok, err := p.accept("asc"); err != nil {
				return nil, err
			} else if !ok {
				if o.Desc, err = p.accept("desc"); err != nil {
					return nil, err
				}
			}
			if ok, err := p.accept("nulls"); err != nil {
				return nil, err
			} else if ok {
				switch {
				case p.isWord("first"):
					o.Nulls = ast.NullsFirst
				case p.isWord("last"):
					o.Nulls = ast.NullsLast
				default:
					return nil, p.errorf("expected FIRST or LAST, got %v", p.tok)
				}
				if err := p.advance(); err != nil {
					return nil, err
				}
			}
			s.OrderBy = append(s.OrderBy, o)
			if p.isOp(",") {
				if err := p.advance(); err != nil {
					return nil, err
				}
				continue
			}
			break
		}
	}
	if ok, err := p.accept("limit"); err != nil {
		return nil, err
	} else if ok {
		if s.Limit, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	if ok, err := p.accept("offset"); err != nil {
		return nil, err
	} else if ok {
		if s.Offset, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Binding powers, tighter is higher, following the Postgres operator precedence table.
const (
	bpOr = iota + 1
	bpAnd
	bpNot
	bpIs
	bpCmp
	bpLike
	bpConcat
	bpAdd
	bpMul
	bpUnary
)

var binaryOps = map[string]struct {
	op ast.BinaryOp
	bp int
}{
	"or": {ast.Or, bpOr}, "and": {ast.And, bpAnd},
	"=": {ast.Eq, bpCmp}, "<>": {ast.Ne, bpCmp}, "<": {ast.Lt, bpCmp}, "<=": {ast.Le, bpCmp}, ">": {ast.Gt, bpCmp}, ">=": {ast.Ge, bpCmp},
	"||": {ast.Concat, bpConcat},
	"+":  {ast.Add, bpAdd}, "-": {ast.Sub, bpAdd},
	"*": {ast.Mul, bpMul}, "/": {ast.Div, bpMul}, "%": {ast.Mod, bpMul},
}

// expr parses an expression whose operators all bind at least as tightly as minBp.
func (p *parser) expr(minBp int) (ast.Expr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.tok.kind == tokOp || p.isKeyword("and") || p.isKeyword("or"):
			op, ok := binaryOps[p.tok.text]
			if !ok || op.bp < minBp {
				return left, nil
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			right, err := p.expr(op.bp + 1)
			if err != nil {
				return nil, err
			}
			left = &ast.Binary{Op: op.op, Left: left, Right: right}
			if next, ok := binaryOps[p.tok.text]; op.bp == bpCmp && p.tok.kind == tokOp && ok && next.bp == bpCmp {
				return nil, p.errorf("comparisons do not chain, use parentheses")
			}
		case p.isKeyword("is"):
			if bpIs < minBp {
				return left, nil
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			not, err := p.accept("not")
			if err != nil {
				return nil, err
			}
			if err := p.expectWord("null"); err != nil {
				return nil, err
			}
			left = &ast.IsNull{Expr: left, Not: not}
		case p.isKeyword("not"), p.isKeyword("between"), p.isKeyword("in"), p.isKeyword("like"):
			if bpLike < minBp {
				return left, nil
			}
			not := false
			if p.isKeyword("not") {
				not = true
				if err := p.advance(); err != nil {
					return nil, err
				}
			}
			switch {
			case p.isKeyword("between"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				low, err := p.expr(bpLike + 1)
				if err != nil {
					return nil, err
				}
				if err := p.expectWord("and"); err != nil {
					return nil, err
				}
				high, err := p.expr(bpLike + 1)
				if err != nil {
					return nil, err
				}
				left = &ast.Between{Expr: left, Low: low, High: high, Not: not}
			case p.isKeyword("in"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				if err := p.expectOp("("); err != nil {
					return nil, err
				}
				list, err := p.exprList()
				if err != nil {
					return nil, err
				}
				if err := p.expectOp(")"); err != nil {
					return nil, err
				}
				left = &ast.In{Expr: left, List: list, Not: not}
			case p.isKeyword("like"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				pattern, err := p.expr(bpLike + 1)
				if err != nil {
					return nil, err
				}
				left = &ast.Like{Expr: left, Pattern: pattern, Not: not}
			default:
				return nil, p.errorf("expected BETWEEN, IN, or LIKE after NOT, got %v", p.tok)
			}
		default:
			return left, nil
		}
	}
}

func (p *parser) unary() (ast.Expr, error) {
	switch {
	case p.isKeyword("not"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		e, err := p.expr(bpNot + 1)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: ast.Not, Expr: e}, nil
	case p.isOp("-"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind == tokNumber {
			// A sign directly before a number is part of the literal, as in Postgres, so that
			// the most negative int can be written.
			return p.number("-")
		}
		e, err := p.expr(bpUnary)
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: ast.Neg, Expr: e}, nil
	case p.isOp("+"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		return p.expr(bpUnary)
	}
	return p.primary()
}

func (p *parser) number(sign string) (ast.Expr, error) {
	text := sign + p.tok.text
	pos := p.tok.pos
	if err := p.advance(); err != nil {
		return nil, err
	}
	if strings.ContainsAny(text, ".eE") {
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("%w at position %d: invalid float %q", ErrSyntax, pos+1, text)
		}
		return &ast.Literal{Value: value.NewFloat(f)}, nil
	}
	i, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w at position %d: integer %s is out of range for int", ErrSyntax, pos+1, text)
	}
	return &ast.Literal{Value: value.NewInt(i)}, nil
}

func (p *parser) primary() (ast.Expr, error) {
	tok := p.tok
	switch tok.kind {
	case tokNumber:
		return p.number("")
	case tokString:
		return &ast.Literal{Value: value.NewString(tok.text)}, p.advance()
	case tokBytes:
		b, err := hex.DecodeString(tok.text)
		if err != nil {
			return nil, p.errorf("invalid bytes literal: %v", err)
		}
		return &ast.Literal{Value: value.NewBytes(b)}, p.advance()
	case tokParam:
		n, err := strconv.Atoi(tok.text)
		if err != nil || n < 1 {
			return nil, p.errorf("invalid parameter $%s", tok.text)
		}
		return &ast.Param{Index: n}, p.advance()
	case tokKeyword:
		switch tok.text {
		case "true":
			return &ast.Literal{Value: value.NewBool(true)}, p.advance()
		case "false":
			return &ast.Literal{Value: value.NewBool(false)}, p.advance()
		case "null":
			return &ast.Literal{Value: value.NewNull(value.Unknown)}, p.advance()
		case "cast":
			return p.cast()
		case "case":
			return p.caseExpr()
		}
	case tokIdent:
		if err := p.advance(); err != nil {
			return nil, err
		}
		if tok.text == "time" && p.tok.kind == tokString {
			t, err := ParseTime(p.tok.text)
			if err != nil {
				return nil, p.errorf("%v", err)
			}
			return &ast.Literal{Value: value.NewTime(t)}, p.advance()
		}
		if p.isOp("(") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			var args []ast.Expr
			if !p.isOp(")") {
				var err error
				if args, err = p.exprList(); err != nil {
					return nil, err
				}
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return &ast.Call{Name: tok.text, Args: args}, nil
		}
		return &ast.ColumnRef{Name: tok.text}, nil
	case tokOp:
		if tok.text == "(" {
			if err := p.advance(); err != nil {
				return nil, err
			}
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, p.errorf("expected an expression, got %v", tok)
}

func (p *parser) cast() (ast.Expr, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	e, err := p.expr(0)
	if err != nil {
		return nil, err
	}
	if err := p.expectWord("as"); err != nil {
		return nil, err
	}
	t, err := p.typeName()
	if err != nil {
		return nil, err
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	return &ast.Cast{Expr: e, Type: t}, nil
}

func (p *parser) caseExpr() (ast.Expr, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	c := &ast.Case{}
	if !p.isKeyword("when") {
		var err error
		if c.Operand, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	for p.isKeyword("when") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		cond, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		if err := p.expectWord("then"); err != nil {
			return nil, err
		}
		result, err := p.expr(0)
		if err != nil {
			return nil, err
		}
		c.Whens = append(c.Whens, ast.When{Cond: cond, Result: result})
	}
	if len(c.Whens) == 0 {
		return nil, p.errorf("expected WHEN, got %v", p.tok)
	}
	if ok, err := p.accept("else"); err != nil {
		return nil, err
	} else if ok {
		if c.Else, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	if err := p.expectWord("end"); err != nil {
		return nil, err
	}
	return c, nil
}
