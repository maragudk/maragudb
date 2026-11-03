package parser_test

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
	"maragu.dev/maragudb/internal/sql/value"
)

func TestParse(t *testing.T) {
	t.Run("parses statements and renders them back canonically", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"create table t (id int primary key, name string not null, f float null)", "CREATE TABLE t (id int NOT NULL, name string NOT NULL, f float, PRIMARY KEY (id))"},
			{"CREATE TABLE IF NOT EXISTS t (a int, b bytes, c bool, d time, PRIMARY KEY (a, b))", "CREATE TABLE IF NOT EXISTS t (a int, b bytes, c bool, d time, PRIMARY KEY (a, b))"},
			{"drop table t", "DROP TABLE t"},
			{"drop table if exists T", "DROP TABLE IF EXISTS t"},
			{"insert into t values (1, 'a'), (2, 'b')", "INSERT INTO t VALUES (1, 'a'), (2, 'b')"},
			{"insert into t (a, b) values ($1, $2)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
			{"update t set a = 1, b = b + 1 where id = 3", "UPDATE t SET a = 1, b = b + 1 WHERE id = 3"},
			{"update t set a = 1", "UPDATE t SET a = 1"},
			{"delete from t where a is null", "DELETE FROM t WHERE a IS NULL"},
			{"delete from t", "DELETE FROM t"},
			{"select * from t", "SELECT * FROM t"},
			{"select a, b as c, d e from t where a > 1 order by b desc nulls first, c asc nulls last, d limit 10 offset 5", "SELECT a, b AS c, d AS e FROM t WHERE a > 1 ORDER BY b DESC NULLS FIRST, c NULLS LAST, d LIMIT 10 OFFSET 5"},
			{"select 1 + 1", "SELECT 1 + 1"},
			{"select 1;", "SELECT 1"},
			{"begin", "BEGIN"},
			{"begin transaction read only", "BEGIN READ ONLY"},
			{"start transaction read write", "BEGIN"},
			{"commit", "COMMIT"},
			{"end transaction", "COMMIT"},
			{"rollback", "ROLLBACK"},
			{"select a from t -- comment\n where b = 1 /* block */", "SELECT a FROM t WHERE b = 1"},
			{`select "Quoted", "select", "a b", "x""y" from "key"`, `SELECT "Quoted", "select", "a b", "x""y" FROM "key"`},
			{`select "cast"(1)`, `SELECT "cast"(1)`},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				s, err := parser.Parse(test.sql)
				is.NotError(t, err)
				is.Equal(t, test.want, ast.Render(s, ast.Maragudb))
			})
		}
	})

	t.Run("parses expressions with Postgres precedence", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"a + b * c", "a + b * c"},
			{"(a + b) * c", "(a + b) * c"},
			{"a - b - c", "a - b - c"},
			{"a - (b - c)", "a - (b - c)"},
			{"a / b % c", "a / b % c"},
			{"-a * b", "-a * b"},
			{"-(a * b)", "-(a * b)"},
			{"- 5", "-5"},
			{"-(5)", "-(5)"},
			{"2 - -5", "2 - -5"},
			{"- -a", "-(-a)"},
			{"+a", "a"},
			{"a || b || c", "a || b || c"},
			{"a || (b || c)", "a || (b || c)"},
			{"a + b || c + d", "a + b || c + d"},
			{"a = b", "a = b"},
			{"a != b", "a <> b"},
			{"a < b", "a < b"},
			{"a <= b", "a <= b"},
			{"a > b", "a > b"},
			{"a >= b", "a >= b"},
			{"a = b AND c = d OR e = f", "a = b AND c = d OR e = f"},
			{"a = b AND (c = d OR e = f)", "a = b AND (c = d OR e = f)"},
			{"NOT a = b", "NOT a = b"},
			{"NOT (a = b)", "NOT a = b"},
			{"(NOT a) = b", "(NOT a) = b"},
			{"NOT a AND b", "NOT a AND b"},
			{"NOT (a AND b)", "NOT (a AND b)"},
			{"NOT NOT a", "NOT NOT a"},
			{"a = b IS NULL", "a = b IS NULL"},
			{"a = (b IS NULL)", "a = (b IS NULL)"},
			{"(a IS NULL) = b", "(a IS NULL) = b"},
			{"a IS NOT NULL", "a IS NOT NULL"},
			{"NOT a IS NULL", "NOT a IS NULL"},
			{"a BETWEEN 1 AND 2 AND b", "a BETWEEN 1 AND 2 AND b"},
			{"a BETWEEN 1 + 1 AND 2 || 'x'", "a BETWEEN 1 + 1 AND 2 || 'x'"},
			{"a NOT BETWEEN 1 AND 2", "a NOT BETWEEN 1 AND 2"},
			{"a IN (1, 2, 3)", "a IN (1, 2, 3)"},
			{"a NOT IN (1)", "a NOT IN (1)"},
			{"a LIKE 'x%'", "a LIKE 'x%'"},
			{"a NOT LIKE 'x%' || b", "a NOT LIKE 'x%' || b"},
			{"a < b IN (TRUE)", "a < b IN (TRUE)"},
			{"a < (b IN (TRUE))", "a < b IN (TRUE)"},
			{"(a < b) IN (TRUE)", "(a < b) IN (TRUE)"},
			{"a + b BETWEEN 1 AND 2", "a + b BETWEEN 1 AND 2"},
			{"CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END", "CASE WHEN a THEN 1 WHEN b THEN 2 ELSE 3 END"},
			{"CASE a WHEN 1 THEN 'x' END", "CASE a WHEN 1 THEN 'x' END"},
			{"CAST(a AS int) + CAST('1.5' AS float)", "CAST(a AS int) + CAST('1.5' AS float)"},
			{"coalesce(a, b, 1)", "coalesce(a, b, 1)"},
			{"LENGTH(Lower(x))", "length(lower(x))"},
			{"abs(-1)", "abs(-1)"},
			{"1.5 + 2e3 + .5 + 1E-2", "1.5 + 2000.0 + 0.5 + 0.01"},
			{"'it''s'", "'it''s'"},
			{"X'0aFF'", "X'0aff'"},
			{"x''", "X''"},
			{"TRUE AND FALSE OR NULL", "TRUE AND FALSE OR NULL"},
			{"$1 + $2", "$1 + $2"},
			{"time '2024-01-02T03:04:05.5Z'", "TIME '2024-01-02T03:04:05.5Z'"},
			{"TIME '2024-01-02 03:04:05+02:00'", "TIME '2024-01-02T01:04:05Z'"},
			{"time", "time"},
			{"-9223372036854775808", "-9223372036854775808"},
			{"((a))", "a"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				e, err := parser.ParseExpr(test.sql)
				is.NotError(t, err)
				is.Equal(t, test.want, ast.RenderExpr(e, ast.Maragudb))
			})
		}
	})

	t.Run("rejects invalid statements with a position", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"", "syntax error at position 1: expected a statement, got end of statement"},
			{"selec 1", "syntax error at position 1: expected a statement, got \"selec\""},
			{"select 1 2", "syntax error at position 10: unexpected \"2\""},
			{"select 1; select 2", "syntax error at position 11: unexpected SELECT"},
			{"select from t", "syntax error at position 8: expected an expression, got FROM"},
			{"select 'abc", "syntax error at position 8: unterminated string"},
			{"select 9223372036854775808", "syntax error at position 8: integer 9223372036854775808 is out of range for int"},
			{"select 1e", "syntax error at position 8: exponent needs digits"},
			{"select 1abc", "syntax error at position 8: invalid number"},
			{"select x'abc'", "syntax error at position 8: bytes literal needs an even number of hex digits"},
			{"select $", "syntax error at position 8: parameter needs a number"},
			{"select $0", "syntax error at position 8: invalid parameter $0"},
			{"select a from t where", "syntax error at position 22: expected an expression, got end of statement"},
			{"select cast(a as integer)", "syntax error at position 18: unknown type \"integer\", want int, float, bool, string, bytes, or time"},
			{"select case end", "syntax error at position 13: expected an expression, got END"},
			{"select case 1 end", "syntax error at position 15: expected WHEN, got END"},
			{"select a = b = c", "syntax error at position 14: comparisons do not chain, use parentheses"},
			{"select a < b >= c", "syntax error at position 14: comparisons do not chain, use parentheses"},
			{"select a not 1", "syntax error at position 14: expected BETWEEN, IN, or LIKE after NOT, got \"1\""},
			{"create table t (a int)", "syntax error at position 22: table \"t\" needs a PRIMARY KEY"},
			{"create table t (a int primary key, b int primary key)", "syntax error at position 53: multiple primary keys for table \"t\""},
			{"select time 'yesterday'", "syntax error at position 13: invalid time \"yesterday\", want RFC 3339 such as 2006-01-02T15:04:05Z"},
			{"select @", "syntax error at position 8: unexpected character '@'"},
			{"select a from t order by b nulls sometimes", "syntax error at position 34: expected FIRST or LAST, got \"sometimes\""},
			{"begin read sometimes", "syntax error at position 12: expected ONLY or WRITE, got \"sometimes\""},
			{"select /* unterminated", "syntax error at position 8: unterminated comment"},
			{"select \"\"", "syntax error at position 8: empty quoted identifier"},
			{"select \"a\x00b\"", "syntax error at position 8: quoted identifier must be valid UTF-8 without NUL bytes"},
			{"select \"\xff\"", "syntax error at position 8: quoted identifier must be valid UTF-8 without NUL bytes"},
			{"create table t (primary key (id))", "syntax error at position 33: table \"t\" needs at least one column"},
			{"select 'a' from select", "syntax error at position 17: expected identifier, got SELECT"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				_, err := parser.Parse(test.sql)
				is.Error(t, parser.ErrSyntax, err)
				is.Equal(t, test.want, err.Error())
			})
		}
	})

	t.Run("round trips every statement through the renderer", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			s := drawStatement(ht)
			sql := ast.Render(s, ast.Maragudb)
			ht.Note(sql)
			got, err := parser.Parse(sql)
			if err != nil {
				ht.Fatalf("parse %q: %v", sql, err)
			}
			if !reflect.DeepEqual(s, got) {
				ht.Fatalf("round trip of %q gave %q", sql, ast.Render(got, ast.Maragudb))
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("round trips every expression, so rendering adds exactly the parentheses the precedence needs", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			e := drawExpr(ht, 0)
			sql := ast.RenderExpr(e, ast.Maragudb)
			ht.Note(sql)
			got, err := parser.ParseExpr(sql)
			if err != nil {
				ht.Fatalf("parse %q: %v", sql, err)
			}
			if !reflect.DeepEqual(e, got) {
				ht.Fatalf("round trip of %q gave %q", sql, ast.RenderExpr(got, ast.Maragudb))
			}
		}, hegel.WithTestCases(5000))
	})

	t.Run("never panics on arbitrary input", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			sql := hegel.Draw(ht, hegel.OneOf(
				hegel.Text().MaxSize(40),
				hegel.Text().Alphabet("select from where a b 1 2 ( ) , + - * / || = < > not and or is null in between like case when then else end cast as int $ ' x \" ; -- /* */ \n ").MaxSize(60),
			))
			_, _ = parser.Parse(sql)
			_, _ = parser.ParseExpr(sql)
		}, hegel.WithTestCases(3000))
	})
}

