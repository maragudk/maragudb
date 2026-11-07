package sim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sqldriver"
)

// The SQL workloads run the same transactions as the engine workloads, through the SQL layer and
// the database/sql driver. Accounts are rows of `accounts`, which transfers update either
// relative to the current balance or by overwriting it with the balance they read. A list with an
// even key is the rows of `lists` with that key, ordered by position, so that an append inserts a
// new row. A list with an odd key is one row of `packed`, whose `vals` holds the list as
// comma-prefixed values, so that an append updates an existing row: in a transaction by
// overwriting it with the list it read plus the new value, and as an autocommit statement by
// concatenating the new value in the statement.

// openSQL opens a database/sql handle for a process over the given engine options, and returns
// the engine behind it. The driver's own maintenance loop stays off, because its timing would
// not be deterministic; the clients drive maintenance steps themselves.
func (r *run) openSQL(ctx context.Context, opts engine.Options) (*sql.DB, *engine.DB, error) {
	d := sqldriver.New(sqldriver.Options{Engine: opts, Exec: exec.Options{Mutations: r.cfg.SQLMutations}, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}, OnPanic: r.recovered})
	c, err := sqldriver.NewConnector(d, "s3://sim/db")
	if err != nil {
		return nil, nil, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	edb, err := c.DB(ctx)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, edb, nil
}

// seedSQL creates the tables, and the accounts, with a fault-free client before the clients start.
func (r *run) seedSQL() {
	r.goTask("seed", func(ctx context.Context) {
		db, _, err := r.openSQL(ctx, engine.Options{Client: r.fake.WithoutFaults(), Now: r.sched.Now, Sleep: r.sched.Sleep, BlockSize: r.blockSize(), Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0x5eed))})
		if err != nil {
			panic(err)
		}
		defer func() { _ = db.Close() }()
		switch r.cfg.Workload {
		case Bank:
			r.total = int64(r.cfg.Keys) * initialBalance
			// The index on the balances makes every transfer move index entries.
			for _, q := range []string{"CREATE TABLE accounts (id bigint PRIMARY KEY, balance bigint NOT NULL)", "CREATE INDEX accounts_balance ON accounts (balance)"} {
				if _, err := db.ExecContext(ctx, q); err != nil {
					panic(err)
				}
			}
			for i := range r.cfg.Keys {
				if _, err := db.ExecContext(ctx, "INSERT INTO accounts VALUES ($1, $2)", i, initialBalance); err != nil {
					panic(err)
				}
			}
		case ListAppend:
			for _, q := range ListTablesSQL(r.cfg.Keys) {
				if _, err := db.ExecContext(ctx, q); err != nil {
					panic(err)
				}
			}
		case Unique:
			for _, q := range UniqueTablesSQL() {
				if _, err := db.ExecContext(ctx, q); err != nil {
					panic(err)
				}
			}
		case ForeignKey:
			for _, q := range ForeignKeyTablesSQL() {
				if _, err := db.ExecContext(ctx, q); err != nil {
					panic(err)
				}
			}
		}
	})
	r.sched.Run()
}

// readFinalSQL reads every list, account, or user with a fresh, fault-free, cache-free process.
func (r *run) readFinalSQL() (map[string][]int64, map[string]int64, []string, error) {
	var lists map[string][]int64
	var balances map[string]int64
	var users []string
	var err error
	r.goTask("final", func(ctx context.Context) {
		var db *sql.DB
		db, _, err = r.openSQL(ctx, engine.Options{Client: r.fake.WithoutFaults(), Now: r.sched.Now, Sleep: r.sched.Sleep, MemoryCacheBytes: new(int64), Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0xf1a1))})
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		switch r.cfg.Workload {
		case Unique:
			users, err = r.readFinalUnique(ctx, db)
		case ForeignKey:
			users, err = r.readFinalForeignKeys(ctx, db)
		case ListAppend:
			lists = map[string][]int64{}
			var rows *sql.Rows
			rows, err = db.QueryContext(ctx, "SELECT k, pos, v FROM lists ORDER BY k, pos")
			if err != nil {
				return
			}
			defer func() { _ = rows.Close() }()
			pos := map[int64]int64{}
			for rows.Next() {
				var k, p, v int64
				if err = rows.Scan(&k, &p, &v); err != nil {
					return
				}
				if p != pos[k] {
					err = fmt.Errorf("list %d has position %d after %d", k, p, pos[k])
					return
				}
				pos[k]++
				lists[keyName(int(k))] = append(lists[keyName(int(k))], v)
			}
			if err = rows.Err(); err != nil {
				return
			}
			var packedRows *sql.Rows
			packedRows, err = db.QueryContext(ctx, "SELECT k, vals FROM packed ORDER BY k")
			if err != nil {
				return
			}
			defer func() { _ = packedRows.Close() }()
			for packedRows.Next() {
				var k int64
				var vals string
				if err = packedRows.Scan(&k, &vals); err != nil {
					return
				}
				var list []int64
				if list, err = DecodePacked(vals); err != nil {
					return
				}
				if len(list) > 0 {
					lists[keyName(int(k))] = list
				}
			}
			err = packedRows.Err()
		case Bank:
			balances = map[string]int64{}
			var rows *sql.Rows
			rows, err = db.QueryContext(ctx, "SELECT id, balance FROM accounts")
			if err != nil {
				return
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id, balance int64
				if err = rows.Scan(&id, &balance); err != nil {
					return
				}
				balances[keyName(int(id))] = balance
			}
			err = rows.Err()
		}
	})
	r.sched.Run()
	return lists, balances, users, err
}

