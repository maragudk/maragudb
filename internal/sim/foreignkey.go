package sim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/sql/exec"
)

// The foreign key workload keeps parents, their children, the children's toys, and a tree of
// nodes that each may have an owner among the parents, with ids from small pools so that clients
// collide. Children cascade with their parent and toys with their child, a node's children lose
// their parent when it is deleted, and an owner cannot be deleted. Every table has a CHECK.
// Clients insert, update, and delete rows of every table, with keys that may be missing or
// concurrently deleted, and with values that violate the checks now and then, as autocommit
// statements and in transactions. Readers check that no snapshot has a reference to a missing
// row or a row that violates a check. A counter that transactions increment shows that a reader
// sees every commit acknowledged before it began, and the final state every acknowledged
// increment. Children and toys with ids outside of the pools are only ever deleted with their
// parent, so that readers and the final state show those of acknowledged writes whose parent no
// write could have deleted since, and never those of a failed write.

// ForeignKeyTablesSQL are the statements that create the tables of the foreign key workload.
func ForeignKeyTablesSQL() []string {
	return []string{
		"CREATE TABLE parents (id bigint PRIMARY KEY, n bigint NOT NULL DEFAULT 0 CHECK (n >= 0), created timestamptz NOT NULL DEFAULT now())",
		"CREATE TABLE children (id bigint PRIMARY KEY, pid bigint REFERENCES parents ON DELETE CASCADE, n bigint NOT NULL DEFAULT 0, CHECK (n >= 0 AND n < 20))",
		"CREATE INDEX children_pid ON children (pid)",
		"CREATE TABLE toys (id bigint PRIMARY KEY, cid bigint NOT NULL REFERENCES children ON DELETE CASCADE, label text NOT NULL DEFAULT 'toy' CHECK (label <> ''))",
		"CREATE TABLE nodes (id bigint PRIMARY KEY, pid bigint REFERENCES nodes ON DELETE SET NULL, owner bigint REFERENCES parents ON DELETE RESTRICT, CHECK (pid <> id))",
		"CREATE TABLE ticks (id bigint PRIMARY KEY, n bigint NOT NULL)",
		"INSERT INTO ticks VALUES (0, 0)",
	}
}

// fkKey is a key from the pool of the foreign key workload, which has 2 * keys of them.
func (c *client) fkKey() int64 {
	return int64(c.rng.IntN(2 * c.run.cfg.Keys))
}

// hotParent is the parent 0 half the time, and otherwise a key of the pool, so that clients
// insert children of a parent that other clients delete and insert again.
func (c *client) hotParent() int64 {
	if c.rng.IntN(2) == 0 {
		return 0
	}
	return c.fkKey()
}

// nullableKey is a key, or now and then NULL, which references nothing.
func (c *client) nullableKey() any {
	if c.rng.IntN(6) == 0 {
		return nil
	}
	return c.fkKey()
}

