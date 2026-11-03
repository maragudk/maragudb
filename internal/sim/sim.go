// Package sim is a deterministic simulator: many client processes, each with its own engine,
// share one fault-injecting fake store under a cooperative scheduler in virtual time. The same
// seed always produces the same history, and the history is checked for anomalies.
package sim

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sched"
)

// Workload kind.
type Workload int

// Workloads.
const (
	// ListAppend keeps a list per key; transactions read lists and append unique values.
	ListAppend Workload = iota
	// Bank keeps account balances; transactions transfer money and audits read every account.
	Bank
)

func (w Workload) String() string {
	if w == Bank {
		return "bank"
	}
	return "list-append"
}

// Config of a simulation run.
type Config struct {
	Seed     uint64
	Workload Workload
	// Clients is the number of concurrent client processes.
	Clients int
	// TxPerClient is how many transactions each client runs.
	TxPerClient int
	// Keys for the list-append workload, or accounts for the bank workload.
	Keys int
	// Maintainers is the number of processes that compact and garbage collect.
	Maintainers int
	// Faults rates. See [s3fake.Faults].
	ErrorRate   float64
	LostAckRate float64
	CrashRate   float64
	MaxLatency  time.Duration
	// GracePeriod for GC.
	GracePeriod time.Duration
	// Mutations to break the engine with.
	Mutations engine.Mutations
	// Trace receives one line per event when non-nil.
	Trace func(string)
}

// DefaultConfig for a seed: a moderately faulty run that finishes in well under a second.
func DefaultConfig(seed uint64) Config {
	return Config{
		Seed:        seed,
		Workload:    ListAppend,
		Clients:     6,
		TxPerClient: 25,
		Keys:        4,
		Maintainers: 1,
		ErrorRate:   0.05,
		LostAckRate: 0.05,
		CrashRate:   0.01,
		MaxLatency:  20 * time.Millisecond,
		GracePeriod: 2 * time.Second,
	}
}

// Result of a run.
type Result struct {
	Config    Config
	History   []history.Tx
	Audits    []history.BankAudit
	Final     map[string][]int64
	Balances  map[string]int64
	Anomalies []error
	Stats     Stats
	// Digest summarizes the whole run; two runs with the same seed must have equal digests.
	Digest string
}

// Stats of a run.
type Stats struct {
	Commits, Conflicts, Unknown, Crashes, Compactions, GCs, Deleted int
	Elapsed                                                         time.Duration
}

// Failed reports whether any anomaly was found.
func (r Result) Failed() bool {
	return len(r.Anomalies) > 0
}

// Report of the run for test output.
func (r Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d (%v): %d tx, %d commits, %d conflicts, %d unknown, %d crashes, %d compactions, %d GCs (%d deleted), %v virtual time",
		r.Config.Seed, r.Config.Workload, len(r.History), r.Stats.Commits, r.Stats.Conflicts, r.Stats.Unknown, r.Stats.Crashes, r.Stats.Compactions, r.Stats.GCs, r.Stats.Deleted, r.Stats.Elapsed)
	for _, a := range r.Anomalies {
		fmt.Fprintf(&b, "\nANOMALY: %v", a)
	}
	return b.String()
}

type run struct {
	cfg    Config
	sched  *sched.Scheduler
	fake   *s3fake.Client
	rng    *rand.Rand
	txs    []history.Tx
	audits []history.BankAudit
	anoms  []error
	stats  Stats
	total  int64
	// activeClients counts clients still running, so maintainers know when to stop.
	activeClients int
}

const initialBalance = 100

