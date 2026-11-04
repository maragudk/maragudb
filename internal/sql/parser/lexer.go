package parser

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"maragu.dev/maragudb/internal/sql/ast"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokKeyword
	tokNumber
	tokString
	tokParam
	tokOp
)

type token struct {
	kind tokenKind
	// text is the lowercased identifier or keyword, the number as written, the string's contents,
	// the parameter number, or the operator.
	text string
	pos  int
	// quoted is set for a quoted identifier, which is never a keyword.
	quoted bool
}

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of statement"
	case tokString:
		return fmt.Sprintf("string %q", t.text)
	case tokParam:
		return "$" + t.text
	case tokKeyword:
		return strings.ToUpper(t.text)
	}
	return fmt.Sprintf("%q", t.text)
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) errorf(pos int, format string, args ...any) error {
	return fmt.Errorf("%w at position %d: %s", ErrSyntax, pos+1, fmt.Sprintf(format, args...))
}

func (l *lexer) unsupportedf(pos int, format string, args ...any) error {
	return fmt.Errorf("%w at position %d: %s", ErrUnsupported, pos+1, fmt.Sprintf(format, args...))
}

func (l *lexer) skipSpace() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "--"):
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			// Block comments nest, as in Postgres.
			start := l.pos
			depth := 0
			for depth > 0 || l.pos == start {
				switch {
				case l.pos >= len(l.src):
					return l.errorf(start, "unterminated comment")
				case strings.HasPrefix(l.src[l.pos:], "/*"):
					depth++
					l.pos += 2
				case strings.HasPrefix(l.src[l.pos:], "*/"):
					depth--
					l.pos += 2
				default:
					l.pos++
				}
			}
		default:
			return nil
		}
	}
	return nil
}

// operators that the parser understands. Any other run of operator characters is a Postgres
// operator that is not supported.
var operators = []string{"||", "<>", "<=", ">=", "<", ">", "=", "+", "-", "*", "/", "%"}

const operatorChars = "+-*/<>=~!@#%^&|`?"

func (l *lexer) next() (token, error) {
	if err := l.skipSpace(); err != nil {
		return token{}, err
	}
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: l.pos}, nil
	}
	start := l.pos
	c := l.src[l.pos]
	switch {
	case c == '\'':
		s, err := l.string()
		if err != nil {
			return token{}, err
		}
		if err := l.checkContinuation(start); err != nil {
			return token{}, err
		}
		return token{kind: tokString, text: s, pos: start}, nil
	case (c == 'e' || c == 'E' || c == 'b' || c == 'B' || c == 'x' || c == 'X' || c == 'n' || c == 'N') && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'':
		switch c {
		case 'e', 'E':
			return token{}, l.unsupportedf(start, "escape string constants E'...' are not supported, write the string with standard quotes")
		case 'b', 'B', 'x', 'X':
			return token{}, l.unsupportedf(start, "bit string constants are not supported, write bytes as a bytea such as '\\x01'::bytea")
		}
		return token{}, l.unsupportedf(start, "national character string constants are not supported, write the string with standard quotes")
	case (c == 'u' || c == 'U') && strings.HasPrefix(l.src[l.pos+1:], "&'"):
		return token{}, l.unsupportedf(start, "Unicode escape string constants are not supported, write the characters directly")
	case c == '$':
		if strings.HasPrefix(l.src[l.pos:], "$$") || (l.pos+1 < len(l.src) && isIdentStart(l.src[l.pos+1])) {
			return token{}, l.unsupportedf(start, "dollar-quoted string constants are not supported, write the string with standard quotes")
		}
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == start+1 {
			return token{}, l.errorf(start, "parameter needs a number")
		}
		return token{kind: tokParam, text: l.src[start+1 : l.pos], pos: start}, nil
	case isDigit(c) || (c == '.' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1])):
		return l.number()
	case isIdentStart(c):
		for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
			l.pos++
		}
		text := strings.ToLower(l.src[start:l.pos])
		if ast.Keywords[text] {
			return token{kind: tokKeyword, text: text, pos: start}, nil
		}
		return token{kind: tokIdent, text: text, pos: start}, nil
	case c == '"':
		return l.quotedIdent()
	case c >= 0x80:
		r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
		if r == utf8.RuneError {
			return token{}, l.errorf(start, "invalid UTF-8")
		}
		return token{}, l.unsupportedf(start, "identifiers with non-ASCII letters such as %q are not supported unquoted, quote the identifier", r)
	}
	for _, op := range []string{"(", ")", ",", ";"} {
		if c == op[0] {
			l.pos++
			return token{kind: tokOp, text: op, pos: start}, nil
		}
	}
	if c == '.' {
		return token{}, l.unsupportedf(start, "qualified names such as table.column are not supported, name the column alone")
	}
	if c == '[' || c == ']' {
		return token{}, l.unsupportedf(start, "arrays are not supported")
	}
	if strings.IndexByte(operatorChars, c) >= 0 {
		end := l.pos
		for end < len(l.src) && strings.IndexByte(operatorChars, l.src[end]) >= 0 && !strings.HasPrefix(l.src[end:], "--") && !strings.HasPrefix(l.src[end:], "/*") {
			end++
		}
		// A run of operator characters is one operator, except that trailing + or - signs are
		// split off unless the run contains one of ~ ! @ # % ^ & | ` ?, as in Postgres.
		run := l.src[l.pos:end]
		for len(run) > 1 && (run[len(run)-1] == '+' || run[len(run)-1] == '-') && !strings.ContainsAny(run, "~!@#%^&|`?") {
			run = run[:len(run)-1]
		}
		l.pos += len(run)
		if run == "!=" {
			run = "<>"
		}
		if slices.Contains(operators, run) {
			return token{kind: tokOp, text: run, pos: start}, nil
		}
		return token{}, l.unsupportedf(start, "operator %s is not supported", run)
	}
	if c == ':' {
		if strings.HasPrefix(l.src[l.pos:], "::") {
			l.pos += 2
			return token{kind: tokOp, text: "::", pos: start}, nil
		}
		return token{}, l.errorf(start, "unexpected character ':'")
	}
	r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
	return token{}, l.errorf(start, "unexpected character %q", r)
}