func (c *client) foreignKeySQL(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
	var w fkWrite
	var res sql.Result
	var err error
	switch c.rng.IntN(19) {
	case 0, 1:
		c.auditForeignKeys(ctx, h)
		return
	case 16:
		id := c.value()
		w.children = []fkChild{{id: id, pid: 0}}
		_, err = c.sql.ExecContext(ctx, "INSERT INTO children (id, pid) VALUES ($1, 0)", id)
	case 17:
		w.deletes = []int64{0}
		res, err = c.sql.ExecContext(ctx, "DELETE FROM parents WHERE id = 0")
	case 18:
		_, err = c.sql.ExecContext(ctx, "INSERT INTO parents (id) VALUES (0)")
	case 2:
		_, err = c.sql.ExecContext(ctx, "INSERT INTO parents (id, n) VALUES ($1, $2)", c.hotParent(), c.rng.IntN(6)-1)
	case 3:
		if c.rng.IntN(2) == 0 {
			id, pid := c.value(), c.hotParent()
			w.children = []fkChild{{id: id, pid: pid}}
			_, err = c.sql.ExecContext(ctx, "INSERT INTO children (id, pid) VALUES ($1, $2)", id, pid)
			break
		}
		_, err = c.sql.ExecContext(ctx, "INSERT INTO children VALUES ($1, $2, $3)", c.fkKey(), c.nullableKey(), c.rng.IntN(21)-1)
	case 4, 5:
		_, err = c.sql.ExecContext(ctx, "UPDATE children SET pid = $1, n = n + $2 WHERE id = $3", c.nullableKey(), c.rng.IntN(7)-3, c.fkKey())
	case 6:
		_, err = c.sql.ExecContext(ctx, "UPDATE parents SET n = n + $1 WHERE id = $2", c.rng.IntN(5)-2, c.fkKey())
	case 7:
		pid := c.hotParent()
		w.deletes = []int64{pid}
		res, err = c.sql.ExecContext(ctx, "DELETE FROM parents WHERE id = $1", pid)
	case 8:
		_, err = c.sql.ExecContext(ctx, "INSERT INTO toys (id, cid, label) VALUES ($1, $2, $3)", c.fkKey(), c.fkKey(), []string{"ball", "", "kite"}[c.rng.IntN(3)])
	case 9, 10:
		// A node, or a node and a child of it in one statement, mostly without references
		// elsewhere, so that the tree grows, and now and then with a parent and an owner that may
		// be missing.
		var pid, owner any
		if c.rng.IntN(3) == 0 {
			pid, owner = c.nullableKey(), c.nullableKey()
		}
		if c.rng.IntN(2) == 0 {
			_, err = c.sql.ExecContext(ctx, "INSERT INTO nodes VALUES ($1, $2, $3)", c.fkKey(), pid, owner)
		} else {
			_, err = c.sql.ExecContext(ctx, "INSERT INTO nodes VALUES ($1, $2, $3), ($4, $1, NULL)", c.fkKey(), pid, owner, c.fkKey())
		}
	case 11:
		a, b, other := c.fkKey(), c.fkKey(), c.fkKey()
		_, err = c.sql.ExecContext(ctx, moveNode, a, b, other)
	case 12:
		err = c.moveNodeTx(ctx)
	case 13:
		_, err = c.sql.ExecContext(ctx, []string{"DELETE FROM nodes WHERE id = $1", "DELETE FROM children WHERE id = $1", "UPDATE nodes SET owner = NULL WHERE owner = $1"}[c.rng.IntN(3)], c.fkKey())
	case 14:
		err = c.foreignKeyTx(ctx, &w)
	case 15:
		err = c.tick(ctx)
	}
	switch {
	case errors.Is(err, exec.ErrForeignKey), errors.Is(err, exec.ErrRestrict):
		c.run.stats.ForeignKeyViolations++
	case errors.Is(err, exec.ErrCheck):
		c.run.stats.CheckViolations++
	}
	h.Complete = c.run.stamp()
	h.Outcome = c.outcomeSQL(ctx, err)
	if err != nil {
		h.Err = err.Error()
	}
	c.record(h)
	w.invoke, w.complete, w.outcome = h.Invoke, h.Complete, h.Outcome
	if res != nil && h.Outcome == history.OK {
		if n, _ := res.RowsAffected(); n == 0 {
			// The statement found no parent, so it deleted nothing.
			w.deletes = nil
		}
	}
	w.children = slices.DeleteFunc(w.children, func(ch fkChild) bool { return !c.run.freshKey(ch.id) })
	if len(w.children) > 0 || len(w.deletes) > 0 {
		c.run.fkWrites = append(c.run.fkWrites, w)
	}
}

// tick increments the counter in a transaction, and records its new value when the commit is
// acknowledged.
func (c *client) tick(ctx context.Context) error {
	tx, err := c.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT n FROM ticks WHERE id = 0").Scan(&n); err != nil {
		return err
	}
	c.pause(ctx)
	if _, err := tx.ExecContext(ctx, "UPDATE ticks SET n = $1 WHERE id = 0", n+1); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.run.ticked = max(c.run.ticked, n+1)
	c.run.ticks = append(c.run.ticks, n+1)
	return nil
}

// moveNode moves the node $1 to the key $2 and points the node $3 at the key it leaves, which
// fails whenever the moved node existed and $3 is another existing node, because that node then
// references a missing key. Only the statement's own writes show it.
const moveNode = "UPDATE nodes SET id = CASE WHEN id = $1 THEN $2 ELSE id END, pid = CASE WHEN id = $3 THEN $1 ELSE pid END WHERE id IN ($1, $3)"