// Run a simulation.
func Run(cfg Config) Result {
	r := &run{cfg: cfg}
	r.sched = sched.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	r.rng = rand.New(rand.NewPCG(cfg.Seed, 0x5ca1ab1e))
	r.fake = s3fake.New(s3fake.WithClock(r.sched), s3fake.WithFaults(s3fake.Faults{
		Rand:        rand.New(rand.NewPCG(cfg.Seed, 0xfa17)),
		ErrorRate:   cfg.ErrorRate,
		LostAckRate: cfg.LostAckRate,
		CrashRate:   cfg.CrashRate,
		MaxLatency:  cfg.MaxLatency,
	}))
	if cfg.Trace != nil {
		r.fake.Log = func(line string) {
			cfg.Trace(fmt.Sprintf("%v s3 %s", r.sched.Elapsed(), line))
		}
	}

	if cfg.Workload == Bank {
		r.seedBank()
		r.sched.Run()
	}
	r.activeClients = cfg.Clients
	for i := range cfg.Clients {
		c := &client{run: r, id: i, rng: rand.New(rand.NewPCG(cfg.Seed, uint64(i)+1)), disk: cache.NewMemory(0)}
		r.sched.Go(fmt.Sprint("client", i), c.loop)
	}
	for i := range cfg.Maintainers {
		m := &maintainer{run: r, id: i, rng: rand.New(rand.NewPCG(cfg.Seed, 0xaaaa+uint64(i)))}
		r.sched.Go(fmt.Sprint("maintainer", i), m.loop)
	}
	r.sched.Run()
	r.stats.Elapsed = r.sched.Elapsed()

	res := Result{Config: cfg, History: r.txs, Audits: r.audits, Stats: r.stats}
	final, balances, err := r.readFinal()
	if err != nil {
		r.anoms = append(r.anoms, fmt.Errorf("reading final state: %w", err))
	}
	res.Final = final
	res.Balances = balances
	switch cfg.Workload {
	case ListAppend:
		if err == nil {
			if err := history.CheckDurability(r.txs, final); err != nil {
				r.anoms = append(r.anoms, err)
			}
		}
		// The list-append checker works from the reads alone when the final state is unavailable.
		if err := history.CheckListAppend(r.txs, final); err != nil {
			r.anoms = append(r.anoms, err)
		}
	case Bank:
		if err == nil {
			r.audits = append(r.audits, history.BankAudit{TxID: -1, Balances: balances})
		}
		res.Audits = r.audits
		if err := history.CheckBank(r.audits, r.total); err != nil {
			r.anoms = append(r.anoms, err)
		}
	}
	res.Anomalies = r.anoms
	assert.Always(len(r.anoms) == 0, "the simulated history has no anomalies", map[string]any{"seed": cfg.Seed})
	res.Digest = r.digest(res)
	return res
}

func (r *run) digest(res Result) string {
	var b strings.Builder
	for _, tx := range res.History {
		b.WriteString(tx.String())
		b.WriteByte('\n')
	}
	for _, a := range res.Audits {
		fmt.Fprintf(&b, "audit %d: %v\n", a.TxID, a.Balances)
	}
	fmt.Fprintf(&b, "final: %v\nbalances: %v\nobjects: %v\nstats: %+v\n", res.Final, res.Balances, r.fake.Keys(), res.Stats)
	return b.String()
}

func (r *run) trace(format string, args ...any) {
	if r.cfg.Trace != nil {
		r.cfg.Trace(fmt.Sprintf("%v %s", r.sched.Elapsed(), fmt.Sprintf(format, args...)))
	}
}

func (r *run) anomaly(format string, args ...any) {
	err := fmt.Errorf(format, args...)
	r.trace("ANOMALY %v", err)
	r.anoms = append(r.anoms, err)
}

// open an engine for a process. Clients vary in caching: every third one has no cache at all,
// so that its reads go to the store and exercise the garbage collection grace period, and every
// third one has only a disk cache.
func (r *run) open(ctx context.Context, id int, rng *rand.Rand, disk cache.Cache) (*engine.DB, error) {
	grace := r.cfg.GracePeriod
	opts := engine.Options{
		Client:        r.fake,
		Prefix:        "db",
		DiskCache:     disk,
		GCGracePeriod: &grace,
		Now:           r.sched.Now,
		Rand:          rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64())),
		Mutations:     r.cfg.Mutations,
	}
	switch id % 3 {
	case 1:
		opts.MemoryCacheBytes = new(int64)
		opts.DiskCache = nil
	case 2:
		opts.MemoryCacheBytes = new(int64)
	}
	return engine.Open(ctx, opts)
}

