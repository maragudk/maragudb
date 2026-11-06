package sqllogictest

import (
	"testing"

	"maragu.dev/is"
)

func TestRewrite(t *testing.T) {
	statements := func(sqls ...string) []record {
		var rs []record
		for i, s := range sqls {
			rs = append(rs, record{kind: kindStatement, sql: s, line: i + 1})
		}
		return rs
	}
	texts := func(rs []record) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.sql)
		}
		return out
	}

	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "rewrites the column types to the supported ones",
			in:   []string{"CREATE TABLE tab0(pk INTEGER PRIMARY KEY, col0 INTEGER, col1 FLOAT, col2 TEXT, col3 REAL, col4 VARCHAR(8), col5 int, col6 DOUBLE PRECISION, col7 CHAR ( 3 ))"},
			want: []string{"CREATE TABLE tab0(pk bigint PRIMARY KEY, col0 bigint, col1 double precision, col2 text, col3 double precision, col4 text, col5 bigint, col6 double precision, col7 text)"},
		},
		{
			name: "adds a primary key over every column to a table without one",
			in:   []string{"CREATE TABLE t1( x INTEGER, y VARCHAR(8) )", "CREATE TABLE t2(\n  a INTEGER NOT NULL,\n  b TEXT\n)"},
			want: []string{"CREATE TABLE t1( x bigint, y text , PRIMARY KEY (x, y))", "CREATE TABLE t2(\n  a bigint NOT NULL,\n  b text\n, PRIMARY KEY (a, b))"},
		},
		{
			name: "leaves a table with a primary key or a table constraint as it is, apart from the types",
			in:   []string{"CREATE TABLE t1(a INTEGER, b INTEGER, PRIMARY KEY (b))", "CREATE TABLE t2(a TEXT, UNIQUE (a(1)))"},
			want: []string{"CREATE TABLE t1(a bigint, b bigint, PRIMARY KEY (b))", "CREATE TABLE t2(a text, UNIQUE (a(1)), PRIMARY KEY (a))"},
		},
		{
			name: "rewrites the target type of a cast, and nothing else",
			in:   []string{"SELECT CAST ( col0 AS INTEGER ) AS integer, CAST(NULL AS REAL), 'CAST(x AS INTEGER)', (col0 AS INTEGER) FROM tab0"},
			want: []string{"SELECT CAST ( col0 AS bigint ) AS integer, CAST(NULL AS double precision), 'CAST(x AS INTEGER)', (col0 AS INTEGER) FROM tab0"},
		},
		{
			name: "keeps CREATE INDEX",
			in:   []string{"CREATE INDEX idx_tab1_0 on tab1 (col0)", "CREATE UNIQUE INDEX i ON t(a)", "SELECT 1"},
			want: []string{"CREATE INDEX idx_tab1_0 on tab1 (col0)", "CREATE UNIQUE INDEX i ON t(a)", "SELECT 1"},
		},
		{
			name: "replaces INSERT ... SELECT * from a table filled only by INSERT ... VALUES with those values",
			in: []string{
				"CREATE TABLE tab0(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"INSERT INTO tab0 VALUES(0,'a')",
				"INSERT INTO tab0 VALUES (1,'b'), (2, 'c')",
				"CREATE TABLE tab1(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"insert into tab1 select * from tab0",
				"CREATE TABLE tab2(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"INSERT INTO tab2 SELECT * FROM tab1",
			},
			want: []string{
				"CREATE TABLE tab0(pk bigint PRIMARY KEY, col0 text)",
				"INSERT INTO tab0 VALUES(0,'a')",
				"INSERT INTO tab0 VALUES (1,'b'), (2, 'c')",
				"CREATE TABLE tab1(pk bigint PRIMARY KEY, col0 text)",
				"insert into tab1 VALUES (0,'a'), (1,'b'), (2, 'c')",
				"CREATE TABLE tab2(pk bigint PRIMARY KEY, col0 text)",
				"INSERT INTO tab2 VALUES (0,'a'), (1,'b'), (2, 'c')",
			},
		},
		{
			name: "keeps INSERT ... SELECT when the source had other writes or other columns",
			in: []string{
				"CREATE TABLE tab0(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"INSERT INTO tab0 VALUES(0,'a')",
				"CREATE TABLE tab1(pk INTEGER PRIMARY KEY, col1 TEXT)",
				"INSERT INTO tab1 SELECT * FROM tab0",
				"CREATE TABLE tab2(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"DELETE FROM tab0 WHERE pk = 0",
				"INSERT INTO tab2 SELECT * FROM tab0",
				"INSERT INTO tab2 SELECT * FROM nothing",
				"INSERT INTO tab2(pk, col0) VALUES(5, 'x')",
				"INSERT INTO tab1 SELECT * FROM tab2",
				"CREATE TABLE tab3(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"INSERT INTO tab3 VALUES(0,'a');",
				"INSERT INTO tab2 SELECT * FROM tab3",
				"CREATE TABLE tab4(pk INTEGER PRIMARY KEY, col0 TEXT)",
				"INSERT INTO tab4 VALUES(0,'a')",
				"DROP TABLE IF EXISTS tab4",
				"INSERT INTO tab2 SELECT * FROM tab4",
			},
			want: []string{
				"CREATE TABLE tab0(pk bigint PRIMARY KEY, col0 text)",
				"INSERT INTO tab0 VALUES(0,'a')",
				"CREATE TABLE tab1(pk bigint PRIMARY KEY, col1 text)",
				"INSERT INTO tab1 SELECT * FROM tab0",
				"CREATE TABLE tab2(pk bigint PRIMARY KEY, col0 text)",
				"DELETE FROM tab0 WHERE pk = 0",
				"INSERT INTO tab2 SELECT * FROM tab0",
				"INSERT INTO tab2 SELECT * FROM nothing",
				"INSERT INTO tab2(pk, col0) VALUES(5, 'x')",
				"INSERT INTO tab1 SELECT * FROM tab2",
				"CREATE TABLE tab3(pk bigint PRIMARY KEY, col0 text)",
				"INSERT INTO tab3 VALUES(0,'a');",
				"INSERT INTO tab2 SELECT * FROM tab3",
				"CREATE TABLE tab4(pk bigint PRIMARY KEY, col0 text)",
				"INSERT INTO tab4 VALUES(0,'a')",
				"DROP TABLE IF EXISTS tab4",
				"INSERT INTO tab2 SELECT * FROM tab4",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := rewrite(statements(test.in...))
			is.EqualSlice(t, test.want, texts(got))
		})
	}

	t.Run("keeps CREATE INDEX and the line numbers", func(t *testing.T) {
		got := rewrite(statements("CREATE INDEX i ON t(a)", "SELECT 1"))
		is.EqualSlice(t, []string{"CREATE INDEX i ON t(a)", "SELECT 1"}, texts(got))
		is.Equal(t, 2, got[1].line)
	})
}
