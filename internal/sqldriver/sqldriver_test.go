package sqldriver_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sqldriver"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestParseDSN(t *testing.T) {
	verify.Covers(t, "s3://bucket/prefix DSNs parse into a bucket and a prefix, and malformed DSNs are rejected", "INV-DSN")
	tests := []struct {
		dsn            string
		bucket, prefix string
		ok             bool
	}{
		{"s3://bucket/prefix", "bucket", "prefix", true},
		{"s3://bucket/a/b/", "bucket", "a/b/", true},
		{"s3://bucket", "bucket", "", true},
		{"s3://bucket/", "bucket", "", true},
		{"s3:///prefix", "", "", false},
		{"bucket/prefix", "", "", false},
		{"https://bucket/prefix", "", "", false},
		{"s3://bucket/prefix?region=x", "", "", false},
		{"s3://bucket/prefix#x", "", "", false},
		{"s3://user:pw@bucket/prefix", "", "", false},
		{"", "", "", false},
	}
	for _, test := range tests {
		t.Run(test.dsn, func(t *testing.T) {
			bucket, prefix, err := sqldriver.ParseDSN(test.dsn)
			if !test.ok {
				is.Error(t, sqldriver.ErrDSN, err)
				return
			}
			is.NotError(t, err)
			is.Equal(t, test.bucket, bucket)
			is.Equal(t, test.prefix, prefix)
		})
	}
}

func TestConnector(t *testing.T) {
	t.Run("shares one engine between connections and opens it on first connect", func(t *testing.T) {
		fake := s3fake.New()
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		is.Equal(t, 0, len(fake.Keys()), "nothing is opened yet")
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		db.SetMaxOpenConns(3)
		is.NotError(t, db.PingContext(t.Context()))
		is.EqualSlice(t, []string{"db/manifest"}, fake.Keys())
		_, err = db.ExecContext(t.Context(), "CREATE TABLE t (id bigint PRIMARY KEY)")
		is.NotError(t, err)
		var conns []*sql.Conn
		for range 3 {
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			conns = append(conns, conn)
			_, err = conn.ExecContext(t.Context(), "INSERT INTO t VALUES ($1)", len(conns))
			is.NotError(t, err)
		}
		for _, conn := range conns {
			is.NotError(t, conn.Close())
		}
		engineDB, err := c.DB(t.Context())
		is.NotError(t, err)
		is.Equal(t, uint64(4), engineDB.Stats().Seq, "every connection commits through the same engine")
	})

	t.Run("runs background maintenance until it is closed, and then releases the lease", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := s3fake.New()
			d := sqldriver.New(sqldriver.Options{
				Engine:      engine.Options{Client: fake},
				Maintenance: sqldriver.MaintenanceOptions{Interval: time.Second, Engine: engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 3}}},
			})
			c, err := sqldriver.NewConnector(d, "s3://bucket/db")
			is.NotError(t, err)
			db := sql.OpenDB(c)
			_, err = db.ExecContext(t.Context(), "CREATE TABLE t (id bigint PRIMARY KEY)")
			is.NotError(t, err)
			_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (1)")
			is.NotError(t, err)
			engineDB, err := c.DB(t.Context())
			is.NotError(t, err)
			is.Equal(t, 2, engineDB.Stats().Segments)
			time.Sleep(2 * time.Second)
			is.Equal(t, 2, engineDB.Stats().Segments, "two segments in tier 0 are not merged")
			_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (2)")
			is.NotError(t, err)
			time.Sleep(2 * time.Second)
			s := engineDB.Stats()
			is.Equal(t, 1, s.Segments, "compacted in the background")
			is.Equal(t, int64(1), s.MaintenanceRuns)
			is.True(t, strings.Contains(strings.Join(fake.Keys(), ","), "db/maintenance-lease"))

			is.NotError(t, db.Close())
			before := fake.Counts()
			time.Sleep(10 * time.Second)
			is.Equal(t, int64(0), fake.Counts().Sub(before).Total(), "the loop stopped")
			body, ok := fake.Body("db/maintenance-lease")
			is.True(t, ok)
			is.True(t, strings.Contains(string(body), `"expiresAt":"0001-01-01`), string(body))
		})
	})

	t.Run("a connection from Driver.Open closes its connector and the maintenance with it", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fake := s3fake.New()
			d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Interval: time.Second}})
			conn, err := d.Open("s3://bucket/db")
			is.NotError(t, err)
			_, err = conn.(driver.ExecerContext).ExecContext(t.Context(), "CREATE TABLE t (id bigint PRIMARY KEY)", nil)
			is.NotError(t, err)
			is.NotError(t, conn.Close())
			before := fake.Counts()
			time.Sleep(10 * time.Second)
			is.Equal(t, int64(0), fake.Counts().Sub(before).Total(), "the loop stopped with the connection")
		})
	})

	t.Run("needs a client or a client factory", func(t *testing.T) {
		d := sqldriver.New(sqldriver.Options{})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		_, err = c.Connect(t.Context())
		is.True(t, err != nil)
	})

	t.Run("uses a disk cache directory per DSN", func(t *testing.T) {
		fake := s3fake.New()
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake, MemoryCacheBytes: new(int64)}, DiskCacheDir: t.TempDir(), DiskCacheBytes: 1 << 20, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		_, err = db.ExecContext(t.Context(), "CREATE TABLE t (id bigint PRIMARY KEY)")
		is.NotError(t, err)
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (1)")
		is.NotError(t, err)
		var id int64
		is.NotError(t, db.QueryRowContext(t.Context(), "SELECT id FROM t").Scan(&id))
		engineDB, err := c.DB(t.Context())
		is.NotError(t, err)
		is.True(t, engineDB.Stats().DiskCacheHits+engineDB.Stats().DiskCacheMisses > 0, "the disk cache is in use")
	})
}

