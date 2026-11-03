package maragudb_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
)

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

// open a database over the fake with sql.OpenDB.
func (w *world) open(opts maragudb.Options) *sql.DB {
	w.t.Helper()
	opts.Client = w.fake
	c, err := maragudb.NewConnector(opts, "s3://bucket/app")
	is.NotError(w.t, err)
	db := sql.OpenDB(c)
	w.t.Cleanup(func() { _ = db.Close() })
	return db
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
		w := newWorld(t)
		maragudb.RegisterDriver(maragudb.Options{Name: new("maragudb-test-register"), Client: w.fake})
		db, err := sql.Open("maragudb-test-register", "s3://bucket/app")
		is.NotError(t, err)
		defer func() { _ = db.Close() }()
		is.NotError(t, db.PingContext(t.Context()))
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, name string)")
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2)", 1, "a")
		is.Equal(t, `1 "a"`, queryRows(t, db, "SELECT * FROM t"))
	})

	t.Run("rejects a bad DSN when connecting", func(t *testing.T) {
		w := newWorld(t)
		for _, dsn := range []string{"bucket/app", "s3:///app", "http://bucket/app", "s3://bucket/app?x=1", "s3://user@bucket/app"} {
			_, err := maragudb.NewConnector(maragudb.Options{Client: w.fake}, dsn)
			is.Error(t, maragudb.ErrDSN, err, dsn)
		}
		c, err := maragudb.NewConnector(maragudb.Options{Client: w.fake}, "s3://bucket")
		is.NotError(t, err, "an empty prefix is fine")
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		is.NotError(t, db.PingContext(t.Context()))
	})
}