// outcomeSQL classifies the error of a commit or an autocommit statement. Only
// [engine.ErrCommitUnknown] is ambiguous; every other error means the statement was not applied.
func (c *client) outcomeSQL(ctx context.Context, err error) history.Outcome {
	return c.outcome(ctx, err)
}

// readErrorSQL is like [client.readError] for a transaction begun at startedAt.
func (c *client) readErrorSQL(ctx context.Context, startedAt time.Time, err error) {
	c.run.stats.FailedReads++
	c.checkError(err)
	if errors.Is(err, engine.ErrSnapshotExpired) {
		age := c.run.sched.Now().Sub(startedAt)
		assert.Sometimes(age >= c.run.cfg.GracePeriod, "a reader older than the grace period is told so", nil)
		if age >= c.run.cfg.GracePeriod {
			reach.Hit(ctx, reach.SnapshotExpiredRefused)
		}
		if age < c.run.cfg.GracePeriod {
			c.run.anomaly(CheckerGracePeriod, "client %d: snapshot expired after %v, grace period is %v: %v", c.id, age, c.run.cfg.GracePeriod, err)
		}
	}
}

// ListTablesSQL are the statements that create the tables of the SQL list-append workload with
// keys lists, all empty.
func ListTablesSQL(keys int) []string {
	stmts := []string{
		"CREATE TABLE lists (k bigint, pos bigint, v bigint, PRIMARY KEY (k, pos))",
		"CREATE TABLE packed (k bigint PRIMARY KEY, vals text NOT NULL)",
	}
	for k := range keys {
		if PackedList(k) {
			stmts = append(stmts, fmt.Sprintf("INSERT INTO packed VALUES (%d, '')", k))
		}
	}
	return stmts
}

// PackedList reports whether the list of key k is one row of `packed` rather than rows of
// `lists`.
func PackedList(k int) bool {
	return k%2 == 1
}

// ErrMissingList is returned by [ReadListSQL] when the row of a packed list is gone, which no
// transaction ever does.
var ErrMissingList = errors.New("sim: the row of a packed list is missing")

// ErrCorruptList is returned by [DecodePacked] for values that no transaction ever writes.
var ErrCorruptList = errors.New("sim: corrupt packed list")

// DecodePacked decodes the comma-prefixed values of a packed list.
func DecodePacked(vals string) ([]int64, error) {
	list := []int64{}
	if vals == "" {
		return list, nil
	}
	if vals[0] != ',' {
		return nil, fmt.Errorf("%w: %q", ErrCorruptList, vals)
	}
	for _, s := range strings.Split(vals[1:], ",") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrCorruptList, vals)
		}
		list = append(list, v)
	}
	return list, nil
}

// EncodePacked is the inverse of [DecodePacked].
func EncodePacked(list []int64) string {
	var b strings.Builder
	for _, v := range list {
		fmt.Fprintf(&b, ",%d", v)
	}
	return b.String()
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ReadListSQL reads a list in a transaction or on the database.
func ReadListSQL(ctx context.Context, q querier, k int) ([]int64, error) {
	if PackedList(k) {
		var vals string
		err := q.QueryRowContext(ctx, "SELECT vals FROM packed WHERE k = $1", k).Scan(&vals)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrMissingList
		}
		if err != nil {
			return nil, err
		}
		return DecodePacked(vals)
	}
	rows, err := q.QueryContext(ctx, "SELECT pos, v FROM lists WHERE k = $1 ORDER BY pos", k)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	list := []int64{}
	for rows.Next() {
		var pos, v int64
		if err := rows.Scan(&pos, &v); err != nil {
			return nil, err
		}
		if pos != int64(len(list)) {
			return nil, fmt.Errorf("list %d has position %d at index %d", k, pos, len(list))
		}
		list = append(list, v)
	}
	return list, rows.Err()
}

