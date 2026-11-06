package sqllogictest

import (
	"strings"
)

// rewrite the records of a file, once, before they run. The same rewritten text then runs on
// both databases, so a rewrite can never hide a difference between them; it only decides how
// much of the corpus maragudb can run. These are all the rewrites:
//
//   - Column types and the target types of CAST become the supported types: INTEGER and INT
//     become bigint; FLOAT, REAL, and DOUBLE PRECISION become double precision; and TEXT,
//     VARCHAR(n), CHAR(n), and CHARACTER(n) become text.
//   - A CREATE TABLE without a primary key gets one over all of its columns, because maragudb
//     needs a primary key in every table.
//   - INSERT INTO a SELECT * FROM b becomes INSERT INTO a VALUES with the rows of every earlier
//     INSERT INTO b VALUES, when the tables have the same column definitions and b was written
//     only by those inserts since it was created, because maragudb has no INSERT ... SELECT.
//     The rows are those of the text, so a row whose insert failed makes the expanded insert
//     fail as a whole. That only costs coverage, which the setup failures in the report show.
//
// It returns the records to run.
func rewrite(records []record) []record {
	var out []record
	tables := map[string]*tableValues{}
	for _, r := range records {
		if r.kind == kindHalt {
			out = append(out, r)
			continue
		}
		toks := lex(r.sql)
		words := keywords(toks)
		toks = rewriteTypes(toks)
		if isCreateTable(words) {
			toks = addPrimaryKey(toks)
		}
		r.sql = join(toks)
		if r.kind == kindStatement {
			r.sql = expandInsertSelect(r.sql, toks, tables)
		}
		out = append(out, r)
	}
	return out
}

// tableValues are the VALUES lists that filled a table since its CREATE TABLE, when nothing
// else wrote to it.
type tableValues struct {
	// columns is the text of the column definitions.
	columns string
	values  []string
	// clean reports whether only INSERT ... VALUES wrote to the table.
	clean bool
}

// expandInsertSelect tracks what writes each table, and returns the statement with an INSERT
// ... SELECT * replaced by the source table's values, when they are known.
func expandInsertSelect(sql string, toks []token, tables map[string]*tableValues) string {
	sig := significantIndexes(toks)
	words := make([]token, len(sig))
	for i, j := range sig {
		words[i] = toks[j]
	}
	is := func(i int, s string) bool { return i < len(words) && strings.EqualFold(words[i].text, s) }
	name := func(i int) string {
		if i < len(words) && words[i].kind == tokWord {
			return strings.ToLower(words[i].text)
		}
		return ""
	}
	switch {
	case is(0, "CREATE") && is(1, "TABLE") && name(2) != "":
		open := indexOf(toks, "(")
		closing := matching(toks, open)
		if open < 0 || closing < 0 {
			delete(tables, name(2))
			return sql
		}
		tables[name(2)] = &tableValues{columns: strings.TrimSpace(join(toks[open+1 : closing])), clean: true}
	case is(0, "INSERT") && is(1, "INTO") && is(3, "SELECT") && is(4, "*") && is(5, "FROM") && name(6) != "" && len(words) == 7:
		target, source := tables[name(2)], tables[name(6)]
		if target == nil || source == nil || !source.clean || !target.clean || len(source.values) == 0 || target.columns != source.columns {
			markWritten(tables, name(2))
			return sql
		}
		// The statement keeps its own text up to the target table.
		head := join(toks[:sig[3]])
		target.values = append(target.values, source.values...)
		return head + "VALUES " + strings.Join(source.values, ", ")
	case is(0, "INSERT") && is(1, "INTO") && is(3, "VALUES"):
		t := tables[name(2)]
		if t == nil {
			return sql
		}
		if words[len(words)-1].text == ";" {
			t.clean = false
			return sql
		}
		start := sig[3] + 1
		t.values = append(t.values, strings.TrimSpace(join(toks[start:])))
	case is(0, "INSERT") && is(1, "INTO"), is(0, "REPLACE") && is(1, "INTO"), is(0, "UPDATE"):
		target := name(2)
		if is(0, "UPDATE") {
			target = name(1)
		}
		markWritten(tables, target)
	case is(0, "DELETE") && is(1, "FROM"):
		markWritten(tables, name(2))
	case is(0, "DROP") && is(1, "TABLE") && is(2, "IF") && is(3, "EXISTS"):
		delete(tables, name(4))
	case is(0, "DROP") && is(1, "TABLE"):
		delete(tables, name(2))
	}
	return sql
}

