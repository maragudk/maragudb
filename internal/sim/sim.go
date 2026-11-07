// Package sim is a deterministic simulator: many client processes, each with its own engine,
// share one fault-injecting fake store under a cooperative scheduler in virtual time. The same
// seed always produces the same history, and the history is checked for anomalies.
package sim

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/fsfake"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sched"
	"maragu.dev/maragudb/internal/sql/exec"
)

// Workload kind.
type Workload int

// Workloads.
const (
	// ListAppend keeps a list per key; transactions read lists and append unique values.
	ListAppend Workload = iota
	// Bank keeps account balances; transactions transfer money and audits read every account.
	Bank
	// Unique keeps rows with a unique column, which transactions insert, move, and delete
	// concurrently, and audits check. It runs only through SQL.
	Unique
	// ForeignKey keeps tables that reference each other and themselves, with CHECK constraints,
	// which transactions insert into, update, and delete from concurrently, also through foreign
	// key actions, and audits check. It runs only through SQL.
	ForeignKey
)

func (w Workload) String() string {
	switch w {
	case Bank:
		return "bank"
	case Unique:
		return "unique"
	case ForeignKey:
		return "foreign-key"
	}
	return "list-append"
}

// Name of the configuration's workload, with its API and maintenance mode.
func (c Config) Name() string {
	name := c.Workload.String()
	if c.SQL {
		name += " through SQL"
	}
	if c.Maintenance != MaintenanceOff {
		name += " with maintenance " + c.Maintenance.String()
	}
	return name
}

// MaintenanceMode selects how the clients run background maintenance, which the clients drive
// deterministically between their transactions through the engine's step function.
type MaintenanceMode int

// Maintenance modes.
const (
	// MaintenanceOff runs no maintenance in the clients; only [Config.Maintainers] compact.
	MaintenanceOff MaintenanceMode = iota
	// MaintenanceLease coordinates the clients' maintenance through the lease.
	MaintenanceLease
	// MaintenanceNoLease runs maintenance in every client without the lease.
	MaintenanceNoLease
	// MaintenanceOverlap forces overlapping leases: every lease expires at once and no client
	// renews or notices, so that several clients believe they hold it at the same time.
	MaintenanceOverlap
)

func (m MaintenanceMode) String() string {
	switch m {
	case MaintenanceLease:
		return "lease"
	case MaintenanceNoLease:
		return "no lease"
	case MaintenanceOverlap:
		return "overlapping leases"
	}
	return "off"
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
	// Maintenance mode of the clients.
	Maintenance MaintenanceMode
	// Faults rates. See [s3fake.Faults].
	ErrorRate   float64
	LostAckRate float64
	CrashRate   float64
	MaxLatency  time.Duration
	// OutageRate, MaxOutage, and OutageTimeout make the store unreachable for a stretch of
	// virtual time, for every process or for one, while the processes keep running. See
	// [s3fake.Faults].
	OutageRate    float64
	MaxOutage     time.Duration
	OutageTimeout time.Duration
	// GracePeriod for GC.
	GracePeriod time.Duration
	// Tiers of the compaction that the clients' maintenance and the maintainers run.
	Tiers engine.Tiers
	// Mutations to break the engine with.
	Mutations engine.Mutations
	// SQL runs the clients through the SQL layer and the database/sql driver instead of the
	// engine's key-value API.
	SQL bool
	// SQLMutations to break the SQL layer with. Only used with SQL.
	SQLMutations exec.Mutations
	// DiskFaults bounds the disk cache of every client that has one to between 2 and 16 blocks of
	// 4 KiB, so that it evicts, and damages its files at every crash and now and then between
	// transactions. Without it, the disk caches are far larger than a run fills, and their files
	// stay whole.
	DiskFaults bool
	// DiskMutations to break the clients' disk caches with.
	DiskMutations cache.DiskMutations
	// StoreDamage makes the store damage the body of one in fifty of the reads of the clients and
	// the maintainers, with a flipped bit, a cut, or another object's bytes at the same range. The
	// objects in the store stay whole. Without it, no read is damaged, and the earlier
	// configurations keep their histories.
	StoreDamage bool
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
		// Outages long enough that a commit sometimes runs out of retries in one, so its outcome
		// is unknown in a process that keeps running.
		OutageRate:    0.01,
		MaxOutage:     time.Second,
		OutageTimeout: 50 * time.Millisecond,
		GracePeriod:   2 * time.Second,
		// Tiers far smaller than the defaults, for the small databases of a run, so that
		// compaction merges segments of several tiers, and rarely merges the whole database.
		Tiers: engine.Tiers{FanIn: 3, BaseBytes: 64, FullRatio: 2},
	}
}

// Result of a run.
type Result struct {
	Config   Config
	History  []history.Tx
	Audits   []history.BankAudit
	Final    map[string][]int64
	Balances map[string]int64
	// Users are the final rows of the unique workload, or of the foreign key workload.
	Users []string
	// Anomalies found, each an [*Anomaly].
	Anomalies []error
	Stats     Stats
	// Reached counts the interesting states the run reached. See [reach.Declarations].
	Reached map[reach.State]int
	// Digest summarizes the whole run; two runs with the same seed must have equal digests.
	Digest string
}