func (c *client) listAppendSQL(ctx context.Context) {
	type plan struct {
		append bool
		key    int
	}
	nOps := 1 + c.rng.IntN(4)
	var plans []plan
	readOnly := true
	for range nOps {
		p := plan{append: c.rng.IntN(2) == 0, key: c.rng.IntN(c.run.cfg.Keys)}
		if p.append {
			readOnly = false
		}
		plans = append(plans, p)
	}

	// A single append, and a third of all operations, run as two autocommit statements: a read,
	// and an insert at the position the read found. The insert is its own transaction in the
	// history. Autocommit statements are frequent so that their retries meet every fault.
	if c.rng.IntN(3) == 0 || len(plans) == 1 && plans[0].append {
		k := plans[0].key
		h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
		startedAt := c.run.sched.Now()
		list, err := ReadListSQL(ctx, c.sql, k)
		if err != nil {
			c.listReadError(ctx, startedAt, err)
			return
		}
		h.Ops = []history.Op{{Kind: history.Read, Key: keyName(k), List: list}}
		h.Complete = c.run.stamp()
		h.Outcome = history.OK
		c.record(h)

		h = history.Tx{Client: c.id, Invoke: c.run.stamp()}
		value := c.value()
		var res sql.Result
		switch {
		case PackedList(k) && value%2 == 0:
			// The same append, with the list read by a subquery, whose read the commit must
			// check like any other: a retry after a conflict must read the list again.
			res, err = c.sql.ExecContext(ctx, "UPDATE packed SET vals = (SELECT p.vals FROM packed AS p WHERE p.k = $2) || $1 WHERE k = $2", EncodePacked([]int64{value}), k)
		case PackedList(k):
			res, err = c.sql.ExecContext(ctx, "UPDATE packed SET vals = vals || $1 WHERE k = $2", EncodePacked([]int64{value}), k)
		default:
			res, err = c.sql.ExecContext(ctx, "INSERT INTO lists VALUES ($1, $2, $3)", k, len(list), value)
		}
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				c.run.anomaly(CheckerWorkload, "client %d: append to %s affected %d rows", c.id, keyName(k), n)
			}
		}
		h.Complete = c.run.stamp()
		h.Ops = []history.Op{{Kind: history.Append, Key: keyName(k), Value: value}}
		h.Outcome = c.outcomeSQL(ctx, err)
		if err != nil {
			h.Err = err.Error()
		}
		c.record(h)
		return
	}

	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
	startedAt := c.run.sched.Now()
	tx, err := c.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		c.run.trace("client %d: begin: %v", c.id, err)
		return
	}
	for _, p := range plans {
		c.pause(ctx)
		list, err := ReadListSQL(ctx, tx, p.key)
		if err != nil {
			c.listReadError(ctx, startedAt, err)
			_ = tx.Rollback()
			if len(h.Ops) > 0 {
				h.Complete = c.run.stamp()
				h.Outcome = history.Failed
				h.Err = err.Error()
				c.record(h)
			}
			return
		}
		h.Ops = append(h.Ops, history.Op{Kind: history.Read, Key: keyName(p.key), List: list})
		if p.append {
			value := c.value()
			var res sql.Result
			var err error
			if PackedList(p.key) {
				res, err = tx.ExecContext(ctx, "UPDATE packed SET vals = $1 WHERE k = $2", EncodePacked(append(list, value)), p.key)
			} else {
				res, err = tx.ExecContext(ctx, "INSERT INTO lists VALUES ($1, $2, $3)", p.key, len(list), value)
			}
			if err != nil {
				c.run.trace("client %d: insert: %v", c.id, err)
				c.checkError(err)
				_ = tx.Rollback()
				h.Complete = c.run.stamp()
				h.Outcome = history.Failed
				h.Err = err.Error()
				c.record(h)
				return
			}
			if n, _ := res.RowsAffected(); n != 1 {
				c.run.anomaly(CheckerWorkload, "client %d: append to %s affected %d rows", c.id, keyName(p.key), n)
			}
			h.Ops = append(h.Ops, history.Op{Kind: history.Append, Key: keyName(p.key), Value: value})
		}
	}
	err = tx.Commit()
	h.Complete = c.run.stamp()
	if readOnly && err != nil {
		h.Outcome = history.Failed
	} else {
		h.Outcome = c.outcomeSQL(ctx, err)
	}
	if err != nil {
		h.Err = err.Error()
	}
	c.record(h)
}

