package maragudb

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sqldriver"
	"maragu.dev/maragudb/internal/verify"
)

func TestOptions_withDefaults(t *testing.T) {
	t.Parallel()

	t.Run("bounds the disk cache to 1 GiB by default, and keeps a bound that is set", func(t *testing.T) {
		verify.Covers(t, "DiskCacheBytes defaults to 1 GiB, and a bound that is set, also zero, stays", "INV-OPTIONS")
		is.Equal(t, int64(1<<30), *Options{}.withDefaults().DiskCacheBytes)
		is.Equal(t, int64(0), *Options{DiskCacheBytes: new(int64)}.withDefaults().DiskCacheBytes)
	})
}

func TestDriverOptions(t *testing.T) {
	t.Parallel()

	t.Run("recover a panic in a driver call as ErrInternal, log it with its stack, and run a write that panicked after its commit once", func(t *testing.T) {
		verify.Covers(t, "through the public driver's options, an UPDATE outside a transaction that panics after its commit fails with ErrInternal and not driver.ErrBadConn, Options.Logger gets the panic with its stack at the error level, and the update lands once", "INV-PANIC-RECOVERED")
		fake := s3fake.New()
		var log bytes.Buffer
		o := driverOptions(Options{Logger: slog.New(slog.NewTextHandler(&log, nil)), Maintenance: new(false)})
		o.Engine.Client = fake
		open := func(o sqldriver.Options) *sql.DB {
			c, err := sqldriver.NewConnector(sqldriver.New(o), "s3://bucket/app")
			is.NotError(t, err)
			db := sql.OpenDB(c)
			t.Cleanup(func() { _ = db.Close() })
			return db
		}
		clean := open(o)
		for _, q := range []string{"CREATE TABLE t (id bigint PRIMARY KEY, v bigint)", "INSERT INTO t VALUES (1, 0)"} {
			_, err := clean.ExecContext(t.Context(), q)
			is.NotError(t, err)
		}
		o.Exec.Mutations.PanicAfterCommit = true
		db := open(o)
		_, err := db.ExecContext(t.Context(), "UPDATE t SET v = v + 1 WHERE id = 1")
		is.Error(t, ErrInternal, err)
		is.True(t, !errors.Is(err, driver.ErrBadConn))
		var v int64
		is.NotError(t, clean.QueryRowContext(t.Context(), "SELECT v FROM t WHERE id = 1").Scan(&v))
		is.Equal(t, int64(1), v, "the update ran once")
		is.True(t, strings.Contains(log.String(), "level=ERROR"), log.String())
		is.True(t, strings.Contains(log.String(), "exec.(*Session).commit"), "the log has the stack")
	})
}
