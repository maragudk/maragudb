// Package workload runs the bank and list-append workloads against a real object store, through
// the engine or through SQL, with concurrent clients that each have their own engine, as separate
// processes would. It records the history in wall-clock time and checks it with the same checkers
// as the simulator. Unlike the simulator it is not deterministic: faults come from the network and
// the store, such as the ones Antithesis injects.
package workload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sqldriver"
)

// Config of a run.
type Config struct {
	// Client for the object store. Required.
	Client s3.Client
	// Prefix of the database, which must be empty. Required.
	Prefix   string
	Workload sim.Workload
	// SQL runs the clients through the SQL layer and the database/sql driver.
	SQL bool
	// Clients is the number of concurrent clients.
	Clients int
	// TxPerClient is how many transactions each client runs.
	TxPerClient int
	// Keys for the list-append workload, or accounts for the bank workload.
	Keys int
	// MaxTimeout bounds the drawn timeout of each transaction. A transaction cut off by its
	// timeout is like a client that crashed mid-request, and the client then restarts its engine.
	MaxTimeout time.Duration
	// GracePeriod for GC. A reader younger than it that loses its snapshot is an anomaly.
	// Defaults to one hour.
	GracePeriod time.Duration
	// MaintenanceInterval between the background maintenance checks of each client, which
	// compact and garbage collect under the lease. Defaults to 200 milliseconds.
	MaintenanceInterval time.Duration
	// Tiers of the compaction that maintenance runs. Their defaults apply.
	Tiers engine.Tiers
	// Rand drives every decision. Required.
	Rand *rand.Rand
	// Log receives progress lines when non-nil.
	Log func(string)
	// Mutations to break the engine with, for tests.
	Mutations engine.Mutations
}

// Result of a run.
type Result struct {
	History   []history.Tx
	Audits    []history.BankAudit
	Anomalies []error
	Stats     Stats
}

// Stats of a run.
type Stats struct {
	Commits, Conflicts, Unknown, Failed, Restarts int
	// Compactions and Deleted count what the clients' background maintenance did, and
	// MaintenanceRuns, LeaseAcquisitions, and LeaseLosses how it was coordinated.
	Compactions, Deleted, MaintenanceRuns, LeaseAcquisitions, LeaseLosses int
}

// Report of the run, for logs.
func (r Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d tx, %d commits, %d conflicts, %d unknown, %d other failures, %d restarts, %d compactions, %d deleted, %d maintenance runs (%d lease acquisitions, %d losses)",
		len(r.History), r.Stats.Commits, r.Stats.Conflicts, r.Stats.Unknown, r.Stats.Failed, r.Stats.Restarts, r.Stats.Compactions, r.Stats.Deleted, r.Stats.MaintenanceRuns, r.Stats.LeaseAcquisitions, r.Stats.LeaseLosses)
	for _, a := range r.Anomalies {
		fmt.Fprintf(&b, "\nANOMALY: %v", a)
	}
	return b.String()
}

const initialBalance = 100

type run struct {
	cfg   Config
	start time.Time

	mu     sync.Mutex
	rng    *rand.Rand
	txs    []history.Tx
	audits []history.BankAudit
	stats  Stats
	anoms  []error
	last   time.Duration
	total  int64
	next   int64
}