func (c *client) bankSQL(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
	if c.rng.IntN(3) == 0 {
		startedAt := c.run.sched.Now()
		tx, err := c.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		balances := map[string]int64{}
		if c.rng.IntN(2) == 0 {
			// One statement for every account.
			rows, err := tx.QueryContext(ctx, "SELECT id, balance FROM accounts ORDER BY id")
			if err != nil {
				c.readErrorSQL(ctx, startedAt, err)
				return
			}
			for rows.Next() {
				var id, balance int64
				if err := rows.Scan(&id, &balance); err != nil {
					c.run.anomaly(CheckerWorkload, "client %d: scan: %v", c.id, err)
					_ = rows.Close()
					return
				}
				balances[keyName(int(id))] = balance
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				c.readErrorSQL(ctx, startedAt, err)
				return
			}
		} else {
			// One statement per account, so that the statements must share a snapshot.
			for i := range c.run.cfg.Keys {
				c.pause(ctx)
				var balance int64
				err := tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = $1", i).Scan(&balance)
				if errors.Is(err, sql.ErrNoRows) {
					c.run.anomaly(CheckerWorkload, "client %d: account %s is missing", c.id, keyName(i))
					return
				}
				if err != nil {
					c.readErrorSQL(ctx, startedAt, err)
					return
				}
				balances[keyName(i)] = balance
			}
		}
		if len(balances) != c.run.cfg.Keys {
			c.run.anomaly(CheckerWorkload, "client %d: read %d accounts, want %d", c.id, len(balances), c.run.cfg.Keys)
			return
		}
		// The same snapshot as aggregates, whose numeric sum reaches Go as its text.
		var sum string
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT sum(balance), count(*) FROM accounts").Scan(&sum, &count); err != nil {
			c.readErrorSQL(ctx, startedAt, err)
			return
		}
		var total int64
		for _, b := range balances {
			total += b
		}
		if sum != fmt.Sprint(total) || count != int64(c.run.cfg.Keys) {
			c.run.anomaly(CheckerWorkload, "client %d: SUM and COUNT of the accounts are %s and %d, but their rows add up to %d in %d accounts", c.id, sum, count, total, len(balances))
			return
		}
		h.Complete = c.run.stamp()
		h.Outcome = history.OK
		c.record(h)
		c.run.audits = append(c.run.audits, history.BankAudit{TxID: h.ID, Balances: balances})
		return
	}

	from := c.rng.IntN(c.run.cfg.Keys)
	to := c.rng.IntN(c.run.cfg.Keys)
	amount := int64(1 + c.rng.IntN(initialBalance/2))
	startedAt := c.run.sched.Now()
	tx, err := c.sql.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	read := func(id int) (int64, bool) {
		c.pause(ctx)
		var balance int64
		err := tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = $1", id).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) {
			c.run.anomaly(CheckerWorkload, "client %d: account %s is missing", c.id, keyName(id))
			return 0, false
		}
		if err != nil {
			c.readErrorSQL(ctx, startedAt, err)
			return 0, false
		}
		return balance, true
	}
	a, ok := read(from)
	if !ok {
		_ = tx.Rollback()
		return
	}
	b, ok := read(to)
	if !ok {
		_ = tx.Rollback()
		return
	}
	if a < amount || from == to {
		_ = tx.Rollback()
		return
	}
	// Half the transfers overwrite the balances they read, and half update them relative to the
	// current balance.
	overwrite := c.rng.IntN(2) == 0
	for _, step := range []struct {
		id    int
		delta int64
		read  int64
	}{{from, -amount, a}, {to, amount, b}} {
		query, arg := "UPDATE accounts SET balance = balance + $1 WHERE id = $2", step.delta
		if overwrite {
			query, arg = "UPDATE accounts SET balance = $1 WHERE id = $2", step.read+step.delta
		}
		res, err := tx.ExecContext(ctx, query, arg, step.id)
		if err != nil {
			c.run.trace("client %d: update: %v", c.id, err)
			c.checkError(err)
			_ = tx.Rollback()
			return
		}
		if n, _ := res.RowsAffected(); n != 1 {
			c.run.anomaly(CheckerWorkload, "client %d: update of account %s affected %d rows", c.id, keyName(step.id), n)
			_ = tx.Rollback()
			return
		}
	}
	err = tx.Commit()
	h.Complete = c.run.stamp()
	h.Outcome = c.outcomeSQL(ctx, err)
	h.Ops = []history.Op{{Kind: history.Append, Key: keyName(from), Value: -amount}, {Kind: history.Append, Key: keyName(to), Value: amount}}
	if err != nil {
		h.Err = err.Error()
	}
	c.record(h)
}

// listReadError handles a failed read of a list: a missing or corrupt packed list is an anomaly,
// and every other error is a failed read.
func (c *client) listReadError(ctx context.Context, startedAt time.Time, err error) {
	c.run.trace("client %d: read: %v", c.id, err)
	if errors.Is(err, ErrMissingList) || errors.Is(err, ErrCorruptList) {
		c.run.anomaly(CheckerWorkload, "client %d: %v", c.id, err)
		return
	}
	c.readErrorSQL(ctx, startedAt, err)
}
