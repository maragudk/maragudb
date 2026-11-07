package maragudb_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
	"maragu.dev/maragudb/internal/sqldriver"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

// world is a fake store shared by several databases, like several processes.
type world struct {
	t      *testing.T
	fake   *s3fake.Client
	decide func(kind s3.Kind, key string) s3fake.Outcome
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t}
	w.fake = s3fake.New(s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
		if w.decide == nil {
			return s3fake.OutcomeNormal
		}
		return w.decide(kind, key)
	}}))
	return w
}

// open a database over the fake with sql.OpenDB. Background maintenance stays off unless the
// test turns it on, so that its timing cannot add requests to what a test measures.
func (w *world) open(opts maragudb.Options) *sql.DB {
	w.t.Helper()
	opts.Store = s3test.NewStore(w.fake)
	if opts.Maintenance == nil {
		opts.Maintenance = new(false)
	}
	c, err := maragudb.NewConnector(opts, "s3://bucket/app")
	is.NotError(w.t, err)
	db := sql.OpenDB(c)
	w.t.Cleanup(func() { _ = db.Close() })
	return db
}

// clone the world into a new one for t, with a copy of every object and no faults.
func (w *world) clone(t *testing.T) *world {
	t.Helper()
	c := newWorld(t)
	for _, key := range w.fake.Keys() {
		body, _ := w.fake.Body(key)
		_, err := c.fake.WithoutFaults().Put(t.Context(), key, body, s3.PutOptions{})
		is.NotError(t, err)
	}
	return c
}

// loadedWorlds returns a function that gives a world with the database that load makes for n,
// loaded once per n for every subtest of t and copied for each, so that no subtest sees the
// requests or the writes of another.
func loadedWorlds(t *testing.T, load func(t *testing.T, db *sql.DB, n int)) func(t *testing.T, n int) *world {
	worlds := map[int]*world{}
	return func(sub *testing.T, n int) *world {
		sub.Helper()
		if worlds[n] == nil {
			w := newWorld(t)
			load(t, w.open(maragudb.Options{}), n)
			worlds[n] = w
		}
		return worlds[n].clone(sub)
	}
}

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	return newWorld(t).open(maragudb.Options{})
}

func mustExec(t *testing.T, db interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, query string, args ...any) sql.Result {
	t.Helper()
	res, err := db.ExecContext(t.Context(), query, args...)
	is.NotError(t, err, query)
	return res
}

// queryRows renders the rows of a query, one line per row, with values separated by spaces.
func queryRows(t *testing.T, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), query, args...)
	is.NotError(t, err, query)
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	is.NotError(t, err)
	var lines []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		is.NotError(t, rows.Scan(ptrs...))
		var parts []string
		for _, v := range vals {
			switch v := v.(type) {
			case nil:
				parts = append(parts, "NULL")
			case []byte:
				parts = append(parts, fmt.Sprintf("X'%x'", v))
			case time.Time:
				parts = append(parts, v.Format(time.RFC3339Nano))
			case string:
				parts = append(parts, fmt.Sprintf("%q", v))
			default:
				parts = append(parts, fmt.Sprint(v))
			}
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	is.NotError(t, rows.Err())
	return strings.Join(lines, "\n")
}

func TestRegisterDriver(t *testing.T) {
	t.Run("registers under the given name and opens with sql.Open", func(t *testing.T) {
		verify.Covers(t, "RegisterDriver registers the driver explicitly under a name, and sql.Open with an s3://bucket/prefix DSN works", "INV-REGISTER", "INV-DSN")
		w := newWorld(t)
		maragudb.RegisterDriver(maragudb.Options{Name: new("maragudb-test-register"), Store: s3test.NewStore(w.fake)})
		db, err := sql.Open("maragudb-test-register", "s3://bucket/app")
		is.NotError(t, err)
		defer func() { _ = db.Close() }()
		is.NotError(t, db.PingContext(t.Context()))
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, name text)")
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2)", 1, "a")
		is.Equal(t, `1 "a"`, queryRows(t, db, "SELECT * FROM t"))
	})

	t.Run("rejects a bad DSN when connecting", func(t *testing.T) {
		verify.Covers(t, "DSNs that are not s3://bucket/prefix fail with ErrDSN", "INV-DSN")
		w := newWorld(t)
		for _, dsn := range []string{"bucket/app", "s3:///app", "http://bucket/app", "s3://bucket/app?x=1", "s3://user@bucket/app"} {
			_, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(w.fake)}, dsn)
			is.Error(t, maragudb.ErrDSN, err, dsn)
		}
		c, err := maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(w.fake)}, "s3://bucket")
		is.NotError(t, err, "an empty prefix is fine")
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		is.NotError(t, db.PingContext(t.Context()))
	})
}