// Run the workload. It returns an error only when the store cannot be set up or read back at
// all before ctx ends; anomalies are in the result.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.GracePeriod == 0 {
		cfg.GracePeriod = time.Hour
	}
	if cfg.MaintenanceInterval == 0 {
		cfg.MaintenanceInterval = 200 * time.Millisecond
	}
	r := &run{cfg: cfg, start: time.Now(), rng: cfg.Rand}
	if err := r.seed(ctx); err != nil {
		return Result{}, fmt.Errorf("workload: seeding: %w", err)
	}

	var clients sync.WaitGroup
	for i := range cfg.Clients {
		clients.Go(func() { r.client(ctx, i) })
	}
	clients.Wait()

	// The clients may have stopped because ctx ended, and the history must still be checked, so
	// the final read gets a deadline of its own.
	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	final, balances, err := r.readFinal(finalCtx)
	if err != nil {
		return Result{}, fmt.Errorf("workload: reading the final state: %w", err)
	}
	switch cfg.Workload {
	case sim.ListAppend:
		if err := history.CheckDurability(r.txs, final); err != nil {
			r.anoms = append(r.anoms, err)
		}
		if err := history.CheckListAppend(r.txs, final); err != nil {
			r.anoms = append(r.anoms, err)
		}
	case sim.Bank:
		r.audits = append(r.audits, history.BankAudit{TxID: -1, Balances: balances})
		if err := history.CheckBank(r.audits, r.total); err != nil {
			r.anoms = append(r.anoms, err)
		}
	}
	if cfg.Mutations == (engine.Mutations{}) {
		assert.Always(len(r.anoms) == 0, "the history against a real object store has no anomalies", map[string]any{"workload": cfg.Workload.String(), "sql": cfg.SQL})
	}
	assert.Sometimes(r.stats.Unknown > 0, "a commit against a real object store has an unknown outcome", nil)
	assert.Sometimes(r.stats.Conflicts > 0, "a commit against a real object store conflicts", nil)
	assert.Sometimes(r.stats.Compactions > 0, "background maintenance compacts against a real object store", nil)
	assert.Sometimes(r.stats.Deleted > 0, "background maintenance deletes garbage against a real object store", nil)
	assert.Sometimes(r.stats.LeaseLosses > 0, "a maintenance lease is lost against a real object store", nil)
	return Result{History: r.txs, Audits: r.audits, Anomalies: r.anoms, Stats: r.stats}, nil
}

func (r *run) log(format string, args ...any) {
	if r.cfg.Log != nil {
		r.cfg.Log(fmt.Sprintf(format, args...))
	}
}

// intN draws from the shared random source.
func (r *run) intN(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rng.IntN(n)
}

// stamp returns a history timestamp that is later than every earlier one.
func (r *run) stamp() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := time.Since(r.start)
	if t <= r.last {
		t = r.last + 1
	}
	r.last = t
	return t
}

func (r *run) value(client int) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	return int64(client)<<32 | r.next
}

func (r *run) record(tx history.Tx) {
	r.mu.Lock()
	defer r.mu.Unlock()
	tx.ID = len(r.txs) + 1
	r.txs = append(r.txs, tx)
}

func (r *run) audit(tx history.Tx, balances map[string]int64) {
	r.mu.Lock()
	tx.ID = len(r.txs) + 1
	r.txs = append(r.txs, tx)
	r.audits = append(r.audits, history.BankAudit{TxID: tx.ID, Balances: balances})
	r.mu.Unlock()
}

func (r *run) anomaly(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.anoms = append(r.anoms, fmt.Errorf(format, args...))
}

// outcome of a commit. Only [engine.ErrCommitUnknown] is ambiguous; every other error means the
// transaction was not applied.
func (r *run) outcome(err error) history.Outcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err == nil:
		r.stats.Commits++
		return history.OK
	case errors.Is(err, engine.ErrCommitUnknown):
		r.stats.Unknown++
		return history.Unknown
	case errors.Is(err, engine.ErrConflict):
		r.stats.Conflicts++
	default:
		r.stats.Failed++
	}
	return history.Failed
}

// listReadError records an anomaly for a missing or corrupt packed list, which no transaction
// ever writes, and otherwise handles a failed read like [run.readError].
func (r *run) listReadError(id int, startedAt time.Time, err error) {
	if errors.Is(err, sim.ErrMissingList) || errors.Is(err, sim.ErrCorruptList) {
		r.anomaly("client %d: %v", id, err)
		return
	}
	r.readError(id, startedAt, err)
}

// readError records an anomaly when a reader younger than the grace period lost its snapshot.
func (r *run) readError(id int, startedAt time.Time, err error) {
	if !errors.Is(err, engine.ErrSnapshotExpired) {
		return
	}
	if age := time.Since(startedAt); age < r.cfg.GracePeriod {
		r.anomaly("client %d: snapshot expired after %v, grace period is %v: %v", id, age, r.cfg.GracePeriod, err)
	}
}

