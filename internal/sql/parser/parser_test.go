package parser_test

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
	"maragu.dev/maragudb/internal/sql/value"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestParse(t *testing.T) {
	t.Run("parses statements and renders them back canonically", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"create table t (id bigint primary key, name text not null, f double precision null)", "CREATE TABLE t (id bigint NOT NULL, name text NOT NULL, f double precision, PRIMARY KEY (id))"},
			{"CREATE TABLE IF NOT EXISTS t (a int8, b bytea, c bool, d timestamptz, e float8, f boolean, g timestamp with time zone, h float, PRIMARY KEY (a, b))", "CREATE TABLE IF NOT EXISTS t (a bigint, b bytea, c boolean, d timestamptz, e double precision, f boolean, g timestamptz, h double precision, PRIMARY KEY (a, b))"},
			{"create table t (id bigint constraint pk primary key, constraint c2 primary key (id))", ""},
			{"create table t (id bigint, constraint pk primary key (id))", "CREATE TABLE t (id bigint, CONSTRAINT pk PRIMARY KEY (id))"},
			{"create table t (id bigint constraint pk primary key, a text constraint u unique nulls not distinct, b bytea unique nulls distinct unique, unique (a, b), constraint ab unique nulls not distinct (b, a))", "CREATE TABLE t (id bigint NOT NULL, a text, b bytea, CONSTRAINT pk PRIMARY KEY (id), CONSTRAINT u UNIQUE NULLS NOT DISTINCT (a), UNIQUE (b), UNIQUE (b), UNIQUE (a, b), CONSTRAINT ab UNIQUE NULLS NOT DISTINCT (b, a))"},
			{"create table t (id bigint unique primary key not null)", "CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY (id), UNIQUE (id))"},
			{"create table t (id bigint primary key default 1 + 2 * 3, a text default 'x' || 'y' not null, b boolean default 1 < 2, c boolean default (1 is null) check (c), d timestamptz default current_timestamp, e boolean default case when 1 is null then true end, f bigint default -abs(-1)::bigint)", "CREATE TABLE t (id bigint NOT NULL DEFAULT 1 + 2 * 3, a text NOT NULL DEFAULT 'x' || 'y', b boolean DEFAULT 1 < 2, c boolean DEFAULT (1 IS NULL), d timestamptz DEFAULT CURRENT_TIMESTAMP, e boolean DEFAULT CASE WHEN 1 IS NULL THEN TRUE END, f bigint DEFAULT -abs(-1)::bigint, PRIMARY KEY (id), CHECK (c))"},
			{"create table t (id bigint primary key constraint d default 1 check (id > 0) enforced, check (id < 9), constraint c check (id <> 5) enforced)", "CREATE TABLE t (id bigint NOT NULL DEFAULT 1, PRIMARY KEY (id), CHECK (id > 0), CHECK (id < 9), CONSTRAINT c CHECK (id <> 5))"},
			{"create table t (id bigint primary key references p, a bigint constraint f references p (x) match simple on update restrict on delete set null, b bigint, foreign key (a, b) references q (x, y) on delete set default (b) on update no action, constraint g foreign key (b) references t on delete cascade)", "CREATE TABLE t (id bigint NOT NULL, a bigint, b bigint, PRIMARY KEY (id), FOREIGN KEY (id) REFERENCES p, CONSTRAINT f FOREIGN KEY (a) REFERENCES p (x) ON DELETE SET NULL ON UPDATE RESTRICT, FOREIGN KEY (a, b) REFERENCES q (x, y) ON DELETE SET DEFAULT (b), CONSTRAINT g FOREIGN KEY (b) REFERENCES t ON DELETE CASCADE)"},
			{"create table t (id bigint primary key, a bigint references p on delete no action on update no action, b bigint references p on delete restrict, c bigint references p on delete set null (c, c))", "CREATE TABLE t (id bigint NOT NULL, a bigint, b bigint, c bigint, PRIMARY KEY (id), FOREIGN KEY (a) REFERENCES p, FOREIGN KEY (b) REFERENCES p ON DELETE RESTRICT, FOREIGN KEY (c) REFERENCES p ON DELETE SET NULL (c, c))"},
			{"insert into t default values", "INSERT INTO t DEFAULT VALUES"},
			{"insert into t (a, b) values (default, 1), (2, DEFAULT)", "INSERT INTO t (a, b) VALUES (DEFAULT, 1), (2, DEFAULT)"},
			{"update t set a = default, b = 1", "UPDATE t SET a = DEFAULT, b = 1"},
			{"select now(), current_timestamp, transaction_timestamp(), \"now\"()", "SELECT now(), CURRENT_TIMESTAMP, transaction_timestamp(), now()"},
			{"create index on t (a)", "CREATE INDEX ON t (a)"},
			{"create unique index if not exists i on t (a desc, b asc nulls first, c nulls last, d desc nulls last) nulls not distinct", "CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a DESC, b NULLS FIRST, c NULLS LAST, d DESC NULLS LAST) NULLS NOT DISTINCT"},
			{"create index if on t (a) nulls distinct", "CREATE INDEX if ON t (a)"},
			{`create index "Index" on "T" ("A")`, `CREATE INDEX "Index" ON "T" ("A")`},
			{"drop index i", "DROP INDEX i"},
			{"drop index if exists i restrict", "DROP INDEX IF EXISTS i"},
			{"create table t (key bigint primary key, if bigint, exists bigint, set bigint, begin bigint, nulls bigint, first bigint, read bigint, by bigint, bigint bigint, time bigint, double bigint, delete bigint)", "CREATE TABLE t (key bigint NOT NULL, if bigint, exists bigint, set bigint, begin bigint, nulls bigint, first bigint, read bigint, by bigint, bigint bigint, time bigint, double bigint, delete bigint, PRIMARY KEY (key))"},
			{"drop table t", "DROP TABLE t"},
			{"drop table if exists T restrict", "DROP TABLE IF EXISTS t"},
			{"insert into t values (1, 'a'), (2, 'b')", "INSERT INTO t VALUES (1, 'a'), (2, 'b')"},
			{"insert into t (a, b) values ($1, $2)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
			{"update t set a = 1, b = b + 1 where id = 3", "UPDATE t SET a = 1, b = b + 1 WHERE id = 3"},
			{"update t set set = 1, key = 2", "UPDATE t SET set = 1, key = 2"},
			{"delete from t where a is null", "DELETE FROM t WHERE a IS NULL"},
			{"delete from t", "DELETE FROM t"},
			{"select * from t", "SELECT * FROM t"},
			{"select all a from t", "SELECT a FROM t"},
			{"select a, b as c, d e from t where a > 1 order by b desc nulls first, c asc nulls last, d limit 10 offset 5", "SELECT a, b AS c, d AS e FROM t WHERE a > 1 ORDER BY b DESC NULLS FIRST, c NULLS LAST, d LIMIT 10 OFFSET 5"},
			{"select a from t offset 5 limit 10", "SELECT a FROM t LIMIT 10 OFFSET 5"},
			{"select a from t limit all", "SELECT a FROM t LIMIT NULL"},
			{"select a as select, b as from, c key, d as end from t", `SELECT a AS "select", b AS "from", c AS key, d AS "end" FROM t`},
			{"select 1 + 1", "SELECT 1 + 1"},
			{"select distinct t.a, x.\"B\", x.order, u.* from t, u as x where t.a = x.b", `SELECT DISTINCT t.a, x."B", x."order", u.* FROM t, u AS x WHERE t.a = x.b`},
			{"select * from t x (a, b) join u using (a) left outer join v y on y.a = x.a cross join w natural join z natural left join q", "SELECT * FROM t AS x (a, b) JOIN u USING (a) LEFT JOIN v AS y ON y.a = x.a CROSS JOIN w NATURAL JOIN z NATURAL LEFT JOIN q"},
			{"select * from a join b join c on x on y", "SELECT * FROM a JOIN (b JOIN c ON x) ON y"},
			{"select * from a cross join b join c on x", "SELECT * FROM a CROSS JOIN b JOIN c ON x"},
			{"select * from a inner join (b cross join c) on x, ((d join e on true))", "SELECT * FROM a JOIN (b CROSS JOIN c) ON x, d JOIN e ON TRUE"},
			{"update t as x set a = x.b, x.c = 1 where x.a = 1", "UPDATE t AS x SET a = x.b, x.c = 1 WHERE x.a = 1"},
			{"update t x set a = 1", "UPDATE t AS x SET a = 1"},
			{"delete from t x where x.a = 1", "DELETE FROM t AS x WHERE x.a = 1"},
			{"select 1;", "SELECT 1"},
			{"begin", "BEGIN"},
			{"begin work", "BEGIN"},
			{"begin transaction read only", "BEGIN READ ONLY"},
			{"begin isolation level serializable, read only", "BEGIN READ ONLY"},
			{"begin isolation level serializable read write not deferrable", "BEGIN"},
			{"start transaction read write", "BEGIN"},
			{"commit", "COMMIT"},
			{"commit work", "COMMIT"},
			{"end transaction", "COMMIT"},
			{"rollback", "ROLLBACK"},
			{"abort", "ROLLBACK"},
			{"abort transaction", "ROLLBACK"},
			{"select a from t -- comment\n where b = 1 /* block /* nested */ still */", "SELECT a FROM t WHERE b = 1"},
			{`select "Quoted", "select", "a b", "x""y" from "key"`, `SELECT "Quoted", "select", "a b", "x""y" FROM key`},
			{"select 0x10, 0o17, 0b101, 1_000, 1_000.5, 0X1F", "SELECT 16, 15, 5, 1000, 1000.5, 31"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				s, err := parser.Parse(test.sql)
				if test.want == "" {
					is.Error(t, parser.ErrInvalid, err)
					return
				}
				is.NotError(t, err)
				is.Equal(t, test.want, ast.Render(s))
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
			{"-(5)", "-5"},
			{"-(-5)", "5"},
			{"-(x)", "-x"},
			{"2 - -5", "2 - -5"},
			{"2--5", "2"},
			{"- -a", "-(-a)"},
			{"+a", "+a"},
			{"+5", "+(5)"},
			{"a || b || c", "a || b || c"},
			{"a || (b || c)", "a || (b || c)"},
			{"a + b || c + d", "a + b || c + d"},
			{"a = b", "a = b"},
			{"a != b", "a <> b"},
			{"a<>-1", "a <> -1"},
			{"a*-1", "a * -1"},
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
			{"a BETWEEN ASYMMETRIC 1 + 1 AND 2 || 'x'", "a BETWEEN 1 + 1 AND 2 || 'x'"},
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
			{"CAST(a AS bigint) + CAST('1.5' AS double precision)", "CAST(a AS bigint) + CAST('1.5' AS double precision)"},
			{"a::int8 + '1.5'::float8", "a::bigint + '1.5'::double precision"},
			{"-1::bigint", "-1::bigint"},
			{"-(1)::bigint", "-1::bigint"},
			{"(-1)::bigint", "(-1)::bigint"},
			{"-a::bigint * 2", "-a::bigint * 2"},
			{"(a + b)::text", "(a + b)::text"},
			{"a::text::bytea", "a::text::bytea"},
			{"timestamptz '2024-01-02 03:04:05+00'", "timestamptz '2024-01-02 03:04:05+00'"},
			{"timestamp with time zone '2024-01-02'", "timestamptz '2024-01-02'"},
			{"double precision '1.5' + bigint '1' + text 'x' || bool 't'", "double precision '1.5' + bigint '1' + text 'x' || boolean 't'"},
			{"coalesce(a, b, 1)", "coalesce(a, b, 1)"},
			{"LENGTH(Lower(x))", "length(lower(x))"},
			{"abs(-1)", "abs(-1)"},
			{"1.5 + 2e3 + .5 + 1E-2 + 1.", "1.5 + 2e3 + .5 + 1E-2 + 1."},
			{"'it''s'", "'it''s'"},
			{"TRUE AND FALSE OR NULL", "TRUE AND FALSE OR NULL"},
			{"$1 + $2", "$1 + $2"},
			{"time", "time"},
			{"-9223372036854775808", "-9223372036854775808"},
			{"9223372036854775808", "9223372036854775808"},
			{"-2147483648", "-2147483648"},
			{"((a))", "a"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				e, err := parser.ParseExpr(test.sql)
				is.NotError(t, err)
				is.Equal(t, test.want, ast.RenderExpr(e))
			})
		}
	})

	t.Run("types integer literals by magnitude, as Postgres does", func(t *testing.T) {
		for _, test := range []struct {
			sql  string
			want value.Value
		}{
			{"1", value.NewInt4(1)},
			{"2147483647", value.NewInt4(math.MaxInt32)},
			{"-2147483647", value.NewInt4(-math.MaxInt32)},
			{"-2147483648", value.NewInt4(math.MinInt32)},
			{"2147483648", value.NewInt(math.MaxInt32 + 1)},
			{"9223372036854775807", value.NewInt(math.MaxInt64)},
			{"-9223372036854775808", value.NewInt(math.MinInt64)},
			{"9223372036854775808", value.NewNumeric("9223372036854775808")},
			{"-9223372036854775809", value.NewNumeric("-9223372036854775809")},
			{"1.5", value.NewNumeric("1.5")},
			{"-1.5", value.NewNumeric("-1.5")},
			{"1e3", value.NewNumeric("1e3")},
			{"0x7fffffff", value.NewInt4(math.MaxInt32)},
			{"0x_1F", value.NewInt4(31)},
			{"0o_1_7", value.NewInt4(15)},
			{"0b_101", value.NewInt4(5)},
			{"0x80000000", value.NewInt(math.MaxInt32 + 1)},
			{"0xffffffffffffffffff", value.NewNumeric("4722366482869645213695")},
			{"-(5)", value.NewInt4(-5)},
			{"-(-5)", value.NewInt4(5)},
			{"-(-2147483648)", value.NewInt(math.MaxInt32 + 1)},
			{"-(-9223372036854775808)", value.NewNumeric("9223372036854775808")},
			{"-(1.5)", value.NewNumeric("-1.5")},
			{"-(-1.5)", value.NewNumeric("1.5")},
			{"'x'", value.NewUnknown("x")},
		} {
			e, err := parser.ParseExpr(test.sql)
			is.NotError(t, err, test.sql)
			lit, ok := e.(*ast.Literal)
			is.True(t, ok, test.sql)
			is.True(t, reflect.DeepEqual(test.want, lit.Value), test.sql)
		}
		e, err := parser.ParseExpr("-1::bigint")
		is.NotError(t, err)
		u, ok := e.(*ast.Unary)
		is.True(t, ok && u.Op == ast.Neg, "the sign applies to the cast")
		_, ok = u.Expr.(*ast.Cast)
		is.True(t, ok)
	})

	t.Run("rejects invalid statements with a position, as Postgres does", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"", "syntax error at position 1: expected a statement, got end of statement"},
			{"selec 1", "syntax error at position 1: expected a statement, got \"selec\""},
			{"select 1 2", "syntax error at position 10: unexpected \"2\""},
			{"select 'abc", "syntax error at position 8: unterminated string"},
			{"select 1e", "syntax error at position 8: trailing junk after numeric literal"},
			{"select 1abc", "syntax error at position 8: trailing junk after numeric literal"},
			{"select * from a join b", "syntax error at position 23: expected ON or USING, got end of statement"},
			{"select * from a cross join b on true", "syntax error at position 30: unexpected ON"},
			{"select * from a natural join b using (c)", "syntax error at position 32: unexpected USING"},
			{"select * from (a)", "syntax error at position 17: unexpected \")\""},
			{"select a.*.b from a", "syntax error at position 11: improper use of \"*\""},
			{"select 0x", "syntax error at position 8: invalid number"},
			{"select $", "syntax error at position 8: parameter needs a number"},
			{"select $0", "syntax error at position 8: there is no parameter $0"},
			{"select a from t where", "syntax error at position 22: expected an expression, got end of statement"},
			{"select case end", "syntax error at position 13: expected an expression, got END"},
			{"select case 1 end", "syntax error at position 15: expected WHEN, got END"},
			{"select a = b = c", "syntax error at position 14: syntax error at or near \"=\""},
			{"select a < b >= c", "syntax error at position 14: syntax error at or near \">=\""},
			{"select a not 1", "syntax error at position 10: unexpected NOT"},
			{"select a from t order by b nulls sometimes", "syntax error at position 34: expected FIRST or LAST, got \"sometimes\""},
			{"begin read sometimes", "syntax error at position 12: expected ONLY or WRITE, got \"sometimes\""},
			{"select /* unterminated", "syntax error at position 8: unterminated comment"},
			{"select \"\"", "syntax error at position 8: zero-length delimited identifier"},
			{"select \"a\x00b\"", "syntax error at position 8: quoted identifier must be valid UTF-8 without NUL bytes"},
			{"select \"\xff\"", "syntax error at position 8: quoted identifier must be valid UTF-8 without NUL bytes"},
			{"select 'a' from select", "syntax error at position 17: expected identifier, got SELECT"},
			{"create table t (only bigint primary key)", "syntax error at position 17: expected identifier, got ONLY"},
			{"create table t (limit bigint primary key)", "syntax error at position 17: expected identifier, got LIMIT"},
			{"create table t (is bigint primary key)", "syntax error at position 17: expected identifier, got IS"},
			{"select a from t limit 1 limit 2", "syntax error at position 31: multiple LIMIT clauses not allowed"},
			{"select 1 2 3", "syntax error at position 10: unexpected \"2\""},
			{"select a:b", "syntax error at position 9: unexpected character ':'"},
			{"begin read only,", "syntax error at position 17: expected a transaction mode, got end of statement"},
			{"start", "syntax error at position 6: expected TRANSACTION, got end of statement"},
			{"start work", "syntax error at position 7: expected TRANSACTION, got \"work\""},
			{"select 1_", "syntax error at position 8: trailing junk after numeric literal"},
			{"create index if not exists on t (a)", "syntax error at position 28: expected identifier, got ON"},
			{"create index i on t (a nulls sometimes)", "syntax error at position 30: expected FIRST or LAST, got \"sometimes\""},
			{"create index i on t ()", "syntax error at position 22: expected identifier, got \")\""},
			{"create index i on t (a) nulls distinct x", "syntax error at position 40: unexpected \"x\""},
			{"create table t (a bigint primary key, constraint c bigint)", "syntax error at position 52: expected a constraint after CONSTRAINT c, got \"bigint\""},
			{"create table t (a bigint primary key, unique a)", "syntax error at position 46: expected \"(\", got \"a\""},
			{`select "coalesce"(1)`, `invalid statement at position 19: function "coalesce" does not exist`},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				_, err := parser.Parse(test.sql)
				if strings.HasPrefix(test.want, "invalid") {
					is.Error(t, parser.ErrInvalid, err)
				} else {
					is.Error(t, parser.ErrSyntax, err)
				}
				is.Equal(t, test.want, err.Error())
			})
		}
	})

	t.Run("reports Postgres syntax it does not support as unsupported, with a hint", func(t *testing.T) {
		tests := []struct {
			sql  string
			want string
		}{
			{"select a from t right join u on a = b", "RIGHT JOIN is not supported, write a LEFT JOIN with the tables the other way round"},
			{"select a from t full outer join u on a = b", "FULL JOIN is not supported, write a LEFT JOIN with the tables the other way round"},
			{"select a from t join lateral u on true", "LATERAL is not supported"},
			{"select a from (t join u on true) as j", "an alias for a join is not supported, name the columns through the aliases of its tables"},
			{"select a from t join u using (a) as j", "an alias for the columns of USING is not supported"},
			{"select a from (select 1) x", "subqueries in FROM are not supported"},
			{"select a from generate_series(1, 2)", "functions in FROM are not supported"},
			{"select a from t tablesample bernoulli (1)", "TABLESAMPLE is not supported"},
			{"select a.b.c from t", "names of more than two parts, such as schema.table.column, are not supported"},
			{"select t.* is null from t", "t.* is not supported in an expression, name the columns"},
			{"select t.*::text from t", "t.* is not supported in an expression, name the columns"},
			{"select x.f(1) from t", "functions with a schema are not supported"},
			{"select distinct on (a) a from t", "DISTINCT ON is not supported, write DISTINCT"},
			{"select a from t group by a", "GROUP BY is not supported"},
			{"select a from t union select b from u", "UNION is not supported"},
			{"select from t", "a SELECT without output columns is not supported"},
			{"select", "a SELECT without output columns is not supported"},
			{"select count(*) from t", "function count is not supported"},
			{"select \"count\"(*) from t", "function count is not supported"},
			{`select "cast"(1)`, "function cast is not supported"},
			{"select int8pl(1, 2)", "function int8pl is not supported"},
			{"select cast(a as nope)", "type nope is not supported; the supported types are bigint, double precision, boolean, text, bytea, and timestamptz"},
			{"select cast(a as int4range)", "type int4range is not supported; the supported types are bigint, double precision, boolean, text, bytea, and timestamptz"},
			{"select @", "operator @ is not supported"},
			{"select sum(a) from t", "function sum is not supported"},
			{"select statement_timestamp()", "function statement_timestamp is not supported, write now(), which is the start of the transaction"},
			{"select clock_timestamp()", "function clock_timestamp is not supported, write now(), which is the start of the transaction"},
			{"select current_timestamp(3)", "CURRENT_TIMESTAMP with a precision is not supported, write CURRENT_TIMESTAMP"},
			{"select localtimestamp", "LOCALTIMESTAMP is not supported, because its type is not; write CURRENT_TIMESTAMP or now()"},
			{"select current_date", "CURRENT_DATE is not supported, because its type is not; write CURRENT_TIMESTAMP or now()"},
			{"select nextval('s')", "function nextval is not supported, because there are no sequences; generate the keys in the application"},
			{"select current_user", "CURRENT_USER is not supported, pass the value as a parameter"},
			{"select (select 1)", "subqueries are not supported"},
			{"select exists (select 1)", "subqueries are not supported"},
			{"select a from t where a in (select b from u)", "subqueries are not supported"},
			{"select a from t where a = any (b)", "ANY is not supported, write IN or several comparisons"},
			{"select array[1]", "arrays are not supported"},
			{"select a[1] from t", "arrays are not supported"},
			{"select a from t where a is true", "IS TRUE is not supported, compare with = or write IS NOT NULL AND"},
			{"select a from t where a is distinct from b", "IS DISTINCT FROM is not supported, write the NULL cases out"},
			{"select a from t where a isnull", "ISNULL is not supported, write IS NULL or IS NOT NULL"},
			{"select a from t where a ilike 'x'", "ILIKE is not supported, write lower(x) LIKE lower(pattern)"},
			{"select a from t where a similar to 'x'", "SIMILAR TO is not supported"},
			{"select a from t where a like 'x' escape '!'", "LIKE with ESCAPE is not supported, escape with a backslash"},
			{"select a from t where a ~ 'x'", "operator ~ is not supported"},
			{"select a ^ 2 from t", "operator ^ is not supported"},
			{"select a & 2 from t", "operator & is not supported"},
			{"select a between symmetric 1 and 2 from t", "BETWEEN SYMMETRIC is not supported, write the bounds in order"},
			{"select a at time zone 'UTC' from t", "AT TIME ZONE is not supported, every timestamptz is read and written in UTC"},
			{"select a collate \"C\" from t", "COLLATE is not supported, strings compare by byte order"},
			{"select e'x'", "escape string constants E'...' are not supported, write the string with standard quotes"},
			{"select x'01'", "bit string constants are not supported, write bytes as a bytea such as '\\x01'::bytea"},
			{"select b'01'", "bit string constants are not supported, write bytes as a bytea such as '\\x01'::bytea"},
			{"select $$x$$", "dollar-quoted string constants are not supported, write the string with standard quotes"},
			{"select $a$x$a$", "dollar-quoted string constants are not supported, write the string with standard quotes"},
			{"select u&'x'", "Unicode escape string constants are not supported, write the characters directly"},
			{"select 'a'\n'b'", "string constants continued on the next line are not supported, write one string"},
			{"select cast(1 as integer)", "integer is not supported, use bigint"},
			{"select 1::int", "int is not supported, use bigint"},
			{"select cast(1 as numeric)", "numeric is not supported, use double precision or bigint"},
			{"select cast('x' as varchar)", "varchar is not supported, use text"},
			{"select cast('x' as timestamp)", "timestamp without time zone is not supported, use timestamptz"},
			{"select cast('x' as timestamp without time zone)", "timestamp without time zone is not supported, use timestamptz"},
			{"select cast('x' as time)", "the time type is not supported, use timestamptz"},
			{"select time '12:00'", "the time type is not supported, use timestamptz"},
			{"select date '2024-01-01'", "date is not supported, use timestamptz"},
			{"select interval '1 day'", "interval is not supported"},
			{"select cast('x' as timestamptz(3))", "a type modifier is not supported on timestamptz"},
			{"create table t (a bigint primary key, b varchar(10))", "varchar is not supported, use text"},
			{"create table t (a serial primary key)", "serial is not supported, because there are no sequences; use bigint and generate the keys in the application"},
			{"create table t (a bigint generated always as identity primary key)", "GENERATED columns, identity columns among them, are not supported, because there are no sequences; generate the values in the application"},
			{"create sequence s", "CREATE SEQUENCE is not supported, only CREATE TABLE and CREATE INDEX"},
			{"create table t (a bigint constraint n not null primary key)", "a name for a NOT NULL constraint is not supported, leave out CONSTRAINT n"},
			{"create table t (a bigint)", "a table without a primary key is not supported, add PRIMARY KEY to a column or PRIMARY KEY (columns)"},
			{"create table t ()", "a table without columns is not supported"},
			{"create table t (a bigint primary key, b bigint check (b > 0) no inherit)", "NO INHERIT is not supported, there is no inheritance"},
			{"create table t (a bigint primary key, check (a > 0) not valid)", "NOT VALID is not supported, every constraint holds for every row"},
			{"create table t (a bigint primary key, check (a > 0) not enforced)", "NOT ENFORCED is not supported, every constraint is enforced"},
			{"create table t (a bigint primary key check (a > 0) not enforced)", "NOT ENFORCED is not supported, every constraint is enforced"},
			{"create table t (a bigint primary key, b bigint references u not enforced)", "NOT ENFORCED is not supported, every constraint is enforced"},
			{"create table t (a bigint primary key, b bigint references u deferrable)", "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once"},
			{"create table t (a bigint primary key, foreign key (a) references u not valid)", "NOT VALID is not supported, every constraint holds for every row"},
			{"create table t (a bigint primary key, b bigint references u match full)", "MATCH FULL is not supported, write MATCH SIMPLE or nothing, and NOT NULL on the columns that must be set together"},
			{"create table t (a bigint primary key, b bigint references u match partial)", "MATCH PARTIAL is not supported"},
			{"create table t (a bigint primary key, b bigint references u on update cascade)", "ON UPDATE CASCADE is not supported, only NO ACTION and RESTRICT; update the referencing rows in a statement of their own"},
			{"create table t (a bigint primary key, b bigint references u on update set null)", "ON UPDATE SET NULL is not supported, only NO ACTION and RESTRICT; update the referencing rows in a statement of their own"},
			{"create table t (a bigint primary key, b bigint references u on update set default)", "ON UPDATE SET DEFAULT is not supported, only NO ACTION and RESTRICT; update the referencing rows in a statement of their own"},
			{"create table t (a bigint primary key, unique (a) include (b))", "INCLUDE is not supported"},
			{"create table t (a bigint primary key include (b))", "INCLUDE is not supported"},
			{"create table t (a bigint primary key, unique (a) with (fillfactor = 70))", "WITH storage parameters are not supported"},
			{"create table t (a bigint primary key using index tablespace x)", "USING INDEX TABLESPACE is not supported"},
			{"create table t (a bigint primary key, unique (a) deferrable)", "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once"},
			{"create table t (a bigint primary key, b bigint unique not deferrable)", "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once"},
			{"create table t (a bigint primary key initially deferred)", "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once"},
			{"create table t (a bigint, primary key (a) deferrable initially deferred)", "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once"},
			{"create temp table t (a bigint primary key)", "TEMP tables are not supported"},
			{"create view v as select 1", "CREATE VIEW is not supported, only CREATE TABLE and CREATE INDEX"},
			{"create index concurrently i on t (a)", "CREATE INDEX CONCURRENTLY is not supported, every statement is atomic"},
			{"create index i on only t (a)", "ON ONLY is not supported, there is no inheritance"},
			{"create index i on t using btree (a)", "USING is not supported, every index is a btree"},
			{"create index i on t ((a + 1))", "expression indexes are not supported, index columns"},
			{"create index i on t (lower(a))", "expression indexes are not supported, index columns"},
			{"create index i on t (a collate \"C\")", "COLLATE is not supported, strings compare by byte order"},
			{"create index i on t (a text_pattern_ops)", "operator classes are not supported"},
			{"create index i on t (a) include (b)", "INCLUDE is not supported"},
			{"create index i on t (a) with (fillfactor = 70)", "WITH storage parameters are not supported"},
			{"create index i on t (a) tablespace x", "TABLESPACE is not supported"},
			{"create index i on t (a) where a > 0", "partial indexes are not supported, index the whole table"},
			{"drop index concurrently i", "DROP INDEX CONCURRENTLY is not supported, every statement is atomic"},
			{"drop index i, j", "dropping several tables or indexes in one statement is not supported, drop them one at a time"},
			{"drop index i cascade", "CASCADE is not supported, drop the tables one at a time"},
			{"alter index i rename to j", "ALTER is not supported"},
			{"create table t (a bigint primary key) inherits (u)", "INHERITS is not supported"},
			{"drop table t cascade", "CASCADE is not supported, drop the tables one at a time"},
			{"drop table t, u", "dropping several tables or indexes in one statement is not supported, drop them one at a time"},
			{"drop view v", "DROP VIEW is not supported, only DROP TABLE and DROP INDEX"},
			{"insert into t select * from u", "INSERT ... SELECT is not supported, write VALUES"},
			{"insert into t values (1) on conflict do nothing", "ON CONFLICT is not supported"},
			{"insert into t values (1) returning a", "RETURNING is not supported, read the rows with a SELECT"},
			{"update t set a = 1 from u", "UPDATE ... FROM is not supported, read the other tables in a SELECT first"},
			{"update t set a = 1 returning a", "RETURNING is not supported, read the rows with a SELECT"},
			{"update t set (a, b) = (1, 2)", "SET (columns) = (values) is not supported, assign the columns one at a time"},
			{"update t.u set a = 1", "a table name with a schema is not supported, name the table alone"},
			{"update t set a.b.c = 1", "assigning to a field of a field is not supported"},
			{"delete from t using u", "DELETE ... USING is not supported, read the other tables in a SELECT first"},
			{"delete from t x using u", "DELETE ... USING is not supported, read the other tables in a SELECT first"},
			{"delete from t where current of c", "WHERE CURRENT OF is not supported"},
			{"select a from t for update", "FOR UPDATE and FOR SHARE are not supported, every transaction is serializable"},
			{"select a from t order by a limit 1 offset 2 rows", "OFFSET n ROWS is not supported, write OFFSET n"},
			{"select a from t fetch first 1 rows only", "FETCH is not supported, write LIMIT"},
			{"select a from t order by a using <", "ORDER BY ... USING is not supported, write ASC or DESC"},
			{"with x as (select 1) select * from x", "WITH is not supported"},
			{"explain select 1", "EXPLAIN is not supported"},
			{"set time zone 'UTC'", "SET is not supported, maragudb has no session settings"},
			{"truncate t", "TRUNCATE is not supported, write DELETE FROM"},
			{"values (1)", "VALUES as a statement is not supported, write a SELECT"},
			{"begin isolation level read committed", "isolation level READ is not supported, every transaction is SERIALIZABLE"},
			{"begin deferrable", "DEFERRABLE transactions are not supported"},
			{"commit and chain", "AND CHAIN is not supported"},
			{"rollback to savepoint s", "ROLLBACK TO SAVEPOINT is not supported"},
			{"savepoint s", "SAVEPOINT is not supported"},
			{"select 1; select 2", "several statements in one string are not supported, run them one at a time"},
			{"select Ä from t", "identifiers with non-ASCII letters such as 'Ä' are not supported unquoted, quote the identifier"},
			{"select a from public.t", "a table name with a schema is not supported, name the table alone"},
			{"(select 1)", "a parenthesized statement is not supported"},
			{"select row(1, 2)", "row constructors are not supported"},
			{"select left('abc', 1)", "function left is not supported"},
			{"select 'a' -- c\n'b'", "string constants continued on the next line are not supported, write one string"},
			{"select 'a' \v\n 'b'", "string constants continued on the next line are not supported, write one string"},
			{"select (1, 2)", "row constructors are not supported"},
			{"select nullif(a, b) from t", "function nullif is not supported"},
			{"select lower(a) over () from t", "window functions are not supported"},
		}
		for _, test := range tests {
			t.Run(test.sql, func(t *testing.T) {
				_, err := parser.Parse(test.sql)
				is.Error(t, parser.ErrUnsupported, err)
				is.True(t, err != nil && strings.HasSuffix(err.Error(), test.want), fmt.Sprint(err))
			})
		}
	})

	t.Run("reports a table with two primary keys as invalid, not as a syntax error, as Postgres does", func(t *testing.T) {
		for _, sql := range []string{"create table t (a bigint primary key, b bigint primary key)", "create table t (a bigint primary key, primary key (a))"} {
			_, err := parser.Parse(sql)
			is.Error(t, parser.ErrInvalid, err)
			is.Equal(t, `invalid statement: multiple primary keys for table "t" are not allowed`, err.Error())
		}
	})

	t.Run("round trips every statement through the renderer", func(t *testing.T) {
		verify.Covers(t, "every generated statement AST renders to SQL that parses back to the same AST", "AC-4.1")
		hegel.Test(t, func(ht *hegel.T) {
			s := drawStatement(ht)
			sql := ast.Render(s)
			ht.Note(sql)
			got, err := parser.Parse(sql)
			if err != nil {
				ht.Fatalf("parse %q: %v", sql, err)
			}
			if !reflect.DeepEqual(s, got) {
				ht.Fatalf("round trip of %q gave %q", sql, ast.Render(got))
			}
		}, hegel.WithTestCases(3000))
	})

	t.Run("round trips every expression, so rendering adds exactly the parentheses the precedence needs", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			e := drawExpr(ht, 0)
			sql := ast.RenderExpr(e)
			ht.Note(sql)
			got, err := parser.ParseExpr(sql)
			if err != nil {
				ht.Fatalf("parse %q: %v", sql, err)
			}
			if !reflect.DeepEqual(e, got) {
				ht.Fatalf("round trip of %q gave %q", sql, ast.RenderExpr(got))
			}
		}, hegel.WithTestCases(5000))
	})

	t.Run("never panics on arbitrary input, and every error is syntax, invalid, or unsupported", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			sql := hegel.Draw(ht, hegel.OneOf(
				hegel.Text().MaxSize(40),
				hegel.Text().Alphabet("select from where a b 1 2 ( ) , + - * / || = < > not and or is null in between like case when then else end cast as bigint $ ' x \" ; -- /* */ \n :: . [ ] ~ ^ e ' 0x 1.5 time zone with ").MaxSize(60),
			))
			for _, err := range []error{first(parser.Parse(sql)), first(parser.ParseExpr(sql))} {
				if err != nil && !errors.Is(err, parser.ErrSyntax) && !errors.Is(err, parser.ErrInvalid) && !errors.Is(err, parser.ErrUnsupported) {
					ht.Fatalf("%q: unexpected error kind %v", sql, err)
				}
			}
		}, hegel.WithTestCases(3000))
	})
}