// Stats of a run.
type Stats struct {
	Commits, Conflicts, Unknown, Crashes, Compactions, GCs, Deleted int
	// CompactionAttempts counts every compaction call, succeeded or not, and CompactionConflicts
	// the ones that lost to another compaction, so that the success rate under concurrent
	// writers can be reported.
	CompactionAttempts, CompactionConflicts int
	// CompactionRebases counts the compactions that reapplied their swap on top of newer commits.
	CompactionRebases int
	// Outages and partitions injected.
	Outages int
	// LiveUnknown counts the unknown commit outcomes in processes that did not crash, which only
	// outages and lost acknowledgements cause.
	LiveUnknown int
	// Unavailable counts client operations that failed because the store was unreachable.
	Unavailable int
	// FailedReads counts reads in client transactions that failed for any reason.
	FailedReads int
	// UniqueViolations counts the statements of the unique workload that failed because they
	// would have repeated a unique value.
	UniqueViolations int
	// ForeignKeyViolations and CheckViolations count the statements of the foreign key workload
	// that failed because of a foreign key or a CHECK.
	ForeignKeyViolations, CheckViolations int
	// MaintenanceSteps counts the clients' maintenance steps, MaintenanceAttempts the ones that
	// found work and got past the lease, MaintenanceRuns the ones that changed the database,
	// and MergesStarted the compactions that began merging in those steps. LeaseAcquisitions
	// and LeaseLosses count the lease handovers.
	MaintenanceSteps, MaintenanceAttempts, MaintenanceRuns, MergesStarted int
	LeaseAcquisitions, LeaseLosses                                        int
	Elapsed                                                               time.Duration
}

// Add the counts of o, for totals over several runs.
func (s *Stats) Add(o Stats) {
	s.Commits += o.Commits
	s.Conflicts += o.Conflicts
	s.Unknown += o.Unknown
	s.Crashes += o.Crashes
	s.Compactions += o.Compactions
	s.CompactionAttempts += o.CompactionAttempts
	s.CompactionConflicts += o.CompactionConflicts
	s.CompactionRebases += o.CompactionRebases
	s.GCs += o.GCs
	s.Deleted += o.Deleted
	s.Outages += o.Outages
	s.LiveUnknown += o.LiveUnknown
	s.Unavailable += o.Unavailable
	s.FailedReads += o.FailedReads
	s.UniqueViolations += o.UniqueViolations
	s.ForeignKeyViolations += o.ForeignKeyViolations
	s.CheckViolations += o.CheckViolations
	s.MaintenanceSteps += o.MaintenanceSteps
	s.MaintenanceAttempts += o.MaintenanceAttempts
	s.MaintenanceRuns += o.MaintenanceRuns
	s.MergesStarted += o.MergesStarted
	s.LeaseAcquisitions += o.LeaseAcquisitions
	s.LeaseLosses += o.LeaseLosses
	s.Elapsed += o.Elapsed
}

// Checkers that find anomalies.
const (
	// CheckerListAppend is [history.CheckListAppend], the strict serializability checker.
	CheckerListAppend = "list-append"
	// CheckerDurability is [history.CheckDurability] on the final state.
	CheckerDurability = "durability"
	// CheckerBank is [history.CheckBank], the conservation of money.
	CheckerBank = "bank"
	// CheckerGracePeriod finds readers younger than the GC grace period that lost their snapshot.
	CheckerGracePeriod = "grace period"
	// CheckerWorkload finds states the workload never writes, such as a missing account.
	CheckerWorkload = "workload invariants"
	// CheckerFinalRead fails when the final state cannot be read back.
	CheckerFinalRead = "final read"
	// CheckerUnique finds two rows with the same value of a unique column, in a snapshot that a
	// reader saw or in the final state.
	CheckerUnique = "unique"
	// CheckerIndex finds an index that does not match its table: in the final state of the
	// store, with [exec.CheckIndexes], or through a lookup that finds other rows than the table.
	CheckerIndex = "index consistency"
	// CheckerForeignKey finds a row whose foreign key references a missing row, in a snapshot
	// that a reader saw, or in the final state of the store, with [exec.CheckConstraints].
	CheckerForeignKey = "foreign key"
	// CheckerCheck finds a row that violates a CHECK constraint, in a snapshot that a reader saw,
	// or in the final state of the store, with [exec.CheckConstraints].
	CheckerCheck = "check constraint"
	// CheckerStorage finds objects that garbage collection never deletes, or that it fails on,
	// after the run: a fresh process runs GC a grace period apart until all garbage has expired,
	// and then the store must hold only the manifest, the lease, the live segments, and the
	// objects of writes that the manifest has not moved past.
	CheckerStorage = "storage"
	// CheckerRealTime finds a reader that misses a commit acknowledged before it began, through
	// a counter that only grows.
	CheckerRealTime = "real-time order"
	// CheckerDiskCache finds a client whose disk cache holds files that take more bytes than its
	// bound, or more than it counts, after any of its operations.
	CheckerDiskCache = "disk cache bound"
	// CheckerCache finds a client whose memory or disk cache returned a block or a manifest that
	// failed the engine's check: the cache did not keep what it was given, or the engine gave it
	// bytes that it had not checked.
	CheckerCache = "cache integrity"
	// CheckerDamage finds an operation of a client that failed with [engine.ErrCorrupt] although
	// the store damaged none of the reads of the operation, which means damaged bytes got past a
	// check earlier, and an operation through the engine that failed with an error of no kind
	// that the engine or the store gives, such as a block that did not decode.
	CheckerDamage = "damaged reads"
	// CheckerPanic is a panic in a task of the run, whose value and stack it reports.
	CheckerPanic = "panic"
)

// Anomaly found in a run, with the checker that found it.
type Anomaly struct {
	Checker string
	Err     error
}

func (a *Anomaly) Error() string {
	return a.Checker + ": " + a.Err.Error()
}

func (a *Anomaly) Unwrap() error {
	return a.Err
}

// Checkers that found the anomalies of the run, each once, in the order they found them.
func (r Result) Checkers() []string {
	var names []string
	seen := map[string]bool{}
	for _, err := range r.Anomalies {
		var a *Anomaly
		if errors.As(err, &a) && !seen[a.Checker] {
			seen[a.Checker] = true
			names = append(names, a.Checker)
		}
	}
	return names
}

