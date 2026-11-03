package pgdiff_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb"
)

// Each test here is one numbered deviation in /docs/deviations.md. It runs the statement on both
// databases and asserts the documented difference, so that the document cannot go stale: when
// either side changes, the test fails.

// both is a maragudb and a Postgres database with the same table `dev`.
type both struct {
	m, pg *sql.DB
}

func newBoth(t *testing.T, create string) both {
	t.Helper()
	r := newRunner(t)
	m, err := r.New()
	is.NotError(t, err)
	t.Cleanup(func() { _ = m.Close() })
	b := both{m: m, pg: r.PG}
	_, err = b.pg.ExecContext(t.Context(), "DROP TABLE IF EXISTS dev")
	is.NotError(t, err)
	if create != "" {
		_, err = b.m.ExecContext(t.Context(), create)
		is.NotError(t, err)
		pgCreate := strings.NewReplacer(" int", " bigint", " float", " double precision", " string", " text", " bytes", " bytea", " time", " timestamptz").Replace(create)
		_, err = b.pg.ExecContext(t.Context(), pgCreate)
		is.NotError(t, err)
	}
	return b
}

// scan one value of a query on db, or an error.
func scan[T any](ctx context.Context, db *sql.DB, query string, args ...any) (T, error) {
	var v T
	err := db.QueryRowContext(ctx, query, args...).Scan(&v)
	return v, err
}