// seedBank writes the initial balances with a fault-free client before the clients start.
func (r *run) seedBank() {
	r.total = int64(r.cfg.Keys) * initialBalance
	r.sched.Go("seed", func(ctx context.Context) {
		for {
			db, err := r.open(ctx, 0, r.rng, nil)
			if err != nil {
				continue
			}
			tx, err := db.Begin(ctx)
			if err != nil {
				continue
			}
			for i := range r.cfg.Keys {
				_ = tx.Put([]byte(keyName(i)), []byte(strconv.Itoa(initialBalance)))
			}
			if err := tx.Commit(ctx); err == nil {
				return
			}
			if _, ok := checkExists(ctx, db, keyName(0)); ok {
				return
			}
		}
	})
}

func checkExists(ctx context.Context, db *engine.DB, key string) ([]byte, bool) {
	tx, err := db.BeginReadOnly(ctx)
	if err != nil {
		return nil, false
	}
	v, ok, err := tx.Get(ctx, []byte(key))
	if err != nil {
		return nil, false
	}
	return v, ok
}

// readFinal reads every key with a fresh, fault-free, cache-free process.
func (r *run) readFinal() (map[string][]int64, map[string]int64, error) {
	var lists map[string][]int64
	var balances map[string]int64
	var err error
	r.sched.Go("final", func(ctx context.Context) {
		var db *engine.DB
		db, err = engine.Open(ctx, engine.Options{Client: r.fakeWithoutFaults(), Prefix: "db", Now: r.sched.Now, MemoryCacheBytes: new(int64)})
		if err != nil {
			return
		}
		var tx *engine.Tx
		tx, err = db.BeginReadOnly(ctx)
		if err != nil {
			return
		}
		it := tx.Scan(ctx, nil, nil)
		lists = map[string][]int64{}
		balances = map[string]int64{}
		for it.Next() {
			key := string(it.Key())
			switch r.cfg.Workload {
			case ListAppend:
				lists[key] = decodeList(it.Value())
			case Bank:
				balances[key], err = strconv.ParseInt(string(it.Value()), 10, 64)
				if err != nil {
					return
				}
			}
		}
		err = it.Err()
	})
	r.sched.Run()
	return lists, balances, err
}

// fakeWithoutFaults is the same store with faults disabled, for the final read.
func (r *run) fakeWithoutFaults() *s3fake.Client {
	return r.fake.WithoutFaults()
}

func keyName(i int) string {
	return fmt.Sprintf("k%02d", i)
}

func decodeList(b []byte) []int64 {
	if len(b) == 0 {
		return []int64{}
	}
	var list []int64
	for _, s := range strings.Split(string(b), ",") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			panic(fmt.Sprintf("sim: corrupt list %q", b))
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

// client is one simulated process running transactions. It restarts after a crash, keeping its
// disk cache but losing its memory.
type client struct {
	run  *run
	id   int
	rng  *rand.Rand
	disk cache.Cache
	proc *s3fake.Process
	db   *engine.DB
	next int64
}

func (c *client) loop(ctx context.Context) {
	defer func() {
		c.run.activeClients--
	}()
	for i := 0; i < c.run.cfg.TxPerClient; i++ {
		if c.db == nil {
			c.proc = &s3fake.Process{Name: fmt.Sprint("client", c.id)}
			pctx := s3fake.WithProcess(ctx, c.proc)
			db, err := c.run.open(pctx, c.id, c.rng, c.disk)
			if err != nil {
				c.run.trace("client %d: open: %v", c.id, err)
				c.afterCrash()
				c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(10*time.Millisecond))))
				i--
				continue
			}
			c.db = db
		}
		pctx := s3fake.WithProcess(ctx, c.proc)
		switch c.run.cfg.Workload {
		case ListAppend:
			c.listAppendTx(pctx)
		case Bank:
			c.bankTx(pctx)
		}
		c.afterCrash()
		c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(5*time.Millisecond))))
	}
}