// Failed reports whether any anomaly was found.
func (r Result) Failed() bool {
	return len(r.Anomalies) > 0
}

// Report of the run for test output.
func (r Result) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "seed %d (%v): %d tx, %d commits, %d conflicts, %d unknown (%d without a crash), %d crashes, %d outages (%d operations failed), %d compactions, %d GCs (%d deleted), %d maintenance runs (%d lease acquisitions, %d losses), %v virtual time",
		r.Config.Seed, r.Config.Name(), len(r.History), r.Stats.Commits, r.Stats.Conflicts, r.Stats.Unknown, r.Stats.LiveUnknown, r.Stats.Crashes, r.Stats.Outages, r.Stats.Unavailable, r.Stats.Compactions, r.Stats.GCs, r.Stats.Deleted, r.Stats.MaintenanceRuns, r.Stats.LeaseAcquisitions, r.Stats.LeaseLosses, r.Stats.Elapsed)
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
	// lastStamp makes history timestamps strictly increasing in event order, even when virtual
	// time did not advance between two events.
	lastStamp time.Duration
	// reached counts the interesting states of the run.
	reached reach.Counter
	// clients of the run, and whether two of their maintainers held the lease at once when last
	// looked at.
	clients     []*client
	overlapping bool
	// ticked is the highest value of the counter of the foreign key workload that an
	// acknowledged commit wrote.
	ticked int64
	// panicked is the first panic that the SQL driver recovered, with the stack where it
	// happened, which the task that made the call panics with again once the call returned.
	panicked *sched.TaskPanic
}

// recovered records a panic that the SQL driver recovered, and returns the error that the call
// returns instead, so that database/sql unwinds without leaving its locks held.
func (r *run) recovered(value any, stack []byte) error {
	if r.panicked == nil {
		r.panicked = &sched.TaskPanic{Value: value, Stack: stack}
	}
	return fmt.Errorf("sim: the call panicked: %v", value)
}

// repanic with the panic that the SQL driver recovered, if any, so that the run fails with it.
func (r *run) repanic() {
	if r.panicked != nil {
		panic(r.panicked)
	}
}

// goTask starts a scheduled task whose context counts the states it reaches.
func (r *run) goTask(name string, f func(ctx context.Context)) {
	r.sched.Go(name, func(ctx context.Context) {
		f(reach.WithCounter(ctx, &r.reached))
	})
}

// stamp returns a timestamp for a history event that is at least the virtual time and later than every earlier stamp.
func (r *run) stamp() time.Duration {
	t := r.sched.Elapsed()
	if t <= r.lastStamp {
		t = r.lastStamp + 1
	}
	r.lastStamp = t
	return t
}

const initialBalance = 100

// damageRate of the reads of the run's store.
func damageRate(cfg Config) float64 {
	if cfg.StoreDamage {
		return 0.02
	}
	return 0
}

// Run a simulation. A panic in a task of the run ends it at once, with the panic as the
// anomaly of [CheckerPanic].
func Run(cfg Config) (res Result) {
	r := &run{cfg: cfg}
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		tp, ok := p.(*sched.TaskPanic)
		if !ok {
			panic(p)
		}
		if inner, ok := tp.Value.(*sched.TaskPanic); ok {
			// A panic that the SQL driver recovered, which the task panicked with again.
			inner.Task = tp.Task
			tp = inner
		}
		err := error(tp)
		r.trace("ANOMALY %v", err)
		res = Result{Config: cfg, History: r.txs, Audits: r.audits, Stats: r.stats, Reached: r.reached.Counts(), Anomalies: append(r.anoms, &Anomaly{Checker: CheckerPanic, Err: err})}
	}()
	r.sched = sched.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	r.rng = rand.New(rand.NewPCG(cfg.Seed, 0x5ca1ab1e))
	r.fake = s3fake.New(s3fake.WithClock(r.sched), s3fake.WithFaults(s3fake.Faults{
		Rand:          rand.New(rand.NewPCG(cfg.Seed, 0xfa17)),
		ErrorRate:     cfg.ErrorRate,
		LostAckRate:   cfg.LostAckRate,
		CrashRate:     cfg.CrashRate,
		MaxLatency:    cfg.MaxLatency,
		OutageRate:    cfg.OutageRate,
		MaxOutage:     cfg.MaxOutage,
		OutageTimeout: cfg.OutageTimeout,
		DamageRate:    damageRate(cfg),
	}))
	if cfg.Trace != nil {
		r.fake.Log = func(line string) {
			cfg.Trace(fmt.Sprintf("%v s3 %s", r.sched.Elapsed(), line))
		}
	}

	if (cfg.Workload == Unique || cfg.Workload == ForeignKey) && !cfg.SQL {
		panic(fmt.Sprintf("sim: the %v workload runs only through SQL", cfg.Workload))
	}
	switch {
	case cfg.SQL:
		r.seedSQL()
	case cfg.Workload == Bank:
		r.seedBank()
		r.sched.Run()
	}
	r.activeClients = cfg.Clients
	for i := range cfg.Clients {
		c := &client{run: r, id: i, rng: rand.New(rand.NewPCG(cfg.Seed, uint64(i)+1)), freed: map[string]bool{}}
		c.diskRng = rand.New(rand.NewPCG(cfg.Seed, 0xd15c+uint64(i)))
		c.diskBytes = 1 << 40
		if cfg.DiskFaults {
			c.diskBytes = 4096 * (2 + c.diskRng.Int64N(15))
		}
		r.clients = append(r.clients, c)
		r.goTask(fmt.Sprint("client", i), c.loop)
	}
	for i := range cfg.Maintainers {
		m := &maintainer{run: r, id: i, rng: rand.New(rand.NewPCG(cfg.Seed, 0xaaaa+uint64(i)))}
		r.goTask(fmt.Sprint("maintainer", i), m.loop)
	}
	r.sched.Run()
	r.stats.Elapsed = r.sched.Elapsed()
	r.stats.Outages = r.fake.Outages()

	res = Result{Config: cfg, History: r.txs, Audits: r.audits, Stats: r.stats, Reached: r.reached.Counts()}
	var final map[string][]int64
	var balances map[string]int64
	var err error
	if cfg.SQL {
		final, balances, res.Users, err = r.readFinalSQL()
		r.checkIndexes()
		r.checkConstraints()
	} else {
		final, balances, err = r.readFinal()
	}
	if err != nil {
		r.anoms = append(r.anoms, &Anomaly{Checker: CheckerFinalRead, Err: fmt.Errorf("reading final state: %w", err)})
	}
	res.Final = final
	res.Balances = balances
	switch cfg.Workload {
	case ListAppend:
		if err == nil {
			if err := history.CheckDurability(r.txs, final); err != nil {
				r.anoms = append(r.anoms, &Anomaly{Checker: CheckerDurability, Err: err})
			}
		}
		// The list-append checker works from the reads alone when the final state is unavailable.
		if err := history.CheckListAppend(r.txs, final); err != nil {
			r.anoms = append(r.anoms, &Anomaly{Checker: CheckerListAppend, Err: err})
		}
	case Bank:
		if err == nil {
			r.audits = append(r.audits, history.BankAudit{TxID: -1, Balances: balances})
		}
		res.Audits = r.audits
		if err := history.CheckBank(r.audits, r.total); err != nil {
			r.anoms = append(r.anoms, &Anomaly{Checker: CheckerBank, Err: err})
		}
	}
	r.checkStorage()
	res.Anomalies = r.anoms
	if cfg.Mutations == (engine.Mutations{}) && cfg.SQLMutations == (exec.Mutations{}) && cfg.DiskMutations == (cache.DiskMutations{}) {
		assert.Always(len(r.anoms) == 0, "the simulated history has no anomalies", map[string]any{"seed": cfg.Seed})
	}
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
	fmt.Fprintf(&b, "final: %v\nbalances: %v\nusers: %v\nobjects: %v\nstats: %+v\n", res.Final, res.Balances, res.Users, r.fake.Keys(), res.Stats)
	return b.String()
}