func keyName(i int) string {
	return fmt.Sprintf("k%02d", i)
}

func (r *run) engineOptions(cache bool) engine.Options {
	grace := r.cfg.GracePeriod
	opts := engine.Options{Client: r.cfg.Client, Prefix: r.cfg.Prefix, Mutations: r.cfg.Mutations, GCGracePeriod: &grace}
	if !cache {
		opts.MemoryCacheBytes = new(int64)
	}
	return opts
}

// process is one client's connection to the database: an engine, or a database/sql handle,
// each with background maintenance running: the driver's own loop for a handle, and a loop
// here for an engine.
type process struct {
	run    *run
	engine *engine.DB
	db     *engine.DB
	sql    *sql.DB
	stop   context.CancelFunc
	done   chan struct{}
}

// close the process and add its maintenance to the run's statistics.
func (p *process) close() {
	if p.stop != nil {
		p.stop()
		<-p.done
	}
	if p.sql != nil {
		_ = p.sql.Close()
	}
	s := p.engine.Stats()
	p.run.mu.Lock()
	p.run.stats.Compactions += int(s.Compactions)
	p.run.stats.Deleted += int(s.Deleted)
	p.run.stats.MaintenanceRuns += int(s.MaintenanceRuns)
	p.run.stats.LeaseAcquisitions += int(s.LeaseAcquisitions)
	p.run.stats.LeaseLosses += int(s.LeaseLosses)
	p.run.mu.Unlock()
}

// open a process. maintain says whether it runs background maintenance.
func (r *run) open(ctx context.Context, cache, maintain bool) (*process, error) {
	if !r.cfg.SQL {
		db, err := engine.Open(ctx, r.engineOptions(cache))
		if err != nil {
			return nil, err
		}
		p := &process{run: r, engine: db, db: db}
		if maintain {
			m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: r.cfg.Tiers})
			loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			p.stop, p.done = cancel, make(chan struct{})
			go func() {
				defer close(p.done)
				defer func() {
					closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
					defer cancel()
					_ = m.Close(closeCtx)
				}()
				for {
					select {
					case <-loopCtx.Done():
						return
					case <-time.After(r.cfg.MaintenanceInterval):
					}
					if _, err := m.Step(loopCtx); err != nil && loopCtx.Err() == nil {
						r.log("maintenance: %v", err)
					}
				}
			}()
		}
		return p, nil
	}
	d := sqldriver.New(sqldriver.Options{
		Engine:      r.engineOptions(cache),
		Exec:        exec.Options{},
		Maintenance: sqldriver.MaintenanceOptions{Disabled: !maintain, Interval: r.cfg.MaintenanceInterval, Engine: engine.MaintenanceOptions{Tiers: r.cfg.Tiers}},
	})
	c, err := sqldriver.NewConnector(d, "s3://workload/"+r.cfg.Prefix)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	edb, err := c.DB(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &process{run: r, engine: edb, sql: db}, nil
}

// GaveUpError is returned when the store did not answer before the context ended. Err is the
// last error, which says whether the store was unreachable or something else failed.
type GaveUpError struct {
	Err error
}

func (e *GaveUpError) Error() string {
	return "gave up retrying: " + e.Err.Error()
}

func (e *GaveUpError) Unwrap() error {
	return e.Err
}

// retry fn until it succeeds or ctx ends, backing off between attempts.
func retry(ctx context.Context, fn func() error) error {
	wait := 10 * time.Millisecond
	for {
		err := fn()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return &GaveUpError{Err: err}
		case <-time.After(wait):
		}
		wait = min(2*wait, time.Second)
	}
}

