package pgdiff_test

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/pgdiff"
	"maragu.dev/maragudb/internal/verify"
)

func TestErrorClasses(t *testing.T) {
	verify.Covers(t, "every maragudb error class fails with its mapped SQLSTATE code on Postgres for the same statement", "AC-4.3")
	// Each case fails on both databases. Postgres is the Postgres spelling when it differs.
	// Each statement fails on both databases.
	tests := []struct {
		setup string
		sql   string
		want  error
		code  string
	}{
		{sql: "SELECT 1 / 0", want: maragudb.ErrDivisionByZero, code: "22012"},
		{sql: "SELECT 9223372036854775807 + 1", want: maragudb.ErrOutOfRange, code: "22003"},
		{sql: "SELECT 2147483647 + 1", want: maragudb.ErrOutOfRange, code: "22003"},
		{sql: "SELECT CAST(1e300::float8 AS bigint)", want: maragudb.ErrOutOfRange, code: "22003"},
		{sql: "INSERT INTO dev VALUES (2, NULL)", want: maragudb.ErrNotNull, code: "23502"},
		{sql: "INSERT INTO dev VALUES (1, 1)", want: maragudb.ErrDuplicateKey, code: "23505"},
		{sql: "SELEC 1", want: maragudb.ErrSyntax, code: "42601"},
		{sql: "SELECT 1 ORDER BY 'a'", want: maragudb.ErrSyntax, code: "42601"},
		{sql: "SELECT * FROM nope", want: maragudb.ErrNoTable, code: "42P01"},
		{sql: "CREATE TABLE dev (i bigint PRIMARY KEY)", want: maragudb.ErrTableExists, code: "42P07"},
		{sql: "CREATE INDEX dev ON dev (j)", want: maragudb.ErrTableExists, code: "42P07"},
		{sql: "CREATE TABLE dev_pkey (i bigint PRIMARY KEY)", want: maragudb.ErrTableExists, code: "42P07"},
		{setup: "CREATE UNIQUE INDEX ON dev (j, j)", sql: "INSERT INTO dev VALUES (3, 1)", want: maragudb.ErrDuplicateKey, code: "23505"},
		{sql: "DROP INDEX nope", want: maragudb.ErrNoIndex, code: "42704"},
		{sql: "DROP INDEX dev", want: maragudb.ErrWrongObjectType, code: "42809"},
		{sql: "DROP TABLE IF EXISTS dev_pkey", want: maragudb.ErrWrongObjectType, code: "42809"},
		{sql: "SELECT * FROM dev_pkey", want: maragudb.ErrWrongObjectType, code: "42809"},
		{sql: "DROP INDEX dev_pkey", want: maragudb.ErrDependentObjects, code: "2BP01"},
		{sql: "CREATE INDEX ON dev (" + strings.TrimSuffix(strings.Repeat("j, ", 33), ", ") + ")", want: maragudb.ErrProgramLimit, code: "54011"},
		{sql: "CREATE TABLE dev2 (a bigint PRIMARY KEY, UNIQUE (" + strings.TrimSuffix(strings.Repeat("a, ", 33), ", ") + "))", want: maragudb.ErrInvalid, code: "42701"},
		{sql: "SELECT 1 WHERE 1", want: maragudb.ErrType, code: "42804"},
		{sql: "INSERT INTO dev VALUES (TRUE, 1)", want: maragudb.ErrType, code: "42804"},
		{sql: "SELECT 1 + TRUE", want: maragudb.ErrType, code: "42883"},
		{sql: "SELECT CAST('a' AS bigint)", want: maragudb.ErrCast, code: "22P02"},
		{sql: "INSERT INTO dev VALUES ('a', 1)", want: maragudb.ErrCast, code: "22P02"},
		{sql: "SELECT CAST('' AS timestamptz)", want: maragudb.ErrCast, code: "22007"},
		{sql: "SELECT CAST('2024-01-02 03:04:05+99' AS timestamptz)", want: maragudb.ErrCast, code: "22009"},
		{sql: "SELECT CAST('2024-02-30' AS timestamptz)", want: maragudb.ErrCast, code: "22008"},
		{sql: "SELECT CAST(TRUE AS bytea)", want: maragudb.ErrCast, code: "42846"},
		{sql: "SELECT CAST('\\x0g' AS bytea)", want: maragudb.ErrCast, code: "22023"},
		{sql: "SELECT CAST('\\x0' AS bytea)", want: maragudb.ErrCast, code: "22023"},
		{sql: "SELECT CAST('\\' AS bytea)", want: maragudb.ErrCast, code: "22P02"},
		{sql: "SELECT nope FROM dev", want: maragudb.ErrInvalid, code: "42703"},
		{sql: "SELECT length()", want: maragudb.ErrInvalid, code: "42883"},
		{sql: "SELECT 1 ORDER BY 2", want: maragudb.ErrInvalid, code: "42P10"},
		{sql: "SELECT i FROM dev LIMIT i", want: maragudb.ErrInvalid, code: "42P10"},
		{sql: "CREATE TABLE dev2 (a bigint PRIMARY KEY, a bigint)", want: maragudb.ErrInvalid, code: "42701"},
		{sql: "SELECT i AS a, j AS a FROM dev ORDER BY a", want: maragudb.ErrInvalid, code: "42702"},
		{sql: "CREATE TABLE dev2 (a bigint PRIMARY KEY, b bigint PRIMARY KEY)", want: maragudb.ErrInvalid, code: "42P16"},
		{sql: "SELECT 'ab' LIKE 'a\\'", want: maragudb.ErrInvalid, code: "22025"},
		{sql: "SELECT 1 LIMIT -1", want: maragudb.ErrInvalid, code: "2201W"},
		{sql: "SELECT 1 OFFSET -1", want: maragudb.ErrInvalid, code: "2201X"},
	}

	seen := map[string]bool{}
	for _, test := range tests {
		t.Run(fmt.Sprintf("%s fails with the class of SQLSTATE %s on both", test.sql, test.code), func(t *testing.T) {
			b := newBoth(t, "CREATE TABLE dev (i bigint PRIMARY KEY, j bigint NOT NULL)")
			for _, db := range []*sql.DB{b.m, b.pg} {
				_, err := db.ExecContext(t.Context(), "INSERT INTO dev VALUES (1, 1)")
				is.NotError(t, err)
			}
			if test.setup != "" {
				b.exec(t, test.setup)
			}
			_, mErr := b.m.ExecContext(t.Context(), test.sql)
			_, pgErr := b.pg.ExecContext(t.Context(), test.sql)
			is.Error(t, test.want, mErr)
			is.True(t, pgErr != nil, "postgres fails too")
			is.Equal(t, test.code, pgdiff.SQLState(pgErr))
			is.True(t, pgdiff.SameClass(mErr, pgErr), fmt.Sprintf("maragudb %v and postgres %v are the same class", mErr, pgErr))
			seen[pgdiff.ClassOf(mErr).Name+"/"+pgdiff.SQLState(pgErr)] = true
		})
	}

	t.Run("a write in a read-only transaction fails with the class of SQLSTATE 25006 on both", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i bigint PRIMARY KEY)")
		var errs []error
		for _, db := range []*sql.DB{b.m, b.pg} {
			tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			is.NotError(t, err)
			_, err = tx.ExecContext(t.Context(), "INSERT INTO dev VALUES (1)")
			errs = append(errs, err)
			is.NotError(t, tx.Rollback())
		}
		is.Error(t, maragudb.ErrReadOnlyTx, errs[0])
		is.Equal(t, "25006", pgdiff.SQLState(errs[1]))
		is.True(t, pgdiff.SameClass(errs[0], errs[1]))
		seen["read-only transaction/25006"] = true
	})

	t.Run("a statement after a failed one in a transaction fails with the class of SQLSTATE 25P02 on both, and COMMIT rolls back", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i bigint PRIMARY KEY)")
		var errs []error
		for _, db := range []*sql.DB{b.m, b.pg} {
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			for _, q := range []string{"BEGIN", "INSERT INTO dev VALUES (1)"} {
				_, err = conn.ExecContext(t.Context(), q)
				is.NotError(t, err)
			}
			_, err = conn.ExecContext(t.Context(), "SELECT 1 / 0")
			is.True(t, err != nil)
			_, err = conn.ExecContext(t.Context(), "SELECT 1")
			errs = append(errs, err)
			_, err = conn.ExecContext(t.Context(), "COMMIT")
			is.NotError(t, err)
			err = conn.QueryRowContext(t.Context(), "SELECT i FROM dev").Scan(new(int64))
			is.Error(t, sql.ErrNoRows, err, "the insert was rolled back")
			is.NotError(t, conn.Close())
		}
		is.Error(t, maragudb.ErrTxAborted, errs[0])
		is.Equal(t, "25P02", pgdiff.SQLState(errs[1]))
		is.True(t, pgdiff.SameClass(errs[0], errs[1]))
		seen["transaction aborted/25P02"] = true
	})

	// The code lists are no wider than what the cases show.
	for _, c := range pgdiff.ErrorClasses {
		for _, code := range c.Codes {
			is.True(t, seen[c.Name+"/"+code], fmt.Sprintf("class %s with SQLSTATE %s has a case", c.Name, code))
		}
	}

	t.Run("errors outside the classes match nothing", func(t *testing.T) {
		is.True(t, pgdiff.ClassOf(maragudb.ErrConflict) == nil)
		is.True(t, !pgdiff.SameClass(maragudb.ErrConflict, nil))
		is.True(t, !slices.Contains(pgdiff.ErrorClasses[0].Codes, ""))
	})
}