func TestDB(t *testing.T) {
	t.Run("round trips every type through parameters and Scan", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, f float, b bool, s string, bs bytes, at time, n int)")
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
		is.True(t, got.Equal(at))
		is.Equal(t, time.UTC, got.Location(), "times come back in UTC")
		is.True(t, !n.Valid)

		row = db.QueryRowContext(t.Context(), "SELECT f, s, n FROM t WHERE id = $1", 2)
		var nf sql.NullFloat64
		var ns sql.NullString
		is.NotError(t, row.Scan(&nf, &ns, &n))
		is.True(t, !nf.Valid && !ns.Valid && !n.Valid)
	})

	t.Run("never converts a parameter of the wrong type", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, f float, s string)")
		_, err := db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", "1", 1.5, "x")
		is.Error(t, maragudb.ErrType, err, "a string for an int column")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 1, 2, "x")
		is.Error(t, maragudb.ErrType, err, "an int for a float column")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 1, 1.5, 3)
		is.Error(t, maragudb.ErrType, err, "an int for a string column")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", int32(1), 1.5, "x")
		is.True(t, err != nil && strings.Contains(err.Error(), "unsupported parameter type int32"), fmt.Sprint(err))
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 1, 1.5)
		is.True(t, err != nil && strings.Contains(err.Error(), "3 parameters, 2 given"), fmt.Sprint(err))
		is.Equal(t, "", queryRows(t, db, "SELECT * FROM t"))
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", sql.NullInt64{Int64: 7, Valid: true}, sql.NullFloat64{}, sql.NullString{String: "v", Valid: true})
		is.Equal(t, `7 NULL "v"`, queryRows(t, db, "SELECT * FROM t"), "driver.Valuer parameters are unwrapped")
		var nilValuer *nullableValuer
		mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", 8, nilValuer, nil)
		is.Equal(t, `7 NULL "v"`+"\n"+`8 NULL NULL`, queryRows(t, db, "SELECT * FROM t"), "a nil pointer Valuer is NULL")
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2, $3)", 9, 1.5, "x", "extra")
		is.True(t, err != nil && strings.Contains(err.Error(), "3 parameters, 4 given"), fmt.Sprint(err))
	})

	t.Run("prepares statements and reports parameter count mismatches", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, f float, b bool, s string, bs bytes, at time)")
		rows, err := db.QueryContext(t.Context(), "SELECT id, f, b, s, bs, at, NULL, id + 1 AS next FROM t")
		is.NotError(t, err)
		defer func() { _ = rows.Close() }()
		types, err := rows.ColumnTypes()
		is.NotError(t, err)
		var got []string
		for _, ct := range types {
			nullable, _ := ct.Nullable()
			got = append(got, fmt.Sprintf("%s:%s:%v:%v", ct.Name(), ct.DatabaseTypeName(), ct.ScanType(), nullable))
		}
		is.EqualSlice(t, []string{"id:INT:int64:false", "f:FLOAT:float64:true", "b:BOOL:bool:true", "s:STRING:string:true", "bs:BYTES:[]uint8:true", "at:TIME:time.Time:true", "?column?::interface {}:true", "next:INT:int64:true"}, got)
		res := mustExec(t, db, "INSERT INTO t (id) VALUES (1), (2)")
		n, err := res.RowsAffected()
		is.NotError(t, err)
		is.Equal(t, int64(2), n)
		_, err = res.LastInsertId()
		is.True(t, err != nil, "there is no row id")
	})

	t.Run("reports statement errors with errors.Is", func(t *testing.T) {
		db := newDB(t)
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, n int NOT NULL)")
		mustExec(t, db, "INSERT INTO t VALUES (1, 1)")
		tests := []struct {
			sql  string
			want error
		}{
			{"INSERT INTO t VALUES (1, 1)", maragudb.ErrDuplicateKey},
			{"INSERT INTO t VALUES (2, NULL)", maragudb.ErrNotNull},
			{"SELECT * FROM nope", maragudb.ErrNoTable},
			{"CREATE TABLE t (id int PRIMARY KEY)", maragudb.ErrTableExists},
			{"SELECT 1 / 0", maragudb.ErrDivisionByZero},
			{"SELECT 9223372036854775807 + 1", maragudb.ErrOutOfRange},
			{"SELECT CAST('x' AS int)", maragudb.ErrCast},
			{"SELECT 1 + 'a'", maragudb.ErrType},
			{"SELECT nope FROM t", maragudb.ErrInvalid},
			{"SELECT", maragudb.ErrSyntax},
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
		mustExec(t, tx, "CREATE TABLE t (id int PRIMARY KEY, n int)")
		mustExec(t, tx, "INSERT INTO t VALUES (1, 1)")
		mustExec(t, tx, "UPDATE t SET n = n + 1")
		is.Equal(t, "1 2", queryRows(t, tx, "SELECT * FROM t"))
		is.NotError(t, tx.Rollback())
		_, err = db.ExecContext(t.Context(), "SELECT * FROM t")
		is.Error(t, maragudb.ErrNoTable, err)

		tx, err = db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		mustExec(t, tx, "CREATE TABLE t (id int PRIMARY KEY, n int)")
		mustExec(t, tx, "INSERT INTO t VALUES (1, 1)")
		is.NotError(t, tx.Commit())
		is.Equal(t, "1 1", queryRows(t, db, "SELECT * FROM t"))
	})

	t.Run("an explicit transaction returns ErrConflict, which IsRetryable, and a retry succeeds", func(t *testing.T) {
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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

	t.Run("a read-only transaction never conflicts and rejects writes", func(t *testing.T) {
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
		mustExec(t, a, "INSERT INTO t VALUES (1, 0)")
		tx, err := a.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
		is.NotError(t, err)
		is.Equal(t, "0", queryRows(t, tx, "SELECT n FROM t"))
		mustExec(t, b, "UPDATE t SET n = 1")
		is.Equal(t, "0", queryRows(t, tx, "SELECT n FROM t"), "the snapshot holds")
		_, err = tx.ExecContext(t.Context(), "UPDATE t SET n = 2")
		is.Error(t, maragudb.ErrReadOnlyTx, err)
		is.NotError(t, tx.Commit())
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
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY)")
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
		w := newWorld(t)
		a := w.open(maragudb.Options{})
		b := w.open(maragudb.Options{})
		mustExec(t, a, "CREATE TABLE t (id int PRIMARY KEY, n int)")
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
		db := newDB(t)
		before, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY)")
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
		synctest.Test(t, func(t *testing.T) {
			w := newWorld(t)
			grace := time.Minute
			c, err := maragudb.NewConnector(maragudb.Options{Client: w.fake, GCGracePeriod: &grace}, "s3://bucket/app")
			is.NotError(t, err)
			db := sql.OpenDB(c)
			defer func() { _ = db.Close() }()
			mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY)")
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
			is.Equal(t, 11, n)
			is.Equal(t, "0\n1\n2\n3\n4\n5\n6\n7\n8\n9", queryRows(t, db, "SELECT * FROM t"))
		})
	})

	t.Run("fails on a database from another driver", func(t *testing.T) {
		_, err := maragudb.Stats(t.Context(), sql.OpenDB(otherConnector{}))
		is.True(t, err != nil)
	})
}

