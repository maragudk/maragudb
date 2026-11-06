package sqldriver_test

import (
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3fake"
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
				Maintenance: sqldriver.MaintenanceOptions{Interval: time.Second, Engine: engine.MaintenanceOptions{SegmentThreshold: 3}},
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
			is.Equal(t, 2, engineDB.Stats().Segments, "below the threshold")
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
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: fake, MemoryCacheBytes: new(int64)}, DiskCacheDir: t.TempDir(), Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
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