func TestParseTime(t *testing.T) {
	t.Run("parses RFC 3339 with T or space and normalizes to UTC", func(t *testing.T) {
		for _, s := range []string{"2024-01-02T03:04:05Z", "2024-01-02 03:04:05Z", "2024-01-02T05:04:05+02:00", "2024-01-02T03:04:05.000000000Z"} {
			got, err := parser.ParseTime(s)
			is.NotError(t, err)
			is.Equal(t, time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), got)
			is.Equal(t, time.UTC, got.Location())
		}
	})

	t.Run("rejects other formats", func(t *testing.T) {
		for _, s := range []string{"", "2024-01-02", "yesterday", "2024-01-02T03:04:05"} {
			_, err := parser.ParseTime(s)
			is.True(t, err != nil, s)
			is.True(t, !errors.Is(err, parser.ErrSyntax))
		}
	})
}

// names include keywords, mixed case, and characters that need quoting.
var names = []string{"a", "b", "c", "id", "name", "t1", "x_y", "select", "key", "cast", "Foo", "a b", "x\"y", "héllo", "1a", "_"}

func drawName(ht hegel.TestCase) string {
	return hegel.Draw(ht, hegel.SampledFrom(names))
}

func drawLiteral(ht hegel.TestCase) *ast.Literal {
	switch hegel.Draw(ht, hegel.Integers(0, 6)) {
	case 0:
		return &ast.Literal{Value: value.NewInt(hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64)))}
	case 1:
		return &ast.Literal{Value: value.NewFloat(hegel.Draw(ht, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false)))}
	case 2:
		return &ast.Literal{Value: value.NewBool(hegel.Draw(ht, hegel.Booleans()))}
	case 3:
		return &ast.Literal{Value: value.NewString(hegel.Draw(ht, hegel.Text().MaxSize(8)))}
	case 4:
		return &ast.Literal{Value: value.NewBytes(hegel.Draw(ht, hegel.Binary(0, 4)))}
	case 5:
		sec := hegel.Draw(ht, hegel.Integers[int64](-62135596800, 253402300799))
		nsec := hegel.Draw(ht, hegel.Integers[int64](0, 999999999))
		return &ast.Literal{Value: value.NewTime(time.Unix(sec, nsec))}
	}
	return &ast.Literal{Value: value.NewNull(value.Unknown)}
}

