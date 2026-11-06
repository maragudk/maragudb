package pgdiff

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"
)

func TestPostgres(t *testing.T) {
	t.Run("gives the process a database of its own with the C locale", func(t *testing.T) {
		db := Postgres(t)
		var name string
		is.NotError(t, db.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&name))
		is.True(t, strings.HasPrefix(name, databasePrefix), name)
		is.Equal(t, processDB.name, name)
		is.True(t, name != databasePrefix, "the name is unique")

		again := Postgres(t)
		var same string
		is.NotError(t, again.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&same))
		is.Equal(t, name, same, "every test of the process shares it")
	})

	t.Run("turns sequential scans off on every connection, also after RESET", func(t *testing.T) {
		db := Postgres(t)
		// Connections held at the same time are different connections of the pool.
		var conns []*sql.Conn
		for range 3 {
			conn, err := db.Conn(t.Context())
			is.NotError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			conns = append(conns, conn)
		}
		for _, conn := range conns {
			var setting string
			is.NotError(t, conn.QueryRowContext(t.Context(), "SHOW enable_seqscan").Scan(&setting))
			is.Equal(t, "off", setting)
			_, err := conn.ExecContext(t.Context(), "RESET enable_seqscan")
			is.NotError(t, err)
			is.NotError(t, conn.QueryRowContext(t.Context(), "SHOW enable_seqscan").Scan(&setting))
			is.Equal(t, "off", setting, "the setting is the connection's default")
		}
	})

	t.Run("drops databases left behind before the cutoff and keeps newer ones", func(t *testing.T) {
		Postgres(t)
		admin, err := sql.Open("pgx", processDB.admin)
		is.NotError(t, err)
		t.Cleanup(func() { _ = admin.Close() })
		old := fmt.Sprintf("%s%d_0000test", databasePrefix, time.Now().Add(-48*time.Hour).Unix())
		fresh := fmt.Sprintf("%s%d_1111test", databasePrefix, time.Now().Unix())
		for _, name := range []string{old, fresh} {
			_, err := admin.ExecContext(t.Context(), "CREATE DATABASE "+name)
			is.NotError(t, err)
		}
		t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+fresh) })

		dropStale(t.Context(), admin, time.Now().Add(-24*time.Hour))

		exists := func(name string) bool {
			var ok bool
			is.NotError(t, admin.QueryRowContext(t.Context(), "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok))
			return ok
		}
		is.True(t, !exists(old), "the old database is gone")
		is.True(t, exists(fresh), "the fresh database stays")
		is.True(t, exists(processDB.name), "this process's database stays")
	})
}