func (c *client) afterCrash() {
	if c.proc == nil {
		return
	}
	if crashed, step := c.proc.Crashed(); crashed {
		c.run.trace("client %d: crashed at %s, restarting", c.id, step)
		c.run.stats.Crashes++
		c.db = nil
		c.proc = nil
	}
}

// pause the client in the middle of a transaction now and then, like a stalled process,
// so that snapshots outlive compactions.
func (c *client) pause(ctx context.Context) {
	if c.rng.IntN(5) == 0 {
		c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(200*time.Millisecond))))
	}
}

func (c *client) value() int64 {
	c.next++
	return int64(c.id)<<32 | c.next
}

// readError handles an error from a read: it classifies snapshot expiry as an anomaly when the
// transaction is younger than the grace period.
func (c *client) readError(tx *engine.Tx, err error) {
	if errors.Is(err, engine.ErrSnapshotExpired) {
		age := c.run.sched.Now().Sub(tx.StartedAt())
		assert.Sometimes(age >= c.run.cfg.GracePeriod, "a reader older than the grace period is told so", nil)
		if age < c.run.cfg.GracePeriod {
			c.run.anomaly("client %d: snapshot expired after %v, grace period is %v: %v", c.id, age, c.run.cfg.GracePeriod, err)
		}
	}
}

func (c *client) record(tx history.Tx) {
	tx.ID = len(c.run.txs) + 1
	c.run.txs = append(c.run.txs, tx)
	c.run.trace("%v", tx)
}

func (c *client) outcome(err error) history.Outcome {
	switch {
	case err == nil:
		c.run.stats.Commits++
		return history.OK
	case errors.Is(err, engine.ErrConflict):
		c.run.stats.Conflicts++
		return history.Failed
	default:
		c.run.stats.Unknown++
		return history.Unknown
	}
}

func (c *client) listAppendTx(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.sched.Elapsed()}
	nOps := 1 + c.rng.IntN(4)
	type plan struct {
		append bool
		key    string
	}
	var plans []plan
	readOnly := true
	for range nOps {
		p := plan{append: c.rng.IntN(2) == 0, key: keyName(c.rng.IntN(c.run.cfg.Keys))}
		if p.append {
			readOnly = false
		}
		plans = append(plans, p)
	}

	var tx *engine.Tx
	var err error
	if readOnly {
		tx, err = c.db.BeginReadOnly(ctx)
	} else {
		tx, err = c.db.Begin(ctx)
	}
	if err != nil {
		c.run.trace("client %d: begin: %v", c.id, err)
		return
	}
	for _, p := range plans {
		c.pause(ctx)
		v, _, err := tx.Get(ctx, []byte(p.key))
		if err != nil {
			c.readError(tx, err)
			c.run.trace("client %d: read: %v", c.id, err)
			return
		}
		list := decodeList(v)
		h.Ops = append(h.Ops, history.Op{Kind: history.Read, Key: p.key, List: list})
		if p.append {
			value := c.value()
			if err := tx.Put([]byte(p.key), encodeList(append(list, value))); err != nil {
				panic(err)
			}
			h.Ops = append(h.Ops, history.Op{Kind: history.Append, Key: p.key, Value: value})
		}
	}
	err = tx.Commit(ctx)
	h.Complete = c.run.sched.Elapsed()
	h.Outcome = c.outcome(err)
	if err != nil {
		h.Err = err.Error()
	}
	if readOnly && err != nil {
		// A read-only commit does nothing, so an error means the reads never completed.
		h.Outcome = history.Failed
	}
	c.record(h)
}