func TestDeviations(t *testing.T) {
	t.Run("1 int and float do not mix, except an int literal next to a float", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY, f float)")
		_, err := b.m.ExecContext(t.Context(), "INSERT INTO dev VALUES (1, 1.5)")
		is.NotError(t, err)
		_, err = b.pg.ExecContext(t.Context(), "INSERT INTO dev VALUES (1, 1.5)")
		is.NotError(t, err)
		for _, q := range []string{"SELECT i FROM dev WHERE i = 1.0", "SELECT i + f FROM dev", "SELECT i FROM dev WHERE i < f"} {
			_, err := scan[float64](t.Context(), b.pg, q)
			is.NotError(t, err, "postgres accepts "+q)
			_, err = scan[float64](t.Context(), b.m, q)
			is.Error(t, maragudb.ErrType, err, "maragudb rejects "+q)
		}
		got, err := scan[float64](t.Context(), b.m, "SELECT f + 1 FROM dev WHERE f = 1.5 AND i = 1")
		is.NotError(t, err, "an int literal adapts")
		is.Equal(t, 2.5, got)
	})

	t.Run("2 no assignment casts", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY)")
		_, err := b.pg.ExecContext(t.Context(), "INSERT INTO dev VALUES (1.5)")
		is.NotError(t, err)
		got, err := scan[int64](t.Context(), b.pg, "SELECT i FROM dev")
		is.NotError(t, err)
		is.Equal(t, int64(2), got, "postgres rounds")
		_, err = b.m.ExecContext(t.Context(), "INSERT INTO dev VALUES (1.5)")
		is.Error(t, maragudb.ErrType, err)
	})

	t.Run("3 parameters never adapt", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY, f float)")
		_, err := b.pg.ExecContext(t.Context(), "INSERT INTO dev VALUES ($1, $2)", 1, 2)
		is.NotError(t, err, "postgres converts the int to a float")
		_, err = b.m.ExecContext(t.Context(), "INSERT INTO dev VALUES ($1, $2)", 1, 2)
		is.Error(t, maragudb.ErrType, err)
		_, err = b.m.ExecContext(t.Context(), "INSERT INTO dev VALUES ($1, $2)", 1, 2.0)
		is.NotError(t, err)
	})

	t.Run("4 || concatenates strings with strings and bytes with bytes only", func(t *testing.T) {
		b := newBoth(t, "")
		got, err := scan[string](t.Context(), b.pg, "SELECT 'a' || 1")
		is.NotError(t, err)
		is.Equal(t, "a1", got)
		_, err = scan[string](t.Context(), b.m, "SELECT 'a' || 1")
		is.Error(t, maragudb.ErrType, err)
	})

	t.Run("5 LIMIT and OFFSET take an int", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY)")
		for _, db := range []*sql.DB{b.m, b.pg} {
			_, err := db.ExecContext(t.Context(), "INSERT INTO dev VALUES (1), (2), (3)")
			is.NotError(t, err)
		}
		rows, err := b.pg.QueryContext(t.Context(), "SELECT i FROM dev ORDER BY i LIMIT 1.5")
		is.NotError(t, err)
		n := 0
		for rows.Next() {
			n++
		}
		is.NotError(t, rows.Close())
		is.Equal(t, 2, n, "postgres rounds the limit")
		_, err = b.m.QueryContext(t.Context(), "SELECT i FROM dev ORDER BY i LIMIT 1.5")
		is.Error(t, maragudb.ErrType, err)
	})

	t.Run("6 integer literals must fit in an int", func(t *testing.T) {
		b := newBoth(t, "")
		var s string
		is.NotError(t, b.pg.QueryRowContext(t.Context(), "SELECT CAST(9223372036854775808 AS text)").Scan(&s))
		is.Equal(t, "9223372036854775808", s, "postgres makes a numeric")
		_, err := scan[string](t.Context(), b.m, "SELECT CAST(9223372036854775808 AS string)")
		is.Error(t, maragudb.ErrSyntax, err)
	})

	t.Run("7 time has nanosecond precision", func(t *testing.T) {
		b := newBoth(t, "")
		want := time.Date(2024, 1, 1, 0, 0, 0, 123456789, time.UTC)
		got, err := scan[time.Time](t.Context(), b.m, "SELECT TIME '2024-01-01T00:00:00.123456789Z'")
		is.NotError(t, err)
		is.True(t, got.Equal(want))
		got, err = scan[time.Time](t.Context(), b.pg, "SELECT '2024-01-01T00:00:00.123456789Z'::timestamptz")
		is.NotError(t, err)
		is.True(t, got.Equal(want.Truncate(time.Microsecond).Add(time.Microsecond)), "postgres rounds to microseconds")
	})

	t.Run("8 time casts use RFC 3339", func(t *testing.T) {
		b := newBoth(t, "")
		got, err := scan[string](t.Context(), b.m, "SELECT CAST(TIME '2024-01-02T03:04:05.5Z' AS string)")
		is.NotError(t, err)
		is.Equal(t, "2024-01-02T03:04:05.5Z", got)
		_, err = b.pg.ExecContext(t.Context(), "SET TIME ZONE 'UTC'")
		is.NotError(t, err)
		got, err = scan[string](t.Context(), b.pg, "SELECT CAST('2024-01-02T03:04:05.5Z'::timestamptz AS text)")
		is.NotError(t, err)
		is.Equal(t, "2024-01-02 03:04:05.5+00", got)
		_, err = scan[time.Time](t.Context(), b.pg, "SELECT CAST('January 2, 2024' AS timestamptz)")
		is.NotError(t, err, "postgres accepts many formats")
		_, err = scan[time.Time](t.Context(), b.m, "SELECT CAST('January 2, 2024' AS time)")
		is.Error(t, maragudb.ErrCast, err)
	})

	t.Run("9 bytes casts use UTF-8", func(t *testing.T) {
		b := newBoth(t, "")
		got, err := scan[string](t.Context(), b.m, "SELECT CAST(X'6869' AS string)")
		is.NotError(t, err)
		is.Equal(t, "hi", got)
		got, err = scan[string](t.Context(), b.pg, "SELECT CAST('\\x6869'::bytea AS text)")
		is.NotError(t, err)
		is.Equal(t, `\x6869`, got)
		bs, err := scan[[]byte](t.Context(), b.m, "SELECT CAST('hi' AS bytes)")
		is.NotError(t, err)
		is.EqualSlice(t, []byte("hi"), bs)
		_, err = scan[string](t.Context(), b.m, "SELECT CAST(X'ff' AS string)")
		is.Error(t, maragudb.ErrCast, err, "not valid UTF-8")
	})

	t.Run("10 string to number casts accept decimal notation only", func(t *testing.T) {
		b := newBoth(t, "")
		for _, s := range []string{"0x10", "1_000"} {
			_, err := scan[int64](t.Context(), b.pg, "SELECT CAST($1 AS bigint)", s)
			is.NotError(t, err, "postgres accepts "+s)
			_, err = scan[int64](t.Context(), b.m, "SELECT CAST($1 AS int)", s)
			is.Error(t, maragudb.ErrCast, err, "maragudb rejects "+s)
		}
		got, err := scan[int64](t.Context(), b.m, "SELECT CAST(' 42 ' AS int)")
		is.NotError(t, err)
		is.Equal(t, int64(42), got)
	})

	t.Run("11 lower and upper fold all of Unicode", func(t *testing.T) {
		b := newBoth(t, "")
		got, err := scan[string](t.Context(), b.m, "SELECT lower('ÄBC')")
		is.NotError(t, err)
		is.Equal(t, "äbc", got)
		got, err = scan[string](t.Context(), b.pg, "SELECT lower('ÄBC')")
		is.NotError(t, err)
		is.Equal(t, "Äbc", got, "the C locale folds ASCII only")
	})

	t.Run("12 a failed statement does not abort the transaction", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY)")
		for _, db := range []*sql.DB{b.m, b.pg} {
			tx, err := db.BeginTx(t.Context(), nil)
			is.NotError(t, err)
			_, err = tx.ExecContext(t.Context(), "INSERT INTO dev VALUES (1)")
			is.NotError(t, err)
			_, err = tx.ExecContext(t.Context(), "INSERT INTO dev VALUES (1)")
			is.True(t, err != nil, "the duplicate fails on both")
			var n int64
			err = tx.QueryRowContext(t.Context(), "SELECT i FROM dev").Scan(&n)
			if db == b.m {
				is.NotError(t, err, "maragudb keeps the transaction usable")
				is.Equal(t, int64(1), n)
				is.NotError(t, tx.Commit())
			} else {
				is.True(t, err != nil && strings.Contains(err.Error(), "current transaction is aborted"), "postgres aborts the transaction")
				is.NotError(t, tx.Rollback())
			}
		}
	})

	t.Run("13 UPDATE of key columns visits rows in key order", func(t *testing.T) {
		b := newBoth(t, "CREATE TABLE dev (i int PRIMARY KEY)")
		for _, db := range []*sql.DB{b.m, b.pg} {
			_, err := db.ExecContext(t.Context(), "INSERT INTO dev VALUES (3), (2)")
			is.NotError(t, err)
		}
		_, err := b.pg.ExecContext(t.Context(), "UPDATE dev SET i = i + 1")
		is.NotError(t, err, "postgres moves 3 first, in insertion order")
		_, err = b.m.ExecContext(t.Context(), "UPDATE dev SET i = i + 1")
		is.Error(t, maragudb.ErrDuplicateKey, err, "maragudb moves 2 first, onto 3")
	})

	t.Run("14 every table needs a primary key", func(t *testing.T) {
		b := newBoth(t, "")
		_, err := b.pg.ExecContext(t.Context(), "CREATE TABLE dev (i bigint)")
		is.NotError(t, err)
		_, err = b.m.ExecContext(t.Context(), "CREATE TABLE dev (i int)")
		is.Error(t, maragudb.ErrSyntax, err)
	})

	t.Run("15 more words are reserved", func(t *testing.T) {
		b := newBoth(t, "")
		_, err := b.pg.ExecContext(t.Context(), "CREATE TABLE dev (key bigint PRIMARY KEY)")
		is.NotError(t, err)
		_, err = b.m.ExecContext(t.Context(), "CREATE TABLE dev (key int PRIMARY KEY)")
		is.Error(t, maragudb.ErrSyntax, err)
		_, err = b.m.ExecContext(t.Context(), `CREATE TABLE dev ("key" int PRIMARY KEY)`)
		is.NotError(t, err, "quoting works")
	})

	t.Run("16 the output name of a cast of a constant is the maragudb type name", func(t *testing.T) {
		b := newBoth(t, "")
		name := func(db *sql.DB, q string) string {
			rows, err := db.QueryContext(t.Context(), q)
			is.NotError(t, err)
			defer func() { _ = rows.Close() }()
			cols, err := rows.Columns()
			is.NotError(t, err)
			return cols[0]
		}
		is.Equal(t, "int", name(b.m, "SELECT CAST(1 AS int)"))
		is.Equal(t, "int8", name(b.pg, "SELECT CAST(1 AS bigint)"))
		is.Equal(t, "case", name(b.m, "SELECT CASE WHEN TRUE THEN 1 END"))
		is.Equal(t, "case", name(b.pg, "SELECT CASE WHEN TRUE THEN 1 END"))
		is.Equal(t, "?column?", name(b.m, "SELECT 1 + 1"))
		is.Equal(t, "?column?", name(b.pg, "SELECT 1 + 1"))
	})
}
