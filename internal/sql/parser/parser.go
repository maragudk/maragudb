// Package parser parses the Postgres subset into an [ast.Statement]. Syntax that Postgres
// accepts and maragudb does not is reported as [ErrUnsupported], with a hint of what to write
// instead; syntax that Postgres rejects too is [ErrSyntax].
package parser

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/value"
)

var (
	// ErrSyntax is wrapped by every parse error, except those of [ErrInvalid] and [ErrUnsupported].
	ErrSyntax = errors.New("syntax error")
	// ErrInvalid is wrapped by the errors of statements that parse but cannot run, such as a
	// table with two primary keys.
	ErrInvalid = errors.New("invalid statement")
	// ErrUnsupported is wrapped by the errors of statements that Postgres accepts and maragudb
	// rejects. The message says what is unsupported and, where possible, what to write instead.
	ErrUnsupported = errors.New("unsupported")
)

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
	if p.isOp(";") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind != tokEOF {
			return nil, p.unsupportedf("several statements in one string are not supported, run them one at a time")
		}
	}
	return s, p.end()
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
	return e, p.end()
}

type parser struct {
	lex lexer
	tok token
	// restricted is set while parsing the expression of a DEFAULT, which is Postgres's b_expr:
	// outside of parentheses, function calls, CASE, and CAST, it has no boolean operators, IS,
	// LIKE, BETWEEN, or IN.
	restricted bool
}

// unrestricted lifts the restriction of a DEFAULT expression inside a construct with delimiters
// of its own, and returns the function that restores it.
func (p *parser) unrestricted() func() {
	restricted := p.restricted
	p.restricted = false
	return func() { p.restricted = restricted }
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("%w at position %d: %s", ErrSyntax, p.tok.pos+1, fmt.Sprintf(format, args...))
}

func (p *parser) unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w at position %d: %s", ErrUnsupported, p.tok.pos+1, fmt.Sprintf(format, args...))
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

// peek at the token after the current one, without consuming anything.
func (p *parser) peek() token {
	saved := p.lex
	t, err := p.lex.next()
	p.lex = saved
	if err != nil {
		return token{kind: tokEOF}
	}
	return t
}

// peek2 at the second token after the current one, without consuming anything.
func (p *parser) peek2() token {
	saved := p.lex
	defer func() { p.lex = saved }()
	if _, err := p.lex.next(); err != nil {
		return token{kind: tokEOF}
	}
	t, err := p.lex.next()
	if err != nil {
		return token{kind: tokEOF}
	}
	return t
}

// peekIsWord reports whether the next token is one of the given keywords or unquoted words.
func (p *parser) peekIsWord(words ...string) bool {
	next := p.peek()
	if next.kind != tokKeyword && (next.kind != tokIdent || next.quoted) {
		return false
	}
	return slices.Contains(words, next.text)
}

func (p *parser) isKeyword(kw string) bool {
	return p.tok.kind == tokKeyword && p.tok.text == kw
}