func (r *run) trace(format string, args ...any) {
	if r.cfg.Trace != nil {
		r.cfg.Trace(fmt.Sprintf("%v %s", r.sched.Elapsed(), fmt.Sprintf(format, args...)))
	}
}

func (r *run) anomaly(checker string, format string, args ...any) {
	err := &Anomaly{Checker: checker, Err: fmt.Errorf(format, args...)}
	r.trace("ANOMALY %v", err)
	r.anoms = append(r.anoms, err)
}

// engineOptions for a process. Clients vary in caching: every third one has no cache at all,
// so that its reads go to the store and exercise the garbage collection grace period, and every
// third one has only a disk cache.
func (r *run) engineOptions(id int, rng *rand.Rand, disk cache.Cache) engine.Options {
	grace := r.cfg.GracePeriod
	opts := engine.Options{
		Client:        leaseWatch{Client: r.fake, run: r},
		Prefix:        "db",
		DiskCache:     disk,
		GCGracePeriod: &grace,
		Now:           r.sched.Now,
		Sleep:         r.sched.Sleep,
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
	return opts
}

// open an engine for a process.
func (r *run) open(ctx context.Context, id int, rng *rand.Rand, disk cache.Cache) (*engine.DB, error) {
	return engine.Open(ctx, r.engineOptions(id, rng, disk))
}

// maintenanceOptions for a client in the configured mode, or false in [MaintenanceOff].
func (r *run) maintenanceOptions(id int) (engine.MaintenanceOptions, bool) {
	opts := engine.MaintenanceOptions{Holder: fmt.Sprint("client", id), Tiers: r.cfg.Tiers, LeaseDuration: 400 * time.Millisecond}
	switch r.cfg.Maintenance {
	case MaintenanceOff:
		return opts, false
	case MaintenanceNoLease:
		opts.DisableLease = true
	case MaintenanceOverlap:
		opts.LeaseDuration = time.Millisecond
		opts.IgnoreLeaseExpiry = true
	}
	return opts, true
}

// seedBank writes the initial balances with a fault-free client before the clients start.
func (r *run) seedBank() {
	r.total = int64(r.cfg.Keys) * initialBalance
	r.goTask("seed", func(ctx context.Context) {
		db, err := engine.Open(ctx, engine.Options{Client: r.fake.WithoutFaults(), Prefix: "db", Now: r.sched.Now, Sleep: r.sched.Sleep, Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0x5eed))})
		if err != nil {
			panic(err)
		}
		tx, err := db.Begin(ctx)
		if err != nil {
			panic(err)
		}
		for i := range r.cfg.Keys {
			_ = tx.Put([]byte(keyName(i)), []byte(strconv.Itoa(initialBalance)))
		}
		if err := tx.Commit(ctx); err != nil {
			panic(err)
		}
	})
}

