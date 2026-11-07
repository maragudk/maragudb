package sim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sql/exec"
)

// The unique workload keeps users with a unique email, from a small pool so that clients
// collide, in `users`, which also has an index on a counter that every update changes. Clients
// insert one or two rows at a time, move a row from one email to another, delete a row by its
// email, and run transactions that do several of those, all of which can violate the unique
// email. Readers check that no snapshot has an email twice, and that lookups through the
// indexes find exactly the rows of the table. Every row has an id that no other row ever has,
// and only a statement that names its email, or a transaction that found its id, changes it, so
// that readers and the final state show the rows of acknowledged statements that nothing could
// have changed since, and never a row of a failed statement.

// UniqueTablesSQL are the statements that create the table of the unique workload.
func UniqueTablesSQL() []string {
	return []string{
		"CREATE TABLE users (id bigint PRIMARY KEY, email text UNIQUE, n bigint NOT NULL)",
		"CREATE INDEX users_n ON users (n)",
	}
}

// email of the pool of the unique workload, which has 2 * keys of them.
func (c *client) email() string {
	return fmt.Sprintf("e%02d", c.rng.IntN(2*c.run.cfg.Keys))
}

// nullableEmail is an email, or now and then NULL, which never collides.
func (c *client) nullableEmail() any {
	if c.rng.IntN(8) == 0 {
		return nil
	}
	return c.email()
}

func (c *client) uniqueSQL(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
	// frees is the email that the statement frees when it commits, and takes the one it takes.
	var frees, takes string
	var w userWrite
	var res sql.Result
	var err error
	switch c.rng.IntN(6) {
	case 0:
		c.auditUnique(ctx, h)
		return
	case 1:
		e := c.nullableEmail()
		if s, ok := e.(string); ok {
			takes = s
		}
		id := c.value()
		w.inserts = []user{newUser(id, e)}
		_, err = c.sql.ExecContext(ctx, "INSERT INTO users VALUES ($1, $2, 0)", id, e)
	case 2:
		id1, e1, id2, e2 := c.value(), c.nullableEmail(), c.value(), c.nullableEmail()
		w.inserts = []user{newUser(id1, e1), newUser(id2, e2)}
		_, err = c.sql.ExecContext(ctx, "INSERT INTO users VALUES ($1, $2, 0), ($3, $4, 0)", id1, e1, id2, e2)
	case 3:
		frees, takes = c.email(), c.email()
		w.changes = frees
		res, err = c.sql.ExecContext(ctx, "UPDATE users SET email = $1, n = n + 1 WHERE email = $2", takes, frees)
	case 4:
		frees = c.email()
		w.changes = frees
		res, err = c.sql.ExecContext(ctx, "DELETE FROM users WHERE email = $1", frees)
	default:
		err = c.uniqueTx(ctx, &w)
	}
	if errors.Is(err, exec.ErrDuplicateKey) {
		c.run.stats.UniqueViolations++
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
			// The statement found no row with the email, so it changed nothing.
			w.changes = ""
		}
	}
	if len(w.inserts) > 0 || w.changes != "" || len(w.moves) > 0 {
		c.run.userWrites = append(c.run.userWrites, w)
	}
	if h.Outcome != history.OK {
		return
	}
	if res != nil {
		// A statement that changed no row freed and took nothing.
		if n, _ := res.RowsAffected(); n == 0 {
			frees, takes = "", ""
		}
	}
	assert.Sometimes(c.freed[takes], "a unique value that a committed write freed is taken again", nil)
	if c.freed[takes] {
		reach.Hit(ctx, reach.UniqueValueReused)
	}
	if frees != "" && frees != takes {
		c.freed[frees] = true
	}
}

// uniqueTx moves a row to another email, if it finds one, and inserts a row with another, in a
// transaction, and describes what it writes in w.
func (c *client) uniqueTx(ctx context.Context, w *userWrite) error {
	tx, err := c.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE email = $1", c.email()).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		c.pause(ctx)
		w.moves = []int64{id}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET email = $1, n = n + 1 WHERE id = $2", c.email(), id); err != nil {
			return err
		}
	}
	id2, e := c.value(), c.email()
	w.inserts = []user{newUser(id2, e)}
	if _, err := tx.ExecContext(ctx, "INSERT INTO users VALUES ($1, $2, 0)", id2, e); err != nil {
		return err
	}
	return tx.Commit()
}