func TestDB(t *testing.T) {
	t.Run("fails with an error that names the versions on a database whose manifest an earlier version wrote", func(t *testing.T) {
		verify.Covers(t, "a statement on a database whose manifest has the encoding of version 1 fails with an error that names the versions, and returns no rows", "AC-6.4")
		w := newWorld(t)
		v1 := `{"version":1,"seq":1,"commitID":"c1","segments":[],"garbage":[{"key":"app/seg/0000000000000000-a","unreferencedAt":"2026-01-01T00:00:00Z"}],"commits":[{"seq":1,"id":"c1"}],"updatedAt":"2026-01-01T00:00:00Z"}`
		_, err := w.fake.Put(t.Context(), "app/manifest", []byte(v1), s3.PutOptions{})
		is.NotError(t, err)
		db := w.open(maragudb.Options{})
		rows, err := db.QueryContext(t.Context(), "SELECT 1")
		if err == nil {
			_ = rows.Close()
		}
		is.True(t, err != nil && strings.Contains(err.Error(), "the manifest has version 1, and this version of maragudb reads only version 2"), fmt.Sprint(err))
	})

	t.Run("round trips every type through parameters and Scan", func(t *testing.T) {
		verify.Covers(t, "every type and NULL round-trips through $1-style parameters and Scan", "INV-TYPES", "INV-PLACEHOLDERS")
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, f double precision, b boolean, s text, bs bytea, at timestamptz, n bigint)")
		at := time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.FixedZone("plus2", 2*3600))
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6, $7)", int64(1), 1.5, true, "héllo", []byte{0, 1, 2}, at, nil)
		mustExec(t, db, "INSERT INTO t (id) VALUES ($1)", 2)
		var nilBytes []byte
		mustExec(t, db, "INSERT INTO t (id, bs) VALUES ($1, $2), ($3, $4)", 3, nilBytes, 4, []byte{})
		is.Equal(t, "NULL\nX''", queryRows(t, db, "SELECT bs FROM t WHERE id >= 3 ORDER BY id"), "a nil slice is NULL and an empty one is empty bytes")

		var id, n sql.NullInt64
		var f float64
		var b bool
		var s string
		var by []byte
		var got time.Time
		row := db.QueryRowContext(t.Context(), "SELECT id, f, b, s, bs, at, n FROM t WHERE id = $1", 1)
		is.NotError(t, row.Scan(&id, &f, &b, &s, &by, &got, &n))
		is.Equal(t, int64(1), id.Int64)
		is.Equal(t, 1.5, f)
		is.True(t, b)
		is.Equal(t, "héllo", s)
		is.EqualSlice(t, []byte{0, 1, 2}, by)
		is.True(t, got.Equal(at.Truncate(time.Microsecond)), "a timestamptz keeps microseconds")
		is.Equal(t, time.UTC, got.Location(), "times come back in UTC")
		is.True(t, !n.Valid)

		row = db.QueryRowContext(t.Context(), "SELECT f, s, n FROM t WHERE id = $1", 2)
		var nf sql.NullFloat64
		var ns sql.NullString
		is.NotError(t, row.Scan(&nf, &ns, &n))
		is.True(t, !nf.Valid && !ns.Valid && !n.Valid)
	})

	t.Run("gives a parameter the type of its context, as Postgres does", func(t *testing.T) {
		verify.Covers(t, "a string parameter is read like a quoted literal of the type its context gives it, an int64 converts to a double precision, and a conversion that a Postgres driver might make differently is ErrUnsupported", "INV-PLACEHOLDERS", "UNS-PARAMETERS")
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, f double precision, s text)")
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", "1", 2, "x")
		is.Equal(t, `1 2 "x"`, queryRows(t, db, "SELECT * FROM t"), "a string reads as a bigint, and an int64 widens")
		_, err := db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", "x", 1.5, "x")
		is.Error(t, maragudb.ErrCast, err, "a string that is not a bigint")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 2, 1.5, 3)
		is.Error(t, maragudb.ErrUnsupported, err, "an int64 for a text column")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 2.0, 1.5, "x")
		is.Error(t, maragudb.ErrUnsupported, err, "a float64 for a bigint column")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 2, true, "x")
		is.Error(t, maragudb.ErrUnsupported, err, "a bool for a double precision column")
		_, err = db.ExecContext(t.Context(), "SELECT $1 + 2147483647", 1)
		is.Error(t, maragudb.ErrOutOfRange, err, "an integer context makes the parameter an integer, which overflows as in Postgres")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", int32(1), 1.5, "x")
		is.True(t, err != nil && strings.Contains(err.Error(), "unsupported parameter type int32"), fmt.Sprint(err))
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 1, 1.5)
		is.True(t, err != nil && strings.Contains(err.Error(), "3 parameters, 2 given"), fmt.Sprint(err))
		is.Equal(t, `1 2 "x"`, queryRows(t, db, "SELECT * FROM t"))
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", sql.NullInt64{Int64: 7, Valid: true}, sql.NullFloat64{}, sql.NullString{String: "v", Valid: true})
		is.Equal(t, `1 2 "x"`+"\n"+`7 NULL "v"`, queryRows(t, db, "SELECT * FROM t"), "driver.Valuer parameters are unwrapped")
		var nilValuer *nullableValuer
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", 8, nilValuer, nil)
		is.Equal(t, `1 2 "x"`+"\n"+`7 NULL "v"`+"\n"+`8 NULL NULL`, queryRows(t, db, "SELECT * FROM t"), "a nil pointer Valuer is NULL")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 9, 1.5, "x", "extra")
		is.True(t, err != nil && strings.Contains(err.Error(), "3 parameters, 4 given"), fmt.Sprint(err))
	})

	t.Run("prepares statements and reports parameter count mismatches", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		st, err := db.PrepareContext(t.Context(), "INSERT INTO t VALUES ($1, $2)")
		is.NotError(t, err)
		defer func() { _ = st.Close() }()
		for i := range 3 {
			_, err := st.ExecContext(t.Context(), i, i*10)
			is.NotError(t, err)
		}
		_, err = st.ExecContext(t.Context(), 9)
		is.True(t, err != nil, "too few parameters")
		_, err = st.ExecContext(t.Context(), 9, 9, 9)
		is.True(t, err != nil, "too many parameters")
		is.Equal(t, "0 0\n1 10\n2 20", queryRows(t, db, "SELECT * FROM t ORDER BY id"))
		_, err = db.ExecContext(t.Context(), "SELEC 1")
		is.Error(t, maragudb.ErrSyntax, err)
	})

	t.Run("reports column types", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, f double precision, b boolean, s text, bs bytea, at timestamptz)")
		rows, err := db.QueryContext(t.Context(), "SELECT id, f, b, s, bs, at, NULL, id + 1 AS next, length(s) FROM t")
		is.NotError(t, err)
		defer func() { _ = rows.Close() }()
		types, err := rows.ColumnTypes()
		is.NotError(t, err)
		var got []string
		for _, ct := range types {
			nullable, _ := ct.Nullable()
			got = append(got, fmt.Sprintf("%s:%s:%v:%v", ct.Name(), ct.DatabaseTypeName(), ct.ScanType(), nullable))
		}
		is.EqualSlice(t, []string{"id:INT8:int64:false", "f:FLOAT8:float64:true", "b:BOOL:bool:true", "s:TEXT:string:true", "bs:BYTEA:[]uint8:true", "at:TIMESTAMPTZ:time.Time:true", "?column?:TEXT:string:true", "next:INT8:int64:true", "length:INT4:int64:true"}, got)
		res := mustExec(t, db, "INSERT INTO t (id) VALUES (1), (2)")
		n, err := res.RowsAffected()
		is.NotError(t, err)
		is.Equal(t, int64(2), n)
		_, err = res.LastInsertId()
		is.True(t, err != nil, "there is no row id")
	})

	t.Run("reports statement errors with errors.Is", func(t *testing.T) {
		verify.Covers(t, "statement errors, including int overflow as ErrOutOfRange, are detectable with errors.Is", "INV-INT-OVERFLOW")
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint NOT NULL)")
		mustExec(t, db, "INSERT INTO t VALUES (1, 1)")
		tests := []struct {
			sql  string
			want error
		}{
			{"INSERT INTO t VALUES (1, 1)", maragudb.ErrDuplicateKey},
			{"INSERT INTO t VALUES (2, NULL)", maragudb.ErrNotNull},
			{"SELECT * FROM nope", maragudb.ErrNoTable},
			{"CREATE TABLE t (id bigint PRIMARY KEY)", maragudb.ErrTableExists},
			{"SELECT 1 / 0", maragudb.ErrDivisionByZero},
			{"SELECT 9223372036854775807 + 1", maragudb.ErrOutOfRange},
			{"SELECT CAST('x' AS bigint)", maragudb.ErrCast},
			{"SELECT 1 + 'a'", maragudb.ErrCast},
			{"SELECT 1 + TRUE", maragudb.ErrType},
			{"SELECT nope FROM t", maragudb.ErrInvalid},
			{"SELEC 1", maragudb.ErrSyntax},
			{"SELECT 1.5", maragudb.ErrUnsupported},
		}
		for _, test := range tests {
			_, err := db.ExecContext(t.Context(), test.sql)
			is.Error(t, test.want, err, test.sql)
		}
	})
}