// readFinal reads every key with a fresh, fault-free, cache-free process.
func (r *run) readFinal() (map[string][]int64, map[string]int64, error) {
	var lists map[string][]int64
	var balances map[string]int64
	var err error
	r.goTask("final", func(ctx context.Context) {
		var db *engine.DB
		db, err = engine.Open(ctx, engine.Options{Client: r.fake.WithoutFaults(), Prefix: "db", Now: r.sched.Now, Sleep: r.sched.Sleep, MemoryCacheBytes: new(int64), Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0xf1a1))})
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
				// The overflow of a list follows its packed part in key order.
				list, overflow := strings.CutSuffix(key, overflowSuffix)
				var values []int64
				if values, err = decodeList(it.Value()); err != nil {
					return
				}
				lists[list] = append(lists[list], values...)
				if !overflow && lists[list] == nil {
					lists[list] = []int64{}
				}
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

// checkStorage runs GC in a fresh process without faults, with the run's mutations, five times a
// grace period apart, which is enough for every piece of garbage to be stamped, to expire, and to
// be deleted with its garbage batch, also the orphans that the first pass cannot stamp yet. Then
// every segment object must be live, and no garbage and no garbage batch may remain, apart from
// the objects of writes against the final manifest, which a later commit makes orphans.
func (r *run) checkStorage() {
	r.goTask("storage", func(ctx context.Context) {
		grace := r.cfg.GracePeriod
		db, err := engine.Open(ctx, engine.Options{Client: r.fake.WithoutFaults(), Prefix: "db", Now: r.sched.Now, Sleep: r.sched.Sleep, MemoryCacheBytes: new(int64), GCGracePeriod: &grace, Mutations: r.cfg.Mutations, Rand: rand.New(rand.NewPCG(r.cfg.Seed, 0x6c))})
		if err != nil {
			r.anomaly(CheckerStorage, "opening: %v", err)
			return
		}
		for i := range 5 {
			if _, err := db.GC(ctx); err != nil {
				r.anomaly(CheckerStorage, "GC pass %d after the run: %v", i+1, err)
				return
			}
			r.sched.Sleep(ctx, grace)
		}
		stats := db.Stats()
		if stats.Garbage > 0 {
			r.anomaly(CheckerStorage, "%d garbage objects remain after their grace period", stats.Garbage)
		}
		var segments int
		var leaked []string
		for _, key := range r.fake.Keys() {
			name, ok := strings.CutPrefix(key, "db/")
			if !ok || name == "manifest" || name == "maintenance-lease" {
				continue
			}
			kind, rest, _ := strings.Cut(name, "/")
			hexSeq, _, _ := strings.Cut(rest, "-")
			if seq, err := strconv.ParseUint(hexSeq, 16, 64); err == nil && seq >= stats.Seq {
				continue
			}
			if kind == "seg" {
				segments++
				continue
			}
			leaked = append(leaked, key)
		}
		if segments > stats.Segments {
			r.anomaly(CheckerStorage, "%d segment objects for %d live segments", segments, stats.Segments)
		}
		if len(leaked) > 0 {
			r.anomaly(CheckerStorage, "objects that GC never deletes: %v", leaked)
		}
	})
	r.sched.Run()
}

// overflowSuffix of the key that holds the values appended to a list since it was last packed
// in the list-append workload through the engine.
const overflowSuffix = "+"

func keyName(i int) string {
	return fmt.Sprintf("k%02d", i)
}

// decodeList, or fail on bytes that no write of the workload wrote.
func decodeList(b []byte) ([]int64, error) {
	if len(b) == 0 {
		return []int64{}, nil
	}
	var list []int64
	for _, s := range strings.Split(string(b), ",") {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("corrupt list %q", b)
		}
		list = append(list, v)
	}
	return list, nil
}

func encodeList(list []int64) []byte {
	parts := make([]string, len(list))
	for i, v := range list {
		parts[i] = strconv.FormatInt(v, 10)
	}
	return []byte(strings.Join(parts, ","))
}

// client is one simulated process running transactions. It restarts after a crash, keeping the
// files of its disk cache but losing its memory, and a crash damages some of those files.
type client struct {
	run *run
	id  int
	rng *rand.Rand
	// files of the disk cache, its bound in bytes, and the disk cache of the current process,
	// whose stats when last looked at are diskStats. diskRng draws the damage of a crash, and
	// the bound.
	files     fsfake.FS
	diskBytes int64
	disk      *cache.Disk
	diskStats cache.DiskStats
	diskRng   *rand.Rand
	proc      *s3fake.Process
	db        *engine.DB
	// sql is the database/sql handle when the run goes through SQL.
	sql *sql.DB
	// engine is the engine of the current process, through SQL or not, and refused the blocks and
	// manifests that its caches returned and that failed their check, when last looked at.
	engine  *engine.DB
	refused int64
	// damagedAt is the number of the client's reads that the store had damaged when its current
	// operation began.
	damagedAt int
	// maint is the client's maintainer in the maintenance modes.
	maint *engine.Maintainer
	next  int64
	// crashedInCommit is true from a crash during a commit's writes until the restarted client
	// commits again.
	crashedInCommit bool
	// freed are the unique values that the client's committed writes freed.
	freed map[string]bool
}

func (c *client) loop(ctx context.Context) {
	defer func() {
		c.closeSQL()
		c.run.activeClients--
	}()
	for i := 0; i < c.run.cfg.TxPerClient; i++ {
		if c.db == nil && c.sql == nil {
			c.proc = &s3fake.Process{Name: fmt.Sprint("client", c.id)}
			pctx := s3fake.WithProcess(ctx, c.proc)
			c.openDisk(ctx)
			var err error
			var db *engine.DB
			if c.run.cfg.SQL {
				c.sql, db, err = c.run.openSQL(pctx, c.run.engineOptions(c.id, c.rng, c.disk))
			} else {
				c.db, err = c.run.open(pctx, c.id, c.rng, c.disk)
				db = c.db
			}
			if err != nil {
				c.run.trace("client %d: open: %v", c.id, err)
				c.run.repanic()
				c.afterCrash()
				c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(10*time.Millisecond))))
				i--
				continue
			}
			c.engine, c.refused = db, 0
			c.checkCache()
			if opts, ok := c.run.maintenanceOptions(c.id); ok {
				c.maint = db.NewMaintainer(opts)
			}
		}
		pctx := s3fake.WithProcess(ctx, c.proc)
		c.damagedAt = c.run.fake.Damaged(c.proc.Name)
		switch {
		case c.run.cfg.Workload == Unique:
			c.uniqueSQL(pctx)
		case c.run.cfg.Workload == ForeignKey:
			c.foreignKeySQL(pctx)
		case c.run.cfg.SQL && c.run.cfg.Workload == ListAppend:
			c.listAppendSQL(pctx)
		case c.run.cfg.SQL:
			c.bankSQL(pctx)
		case c.run.cfg.Workload == ListAppend:
			c.listAppendTx(pctx)
		default:
			c.bankTx(pctx)
		}
		c.run.repanic()
		c.checkDisk(ctx)
		c.checkCache()
		if names := c.files.Names(); c.run.cfg.DiskFaults && len(names) > 0 && c.diskRng.IntN(8) == 0 {
			c.damageFile(names[c.diskRng.IntN(len(names))])
		}
		c.afterCrash()
		if c.maint != nil && c.rng.IntN(3) == 0 {
			c.step(pctx)
			c.run.repanic()
			c.checkDisk(ctx)
			c.checkCache()
			c.afterCrash()
		}
		c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(5*time.Millisecond))))
	}
}