// seed creates the schema and the accounts. A seed whose commit outcome is unknown is checked
// by reading it back.
func (r *run) seed(ctx context.Context) error {
	r.total = int64(r.cfg.Keys) * initialBalance
	return retry(ctx, func() error {
		p, err := r.open(ctx, false, false)
		if err != nil {
			return err
		}
		defer p.close()
		if seeded, err := r.seeded(ctx, p); err != nil || seeded {
			return err
		}
		if p.db != nil {
			if r.cfg.Workload == sim.ListAppend {
				return nil
			}
			tx, err := p.db.Begin(ctx)
			if err != nil {
				return err
			}
			for i := range r.cfg.Keys {
				_ = tx.Put([]byte(keyName(i)), []byte(strconv.Itoa(initialBalance)))
			}
			return tx.Commit(ctx)
		}
		tx, err := p.sql.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		stmts := sim.ListTablesSQL(r.cfg.Keys)
		if r.cfg.Workload == sim.Bank {
			stmts = []string{"CREATE TABLE accounts (id bigint PRIMARY KEY, balance bigint NOT NULL)"}
			for i := range r.cfg.Keys {
				stmts = append(stmts, fmt.Sprintf("INSERT INTO accounts VALUES (%d, %d)", i, initialBalance))
			}
		}
		for _, s := range stmts {
			if _, err := tx.ExecContext(ctx, s); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

// seeded reports whether the seed transaction has been applied.
func (r *run) seeded(ctx context.Context, p *process) (bool, error) {
	if p.db != nil {
		if r.cfg.Workload == sim.ListAppend {
			return true, nil
		}
		tx, err := p.db.BeginReadOnly(ctx)
		if err != nil {
			return false, err
		}
		_, ok, err := tx.Get(ctx, []byte(keyName(0)))
		return ok, err
	}
	table := "lists"
	if r.cfg.Workload == sim.Bank {
		table = "accounts"
	}
	_, err := p.sql.ExecContext(ctx, "SELECT 1 FROM "+table+" WHERE FALSE")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, exec.ErrNoTable):
		return false, nil
	}
	return false, err
}

// client runs transactions, with background maintenance. A transaction that runs out of time
// is like a crash in the middle of it, after which the client starts over with a new engine and
// cold caches.
func (r *run) client(ctx context.Context, id int) {
	var p *process
	defer func() {
		if p != nil {
			p.close()
		}
	}()
	for i := 0; i < r.cfg.TxPerClient && ctx.Err() == nil; i++ {
		if p == nil {
			var err error
			if p, err = r.open(ctx, r.intN(2) == 0, true); err != nil {
				r.log("client %d: open: %v", id, err)
				time.Sleep(10 * time.Millisecond)
				i--
				continue
			}
		}
		timeout := time.Duration(1 + r.intN(int(r.cfg.MaxTimeout)))
		txCtx, cancel := context.WithTimeout(ctx, timeout)
		switch {
		case r.cfg.SQL && r.cfg.Workload == sim.ListAppend:
			r.listAppendSQL(txCtx, p, id)
		case r.cfg.SQL:
			r.bankSQL(txCtx, p, id)
		case r.cfg.Workload == sim.ListAppend:
			r.listAppendTx(txCtx, p, id)
		default:
			r.bankTx(txCtx, p, id)
		}
		expired := txCtx.Err() != nil
		cancel()
		// A short pause between transactions, as an application has, leaves room for compaction.
		time.Sleep(time.Duration(r.intN(int(5 * time.Millisecond))))
		if expired {
			r.mu.Lock()
			r.stats.Restarts++
			r.mu.Unlock()
			p.close()
			p = nil
		}
	}
}

// pause in the middle of a transaction now and then, like a stalled process, so that snapshots
// outlive compactions.
func (r *run) pause() {
	if r.intN(5) == 0 {
		time.Sleep(time.Duration(r.intN(int(20 * time.Millisecond))))
	}
}

type plan struct {
	append bool
	key    int
}

func (r *run) plans() ([]plan, bool) {
	var plans []plan
	readOnly := true
	for range 1 + r.intN(4) {
		p := plan{append: r.intN(2) == 0, key: r.intN(r.cfg.Keys)}
		readOnly = readOnly && !p.append
		plans = append(plans, p)
	}
	return plans, readOnly
}

func decodeList(b []byte) []int64 {
	list := []int64{}
	if len(b) == 0 {
		return list
	}
	for _, s := range strings.Split(string(b), ",") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			panic(fmt.Sprintf("workload: corrupt list %q", b))
		}
		list = append(list, v)
	}
	return list
}