// isWord reports whether the current token is the keyword or unquoted identifier w, for words
// that are keywords only in context.
func (p *parser) isWord(w string) bool {
	return (p.tok.kind == tokKeyword || (p.tok.kind == tokIdent && !p.tok.quoted)) && p.tok.text == w
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

// end checks that nothing follows the statement.
func (p *parser) end() error {
	if p.tok.kind == tokEOF {
		return nil
	}
	if msg, ok := unsupportedClauses[p.tok.text]; ok && (p.tok.kind == tokKeyword || (p.tok.kind == tokIdent && !p.tok.quoted)) {
		return p.unsupportedf("%s", msg)
	}
	return p.errorf("unexpected %v", p.tok)
}

// unsupportedClauses that Postgres accepts after the parts of a statement that maragudb parses.
var unsupportedClauses = map[string]string{
	"union":      "UNION is not supported",
	"intersect":  "INTERSECT is not supported",
	"except":     "EXCEPT is not supported",
	"group":      "GROUP BY is not supported",
	"having":     "HAVING is not supported",
	"window":     "WINDOW is not supported",
	"fetch":      "FETCH is not supported, write LIMIT",
	"for":        "FOR UPDATE and FOR SHARE are not supported, every transaction is serializable",
	"returning":  "RETURNING is not supported, read the rows with a SELECT",
	"on":         "ON CONFLICT is not supported",
	"from":       "FROM in UPDATE is not supported",
	"join":       "JOIN is not supported",
	"inner":      "JOIN is not supported",
	"left":       "JOIN is not supported",
	"right":      "JOIN is not supported",
	"full":       "JOIN is not supported",
	"cross":      "JOIN is not supported",
	"natural":    "JOIN is not supported",
	"into":       "SELECT INTO is not supported",
	"with":       "WITH is not supported",
	"inherits":   "INHERITS is not supported",
	"partition":  "PARTITION BY is not supported",
	"tablespace": "TABLESPACE is not supported",
	"cascade":    "CASCADE is not supported, drop the tables one at a time",
	"and":        "AND CHAIN is not supported",
	"to":         "ROLLBACK TO SAVEPOINT is not supported",
	"rows":       "OFFSET n ROWS is not supported, write OFFSET n",
	"row":        "OFFSET n ROW is not supported, write OFFSET n",
	"escape":     "LIKE with ESCAPE is not supported, escape with a backslash",
	"at":         "AT TIME ZONE is not supported, every timestamptz is read and written in UTC",
	"collate":    "COLLATE is not supported, strings compare by byte order",
	"over":       "window functions are not supported",
	"filter":     "aggregate FILTER is not supported",
	"within":     "WITHIN GROUP is not supported",
}

// unsupportedStatements that Postgres accepts and maragudb does not.
var unsupportedStatements = map[string]string{
	"with": "WITH is not supported", "explain": "EXPLAIN is not supported", "set": "SET is not supported, maragudb has no session settings",
	"show": "SHOW is not supported", "alter": "ALTER is not supported", "truncate": "TRUNCATE is not supported, write DELETE FROM",
	"values": "VALUES as a statement is not supported, write a SELECT", "table": "TABLE is not supported, write SELECT * FROM",
	"grant": "GRANT is not supported", "revoke": "REVOKE is not supported", "copy": "COPY is not supported", "vacuum": "VACUUM is not supported",
	"analyze": "ANALYZE is not supported", "analyse": "ANALYZE is not supported", "savepoint": "SAVEPOINT is not supported", "release": "RELEASE is not supported",
	"prepare": "PREPARE is not supported, use the driver's prepared statements", "execute": "EXECUTE is not supported", "deallocate": "DEALLOCATE is not supported",
	"discard": "DISCARD is not supported", "lock": "LOCK is not supported, every transaction is serializable", "do": "DO is not supported", "comment": "COMMENT is not supported",
	"merge": "MERGE is not supported", "declare": "DECLARE is not supported", "fetch": "FETCH is not supported", "close": "CLOSE is not supported", "move": "MOVE is not supported",
	"listen": "LISTEN is not supported", "notify": "NOTIFY is not supported", "unlisten": "UNLISTEN is not supported", "reset": "RESET is not supported",
	"reindex": "REINDEX is not supported", "cluster": "CLUSTER is not supported", "checkpoint": "CHECKPOINT is not supported", "call": "CALL is not supported",
	"refresh": "REFRESH is not supported", "security": "SECURITY LABEL is not supported", "import": "IMPORT is not supported", "load": "LOAD is not supported",
	"reassign": "REASSIGN is not supported",
}

func (p *parser) statement() (ast.Statement, error) {
	switch {
	case p.isWord("create"):
		return p.createTable()
	case p.isWord("drop"):
		return p.dropTable()
	case p.isWord("insert"):
		return p.insert()
	case p.isWord("update"):
		return p.update()
	case p.isWord("delete"):
		return p.delete()
	case p.isKeyword("select"):
		return p.selectStmt()
	case p.isWord("begin"), p.isWord("start"):
		return p.begin()
	case p.isWord("commit"), p.isKeyword("end"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.transactionWord(); err != nil {
			return nil, err
		}
		return &ast.Commit{}, nil
	case p.isWord("rollback"), p.isWord("abort"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.transactionWord(); err != nil {
			return nil, err
		}
		return &ast.Rollback{}, nil
	}
	if msg, ok := unsupportedStatements[p.tok.text]; ok && (p.tok.kind == tokKeyword || (p.tok.kind == tokIdent && !p.tok.quoted)) {
		return nil, p.unsupportedf("%s", msg)
	}
	if p.isOp("(") {
		return nil, p.unsupportedf("a parenthesized statement is not supported")
	}
	return nil, p.errorf("expected a statement, got %v", p.tok)
}

// transactionWord accepts the optional WORK or TRANSACTION after BEGIN, COMMIT, and ROLLBACK.
func (p *parser) transactionWord() error {
	if ok, err := p.accept("work"); err != nil || ok {
		return err
	}
	_, err := p.accept("transaction")
	return err
}

// begin parses BEGIN [WORK | TRANSACTION] or START TRANSACTION, with transaction modes that
// may be separated by commas.
func (p *parser) begin() (ast.Statement, error) {
	start := p.isWord("start")
	if err := p.advance(); err != nil {
		return nil, err
	}
	if start {
		if err := p.expectWord("transaction"); err != nil {
			return nil, err
		}
	} else if err := p.transactionWord(); err != nil {
		return nil, err
	}
	s := &ast.Begin{}
	afterComma := false
	for {
		switch {
		case p.isWord("read"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			switch {
			case p.isKeyword("only"):
				s.ReadOnly = true
			case p.isWord("write"):
				s.ReadOnly = false
			default:
				return nil, p.errorf("expected ONLY or WRITE, got %v", p.tok)
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		case p.isWord("isolation"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			if err := p.expectWord("level"); err != nil {
				return nil, err
			}
			if !p.isWord("serializable") {
				return nil, p.unsupportedf("isolation level %s is not supported, every transaction is SERIALIZABLE", strings.ToUpper(p.tok.text))
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		case p.isKeyword("not"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			if err := p.expectWord("deferrable"); err != nil {
				return nil, err
			}
		case p.isKeyword("deferrable"):
			return nil, p.unsupportedf("DEFERRABLE transactions are not supported")
		case afterComma:
			return nil, p.errorf("expected a transaction mode, got %v", p.tok)
		default:
			return s, nil
		}
		afterComma = p.isOp(",")
		if afterComma {
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
}

// unsupportedTypes that Postgres has and maragudb does not, with the type to use instead.
var unsupportedTypes = map[string]string{
	"integer": "integer is not supported, use bigint", "int": "int is not supported, use bigint", "int4": "int4 is not supported, use bigint",
	"smallint": "smallint is not supported, use bigint", "int2": "int2 is not supported, use bigint", "serial": "serial is not supported, because there are no sequences; use bigint and generate the keys in the application",
	"bigserial":   "bigserial is not supported, because there are no sequences; use bigint and generate the keys in the application",
	"smallserial": "smallserial is not supported, because there are no sequences; use bigint and generate the keys in the application",
	"real":        "real is not supported, use double precision", "float4": "float4 is not supported, use double precision",
	"numeric": "numeric is not supported, use double precision or bigint", "decimal": "decimal is not supported, use double precision or bigint",
	"dec": "dec is not supported, use double precision or bigint", "money": "money is not supported, use bigint",
	"varchar": "varchar is not supported, use text", "character": "character and character varying are not supported, use text",
	"char": "char is not supported, use text", "bpchar": "bpchar is not supported, use text", "name": "name is not supported, use text",
	"date": "date is not supported, use timestamptz", "interval": "interval is not supported",
	"json": "json is not supported, use text", "jsonb": "jsonb is not supported, use text", "uuid": "uuid is not supported, use text or bytea",
	"bit": "bit is not supported, use bytea", "varbit": "varbit is not supported, use bytea", "xml": "xml is not supported, use text",
	"inet": "inet is not supported, use text", "cidr": "cidr is not supported, use text", "macaddr": "macaddr is not supported, use text",
	"macaddr8": "macaddr8 is not supported, use text", "point": "point is not supported", "line": "line is not supported", "lseg": "lseg is not supported",
	"box": "box is not supported", "path": "path is not supported", "polygon": "polygon is not supported", "circle": "circle is not supported",
	"tsvector": "tsvector is not supported", "tsquery": "tsquery is not supported", "oid": "oid is not supported, use bigint",
	"regclass": "regclass is not supported", "pg_lsn": "pg_lsn is not supported", "txid_snapshot": "txid_snapshot is not supported",
	"void": "void is not supported", "record": "record is not supported", "anyelement": "anyelement is not supported", "unknown": "unknown is not supported",
}

// typeName parses a type: bigint or int8; double precision, float8, or float; boolean or
// bool; text; bytea; and timestamptz or timestamp with time zone.
func (p *parser) typeName() (value.Type, error) {
	if p.tok.kind != tokIdent && p.tok.kind != tokKeyword || p.tok.quoted {
		return 0, p.errorf("expected a type, got %v", p.tok)
	}
	word := p.tok.text
	var t value.Type
	switch word {
	case "bigint", "int8":
		t = value.Int
	case "float8", "float":
		t = value.Float
	case "boolean", "bool":
		t = value.Bool
	case "text":
		t = value.String
	case "bytea":
		t = value.Bytes
	case "timestamptz":
		t = value.Time
	case "double":
		if err := p.advance(); err != nil {
			return 0, err
		}
		if err := p.expectWord("precision"); err != nil {
			return 0, err
		}
		return p.typeModifier(value.Float)
	case "timestamp", "time":
		if err := p.advance(); err != nil {
			return 0, err
		}
		if p.isOp("(") {
			return 0, p.unsupportedf("a precision on %s is not supported, timestamptz has microseconds", word)
		}
		with := p.isKeyword("with")
		if with || p.isWord("without") {
			if err := p.advance(); err != nil {
				return 0, err
			}
			if err := p.expectWord("time"); err != nil {
				return 0, err
			}
			if err := p.expectWord("zone"); err != nil {
				return 0, err
			}
		}
		if word == "timestamp" && with {
			return value.Time, nil
		}
		if word == "timestamp" {
			return 0, p.unsupportedf("timestamp without time zone is not supported, use timestamptz")
		}
		return 0, p.unsupportedf("the time type is not supported, use timestamptz")
	default:
		if msg, ok := unsupportedTypes[word]; ok {
			return 0, p.unsupportedf("%s", msg)
		}
		if p.tok.kind == tokKeyword {
			return 0, p.errorf("expected a type, got %v", p.tok)
		}
		// Postgres has more types than the list above, and extensions add more, so any other
		// name is unsupported too, rather than a type that does not exist.
		return 0, p.unsupportedf("type %s is not supported; the supported types are bigint, double precision, boolean, text, bytea, and timestamptz", word)
	}
	if err := p.advance(); err != nil {
		return 0, err
	}
	return p.typeModifier(t)
}

// typeModifier rejects a modifier such as (3) after a type, since none of the supported types
// takes one in a way maragudb supports.
func (p *parser) typeModifier(t value.Type) (value.Type, error) {
	if p.isOp("(") {
		return 0, p.unsupportedf("a type modifier is not supported on %s", t)
	}
	return t, nil
}

func (p *parser) createTable() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.isKeyword("unique") || p.isWord("index") {
		return p.createIndex()
	}
	for _, w := range []string{"temp", "temporary", "unlogged", "global", "local"} {
		if p.isWord(w) {
			return nil, p.unsupportedf("%s tables are not supported", strings.ToUpper(w))
		}
	}
	if !p.isKeyword("table") {
		return nil, p.unsupportedf("CREATE %s is not supported, only CREATE TABLE and CREATE INDEX", strings.ToUpper(p.tok.text))
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	s := &ast.CreateTable{}
	if p.isWord("if") && p.peek().text == "not" {
		if err := p.advance(); err != nil {
			return nil, err
		}
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
	if p.isKeyword("as") || p.isWord("of") || p.isKeyword("like") {
		return nil, p.unsupportedf("CREATE TABLE %s is not supported", strings.ToUpper(p.tok.text))
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	if p.isOp(")") {
		return nil, p.unsupportedf("a table without columns is not supported")
	}
	setPrimaryKey := func(name string, columns []string) error {
		if s.PrimaryKey != nil {
			return fmt.Errorf("%w: multiple primary keys for table %q are not allowed", ErrInvalid, s.Name)
		}
		s.PrimaryKey, s.PrimaryKeyName = columns, name
		return nil
	}
	for {
		name := ""
		if p.isKeyword("constraint") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			if name, err = p.ident(); err != nil {
				return nil, err
			}
		}
		switch {
		case p.isKeyword("primary"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			if err := p.expectWord("key"); err != nil {
				return nil, err
			}
			columns, err := p.columnList()
			if err != nil {
				return nil, err
			}
			if err := setPrimaryKey(name, columns); err != nil {
				return nil, err
			}
			if err := p.constraintOptions(); err != nil {
				return nil, err
			}
		case p.isKeyword("unique"):
			u, err := p.unique(name)
			if err != nil {
				return nil, err
			}
			if u.Columns, err = p.columnList(); err != nil {
				return nil, err
			}
			if err := p.constraintOptions(); err != nil {
				return nil, err
			}
			s.Unique = append(s.Unique, u)
		case p.isKeyword("check"):
			c, err := p.check(name)
			if err != nil {
				return nil, err
			}
			s.Checks = append(s.Checks, c)
		case p.isKeyword("foreign"):
			if err := p.advance(); err != nil {
				return nil, err
			}
			if err := p.expectWord("key"); err != nil {
				return nil, err
			}
			columns, err := p.columnList()
			if err != nil {
				return nil, err
			}
			fk, err := p.references(name, columns)
			if err != nil {
				return nil, err
			}
			s.ForeignKeys = append(s.ForeignKeys, fk)
		case p.tok.kind == tokKeyword && unsupportedConstraints[p.tok.text] != "":
			return nil, p.unsupportedf("%s", unsupportedConstraints[p.tok.text])
		case p.isWord("exclude") && (p.peekIsWord("using") || p.peek().kind == tokOp && p.peek().text == "("):
			return nil, p.unsupportedf("%s", unsupportedConstraints["exclude"])
		case p.isKeyword("like"):
			return nil, p.unsupportedf("LIKE in CREATE TABLE is not supported")
		case name != "":
			return nil, p.errorf("expected a constraint after CONSTRAINT %s, got %v", name, p.tok)
		default:
			col, err := p.columnDef()
			if err != nil {
				return nil, err
			}
			if col.pk {
				if err := setPrimaryKey(col.pkName, []string{col.def.Name}); err != nil {
					return nil, err
				}
			}
			s.Columns = append(s.Columns, col.def)
			s.Unique = append(s.Unique, col.unique...)
			s.Checks = append(s.Checks, col.checks...)
			s.ForeignKeys = append(s.ForeignKeys, col.foreignKeys...)
		}
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
	if len(s.Columns) == 0 {
		return nil, p.unsupportedf("a table without columns is not supported")
	}
	if s.PrimaryKey == nil {
		return nil, p.unsupportedf("a table without a primary key is not supported, add PRIMARY KEY to a column or PRIMARY KEY (columns)")
	}
	return s, nil
}

// columnList parses a parenthesized list of column names.
func (p *parser) columnList() ([]string, error) {
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	var columns []string
	for {
		col, err := p.ident()
		if err != nil {
			return nil, err
		}
		columns = append(columns, col)
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	return columns, p.expectOp(")")
}

// unique parses UNIQUE [NULLS [NOT] DISTINCT] of a constraint with the given name.
func (p *parser) unique(name string) (ast.UniqueConstraint, error) {
	u := ast.UniqueConstraint{Name: name}
	if err := p.advance(); err != nil {
		return u, err
	}
	var err error
	u.NullsNotDistinct, err = p.nullsDistinct()
	return u, err
}

// nullsDistinct parses an optional NULLS [NOT] DISTINCT and reports whether it is NOT DISTINCT.
func (p *parser) nullsDistinct() (bool, error) {
	if !p.isWord("nulls") {
		return false, nil
	}
	if err := p.advance(); err != nil {
		return false, err
	}
	not, err := p.accept("not")
	if err != nil {
		return false, err
	}
	return not, p.expectWord("distinct")
}

// constraintOptions rejects the index options that may follow a PRIMARY KEY or UNIQUE
// constraint.
func (p *parser) constraintOptions() error {
	switch {
	case p.isWord("include"):
		return p.unsupportedf("INCLUDE is not supported")
	case p.isKeyword("with"):
		return p.unsupportedf("WITH storage parameters are not supported")
	case p.isKeyword("using"):
		return p.unsupportedf("USING INDEX TABLESPACE is not supported")
	case p.isKeyword("deferrable"), p.isKeyword("initially"), p.isKeyword("not") && p.peekIsWord("deferrable"):
		return p.unsupportedf("DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once")
	}
	return nil
}

// check parses CHECK (expr) and its options, of a constraint with the given name.
func (p *parser) check(name string) (ast.CheckConstraint, error) {
	c := ast.CheckConstraint{Name: name}
	if err := p.advance(); err != nil {
		return c, err
	}
	if err := p.expectOp("("); err != nil {
		return c, err
	}
	var err error
	if c.Expr, err = p.expr(0); err != nil {
		return c, err
	}
	if err := p.expectOp(")"); err != nil {
		return c, err
	}
	if p.isWord("no") && p.peekIsWord("inherit") {
		return c, p.unsupportedf("NO INHERIT is not supported, there is no inheritance")
	}
	return c, p.enforcement()
}

// references parses REFERENCES and what follows it, of a foreign key with the given name on the
// given columns.
func (p *parser) references(name string, columns []string) (ast.ForeignKey, error) {
	fk := ast.ForeignKey{Name: name, Columns: columns}
	if err := p.expectWord("references"); err != nil {
		return fk, err
	}
	var err error
	if fk.Table, err = p.ident(); err != nil {
		return fk, err
	}
	if p.isOp("(") {
		if fk.RefColumns, err = p.columnList(); err != nil {
			return fk, err
		}
	}
	if p.isWord("match") {
		if err := p.advance(); err != nil {
			return fk, err
		}
		switch {
		case p.isKeyword("full"):
			return fk, p.unsupportedf("MATCH FULL is not supported, write MATCH SIMPLE or nothing, and NOT NULL on the columns that must be set together")
		case p.isWord("partial"):
			return fk, p.unsupportedf("MATCH PARTIAL is not supported")
		case p.isWord("simple"):
		default:
			return fk, p.errorf("expected FULL, PARTIAL, or SIMPLE, got %v", p.tok)
		}
		if err := p.advance(); err != nil {
			return fk, err
		}
	}
	var onDelete, onUpdate bool
	for p.isKeyword("on") {
		if err := p.advance(); err != nil {
			return fk, err
		}
		switch {
		case p.isWord("delete") && !onDelete:
			onDelete = true
			if err := p.advance(); err != nil {
				return fk, err
			}
			if fk.OnDelete, fk.SetColumns, err = p.referentialAction(true); err != nil {
				return fk, err
			}
		case p.isWord("update") && !onUpdate:
			onUpdate = true
			if err := p.advance(); err != nil {
				return fk, err
			}
			if fk.OnUpdate, _, err = p.referentialAction(false); err != nil {
				return fk, err
			}
		default:
			return fk, p.errorf("syntax error at or near %v", p.tok)
		}
	}
	return fk, p.enforcement()
}

// referentialAction parses the action of ON DELETE, or of ON UPDATE, and the columns of SET NULL
// or SET DEFAULT.
func (p *parser) referentialAction(onDelete bool) (ast.Action, []string, error) {
	var a ast.Action
	switch {
	case p.isWord("no"):
		if err := p.advance(); err != nil {
			return a, nil, err
		}
		if err := p.expectWord("action"); err != nil {
			return a, nil, err
		}
		return ast.NoAction, nil, nil
	case p.isWord("restrict"):
		a = ast.Restrict
	case p.isWord("cascade"):
		a = ast.Cascade
	case p.isWord("set"):
		if err := p.advance(); err != nil {
			return a, nil, err
		}
		switch {
		case p.isKeyword("null"):
			a = ast.SetNull
		case p.isKeyword("default"):
			a = ast.SetDefault
		default:
			return a, nil, p.errorf("expected NULL or DEFAULT, got %v", p.tok)
		}
	default:
		return a, nil, p.errorf("expected a referential action, got %v", p.tok)
	}
	if !onDelete && a != ast.Restrict {
		return a, nil, p.unsupportedf("ON UPDATE %s is not supported, only NO ACTION and RESTRICT; update the referencing rows in a statement of their own", a)
	}
	if err := p.advance(); err != nil {
		return a, nil, err
	}
	var columns []string
	if (a == ast.SetNull || a == ast.SetDefault) && p.isOp("(") {
		var err error
		if columns, err = p.columnList(); err != nil {
			return a, nil, err
		}
	}
	return a, columns, nil
}

// enforcement parses the options of a CHECK or FOREIGN KEY constraint: ENFORCED, which every
// constraint is, and rejects NOT ENFORCED and NOT VALID.
func (p *parser) enforcement() error {
	for {
		switch {
		case p.isWord("enforced"):
			if err := p.advance(); err != nil {
				return err
			}
		case p.isKeyword("not") && p.peekIsWord("enforced"):
			return p.unsupportedf("NOT ENFORCED is not supported, every constraint is enforced")
		case p.isKeyword("not") && p.peekIsWord("valid"):
			return p.unsupportedf("NOT VALID is not supported, every constraint holds for every row")
		case p.isKeyword("deferrable"), p.isKeyword("initially"), p.isKeyword("not") && p.peekIsWord("deferrable"):
			return p.unsupportedf("%s", unsupportedConstraints["deferrable"])
		default:
			return nil
		}
	}
}

// unsupportedConstraints are the table and column constraints Postgres has beyond PRIMARY KEY,
// UNIQUE, NOT NULL, DEFAULT, CHECK, and FOREIGN KEY.
var unsupportedConstraints = map[string]string{
	"exclude":    "EXCLUDE is not supported",
	"generated":  "GENERATED columns, identity columns among them, are not supported, because there are no sequences; generate the values in the application",
	"collate":    "COLLATE is not supported, strings compare by byte order",
	"deferrable": "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once",
	"initially":  "DEFERRABLE and NOT DEFERRABLE are not supported, every constraint is checked at once",
}

type columnDef struct {
	def         ast.ColumnDef
	pk          bool
	pkName      string
	unique      []ast.UniqueConstraint
	checks      []ast.CheckConstraint
	foreignKeys []ast.ForeignKey
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
		name := ""
		if p.isKeyword("constraint") {
			if err := p.advance(); err != nil {
				return c, err
			}
			if name, err = p.ident(); err != nil {
				return c, err
			}
		}
		switch {
		case p.isKeyword("not"):
			if err := p.advance(); err != nil {
				return c, err
			}
			switch {
			case p.isKeyword("deferrable"):
				return c, p.unsupportedf("%s", unsupportedConstraints["deferrable"])
			case p.isWord("enforced"):
				return c, p.unsupportedf("NOT ENFORCED is not supported, every constraint is enforced")
			}
			if err := p.expectWord("null"); err != nil {
				return c, err
			}
			if name != "" {
				return c, p.unsupportedf("a name for a NOT NULL constraint is not supported, leave out CONSTRAINT %s", name)
			}
			c.def.NotNull = true
		case p.isKeyword("default"):
			if c.def.Default != nil {
				return c, p.errorf("multiple default values specified for column %q", c.def.Name)
			}
			if err := p.advance(); err != nil {
				return c, err
			}
			p.restricted = true
			e, err := p.expr(0)
			p.restricted = false
			if err != nil {
				return c, err
			}
			c.def.Default = e
		case p.isKeyword("check"):
			check, err := p.check(name)
			if err != nil {
				return c, err
			}
			c.checks = append(c.checks, check)
		case p.isKeyword("references"):
			fk, err := p.references(name, []string{c.def.Name})
			if err != nil {
				return c, err
			}
			c.foreignKeys = append(c.foreignKeys, fk)
		case p.isWord("enforced"):
			if err := p.advance(); err != nil {
				return c, err
			}
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
			if c.pk {
				return c, fmt.Errorf("%w: multiple primary keys for table are not allowed", ErrInvalid)
			}
			c.pk, c.pkName = true, name
			c.def.NotNull = true
			if err := p.constraintOptions(); err != nil {
				return c, err
			}
		case p.isKeyword("unique"):
			u, err := p.unique(name)
			if err != nil {
				return c, err
			}
			u.Columns = []string{c.def.Name}
			c.unique = append(c.unique, u)
			if err := p.constraintOptions(); err != nil {
				return c, err
			}
		default:
			if msg, ok := unsupportedConstraints[p.tok.text]; ok && (p.tok.kind == tokKeyword || (p.tok.kind == tokIdent && !p.tok.quoted)) {
				return c, p.unsupportedf("%s", msg)
			}
			if name != "" {
				return c, p.errorf("expected a constraint after CONSTRAINT %s, got %v", name, p.tok)
			}
			return c, nil
		}
	}
}

// createIndex parses CREATE [UNIQUE] INDEX after CREATE.
func (p *parser) createIndex() (ast.Statement, error) {
	s := &ast.CreateIndex{}
	if p.isKeyword("unique") {
		s.Unique = true
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if err := p.expectWord("index"); err != nil {
		return nil, err
	}
	if p.isKeyword("concurrently") {
		return nil, p.unsupportedf("CREATE INDEX CONCURRENTLY is not supported, every statement is atomic")
	}
	var err error
	switch {
	case p.isWord("if") && p.peek().text == "not":
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.expectWord("not"); err != nil {
			return nil, err
		}
		if err := p.expectWord("exists"); err != nil {
			return nil, err
		}
		s.IfNotExists = true
		if s.Name, err = p.ident(); err != nil {
			return nil, err
		}
	case !p.isKeyword("on"):
		if s.Name, err = p.ident(); err != nil {
			return nil, err
		}
	}
	if err := p.expectWord("on"); err != nil {
		return nil, err
	}
	if p.isKeyword("only") {
		return nil, p.unsupportedf("ON ONLY is not supported, there is no inheritance")
	}
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.isKeyword("using") {
		return nil, p.unsupportedf("USING is not supported, every index is a btree")
	}
	if err := p.expectOp("("); err != nil {
		return nil, err
	}
	for {
		if p.isOp("(") {
			return nil, p.unsupportedf("expression indexes are not supported, index columns")
		}
		var c ast.IndexColumn
		if c.Name, err = p.ident(); err != nil {
			return nil, err
		}
		switch {
		case p.isOp("("):
			return nil, p.unsupportedf("expression indexes are not supported, index columns")
		case p.isKeyword("collate"):
			return nil, p.unsupportedf("COLLATE is not supported, strings compare by byte order")
		case p.tok.kind == tokIdent && !p.isWord("nulls"):
			return nil, p.unsupportedf("operator classes are not supported")
		}
		if p.isKeyword("desc") {
			c.Desc = true
		}
		if p.isKeyword("asc") || p.isKeyword("desc") {
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		if p.isWord("nulls") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			switch {
			case p.isWord("first"):
				c.Nulls = ast.NullsFirst
			case p.isWord("last"):
				c.Nulls = ast.NullsLast
			default:
				return nil, p.errorf("expected FIRST or LAST, got %v", p.tok)
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
		s.Columns = append(s.Columns, c)
		if !p.isOp(",") {
			break
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if p.isWord("include") {
		return nil, p.unsupportedf("INCLUDE is not supported")
	}
	if s.NullsNotDistinct, err = p.nullsDistinct(); err != nil {
		return nil, err
	}
	switch {
	case p.isKeyword("with"):
		return nil, p.unsupportedf("WITH storage parameters are not supported")
	case p.isWord("tablespace"):
		return nil, p.unsupportedf("TABLESPACE is not supported")
	case p.isKeyword("where"):
		return nil, p.unsupportedf("partial indexes are not supported, index the whole table")
	}
	return s, nil
}

func (p *parser) dropTable() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	index := p.isWord("index")
	if !p.isKeyword("table") && !index {
		return nil, p.unsupportedf("DROP %s is not supported, only DROP TABLE and DROP INDEX", strings.ToUpper(p.tok.text))
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if index && p.isKeyword("concurrently") {
		return nil, p.unsupportedf("DROP INDEX CONCURRENTLY is not supported, every statement is atomic")
	}
	ifExists := false
	if p.isWord("if") && p.peek().text == "exists" {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if err := p.expectWord("exists"); err != nil {
			return nil, err
		}
		ifExists = true
	}
	name, err := p.ident()
	if err != nil {
		return nil, err
	}
	if p.isOp(",") {
		return nil, p.unsupportedf("dropping several tables or indexes in one statement is not supported, drop them one at a time")
	}
	if _, err := p.accept("restrict"); err != nil {
		return nil, err
	}
	if index {
		return &ast.DropIndex{Name: name, IfExists: ifExists}, nil
	}
	return &ast.DropTable{Name: name, IfExists: ifExists}, nil
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
	if p.isKeyword("as") {
		return nil, p.unsupportedf("a table alias in INSERT is not supported")
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
	switch {
	case p.isKeyword("select"), p.isKeyword("with"), p.isOp("("):
		return nil, p.unsupportedf("INSERT ... SELECT is not supported, write VALUES")
	case p.isKeyword("default") && s.Columns == nil:
		if err := p.advance(); err != nil {
			return nil, err
		}
		s.DefaultValues = true
		return s, p.expectWord("values")
	case p.isWord("overriding"):
		return nil, p.unsupportedf("OVERRIDING is not supported")
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
	if p.isKeyword("only") {
		return nil, p.unsupportedf("UPDATE ONLY is not supported, there is no inheritance")
	}
	s := &ast.Update{}
	var err error
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.isOp(".") {
		return nil, p.unsupportedf("a table name with a schema is not supported, name the table alone")
	}
	// SET is a keyword only in context, and never an alias without AS, as in Postgres.
	if s.Alias, err = p.alias(!p.isWord("set")); err != nil {
		return nil, err
	}
	if err := p.expectWord("set"); err != nil {
		return nil, err
	}
	for {
		if p.isOp("(") {
			return nil, p.unsupportedf("SET (columns) = (values) is not supported, assign the columns one at a time")
		}
		var a ast.Assignment
		if a.Column, err = p.ident(); err != nil {
			return nil, err
		}
		if p.isOp(".") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			a.Qualifier = a.Column
			if a.Column, err = p.label(); err != nil {
				return nil, err
			}
			if p.isOp(".") {
				return nil, p.unsupportedf("assigning to a field of a field is not supported")
			}
		}
		if err := p.expectOp("="); err != nil {
			return nil, err
		}
		if a.Value, err = p.expr(0); err != nil {
			return nil, err
		}
		s.Set = append(s.Set, a)
		if p.isOp(",") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			continue
		}
		break
	}
	if p.isKeyword("from") {
		return nil, p.unsupportedf("UPDATE ... FROM is not supported, read the other tables in a SELECT first")
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
	if p.isKeyword("only") {
		return nil, p.unsupportedf("DELETE FROM ONLY is not supported, there is no inheritance")
	}
	s := &ast.Delete{}
	var err error
	if s.Table, err = p.ident(); err != nil {
		return nil, err
	}
	if p.isOp(".") {
		return nil, p.unsupportedf("a table name with a schema is not supported, name the table alone")
	}
	if s.Alias, err = p.alias(true); err != nil {
		return nil, err
	}
	if p.isKeyword("using") {
		return nil, p.unsupportedf("DELETE ... USING is not supported, read the other tables in a SELECT first")
	}
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	return s, nil
}

// alias of a table: AS and a name, or, when bare, a name alone, as Postgres reads an alias
// without AS.
func (p *parser) alias(bare bool) (string, error) {
	if ok, err := p.accept("as"); err != nil {
		return "", err
	} else if ok {
		return p.ident()
	}
	if bare && p.tok.kind == tokIdent {
		return p.ident()
	}
	return "", nil
}

// label is a name after a dot, which can be any keyword, as in Postgres.
func (p *parser) label() (string, error) {
	if p.tok.kind != tokIdent && p.tok.kind != tokKeyword {
		return "", p.errorf("expected identifier, got %v", p.tok)
	}
	name := p.tok.text
	return name, p.advance()
}

func (p *parser) where() (ast.Expr, error) {
	if ok, err := p.accept("where"); err != nil || !ok {
		return nil, err
	}
	if p.isWord("current") {
		return nil, p.unsupportedf("WHERE CURRENT OF is not supported")
	}
	return p.expr(0)
}

func (p *parser) selectStmt() (ast.Statement, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	s := &ast.Select{}
	if p.isKeyword("distinct") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.isKeyword("on") {
			return nil, p.unsupportedf("DISTINCT ON is not supported, write DISTINCT")
		}
		s.Distinct = true
	} else if _, err := p.accept("all"); err != nil {
		return nil, err
	}
	if p.tok.kind == tokEOF || p.isKeyword("from") || p.isKeyword("where") || p.isOp(";") {
		return nil, p.unsupportedf("a SELECT without output columns is not supported")
	}
	for {
		var item ast.SelectItem
		if p.tok.kind == tokIdent && p.peek().kind == tokOp && p.peek().text == "." && p.peek2().kind == tokOp && p.peek2().text == "*" {
			item.Table = p.tok.text
			for range 2 {
				if err := p.advance(); err != nil {
					return nil, err
				}
			}
		}
		if p.isOp("*") {
			if err := p.advance(); err != nil {
				return nil, err
			}
			item.Star = true
			switch {
			case p.isOp("."):
				return nil, p.errorf("improper use of \"*\"")
			case item.Table != "" && !p.isOp(",") && !p.isOp(";") && p.tok.kind != tokEOF && !p.isKeyword("from") && !p.isKeyword("where") && !p.isKeyword("order") && !p.isKeyword("limit") && !p.isKeyword("offset"):
				return nil, p.unsupportedf("%s.* is not supported in an expression, name the columns", item.Table)
			}
		} else {
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			item.Expr = e
			if ok, err := p.accept("as"); err != nil {
				return nil, err
			} else if ok {
				// After AS, any word is a label.
				if p.tok.kind != tokIdent && p.tok.kind != tokKeyword {
					return nil, p.errorf("expected identifier, got %v", p.tok)
				}
				item.Alias = p.tok.text
				if err := p.advance(); err != nil {
					return nil, err
				}
			} else if p.tok.kind == tokIdent {
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
	if p.isKeyword("into") {
		return nil, p.unsupportedf("SELECT INTO is not supported")
	}
	if ok, err := p.accept("from"); err != nil {
		return nil, err
	} else if ok {
		for {
			from, err := p.tableRef()
			if err != nil {
				return nil, err
			}
			s.From = append(s.From, from)
			if p.isKeyword("on") || p.isKeyword("using") {
				return nil, p.errorf("unexpected %v", p.tok)
			}
			if !p.isOp(",") {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	var err error
	if s.Where, err = p.where(); err != nil {
		return nil, err
	}
	for _, w := range []string{"group", "having", "window", "union", "intersect", "except"} {
		if p.isKeyword(w) {
			return nil, p.unsupportedf("%s", unsupportedClauses[w])
		}
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
			switch {
			case p.isKeyword("asc"):
				if err := p.advance(); err != nil {
					return nil, err
				}
			case p.isKeyword("desc"):
				o.Desc = true
				if err := p.advance(); err != nil {
					return nil, err
				}
			case p.isKeyword("using"):
				return nil, p.unsupportedf("ORDER BY ... USING is not supported, write ASC or DESC")
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
	// LIMIT and OFFSET come in either order, each at most once, as in Postgres.
	for p.isKeyword("limit") || p.isKeyword("offset") {
		limit := p.isKeyword("limit")
		if err := p.advance(); err != nil {
			return nil, err
		}
		if limit {
			if s.Limit != nil {
				return nil, p.errorf("multiple LIMIT clauses not allowed")
			}
			if p.isKeyword("all") {
				if err := p.advance(); err != nil {
					return nil, err
				}
				s.Limit = &ast.Literal{Value: value.NewNull(value.Unknown)}
				continue
			}
			if s.Limit, err = p.expr(0); err != nil {
				return nil, err
			}
			continue
		}
		if s.Offset != nil {
			return nil, p.errorf("multiple OFFSET clauses not allowed")
		}
		if s.Offset, err = p.expr(0); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// tableRef is an item of FROM: a table or a parenthesized join, followed by any joins with it
// on the left, which associate to the left.
func (p *parser) tableRef() (ast.TableExpr, error) {
	left, err := p.tablePrimary()
	if err != nil {
		return nil, err
	}
	for p.atJoin() {
		if left, err = p.join(left); err != nil {
			return nil, err
		}
	}
	return left, nil
}

// atJoin reports whether a join starts at the current token.
func (p *parser) atJoin() bool {
	for _, w := range []string{"join", "inner", "left", "right", "full", "cross", "natural"} {
		if p.isKeyword(w) {
			return true
		}
	}
	return false
}

// join parses a join with left on its left side. A join that needs a condition takes the joins
// before its condition into its right side, as Postgres's grammar does, so that a JOIN b JOIN c
// ON x ON y is a JOIN (b JOIN c ON x) ON y.
func (p *parser) join(left ast.TableExpr) (ast.TableExpr, error) {
	j := &ast.Join{Left: left}
	if ok, err := p.accept("natural"); err != nil {
		return nil, err
	} else if ok {
		j.Natural = true
	}
	switch {
	case p.isKeyword("cross") && !j.Natural:
		j.Kind = ast.JoinCross
		if err := p.advance(); err != nil {
			return nil, err
		}
	case p.isKeyword("inner"):
		if err := p.advance(); err != nil {
			return nil, err
		}
	case p.isKeyword("left"):
		j.Kind = ast.JoinLeft
		if err := p.advance(); err != nil {
			return nil, err
		}
		if _, err := p.accept("outer"); err != nil {
			return nil, err
		}
	case p.isKeyword("right"), p.isKeyword("full"):
		return nil, p.unsupportedf("%s JOIN is not supported, write a LEFT JOIN with the tables the other way round", strings.ToUpper(p.tok.text))
	}
	if err := p.expectWord("join"); err != nil {
		return nil, err
	}
	right, err := p.tablePrimary()
	if err != nil {
		return nil, err
	}
	if j.Kind == ast.JoinCross || j.Natural {
		j.Right = right
		return j, nil
	}
	for p.atJoin() {
		if right, err = p.join(right); err != nil {
			return nil, err
		}
	}
	j.Right = right
	switch {
	case p.isKeyword("on"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if j.On, err = p.expr(0); err != nil {
			return nil, err
		}
	case p.isKeyword("using"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if j.Using, err = p.columnList(); err != nil {
			return nil, err
		}
		if p.isKeyword("as") {
			return nil, p.unsupportedf("an alias for the columns of USING is not supported")
		}
	default:
		return nil, p.errorf("expected ON or USING, got %v", p.tok)
	}
	return j, nil
}

// tablePrimary is a table with an optional alias, or a join in parentheses.
func (p *parser) tablePrimary() (ast.TableExpr, error) {
	switch {
	case p.isKeyword("only"):
		return nil, p.unsupportedf("FROM ONLY is not supported, there is no inheritance")
	case p.isKeyword("lateral"):
		return nil, p.unsupportedf("LATERAL is not supported")
	case p.isOp("("):
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.isKeyword("select") || p.isKeyword("with") || p.isKeyword("values") || p.isOp("(") && p.peekIsWord("select", "with", "values") {
			return nil, p.unsupportedf("subqueries in FROM are not supported")
		}
		inner, err := p.tableRef()
		if err != nil {
			return nil, err
		}
		if _, ok := inner.(*ast.Join); !ok {
			return nil, p.errorf("unexpected %v", p.tok)
		}
		if err := p.expectOp(")"); err != nil {
			return nil, err
		}
		if p.isKeyword("as") || p.tok.kind == tokIdent {
			return nil, p.unsupportedf("an alias for a join is not supported, name the columns through the aliases of its tables")
		}
		return inner, nil
	}
	t := &ast.TableRef{}
	var err error
	if t.Name, err = p.ident(); err != nil {
		return nil, err
	}
	switch {
	case p.isOp("."):
		return nil, p.unsupportedf("a table name with a schema is not supported, name the table alone")
	case p.isOp("("):
		return nil, p.unsupportedf("functions in FROM are not supported")
	}
	if t.Alias, err = p.alias(true); err != nil {
		return nil, err
	}
	if t.Alias != "" && p.isOp("(") {
		if t.Columns, err = p.columnList(); err != nil {
			return nil, err
		}
	}
	if p.isKeyword("tablesample") {
		return nil, p.unsupportedf("TABLESAMPLE is not supported")
	}
	return t, nil
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
	bpCast
)

// binaryOp and its binding power for an operator token, if it is one.
func binaryOp(text string) (ast.BinaryOp, int, bool) {
	switch text {
	case "or":
		return ast.Or, bpOr, true
	case "and":
		return ast.And, bpAnd, true
	case "=":
		return ast.Eq, bpCmp, true
	case "<>":
		return ast.Ne, bpCmp, true
	case "<":
		return ast.Lt, bpCmp, true
	case "<=":
		return ast.Le, bpCmp, true
	case ">":
		return ast.Gt, bpCmp, true
	case ">=":
		return ast.Ge, bpCmp, true
	case "||":
		return ast.Concat, bpConcat, true
	case "+":
		return ast.Add, bpAdd, true
	case "-":
		return ast.Sub, bpAdd, true
	case "*":
		return ast.Mul, bpMul, true
	case "/":
		return ast.Div, bpMul, true
	case "%":
		return ast.Mod, bpMul, true
	}
	return 0, 0, false
}

// expr parses an expression whose operators all bind at least as tightly as minBp.
func (p *parser) expr(minBp int) (ast.Expr, error) {
	left, err := p.unary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.restricted && (p.isKeyword("and") || p.isKeyword("or") || p.isKeyword("is") || p.isKeyword("isnull") || p.isKeyword("notnull") ||
			p.isKeyword("not") || p.isWord("between") || p.isKeyword("in") || p.isKeyword("like") || p.isKeyword("ilike") || p.isKeyword("similar")):
			// What follows is not part of the restricted expression of a DEFAULT.
			return left, nil
		case p.tok.kind == tokOp || p.isKeyword("and") || p.isKeyword("or"):
			op, bp, ok := binaryOp(p.tok.text)
			if !ok || bp < minBp {
				return left, nil
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			right, err := p.expr(bp + 1)
			if err != nil {
				return nil, err
			}
			left = &ast.Binary{Op: op, Left: left, Right: right}
			if _, nextBp, ok := binaryOp(p.tok.text); bp == bpCmp && p.tok.kind == tokOp && ok && nextBp == bpCmp {
				return nil, p.errorf("syntax error at or near %v", p.tok)
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
			switch {
			case p.isKeyword("null"):
			case p.isKeyword("true"), p.isKeyword("false"):
				return nil, p.unsupportedf("IS %s is not supported, compare with = or write IS NOT NULL AND", strings.ToUpper(p.tok.text))
			case p.isWord("unknown"):
				return nil, p.unsupportedf("IS UNKNOWN is not supported, write IS NULL")
			case p.isKeyword("distinct"):
				return nil, p.unsupportedf("IS DISTINCT FROM is not supported, write the NULL cases out")
			default:
				return nil, p.errorf("expected NULL, got %v", p.tok)
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
			left = &ast.IsNull{Expr: left, Not: not}
		case p.isKeyword("isnull"), p.isKeyword("notnull"):
			return nil, p.unsupportedf("%s is not supported, write IS NULL or IS NOT NULL", strings.ToUpper(p.tok.text))
		case p.isKeyword("not") && p.peekIsWord("between", "in", "like", "ilike", "similar"),
			p.isWord("between"), p.isKeyword("in"), p.isKeyword("like"), p.isKeyword("ilike"), p.isKeyword("similar"):
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
			case p.isWord("between"):
				if err := p.advance(); err != nil {
					return nil, err
				}
				if p.isKeyword("symmetric") {
					return nil, p.unsupportedf("BETWEEN SYMMETRIC is not supported, write the bounds in order")
				}
				if _, err := p.accept("asymmetric"); err != nil {
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
				if p.isKeyword("select") || p.isKeyword("with") {
					return nil, p.unsupportedf("subqueries are not supported")
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
				if p.isWord("escape") {
					return nil, p.unsupportedf("LIKE with ESCAPE is not supported, escape with a backslash")
				}
				left = &ast.Like{Expr: left, Pattern: pattern, Not: not}
			case p.isKeyword("ilike"):
				return nil, p.unsupportedf("ILIKE is not supported, write lower(x) LIKE lower(pattern)")
			default:
				return nil, p.unsupportedf("SIMILAR TO is not supported")
			}
		case p.isWord("at") && p.peek().text == "time":
			return nil, p.unsupportedf("AT TIME ZONE is not supported, every timestamptz is read and written in UTC")
		case p.isKeyword("collate"):
			return nil, p.unsupportedf("COLLATE is not supported, strings compare by byte order")
		default:
			return left, nil
		}
	}
}

func (p *parser) unary() (ast.Expr, error) {
	switch {
	case p.isKeyword("not") && p.restricted:
		return nil, p.errorf("syntax error at or near NOT")
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
		if p.tok.kind == tokNumber && (p.peek().kind != tokOp || p.peek().text != "::") {
			// A sign directly before a number is part of the literal, as in Postgres, so that
			// the most negative bigint can be written. Before a cast it is an operator applied
			// to the cast's result, also as in Postgres.
			return p.number("-")
		}
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		if lit, ok := e.(*ast.Literal); ok {
			if negated, ok := negateLiteral(lit); ok {
				return negated, nil
			}
		}
		return &ast.Unary{Op: ast.Neg, Expr: e}, nil
	case p.isOp("+"):
		if err := p.advance(); err != nil {
			return nil, err
		}
		// Unlike a minus, Postgres never folds a plus into the number, so +1 in ORDER BY is an
		// expression and not an output position.
		e, err := p.unary()
		if err != nil {
			return nil, err
		}
		return &ast.Unary{Op: ast.Pos, Expr: e}, nil
	}
	return p.postfix()
}

// postfix parses a primary expression followed by any number of ::type casts, which bind
// tighter than every operator, including a sign in front.
func (p *parser) postfix() (ast.Expr, error) {
	e, err := p.primary()
	if err != nil {
		return nil, err
	}
	for p.isOp("::") {
		if err := p.advance(); err != nil {
			return nil, err
		}
		t, err := p.typeName()
		if err != nil {
			return nil, err
		}
		e = &ast.Cast{Expr: e, Type: t, Style: ast.CastColons}
	}
	return e, nil
}

func (p *parser) number(sign string) (ast.Expr, error) {
	text := sign + strings.ReplaceAll(p.tok.text, "_", "")
	pos := p.tok.pos
	if err := p.advance(); err != nil {
		return nil, err
	}
	lower := strings.ToLower(text)
	base := 10
	digits := strings.TrimPrefix(lower, "-")
	switch {
	case strings.HasPrefix(digits, "0x"):
		base, digits = 16, digits[2:]
	case strings.HasPrefix(digits, "0o"):
		base, digits = 8, digits[2:]
	case strings.HasPrefix(digits, "0b"):
		base, digits = 2, digits[2:]
	case strings.ContainsAny(lower, ".e"):
		// A numeric literal, which only becomes a value when its context gives it a type.
		return &ast.Literal{Value: value.NewNumeric(text)}, nil
	}
	n, ok := new(big.Int).SetString(digits, base)
	if !ok {
		return nil, fmt.Errorf("%w at position %d: invalid number %s", ErrSyntax, pos+1, text)
	}
	if sign == "-" {
		n.Neg(n)
	}
	if lit, ok := integerLiteral(n); ok {
		return lit, nil
	}
	// Beyond the bigint range, an integer is a numeric, in decimal whatever its base.
	return &ast.Literal{Value: value.NewNumeric(n.String())}, nil
}

// integerLiteral types an integer constant by its value, sign included, as Postgres does: an
// integer when it fits in 32 bits, a bigint when it fits in 64, and otherwise none.
func integerLiteral(n *big.Int) (*ast.Literal, bool) {
	switch {
	case !n.IsInt64():
		return nil, false
	case n.Int64() >= math.MinInt32 && n.Int64() <= math.MaxInt32:
		return &ast.Literal{Value: value.NewInt4(int32(n.Int64()))}, true
	}
	return &ast.Literal{Value: value.NewInt(n.Int64())}, true
}

// negateLiteral folds a minus into a numeric constant, as Postgres does even through
// parentheses, so that -(5) is the constant -5 and -(-2147483648) the bigint 2147483648.
func negateLiteral(lit *ast.Literal) (*ast.Literal, bool) {
	v := lit.Value
	switch {
	case v.Null:
		return nil, false
	case v.Type == value.Int4 || v.Type == value.Int:
		n := big.NewInt(v.I)
		n.Neg(n)
		if out, ok := integerLiteral(n); ok {
			return out, true
		}
		return &ast.Literal{Value: value.NewNumeric(n.String())}, true
	case v.Type == value.Numeric:
		text := "-" + v.S
		if strings.HasPrefix(v.S, "-") {
			text = v.S[1:]
		}
		if n, ok := new(big.Int).SetString(text, 10); ok {
			if out, ok := integerLiteral(n); ok {
				return out, true
			}
		}
		return &ast.Literal{Value: value.NewNumeric(text)}, true
	}
	return nil, false
}

// typeFuncNameKeywords are the keywords of Postgres's type_func_name class, which are reserved
// as column names but can name a function or a type.
var typeFuncNameKeywords = map[string]bool{
	"authorization": true, "binary": true, "collation": true, "concurrently": true, "cross": true, "current_schema": true,
	"freeze": true, "full": true, "ilike": true, "inner": true, "is": true, "isnull": true, "join": true, "left": true,
	"like": true, "natural": true, "notnull": true, "outer": true, "overlaps": true, "right": true, "similar": true,
	"tablesample": true, "verbose": true,
}

// supportedFunctions, which the executor binds.
var supportedFunctions = map[string]bool{"length": true, "lower": true, "upper": true, "abs": true, "coalesce": true, "now": true, "transaction_timestamp": true}

// unsupportedFunctions have a message with what to write instead.
var unsupportedFunctions = map[string]string{
	"statement_timestamp": "function statement_timestamp is not supported, write now(), which is the start of the transaction",
	"clock_timestamp":     "function clock_timestamp is not supported, write now(), which is the start of the transaction",
	"timeofday":           "function timeofday is not supported, write now(), which is the start of the transaction",
	"nextval":             "function nextval is not supported, because there are no sequences; generate the keys in the application",
	"currval":             "function currval is not supported, because there are no sequences",
	"setval":              "function setval is not supported, because there are no sequences",
	"lastval":             "function lastval is not supported, because there are no sequences",
}

func (p *parser) primary() (ast.Expr, error) {
	tok := p.tok
	switch tok.kind {
	case tokNumber:
		return p.number("")
	case tokString:
		return &ast.Literal{Value: value.NewUnknown(tok.text)}, p.advance()
	case tokParam:
		n, err := strconv.Atoi(tok.text)
		if err != nil || n < 1 {
			return nil, p.errorf("there is no parameter $%s", tok.text)
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
		case "default":
			// DEFAULT is an expression only where it stands for a column's default value, as a
			// whole item of VALUES or of SET, which the executor checks, as Postgres does.
			return &ast.Default{}, p.advance()
		case "array":
			return nil, p.unsupportedf("arrays are not supported")
		case "current_timestamp":
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.isOp("(") {
				return nil, p.unsupportedf("CURRENT_TIMESTAMP with a precision is not supported, write CURRENT_TIMESTAMP")
			}
			return &ast.Call{Name: tok.text, Keyword: true}, nil
		case "current_date", "current_time", "localtime", "localtimestamp":
			return nil, p.unsupportedf("%s is not supported, because its type is not; write CURRENT_TIMESTAMP or now()", strings.ToUpper(tok.text))
		case "current_user", "current_role", "current_catalog", "current_schema", "session_user", "system_user", "user":
			return nil, p.unsupportedf("%s is not supported, pass the value as a parameter", strings.ToUpper(tok.text))
		case "select", "with":
			return nil, p.unsupportedf("subqueries are not supported")
		case "any", "all", "some":
			return nil, p.unsupportedf("%s is not supported, write IN or several comparisons", strings.ToUpper(tok.text))
		}
		// Postgres's type_func_name keywords can name a function, such as left(s, 1).
		if typeFuncNameKeywords[tok.text] && p.peek().kind == tokOp && p.peek().text == "(" {
			return nil, p.unsupportedf("function %s is not supported", tok.text)
		}
	case tokIdent:
		if p.peek().kind == tokOp && p.peek().text == "." {
			return p.qualified()
		}
		if tok.quoted {
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.isOp("(") {
				return p.call(tok.text, true)
			}
			return &ast.ColumnRef{Name: tok.text}, nil
		}
		switch tok.text {
		case "exists":
			if p.peek().kind == tokOp && p.peek().text == "(" {
				return nil, p.unsupportedf("subqueries are not supported")
			}
		case "row":
			if p.peek().kind == tokOp && p.peek().text == "(" {
				return nil, p.unsupportedf("row constructors are not supported")
			}
		case "interval", "date", "time", "timestamp", "numeric", "decimal", "int", "integer", "varchar", "char", "character", "real", "smallint", "int4", "int2", "float4", "json", "jsonb", "uuid",
			"bigint", "int8", "float8", "float", "boolean", "bool", "text", "bytea", "timestamptz", "double":
			next := p.peek()
			if next.kind == tokString || (tok.text == "double" && next.text == "precision") || ((tok.text == "timestamp" || tok.text == "time") && (next.text == "with" || next.text == "without")) {
				return p.prefixLiteral()
			}
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.isOp("(") {
			return p.call(tok.text, false)
		}
		return &ast.ColumnRef{Name: tok.text}, nil
	case tokOp:
		if tok.text == "(" {
			defer p.unrestricted()()
			if err := p.advance(); err != nil {
				return nil, err
			}
			if p.isKeyword("select") || p.isKeyword("with") || p.isKeyword("values") {
				return nil, p.unsupportedf("subqueries are not supported")
			}
			e, err := p.expr(0)
			if err != nil {
				return nil, err
			}
			if p.isOp(",") {
				return nil, p.unsupportedf("row constructors are not supported")
			}
			if err := p.expectOp(")"); err != nil {
				return nil, err
			}
			return e, nil
		}
	}
	return nil, p.errorf("expected an expression, got %v", tok)
}

// qualified parses a column qualified by the name of a table, table.column.
func (p *parser) qualified() (ast.Expr, error) {
	ref := &ast.ColumnRef{Table: p.tok.text}
	for range 2 {
		if err := p.advance(); err != nil {
			return nil, err
		}
	}
	if p.isOp("*") {
		return nil, p.unsupportedf("%s.* is not supported in an expression, name the columns", ref.Table)
	}
	var err error
	if ref.Name, err = p.label(); err != nil {
		return nil, err
	}
	switch {
	case p.isOp("."):
		return nil, p.unsupportedf("names of more than two parts, such as schema.table.column, are not supported")
	case p.isOp("("):
		return nil, p.unsupportedf("functions with a schema are not supported")
	}
	return ref, nil
}

// prefixLiteral parses `type 'string'`, which is a cast of the string.
func (p *parser) prefixLiteral() (ast.Expr, error) {
	t, err := p.typeName()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokString {
		return nil, p.errorf("expected a string, got %v", p.tok)
	}
	lit := &ast.Literal{Value: value.NewUnknown(p.tok.text)}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return &ast.Cast{Expr: lit, Type: t, Style: ast.CastPrefix}, nil
}

// call parses the arguments of a function call, after its name. A quoted name is never one of
// the Postgres functions that are reported as unsupported.
func (p *parser) call(name string, quoted bool) (ast.Expr, error) {
	defer p.unrestricted()()
	if err := p.advance(); err != nil {
		return nil, err
	}
	// Postgres has far more functions than the list above, so any other name is unsupported too,
	// rather than a function that does not exist.
	if msg, ok := unsupportedFunctions[name]; ok {
		return nil, p.unsupportedf("%s", msg)
	}
	if !supportedFunctions[name] {
		return nil, p.unsupportedf("function %s is not supported", name)
	}
	if quoted && name == "coalesce" {
		// COALESCE is a construct of the grammar, so a quoted name is an ordinary function, which
		// Postgres does not have.
		return nil, fmt.Errorf("%w at position %d: function \"coalesce\" does not exist", ErrInvalid, p.tok.pos+1)
	}
	var args []ast.Expr
	if !p.isOp(")") {
		if p.isKeyword("distinct") || p.isKeyword("all") {
			return nil, p.unsupportedf("aggregate functions are not supported")
		}
		if p.isOp("*") {
			return nil, p.unsupportedf("aggregate functions are not supported")
		}
		var err error
		if args, err = p.exprList(); err != nil {
			return nil, err
		}
		if p.isKeyword("order") {
			return nil, p.unsupportedf("aggregate functions are not supported")
		}
	}
	if err := p.expectOp(")"); err != nil {
		return nil, err
	}
	if p.isWord("filter") || p.isWord("within") || p.isWord("over") {
		return nil, p.unsupportedf("%s", unsupportedClauses[p.tok.text])
	}
	return &ast.Call{Name: name, Args: args}, nil
}

func (p *parser) cast() (ast.Expr, error) {
	defer p.unrestricted()()
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
	defer p.unrestricted()()
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