// step runs one maintenance step of the client and records what it did.
func (c *client) step(ctx context.Context) {
	before := c.maint.DB().Stats()
	res, err := c.maint.Step(ctx)
	after := c.maint.DB().Stats()
	s := &c.run.stats
	s.MaintenanceSteps++
	if res.Attempted {
		s.MaintenanceAttempts++
	}
	if res.Ran {
		s.MaintenanceRuns++
	}
	// A step compacts again while a merge is due, so it can land several compactions.
	s.Compactions += int(after.Compactions - before.Compactions)
	if res.Ran && !res.Compacted {
		s.GCs++
	}
	s.Deleted += res.Deleted
	s.MergesStarted += int(after.CompactionsStarted - before.CompactionsStarted)
	s.CompactionRebases += int(after.CompactionRebases - before.CompactionRebases)
	s.LeaseAcquisitions += int(after.LeaseAcquisitions - before.LeaseAcquisitions)
	s.LeaseLosses += int(after.LeaseLosses - before.LeaseLosses)
	if res.Attempted || err != nil {
		c.run.trace("client %d: maintenance: %+v, %v", c.id, res, err)
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
		c.engine = nil
		c.maint = nil
		c.closeSQL()
		c.proc = nil
		c.disk = nil
		if c.run.cfg.DiskFaults {
			c.damageDisk()
		}
	}
}

// openDisk opens the client's disk cache on its files, as a process does when it starts.
func (c *client) openDisk(ctx context.Context) {
	disk, err := cache.NewDisk(&c.files, cache.DiskOptions{MaxBytes: c.diskBytes, Mutations: c.run.cfg.DiskMutations})
	if err != nil {
		panic(fmt.Sprintf("sim: opening a disk cache: %v", err))
	}
	c.disk = disk
	c.diskStats = cache.DiskStats{}
	c.checkDisk(ctx)
}

// checkDisk checks that the files of the client's disk cache are within its bound and all
// counted, and records the states its disk cache reached.
func (c *client) checkDisk(ctx context.Context) {
	if c.disk == nil {
		return
	}
	// No Put runs between the operations of a client, so not even the one entry that a Put
	// writes beyond the bound may be there.
	total, _ := c.files.Bytes()
	s := c.disk.Stats()
	switch {
	case total > c.diskBytes:
		c.run.anomaly(CheckerDiskCache, "client %d: the files of the disk cache take %d bytes, more than the bound %d", c.id, total, c.diskBytes)
	case total > s.Bytes:
		c.run.anomaly(CheckerDiskCache, "client %d: the files of the disk cache take %d bytes, and it counts %d", c.id, total, s.Bytes)
	}
	if s.Evictions > c.diskStats.Evictions {
		assert.Sometimes(true, "a disk cache evicts to stay within its bound", nil)
		reach.Hit(ctx, reach.DiskCacheEviction)
	}
	if s.Refused > c.diskStats.Refused {
		assert.Sometimes(true, "a disk cache refuses a damaged file", nil)
		reach.Hit(ctx, reach.DiskCacheRefused)
	}
	c.diskStats = s
}

// checkCache checks that the caches of the client's engine returned no block or manifest that
// failed its check since it was last looked at.
func (c *client) checkCache() {
	if c.engine == nil {
		return
	}
	s := c.engine.Stats()
	if refused := s.MemoryCacheRefused + s.DiskCacheRefused; refused > c.refused {
		c.run.anomaly(CheckerCache, "client %d: its caches returned %d blocks or manifests that failed their check, %d from memory and %d from disk", c.id, refused-c.refused, s.MemoryCacheRefused, s.DiskCacheRefused)
		c.refused = refused
	}
}

// checkError of an operation of the client: an [engine.ErrCorrupt] needs a read of the operation
// that the store damaged, and through the engine, every error must be of a kind that the engine
// or the store gives.
func (c *client) checkError(err error) {
	if err == nil {
		return
	}
	if errors.Is(err, engine.ErrCorrupt) {
		if c.run.fake.Damaged(c.proc.Name) == c.damagedAt {
			c.run.anomaly(CheckerDamage, "client %d: an operation fails with %v, but the store damaged none of its reads", c.id, err)
		}
		return
	}
	if c.run.cfg.SQL {
		return
	}
	for _, known := range []error{engine.ErrConflict, engine.ErrCommitUnknown, engine.ErrSnapshotExpired, s3fake.ErrInjected, s3fake.ErrUnavailable, s3fake.ErrCrashed} {
		if errors.Is(err, known) {
			return
		}
	}
	c.run.anomaly(CheckerDamage, "client %d: an operation fails with an error of no kind that the engine or the store gives: %v", c.id, err)
}