func encodeList(list []int64) []byte {
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return []byte(strings.Join(parts, ","))
}

func (r *run) listAppendTx(ctx context.Context, p *process, id int) {
	plans, readOnly := r.plans()
	h := history.Tx{Client: id, Invoke: r.stamp()}
	begin := p.db.Begin
	if readOnly {
		begin = p.db.BeginReadOnly
	}
	tx, err := begin(ctx)
	if err != nil {
		return
	}
	for _, pl := range plans {
		r.pause()
		v, _, err := tx.Get(ctx, []byte(keyName(pl.key)))
		if err != nil {
			r.readError(id, tx.StartedAt(), err)
			// The reads that completed are real observations of a transaction that never committed.
			if len(h.Ops) > 0 {
				h.Complete, h.Outcome, h.Err = r.stamp(), history.Failed, err.Error()
				r.record(h)
			}
			return
		}
		list := decodeList(v)
		h.Ops = append(h.Ops, history.Op{Kind: history.Read, Key: keyName(pl.key), List: list})
		if pl.append {
			value := r.value(id)
			_ = tx.Put([]byte(keyName(pl.key)), encodeList(append(list, value)))
			h.Ops = append(h.Ops, history.Op{Kind: history.Append, Key: keyName(pl.key), Value: value})
		}
	}
	err = tx.Commit(ctx)
	h.Complete = r.stamp()
	if readOnly && err != nil {
		// A read-only commit does nothing, so an error means the reads never completed, and is
		// not an unknown outcome.
		h.Outcome = history.Failed
	} else {
		h.Outcome = r.outcome(err)
	}
	if err != nil {
		h.Err = err.Error()
	}
	r.record(h)
}

func (r *run) bankTx(ctx context.Context, p *process, id int) {
	h := history.Tx{Client: id, Invoke: r.stamp()}
	if r.intN(3) == 0 {
		tx, err := p.db.BeginReadOnly(ctx)
		if err != nil {
			return
		}
		balances := map[string]int64{}
		for i := range r.cfg.Keys {
			v, ok, err := tx.Get(ctx, []byte(keyName(i)))
			if err != nil {
				r.readError(id, tx.StartedAt(), err)
				return
			}
			if !ok {
				r.anomaly("client %d: account %s is missing", id, keyName(i))
				return
			}
			if balances[keyName(i)], err = strconv.ParseInt(string(v), 10, 64); err != nil {
				r.anomaly("client %d: corrupt balance %q", id, v)
				return
			}
		}
		h.Complete, h.Outcome = r.stamp(), history.OK
		r.audit(h, balances)
		return
	}
	from, to := keyName(r.intN(r.cfg.Keys)), keyName(r.intN(r.cfg.Keys))
	amount := int64(1 + r.intN(initialBalance/2))
	tx, err := p.db.Begin(ctx)
	if err != nil {
		return
	}
	read := func(key string) (int64, bool) {
		r.pause()
		v, ok, err := tx.Get(ctx, []byte(key))
		if err != nil {
			r.readError(id, tx.StartedAt(), err)
			return 0, false
		}
		if !ok {
			r.anomaly("client %d: account %s is missing", id, key)
			return 0, false
		}
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			r.anomaly("client %d: corrupt balance %q", id, v)
			return 0, false
		}
		return n, true
	}
	a, ok := read(from)
	if !ok {
		return
	}
	b, ok := read(to)
	if !ok || a < amount || from == to {
		_ = tx.Rollback()
		return
	}
	_ = tx.Put([]byte(from), []byte(strconv.FormatInt(a-amount, 10)))
	_ = tx.Put([]byte(to), []byte(strconv.FormatInt(b+amount, 10)))
	err = tx.Commit(ctx)
	h.Complete = r.stamp()
	h.Outcome = r.outcome(err)
	h.Ops = []history.Op{{Kind: history.Append, Key: from, Value: -amount}, {Kind: history.Append, Key: to, Value: amount}}
	if err != nil {
		h.Err = err.Error()
	}
	r.record(h)
}