func TestConn(t *testing.T) {
	t.Run("rejects named parameters and unsupported types before running anything", func(t *testing.T) {
		fake := s3fake.New()
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		_, err = db.ExecContext(t.Context(), "SELECT $1", sql.Named("x", 1))
		is.True(t, err != nil)
		_, err = db.ExecContext(t.Context(), "SELECT $1", uint64(1))
		is.True(t, err != nil)
		_, err = db.ExecContext(t.Context(), "SELECT $1", struct{}{})
		is.True(t, err != nil)
		var got int64
		is.NotError(t, db.QueryRowContext(t.Context(), "SELECT $1::bigint", 1).Scan(&got))
		is.Equal(t, int64(1), got)
	})

	t.Run("implements the optional driver interfaces", func(t *testing.T) {
		fake := s3fake.New()
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		conn, err := c.Connect(t.Context())
		is.NotError(t, err)
		_, ok := conn.(driver.ConnBeginTx)
		is.True(t, ok)
		_, ok = conn.(driver.ExecerContext)
		is.True(t, ok)
		_, ok = conn.(driver.QueryerContext)
		is.True(t, ok)
		_, ok = conn.(driver.ConnPrepareContext)
		is.True(t, ok)
		_, ok = conn.(driver.NamedValueChecker)
		is.True(t, ok)
		_, ok = conn.(driver.Pinger)
		is.True(t, ok)
		_, ok = conn.(driver.SessionResetter)
		is.True(t, ok)
		_, ok = conn.(driver.Validator)
		is.True(t, ok)
		_, ok = driver.Driver(d).(driver.DriverContext)
		is.True(t, ok)
		is.NotError(t, conn.Close())
	})

	t.Run("gives a numeric as its text, of the database type NUMERIC", func(t *testing.T) {
		verify.Covers(t, "the numeric results of SUM and AVG reach database/sql as strings in Postgres's text, and ColumnTypeDatabaseTypeName is NUMERIC, as with Postgres drivers", "INV-TYPES")
		fake := s3fake.New()
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		for _, q := range []string{"CREATE TABLE t (id bigint PRIMARY KEY, v bigint)", "INSERT INTO t VALUES (1, 9223372036854775807), (2, 9223372036854775807), (3, 1)"} {
			_, err := db.ExecContext(t.Context(), q)
			is.NotError(t, err)
		}
		rows, err := db.QueryContext(t.Context(), "SELECT sum(v), avg(id), count(*) FROM t")
		is.NotError(t, err)
		defer func() { _ = rows.Close() }()
		types, err := rows.ColumnTypes()
		is.NotError(t, err)
		is.Equal(t, "NUMERIC", types[0].DatabaseTypeName())
		is.Equal(t, "NUMERIC", types[1].DatabaseTypeName())
		is.Equal(t, "INT8", types[2].DatabaseTypeName())
		is.True(t, rows.Next())
		var sum, avg any
		var count int64
		is.NotError(t, rows.Scan(&sum, &avg, &count))
		is.Equal(t, any("18446744073709551615"), sum)
		is.Equal(t, any("2.0000000000000000"), avg)
		is.Equal(t, int64(3), count)
	})
}