// auditUnique reads every row in a read-only transaction, and checks that no email is there
// twice and that the indexes find the rows of the table.
func (c *client) auditUnique(ctx context.Context, h history.Tx) {
	startedAt := c.run.sched.Now()
	tx, err := c.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := readUsers(ctx, tx, "SELECT id, email, n FROM users")
	if err != nil {
		c.lookupError(ctx, startedAt, err)
		return
	}
	if err := checkUnique(rows); err != nil {
		c.run.anomaly(CheckerUnique, "client %d: %v", c.id, err)
		return
	}
	// The rows are checked against the writes once every write has its outcome, because a write
	// whose outcome is not known yet may be in the snapshot.
	c.run.userReads = append(c.run.userReads, userRead{client: c.id, invoke: h.Invoke, rows: rows})
	c.pause(ctx)
	if err := checkLookups(ctx, tx, rows, []string{c.email(), c.email()}); err != nil {
		c.lookupError(ctx, startedAt, err)
		return
	}
	h.Complete = c.run.stamp()
	h.Outcome = history.OK
	c.record(h)
}

// lookupError handles a failed read of the unique workload: a lookup that disagrees with the
// table, or an index entry of a missing row, is an anomaly, and every other error a failed read.
func (c *client) lookupError(ctx context.Context, startedAt time.Time, err error) {
	if errors.Is(err, errIndexMismatch) || errors.Is(err, exec.ErrIndexInconsistent) {
		c.run.anomaly(CheckerIndex, "client %d: %v", c.id, err)
		return
	}
	c.readErrorSQL(ctx, startedAt, err)
}

// user is a row of the unique workload.
type user struct {
	id    int64
	email sql.NullString
	n     int64
}

// newUser with an email that is a string or nil.
func newUser(id int64, email any) user {
	u := user{id: id}
	if e, ok := email.(string); ok {
		u.email = sql.NullString{String: e, Valid: true}
	}
	return u
}

func (u user) String() string {
	e := "NULL"
	if u.email.Valid {
		e = u.email.String
	}
	return fmt.Sprintf("%d %s %d", u.id, e, u.n)
}

// readUsers runs a query of every column of users.
func readUsers(ctx context.Context, q querier, query string, args ...any) ([]user, error) {
	rs, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rs.Close() }()
	var out []user
	for rs.Next() {
		var u user
		if err := rs.Scan(&u.id, &u.email, &u.n); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rs.Err()
}

// checkUnique reports two rows with the same email.
func checkUnique(rows []user) error {
	seen := map[string]int64{}
	for _, u := range rows {
		if !u.email.Valid {
			continue
		}
		if other, ok := seen[u.email.String]; ok {
			return fmt.Errorf("rows %d and %d have the unique email %s", other, u.id, u.email.String)
		}
		seen[u.email.String] = u.id
	}
	return nil
}

// errIndexMismatch is a lookup through an index that disagrees with the rows of the table.
var errIndexMismatch = errors.New("a lookup through an index disagrees with the table")

// checkLookups checks that lookups through the indexes of users find the rows of the table:
// by each of the emails, and every row in the order of the email index.
func checkLookups(ctx context.Context, q querier, rows []user, emails []string) error {
	render := func(us []user) string {
		var parts []string
		for _, u := range us {
			parts = append(parts, u.String())
		}
		sort.Strings(parts)
		return strings.Join(parts, "; ")
	}
	for _, e := range emails {
		got, err := readUsers(ctx, q, "SELECT id, email, n FROM users WHERE email = $1", e)
		if err != nil {
			return err
		}
		var want []user
		for _, u := range rows {
			if u.email.Valid && u.email.String == e {
				want = append(want, u)
			}
		}
		if render(got) != render(want) {
			return fmt.Errorf("%w: email = %s finds %q, the table has %q", errIndexMismatch, e, render(got), render(want))
		}
	}
	got, err := readUsers(ctx, q, "SELECT id, email, n FROM users ORDER BY email, id")
	if err != nil {
		return err
	}
	if render(got) != render(rows) {
		return fmt.Errorf("%w: in the order of the email index, the rows are %q, and the table has %q", errIndexMismatch, render(got), render(rows))
	}
	for i := 1; i < len(got); i++ {
		a, b := got[i-1].email, got[i].email
		if a.Valid && b.Valid && a.String > b.String || !a.Valid && b.Valid {
			return fmt.Errorf("%w: the email index returns %s before %s", errIndexMismatch, got[i-1], got[i])
		}
	}
	got, err = readUsers(ctx, q, "SELECT id, email, n FROM users WHERE n >= 0 AND n < $1", 1<<40)
	if err != nil {
		return err
	}
	if render(got) != render(rows) {
		return fmt.Errorf("%w: through the index on n, the rows are %q, and the table has %q", errIndexMismatch, render(got), render(rows))
	}
	return nil
}