func (r *run) listAppendSQL(ctx context.Context, p *process, id int) {
	plans, readOnly := r.plans()
	// A third of the operations, and every lone append, run as two autocommit statements: a read,
	// and an insert at the position it found, which is its own transaction in the history.
	if r.intN(3) == 0 || len(plans) == 1 && plans[0].append {
		k := plans[0].key
		h := history.Tx{Client: id, Invoke: r.stamp()}
		startedAt := time.Now()
		list, err := sim.ReadListSQL(ctx, p.sql, k)
		if err != nil {
			r.listReadError(id, startedAt, err)
			return
		}
		h.Ops = []history.Op{{Kind: history.Read, Key: keyName(k), List: list}}
		h.Complete, h.Outcome = r.stamp(), history.OK
		r.record(h)

		h = history.Tx{Client: id, Invoke: r.stamp()}
		value := r.value(id)
		var res sql.Result
		if sim.PackedList(k) {
			res, err = p.sql.ExecContext(ctx, "UPDATE packed SET vals = vals || $1 WHERE k = $2", sim.EncodePacked([]int64{value}), k)
		} else {
			res, err = p.sql.ExecContext(ctx, "INSERT INTO lists VALUES ($1, $2, $3)", k, len(list), value)
		}
		if err == nil {
			if n, _ := res.RowsAffected(); n != 1 {
				r.anomaly("client %d: append to %s affected %d rows", id, keyName(k), n)
			}
		}
		h.Complete = r.stamp()
		h.Ops = []history.Op{{Kind: history.Append, Key: keyName(k), Value: value}}
		h.Outcome = r.outcome(err)
		if err != nil {
			h.Err = err.Error()
		}
		r.record(h)
		return
	}

	h := history.Tx{Client: id, Invoke: r.stamp()}
	startedAt := time.Now()
	tx, err := p.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: readOnly})
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	fail := func(err error) {
		if len(h.Ops) > 0 {
			h.Complete, h.Outcome, h.Err = r.stamp(), history.Failed, err.Error()
			r.record(h)
		}
	}
	for _, pl := range plans {
		r.pause()
		list, err := sim.ReadListSQL(ctx, tx, pl.key)
		if err != nil {
			r.listReadError(id, startedAt, err)
			fail(err)
			return
		}
		h.Ops = append(h.Ops, history.Op{Kind: history.Read, Key: keyName(pl.key), List: list})
		if pl.append {
			value := r.value(id)
			var res sql.Result
			var err error
			if sim.PackedList(pl.key) {
				res, err = tx.ExecContext(ctx, "UPDATE packed SET vals = $1 WHERE k = $2", sim.EncodePacked(append(list, value)), pl.key)
			} else {
				res, err = tx.ExecContext(ctx, "INSERT INTO lists VALUES ($1, $2, $3)", pl.key, len(list), value)
			}
			if err != nil {
				fail(err)
				return
			}
			if n, _ := res.RowsAffected(); n != 1 {
				r.anomaly("client %d: append to %s affected %d rows", id, keyName(pl.key), n)
			}
			h.Ops = append(h.Ops, history.Op{Kind: history.Append, Key: keyName(pl.key), Value: value})
		}
	}
	err = tx.Commit()
	h.Complete = r.stamp()
	if readOnly && err != nil {
		// A read-only commit does nothing, so an error means the reads never completed, and is
		// not an unknown outcome.
		h.Outcome = history.Failed
	} else {
		h.Outcome = r.outcome(err)
	}
	if err != nil {
		h.Err = err.Error()
	}
	r.record(h)
}