func (c *client) bankTx(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.sched.Elapsed()}
	if c.rng.IntN(3) == 0 {
		tx, err := c.db.BeginReadOnly(ctx)
		if err != nil {
			return
		}
		balances := map[string]int64{}
		for i := range c.run.cfg.Keys {
			v, ok, err := tx.Get(ctx, []byte(keyName(i)))
			if err != nil {
				c.readError(tx, err)
				return
			}
			if !ok {
				c.run.anomaly("client %d: account %s is missing", c.id, keyName(i))
				return
			}
			balances[keyName(i)], err = strconv.ParseInt(string(v), 10, 64)
			if err != nil {
				c.run.anomaly("client %d: corrupt balance %q", c.id, v)
				return
			}
		}
		h.Complete = c.run.sched.Elapsed()
		h.Outcome = history.OK
		c.record(h)
		c.run.audits = append(c.run.audits, history.BankAudit{TxID: h.ID, Balances: balances})
		return
	}

	from := keyName(c.rng.IntN(c.run.cfg.Keys))
	to := keyName(c.rng.IntN(c.run.cfg.Keys))
	amount := int64(1 + c.rng.IntN(initialBalance/2))
	tx, err := c.db.Begin(ctx)
	if err != nil {
		return
	}
	read := func(key string) (int64, bool) {
		c.pause(ctx)
		v, ok, err := tx.Get(ctx, []byte(key))
		if err != nil {
			c.readError(tx, err)
			return 0, false
		}
		if !ok {
			c.run.anomaly("client %d: account %s is missing", c.id, key)
			return 0, false
		}
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			c.run.anomaly("client %d: corrupt balance %q", c.id, v)
			return 0, false
		}
		return n, true
	}
	a, ok := read(from)
	if !ok {
		return
	}
	b, ok := read(to)
	if !ok {
		return
	}
	if a < amount || from == to {
		_ = tx.Rollback()
		return
	}
	_ = tx.Put([]byte(from), []byte(strconv.FormatInt(a-amount, 10)))
	_ = tx.Put([]byte(to), []byte(strconv.FormatInt(b+amount, 10)))
	err = tx.Commit(ctx)
	h.Complete = c.run.sched.Elapsed()
	h.Outcome = c.outcome(err)
	h.Ops = []history.Op{{Kind: history.Append, Key: from, Value: -amount}, {Kind: history.Append, Key: to, Value: amount}}
	if err != nil {
		h.Err = err.Error()
	}
	c.record(h)
}

// maintainer is a process that compacts and garbage collects at random intervals.
type maintainer struct {
	run  *run
	id   int
	rng  *rand.Rand
	proc *s3fake.Process
	db   *engine.DB
}

func (m *maintainer) loop(ctx context.Context) {
	for m.run.activeClients > 0 {
		m.run.sched.Sleep(ctx, time.Duration(m.rng.Int64N(int64(200*time.Millisecond))))
		if m.db == nil {
			m.proc = &s3fake.Process{Name: fmt.Sprint("maintainer", m.id)}
			db, err := m.run.open(s3fake.WithProcess(ctx, m.proc), 0, m.rng, nil)
			if err != nil {
				m.afterCrash()
				continue
			}
			m.db = db
		}
		pctx := s3fake.WithProcess(ctx, m.proc)
		if m.rng.IntN(2) == 0 {
			// Compaction loses to concurrent commits, so retry a few times.
			var err error
			for range 5 {
				err = m.db.Compact(pctx)
				if !errors.Is(err, engine.ErrConflict) {
					break
				}
			}
			if err == nil {
				m.run.stats.Compactions++
			}
			m.run.trace("maintainer %d: compact: %v", m.id, err)
			if crashed, _ := m.proc.Crashed(); crashed {
				m.afterCrash()
				continue
			}
		}
		n, err := m.db.GC(pctx)
		if err == nil {
			m.run.stats.GCs++
			m.run.stats.Deleted += n
		}
		m.run.trace("maintainer %d: gc: %d deleted, %v", m.id, n, err)
		m.afterCrash()
	}
}

func (m *maintainer) afterCrash() {
	if m.proc == nil {
		return
	}
	if crashed, step := m.proc.Crashed(); crashed {
		m.run.trace("maintainer %d: crashed at %s, restarting", m.id, step)
		m.run.stats.Crashes++
		m.db = nil
		m.proc = nil
	}
}