func TestTx(t *testing.T) {
	t.Run("commits and rolls back through database/sql, with DDL in the transaction", func(t *testing.T) {
		db := newDB(t)
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		mustExec(t, tx, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, tx, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, tx, "UPDATE t SET n = n + 1")
		is.Equal(t, "1 2", queryRows(t, tx, "SELECT * FROM t"))
		is.NotError(t, tx.Rollback())
		_, err = db.ExecContext(t.Context(), "SELECT * FROM t")
		is.Error(t, maragudb.ErrNoTable, err)

		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		mustExec(t, tx, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, tx, "INSERT INTO t VALUES (1, 1)")
		is.NotError(t, tx.Commit())
		is.Equal(t, "1 1", queryRows(t, db, "SELECT * FROM t"))
	})

	t.Run("an explicit transaction returns ErrConflict, which IsRetryable, and a retry succeeds", func(t *testing.T) {
		verify.Covers(t, "an explicit transaction that loses to a concurrent commit returns ErrConflict, which errors.Is and IsRetryable detect, and a retry succeeds", "INV-RETRYABLE-ERROR", "INV-DB-CONFLICTS")
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")

		transfer := func() error {
			tx, err := a.BeginTx(t.Context(), nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var n int64
			if err := tx.QueryRowContext(t.Context(), "SELECT n FROM t WHERE id = 1").Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				// Another process commits after this transaction took its snapshot.
				mustExec(t, b, "UPDATE t SET n = 10 WHERE id = 1")
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE t SET n = $1 WHERE id = 1", n+1); err != nil {
				return err
			}
			return tx.Commit()
		}
		err := transfer()
		is.Error(t, maragudb.ErrConflict, err)
		is.True(t, maragudb.IsRetryable(err))
		is.Equal(t, "10", queryRows(t, a, "SELECT n FROM t"), "the transaction had no effect")
		is.NotError(t, transfer())
		is.Equal(t, "11", queryRows(t, b, "SELECT n FROM t"))
	})

	t.Run("a statement that does not parse aborts the transaction, as in Postgres", func(t *testing.T) {
		verify.Covers(t, "a syntax error or an unsupported statement in an explicit transaction aborts it, so a later commit rolls everything back", "INV-PG-SUBSET")
		for _, bad := range []string{"SELEC 1", "SELECT 1.5"} {
			db := newDB(t)
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
			tx, err := db.BeginTx(t.Context(), nil)
			is.NotError(t, err)
			mustExec(t, tx, "INSERT INTO t VALUES (1)")
			_, err = tx.ExecContext(t.Context(), bad)
			is.True(t, err != nil, bad)
			_, err = tx.ExecContext(t.Context(), "INSERT INTO t VALUES (2)")
			is.Error(t, maragudb.ErrTxAborted, err, bad)
			is.Error(t, maragudb.ErrTxAborted, tx.Commit(), bad)
			is.Equal(t, "", queryRows(t, db, "SELECT id FROM t"), "nothing was committed")
		}
	})

	t.Run("a read-only transaction never conflicts and rejects writes", func(t *testing.T) {
		verify.Covers(t, "a read-only transaction commits despite a concurrent commit and rejects writes", "INV-READ-ONLY")
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")
		tx, err := a.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		is.Equal(t, "0", queryRows(t, tx, "SELECT n FROM t"))
		mustExec(t, b, "UPDATE t SET n = 1")
		is.Equal(t, "0", queryRows(t, tx, "SELECT n FROM t"), "the snapshot holds")
		is.NotError(t, tx.Commit())
		tx, err = a.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "UPDATE t SET n = 2")
		is.Error(t, maragudb.ErrReadOnlyTx, err)
		is.Error(t, maragudb.ErrTxAborted, tx.Commit(), "the failed write aborted the transaction, as in Postgres")
		is.Equal(t, "1", queryRows(t, a, "SELECT n FROM t"))
	})

	t.Run("rejects isolation levels other than serializable", func(t *testing.T) {
		db := newDB(t)
		_, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		is.True(t, err != nil)
		tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelSerializable})
		is.NotError(t, err)
		is.NotError(t, tx.Rollback())
	})

	t.Run("SQL BEGIN, COMMIT, and ROLLBACK work on a dedicated connection", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
		conn, err := db.Conn(t.Context())
		is.NotError(t, err)
		mustExec(t, conn, "BEGIN")
		mustExec(t, conn, "INSERT INTO t VALUES (1)")
		mustExec(t, conn, "ROLLBACK")
		mustExec(t, conn, "BEGIN READ ONLY")
		is.Equal(t, "", queryRows(t, conn, "SELECT * FROM t"))
		mustExec(t, conn, "COMMIT")
		mustExec(t, conn, "BEGIN")
		mustExec(t, conn, "INSERT INTO t VALUES (2)")
		mustExec(t, conn, "COMMIT")
		is.Equal(t, "2", queryRows(t, db, "SELECT * FROM t"))
		// A transaction left open when the connection goes back to the pool is rolled back.
		mustExec(t, conn, "BEGIN")
		mustExec(t, conn, "INSERT INTO t VALUES (3)")
		is.NotError(t, conn.Close())
		is.Equal(t, "2", queryRows(t, db, "SELECT * FROM t"))
	})

	t.Run("a statement outside a transaction retries on conflict", func(t *testing.T) {
		verify.Covers(t, "an autocommit statement that conflicts retries by itself and succeeds", "INV-AUTOCOMMIT-RETRY")
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")
		interfered := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && strings.HasSuffix(key, "manifest") && interfered < 3 {
				interfered++
				decide := w.decide
				w.decide = nil
				mustExec(t, b, "UPDATE t SET n = n + 100")
				w.decide = decide
			}
			return s3fake.OutcomeNormal
		}
		mustExec(t, a, "UPDATE t SET n = n + 1")
		w.decide = nil
		is.Equal(t, 3, interfered)
		is.Equal(t, "301", queryRows(t, a, "SELECT n FROM t"))
	})
}