func (r *run) bankSQL(ctx context.Context, p *process, id int) {
	h := history.Tx{Client: id, Invoke: r.stamp()}
	startedAt := time.Now()
	if r.intN(3) == 0 {
		tx, err := p.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return
		}
		defer func() { _ = tx.Rollback() }()
		rows, err := tx.QueryContext(ctx, "SELECT id, balance FROM accounts ORDER BY id")
		if err != nil {
			r.readError(id, startedAt, err)
			return
		}
		balances := map[string]int64{}
		for rows.Next() {
			var id, balance int64
			if err := rows.Scan(&id, &balance); err != nil {
				_ = rows.Close()
				return
			}
			balances[keyName(int(id))] = balance
		}
		_ = rows.Close()
		if rows.Err() != nil {
			return
		}
		if len(balances) != r.cfg.Keys {
			r.anomaly("client %d: read %d accounts, want %d", id, len(balances), r.cfg.Keys)
			return
		}
		h.Complete, h.Outcome = r.stamp(), history.OK
		r.audit(h, balances)
		return
	}
	from, to := r.intN(r.cfg.Keys), r.intN(r.cfg.Keys)
	amount := int64(1 + r.intN(initialBalance/2))
	if from == to {
		return
	}
	tx, err := p.sql.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	var balance int64
	if err := tx.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = $1", from).Scan(&balance); err != nil || balance < amount {
		r.readError(id, startedAt, err)
		return
	}
	for _, step := range []struct {
		id    int
		delta int64
	}{{from, -amount}, {to, amount}} {
		res, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + $1 WHERE id = $2", step.delta, step.id)
		if err != nil {
			return
		}
		if n, _ := res.RowsAffected(); n != 1 {
			r.anomaly("client %d: update of account %s affected %d rows", id, keyName(step.id), n)
			return
		}
	}
	err = tx.Commit()
	h.Complete = r.stamp()
	h.Outcome = r.outcome(err)
	h.Ops = []history.Op{{Kind: history.Append, Key: keyName(from), Value: -amount}, {Kind: history.Append, Key: keyName(to), Value: amount}}
	if err != nil {
		h.Err = err.Error()
	}
	r.record(h)
}

// readFinal reads every list or account with a new engine without caches, retrying until the
// store answers.
func (r *run) readFinal(ctx context.Context) (map[string][]int64, map[string]int64, error) {
	var lists map[string][]int64
	var balances map[string]int64
	err := retry(ctx, func() error {
		p, err := r.open(ctx, false, false)
		if err != nil {
			return err
		}
		defer p.close()
		lists, balances = map[string][]int64{}, map[string]int64{}
		if p.db != nil {
			tx, err := p.db.BeginReadOnly(ctx)
			if err != nil {
				return err
			}
			it := tx.Scan(ctx, nil, nil)
			for it.Next() {
				key := string(it.Key())
				if r.cfg.Workload == sim.ListAppend {
					lists[key] = decodeList(it.Value())
					continue
				}
				if balances[key], err = strconv.ParseInt(string(it.Value()), 10, 64); err != nil {
					return err
				}
			}
			return it.Err()
		}
		if r.cfg.Workload == sim.ListAppend {
			rows, err := p.sql.QueryContext(ctx, "SELECT k, v FROM lists ORDER BY k, pos")
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var k, v int64
				if err := rows.Scan(&k, &v); err != nil {
					return err
				}
				lists[keyName(int(k))] = append(lists[keyName(int(k))], v)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			packed, err := p.sql.QueryContext(ctx, "SELECT k, vals FROM packed ORDER BY k")
			if err != nil {
				return err
			}
			defer func() { _ = packed.Close() }()
			for packed.Next() {
				var k int64
				var vals string
				if err := packed.Scan(&k, &vals); err != nil {
					return err
				}
				list, err := sim.DecodePacked(vals)
				if err != nil {
					r.anomaly("final read: %v", err)
					continue
				}
				if len(list) > 0 {
					lists[keyName(int(k))] = list
				}
			}
			return packed.Err()
		}
		rows, err := p.sql.QueryContext(ctx, "SELECT id, balance FROM accounts")
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, balance int64
			if err := rows.Scan(&id, &balance); err != nil {
				return err
			}
			balances[keyName(int(id))] = balance
		}
		return rows.Err()
	})
	return lists, balances, err
}
