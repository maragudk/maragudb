package sim

import (
	"database/sql"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sqldriver"
)

func TestOrphan(t *testing.T) {
	t.Run("finds every kind of reference to a missing row with a LEFT JOIN, and none in a consistent state", func(t *testing.T) {
		// The mutation lets rows reference missing rows, as a broken build would.
		d := sqldriver.New(sqldriver.Options{Engine: engine.Options{Client: s3fake.New(), Prefix: "db"}, Exec: exec.Options{Mutations: exec.Mutations{ForeignKeyNoChildCheck: true}}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}})
		c, err := sqldriver.NewConnector(d, "s3://sim/db")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		t.Cleanup(func() { _ = db.Close() })
		for _, q := range append(ForeignKeyTablesSQL(),
			"INSERT INTO parents (id) VALUES (1)",
			"INSERT INTO children (id, pid) VALUES (1, 1), (2, NULL)",
			"INSERT INTO toys (id, cid) VALUES (1, 1)",
			"INSERT INTO nodes VALUES (1, NULL, 1), (2, 1, NULL)",
		) {
			_, err := db.ExecContext(t.Context(), q)
			is.NotError(t, err, q)
		}
		o, err := orphan(t.Context(), db)
		is.NotError(t, err)
		is.Equal(t, "", o)
		for _, e := range []struct {
			insert, want string
		}{
			{"INSERT INTO nodes VALUES (3, NULL, 9)", "owned node 3 references the missing row 9, which a LEFT JOIN finds"},
			{"INSERT INTO nodes VALUES (4, 8, NULL)", "node 4 references the missing row 8, which a LEFT JOIN finds"},
			{"INSERT INTO toys (id, cid) VALUES (2, 7)", "toy 2 references the missing row 7, which a LEFT JOIN finds"},
			{"INSERT INTO children (id, pid) VALUES (3, 6)", "child 3 references the missing row 6, which a LEFT JOIN finds"},
		} {
			_, err := db.ExecContext(t.Context(), e.insert)
			is.NotError(t, err, e.insert)
			o, err := orphan(t.Context(), db)
			is.NotError(t, err)
			is.Equal(t, e.want, o)
		}
	})
}