func TestRequestBudgets(t *testing.T) {
	sizes := []int{100, 1_000, 10_000, 100_000}
	load := func(t *testing.T, db *sql.DB, n int) {
		t.Helper()
		mustExec(t, db, "CREATE TABLE t (id int PRIMARY KEY, name string, f float)")
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
		}
		is.NotError(t, maragudb.Compact(t.Context(), db))
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
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			load(t, db, n)
			// Warm the cache with a point lookup, which reads the table definition and a row.
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, db, "SELECT name FROM t WHERE id = $1", n/2))
			before := stats(t, db)
			mustExec(t, db, "INSERT INTO t VALUES ($1, $2, $3)", n+1, "new", 0.5)
			d := stats(t, db).Sub(before)
			t.Logf("n=%d warm insert: %+v", n, d)
			is.Equal(t, maragudb.Requests{GetConditional: 1, Put: 1, PutConditional: 1}, d)

			// Cold, the duplicate key check and the table definition each cost a descent of the tree.
			mustExec(t, db, "DELETE FROM t WHERE id = $1", n/2)
			cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
			before = stats(t, cold)
			mustExec(t, cold, "INSERT INTO t VALUES ($1, $2, $3)", n/2, "back", 0.5)
			d = stats(t, cold).Sub(before)
			t.Logf("n=%d cold insert: %+v", n, d)
			is.Equal(t, maragudb.Requests{Get: d.Get, GetConditional: 1, Put: 1, PutConditional: 1}, d)
			is.True(t, d.Get <= int64(2*maxDepth(n)), fmt.Sprintf("n=%d: %d GETs, max depth %d", n, d.Get, maxDepth(n)))
		}
	})

	t.Run("a SELECT by primary key with a warm cache makes exactly one conditional GET", func(t *testing.T) {
		for _, n := range sizes[:3] {
			db := newDB(t)
			load(t, db, n)
			q := "SELECT name FROM t WHERE id = $1"
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, db, q, n/2))
			before := stats(t, db)
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, db, q, n/2))
			d := stats(t, db).Sub(before)
			t.Logf("n=%d warm select: %+v", n, d)
			is.Equal(t, maragudb.Requests{GetConditional: 1}, d)
		}
	})

	t.Run("a SELECT by primary key with a cold cache makes a logarithmic number of GETs", func(t *testing.T) {
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(maragudb.Options{})
			load(t, db, n)
			cold := w.open(maragudb.Options{MemoryCacheBytes: new(int64)})
			before := stats(t, cold)
			is.Equal(t, fmt.Sprintf("%q", fmt.Sprint("name-", n/2)), queryRows(t, cold, "SELECT name FROM t WHERE id = $1", n/2))
			d := stats(t, cold).Sub(before)
			t.Logf("n=%d cold select: %+v", n, d)
			// The table definition and the row are each found by one descent of the tree,
			// one block per level.
			is.True(t, d.Get <= int64(2*maxDepth(n)), fmt.Sprintf("n=%d: %d GETs, max depth %d", n, d.Get, maxDepth(n)))
			is.Equal(t, int64(1), d.GetConditional, "one freshness check")
			is.Equal(t, int64(0), d.Put+d.PutConditional+d.List+d.Delete)
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
