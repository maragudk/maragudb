package sim

import (
	"database/sql"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
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

func TestForeignKeyCheck(t *testing.T) {
	insert := func(invoke, complete int, outcome history.Outcome, children ...fkChild) fkWrite {
		return fkWrite{invoke: dur(invoke), complete: dur(complete), outcome: outcome, children: children}
	}
	deletes := func(invoke, complete int, outcome history.Outcome, pid int64) fkWrite {
		return fkWrite{invoke: dur(invoke), complete: dur(complete), outcome: outcome, deletes: []int64{pid}}
	}
	child := fkChild{id: 100, pid: 1, n: 3, toys: []int64{200}}
	rows := func(children map[int64][2]sql.NullInt64, toys map[int64]toy) fkRows {
		return fkRows{parents: map[int64]int64{1: 0}, children: children, toys: toys, nodes: map[int64][2]sql.NullInt64{}}
	}
	whole := rows(map[int64][2]sql.NullInt64{100: {{Int64: 1, Valid: true}, {Int64: 3, Valid: true}}}, map[int64]toy{200: {cid: 100, label: "toy"}})
	none := rows(map[int64][2]sql.NullInt64{}, map[int64]toy{})
	changed := rows(map[int64][2]sql.NullInt64{100: {{Int64: 1, Valid: true}, {Int64: 4, Valid: true}}}, map[int64]toy{200: {cid: 100, label: "toy"}})
	noToy := rows(map[int64][2]sql.NullInt64{100: {{Int64: 1, Valid: true}, {Int64: 3, Valid: true}}}, map[int64]toy{})

	tests := []struct {
		name   string
		writes []fkWrite
		rows   fkRows
		invoke int
		// by is when the reader's snapshot was taken at the latest, or none when it is zero.
		by int
		ok bool
	}{
		{name: "accepts the child and toy of an acknowledged insert", writes: []fkWrite{insert(0, 10, history.OK, child)}, rows: whole, invoke: -1, ok: true},
		{name: "rejects a missing child whose parent no write deletes", writes: []fkWrite{insert(0, 10, history.OK, child)}, rows: none, invoke: -1},
		{name: "rejects a changed child", writes: []fkWrite{insert(0, 10, history.OK, child)}, rows: changed, invoke: -1},
		{name: "rejects a missing toy of a child", writes: []fkWrite{insert(0, 10, history.OK, child)}, rows: noToy, invoke: -1},
		{name: "accepts a missing child whose parent a later write deletes", writes: []fkWrite{insert(0, 10, history.OK, child), deletes(20, 30, history.Unknown, 1)}, rows: none, invoke: -1, ok: true},
		{name: "rejects a missing child whose parent only a failed write deletes", writes: []fkWrite{insert(0, 10, history.OK, child), deletes(20, 30, history.Failed, 1)}, rows: none, invoke: -1},
		{name: "rejects a missing child whose parent only a write acknowledged before the insert began deletes", writes: []fkWrite{deletes(0, 5, history.OK, 1), insert(10, 20, history.OK, child)}, rows: none, invoke: -1},
		{name: "rejects a missing child when a write deletes another parent", writes: []fkWrite{insert(0, 10, history.OK, child), deletes(20, 30, history.OK, 2)}, rows: none, invoke: -1},
		{name: "rejects a child of a failed insert", writes: []fkWrite{insert(0, 10, history.Failed, child)}, rows: whole, invoke: -1},
		{name: "rejects a toy of a failed insert", writes: []fkWrite{insert(0, 10, history.Failed, child)}, rows: rows(map[int64][2]sql.NullInt64{}, map[int64]toy{200: {cid: 100, label: "toy"}}), invoke: -1},
		{name: "accepts a child of an insert with an unknown outcome or not", writes: []fkWrite{insert(0, 10, history.Unknown, child)}, rows: none, invoke: -1, ok: true},
		{name: "rejects a reader that misses a child acknowledged before it began", writes: []fkWrite{insert(0, 10, history.OK, child)}, rows: none, invoke: 20},
		{name: "accepts a reader that misses a child acknowledged after it began", writes: []fkWrite{insert(0, 30, history.OK, child)}, rows: none, invoke: 20, ok: true},
		{name: "rejects a reader that sees a child of a write that began after its snapshot was taken", writes: []fkWrite{insert(30, 40, history.OK, child)}, rows: whole, invoke: 20, by: 25},
		{name: "rejects a reader that sees a toy of a write with an unknown outcome that began after its snapshot was taken", writes: []fkWrite{insert(30, 40, history.Unknown, child)}, rows: rows(map[int64][2]sql.NullInt64{}, map[int64]toy{200: {cid: 100, label: "toy"}}), invoke: 20, by: 25},
		{name: "accepts a reader that sees a child of a write that began before its snapshot was taken", writes: []fkWrite{insert(22, 40, history.OK, child)}, rows: whole, invoke: 20, by: 25, ok: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			by := dur(test.by)
			if test.by == 0 {
				by = -1
			}
			err := newForeignKeyCheck(test.writes).check(test.rows, dur(test.invoke), by)
			if test.ok {
				is.NotError(t, err)
				return
			}
			is.True(t, err != nil, "an error")
		})
	}
}