func first[T any](_ T, err error) error {
	return err
}

// names include keywords, mixed case, and characters that need quoting.
var names = []string{"a", "b", "c", "id", "name", "t1", "x_y", "select", "key", "cast", "Foo", "a b", "x\"y", "héllo", "1a", "_", "set", "time", "end", "if"}

func drawName(ht hegel.TestCase) string {
	return hegel.Draw(ht, hegel.SampledFrom(names))
}

// drawLiteral draws a literal as the parser produces it: an integer typed by its magnitude, a
// numeric text, a quoted string of unknown type, a boolean, or NULL.
func drawLiteral(ht hegel.TestCase) *ast.Literal {
	switch hegel.Draw(ht, hegel.Integers(0, 5)) {
	case 0:
		return &ast.Literal{Value: value.NewInt4(hegel.Draw(ht, hegel.Integers[int32](math.MinInt32+1, math.MaxInt32)))}
	case 1:
		i := hegel.Draw(ht, hegel.Integers[int64](math.MinInt64, math.MaxInt64))
		// Out of the integer range, so that the parser types it bigint.
		switch {
		case i >= 0 && i <= math.MaxInt32:
			i += 1 << 31
		case i < 0 && i >= math.MinInt32:
			i -= 1<<31 + 1
		}
		return &ast.Literal{Value: value.NewInt(i)}
	case 2:
		f := hegel.Draw(ht, hegel.Floats[float64]().AllowNaN(false).AllowInfinity(false))
		text := value.FormatFloat(math.Abs(f))
		if !strings.ContainsAny(text, ".e") {
			text += ".0"
		}
		if f < 0 {
			text = "-" + text
		}
		return &ast.Literal{Value: value.NewNumeric(text)}
	case 3:
		return &ast.Literal{Value: value.NewBool(hegel.Draw(ht, hegel.Booleans()))}
	case 4:
		return &ast.Literal{Value: value.NewUnknown(hegel.Draw(ht, hegel.Text().MaxSize(8)))}
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
		kind = hegel.Draw(ht, hegel.Integers(0, 13))
	} else {
		kind = hegel.Draw(ht, hegel.Integers(0, 2))
	}
	switch kind {
	case 0:
		return drawLiteral(ht)
	case 1:
		if hegel.Draw(ht, hegel.Integers(0, 3)) == 0 {
			return &ast.ColumnRef{Table: drawName(ht), Name: drawName(ht)}
		}
		return &ast.ColumnRef{Name: drawName(ht)}
	case 2:
		return &ast.Param{Index: hegel.Draw(ht, hegel.Integers(1, 5))}
	case 3:
		op := ast.UnaryOp(hegel.Draw(ht, hegel.Integers(0, 2)))
		x := drawExpr(ht, depth+1)
		if lit, ok := x.(*ast.Literal); ok && op == ast.Neg && !lit.Value.Null && (lit.Value.Type == value.Int4 || lit.Value.Type == value.Int || lit.Value.Type == value.Numeric) {
			// A minus before a number is part of the number, as the parser reads it.
			return x
		}
		return &ast.Unary{Op: op, Expr: x}
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
	case 12:
		typ := hegel.Draw(ht, hegel.SampledFrom(allTypes))
		switch hegel.Draw(ht, hegel.Integers(0, 2)) {
		case 0:
			return &ast.Cast{Expr: drawExpr(ht, depth+1), Type: typ}
		case 1:
			return &ast.Cast{Expr: drawExpr(ht, depth+1), Type: typ, Style: ast.CastColons}
		}
		return &ast.Cast{Expr: &ast.Literal{Value: value.NewUnknown(hegel.Draw(ht, hegel.Text().MaxSize(8)))}, Type: typ, Style: ast.CastPrefix}
	default:
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

// drawAlias is an alias of a table, or none.
func drawAlias(ht hegel.TestCase) string {
	if hegel.Draw(ht, hegel.Booleans()) {
		return ""
	}
	return drawName(ht)
}

// drawTableExpr is a table with an alias or without, or a join of any kind.
func drawTableExpr(ht hegel.TestCase, depth int) ast.TableExpr {
	if depth >= 2 || hegel.Draw(ht, hegel.Booleans()) {
		t := &ast.TableRef{Name: drawName(ht), Alias: drawAlias(ht)}
		if t.Alias != "" && hegel.Draw(ht, hegel.Integers(0, 3)) == 0 {
			t.Columns = drawNames(ht, 1, 2)
		}
		return t
	}
	j := &ast.Join{Kind: ast.JoinKind(hegel.Draw(ht, hegel.Integers(0, 2))), Left: drawTableExpr(ht, depth+1), Right: drawTableExpr(ht, depth+1)}
	if j.Kind != ast.JoinCross {
		switch hegel.Draw(ht, hegel.Integers(0, 2)) {
		case 0:
			j.Natural = true
		case 1:
			j.Using = drawNames(ht, 1, 2)
		default:
			j.On = drawExpr(ht, 1)
		}
	}
	return j
}

func drawStatement(ht hegel.TestCase) ast.Statement {
	switch hegel.Draw(ht, hegel.Integers(0, 8)) {
	case 0:
		s := &ast.CreateTable{Name: drawName(ht), IfNotExists: hegel.Draw(ht, hegel.Booleans())}
		for range hegel.Draw(ht, hegel.Integers(1, 4)) {
			s.Columns = append(s.Columns, ast.ColumnDef{Name: drawName(ht), Type: hegel.Draw(ht, hegel.SampledFrom(allTypes)), NotNull: hegel.Draw(ht, hegel.Booleans())})
		}
		s.PrimaryKey = drawNames(ht, 1, 2)
		if hegel.Draw(ht, hegel.Booleans()) {
			s.PrimaryKeyName = drawName(ht)
		}
		for range hegel.Draw(ht, hegel.Integers(0, 2)) {
			u := ast.UniqueConstraint{Columns: drawNames(ht, 1, 2), NullsNotDistinct: hegel.Draw(ht, hegel.Booleans())}
			if hegel.Draw(ht, hegel.Booleans()) {
				u.Name = drawName(ht)
			}
			s.Unique = append(s.Unique, u)
		}
		return s
	case 1:
		if hegel.Draw(ht, hegel.Booleans()) {
			return &ast.DropIndex{Name: drawName(ht), IfExists: hegel.Draw(ht, hegel.Booleans())}
		}
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
		s := &ast.Update{Table: drawName(ht), Alias: drawAlias(ht), Where: drawOptionalExpr(ht)}
		for range hegel.Draw(ht, hegel.Integers(1, 3)) {
			a := ast.Assignment{Column: drawName(ht), Value: drawExpr(ht, 1)}
			if hegel.Draw(ht, hegel.Integers(0, 5)) == 0 {
				a.Qualifier = drawName(ht)
			}
			s.Set = append(s.Set, a)
		}
		return s
	case 4:
		return &ast.Delete{Table: drawName(ht), Alias: drawAlias(ht), Where: drawOptionalExpr(ht)}
	case 5, 6:
		s := &ast.Select{Distinct: hegel.Draw(ht, hegel.Booleans()), Where: drawOptionalExpr(ht)}
		if hegel.Draw(ht, hegel.Booleans()) {
			for range hegel.Draw(ht, hegel.Integers(1, 3)) {
				s.From = append(s.From, drawTableExpr(ht, 0))
			}
		}
		for range hegel.Draw(ht, hegel.Integers(1, 3)) {
			switch hegel.Draw(ht, hegel.Integers(0, 5)) {
			case 0:
				s.Items = append(s.Items, ast.SelectItem{Star: true})
				continue
			case 1:
				s.Items = append(s.Items, ast.SelectItem{Star: true, Table: drawName(ht)})
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
		if hegel.Draw(ht, hegel.Booleans()) {
			s := &ast.CreateIndex{Table: drawName(ht), Unique: hegel.Draw(ht, hegel.Booleans()), NullsNotDistinct: hegel.Draw(ht, hegel.Booleans())}
			if hegel.Draw(ht, hegel.Booleans()) {
				s.Name = drawName(ht)
				s.IfNotExists = hegel.Draw(ht, hegel.Booleans())
			}
			for _, name := range drawNames(ht, 1, 3) {
				s.Columns = append(s.Columns, ast.IndexColumn{Name: name, Desc: hegel.Draw(ht, hegel.Booleans()), Nulls: ast.NullsOrder(hegel.Draw(ht, hegel.Integers(0, 2)))})
			}
			return s
		}
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
	fmt.Println(ast.Render(s))
	// Output:
	// SELECT id, name FROM users WHERE id = $1 ORDER BY name
}