func (l *lexer) string() (string, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return "", l.errorf(start, "unterminated string")
		}
		c := l.src[l.pos]
		if c == '\'' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				b.WriteByte('\'')
				l.pos += 2
				continue
			}
			l.pos++
			break
		}
		b.WriteByte(c)
		l.pos++
	}
	s := b.String()
	if !utf8.ValidString(s) {
		return "", l.errorf(start, "string is not valid UTF-8")
	}
	return s, nil
}

// checkContinuation rejects two string constants separated only by whitespace and -- comments
// that include a newline, which Postgres joins into one.
func (l *lexer) checkContinuation(start int) error {
	i := l.pos
	newline := false
scan:
	for i < len(l.src) {
		switch c := l.src[i]; {
		case c == ' ' || c == '\t' || c == '\f' || c == '\v':
			i++
		case c == '\n' || c == '\r':
			newline = true
			i++
		case strings.HasPrefix(l.src[i:], "--"):
			for i < len(l.src) && l.src[i] != '\n' && l.src[i] != '\r' {
				i++
			}
		default:
			break scan
		}
	}
	if newline && i < len(l.src) && l.src[i] == '\'' {
		return l.unsupportedf(start, "string constants continued on the next line are not supported, write one string")
	}
	return nil
}

func (l *lexer) quotedIdent() (token, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return token{}, l.errorf(start, "unterminated quoted identifier")
		}
		c := l.src[l.pos]
		if c == '"' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '"' {
				b.WriteByte('"')
				l.pos += 2
				continue
			}
			l.pos++
			break
		}
		b.WriteByte(c)
		l.pos++
	}
	if b.Len() == 0 {
		return token{}, l.errorf(start, "zero-length delimited identifier")
	}
	if !utf8.ValidString(b.String()) || strings.IndexByte(b.String(), 0) >= 0 {
		return token{}, l.errorf(start, "quoted identifier must be valid UTF-8 without NUL bytes")
	}
	return token{kind: tokIdent, text: b.String(), pos: start, quoted: true}, nil
}

// number lexes a numeric constant as Postgres does: decimal digits with an optional fraction and
// exponent, or an integer with a 0x, 0o, or 0b prefix, with single underscores between digits.
func (l *lexer) number() (token, error) {
	start := l.pos
	// digits scans digits with single underscores between them. After a base prefix, an
	// underscore may also come first, as in 0x_1F.
	digits := func(valid func(byte) bool, prefixed bool) bool {
		n := 0
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if c == '_' && (n > 0 || prefixed) && l.pos+1 < len(l.src) && valid(l.src[l.pos+1]) {
				l.pos++
				continue
			}
			if !valid(c) {
				break
			}
			l.pos++
			n++
		}
		return n > 0
	}
	if l.src[l.pos] == '0' && l.pos+1 < len(l.src) {
		var valid func(byte) bool
		switch l.src[l.pos+1] {
		case 'x', 'X':
			valid = isHexDigit
		case 'o', 'O':
			valid = func(c byte) bool { return c >= '0' && c <= '7' }
		case 'b', 'B':
			valid = func(c byte) bool { return c == '0' || c == '1' }
		}
		if valid != nil {
			l.pos += 2
			if !digits(valid, true) {
				return token{}, l.errorf(start, "invalid number")
			}
			if l.pos < len(l.src) && (isIdentStart(l.src[l.pos]) || isDigit(l.src[l.pos])) {
				return token{}, l.errorf(start, "trailing junk after numeric literal")
			}
			return token{kind: tokNumber, text: l.src[start:l.pos], pos: start}, nil
		}
	}
	digits(isDigit, false)
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		digits(isDigit, false)
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		if !digits(isDigit, false) {
			return token{}, l.errorf(start, "trailing junk after numeric literal")
		}
	}
	if l.pos < len(l.src) && isIdentStart(l.src[l.pos]) {
		return token{}, l.errorf(start, "trailing junk after numeric literal")
	}
	return token{kind: tokNumber, text: l.src[start:l.pos], pos: start}, nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isHexDigit(c byte) bool   { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) || c == '$' }