// panicking opens a database whose driver has the SQL mutations mut, over the store of clean, a
// database without mutations, so that clean sees what the other one wrote. It returns the
// database and what the driver logged.
func panicking(t *testing.T, fake *s3fake.Client, mut exec.Mutations, onPanic func(value any, stack []byte)) (*sql.DB, *bytes.Buffer) {
	t.Helper()
	var log bytes.Buffer
	d := sqldriver.New(sqldriver.Options{
		Engine:      engine.Options{Client: fake},
		Maintenance: sqldriver.MaintenanceOptions{Disabled: true},
		Exec:        exec.Options{Mutations: mut},
		Logger:      slog.New(slog.NewJSONHandler(&log, nil)),
		OnPanic:     onPanic,
	})
	c, err := sqldriver.NewConnector(d, "s3://bucket/db")
	is.NotError(t, err)
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db, &log
}

// cleanDB opens a database without mutations over fake, with a table t of one row (1, 0).
func cleanDB(t *testing.T, fake *s3fake.Client) *sql.DB {
	t.Helper()
	d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
	c, err := sqldriver.NewConnector(d, "s3://bucket/db")
	is.NotError(t, err)
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	for _, q := range []string{"CREATE TABLE t (id bigint PRIMARY KEY, v bigint)", "INSERT INTO t VALUES (1, 0)"} {
		_, err := db.ExecContext(t.Context(), q)
		is.NotError(t, err, q)
	}
	return db
}

func valueOf(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	var v int64
	is.NotError(t, db.QueryRowContext(t.Context(), "SELECT v FROM t WHERE id = 1").Scan(&v))
	return v
}