// moveNodeTx inserts two nodes with fresh keys and then runs [moveNode] on them, which fails, in
// a transaction.
func (c *client) moveNodeTx(ctx context.Context) error {
	tx, err := c.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	a, other := c.value(), c.value()
	if _, err := tx.ExecContext(ctx, "INSERT INTO nodes VALUES ($1, NULL, NULL), ($2, NULL, NULL)", a, other); err != nil {
		return err
	}
	c.pause(ctx)
	if _, err := tx.ExecContext(ctx, moveNode, a, c.value(), other); err != nil {
		return err
	}
	return tx.Commit()
}

// foreignKeyTx deletes a parent and then inserts a child of it, which fails, or inserts a parent
// and two children of it with fresh keys, and gives one child a toy, in a transaction.
func (c *client) foreignKeyTx(ctx context.Context, w *fkWrite) error {
	tx, err := c.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	parent := c.fkKey()
	if c.rng.IntN(4) == 0 {
		w.deletes = []int64{parent}
		if _, err := tx.ExecContext(ctx, "DELETE FROM parents WHERE id = $1", parent); err != nil {
			return err
		}
		c.pause(ctx)
		if _, err := tx.ExecContext(ctx, "INSERT INTO children (id, pid) VALUES ($1, $2)", c.fkKey(), parent); err != nil {
			return err
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO parents (id) VALUES ($1)", parent); err != nil {
		return err
	}
	c.pause(ctx)
	child := c.value()
	n := c.rng.IntN(5)
	sibling := c.value()
	w.children = []fkChild{{id: child, pid: parent, n: int64(n)}, {id: sibling, pid: parent}}
	if _, err := tx.ExecContext(ctx, "INSERT INTO children (id, pid, n) VALUES ($1, $2, $3), ($4, $2, DEFAULT)", child, parent, n, sibling); err != nil {
		return err
	}
	toy := c.value()
	w.children[0].toys = []int64{toy}
	if _, err := tx.ExecContext(ctx, "INSERT INTO toys (id, cid) VALUES ($1, $2)", toy, child); err != nil {
		return err
	}
	return tx.Commit()
}

// fkRows are the rows of the foreign key workload, each table's rendered.
type fkRows struct {
	parents  map[int64]int64
	children map[int64][2]sql.NullInt64
	toys     map[int64]toy
	nodes    map[int64][2]sql.NullInt64
}

type toy struct {
	cid   int64
	label string
}

// readForeignKeyRows reads every row of every table of the foreign key workload, a table at a
// time, calling between before each.
func readForeignKeyRows(ctx context.Context, q querier, between func()) (fkRows, error) {
	r := fkRows{parents: map[int64]int64{}, children: map[int64][2]sql.NullInt64{}, toys: map[int64]toy{}, nodes: map[int64][2]sql.NullInt64{}}
	read := func(query string, scan func(rows *sql.Rows) error) error {
		between()
		rows, err := q.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	err := read("SELECT id, n FROM parents", func(rows *sql.Rows) error {
		var id, n int64
		err := rows.Scan(&id, &n)
		r.parents[id] = n
		return err
	})
	if err != nil {
		return r, err
	}
	err = read("SELECT id, pid, n FROM children", func(rows *sql.Rows) error {
		var id int64
		var pid, n sql.NullInt64
		err := rows.Scan(&id, &pid, &n)
		r.children[id] = [2]sql.NullInt64{pid, n}
		return err
	})
	if err != nil {
		return r, err
	}
	err = read("SELECT id, cid, label FROM toys", func(rows *sql.Rows) error {
		var id int64
		var t toy
		err := rows.Scan(&id, &t.cid, &t.label)
		r.toys[id] = t
		return err
	})
	if err != nil {
		return r, err
	}
	err = read("SELECT id, pid, owner FROM nodes", func(rows *sql.Rows) error {
		var id int64
		var pid, owner sql.NullInt64
		err := rows.Scan(&id, &pid, &owner)
		r.nodes[id] = [2]sql.NullInt64{pid, owner}
		return err
	})
	return r, err
}

// danglingReference is the first reference to a missing row in r, or "".
func (r fkRows) danglingReference() string {
	for _, id := range sortedKeys(r.children) {
		if pid := r.children[id][0]; pid.Valid {
			if _, ok := r.parents[pid.Int64]; !ok {
				return fmt.Sprintf("child %d references the missing parent %d", id, pid.Int64)
			}
		}
	}
	for _, id := range sortedKeys(r.toys) {
		if _, ok := r.children[r.toys[id].cid]; !ok {
			return fmt.Sprintf("toy %d references the missing child %d", id, r.toys[id].cid)
		}
	}
	for _, id := range sortedKeys(r.nodes) {
		n := r.nodes[id]
		if n[0].Valid {
			if _, ok := r.nodes[n[0].Int64]; !ok {
				return fmt.Sprintf("node %d references the missing node %d", id, n[0].Int64)
			}
		}
		if n[1].Valid {
			if _, ok := r.parents[n[1].Int64]; !ok {
				return fmt.Sprintf("node %d is owned by the missing parent %d", id, n[1].Int64)
			}
		}
	}
	return ""
}

// orphanQueries find the references to missing rows with a LEFT JOIN of each referencing table
// to the referenced one, in which a missing row is a row of NULLs.
var orphanQueries = []string{
	"SELECT 'child', c.id, c.pid FROM children AS c LEFT JOIN parents AS p ON p.id = c.pid WHERE c.pid IS NOT NULL AND p.id IS NULL ORDER BY c.id",
	"SELECT 'toy', t.id, t.cid FROM toys t LEFT JOIN children c ON c.id = t.cid WHERE c.id IS NULL ORDER BY t.id",
	"SELECT 'node', n.id, n.pid FROM nodes n LEFT JOIN nodes m ON m.id = n.pid WHERE n.pid IS NOT NULL AND m.id IS NULL ORDER BY n.id",
	"SELECT 'owned node', n.id, n.owner FROM nodes n LEFT JOIN parents p ON p.id = n.owner WHERE n.owner IS NOT NULL AND p.id IS NULL ORDER BY n.id",
}

// orphan finds the first reference to a missing row with [orphanQueries], next to
// [fkRows.danglingReference], which finds them in the rows of every table. It returns "" when
// there is none.
func orphan(ctx context.Context, q querier) (string, error) {
	for _, query := range orphanQueries {
		var kind string
		var id, ref int64
		err := q.QueryRowContext(ctx, query).Scan(&kind, &id, &ref)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return "", err
		}
		return fmt.Sprintf("%s %d references the missing row %d, which a LEFT JOIN finds", kind, id, ref), nil
	}
	return "", nil
}

// checkViolation is the first row of r that violates a CHECK of its table, or "".
func (r fkRows) checkViolation() string {
	for _, id := range sortedKeys(r.parents) {
		if r.parents[id] < 0 {
			return fmt.Sprintf("parent %d has n = %d, below 0", id, r.parents[id])
		}
	}
	for _, id := range sortedKeys(r.children) {
		if n := r.children[id][1]; n.Valid && (n.Int64 < 0 || n.Int64 >= 20) {
			return fmt.Sprintf("child %d has n = %d, outside [0, 20)", id, n.Int64)
		}
	}
	for _, id := range sortedKeys(r.toys) {
		if r.toys[id].label == "" {
			return fmt.Sprintf("toy %d has an empty label", id)
		}
	}
	for _, id := range sortedKeys(r.nodes) {
		if pid := r.nodes[id][0]; pid.Valid && pid.Int64 == id {
			return fmt.Sprintf("node %d is its own parent", id)
		}
	}
	return ""
}

// String renders the rows, for the digest.
func (r fkRows) String() string {
	var b strings.Builder
	for _, id := range sortedKeys(r.parents) {
		fmt.Fprintf(&b, "p%d=%d ", id, r.parents[id])
	}
	for _, id := range sortedKeys(r.children) {
		fmt.Fprintf(&b, "c%d=%v ", id, r.children[id])
	}
	for _, id := range sortedKeys(r.toys) {
		fmt.Fprintf(&b, "t%d=%v ", id, r.toys[id])
	}
	for _, id := range sortedKeys(r.nodes) {
		fmt.Fprintf(&b, "n%d=%v ", id, r.nodes[id])
	}
	return b.String()
}

func sortedKeys[V any](m map[int64]V) []int64 {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// auditForeignKeys reads every row in a read-only transaction, a table at a time in one snapshot,
// pausing now and then, and checks that no reference is to a missing row and no row violates a check.
func (c *client) auditForeignKeys(ctx context.Context, h history.Tx) {
	startedAt := c.run.sched.Now()
	ticked := c.run.ticked
	tx, err := c.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := readForeignKeyRows(ctx, tx, func() { c.pause(ctx) })
	if err != nil {
		c.readErrorSQL(ctx, startedAt, err)
		return
	}
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT n FROM ticks WHERE id = 0").Scan(&n); err != nil {
		c.readErrorSQL(ctx, startedAt, err)
		return
	}
	if n < ticked {
		c.run.anomaly(CheckerRealTime, "client %d: the counter is %d, but a commit acknowledged before the read began set it to %d", c.id, n, ticked)
	}
	o, err := orphan(ctx, tx)
	if err != nil {
		c.readErrorSQL(ctx, startedAt, err)
		return
	}
	if o != "" {
		c.run.anomaly(CheckerForeignKey, "client %d: %s", c.id, o)
	}
	c.run.checkForeignKeyRows(rows, fmt.Sprintf("client %d", c.id))
	// The rows are checked against the writes once every write has its outcome, because a write
	// whose outcome is not known yet may be in the snapshot.
	c.run.fkReads = append(c.run.fkReads, fkRead{client: c.id, invoke: h.Invoke, rows: rows})
	h.Complete = c.run.stamp()
	h.Outcome = history.OK
	c.record(h)
}

// checkForeignKeyRows reports the dangling references and check violations of rows.
func (r *run) checkForeignKeyRows(rows fkRows, who string) {
	if d := rows.danglingReference(); d != "" {
		r.anomaly(CheckerForeignKey, "%s: %s", who, d)
	}
	if v := rows.checkViolation(); v != "" {
		r.anomaly(CheckerCheck, "%s: %s", who, v)
	}
}

// readFinalForeignKeys reads every row with a fresh, fault-free, cache-free process, and checks
// the final state like an audit does.
func (r *run) readFinalForeignKeys(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := readForeignKeyRows(ctx, db, func() {})
	if err != nil {
		return nil, err
	}
	o, err := orphan(ctx, db)
	if err != nil {
		return nil, err
	}
	if o != "" {
		r.anomaly(CheckerForeignKey, "final state: %s", o)
	}
	r.checkForeignKeyRows(rows, "final state")
	if err := newForeignKeyCheck(r.fkWrites).check(rows, -1); err != nil {
		r.anomaly(CheckerDurability, "final state: %v", err)
	}
	var n int64
	if err := db.QueryRowContext(ctx, "SELECT n FROM ticks WHERE id = 0").Scan(&n); err != nil {
		return nil, err
	}
	if n < r.ticked {
		r.anomaly(CheckerDurability, "final state: the counter is %d, but an acknowledged commit set it to %d", n, r.ticked)
	}
	// Every increment reads the counter in the snapshot of its commit, so two that were both
	// acknowledged never set it to the same value.
	seen := map[int64]bool{}
	for _, v := range r.ticks {
		if seen[v] {
			r.anomaly(CheckerDurability, "two acknowledged commits set the counter to %d", v)
			break
		}
		seen[v] = true
	}
	return []string{rows.String()}, nil
}

// checkConstraints checks every foreign key and check of the final state of the store, reading
// it with a fresh, fault-free, cache-free process.
func (r *run) checkConstraints() {
	var err error
	r.goTask("constraint check", func(ctx context.Context) {
		var db *engine.DB
		db, err = engine.Open(ctx, engine.Options{Client: r.fake.WithoutFaults(), Prefix: "db", Now: r.sched.Now, Sleep: r.sched.Sleep, MemoryCacheBytes: new(int64), Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0xc4ec))})
		if err != nil {
			return
		}
		var tx *engine.Tx
		if tx, err = db.BeginReadOnly(ctx); err != nil {
			return
		}
		err = exec.CheckConstraints(ctx, tx)
	})
	r.sched.Run()
	switch {
	case errors.Is(err, exec.ErrDanglingReference):
		r.anomaly(CheckerForeignKey, "final state: %v", err)
	case errors.Is(err, exec.ErrCheckViolated):
		r.anomaly(CheckerCheck, "final state: %v", err)
	case err != nil:
		r.anoms = append(r.anoms, &Anomaly{Checker: CheckerFinalRead, Err: fmt.Errorf("checking the constraints: %w", err)})
	}
}