// damageDisk as a crash can: a third of the files of the disk cache are damaged, and the write in
// progress may leave its temporary file.
func (c *client) damageDisk() {
	for _, name := range c.files.Names() {
		if c.diskRng.IntN(3) == 0 {
			c.damageFile(name)
		}
	}
	if c.diskRng.IntN(2) == 0 {
		c.files.SetFile(cache.TempPrefix+"crashed", make([]byte, c.diskRng.IntN(5000)))
	}
}

// damageFile of the disk cache, as a disk or another program can at any time: the file may have
// a bit flipped, hold the content of another entry's file, lose its end, or be gone. No file
// grows, so the disk cache still counts every byte of its files.
func (c *client) damageFile(name string) {
	b, _ := c.files.File(name)
	switch n := c.diskRng.IntN(10); {
	case n < 5:
		if len(b) > 0 {
			b[c.diskRng.IntN(len(b))] ^= 1 << c.diskRng.IntN(8)
			c.files.SetFile(name, b)
		}
	case n < 8:
		names := c.files.Names()
		other, _ := c.files.File(names[c.diskRng.IntN(len(names))])
		if cache.FileBytes(int64(len(other))) <= cache.FileBytes(int64(len(b))) {
			c.files.SetFile(name, other)
		}
	case n < 9:
		c.files.SetFile(name, b[:c.diskRng.IntN(len(b)+1)])
	default:
		_ = c.files.Remove(name)
	}
}

// closeSQL closes the client's database/sql handle, if it has one. Closing it releases its
// goroutines, which would otherwise keep the whole run alive after it ends. It reaches neither the
// store nor the clock.
func (c *client) closeSQL() {
	if c.sql != nil {
		_ = c.sql.Close()
		c.sql = nil
	}
}

// pause the client in the middle of a transaction now and then, like a stalled process,
// so that snapshots outlive compactions, and rarely for up to the grace period, so that a reader
// whose snapshot is almost a grace period old reads garbage that GC must still keep.
func (c *client) pause(ctx context.Context) {
	switch c.rng.IntN(25) {
	case 0:
		c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(c.run.cfg.GracePeriod))))
	case 1, 2, 3, 4:
		c.run.sched.Sleep(ctx, time.Duration(c.rng.Int64N(int64(200*time.Millisecond))))
	}
}

func (c *client) value() int64 {
	c.next++
	return int64(c.id)<<32 | c.next
}

// readError handles an error from a read: it classifies snapshot expiry as an anomaly when the
// transaction is younger than the grace period.
func (c *client) readError(ctx context.Context, tx *engine.Tx, err error) {
	c.run.stats.FailedReads++
	c.checkError(err)
	if errors.Is(err, engine.ErrSnapshotExpired) {
		age := c.run.sched.Now().Sub(tx.StartedAt())
		assert.Sometimes(age >= c.run.cfg.GracePeriod, "a reader older than the grace period is told so", nil)
		if age >= c.run.cfg.GracePeriod {
			reach.Hit(ctx, reach.SnapshotExpiredRefused)
		}
		if age < c.run.cfg.GracePeriod {
			c.run.anomaly(CheckerGracePeriod, "client %d: snapshot expired after %v, grace period is %v: %v", c.id, age, c.run.cfg.GracePeriod, err)
		}
	}
}

func (c *client) record(tx history.Tx) {
	tx.ID = len(c.run.txs) + 1
	c.run.txs = append(c.run.txs, tx)
	c.run.trace("%v", tx)
}

// outcome of a commit. Only [engine.ErrCommitUnknown] is ambiguous; every other error means
// the transaction was not applied.
func (c *client) outcome(ctx context.Context, err error) history.Outcome {
	c.checkError(err)
	if errors.Is(err, s3fake.ErrUnavailable) {
		c.run.stats.Unavailable++
	}
	crashed, step := c.proc.Crashed()
	if crashed && strings.Contains(step, "PUT") {
		c.crashedInCommit = true
	}
	switch {
	case err == nil:
		c.run.stats.Commits++
		if c.crashedInCommit {
			assert.Sometimes(true, "a client that crashed during a commit recovers and commits", nil)
			reach.Hit(ctx, reach.CrashMidCommitRecovered)
			c.crashedInCommit = false
		}
		return history.OK
	case errors.Is(err, engine.ErrCommitUnknown):
		c.run.stats.Unknown++
		if !crashed {
			c.run.stats.LiveUnknown++
			reach.Hit(ctx, reach.LiveUnknown)
		}
		assert.Sometimes(!crashed, "a commit outcome is unknown in a process that keeps running", nil)
		return history.Unknown
	case errors.Is(err, engine.ErrConflict):
		c.run.stats.Conflicts++
		return history.Failed
	default:
		return history.Failed
	}
}