func TestStats(t *testing.T) {
	t.Run("counts requests and cache hits across connections", func(t *testing.T) {
		verify.Covers(t, "Stats reports request counts by kind and cache hits across connections", "INV-STATS")
		db := newDB(t)
		before, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
		mustExec(t, db, "INSERT INTO t VALUES (1)")
		is.Equal(t, "1", queryRows(t, db, "SELECT * FROM t"))
		after, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		d := after.Requests.Sub(before.Requests)
		is.Equal(t, int64(2), d.PutConditional, "two commits")
		is.Equal(t, int64(2), d.Put, "two segments")
		is.True(t, d.GetConditional >= 3, "a freshness check per statement")
		is.True(t, d.Total() == d.Put+d.PutConditional+d.GetConditional+d.Get)
		is.Equal(t, uint64(2), after.Seq)
		is.Equal(t, 2, after.Segments)
		is.True(t, after.MemoryCacheHits > 0)
	})

	t.Run("compaction and GC bound the objects", func(t *testing.T) {
		verify.Covers(t, "after 11 commits, compaction leaves one segment, and GC deletes the 11 old ones and their garbage batch only after the grace period", "AC-6.1", "AC-6.2")
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			grace := time.Minute
			db := w.open(maragudb.Options{GCGracePeriod: &grace})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
			for i := range 10 {
				mustExec(t, db, "INSERT INTO t VALUES ($1)", i)
			}
			s, err := maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 11, s.Segments)
			is.NotError(t, maragudb.Compact(t.Context(), db))
			s, err = maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 1, s.Segments)
			is.Equal(t, 11, s.Garbage)
			n, err := maragudb.GC(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 0, n, "the first pass stamps the garbage")
			time.Sleep(grace / 2)
			n, err = maragudb.GC(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 0, n, "within the grace period nothing is deleted")
			time.Sleep(grace)
			n, err = maragudb.GC(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 12, n, "the 11 segments and their garbage batch")
			is.Equal(t, "0\n1\n2\n3\n4\n5\n6\n7\n8\n9", queryRows(t, db, "SELECT * FROM t"))
		})
	})

	t.Run("fails on a database from another driver", func(t *testing.T) {
		_, err := maragudb.Stats(t.Context(), sql.OpenDB(otherConnector{}))
		is.True(t, err != nil)
	})
}

func TestDiskCache(t *testing.T) {
	t.Run("keeps the files of the disk cache within DiskCacheBytes, and serves reads from them after a restart", func(t *testing.T) {
		verify.Covers(t, "a database with a disk cache of 64 KiB, which commits and reads many times that, keeps the files of its disk cache within the bound after every statement, and a database opened again on the directory reads from it", "INV-DISK-CACHE-BOUND")
		w := newWorld(t)
		dir := t.TempDir()
		const bound = 64 << 10
		opts := maragudb.Options{DiskCacheDir: &dir, DiskCacheBytes: new(int64(bound)), MemoryCacheBytes: new(int64)}
		db := w.open(opts)
		var peak int64
		check := func() {
			t.Helper()
			var total int64
			is.NotError(t, filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
				if err != nil || e.IsDir() {
					return err
				}
				info, err := e.Info()
				if err != nil {
					return err
				}
				total += cache.FileBytes(info.Size())
				return nil
			}))
			// No statement runs during the check, so not even the file of a write in progress may
			// exceed the bound.
			is.True(t, total <= bound, fmt.Sprintf("the disk cache takes %d bytes, more than %d", total, bound))
			peak = max(peak, total)
		}
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, name text)")
		for i := range 60 {
			mustExec(t, db, "INSERT INTO t VALUES ($1, $2)", i, strings.Repeat("x", 2000))
			check()
			is.Equal(t, "1", queryRows(t, db, "SELECT count(*) FROM t WHERE id = $1", i/2))
			check()
		}
		is.True(t, peak > bound-4*4096, fmt.Sprintf("the disk cache filled up, to %d bytes", peak))
		is.NotError(t, db.Close())

		db2 := w.open(opts)
		before, err := maragudb.Stats(t.Context(), db2)
		is.NotError(t, err)
		is.Equal(t, "1", queryRows(t, db2, "SELECT count(*) FROM t WHERE id = 59"))
		after, err := maragudb.Stats(t.Context(), db2)
		is.NotError(t, err)
		is.True(t, after.DiskCacheHits > before.DiskCacheHits, "the blocks of the last commit are on disk")
		check()
	})

	t.Run("shares the disk cache of a DSN between databases in one process, and fails with ErrDiskCacheInUse in another process", func(t *testing.T) {
		verify.Covers(t, "two databases of one DSN with one DiskCacheDir in one process both work, and another process that opens the DSN with that directory fails with ErrDiskCacheInUse", "INV-DISK-CACHE-BOUND")
		w := newWorld(t)
		dir := t.TempDir()
		opts := maragudb.Options{DiskCacheDir: &dir}
		db1 := w.open(opts)
		db2 := w.open(opts)
		is.NotError(t, db1.PingContext(t.Context()))
		is.NotError(t, db2.PingContext(t.Context()))

		cmd := exec.Command(os.Args[0], "-test.run=^TestDiskCacheHelperProcess$")
		cmd.Env = helperEnv(diskCacheHelperEnv + "=" + dir)
		out, err := cmd.Output()
		is.NotError(t, err)
		is.Equal(t, "in use", firstLine(out))

		is.NotError(t, db1.Close())
		is.NotError(t, db2.Close())
		cmd = exec.Command(os.Args[0], "-test.run=^TestDiskCacheHelperProcess$")
		cmd.Env = helperEnv(diskCacheHelperEnv + "=" + dir)
		out, err = cmd.Output()
		is.NotError(t, err)
		is.Equal(t, "open", firstLine(out), "closing both databases released the directory")
	})
}

// helperEnv is the environment of this process with the variable set, but without the directory
// of the verification facts, so that a helper process records none.
func helperEnv(variable string) []string {
	var env []string
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "MARAGUDB_VERIFY_DIR=") {
			env = append(env, v)
		}
	}
	return append(env, variable)
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return line
}

// diskCacheHelperEnv names the disk cache directory of [TestDiskCacheHelperProcess].
const diskCacheHelperEnv = "MARAGUDB_DISK_CACHE_HELPER_DIR"

// TestDiskCacheHelperProcess opens a database with the disk cache directory of
// [diskCacheHelperEnv], in a process of its own, and prints whether it could.
func TestDiskCacheHelperProcess(t *testing.T) {
	dir := os.Getenv(diskCacheHelperEnv)
	if dir == "" {
		t.Skip("only runs as a helper process")
	}
	c, err := maragudb.NewConnector(maragudb.Options{DiskCacheDir: &dir, Maintenance: new(false), Store: s3test.NewStore(s3fake.New())}, "s3://bucket/app")
	if err != nil {
		fmt.Println(err)
		return
	}
	db := sql.OpenDB(c)
	defer func() { _ = db.Close() }()
	err = db.PingContext(t.Context())
	switch {
	case errors.Is(err, maragudb.ErrDiskCacheInUse):
		fmt.Println("in use")
	case err != nil:
		fmt.Println(err)
	default:
		fmt.Println("open")
	}
}