func markWritten(tables map[string]*tableValues, name string) {
	if t := tables[name]; t != nil {
		t.clean = false
	}
}

func isCreateTable(words []string) bool {
	return len(words) >= 2 && words[0] == "CREATE" && words[1] == "TABLE"
}

// typeNames maps the type names of the corpus to supported ones. A name in the map with an
// empty value is the start of a two-word name.
var typeNames = map[string]string{
	"INTEGER":   "bigint",
	"INT":       "bigint",
	"FLOAT":     "double precision",
	"REAL":      "double precision",
	"DOUBLE":    "double precision",
	"TEXT":      "text",
	"VARCHAR":   "text",
	"CHAR":      "text",
	"CHARACTER": "text",
}

// rewriteTypes replaces the type of every column definition in a CREATE TABLE and the target
// type of every CAST.
func rewriteTypes(toks []token) []token {
	words := keywords(toks)
	createTable := isCreateTable(words)
	// frames holds, for every open parenthesis, whether it is the column list of the CREATE
	// TABLE or a CAST.
	type frame struct{ columns, cast bool }
	var frames []frame
	var out []token
	// expectType is set where the next word is a type: after a column name, and after the AS of
	// a CAST.
	expectType := false
	// elementStart is set at the start of an element of the column list.
	elementStart := false
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		switch {
		case tok.kind == tokSpace || tok.kind == tokComment:
			out = append(out, tok)
			continue
		case tok.text == "(":
			f := frame{}
			if prev := lastSignificant(out); prev != nil && strings.EqualFold(prev.text, "CAST") {
				f.cast = true
			}
			if createTable && len(frames) == 0 && !f.cast {
				f.columns = true
				elementStart = true
			}
			frames = append(frames, f)
			out = append(out, tok)
			expectType = false
			continue
		case tok.text == ")":
			if len(frames) > 0 {
				frames = frames[:len(frames)-1]
			}
		case tok.text == "," && len(frames) == 1 && frames[0].columns:
			elementStart = true
			out = append(out, tok)
			expectType = false
			continue
		}
		if expectType && tok.kind == tokWord {
			expectType = false
			if to, ok := typeNames[strings.ToUpper(tok.text)]; ok {
				i = skipTypeTail(toks, i)
				out = append(out, token{kind: tokWord, text: to})
				continue
			}
		}
		expectType = false
		if elementStart && (tok.kind == tokWord || tok.kind == tokQuoted) {
			elementStart = false
			switch strings.ToUpper(tok.text) {
			case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			default:
				expectType = true
			}
		} else if tok.kind != tokSpace {
			elementStart = false
		}
		if tok.kind == tokWord && strings.EqualFold(tok.text, "AS") && len(frames) > 0 && frames[len(frames)-1].cast {
			expectType = true
		}
		out = append(out, tok)
	}
	return out
}

// skipTypeTail returns the index of the last token of the type that starts at toks[i]: the
// second word of DOUBLE PRECISION and CHARACTER VARYING, and a length in parentheses.
func skipTypeTail(toks []token, i int) int {
	next := func(j int) int {
		for j++; j < len(toks) && (toks[j].kind == tokSpace || toks[j].kind == tokComment); j++ {
		}
		return j
	}
	first := strings.ToUpper(toks[i].text)
	if j := next(i); j < len(toks) && toks[j].kind == tokWord {
		second := strings.ToUpper(toks[j].text)
		if first == "DOUBLE" && second == "PRECISION" || first == "CHARACTER" && second == "VARYING" {
			i = j
		}
	}
	switch first {
	case "VARCHAR", "CHAR", "CHARACTER":
		if j := next(i); j < len(toks) && toks[j].text == "(" {
			if k := matching(toks, j); k > 0 {
				i = k
			}
		}
	}
	return i
}