func (c *client) listAppendTx(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
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
		c.checkError(err)
		return
	}
	for _, p := range plans {
		c.pause(ctx)
		// A list is kept in two keys: its packed part under the key, and the values appended
		// since it was last packed under the key with overflowSuffix.
		v, _, err := tx.Get(ctx, []byte(p.key))
		var w []byte
		if err == nil {
			w, _, err = tx.Get(ctx, []byte(p.key+overflowSuffix))
		}
		if err != nil {
			c.readError(ctx, tx, err)
			c.run.trace("client %d: read: %v", c.id, err)
			// The reads that completed are real observations; the transaction was never committed.
			if len(h.Ops) > 0 {
				h.Complete = c.run.stamp()
				h.Outcome = history.Failed
				h.Err = err.Error()
				c.record(h)
			}
			return
		}
		overflow, oerr := decodeList(w)
		list, lerr := decodeList(v)
		if err := cmp.Or(lerr, oerr); err != nil {
			c.run.anomaly(CheckerWorkload, "client %d: %v", c.id, err)
			return
		}
		list = append(list, overflow...)
		h.Ops = append(h.Ops, history.Op{Kind: history.Read, Key: p.key, List: list})
		if p.append {
			value := c.value()
			// Half of the appends go to the overflow, and the others pack the whole list and delete
			// the overflow, so that compaction meets tombstones that hide older overflows.
			if c.rng.IntN(2) == 0 {
				if err := tx.Put([]byte(p.key+overflowSuffix), encodeList(append(overflow, value))); err != nil {
					panic(err)
				}
			} else {
				if err := tx.Put([]byte(p.key), encodeList(append(list, value))); err != nil {
					panic(err)
				}
				if w != nil {
					if err := tx.Delete([]byte(p.key + overflowSuffix)); err != nil {
						panic(err)
					}
				}
			}
			h.Ops = append(h.Ops, history.Op{Kind: history.Append, Key: p.key, Value: value})
		}
	}
	err = tx.Commit(ctx)
	h.Complete = c.run.stamp()
	if readOnly && err != nil {
		// A read-only commit does nothing, so an error means the reads never completed, and is
		// not an unknown outcome.
		h.Outcome = history.Failed
	} else {
		h.Outcome = c.outcome(ctx, err)
	}
	if err != nil {
		h.Err = err.Error()
	}
	c.record(h)
}

func (c *client) bankTx(ctx context.Context) {
	h := history.Tx{Client: c.id, Invoke: c.run.stamp()}
	if c.rng.IntN(3) == 0 {
		tx, err := c.db.BeginReadOnly(ctx)
		if err != nil {
			c.checkError(err)
			return
		}
		balances := map[string]int64{}
		for i := range c.run.cfg.Keys {
			v, ok, err := tx.Get(ctx, []byte(keyName(i)))
			if err != nil {
				c.readError(ctx, tx, err)
				return
			}
			if !ok {
				c.run.anomaly(CheckerWorkload, "client %d: account %s is missing", c.id, keyName(i))
				return
			}
			balances[keyName(i)], err = strconv.ParseInt(string(v), 10, 64)
			if err != nil {
				c.run.anomaly(CheckerWorkload, "client %d: corrupt balance %q", c.id, v)
				return
			}
		}
		h.Complete = c.run.stamp()
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
		c.checkError(err)
		return
	}
	read := func(key string) (int64, bool) {
		c.pause(ctx)
		v, ok, err := tx.Get(ctx, []byte(key))
		if err != nil {
			c.readError(ctx, tx, err)
			return 0, false
		}
		if !ok {
			c.run.anomaly(CheckerWorkload, "client %d: account %s is missing", c.id, key)
			return 0, false
		}
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			c.run.anomaly(CheckerWorkload, "client %d: corrupt balance %q", c.id, v)
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
	h.Complete = c.run.stamp()
	h.Outcome = c.outcome(ctx, err)
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
			// Compaction loses to concurrent commits, so retry a few times. Most compactions merge
			// the segments that the tiers make due, and some merge the whole database.
			full := m.rng.IntN(2) == 0
			var err error
			for range 5 {
				rebases := m.db.Stats().CompactionRebases
				if full {
					err = m.db.Compact(pctx)
				} else {
					_, err = m.db.CompactTiers(pctx, m.run.cfg.Tiers)
				}
				m.run.stats.CompactionAttempts++
				m.run.stats.CompactionRebases += int(m.db.Stats().CompactionRebases - rebases)
				if !errors.Is(err, engine.ErrConflict) {
					break
				}
				m.run.stats.CompactionConflicts++
			}
			if err == nil {
				m.run.stats.Compactions++
			}
			m.run.trace("maintainer %d: compact: %v", m.id, err)
			if crashed, _ := m.proc.Crashed(); crashed {
				assert.Sometimes(true, "a process crashes during a compaction", nil)
				reach.Hit(ctx, reach.CrashMidCompaction)
				m.afterCrash()
				continue
			}
		}
		n, err := m.db.GC(pctx)
		m.run.repanic()
		if crashed, _ := m.proc.Crashed(); crashed {
			assert.Sometimes(true, "a process crashes during a GC pass", nil)
			reach.Hit(ctx, reach.CrashMidGC)
		}
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

// leaseWatch is the store as the clients see it. Before every request it looks at whether two
// clients' maintainers believe they hold the lease at the same time. Who holds the lease only
// changes while a request of its holder is in flight, so looking before every request sees every
// overlap.
type leaseWatch struct {
	s3.Client
	run *run
}

func (w leaseWatch) Get(ctx context.Context, key string, opts s3.GetOptions) (s3.Object, error) {
	w.run.watchLeases(ctx)
	return w.Client.Get(ctx, key, opts)
}

func (w leaseWatch) Put(ctx context.Context, key string, body []byte, opts s3.PutOptions) (string, error) {
	w.run.watchLeases(ctx)
	return w.Client.Put(ctx, key, body, opts)
}

func (w leaseWatch) Delete(ctx context.Context, key string) error {
	w.run.watchLeases(ctx)
	return w.Client.Delete(ctx, key)
}

func (w leaseWatch) List(ctx context.Context, prefix string) ([]s3.ObjectInfo, error) {
	w.run.watchLeases(ctx)
	return w.Client.List(ctx, prefix)
}

// watchLeases counts the start of every stretch in which two or more maintainers believe they
// hold the lease.
func (r *run) watchLeases(ctx context.Context) {
	holders := 0
	for _, c := range r.clients {
		if c.maint != nil && c.maint.Holds() {
			holders++
		}
	}
	overlapping := holders >= 2
	if overlapping && !r.overlapping {
		assert.Sometimes(true, "two maintainers believe they hold the lease at once", nil)
		reach.Hit(ctx, reach.LeaseOverlap)
	}
	r.overlapping = overlapping
}