// readFinalUnique reads every user with a fresh, fault-free, cache-free process, and checks the
// final state like an audit does, with a lookup of every email of the pool.
func (r *run) readFinalUnique(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := readUsers(ctx, db, "SELECT id, email, n FROM users")
	if err != nil {
		return nil, err
	}
	if err := checkUnique(rows); err != nil {
		r.anomaly(CheckerUnique, "final state: %v", err)
	}
	if err := newUserCheck(r.userWrites).check(rows, -1); err != nil {
		r.anomaly(CheckerDurability, "final state: %v", err)
	}
	var emails []string
	for i := range 2 * r.cfg.Keys {
		emails = append(emails, fmt.Sprintf("e%02d", i))
	}
	if err := checkLookups(ctx, db, rows, emails); err != nil {
		if !errors.Is(err, errIndexMismatch) && !errors.Is(err, exec.ErrIndexInconsistent) {
			return nil, err
		}
		r.anomaly(CheckerIndex, "final state: %v", err)
	}
	var out []string
	for _, u := range rows {
		out = append(out, u.String())
	}
	return out, nil
}

// checkIndexes checks every index of the final state against its table, reading the store with
// a fresh, fault-free, cache-free process.
func (r *run) checkIndexes() {
	var err error
	r.goTask("index check", func(ctx context.Context) {
		var db *engine.DB
		db, err = engine.Open(ctx, engine.Options{Client: r.fake.WithoutFaults(), Prefix: "db", Now: r.sched.Now, Sleep: r.sched.Sleep, MemoryCacheBytes: new(int64), Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0x1de))})
		if err != nil {
			return
		}
		var tx *engine.Tx
		if tx, err = db.BeginReadOnly(ctx); err != nil {
			return
		}
		err = exec.CheckIndexes(ctx, tx)
	})
	r.sched.Run()
	switch {
	case errors.Is(err, exec.ErrIndexInconsistent):
		r.anomaly(CheckerIndex, "final state: %v", err)
	case err != nil:
		r.anoms = append(r.anoms, &Anomaly{Checker: CheckerFinalRead, Err: fmt.Errorf("checking the indexes: %w", err)})
	}
}

// userWrite of a statement or transaction of the unique workload, with its outcome.
type userWrite struct {
	invoke, complete time.Duration
	outcome          history.Outcome
	// inserts are the rows that the write inserts, each with an id that no other write uses.
	inserts []user
	// changes is the email of the row that the write moves or deletes by its email, if any, and
	// moves are the ids of the rows that it moves by their id.
	changes string
	moves   []int64
}

// userRead is the rows of the unique workload that a reader saw in its snapshot, which it began
// to read at invoke.
type userRead struct {
	client int
	invoke time.Duration
	rows   []user
}

// checkUserReads checks the rows that every reader saw against the writes, once every write has
// its outcome.
func (r *run) checkUserReads() {
	uc := newUserCheck(r.userWrites)
	for _, read := range r.userReads {
		if err := uc.check(read.rows, read.invoke); err != nil {
			r.anomaly(CheckerRealTime, "client %d: %v", read.client, err)
		}
	}
}

// userCheck checks rows of the unique workload against its writes. No row of a failed write may
// be there. A row that an acknowledged write inserted before the reader began must be there as it
// was inserted, unless some write that did not fail and was not acknowledged before the insert
// began could have changed it: one that names its email, or one that found its id. A row with a
// NULL email is never named. Other writes are ignored, so that the reader may see them or not.
type userCheck struct {
	writes []userWrite
	// changeable says for every insert of every write whether another write could have changed
	// its row.
	changeable [][]bool
}

func newUserCheck(writes []userWrite) userCheck {
	uc := userCheck{writes: writes, changeable: make([][]bool, len(writes))}
	for i, w := range writes {
		uc.changeable[i] = make([]bool, len(w.inserts))
		for k, u := range w.inserts {
			uc.changeable[i][k] = changeable(writes, i, u)
		}
	}
	return uc
}

// check rows that a reader that began at invoke saw, or the final state when invoke is negative.
func (uc userCheck) check(rows []user, invoke time.Duration) error {
	byID := map[int64]user{}
	for _, u := range rows {
		byID[u.id] = u
	}
	for i, w := range uc.writes {
		for k, in := range w.inserts {
			u, ok := byID[in.id]
			switch {
			case w.outcome == history.Failed && ok:
				return fmt.Errorf("row %s is there, but the write that inserted it failed", u)
			case w.outcome != history.OK || invoke >= 0 && w.complete >= invoke || uc.changeable[i][k]:
			case !ok:
				return fmt.Errorf("row %s is missing, and no write could have changed it since an acknowledged write inserted it", in)
			case u != in:
				return fmt.Errorf("row %s is %s, and no write could have changed it since an acknowledged write inserted it", in, u)
			}
		}
	}
	return nil
}

// changeable reports whether any write but writes[i] that did not fail could have changed the row
// u that writes[i] inserted, ordered after it: a write that names the email of u or that found
// its id, and that was not acknowledged before writes[i] began.
func changeable(writes []userWrite, i int, u user) bool {
	for j, w := range writes {
		if j == i || w.outcome == history.Failed || w.complete < writes[i].invoke {
			continue
		}
		if u.email.Valid && w.changes == u.email.String || slices.Contains(w.moves, u.id) {
			return true
		}
	}
	return false
}