// addPrimaryKey adds a primary key over all columns to a CREATE TABLE without one.
func addPrimaryKey(toks []token) []token {
	open := indexOf(toks, "(")
	closing := matching(toks, open)
	if open < 0 || closing < 0 {
		return toks
	}
	var columns []string
	depth := 0
	elementStart := true
	for i := open + 1; i < closing; i++ {
		tok := toks[i]
		switch {
		case tok.kind == tokSpace || tok.kind == tokComment:
			continue
		case tok.text == "(":
			depth++
		case tok.text == ")":
			depth--
		case tok.text == "," && depth == 0:
			elementStart = true
			continue
		case tok.kind == tokWord && strings.EqualFold(tok.text, "PRIMARY"):
			return toks
		}
		if elementStart && (tok.kind == tokWord || tok.kind == tokQuoted) {
			switch strings.ToUpper(tok.text) {
			case "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			default:
				columns = append(columns, tok.text)
			}
		}
		elementStart = false
	}
	if len(columns) == 0 {
		return toks
	}
	out := append([]token{}, toks[:closing]...)
	out = append(out, token{kind: tokOther, text: ", PRIMARY KEY (" + strings.Join(columns, ", ") + ")"})
	return append(out, toks[closing:]...)
}

// tokenKind of a [token].
type tokenKind int

const (
	tokSpace tokenKind = iota
	tokComment
	tokWord
	tokNumber
	tokString
	tokQuoted
	tokOther
)

// token of SQL text, which keeps the text exactly as written, so that joining the tokens gives
// the text back.
type token struct {
	kind tokenKind
	text string
}

// lex SQL text into tokens. It knows just enough SQL to find words outside of strings, quoted
// identifiers, and comments.
func lex(s string) []token {
	var toks []token
	for i := 0; i < len(s); {
		c := s[i]
		start := i
		var k tokenKind
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
				i++
			}
			k = tokSpace
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			k = tokComment
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				i = len(s)
			} else {
				i += end + 4
			}
			k = tokComment
		case c == '\'' || c == '"' || c == '`':
			i++
			for i < len(s) {
				if s[i] == c {
					if i+1 < len(s) && s[i+1] == c {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			k = tokString
			if c != '\'' {
				k = tokQuoted
			}
		case isWordStart(c):
			for i < len(s) && isWordPart(s[i]) {
				i++
			}
			k = tokWord
		case c >= '0' && c <= '9' || c == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9':
			for i < len(s) && (isWordPart(s[i]) || s[i] == '.') {
				i++
			}
			k = tokNumber
		default:
			i++
			k = tokOther
		}
		toks = append(toks, token{kind: k, text: s[start:i]})
	}
	return toks
}

func isWordStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= 0x80
}

func isWordPart(c byte) bool {
	return isWordStart(c) || c >= '0' && c <= '9' || c == '$'
}

func join(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.text)
	}
	return b.String()
}

// significantIndexes are the indexes of the tokens that are not spaces or comments.
func significantIndexes(toks []token) []int {
	var out []int
	for i, t := range toks {
		if t.kind != tokSpace && t.kind != tokComment {
			out = append(out, i)
		}
	}
	return out
}

// keywords are the first three significant tokens, in upper case.
func keywords(toks []token) []string {
	var out []string
	for _, i := range significantIndexes(toks) {
		if len(out) == 3 {
			break
		}
		out = append(out, strings.ToUpper(toks[i].text))
	}
	return out
}

func lastSignificant(toks []token) *token {
	for i := len(toks) - 1; i >= 0; i-- {
		if toks[i].kind != tokSpace && toks[i].kind != tokComment {
			return &toks[i]
		}
	}
	return nil
}

// indexOf the first token with the text, or -1.
func indexOf(toks []token, text string) int {
	for i, t := range toks {
		if t.kind == tokOther && t.text == text {
			return i
		}
	}
	return -1
}

// matching is the index of the parenthesis that closes the one at open, or -1.
func matching(toks []token, open int) int {
	if open < 0 {
		return -1
	}
	depth := 0
	for i := open; i < len(toks); i++ {
		switch {
		case toks[i].kind != tokOther:
		case toks[i].text == "(":
			depth++
		case toks[i].text == ")":
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