// freshKey reports whether id is outside of the pool of keys of the foreign key workload, so that
// no statement names it but the one that inserts it.
func (r *run) freshKey(id int64) bool {
	return id >= int64(2*r.cfg.Keys)
}

// fkWrite of a statement or transaction of the foreign key workload, with its outcome: the
// children with fresh keys that it inserts, and the parents that it may delete.
type fkWrite struct {
	invoke, complete time.Duration
	outcome          history.Outcome
	children         []fkChild
	deletes          []int64
}

// fkChild is a child with a fresh key that a write inserts, with the toys with fresh keys that the
// write gives it.
type fkChild struct {
	id, pid, n int64
	toys       []int64
}

// fkRead is the rows of the foreign key workload that a reader saw in its snapshot, which it began
// to read at invoke.
type fkRead struct {
	client int
	invoke time.Duration
	rows   fkRows
}

// checkForeignKeyReads checks the rows that every reader saw against the writes, once every write
// has its outcome.
func (r *run) checkForeignKeyReads() {
	fc := newForeignKeyCheck(r.fkWrites)
	for _, read := range r.fkReads {
		if err := fc.check(read.rows, read.invoke); err != nil {
			r.anomaly(CheckerRealTime, "client %d: %v", read.client, err)
		}
	}
}

// foreignKeyCheck checks rows of the foreign key workload against its writes. No child or toy of
// a failed write may be there. A child that an acknowledged write inserted before the reader
// began must be there as it was inserted, with its toys, unless some write that did not fail and
// was not acknowledged before the insert began could have deleted its parent. Other writes are
// ignored, so that the reader may see them or not.
type foreignKeyCheck struct {
	writes []fkWrite
	// deletable says for every child of every write whether another write could have deleted its
	// parent.
	deletable [][]bool
}