func TestMaintenance(t *testing.T) {
	t.Run("writes manifests no larger after 10,000 commits than after 1,000, with the default grace period and maintenance interval", func(t *testing.T) {
		verify.Covers(t, "with the default grace period and maintenance, a commit every 300 ms for 50 minutes, so that no garbage expires, writes manifests whose bytes beside the live segments grow by at most 2 KiB from the first 1,000 commits to the last, with at most 68 live segments: the 50 commits of the longest interval between maintenance steps, and three per tier of a database of 10,000 rows", "AC-6.4")
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			db := w.open(maragudb.Options{Maintenance: new(true)})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, n bigint)")
			// The manifest that a commit wrote is read from the store right after the commit. The
			// bytes of its live segments depend on when maintenance last compacted, which the
			// jitter of the interval makes random, so they are counted apart and only bounded.
			// Everything else is compared, as the largest among commits 100 to 1,099 and among
			// the last 1,000. Between those, every tenth commit counts toward the bound on segments.
			var early, late, segments, most int
			for i := range 10_000 {
				mustExec(t, db, "INSERT INTO t VALUES ($1, $1)", i)
				if i%10 != 0 && (i < 100 || (i >= 1_100 && i < 9_000)) {
					time.Sleep(300 * time.Millisecond)
					continue
				}
				body, ok := w.fake.Body("app/manifest")
				is.True(t, ok)
				var m struct {
					Segments json.RawMessage `json:"segments"`
				}
				is.NotError(t, json.Unmarshal(body, &m))
				var segs []json.RawMessage
				is.NotError(t, json.Unmarshal(m.Segments, &segs))
				most = max(most, len(body))
				segments = max(segments, len(segs))
				rest := len(body) - len(m.Segments)
				switch {
				case i >= 100 && i < 1_100:
					early = max(early, rest)
				case i >= 9_000:
					late = max(late, rest)
				}
				time.Sleep(300 * time.Millisecond)
			}
			t.Logf("largest manifest of a commit: %d bytes, beside the segments %d bytes after 1,000 commits and %d after 10,000, at most %d segments", most, early, late, segments)
			is.True(t, late <= early+2048, fmt.Sprintf("%d bytes after 1,000 commits, %d after 10,000", early, late))
			// The longest interval between two steps is one and a half intervals, 15 seconds, in which
			// 50 commits add a segment each. Below them, the tiers of a database of 10,000 rows of at
			// most 100 bytes hold fewer than four segments each: tier 0 below 1 KiB, and five tiers
			// four times larger each.
			is.True(t, segments <= 50+3*6, fmt.Sprint(segments, " segments"))
			s, err := maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.True(t, s.Garbage > 9_000, fmt.Sprintf("the garbage of the last 50 minutes is kept: %d objects", s.Garbage))
		})
	})

	t.Run("compacts and collects garbage in the background and reports it in the statistics", func(t *testing.T) {
		verify.Covers(t, "background maintenance compacts and collects garbage and reports it in Stats", "AC-6.1")
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			grace := time.Minute
			interval := time.Second
			db := w.open(maragudb.Options{Maintenance: new(true), MaintenanceInterval: &interval, CompactionFanIn: new(4), GCGracePeriod: &grace})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
			for i := range 3 {
				mustExec(t, db, "INSERT INTO t VALUES ($1)", i)
			}
			s, err := maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 4, s.Segments)
			time.Sleep(2 * interval)
			s, err = maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 1, s.Segments, "four segments in tier 0 are merged")
			is.Equal(t, 5, s.Garbage, "the old segments are stamped, in a garbage batch")
			is.Equal(t, int64(1), s.MaintenanceRuns)
			is.Equal(t, int64(1), s.LeaseAcquisitions)
			is.Equal(t, int64(0), s.LeaseLosses)
			time.Sleep(grace + 2*interval)
			s, err = maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 0, s.Garbage, "deleted after the grace period")
			is.Equal(t, int64(2), s.MaintenanceRuns)
			is.Equal(t, "0\n1\n2", queryRows(t, db, "SELECT * FROM t ORDER BY id"))
		})
	})

	t.Run("keeps the commit and warm-read budgets in steady state", func(t *testing.T) {
		verify.Covers(t, "with background maintenance, an INSERT and a warm SELECT keep their request budgets", "AC-5.1", "AC-5.2")
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			interval := time.Second
			db := w.open(maragudb.Options{Maintenance: new(true), MaintenanceInterval: &interval, CompactionFanIn: new(4)})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, name text)")
			stats := func() maragudb.Statistics {
				s, err := maragudb.Stats(t.Context(), db)
				is.NotError(t, err)
				return s
			}
			// A steady client: one insert and one lookup per interval, with maintenance running
			// in between. The budget of each statement is unchanged, and the segments stay bounded.
			var maintenance maragudb.Requests
			for i := range 40 {
				before := stats()
				mustExec(t, db, "INSERT INTO t VALUES ($1, $2)", i, "n")
				d := stats().Requests.Sub(before.Requests)
				is.Equal(t, maragudb.Requests{GetConditional: 1, Put: 1, PutConditional: 1}, d, fmt.Sprint("insert ", i))

				before = stats()
				is.Equal(t, `"n"`, queryRows(t, db, "SELECT name FROM t WHERE id = $1", i))
				d = stats().Requests.Sub(before.Requests)
				is.Equal(t, maragudb.Requests{GetConditional: 1}, d, fmt.Sprint("select ", i))

				before = stats()
				time.Sleep(2 * interval)
				maintenance = maintenance.Add(stats().Requests.Sub(before.Requests))
				is.True(t, stats().Segments <= 4, fmt.Sprint("segments ", stats().Segments))
			}
			s := stats()
			t.Logf("40 inserts: %d maintenance runs, %d lease acquisitions, maintenance requests %+v", s.MaintenanceRuns, s.LeaseAcquisitions, maintenance)
			is.True(t, s.MaintenanceRuns >= 10, "a compaction every four commits")
			is.Equal(t, s.LeaseAcquisitions, s.MaintenanceRuns, "one lease acquisition per run")
			is.Equal(t, int64(0), s.LeaseLosses)
			is.True(t, maintenance.Total() <= 12*s.MaintenanceRuns, fmt.Sprintf("%+v for %d runs", maintenance, s.MaintenanceRuns))
		})
	})

	t.Run("an idle client makes no requests between runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			interval := time.Second
			db := w.open(maragudb.Options{Maintenance: new(true), MaintenanceInterval: &interval, CompactionFanIn: new(4)})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
			for i := range 3 {
				mustExec(t, db, "INSERT INTO t VALUES ($1)", i)
			}
			time.Sleep(2 * interval)
			s, err := maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, int64(1), s.MaintenanceRuns)
			before := s.Requests
			time.Sleep(20 * interval)
			s, err = maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, int64(0), s.Requests.Sub(before).Total(), "nothing to do within the grace period")
		})
	})

	t.Run("is off when disabled", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			interval := time.Second
			db := w.open(maragudb.Options{Maintenance: new(false), MaintenanceInterval: &interval, CompactionFanIn: new(2)})
			mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
			for i := range 3 {
				mustExec(t, db, "INSERT INTO t VALUES ($1)", i)
			}
			time.Sleep(10 * interval)
			s, err := maragudb.Stats(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, 4, s.Segments)
			is.Equal(t, int64(0), s.MaintenanceRuns)
		})
	})
}

// maintain the database behind db as background maintenance does, with the default tiers and
// without the lease, until no compaction is due.
func maintain(t *testing.T, db *sql.DB) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	is.NotError(t, err)
	defer func() { _ = conn.Close() }()
	is.NotError(t, conn.Raw(func(dc any) error {
		m := dc.(*sqldriver.Conn).DB().NewMaintainer(engine.MaintenanceOptions{DisableLease: true})
		for {
			res, err := m.Step(t.Context())
			if err != nil || !res.Compacted {
				return err
			}
		}
	}))
}