func TestDriver_panics(t *testing.T) {
	t.Run("a panic in a statement of a transaction fails it with ErrInternal, logs the stack, rolls the transaction back at once, and the next statement runs on a new connection", func(t *testing.T) {
		verify.Covers(t, "a panic in a statement of a transaction fails it with an error that wraps ErrInternal and names the panic, the log gets the stack, OnPanic sees it, a deferred rollback returns at once, and the next statement runs on a new connection", "INV-PANIC-RECOVERED")
		verify.Covers(t, "a panic in the SQL layer in a statement of a transaction becomes an error instead of a hang, the transaction rolls back at once, and the next statement runs on a new connection", "INV-DAMAGE-DETECTED")
		fake := s3fake.New()
		cleanDB(t, fake)
		var panics []string
		db, log := panicking(t, fake, exec.Mutations{PanicInScan: true}, func(value any, stack []byte) {
			panics = append(panics, fmt.Sprint(value))
			is.True(t, strings.Contains(string(stack), "scanRows"), string(stack))
		})
		db.SetMaxOpenConns(1)
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		var v int64
		err = tx.QueryRowContext(t.Context(), "SELECT v FROM t WHERE v = 0").Scan(&v)
		is.Error(t, sqldriver.ErrInternal, err)
		is.True(t, !errors.Is(err, driver.ErrBadConn), "database/sql must not run it again")
		is.True(t, strings.Contains(err.Error(), `panic: exec: a scan of "t" panics`), err.Error())
		is.EqualSlice(t, []string{`exec: a scan of "t" panics`}, panics)
		is.True(t, strings.Contains(log.String(), `"level":"ERROR"`), log.String())
		is.True(t, strings.Contains(log.String(), "scanRows"), "the log has the stack")

		_, err = tx.ExecContext(t.Context(), "INSERT INTO t VALUES (2, 0)")
		is.Error(t, sqldriver.ErrInternal, err, "the broken connection runs nothing more")
		done := make(chan error, 1)
		go func() { done <- tx.Rollback() }()
		select {
		case err := <-done:
			is.NotError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the rollback hangs")
		}
		_, err = db.ExecContext(t.Context(), "INSERT INTO t VALUES (3, 0)")
		is.NotError(t, err, "a new connection runs the next statement")
	})

	t.Run("a transaction in which a statement panicked rolls back on commit, and the commit fails with ErrInternal", func(t *testing.T) {
		verify.Covers(t, "a transaction whose statement panicked after it wrote a row fails its commit with an error that wraps ErrInternal, and the row is not there", "INV-PANIC-RECOVERED")
		fake := s3fake.New()
		clean := cleanDB(t, fake)
		db, _ := panicking(t, fake, exec.Mutations{PanicInScan: true}, nil)
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "INSERT INTO t VALUES (2, 2)")
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "UPDATE t SET v = 5 WHERE v = 2")
		is.Error(t, sqldriver.ErrInternal, err)
		err = tx.Commit()
		is.Error(t, sqldriver.ErrInternal, err)
		is.True(t, !errors.Is(err, driver.ErrBadConn))
		var n int64
		is.NotError(t, clean.QueryRowContext(t.Context(), "SELECT count(*) FROM t").Scan(&n))
		is.Equal(t, int64(1), n, "the insert of the transaction was rolled back")
	})

	t.Run("a statement outside a transaction that panics after its commit runs once, and database/sql does not run it again", func(t *testing.T) {
		verify.Covers(t, "an UPDATE outside a transaction, plain and prepared, that panics after its commit fails with an error that wraps ErrInternal and not driver.ErrBadConn, and its increment lands exactly once", "INV-PANIC-RECOVERED")
		fake := s3fake.New()
		clean := cleanDB(t, fake)
		db, _ := panicking(t, fake, exec.Mutations{PanicAfterCommit: true}, nil)
		db.SetMaxOpenConns(3)
		_, err := db.ExecContext(t.Context(), "UPDATE t SET v = v + 1 WHERE id = 1")
		is.Equal(t, int64(1), valueOf(t, clean), "the update ran once")
		is.Error(t, sqldriver.ErrInternal, err)
		is.True(t, !errors.Is(err, driver.ErrBadConn))

		st, err := db.PrepareContext(t.Context(), "UPDATE t SET v = v + 1 WHERE id = $1")
		is.NotError(t, err)
		defer func() { _ = st.Close() }()
		_, err = st.ExecContext(t.Context(), 1)
		is.Error(t, sqldriver.ErrInternal, err)
		is.Equal(t, int64(2), valueOf(t, clean), "the prepared update ran once")

		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "UPDATE t SET v = v + 1 WHERE id = 1")
		is.NotError(t, err)
		is.Error(t, sqldriver.ErrInternal, tx.Commit())
		is.Equal(t, int64(3), valueOf(t, clean), "the transaction, whose commit succeeded before the panic, committed once")
	})

	t.Run("a panic while a connection opens fails it with ErrInternal and leaves the connector usable", func(t *testing.T) {
		verify.Covers(t, "a panic in opening the engine for a connection fails the ping with an error that wraps ErrInternal, and a second ping fails the same way instead of hanging", "INV-PANIC-RECOVERED")
		d := sqldriver.New(sqldriver.Options{
			NewClient:   func(context.Context, string) (s3.Client, error) { panic("boom") },
			Maintenance: sqldriver.MaintenanceOptions{Disabled: true},
		})
		c, err := sqldriver.NewConnector(d, "s3://bucket/db")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		for range 2 {
			err := db.PingContext(t.Context())
			is.Error(t, sqldriver.ErrInternal, err)
			is.True(t, strings.Contains(err.Error(), "panic: boom"), err.Error())
		}
	})
}
