package sqllogictest

import (
	"strings"
	"testing"

	"maragu.dev/is"
)

func TestParse(t *testing.T) {
	t.Run("parses statements and queries and drops the recorded results", func(t *testing.T) {
		records, err := parse(strings.NewReader(`hash-threshold 8

statement ok
CREATE TABLE t1(a INTEGER,
  b TEXT)

# a comment
query I rowsort label-1
SELECT a
  FROM t1
----
1
2

query T nosort
SELECT b FROM t1
----
3 values hashing to 0123456789abcdef

statement error
INSERT INTO t1 VALUES(1)
`))
		is.NotError(t, err)
		is.Equal(t, 4, len(records))
		is.Equal(t, kindStatement, records[0].kind)
		is.Equal(t, "CREATE TABLE t1(a INTEGER,\n  b TEXT)", records[0].sql)
		is.Equal(t, 3, records[0].line)
		is.Equal(t, kindQuery, records[1].kind)
		is.Equal(t, "SELECT a\n  FROM t1", records[1].sql)
		is.Equal(t, 8, records[1].line)
		is.Equal(t, "SELECT b FROM t1", records[2].sql)
		is.Equal(t, kindStatement, records[3].kind)
		is.Equal(t, "INSERT INTO t1 VALUES(1)", records[3].sql)
	})

	t.Run("keeps the conditions of a record, without their comments", func(t *testing.T) {
		records, err := parse(strings.NewReader(`skipif mysql # no reason
onlyif postgresql
query I
SELECT 1
----
1

onlyif mssql
halt
`))
		is.NotError(t, err)
		is.Equal(t, 2, len(records))
		is.EqualSlice(t, []condition{{skip: true, engine: "mysql"}, {engine: "postgresql"}}, records[0].conditions)
		is.Equal(t, kindHalt, records[1].kind)
		is.EqualSlice(t, []condition{{engine: "mssql"}}, records[1].conditions)
	})

	t.Run("reads a query without results and a file without a final newline", func(t *testing.T) {
		records, err := parse(strings.NewReader("query I\nSELECT 1\n\nstatement ok\nDROP TABLE t1"))
		is.NotError(t, err)
		is.Equal(t, 2, len(records))
		is.Equal(t, "SELECT 1", records[0].sql)
		is.Equal(t, "DROP TABLE t1", records[1].sql)
	})

	t.Run("fails on an unknown record type and on a record without SQL", func(t *testing.T) {
		_, err := parse(strings.NewReader("statement ok\nSELECT 1\n\nfrobnicate\n"))
		is.True(t, err != nil && strings.Contains(err.Error(), "line 4"), "unknown record type")
		_, err = parse(strings.NewReader("query I\n----\n1\n"))
		is.True(t, err != nil && strings.Contains(err.Error(), "line 1"), "query without SQL")
		_, err = parse(strings.NewReader("skipif mysql\n\nstatement ok\nSELECT 1\n"))
		is.True(t, err != nil, "condition without a record")
	})
}

func TestRecord_appliesTo(t *testing.T) {
	tests := []struct {
		name       string
		conditions []condition
		want       bool
	}{
		{name: "applies without conditions", want: true},
		{name: "does not apply when only for another engine", conditions: []condition{{engine: "mysql"}}, want: false},
		{name: "applies when only for this engine", conditions: []condition{{engine: "postgresql"}}, want: true},
		{name: "applies when skipped for another engine", conditions: []condition{{skip: true, engine: "mysql"}}, want: true},
		{name: "does not apply when skipped for this engine", conditions: []condition{{skip: true, engine: "postgresql"}}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.Equal(t, test.want, record{conditions: test.conditions}.appliesTo("postgresql"))
		})
	}
}