// churn the database behind db with small commits, each a statement that stmt makes from a
// random number source, and with maintenance after each, so that the database has segments of
// several tiers, as it does after many commits, instead of the one that a full compaction leaves.
func churn(t *testing.T, db *sql.DB, commits int, stmt func(rng *rand.Rand, i int) (string, []any)) {
	t.Helper()
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range commits {
		q, args := stmt(rng, i)
		mustExec(t, db, q, args...)
		maintain(t, db)
	}
}

// segments of the database behind db, which must be the bound of the tiers on the bytes of a
// database of n rows of at most rowBytes bytes each in the store, with the default fan-in of
// four: fewer than four segments in each tier up to that of all the bytes.
func segments(t *testing.T, db *sql.DB, n, rowBytes int) int {
	t.Helper()
	s, err := maragudb.Stats(t.Context(), db)
	is.NotError(t, err)
	// Tier 0 is below 1 KiB, and every tier above is four times as large.
	tiers := 1
	for limit := 1 << 10; limit <= n*rowBytes; limit *= 4 {
		tiers++
	}
	is.True(t, s.Segments <= 3*tiers, fmt.Sprintf("n=%d: %d segments, at most %d", n, s.Segments, 3*tiers))
	return s.Segments
}

func TestRequestBudgets(t *testing.T) {
	sizes := []int{100, 1_000, 10_000, 100_000}
	load := func(t *testing.T, db *sql.DB, n int) {
		t.Helper()
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, name text, f double precision)")
		const batch = 1000
		for i := 0; i < n; i += batch {
			var b strings.Builder
			b.WriteString("INSERT INTO t VALUES ")
			for j := i; j < min(i+batch, n); j++ {
				if j > i {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "(%d, 'name-%d', %d.5)", j, j, j)
			}
			mustExec(t, db, b.String())
			maintain(t, db)
		}
		// Updates of random rows, inserts of rows that the tests do not use, and deletes of them.
		churn(t, db, 150, func(rng *rand.Rand, i int) (string, []any) {
			switch i % 3 {
			case 0:
				return "UPDATE t SET f = f + 1 WHERE id = $1", []any{rng.IntN(n)}
			case 1:
				return "INSERT INTO t VALUES ($1, 'churn', 0.5)", []any{2*n + i}
			}
			return "DELETE FROM t WHERE id = $1", []any{2*n + i - 1}
		})
	}
	stats := func(t *testing.T, db *sql.DB) maragudb.Requests {
		t.Helper()
		s, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		return s.Requests
	}

	maxDepth := func(n int) int {
		// Blocks of 16 KiB hold at least 64 of these rows, so the depth is at most 1 + log64(n).
		depth := 1
		for m := n; m > 1; m = (m + 63) / 64 {
			depth++
		}
		return depth
	}

	t.Run("an autocommit INSERT makes one conditional GET and two PUTs regardless of database size", func(t *testing.T) {
		verify.Covers(t, "an autocommit INSERT makes one conditional GET and two PUTs at every database size, after many commits with the merges of the tiers, and after a compaction of every segment", "AC-5.1")
		verify.Covers(t, "an autocommit INSERT with a cold cache makes at most one GET per tree level of every segment for the table definition and the duplicate key check, after many commits with the merges of the tiers, which leave fewer than four segments per tier, and after a compaction of every segment", "AC-5.3")
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			load(t, db, n)
			for phase, state := range []string{"tiered", "compacted"} {
				if state == "compacted" {
					is.NotError(t, maragudb.Compact(t.Context(), db))
				}
				// Warm the cache with a point lookup, which reads the table definition and a row.
				is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2+phase)), queryRows(t, db, "SELECT name FROM t WHERE id = $1", n/2+phase))
				before := stats(t, db)
				mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", n+1+phase, "new", 0.5)
				d := stats(t, db).Sub(before)
				t.Logf("n=%d %s warm insert: %+v", n, state, d)
				verify.CheckBudget(t, "SQL autocommit INSERT, warm, "+state+": requests", n, d.Total(), 3)
				is.Equal(t, maragudb.Requests{GetConditional: 1, Put: 1, PutConditional: 1}, d)

				// Cold, the duplicate key check and the table definition each cost a descent of the
				// tree of every segment.
				mustExec(t, db, "DELETE FROM t WHERE id = $1", n/2)
				cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
				limit := int64(2 * segments(t, cold, n, 100) * maxDepth(n))
				before = stats(t, cold)
				mustExec(t, cold, "INSERT INTO t VALUES ($1, $2, $3)", n/2, fmt.Sprint("name-", n/2), 0.5)
				d = stats(t, cold).Sub(before)
				t.Logf("n=%d %s cold insert: %+v, limit %d", n, state, d, limit)
				verify.CheckBudget(t, "SQL autocommit INSERT, cold, "+state+": PUTs", n, d.Put+d.PutConditional, 2)
				verify.CheckBudget(t, "SQL autocommit INSERT, cold, "+state+": GETs", n, d.Get, limit)
				is.Equal(t, maragudb.Requests{Get: d.Get, GetConditional: 1, Put: 1, PutConditional: 1}, d)
			}
		}
	})

	t.Run("a SELECT by primary key with a warm cache makes exactly one conditional GET", func(t *testing.T) {
		verify.Covers(t, "a SELECT by primary key with a warm cache makes exactly one conditional GET", "AC-5.2")
		for _, n := range sizes[:3] {
			db := newDB(t)
			load(t, db, n)
			q := "SELECT name FROM t WHERE id = $1"
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, db, q, n/2))
			before := stats(t, db)
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, db, q, n/2))
			d := stats(t, db).Sub(before)
			t.Logf("n=%d warm select: %+v", n, d)
			verify.CheckBudget(t, "SQL SELECT by key, warm: requests", n, d.Total(), 1)
			is.Equal(t, maragudb.Requests{GetConditional: 1}, d)
		}
	})

	t.Run("a SELECT by primary key with a cold cache makes a logarithmic number of GETs", func(t *testing.T) {
		verify.Covers(t, "a SELECT by primary key with a cold cache makes at most one GET per tree level of every segment for the table definition and the row, after many commits with the merges of the tiers, which leave fewer than four segments per tier, and one GET per tree level after a compaction of every segment", "AC-5.3")
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			load(t, db, n)
			for _, state := range []string{"tiered", "compacted"} {
				if state == "compacted" {
					is.NotError(t, maragudb.Compact(t.Context(), db))
				}
				cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
				// The table definition and the row are each found by one descent of the tree of each
				// segment, one block per level.
				limit := int64(2 * segments(t, cold, n, 100) * maxDepth(n))
				before := stats(t, cold)
				is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, cold, "SELECT name FROM t WHERE id = $1", n/2))
				d := stats(t, cold).Sub(before)
				t.Logf("n=%d %s cold select: %+v, limit %d", n, state, d, limit)
				verify.CheckBudget(t, "SQL SELECT by key, cold, "+state+": GETs", n, d.Get, limit)
				is.Equal(t, int64(1), d.GetConditional, "one freshness check")
				is.Equal(t, int64(0), d.Put+d.PutConditional+d.List+d.Delete)
			}
		}
	})
}

