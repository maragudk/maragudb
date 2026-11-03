package parser

import (
	"fmt"
	"strings"
	"unicode"
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
	tokBytes
	tokParam
	tokOp
)

type token struct {
	kind tokenKind
	// text is the lowercased identifier or keyword, the number as written, the string's contents,
	// the hex digits of a bytes literal, the parameter number, or the operator.
	text string
	pos  int
}

func (t token) String() string {
	switch t.kind {
	case tokEOF:
		return "end of statement"
	case tokString:
		return fmt.Sprintf("string %q", t.text)
	case tokBytes:
		return fmt.Sprintf("bytes X'%s'", t.text)
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

func (l *lexer) skipSpace() error {
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case strings.HasPrefix(l.src[l.pos:], "--"):
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
		case strings.HasPrefix(l.src[l.pos:], "/*"):
			end := strings.Index(l.src[l.pos+2:], "*/")
			if end < 0 {
				return l.errorf(l.pos, "unterminated comment")
			}
			l.pos += end + 4
		default:
			return nil
		}
	}
	return nil
}

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
		return token{kind: tokString, text: s, pos: start}, nil
	case (c == 'x' || c == 'X') && l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'':
		l.pos++
		s, err := l.string()
		if err != nil {
			return token{}, err
		}
		if len(s)%2 != 0 || strings.ContainsFunc(s, func(r rune) bool { return !isHex(r) }) {
			return token{}, l.errorf(start, "bytes literal needs an even number of hex digits")
		}
		return token{kind: tokBytes, text: s, pos: start}, nil
	case c == '$':
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
	}
	for _, op := range []string{"||", "<>", "!=", "<=", ">=", "<", ">", "=", "+", "-", "*", "/", "%", "(", ")", ",", ";"} {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.pos += len(op)
			if op == "!=" {
				op = "<>"
			}
			return token{kind: tokOp, text: op, pos: start}, nil
		}
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
		return token{}, l.errorf(start, "empty quoted identifier")
	}
	if !utf8.ValidString(b.String()) || strings.IndexByte(b.String(), 0) >= 0 {
		return token{}, l.errorf(start, "quoted identifier must be valid UTF-8 without NUL bytes")
	}
	return token{kind: tokIdent, text: b.String(), pos: start}, nil
}

func (l *lexer) number() (token, error) {
	start := l.pos
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		digits := l.pos
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
		if l.pos == digits {
			return token{}, l.errorf(start, "exponent needs digits")
		}
	}
	if l.pos < len(l.src) && isIdentStart(l.src[l.pos]) {
		return token{}, l.errorf(start, "invalid number")
	}
	return token{kind: tokNumber, text: l.src[start:l.pos], pos: start}, nil
}

func isDigit(c byte) bool      { return c >= '0' && c <= '9' }
func isIdentStart(c byte) bool { return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isIdentPart(c byte) bool  { return isIdentStart(c) || isDigit(c) }
func isHex(r rune) bool        { return unicode.Is(unicode.ASCII_Hex_Digit, r) }