func drawExprs(ht hegel.TestCase, depth, minN, maxN int) []ast.Expr {
	n := hegel.Draw(ht, hegel.Integers(minN, maxN))
	if n == 0 {
		return nil
	}
	list := make([]ast.Expr, n)
	for i := range list {
		list[i] = drawExpr(ht, depth)
	}
	return list
}

var allTypes = []value.Type{value.Int, value.Float, value.Bool, value.String, value.Bytes, value.Time}

func drawExpr(ht hegel.TestCase, depth int) ast.Expr {
	kind := 0
	if depth < 4 {
		kind = hegel.Draw(ht, hegel.Integers(0, 12))
	} else {
		kind = hegel.Draw(ht, hegel.Integers(0, 2))
	}
	switch kind {
	case 0:
		return drawLiteral(ht)
	case 1:
		return &ast.ColumnRef{Name: drawName(ht)}
	case 2:
		return &ast.Param{Index: hegel.Draw(ht, hegel.Integers(1, 5))}
	case 3:
		return &ast.Unary{Op: ast.UnaryOp(hegel.Draw(ht, hegel.Integers(0, 1))), Expr: drawExpr(ht, depth+1)}
	case 4, 5, 6:
		return &ast.Binary{Op: ast.BinaryOp(hegel.Draw(ht, hegel.Integers(int(ast.Add), int(ast.Or)))), Left: drawExpr(ht, depth+1), Right: drawExpr(ht, depth+1)}
	case 7:
		return &ast.IsNull{Expr: drawExpr(ht, depth+1), Not: hegel.Draw(ht, hegel.Booleans())}
	case 8:
		return &ast.Between{Expr: drawExpr(ht, depth+1), Low: drawExpr(ht, depth+1), High: drawExpr(ht, depth+1), Not: hegel.Draw(ht, hegel.Booleans())}
	case 9:
		return &ast.In{Expr: drawExpr(ht, depth+1), List: drawExprs(ht, depth+1, 1, 3), Not: hegel.Draw(ht, hegel.Booleans())}
	case 10:
		return &ast.Like{Expr: drawExpr(ht, depth+1), Pattern: drawExpr(ht, depth+1), Not: hegel.Draw(ht, hegel.Booleans())}
	case 11:
		c := &ast.Case{}
		if hegel.Draw(ht, hegel.Booleans()) {
			c.Operand = drawExpr(ht, depth+1)
		}
		for range hegel.Draw(ht, hegel.Integers(1, 2)) {
			c.Whens = append(c.Whens, ast.When{Cond: drawExpr(ht, depth+1), Result: drawExpr(ht, depth+1)})
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			c.Else = drawExpr(ht, depth+1)
		}
		return c
	default:
		if hegel.Draw(ht, hegel.Booleans()) {
			return &ast.Cast{Expr: drawExpr(ht, depth+1), Type: hegel.Draw(ht, hegel.SampledFrom(allTypes))}
		}
		name := hegel.Draw(ht, hegel.SampledFrom([]string{"length", "lower", "upper", "abs", "coalesce"}))
		return &ast.Call{Name: name, Args: drawExprs(ht, depth+1, 0, 3)}
	}
}