func TestRequestBudgets_Indexes(t *testing.T) {
	sizes := []int{100, 1_000, 10_000, 100_000}
	// load a table with a unique index and another index, whose entries are keys next to the
	// rows, so the tree holds about three keys per row.
	load := func(t *testing.T, db *sql.DB, n int) {
		t.Helper()
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, email text UNIQUE, n bigint, name text)")
		mustExec(t, db, "CREATE INDEX t_n ON t (n)")
		const batch = 1000
		for i := 0; i < n; i += batch {
			var b strings.Builder
			b.WriteString("INSERT INTO t VALUES ")
			for j := i; j < min(i+batch, n); j++ {
				if j > i {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "(%d, 'user-%d@example.com', %d, 'name-%d')", j, j, j%97, j)
			}
			mustExec(t, db, b.String())
			// The unique check of every row looks in every segment whose keys span the index, so
			// the load compacts as it goes.
			maintain(t, db)
		}
		// Updates of the indexed column n of random rows, inserts of rows that the tests do not
		// use, and deletes of them.
		churn(t, db, 150, func(rng *rand.Rand, i int) (string, []any) {
			switch i % 3 {
			case 0:
				return "UPDATE t SET n = n + 1 WHERE id = $1", []any{rng.IntN(n)}
			case 1:
				return "INSERT INTO t VALUES ($1, $2, 1, 'churn')", []any{2*n + i, fmt.Sprintf("churn-%d@example.com", i)}
			}
			return "DELETE FROM t WHERE id = $1", []any{2*n + i - 1}
		})
	}
	stats := func(t *testing.T, db *sql.DB) maragudb.Requests {
		t.Helper()
		s, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		return s.Requests
	}
	maxDepth := func(keys int) int {
		// Blocks of 16 KiB hold at least 64 rows or entries, so the depth is at most 1 + log64(n).
		depth := 1
		for m := keys; m > 1; m = (m + 63) / 64 {
			depth++
		}
		return depth
	}
	loaded := loadedWorlds(t, load)

	t.Run("a point lookup through a unique index with a cold cache makes a logarithmic number of GETs", func(t *testing.T) {
		verify.Covers(t, "a SELECT by a unique secondary index with a cold cache makes a logarithmic number of GETs: one descent of the tree of every segment each for the table definition, the index entry, and the row, after many commits with the merges of the tiers, which leave fewer than four segments per tier, and after a compaction of every segment", "AC-5.5")
		for _, n := range sizes {
			w := loaded(t, n)
			for _, state := range []string{"tiered", "compacted"} {
				if state == "compacted" {
					is.NotError(t, maragudb.Compact(t.Context(), w.open(maragudb.Options{})))
				}
				cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
				// A scan of the entry can read the next leaf of every segment too.
				segs := segments(t, cold, n, 300)
				limit := int64(segs * (3*maxDepth(3*n) + 1))
				before := stats(t, cold)
				is.Equal(t, fmt.Sprint(n/2), queryRows(t, cold, "SELECT id FROM t WHERE email = $1", fmt.Sprintf("user-%d@example.com", n/2)))
				d := stats(t, cold).Sub(before)
				t.Logf("n=%d %s cold select through the index: %+v, limit %d", n, state, d, limit)
				verify.CheckBudget(t, "SQL SELECT by unique index, cold, "+state+": GETs", n, d.Get, limit)
				is.Equal(t, int64(1), d.GetConditional, "one freshness check")
				is.Equal(t, int64(0), d.Put+d.PutConditional+d.List+d.Delete)
			}
		}
	})

	t.Run("a commit that changes indexed columns makes one conditional GET and two PUTs regardless of database size", func(t *testing.T) {
		verify.Covers(t, "an autocommit UPDATE that changes the columns of two indexes, and so deletes and writes entries of both, makes one conditional GET and two PUTs at every database size", "AC-5.6", "AC-5.1")
		for _, n := range sizes {
			db := loaded(t, n).open(maragudb.Options{})
			// Warm the cache with the same statement on another row, and with reads of the row and
			// of the index entry that the measured statement checks.
			mustExec(t, db, "UPDATE t SET email = $1, n = n + 1 WHERE id = $2", "warm@example.com", n/3)
			is.Equal(t, fmt.Sprint(n/2), queryRows(t, db, "SELECT id FROM t WHERE id = $1", n/2))
			is.Equal(t, "", queryRows(t, db, "SELECT id FROM t WHERE email = $1", "moved@example.com"))
			before := stats(t, db)
			mustExec(t, db, "UPDATE t SET email = $1, n = n + 1 WHERE id = $2", "moved@example.com", n/2)
			d := stats(t, db).Sub(before)
			t.Logf("n=%d warm update of indexed columns: %+v", n, d)
			verify.CheckBudget(t, "SQL autocommit UPDATE of indexed columns: PUTs", n, d.Put+d.PutConditional, 2)
			is.Equal(t, int64(1), d.Put)
			is.Equal(t, int64(1), d.PutConditional)
			is.Equal(t, int64(1), d.GetConditional)
			is.Equal(t, int64(0), d.Get, "the cache is warm")
		}
	})
}