func newForeignKeyCheck(writes []fkWrite) foreignKeyCheck {
	fc := foreignKeyCheck{writes: writes, deletable: make([][]bool, len(writes))}
	for i, w := range writes {
		fc.deletable[i] = make([]bool, len(w.children))
		for k, ch := range w.children {
			fc.deletable[i][k] = parentDeletable(writes, i, ch.pid)
		}
	}
	return fc
}

// check rows that a reader that began at invoke saw, or the final state when invoke is negative.
func (fc foreignKeyCheck) check(rows fkRows, invoke time.Duration) error {
	for i, w := range fc.writes {
		for k, ch := range w.children {
			got, ok := rows.children[ch.id]
			if w.outcome == history.Failed {
				if ok {
					return fmt.Errorf("child %d is there, but the write that inserted it failed", ch.id)
				}
				for _, id := range ch.toys {
					if _, ok := rows.toys[id]; ok {
						return fmt.Errorf("toy %d is there, but the write that inserted it failed", id)
					}
				}
				continue
			}
			if w.outcome != history.OK || invoke >= 0 && w.complete >= invoke || fc.deletable[i][k] {
				continue
			}
			want := [2]sql.NullInt64{{Int64: ch.pid, Valid: true}, {Int64: ch.n, Valid: true}}
			switch {
			case !ok:
				return fmt.Errorf("child %d of parent %d is missing, and no write could have deleted the parent since an acknowledged write inserted the child", ch.id, ch.pid)
			case got != want:
				return fmt.Errorf("child %d is %v, but an acknowledged write inserted it as %v, and no write changes it", ch.id, got, want)
			}
			for _, id := range ch.toys {
				if t, ok := rows.toys[id]; !ok || t != (toy{cid: ch.id, label: "toy"}) {
					return fmt.Errorf("toy %d of child %d is %v, but an acknowledged write inserted it, and no write could have deleted the child since", id, ch.id, t)
				}
			}
		}
	}
	return nil
}

// parentDeletable reports whether any write but writes[i] that did not fail could have deleted
// the parent pid, ordered after writes[i]: one that deletes it and that was not acknowledged
// before writes[i] began.
func parentDeletable(writes []fkWrite, i int, pid int64) bool {
	for j, w := range writes {
		if j != i && w.outcome != history.Failed && w.complete >= writes[i].invoke && slices.Contains(w.deletes, pid) {
			return true
		}
	}
	return false
}