func drawOptionalExpr(ht hegel.TestCase) ast.Expr {
	if hegel.Draw(ht, hegel.Booleans()) {
		return drawExpr(ht, 1)
	}
	return nil
}

func drawNames(ht hegel.TestCase, minN, maxN int) []string {
	n := hegel.Draw(ht, hegel.Integers(minN, maxN))
	out := make([]string, n)
	for i := range out {
		out[i] = drawName(ht)
	}
	return out
}

func drawStatement(ht hegel.TestCase) ast.Statement {
	switch hegel.Draw(ht, hegel.Integers(0, 8)) {
	case 0:
		s := &ast.CreateTable{Name: drawName(ht), IfNotExists: hegel.Draw(ht, hegel.Booleans())}
		for range hegel.Draw(ht, hegel.Integers(1, 4)) {
			s.Columns = append(s.Columns, ast.ColumnDef{Name: drawName(ht), Type: hegel.Draw(ht, hegel.SampledFrom(allTypes)), NotNull: hegel.Draw(ht, hegel.Booleans())})
		}
		s.PrimaryKey = drawNames(ht, 1, 2)
		return s
	case 1:
		return &ast.DropTable{Name: drawName(ht), IfExists: hegel.Draw(ht, hegel.Booleans())}
	case 2:
		s := &ast.Insert{Table: drawName(ht)}
		if hegel.Draw(ht, hegel.Booleans()) {
			s.Columns = drawNames(ht, 1, 3)
		}
		for range hegel.Draw(ht, hegel.Integers(1, 3)) {
			s.Rows = append(s.Rows, drawExprs(ht, 1, 1, 3))
		}
		return s
	case 3:
		s := &ast.Update{Table: drawName(ht), Where: drawOptionalExpr(ht)}
		for range hegel.Draw(ht, hegel.Integers(1, 3)) {
			s.Set = append(s.Set, ast.Assignment{Column: drawName(ht), Value: drawExpr(ht, 1)})
		}
		return s
	case 4:
		return &ast.Delete{Table: drawName(ht), Where: drawOptionalExpr(ht)}
	case 5, 6:
		s := &ast.Select{Where: drawOptionalExpr(ht)}
		if hegel.Draw(ht, hegel.Booleans()) {
			s.From = drawName(ht)
		}
		for range hegel.Draw(ht, hegel.Integers(1, 3)) {
			if hegel.Draw(ht, hegel.Integers(0, 3)) == 0 {
				s.Items = append(s.Items, ast.SelectItem{Star: true})
				continue
			}
			item := ast.SelectItem{Expr: drawExpr(ht, 1)}
			if hegel.Draw(ht, hegel.Booleans()) {
				item.Alias = drawName(ht)
			}
			s.Items = append(s.Items, item)
		}
		for range hegel.Draw(ht, hegel.Integers(0, 2)) {
			s.OrderBy = append(s.OrderBy, ast.OrderItem{Expr: drawExpr(ht, 1), Desc: hegel.Draw(ht, hegel.Booleans()), Nulls: ast.NullsOrder(hegel.Draw(ht, hegel.Integers(0, 2)))})
		}
		s.Limit = drawOptionalExpr(ht)
		s.Offset = drawOptionalExpr(ht)
		return s
	case 7:
		return &ast.Begin{ReadOnly: hegel.Draw(ht, hegel.Booleans())}
	default:
		if hegel.Draw(ht, hegel.Booleans()) {
			return &ast.Commit{}
		}
		return &ast.Rollback{}
	}
}

func ExampleParse() {
	s, err := parser.Parse("select id, name from users where id = $1 order by name")
	if err != nil {
		panic(err)
	}
	fmt.Println(ast.Render(s, ast.Maragudb))
	fmt.Println(ast.Render(s, ast.Postgres))
	// Output:
	// SELECT id, name FROM users WHERE id = $1 ORDER BY name
	// SELECT id, name FROM users WHERE id = $1 ORDER BY name
}
