// Package sqllogictest runs the SQLite sqllogictest corpus as a differential test between
// maragudb and Postgres 18. The corpus is only a source of SQL: its recorded results are never
// read, and Postgres is the oracle. Every statement runs on both databases, through the
// comparison of the pgdiff package, after a few rewrites of the text that [rewrite] lists.
//
// The corpus is used under the MIT option of its COPYRIGHT.md. It is not part of the
// repository: [corpusDir] downloads a pinned commit of a mirror into the user cache directory
// and checks every file against the committed manifest.
package sqllogictest

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// kind of a record.
type kind int

const (
	kindStatement kind = iota
	kindQuery
	kindHalt
)

// condition of a record: onlyif engine, or skipif engine when skip is set.
type condition struct {
	skip   bool
	engine string
}

// record of a test file: a statement or a query, without its expected outcome, or a halt.
type record struct {
	kind       kind
	sql        string
	line       int
	conditions []condition
}

// appliesTo reports whether the record's conditions select the engine.
func (r record) appliesTo(engine string) bool {
	for _, c := range r.conditions {
		if c.skip == (c.engine == engine) {
			return false
		}
	}
	return true
}

// parse the records of a test file in the sqllogictest format. Expected results, hashes,
// hash-threshold lines, and comments are dropped.
func parse(r io.Reader) ([]record, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var records []record
	var conditions []condition
	lineNo := 0
	next := func() (string, bool) {
		if !scanner.Scan() {
			return "", false
		}
		lineNo++
		return strings.TrimRight(scanner.Text(), "\r"), true
	}
	for {
		line, ok := next()
		if !ok {
			break
		}
		if line == "" {
			if len(conditions) > 0 {
				return nil, fmt.Errorf("line %d: a condition without a record", lineNo)
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		start := lineNo
		switch fields[0] {
		case "skipif", "onlyif":
			if len(fields) != 2 {
				return nil, fmt.Errorf("line %d: %s needs one engine", lineNo, fields[0])
			}
			conditions = append(conditions, condition{skip: fields[0] == "skipif", engine: fields[1]})
			continue
		case "hash-threshold":
			continue
		case "halt":
			records = append(records, record{kind: kindHalt, line: start, conditions: conditions})
			conditions = nil
			continue
		case "statement", "query":
		default:
			return nil, fmt.Errorf("line %d: unknown record type %q", lineNo, fields[0])
		}
		k := kindStatement
		if fields[0] == "query" {
			k = kindQuery
		}
		// The SQL runs to the end of the record, or for a query to the line before its results.
		var sql []string
		inResults := false
		for {
			line, ok := next()
			if !ok || line == "" {
				break
			}
			if inResults {
				continue
			}
			if k == kindQuery && line == "----" {
				inResults = true
				continue
			}
			sql = append(sql, line)
		}
		if len(sql) == 0 {
			return nil, fmt.Errorf("line %d: %s without SQL", start, fields[0])
		}
		records = append(records, record{kind: k, sql: strings.Join(sql, "\n"), line: start, conditions: conditions})
		conditions = nil
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(conditions) > 0 {
		return nil, fmt.Errorf("line %d: a condition without a record", lineNo)
	}
	return records, nil
}