func TestRequestBudgets_ForeignKeys(t *testing.T) {
	sizes := []int{100, 1_000, 10_000, 100_000}
	// load parents, and a child table that references them.
	load := func(t *testing.T, db *sql.DB, n int) {
		t.Helper()
		mustExec(t, db, "CREATE TABLE p (id bigint PRIMARY KEY, name text)")
		mustExec(t, db, "CREATE TABLE c (id bigint PRIMARY KEY, pid bigint REFERENCES p)")
		const batch = 1000
		for i := 0; i < n; i += batch {
			var b strings.Builder
			b.WriteString("INSERT INTO p VALUES ")
			for j := i; j < min(i+batch, n); j++ {
				if j > i {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "(%d, 'name-%d')", j, j)
			}
			mustExec(t, db, b.String())
			maintain(t, db)
		}
		// Updates of random parents, inserts of parents that the tests do not use, and deletes of
		// them.
		churn(t, db, 150, func(rng *rand.Rand, i int) (string, []any) {
			switch i % 3 {
			case 0:
				return "UPDATE p SET name = 'churn' WHERE id = $1", []any{rng.IntN(n)}
			case 1:
				return "INSERT INTO p VALUES ($1, 'churn')", []any{2*n + i}
			}
			return "DELETE FROM p WHERE id = $1", []any{2*n + i - 1}
		})
	}
	stats := func(t *testing.T, db *sql.DB) maragudb.Requests {
		t.Helper()
		s, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		return s.Requests
	}
	maxDepth := func(n int) int {
		// Blocks of 16 KiB hold at least 64 of these rows, so the depth is at most 1 + log64(n).
		depth := 1
		for m := n; m > 1; m = (m + 63) / 64 {
			depth++
		}
		return depth
	}

	t.Run("an insert of a row with a foreign key adds no request with a warm cache, and a logarithmic number of GETs with a cold one", func(t *testing.T) {
		verify.Covers(t, "an autocommit INSERT of a row that references a parent makes one conditional GET and two PUTs with a warm cache at every database size, and with a cold cache one descent of the tree of every segment each for the table definitions, the duplicate key check, and the lookup of the parent, after many commits with the merges of the tiers, and after a compaction of every segment", "AC-5.7", "AC-5.1")
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			load(t, db, n)
			for phase, state := range []string{"tiered", "compacted"} {
				if state == "compacted" {
					is.NotError(t, maragudb.Compact(t.Context(), db))
				}
				// Warm the cache with a child of the same parent.
				mustExec(t, db, "INSERT INTO c VALUES ($1, $2)", 10*phase+1, n/2)
				before := stats(t, db)
				mustExec(t, db, "INSERT INTO c VALUES ($1, $2)", 10*phase+2, n/2)
				d := stats(t, db).Sub(before)
				t.Logf("n=%d %s warm insert of a child: %+v", n, state, d)
				verify.CheckBudget(t, "SQL autocommit INSERT with a foreign key, warm, "+state+": requests", n, d.Total(), 3)
				is.Equal(t, maragudb.Requests{GetConditional: 1, Put: 1, PutConditional: 1}, d)

				cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
				// The table definitions, the duplicate key in the small child table, and the parent.
				limit := int64(segments(t, cold, n, 100) * (3*maxDepth(n) + 2))
				before = stats(t, cold)
				mustExec(t, cold, "INSERT INTO c VALUES ($1, $2)", 10*phase+3, n/3)
				d = stats(t, cold).Sub(before)
				t.Logf("n=%d %s cold insert of a child: %+v, limit %d", n, state, d, limit)
				verify.CheckBudget(t, "SQL autocommit INSERT with a foreign key, cold, "+state+": GETs", n, d.Get, limit)
				is.Equal(t, maragudb.Requests{Get: d.Get, GetConditional: 1, Put: 1, PutConditional: 1}, d)
			}
		}
	})
}

func TestRequestBudgets_Joins(t *testing.T) {
	t.Run("a join from one parent row to its children through an index on their foreign key makes a logarithmic number of GETs per row with a cold cache", func(t *testing.T) {
		verify.Covers(t, "a SELECT that joins one parent row to its children with ON c.pid = p.id, through an index on c (pid), makes with a cold cache one descent of the tree of every segment for each table definition, for the parent, for the index entries, and for each child, at every database size, after many commits with the merges of the tiers, which leave fewer than four segments per tier, and after a compaction of every segment", "AC-5.8")
		const children = 3
		for _, n := range []int{100, 1_000, 10_000, 100_000} {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			mustExec(t, db, "CREATE TABLE p (id bigint PRIMARY KEY, name text)")
			mustExec(t, db, "CREATE TABLE c (id bigint PRIMARY KEY, pid bigint REFERENCES p)")
			mustExec(t, db, "CREATE INDEX c_pid ON c (pid)")
			const batch = 1000
			for _, table := range []string{"p", "c"} {
				for i := 0; i < n; i += batch {
					var b strings.Builder
					b.WriteString("INSERT INTO " + table + " VALUES ")
					for j := i; j < min(i+batch, n); j++ {
						if j > i {
							b.WriteString(", ")
						}
						if table == "p" {
							fmt.Fprintf(&b, "(%d, 'name-%d')", j, j)
						} else {
							// Most children have no parent, which makes them as many as the parents
							// without a foreign key check each, and fills the index with them.
							pid := "NULL"
							if j%10 == 0 {
								pid = fmt.Sprint(j)
							}
							fmt.Fprintf(&b, "(%d, %s)", j, pid)
						}
					}
					mustExec(t, db, b.String())
					maintain(t, db)
				}
			}
			for k := range children {
				mustExec(t, db, "INSERT INTO c VALUES ($1, $2)", n+k, n/2+1)
				maintain(t, db)
			}
			// Renames of random parents, inserts of children without a parent that the join does
			// not read, and deletes of them.
			churn(t, db, 150, func(rng *rand.Rand, i int) (string, []any) {
				switch i % 3 {
				case 0:
					return "UPDATE p SET name = 'churn' WHERE id = $1", []any{rng.IntN(n)}
				case 1:
					return "INSERT INTO c VALUES ($1, NULL)", []any{2*n + i}
				}
				return "DELETE FROM c WHERE id = $1", []any{2*n + i - 1}
			})
			for _, state := range []string{"tiered", "compacted"} {
				if state == "compacted" {
					is.NotError(t, maragudb.Compact(t.Context(), db))
				}
				cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
				// Blocks of 16 KiB hold at least 64 rows or entries, so a descent of the tree of a
				// segment is at most 1 + log64(n) GETs.
				depth := int64(1)
				for m := 2 * n; m > 1; m = (m + 63) / 64 {
					depth++
				}
				// The two table definitions, the parent, the index entries, and each child.
				limit := int64(segments(t, cold, 2*n, 100)) * depth * (2 + 1 + 1 + children)
				s, err := maragudb.Stats(t.Context(), cold)
				is.NotError(t, err)
				rows, err := cold.QueryContext(t.Context(), "SELECT p.name, c.id FROM p JOIN c ON c.pid = p.id WHERE p.id = $1", n/2+1)
				is.NotError(t, err)
				found := 0
				for rows.Next() {
					found++
				}
				is.NotError(t, rows.Err())
				is.NotError(t, rows.Close())
				is.Equal(t, children, found)
				after, err := maragudb.Stats(t.Context(), cold)
				is.NotError(t, err)
				d := after.Requests.Sub(s.Requests)
				t.Logf("n=%d %s cold join of a parent to its %d children: %+v, limit %d", n, state, children, d, limit)
				verify.CheckBudget(t, "SQL SELECT that joins a parent to its children through an index, cold, "+state+": GETs", n, d.Get, limit)
				is.Equal(t, maragudb.Requests{Get: d.Get, GetConditional: 1}, d)
			}
		}
	})
}

// nullableValuer is a Valuer with a value receiver, so a nil pointer to it must be NULL.
type nullableValuer struct{}

func (nullableValuer) Value() (driver.Value, error) { return "never", nil }

// otherConnector is a connector of some other driver.
type otherConnector struct{}

func (otherConnector) Connect(context.Context) (driver.Conn, error) { return otherConn{}, nil }
func (otherConnector) Driver() driver.Driver                        { return nil }

type otherConn struct{}

func (otherConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("no") }
func (otherConn) Close() error                        { return nil }
func (otherConn) Begin() (driver.Tx, error)           { return nil, errors.New("no") }
