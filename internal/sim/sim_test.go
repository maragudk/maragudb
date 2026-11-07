package sim_test

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

// slots bound the simulations that run at once, across all tests, to the number of CPUs. A run
// shares nothing with another run, and its scheduler runs one task at a time, so runs in parallel
// give the same results as runs one after another.
var slots = make(chan struct{}, runtime.GOMAXPROCS(0))

// runSim runs a simulation when a slot is free. A panic in the run names the configuration, since
// the run may be in a goroutine of its own, which the panic does not otherwise tie to a test.
func runSim(cfg sim.Config) sim.Result {
	slots <- struct{}{}
	defer func() { <-slots }()
	defer func() {
		if p := recover(); p != nil {
			cfg.Trace = nil
			panic(fmt.Sprintf("%v, seed %d, configuration %+v: %v\n%s", cfg.Name(), cfg.Seed, cfg, p, debug.Stack()))
		}
	}()
	return sim.Run(cfg)
}

// runAll the configurations in parallel, and return their results in the same order.
func runAll(cfgs []sim.Config) []sim.Result {
	results := make([]sim.Result, len(cfgs))
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		wg.Go(func() {
			results[i] = runSim(cfg)
		})
	}
	wg.Wait()
	return results
}

// corpusSummary sums the runs of the corpus. It keeps the whole result only of a failed run, so
// that the histories of the corpus are not held while the other tests run.
type corpusSummary struct {
	runs              int
	elapsed           time.Duration
	totals, sqlTotals sim.Stats
	batch             simBatch
	reached           map[string]int
	failed            []sim.Result
}

// corpusResults of the whole corpus, run once for the tests that check it.
var corpusResults = sync.OnceValue(func() corpusSummary {
	start := time.Now()
	cfgs := corpus()
	s := corpusSummary{runs: len(cfgs), batch: simBatch{name: "corpus"}, reached: map[string]int{}}
	failed := make([]*sim.Result, len(cfgs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, cfg := range cfgs {
		wg.Go(func() {
			res := runSim(cfg)
			mu.Lock()
			defer mu.Unlock()
			if res.Failed() {
				failed[i] = &res
			}
			s.totals.Add(res.Stats)
			if cfg.SQL {
				s.sqlTotals.Add(res.Stats)
			}
			s.batch.add(res)
			for state, n := range res.Reached {
				s.reached[string(state)] += n
			}
		})
	}
	wg.Wait()
	for _, res := range failed {
		if res != nil {
			s.failed = append(s.failed, *res)
		}
	}
	s.elapsed = time.Since(start)
	return s
})

// corpus of fixed seeds and configurations run in `make test`.
func corpus() []sim.Config {
	var cfgs []sim.Config
	// Some configurations of every group get damaged reads from the store.
	for seed := uint64(1); seed <= 40; seed++ {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = w
			cfg.StoreDamage = seed%8 == 0
			cfgs = append(cfgs, cfg)
		}
	}
	// Heavier faults and more contention.
	for seed := uint64(100); seed < 120; seed++ {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = w
			cfg.Clients = 10
			cfg.ErrorRate = 0.15
			cfg.LostAckRate = 0.15
			cfg.CrashRate = 0.03
			cfg.Maintainers = 2
			cfg.GracePeriod = 500 * time.Millisecond
			cfgs = append(cfgs, cfg)
		}
	}
	// Long transactions relative to the grace period, so some readers legitimately expire.
	for seed := uint64(200); seed < 210; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.GracePeriod = 100 * time.Millisecond
		cfg.MaxLatency = 40 * time.Millisecond
		cfg.Maintainers = 2
		cfgs = append(cfgs, cfg)
	}
	// Background maintenance in the clients, coordinated by the lease, without it, and with
	// leases forced to overlap, through the engine and through SQL.
	for seed := uint64(400); seed < 424; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Workload(seed % 2)
		cfg.SQL = seed%4 >= 2
		cfg.Maintenance = sim.MaintenanceMode(1 + (seed/4)%3)
		cfg.Maintainers = int(seed / 12 % 2)
		cfgs = append(cfgs, cfg)
	}
	// Concurrent writers of a unique column through SQL, with background maintenance in every
	// mode, and some with heavier faults and more contention.
	for seed := uint64(500); seed < 530; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Unique
		cfg.SQL = true
		cfg.Maintenance = sim.MaintenanceMode(seed % 4)
		cfg.StoreDamage = seed%6 == 1
		if seed%3 == 0 {
			cfg.Clients = 10
			cfg.ErrorRate = 0.15
			cfg.LostAckRate = 0.15
			cfg.CrashRate = 0.03
			cfg.Maintainers = 2
			cfg.GracePeriod = 500 * time.Millisecond
		}
		cfgs = append(cfgs, cfg)
	}
	// Concurrent writers of tables with foreign keys and checks through SQL, with background
	// maintenance in every mode, and some with heavier faults and more contention.
	for seed := uint64(600); seed < 630; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.ForeignKey
		cfg.SQL = true
		cfg.Maintenance = sim.MaintenanceMode(seed % 4)
		cfg.StoreDamage = seed%6 == 1
		if seed%3 == 0 {
			cfg.Clients = 10
			cfg.ErrorRate = 0.15
			cfg.LostAckRate = 0.15
			cfg.CrashRate = 0.03
			cfg.Maintainers = 2
			cfg.GracePeriod = 500 * time.Millisecond
		}
		cfgs = append(cfgs, cfg)
	}
	// Disk caches small enough to evict, whose files crashes and the disk damage, in every
	// workload, through the engine and through SQL, some with heavier faults.
	for seed := uint64(700); seed < 724; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Workload(seed % 4)
		cfg.SQL = cfg.Workload >= sim.Unique || seed%8 >= 4
		cfg.DiskFaults = true
		cfg.StoreDamage = seed%3 == 1
		if seed%3 == 0 {
			cfg.Clients = 10
			cfg.CrashRate = 0.03
			cfg.Maintainers = 2
		}
		cfgs = append(cfgs, cfg)
	}
	// Blocks of 128 and 256 bytes, so that segments have trees of several levels and key filters
	// in blocks of their own, and with 256 bytes also in the manifest, in every workload, through
	// the engine and through SQL, some with damaged reads from the store, damaged disk caches, or
	// heavier faults.
	for seed := uint64(800); seed < 816; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Workload(seed % 4)
		cfg.SQL = cfg.Workload >= sim.Unique || seed%8 >= 4
		cfg.BlockSize = 128 << (seed / 8 % 2)
		cfg.StoreDamage = seed%3 == 1
		cfg.DiskFaults = seed%4 == 3
		if seed%3 == 0 {
			cfg.Clients = 10
			cfg.CrashRate = 0.03
			cfg.Maintainers = 2
		}
		cfgs = append(cfgs, cfg)
	}
	// Maintainers that stall between the segment write and the manifest write of a compaction,
	// half of them with one maintainer that compacts and one that only collects garbage, and
	// clients partitioned right after some of their manifest writes land, in every workload,
	// through the engine and through SQL.
	for seed := uint64(900); seed < 912; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Workload(seed % 4)
		cfg.SQL = cfg.Workload >= sim.Unique || seed%8 >= 4
		cfg.Maintainers = 2
		cfg.CompactionStall = time.Duration(500+400*(seed%2)) * time.Millisecond
		cfg.OneCompactor = seed%2 == 1
		if seed%3 == 0 {
			cfg.CommitPartitions = 0.4
		}
		cfgs = append(cfgs, cfg)
	}
	// Maintainers that never merge the whole database, and reads from the store of which one in
	// ten is damaged, with blocks of 64 bytes, in every workload, through the engine and through
	// SQL.
	for seed := uint64(920); seed < 928; seed++ {
		cfg := sim.DefaultConfig(seed)
		cfg.Workload = sim.Workload(seed % 4)
		cfg.SQL = cfg.Workload >= sim.Unique || seed%8 >= 4
		cfg.Maintainers = 2
		cfg.TieredCompactions = true
		cfg.StoreDamage = true
		cfg.DamageRate = 0.1
		cfg.BlockSize = 64
		cfgs = append(cfgs, cfg)
	}
	// The same through the SQL layer and the database/sql driver.
	for seed := uint64(300); seed < 330; seed++ {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = w
			cfg.SQL = true
			cfg.StoreDamage = seed%6 == 1
			if seed%3 == 0 {
				cfg.Clients = 10
				cfg.ErrorRate = 0.15
				cfg.LostAckRate = 0.15
				cfg.CrashRate = 0.03
				cfg.Maintainers = 2
				cfg.GracePeriod = 500 * time.Millisecond
			}
			cfgs = append(cfgs, cfg)
		}
	}
	return cfgs
}

func failReport(t *testing.T, res sim.Result) {
	t.Helper()
	cfg := res.Config
	cfg.Trace = nil
	api := "engine"
	if cfg.SQL {
		api = "sql"
	}
	replay := fmt.Sprintf("make replay SEED=%d WORKLOAD=%v API=%s", cfg.Seed, cfg.Workload, api)
	if inCorpus(cfg) {
		replay += " CORPUS=1"
	} else {
		replay += fmt.Sprintf(" MAINTAINERS=%d MAINTENANCE=%s", cfg.Maintainers, maintenanceName(cfg.Maintenance))
		if cfg.DiskFaults {
			replay += " DISK_FAULTS=1"
		}
		if cfg.StoreDamage {
			replay += " STORE_DAMAGE=1"
		}
		if cfg.BlockSize != 0 {
			replay += fmt.Sprint(" BLOCK_SIZE=", cfg.BlockSize)
		}
		if cfg.CompactionStall != 0 {
			replay += fmt.Sprint(" COMPACTION_STALL=", cfg.CompactionStall)
		}
		if cfg.OneCompactor {
			replay += " ONE_COMPACTOR=1"
		}
		if cfg.CommitPartitions != 0 {
			replay += fmt.Sprint(" COMMIT_PARTITIONS=", cfg.CommitPartitions)
		}
	}
	t.Errorf("%s\n\nconfiguration: %+v\n\nreplay with:\n  %s", res.Report(), cfg, replay)
}

// inCorpus reports whether the configuration is one of the corpus, which `make replay CORPUS=1`
// finds by its seed, workload, and API.
func inCorpus(cfg sim.Config) bool {
	for _, c := range corpus() {
		if fmt.Sprintf("%+v", c) == fmt.Sprintf("%+v", cfg) {
			return true
		}
	}
	return false
}

// maintenanceName of a mode for `make replay MAINTENANCE=...`.
func maintenanceName(m sim.MaintenanceMode) string {
	switch m {
	case sim.MaintenanceLease:
		return "lease"
	case sim.MaintenanceNoLease:
		return "none"
	case sim.MaintenanceOverlap:
		return "overlap"
	}
	return "off"
}

func TestSimulator(t *testing.T) {
	t.Parallel()

	t.Run("corpus finds no anomalies", func(t *testing.T) {
		verify.Covers(t, "the fixed corpus, with injected errors, latency, lost acknowledgements, outages, and crashes, finds no anomalies with any checker and exercises every fault", "AC-1.1", "AC-1.2", "AC-1.3", "AC-1.4", "AC-1.5", "AC-1.7", "AC-1.8", "AC-3.1", "AC-3.2", "AC-6.2", "INV-MULTI-WRITER", "INV-STRICT-SERIALIZABLE", "INV-CACHE-LOSS", "INV-S3-ONLY")
		verify.Covers(t, "after every run of the fixed corpus, GC passes a grace period apart leave only the live segments and the objects of writes against the final manifest, and fail on no missing garbage batch", "AC-6.1")
		verify.Covers(t, "the fixed corpus, with the unique workload under the same faults, finds no two rows with the same unique value in any snapshot a reader saw or in the final state, and after every run through SQL every index of the store matches its table, and lookups through the indexes find the rows of the table", "INV-UNIQUE", "INV-INDEX-CONSISTENT")
		verify.Covers(t, "the fixed corpus, with the foreign key workload under the same faults, finds no reference to a missing row and no row that violates a CHECK in any snapshot a reader saw, and after every run through SQL every foreign key and CHECK of the store holds", "INV-FK", "INV-CHECK")
		verify.Covers(t, "the fixed corpus, with disk caches small enough to evict and files that crashes and the disk damage, finds no anomalies, and after every operation of every client the files of its disk cache are within the bound and all counted", "INV-DISK-CACHE-BOUND", "INV-CACHE-LOSS")
		verify.Covers(t, "the fixed corpus, with reads whose bodies the store damages in every workload, through the engine and SQL, finds no anomalies: no wrong data, no cache that returns bytes that fail their check, every failure for damage an ErrCorrupt of an operation whose read the store damaged, and every failure through the engine of a known kind", "INV-DAMAGE-DETECTED")
		verify.Covers(t, "the fixed corpus reaches every declared state at least once, among them crashes during commits, compactions, and GC passes, lost acknowledgements resolved both ways, unique and foreign key violations that only a concurrent commit causes, disk caches that evict and refuse damaged files, damaged reads from the store refused with ErrCorrupt, and point lookups that skip segments by their key filters and read filter blocks with root blocks",
			"AC-1.3", "AC-1.4", "STATE-LOST-ACK-LANDED", "STATE-LOST-ACK-CONFLICT", "STATE-LIVE-UNKNOWN", "STATE-AUTOCOMMIT-RETRY",
			"STATE-COMPACTION-REBASE", "STATE-COMPACTION-PARTIAL-REBASE", "STATE-COMPACTION-KEEPS-TOMBSTONE", "STATE-COMPACTION-UPPER-TIER", "STATE-COMPACTION-WINDOWS", "STATE-GC-REBASE", "STATE-GC-KEEPS-LIVE-GARBAGE", "STATE-GC-BATCH-REWRITTEN", "STATE-GC-CHAIN-DROPPED", "STATE-LEASE-TAKEOVER", "STATE-LEASE-OVERLAP",
			"STATE-SNAPSHOT-EXPIRED-REFUSED", "STATE-BLOCK-READ-RETRY-SUCCEEDED", "STATE-CRASH-MID-COMMIT-RECOVERED",
			"STATE-CRASH-MID-COMPACTION", "STATE-CRASH-MID-GC", "STATE-UNIQUE-AFTER-CONFLICT", "STATE-UNIQUE-VALUE-REUSED",
			"STATE-FK-AFTER-CONFLICT", "STATE-FK-CASCADE-TABLES", "STATE-DISK-CACHE-EVICTION", "STATE-DISK-CACHE-REFUSED", "STATE-DAMAGED-READ-REFUSED",
			"STATE-FILTER-SKIP", "STATE-FILTER-BLOCK-READ")
		t.Parallel()
		c := corpusResults()
		for _, res := range c.failed {
			failReport(t, res)
		}
		totals, reached := c.totals, c.reached
		c.batch.record(t)
		verify.Record(t, verify.Fact{Reached: reached})
		t.Logf("corpus of %d runs in %v: %+v", c.runs, c.elapsed.Round(time.Millisecond), totals)
		for _, d := range reach.Declarations() {
			t.Logf("reached %v %d times", d.State, reached[string(d.State)])
			is.True(t, reached[string(d.State)] > 0, fmt.Sprintf("the corpus reaches %v: %s", d.State, d.Description))
		}
		// The corpus must exercise every interesting path, or passing means nothing.
		is.True(t, totals.Commits > 1000, "commits")
		is.True(t, totals.Conflicts > 100, "conflicts")
		is.True(t, totals.Unknown > 50, "unknown outcomes")
		is.Equal(t, 0, totals.Undecided, "bank runs whose final balances the durability checker could not decide")
		is.Equal(t, 0, totals.UndecidedAudits, "bank runs with an audit whose balances the real-time checker could not decide")
		is.True(t, totals.LiveUnknown > 50, "unknown outcomes in processes that keep running")
		is.True(t, totals.Outages > 200, "outages and partitions")
		is.True(t, totals.Unavailable > 100, "operations failed by an unreachable store")
		is.True(t, totals.Crashes > 50, "crashes")
		is.True(t, totals.Compactions > 50, "compactions")
		is.True(t, totals.Deleted > 50, "deleted objects")
		is.True(t, totals.UniqueViolations > 200, "unique violations")
		is.True(t, totals.ForeignKeyViolations > 200, "foreign key violations")
		is.True(t, totals.CheckViolations > 50, "check violations")
	})

	t.Run("compaction succeeds against steady writers by rebasing on their commits", func(t *testing.T) {
		verify.Covers(t, "at least 90 percent of compactions succeed against fault-free steady writers, so storage stays bounded under load", "AC-6.1")
		t.Parallel()
		var cfgs []sim.Config
		for seed := uint64(1); seed <= 40; seed++ {
			cfg := sim.DefaultConfig(seed)
			cfg.ErrorRate, cfg.LostAckRate, cfg.CrashRate, cfg.OutageRate = 0, 0, 0, 0
			cfg.Clients = 8
			cfg.TxPerClient = 40
			cfgs = append(cfgs, cfg)
		}
		var totals sim.Stats
		for _, res := range runAll(cfgs) {
			if res.Failed() {
				failReport(t, res)
			}
			totals.Add(res.Stats)
		}
		rate := float64(totals.Compactions) / float64(totals.CompactionAttempts)
		t.Logf("fault-free steady writers: %d of %d compactions succeeded (%.0f%%), %d rebased", totals.Compactions, totals.CompactionAttempts, 100*rate, totals.CompactionRebases)
		is.True(t, totals.CompactionAttempts > 100, "compactions attempted")
		is.True(t, rate >= 0.9, fmt.Sprintf("success rate %.2f", rate))
		is.True(t, totals.CompactionRebases > totals.CompactionAttempts/4, "most successes needed a rebase")
	})

	t.Run("every checker passes with the lease working, disabled, and overlapping", func(t *testing.T) {
		verify.Covers(t, "every checker passes with background maintenance under a working lease, without the lease, and with overlapping leases", "INV-MULTI-WRITER")
		t.Parallel()
		modes := []sim.MaintenanceMode{sim.MaintenanceLease, sim.MaintenanceNoLease, sim.MaintenanceOverlap}
		var cfgs []sim.Config
		for _, mode := range modes {
			for seed := uint64(1); seed <= 12; seed++ {
				for _, useSQL := range []bool{false, true} {
					cfg := sim.DefaultConfig(seed)
					cfg.Workload = sim.Workload(seed % 2)
					cfg.SQL = useSQL
					cfg.Maintenance = mode
					cfg.Maintainers = 0
					cfgs = append(cfgs, cfg)
				}
			}
		}
		results := runAll(cfgs)
		for _, mode := range modes {
			var totals sim.Stats
			for _, res := range results {
				if res.Config.Maintenance != mode {
					continue
				}
				if res.Failed() {
					failReport(t, res)
				}
				totals.Add(res.Stats)
			}
			t.Logf("maintenance %v: %+v", mode, totals)
			is.True(t, totals.Compactions > 20, "compactions")
			is.True(t, totals.Deleted > 20, "deleted objects")
			is.True(t, totals.MaintenanceRuns > 20, "maintenance runs")
			is.True(t, totals.CompactionRebases > 0, "rebases")
			switch mode {
			case sim.MaintenanceLease:
				is.True(t, totals.LeaseAcquisitions > 20, "lease acquisitions")
				is.True(t, totals.LeaseLosses < totals.LeaseAcquisitions/4, "leases are rarely lost when renewed")
			case sim.MaintenanceNoLease:
				is.Equal(t, 0, totals.LeaseAcquisitions)
				is.Equal(t, 0, totals.LeaseLosses)
			case sim.MaintenanceOverlap:
				is.True(t, totals.LeaseLosses > 10, "overlapping leases are taken over while held")
			}
		}
	})

	t.Run("the lease makes the work of a trigger happen about once instead of once per client", func(t *testing.T) {
		t.Parallel()
		var cfgs []sim.Config
		for _, mode := range []sim.MaintenanceMode{sim.MaintenanceLease, sim.MaintenanceNoLease} {
			for seed := uint64(1); seed <= 20; seed++ {
				cfg := sim.DefaultConfig(seed)
				cfg.ErrorRate, cfg.LostAckRate, cfg.CrashRate, cfg.OutageRate = 0, 0, 0, 0
				cfg.Clients = 8
				cfg.TxPerClient = 40
				cfg.Maintainers = 0
				cfg.Maintenance = mode
				cfgs = append(cfgs, cfg)
			}
		}
		results := runAll(cfgs)
		sum := func(mode sim.MaintenanceMode) sim.Stats {
			var totals sim.Stats
			for _, res := range results {
				if res.Config.Maintenance != mode {
					continue
				}
				if res.Failed() {
					failReport(t, res)
				}
				totals.Add(res.Stats)
			}
			t.Logf("8 clients, maintenance %v: %d merges started for %d compactions, %d GC runs, %d deletes, %d attempts", mode, totals.MergesStarted, totals.Compactions, totals.GCs, totals.Deleted, totals.MaintenanceAttempts)
			return totals
		}
		lease, none := sum(sim.MaintenanceLease), sum(sim.MaintenanceNoLease)
		is.True(t, lease.Compactions > 50, "compactions")
		is.True(t, lease.MergesStarted <= lease.Compactions*11/10, "with the lease, almost every merge that starts is the one that lands")
		is.True(t, none.MergesStarted >= 2*lease.MergesStarted, "without the lease, clients merge the same segments concurrently")
		is.True(t, none.Deleted >= 3*lease.Deleted, "without the lease, clients delete the same objects over and over")
		is.True(t, none.MaintenanceAttempts >= 3*lease.MaintenanceAttempts, "without the lease, every client attempts the work")
	})

	t.Run("the same seed produces the same history", func(t *testing.T) {
		verify.Covers(t, "the same seed gives a byte-identical history, final state, and statistics, through the engine and SQL, in every maintenance mode, also for the foreign key workload, whose now() comes from the virtual clock", "AC-1.9")
		t.Parallel()
		// Every configuration runs four times, all at once, so that runs which shared any state
		// would differ.
		var cfgs []sim.Config
		for _, useSQL := range []bool{false, true} {
			for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
				cfg := sim.DefaultConfig(7)
				cfg.Workload = w
				cfg.SQL = useSQL
				cfg.Maintainers = 2
				cfg.Maintenance = sim.MaintenanceMode(1 + int(w) + 2*len(fmt.Sprint(useSQL))%3)
				cfgs = append(cfgs, cfg, cfg, cfg, cfg)
			}
		}
		cfg := sim.DefaultConfig(7)
		cfg.Workload = sim.ForeignKey
		cfg.SQL = true
		cfg.Maintainers = 2
		cfg.Maintenance = sim.MaintenanceLease
		cfgs = append(cfgs, cfg, cfg, cfg, cfg)
		results := runAll(cfgs)
		for i := 0; i < len(results); i += 4 {
			first := results[i]
			for _, again := range results[i+1 : i+4] {
				if first.Digest != again.Digest {
					t.Fatalf("%v: digests differ:\n%s", first.Config.Name(), diff(first.Digest, again.Digest))
				}
			}
			is.True(t, len(first.History) > 0)
		}
	})

	t.Run("the SQL workloads exercise the same paths as the engine workloads", func(t *testing.T) {
		verify.Covers(t, "the SQL corpus exercises commits, conflicts, unknown outcomes, outages, crashes, compactions, and deletes", "AC-1.5")
		t.Parallel()
		totals := corpusResults().sqlTotals
		t.Logf("SQL corpus: %+v", totals)
		is.True(t, totals.Commits > 500, "commits")
		is.True(t, totals.Conflicts > 100, "conflicts")
		is.True(t, totals.Unknown > 10, "unknown outcomes")
		is.True(t, totals.LiveUnknown > 10, "unknown outcomes in processes that keep running")
		is.True(t, totals.Unavailable > 50, "operations failed by an unreachable store")
		is.True(t, totals.Crashes > 20, "crashes")
		is.True(t, totals.Compactions > 20, "compactions")
		is.True(t, totals.Deleted > 20, "deleted objects")
	})

	t.Run("different seeds produce different histories", func(t *testing.T) {
		a := runSim(sim.DefaultConfig(1))
		b := runSim(sim.DefaultConfig(2))
		is.True(t, a.Digest != b.Digest)
	})

	t.Run("replay", func(t *testing.T) {
		verify.Covers(t, "a seed in MARAGUDB_SEED replays with a full trace, from make replay; skipped otherwise", "AC-1.9")
		seedVar := os.Getenv("MARAGUDB_SEED")
		if seedVar == "" {
			t.Skip("set MARAGUDB_SEED to replay a seed")
		}
		seed, err := strconv.ParseUint(seedVar, 10, 64)
		is.NotError(t, err)
		cfg := sim.DefaultConfig(seed)
		switch os.Getenv("MARAGUDB_WORKLOAD") {
		case "bank":
			cfg.Workload = sim.Bank
		case "unique":
			cfg.Workload = sim.Unique
		case "foreign-key":
			cfg.Workload = sim.ForeignKey
		}
		cfg.SQL = os.Getenv("MARAGUDB_API") == "sql" || cfg.Workload == sim.Unique || cfg.Workload == sim.ForeignKey
		cfg.Maintainers = 2
		if os.Getenv("MARAGUDB_CORPUS") != "" {
			found := false
			for _, c := range corpus() {
				if c.Seed == cfg.Seed && c.Workload == cfg.Workload && c.SQL == cfg.SQL {
					cfg, found = c, true
				}
			}
			if !found {
				t.Fatalf("no corpus configuration with seed %d, workload %v, and SQL %v", cfg.Seed, cfg.Workload, cfg.SQL)
			}
		}
		if n := os.Getenv("MARAGUDB_MAINTAINERS"); n != "" {
			cfg.Maintainers, err = strconv.Atoi(n)
			is.NotError(t, err)
		}
		if os.Getenv("MARAGUDB_DISK_FAULTS") != "" {
			cfg.DiskFaults = true
		}
		if os.Getenv("MARAGUDB_STORE_DAMAGE") != "" {
			cfg.StoreDamage = true
		}
		if p := os.Getenv("MARAGUDB_DAMAGE_RATE"); p != "" {
			cfg.DamageRate, err = strconv.ParseFloat(p, 64)
			is.NotError(t, err)
		}
		if n := os.Getenv("MARAGUDB_BLOCK_SIZE"); n != "" {
			cfg.BlockSize, err = strconv.Atoi(n)
			is.NotError(t, err)
		}
		if d := os.Getenv("MARAGUDB_COMPACTION_STALL"); d != "" {
			cfg.CompactionStall, err = time.ParseDuration(d)
			is.NotError(t, err)
		}
		if os.Getenv("MARAGUDB_ONE_COMPACTOR") != "" {
			cfg.OneCompactor = true
		}
		if os.Getenv("MARAGUDB_TIERED_COMPACTIONS") != "" {
			cfg.TieredCompactions = true
		}
		if p := os.Getenv("MARAGUDB_COMMIT_PARTITIONS"); p != "" {
			cfg.CommitPartitions, err = strconv.ParseFloat(p, 64)
			is.NotError(t, err)
		}
		switch os.Getenv("MARAGUDB_MAINTENANCE") {
		case "lease":
			cfg.Maintenance = sim.MaintenanceLease
		case "none":
			cfg.Maintenance = sim.MaintenanceNoLease
		case "overlap":
			cfg.Maintenance = sim.MaintenanceOverlap
		}
		if name := os.Getenv("MARAGUDB_MUTATION"); name != "" {
			found := false
			for _, m := range mutants() {
				if m.replay == name {
					found = true
					cfg = m.apply(cfg)
				}
			}
			if !found {
				t.Fatalf("unknown mutation %q", name)
			}
		}
		cfg.Trace = func(line string) {
			t.Log(line)
		}
		res := sim.Run(cfg)
		t.Log(res.Report())
		if res.Failed() {
			t.Fail()
		}
	})

	t.Run("soak", func(t *testing.T) {
		verify.Covers(t, "random seeds and configurations for a set duration find no anomalies, from make soak; skipped otherwise", "AC-1.6")
		if os.Getenv("MARAGUDB_SOAK") == "" {
			t.Skip("set MARAGUDB_SOAK to run the soak")
		}
		duration := 5 * time.Minute
		if d := os.Getenv("MARAGUDB_SOAK_DURATION"); d != "" {
			var err error
			duration, err = time.ParseDuration(d)
			is.NotError(t, err)
		}
		rng := rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 0))
		deadline := time.Now().Add(duration)
		runs := 0
		batch := simBatch{name: "soak"}
		defer batch.record(t)
		for time.Now().Before(deadline) {
			seed := rng.Uint64()
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = sim.Workload(rng.IntN(4))
			cfg.SQL = rng.IntN(2) == 0 || cfg.Workload == sim.Unique || cfg.Workload == sim.ForeignKey
			cfg.Clients = 2 + rng.IntN(12)
			cfg.TxPerClient = 10 + rng.IntN(60)
			cfg.Keys = 1 + rng.IntN(6)
			cfg.Maintainers = rng.IntN(3)
			cfg.Maintenance = sim.MaintenanceMode(rng.IntN(4))
			cfg.ErrorRate = rng.Float64() * 0.3
			cfg.LostAckRate = rng.Float64() * 0.3
			cfg.CrashRate = rng.Float64() * 0.05
			cfg.MaxLatency = time.Duration(1+rng.IntN(50)) * time.Millisecond
			cfg.OutageRate = rng.Float64() * 0.03
			cfg.MaxOutage = time.Duration(1+rng.IntN(3000)) * time.Millisecond
			cfg.OutageTimeout = time.Duration(rng.IntN(200)) * time.Millisecond
			cfg.GracePeriod = time.Duration(50+rng.IntN(3000)) * time.Millisecond
			cfg.DiskFaults = rng.IntN(2) == 0
			cfg.StoreDamage = rng.IntN(2) == 0
			if rng.IntN(4) == 0 {
				cfg.BlockSize = 64 + rng.IntN(448)
			}
			if cfg.StoreDamage && rng.IntN(4) == 0 {
				cfg.DamageRate = rng.Float64() * 0.2
			}
			if rng.IntN(4) == 0 {
				cfg.CompactionStall = time.Duration(1+rng.IntN(1000)) * time.Millisecond
			}
			cfg.OneCompactor = rng.IntN(4) == 0
			cfg.TieredCompactions = rng.IntN(4) == 0
			if rng.IntN(4) == 0 {
				cfg.CommitPartitions = rng.Float64() * 0.5
			}
			res := runSim(cfg)
			runs++
			batch.add(res)
			if res.Failed() {
				t.Errorf("config %+v", cfg)
				failReport(t, res)
				return
			}
		}
		t.Logf("soak: %d runs in %v without anomalies", runs, duration)
	})
}

// simBatch sums the results of a batch of runs for the verification report.
type simBatch struct {
	name  string
	seeds []uint64
	sim   verify.Sim
}

func (b *simBatch) add(res sim.Result) {
	b.seeds = append(b.seeds, res.Config.Seed)
	b.sim.Runs++
	b.sim.Transactions += len(res.History)
	b.sim.Commits += res.Stats.Commits
	b.sim.VirtualTime += res.Stats.Elapsed
	b.sim.Anomalies += len(res.Anomalies)
}

func (b *simBatch) record(t *testing.T) {
	b.sim.Name = b.name
	b.sim.Seeds = verify.SeedRanges(b.seeds)
	verify.Record(t, verify.Fact{Sim: &b.sim})
}

func diff(a, b string) string {
	al := strings.Split(a, "\n")
	bl := strings.Split(b, "\n")
	for i := range min(len(al), len(bl)) {
		if al[i] != bl[i] {
			return fmt.Sprintf("line %d:\n  %s\n  %s", i+1, al[i], bl[i])
		}
	}
	return fmt.Sprintf("lengths %d and %d", len(al), len(bl))
}

// mutant is a deliberate bug that the simulator's checkers must detect.
type mutant struct {
	// id is the mutant's stable ID, and replay its name for `make replay MUTATION=...`.
	id, replay, name, description string
	mutations                     engine.Mutations
	sqlMutations                  exec.Mutations
	// diskMutations break the disk cache, and run with [sim.Config.DiskFaults].
	diskMutations cache.DiskMutations
	// storeDamage and damageRate run the mutant with [sim.Config.StoreDamage] and
	// [sim.Config.DamageRate].
	storeDamage bool
	damageRate  float64
	// blockSize runs the mutant with [sim.Config.BlockSize].
	blockSize int
	// maintenance runs the mutant with the clients' maintenance in this mode, when it is not
	// [sim.MaintenanceOff].
	maintenance sim.MaintenanceMode
	// compactionStall, oneCompactor, tieredCompactions, and commitPartitions run the mutant with
	// [sim.Config.CompactionStall], [sim.Config.OneCompactor], [sim.Config.TieredCompactions], and
	// [sim.Config.CommitPartitions].
	compactionStall   time.Duration
	oneCompactor      bool
	tieredCompactions bool
	commitPartitions  float64
	// keys runs the mutant with [sim.Config.Keys], when it is not zero.
	keys int
	// unsimulated, for a mutant that no cell can run, is why not, and a targeted test detects it.
	unsimulated string
	// engineOnly, for a mutant that only the cells through the engine can run, is why the cells
	// through SQL do not apply.
	engineOnly string
	// cannot says, for every cell that cannot detect the mutant, why not.
	cannot map[cell]string
	// checkers that must each detect the mutant in at least one cell, so that the checker meant
	// to catch it keeps doing so. A mutant that names the panic checker is one whose bug is a
	// panic that escapes, so that checker counts in the cells without maintainers too.
	checkers []string
	// workloads, when set, are the only workloads that run the code the mutant breaks, and the
	// cells of the others do not apply, for the reason only.
	workloads []sim.Workload
	only      string
	// unmaintained, for a mutant of code that only runs with maintainers, is why the cells without
	// them do not apply.
	unmaintained string
	// unreached are the cells that never run the code the mutant breaks, with the reason, so that
	// they do not apply.
	unreached map[cell]string
}

// cell of the kill matrix: a workload, through an API, with a number of maintainers.
type cell struct {
	workload    sim.Workload
	sql         bool
	maintainers int
}

func (c cell) String() string {
	api := "the engine"
	if c.sql {
		api = "SQL"
	}
	maintainers := "no maintainers"
	if c.maintainers > 0 {
		maintainers = fmt.Sprint(c.maintainers, " maintainers")
	}
	return fmt.Sprintf("%v through %s, %s", c.workload, api, maintainers)
}

// cells of the kill matrix, in order. The unique and foreign key workloads run only through SQL.
func cells() []cell {
	var cs []cell
	for _, sql := range []bool{false, true} {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank, sim.Unique, sim.ForeignKey} {
			if (w == sim.Unique || w == sim.ForeignKey) && !sql {
				continue
			}
			for _, maintainers := range []int{0, 2} {
				cs = append(cs, cell{workload: w, sql: sql, maintainers: maintainers})
			}
		}
	}
	return cs
}

// Reasons that recur in the kill matrix.
const (
	noGC              = "without maintainers nothing is ever deleted, so no reader can lose an object"
	noCompaction      = "without maintainers nothing compacts, so there is never a rebase"
	sqlOnly           = "the mutant is in the SQL layer, which the engine workloads do not use"
	noAutocommitWrite = "the bank workload writes only in explicit transactions, so no autocommit statement is ever retried, and a doubled transfer conserves money anyway"
	noSecondaryIndex  = "the tables of the list-append workload have no index besides the primary key"
	noUniqueIndex     = "the workload has no unique index besides the primary key"
	noForeignKey      = "the workload has no foreign keys"
	noCheck           = "the workload has no CHECK constraints"
	noMaintainedGC    = "without maintainers, garbage collection runs only in the storage check after the run, without faults, concurrent passes, or compactions, so the steps the mutant breaks always complete"
	noMaintainedMerge = "without maintainers nothing compacts"
)

// mutants in the kill matrix.
func mutants() []mutant {
	bankCells := func(reason string) map[cell]string {
		m := map[cell]string{}
		for _, c := range cells() {
			if c.workload == sim.Bank {
				m[c] = reason
			}
		}
		return m
	}
	merge := func(ms ...map[cell]string) map[cell]string {
		out := map[cell]string{}
		for _, m := range ms {
			for c, r := range m {
				out[c] = r
			}
		}
		return out
	}
	workloadCells := func(w sim.Workload, reason string) map[cell]string {
		m := map[cell]string{}
		for _, c := range cells() {
			if c.workload == w && c.sql {
				m[c] = reason
			}
		}
		return m
	}
	noMaintainers := func(reason string) map[cell]string {
		m := map[cell]string{}
		for _, c := range cells() {
			if c.maintainers == 0 {
				m[c] = reason
			}
		}
		return m
	}
	return []mutant{
		{
			id: "MUT-COMMIT-WITHOUT-CAS", replay: "cas", name: "commit without CAS",
			description: "`engine.Mutations.CommitWithoutCAS` writes the manifest unconditionally, so a commit can overwrite a concurrent one.",
			mutations:   engine.Mutations{CommitWithoutCAS: true},
			checkers:    []string{sim.CheckerListAppend, sim.CheckerStorage},
		},
		{
			id: "MUT-GC-IGNORE-GRACE", replay: "gc", name: "GC ignoring the grace period",
			description: "`engine.Mutations.GCIgnoreGrace` deletes unreferenced objects at once, so a reader younger than the grace period can lose its snapshot.",
			mutations:   engine.Mutations{GCIgnoreGrace: true},
			cannot:      noMaintainers(noGC),
			checkers:    []string{sim.CheckerGracePeriod},
		},
		{
			id: "MUT-GC-BATCH-OBJECTS-FIRST", replay: "gc-batch-objects-first", name: "garbage batches deleted before their garbage",
			description:  "`engine.Mutations.GCBatchObjectsFirst` deletes the objects of an expired chain of garbage batches before the garbage they list and before the manifest write that drops the chain, so that a failed delete or a crash leaves the manifest referencing a missing batch.",
			mutations:    engine.Mutations{GCBatchObjectsFirst: true},
			unmaintained: noMaintainedGC,
			checkers:     []string{sim.CheckerStorage},
		},
		{
			id: "MUT-GC-SWEEP-SEGMENTS-ONLY", replay: "gc-sweep-segments-only", name: "an orphan sweep that misses garbage batches",
			description:  "`engine.Mutations.GCSweepSegmentsOnly` lists only segments when GC sweeps for orphans, so that a garbage batch whose GC pass lost or crashed, or whose delete failed after its chain was dropped, is never deleted.",
			mutations:    engine.Mutations{GCSweepSegmentsOnly: true},
			unmaintained: noMaintainedGC,
			checkers:     []string{sim.CheckerStorage},
		},
		{
			id: "MUT-GC-CHAIN-FIRST-STAMP", replay: "gc-chain-first-stamp", name: "a chain of garbage batches with the stamp of its first batch",
			description:  "`engine.Mutations.GCChainKeepsFirstStamp` adds every GC pass's garbage batch to the newest chain, however old, and keeps the stamp of the chain's first batch, so that garbage is deleted before it is a grace period old.",
			mutations:    engine.Mutations{GCChainKeepsFirstStamp: true},
			unmaintained: noMaintainedGC,
			checkers:     []string{sim.CheckerGracePeriod},
		},
		{
			id: "MUT-GC-DELETE-REFERENCED", replay: "gc-delete-referenced", name: "GC deleting expired garbage that is referenced again",
			description: "`engine.Mutations.GCDeleteReferenced` deletes the garbage of an expired chain of garbage batches also when the manifest references it otherwise, such as the output of a compaction that GC stamped as an orphan before the compaction's rebase landed. It runs with maintainers that stall for up to 900 ms between the segment write and the manifest write of a compaction, and with one maintainer that compacts while the other only collects garbage.",
			mutations:   engine.Mutations{GCDeleteReferenced: true},
			// GC must stamp the output of a compaction as an orphan between the compaction's
			// segment write and its rebased manifest write: the stall holds that window open for
			// longer than a GC pass of the other maintainer takes, and with one compactor no other
			// compaction makes the stalled one lose instead of rebase.
			compactionStall: 900 * time.Millisecond,
			oneCompactor:    true,
			unmaintained:    noMaintainedGC,
			checkers:        []string{sim.CheckerStorage},
		},
		{
			id: "MUT-COMPACT-NON-ADJACENT", replay: "compact-non-adjacent", name: "a merge of segments that are not adjacent",
			description:  "`engine.Mutations.CompactNonAdjacent` leaves the second-oldest segment of every merge where it is, and merges the segment after the merge's window instead, so that the segment left behind the merged one shadows the newer values in it.",
			mutations:    engine.Mutations{CompactNonAdjacent: true},
			unmaintained: noMaintainedMerge,
			checkers:     []string{sim.CheckerListAppend},
		},
		{
			id: "MUT-COMPACT-DROP-TOMBSTONES", replay: "compact-drop-tombstones", name: "tombstones dropped in a merge that excludes the oldest segment",
			description: "`engine.Mutations.CompactDropTombstones` drops tombstones in every merge, also in one that excludes the oldest segment, so that a value that an older segment holds shows again after its delete. It runs with maintainers that merge only the segments that the tiers make due, never the whole database.",
			mutations:   engine.Mutations{CompactDropTombstones: true},
			// A merge of the whole database includes the oldest segment, so it drops tombstones
			// rightly, and leaves none for the next merge to drop wrongly. Merges by tiers alone
			// leave the oldest segment out and keep tombstones over it.
			tieredCompactions: true,
			unmaintained:      noMaintainedMerge,
			unreached: map[cell]string{
				{workload: sim.Bank, maintainers: 2}:                  "the bank workload through the engine never deletes a key, so no merge meets a tombstone",
				{workload: sim.ListAppend, sql: true, maintainers: 2}: "the list-append workload through SQL never deletes a row, and its tables have no index besides the primary key, so no merge meets a tombstone",
			},
			checkers: []string{sim.CheckerListAppend, sim.CheckerCompaction},
		},
		{
			id: "MUT-COMPACT-REBASE-LOSES-SEGMENT", replay: "compact-rebase-loses-segment", name: "a rebase that loses a segment added meanwhile",
			description:  "`engine.Mutations.CompactRebaseLosesSegment` leaves out the oldest of the segments that commits added during a compaction when the compaction rebases on them, so that an acknowledged commit is lost.",
			mutations:    engine.Mutations{CompactRebaseLosesSegment: true},
			unmaintained: noMaintainedMerge,
			checkers:     []string{sim.CheckerDurability},
		},
		{
			id: "MUT-COMPACT-REBASE-ON-COMPACTION", replay: "compact-rebase-on-compaction", name: "a rebase on another compaction",
			description: "`engine.Mutations.CompactRebaseOnCompaction` rebases a compaction on any newer manifest with at least as many segments, also on one in which another compaction replaced segments, so that the segments of that compaction and of the commits after it are lost or put back. It runs with maintainers that stall for up to 500 ms between the segment write and the manifest write of a compaction.",
			mutations:   engine.Mutations{CompactRebaseOnCompaction: true},
			// Another compaction must land while a compaction is between its segment write and its
			// manifest write, and commits after it: the stall holds that window open for as long as
			// several commits and an iteration of the other maintainer take.
			compactionStall: 500 * time.Millisecond,
			unmaintained:    noMaintainedMerge,
			checkers:        []string{sim.CheckerDurability},
		},
		{
			id: "MUT-SKIP-FRESHNESS", replay: "freshness", name: "serving the cache without a freshness check",
			description: "`engine.Mutations.SkipFreshnessCheck` starts transactions from the cached manifest without asking the store, so a transaction can miss commits acknowledged before it began.",
			mutations:   engine.Mutations{SkipFreshnessCheck: true},
			checkers:    []string{sim.CheckerListAppend, sim.CheckerRealTime},
		},
		{
			id: "MUT-ACK-BEFORE-CONFIRM", replay: "ack", name: "acking before the write is confirmed",
			description: "`engine.Mutations.AckBeforeConfirm` reports a commit as successful without waiting for the store's answer.",
			mutations:   engine.Mutations{AckBeforeConfirm: true},
			checkers:    []string{sim.CheckerDurability, sim.CheckerStorage},
		},
		{
			id: "MUT-COMPACTED-AHEAD", replay: "compacted-ahead", name: "the compacted segment ahead of newer ones after a rebase",
			description: "`engine.Mutations.CompactedSegmentAhead` puts the compacted segment ahead of the segments committed during the compaction when it rebases, so that old values shadow newer ones.",
			mutations:   engine.Mutations{CompactedSegmentAhead: true},
			cannot:      noMaintainers(noCompaction),
			checkers:    []string{sim.CheckerListAppend},
		},
		{
			id: "MUT-FRESH-SNAPSHOT", replay: "fresh-snapshot", name: "a fresh snapshot per statement",
			description:  "`exec.Mutations.FreshSnapshotPerStatement` starts every statement of an explicit transaction that has not written yet from the latest commit, so an audit can straddle a transfer and a reader can see two states.",
			sqlMutations: exec.Mutations{FreshSnapshotPerStatement: true},
			checkers:     []string{sim.CheckerBank, sim.CheckerListAppend, sim.CheckerRealTime},
		},
		{
			id: "MUT-RETRY-UNKNOWN", replay: "retry-unknown", name: "retrying an autocommit statement after an unknown commit outcome",
			description:  "`exec.Mutations.RetryAfterUnknownCommit` retries an autocommit statement whose commit outcome is unknown, which can apply it twice. It runs with clients that are partitioned right after four in ten of their manifest writes land, so that the outcome of their commits is unknown.",
			sqlMutations: exec.Mutations{RetryAfterUnknownCommit: true},
			// The engine learns the outcome of most manifest writes whose answer is lost by reading
			// the manifest again, so only a partition right after the write leaves it unknown.
			commitPartitions: 0.4,
			cannot:           bankCells(noAutocommitWrite),
			checkers:         []string{sim.CheckerDurability},
		},
		{
			id: "MUT-INDEX-STALE-DELETE", replay: "index-stale-delete", name: "stale index entries after an UPDATE or DELETE",
			description:  "`exec.Mutations.IndexStaleDelete` leaves the old index entries of a row that an UPDATE changes or a DELETE removes.",
			sqlMutations: exec.Mutations{IndexStaleDelete: true},
			cannot:       workloadCells(sim.ListAppend, noSecondaryIndex),
			checkers:     []string{sim.CheckerIndex},
		},
		{
			id: "MUT-UNIQUE-NO-CHECK", replay: "unique-no-check", name: "no unique check",
			description:  "`exec.Mutations.UniqueNoCheck` skips the check that no other row has a row's values in a unique index.",
			sqlMutations: exec.Mutations{UniqueNoCheck: true},
			cannot:       merge(workloadCells(sim.ListAppend, noUniqueIndex), workloadCells(sim.Bank, noUniqueIndex), workloadCells(sim.ForeignKey, noUniqueIndex)),
			checkers:     []string{sim.CheckerUnique, sim.CheckerIndex},
		},
		{
			id: "MUT-UNIQUE-OWN-WRITES", replay: "unique-own-writes", name: "a unique check that misses the statement's own writes",
			description:  "`exec.Mutations.UniqueOwnWrites` makes the unique check read the transaction without the statement's own writes, so that an INSERT of two rows with the same value lets both in.",
			sqlMutations: exec.Mutations{UniqueOwnWrites: true},
			cannot:       merge(workloadCells(sim.ListAppend, noUniqueIndex), workloadCells(sim.Bank, noUniqueIndex), workloadCells(sim.ForeignKey, noUniqueIndex)),
			checkers:     []string{sim.CheckerIndex},
		},
		{
			id: "MUT-FK-NO-CHILD-CHECK", replay: "fk-no-child-check", name: "no check of a referencing row",
			description:  "`exec.Mutations.ForeignKeyNoChildCheck` skips the check that an inserted or updated row has the row its foreign key references.",
			sqlMutations: exec.Mutations{ForeignKeyNoChildCheck: true},
			cannot:       merge(workloadCells(sim.ListAppend, noForeignKey), workloadCells(sim.Bank, noForeignKey), workloadCells(sim.Unique, noForeignKey)),
			checkers:     []string{sim.CheckerForeignKey},
		},
		{
			id: "MUT-FK-NO-PARENT-CHECK", replay: "fk-no-parent-check", name: "no check or action on the referencing rows",
			description:  "`exec.Mutations.ForeignKeyNoParentCheck` skips the checks and the actions on the rows that reference a deleted row or an updated key.",
			sqlMutations: exec.Mutations{ForeignKeyNoParentCheck: true},
			cannot:       merge(workloadCells(sim.ListAppend, noForeignKey), workloadCells(sim.Bank, noForeignKey), workloadCells(sim.Unique, noForeignKey)),
			checkers:     []string{sim.CheckerForeignKey},
		},
		{
			id: "MUT-FK-OWN-WRITES", replay: "fk-own-writes", name: "foreign key lookups that miss the statement's own writes",
			description:  "`exec.Mutations.ForeignKeyOwnWrites` makes the lookups of foreign keys read the transaction without the statement's own writes, so that an UPDATE that moves a row away and points another row at its old key passes.",
			sqlMutations: exec.Mutations{ForeignKeyOwnWrites: true},
			cannot:       merge(workloadCells(sim.ListAppend, noForeignKey), workloadCells(sim.Bank, noForeignKey), workloadCells(sim.Unique, noForeignKey)),
			checkers:     []string{sim.CheckerForeignKey},
		},
		{
			id: "MUT-CHECK-SKIP-UPDATE", replay: "check-skip-update", name: "CHECK on INSERT only",
			description:  "`exec.Mutations.CheckSkipUpdate` checks the CHECK constraints of a row only when it is inserted, not when an UPDATE or a foreign key action changes it. It runs with one key, so that the foreign key workload draws its keys from a pool of two.",
			sqlMutations: exec.Mutations{CheckSkipUpdate: true},
			// An UPDATE that breaks a CHECK must find its row, and the parent that it points a
			// child at, among the keys of the pool, which most UPDATEs miss in a pool of eight.
			keys:     1,
			cannot:   merge(workloadCells(sim.ListAppend, noCheck), workloadCells(sim.Bank, noCheck), workloadCells(sim.Unique, noCheck)),
			checkers: []string{sim.CheckerCheck},
		},
		{
			id: "MUT-SUBQUERY-STALE-READS", replay: "subquery-stale-reads", name: "subquery reads that a retry does not repeat",
			description:  "`exec.Mutations.SubqueryStaleAcrossRetries` keeps the results of the uncorrelated subqueries of an autocommit statement when it retries after a conflict, so that the retry writes what a subquery read in the snapshot whose commit lost, outside of the conflict check of the commit that succeeds.",
			sqlMutations: exec.Mutations{SubqueryStaleAcrossRetries: true},
			workloads:    []sim.Workload{sim.ListAppend},
			only:         "only the list-append workload writes with a subquery, an append to a packed list that reads the list in a subquery",
			checkers:     []string{sim.CheckerDurability},
		},
		{
			id: "MUT-UPDATE-SEES-OWN-WRITES", replay: "update-sees-own-writes", name: "an UPDATE that revisits the rows it wrote",
			description:  "`exec.Mutations.UpdateSeesOwnWrites` restarts an UPDATE's scan after every row it writes, the Halloween problem. It only shows when an UPDATE moves rows to keys it has not visited yet.",
			sqlMutations: exec.Mutations{UpdateSeesOwnWrites: true},
			cannot: func() map[cell]string {
				m := map[cell]string{}
				for _, c := range cells() {
					m[c] = "the workload never updates primary key columns, so no row moves ahead of the scan"
					if c.workload == sim.ForeignKey {
						m[c] = "the only UPDATE of a primary key moves one node to a new key, and when it revisits that row, its CASE leaves a row with the new key as it is"
					}
				}
				return m
			}(),
		},
		{
			id: "MUT-DISK-CACHE-UNVERIFIED", replay: "disk-cache-unverified", name: "a disk cache that serves damaged files",
			description:   "`cache.DiskMutations.Unverified` serves the bytes of a disk cache file without checking its header, key, length, and checksum, so that a file that a crash truncated, corrupted, or filled with another entry's content is served. The engine checks every block and manifest it reads, so it refuses them, and the cache integrity checker finds that the cache returned them.",
			diskMutations: cache.DiskMutations{Unverified: true},
			checkers:      []string{sim.CheckerCache},
		},
		{
			id: "MUT-DISK-CACHE-SERVE-CUT-READ", replay: "disk-cache-serve-cut-read", name: "a disk cache that serves a read that an eviction cut short",
			description:   "`cache.DiskMutations.ServeCutReads` serves a disk cache file that is shorter than its header says as it is, as a read that an eviction or a replacement cut short leaves on a file system that does not keep a removed file readable.",
			diskMutations: cache.DiskMutations{ServeCutReads: true},
			unsimulated:   "a disk cache read in the simulator never overlaps an eviction, because the scheduler runs one task at a time and a read of the disk cache does not yield, and a block or manifest cut short fails to decode in the engine anyway; `TestDisk` of the cache overlaps reads with evictions and detects the mutant",
		},
		{
			id: "MUT-SEGMENT-UNCHECKED", replay: "segment-unchecked", name: "blocks of segments read without their checksum",
			description: "`engine.Mutations.SegmentUnchecked` decodes the blocks of segments without checking their checksums, and caches what the store returns before decoding it, so that a block whose bytes the store damaged is served, or cached and served later. It runs with damaged reads from the store.",
			mutations:   engine.Mutations{SegmentUnchecked: true},
			storeDamage: true,
			checkers:    []string{sim.CheckerCache},
		},
		{
			id: "MUT-FILTER-OMITS-KEYS", replay: "filter-omits-keys", name: "key filters that leave out keys",
			description: "`engine.Mutations.FilterOmitsKeys` leaves every other key of a segment out of its key filter, so that a point lookup of such a key skips the segment that holds its newest value and reads an older value, or none.",
			mutations:   engine.Mutations{FilterOmitsKeys: true},
		},
		{
			id: "MUT-FILTER-UNCHECKED", replay: "filter-unchecked", name: "filter blocks read without their checksum",
			description: "`engine.Mutations.FilterUnchecked` decodes the filter blocks of segments without checking their checksums, and caches what the store returns before decoding it, so that a filter block whose bytes the store damaged is trusted, or cached and trusted later. It runs with blocks of 64 bytes, so that segments have filter blocks, and with one in ten reads from the store damaged.",
			mutations:   engine.Mutations{FilterUnchecked: true},
			// Most reads of filter blocks from the store are of the clients without caches, which
			// read a filter block on every lookup, and the damage that they trust is mostly in bits
			// that the lookup does not test. Small blocks give segments more filter blocks, and more
			// damage reaches the reads of the clients that cache them.
			storeDamage: true,
			damageRate:  0.1,
			blockSize:   64,
			unreached: func() map[cell]string {
				m := map[cell]string{}
				for _, c := range cells() {
					if c.workload == sim.Bank && !c.sql {
						m[c] = "the bank workload through the engine keeps four accounts, whose segments fit in one block even of 64 bytes, so no segment has a filter block"
					}
				}
				return m
			}(),
			checkers: []string{sim.CheckerCache},
		},
		{
			id: "MUT-DISK-CACHE-UNCOUNTED", replay: "disk-cache-uncounted", name: "a disk cache that does not count the files of an earlier process",
			description:   "`cache.DiskMutations.UncountedAtOpen` leaves the files that are in a disk cache directory at open out of the cache, so that a restarted process never counts or evicts them and its directory grows past the bound.",
			diskMutations: cache.DiskMutations{UncountedAtOpen: true},
			checkers:      []string{sim.CheckerDiskCache},
		},
		{
			id: "MUT-MAINTENANCE-UNRECOVERED", replay: "maintenance-unrecovered", name: "a maintenance step that does not recover a panic",
			description: "`engine.Mutations.MaintenanceUnrecovered` lets a panic unwind out of a maintenance step instead of recovering it, which in the driver's background goroutine ends the process. It runs with `engine.Mutations.MaintenancePanics`, which makes one in three maintenance steps that get to work panic in the middle of a compaction or a GC pass, and with the clients' maintenance coordinated by the lease, so that the panic reaches the client's task, which fails the run.",
			mutations:   engine.Mutations{MaintenancePanics: true, MaintenanceUnrecovered: true},
			maintenance: sim.MaintenanceLease,
			checkers:    []string{sim.CheckerPanic},
		},
	}
}

// config that runs the mutant in the cell with the seed.
func (m mutant) config(c cell, seed uint64) sim.Config {
	cfg := sim.DefaultConfig(seed)
	cfg.Workload = c.workload
	cfg.SQL = c.sql
	cfg.Maintainers = c.maintainers
	return m.apply(cfg)
}

// apply the mutant to cfg: its mutations, and the faults and settings that it runs with, where
// cfg leaves them off. The block size and the keys of the mutant replace those of cfg.
func (m mutant) apply(cfg sim.Config) sim.Config {
	cfg.Mutations = m.mutations
	cfg.SQLMutations = m.sqlMutations
	cfg.DiskMutations = m.diskMutations
	cfg.DiskFaults = cfg.DiskFaults || m.diskMutations != (cache.DiskMutations{})
	cfg.StoreDamage = cfg.StoreDamage || m.storeDamage
	cfg.DamageRate = cmp.Or(cfg.DamageRate, m.damageRate)
	if m.blockSize != 0 {
		cfg.BlockSize = m.blockSize
	}
	if cfg.Maintenance == sim.MaintenanceOff {
		cfg.Maintenance = m.maintenance
	}
	cfg.CompactionStall = cmp.Or(cfg.CompactionStall, m.compactionStall)
	cfg.OneCompactor = cfg.OneCompactor || m.oneCompactor
	cfg.TieredCompactions = cfg.TieredCompactions || m.tieredCompactions
	cfg.CommitPartitions = cmp.Or(cfg.CommitPartitions, m.commitPartitions)
	if m.keys != 0 {
		cfg.Keys = m.keys
	}
	if m.sqlMutations != (exec.Mutations{}) {
		cfg.SQL = true
	}
	return cfg
}

// applicable reports whether the mutant can run in the cell, and why not: a mutant in the SQL
// layer needs SQL, and one of code that only some workloads run needs one of them.
func (m mutant) applicable(c cell) (bool, string) {
	if m.unsimulated != "" {
		return false, m.unsimulated
	}
	if c.sql && m.engineOnly != "" {
		return false, m.engineOnly
	}
	if !c.sql && m.sqlMutations != (exec.Mutations{}) {
		return false, sqlOnly
	}
	if len(m.workloads) > 0 && !slices.Contains(m.workloads, c.workload) {
		return false, m.only
	}
	if m.unmaintained != "" && c.maintainers == 0 {
		return false, m.unmaintained
	}
	if reason, ok := m.unreached[c]; ok {
		return false, reason
	}
	return true, ""
}

var update = flag.Bool("update", false, "write the kill matrix to "+verify.KillMatrixPath)

// rateWords are the words of a reason that speak of how often a checker detects a mutant.
var rateWords = []string{"seed", "rare", "seldom", "mostly", "often", "usually", "sometimes", "likely", "chance", "rate", "percent"}

// rateWord is the first number or word in reason that speaks of how often a checker detects a
// mutant, or "" when it has none. A reason why a configuration cannot detect a mutant states a
// structural cause, which holds for every seed.
func rateWord(reason string) string {
	for _, word := range strings.FieldsFunc(strings.ToLower(reason), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if strings.IndexFunc(word, unicode.IsDigit) >= 0 {
			return word
		}
		for _, w := range rateWords {
			if strings.HasPrefix(word, w) {
				return word
			}
		}
	}
	return ""
}

func TestRateWord(t *testing.T) {
	tests := []struct {
		reason, want string
	}{
		{reason: "only the storage checker finds the objects of a dropped commit, in 2 of the first 100 seeds", want: "2"},
		{reason: "in twenty seeds, no GC pass stamps the output", want: "seeds"},
		{reason: "a GC pass rarely stamps the output of a compaction", want: "rarely"},
		{reason: "which mostly leaves an older consistent state", want: "mostly"},
		{reason: "without maintainers nothing compacts, so there is never a rebase", want: ""},
		{reason: "the workload has no CHECK constraints", want: ""},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("finds %q in %q", test.want, test.reason), func(t *testing.T) {
			is.Equal(t, test.want, rateWord(test.reason))
		})
	}
}

// killMatrixSeeds tried in `make test` in every cell until one detects the mutant. A cell that
// detects it must do so with one of them.
const killMatrixSeeds = 20

// cannotSeeds tried in `make test` in a cell that is said to be unable to detect the mutant. No
// seed detects it there, so every seed that the cell tries runs.
const cannotSeeds = 5

// rateSeeds tried in every cell that can run the mutant when [allSeeds], and rateFloor the fewest
// of them that must detect the mutant in a cell that detects it. A cell that detects it with fewer
// is fragile: a change elsewhere that shifts the histories can leave it without a detection in the
// first [killMatrixSeeds]. The seeds are fixed, so the count is the same in every run, and it is
// in the kill matrix, where a change shows as a difference.
const (
	rateSeeds = 100
	rateFloor = 20
)

// allSeeds reports whether every cell tries all [rateSeeds]: when the kill matrix is written,
// with -update, and in the soak.
func allSeeds() bool {
	return *update || os.Getenv("MARAGUDB_SOAK") != ""
}

func TestRun(t *testing.T) {
	t.Run("leaves no goroutine running after the run, in every workload and maintenance mode, through the engine and SQL, with and without heavy faults", func(t *testing.T) {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank, sim.Unique, sim.ForeignKey} {
			for _, useSQL := range []bool{false, true} {
				if (w == sim.Unique || w == sim.ForeignKey) && !useSQL {
					continue
				}
				for _, mode := range []sim.MaintenanceMode{sim.MaintenanceOff, sim.MaintenanceLease, sim.MaintenanceNoLease, sim.MaintenanceOverlap} {
					for _, heavy := range []bool{false, true} {
						cfg := sim.DefaultConfig(3)
						cfg.Workload = w
						cfg.SQL = useSQL
						cfg.Maintenance = mode
						if heavy {
							// Crashes in the middle of transactions, and outages long enough that
							// processes fail to open.
							cfg.Seed = 4
							cfg.Clients = 10
							cfg.ErrorRate = 0.15
							cfg.LostAckRate = 0.15
							cfg.CrashRate = 0.05
							cfg.OutageRate = 0.03
							cfg.Maintainers = 2
						}
						t.Run(fmt.Sprintf("%v, heavy faults %v", cfg.Name(), heavy), func(t *testing.T) {
							t.Parallel()
							slots <- struct{}{}
							defer func() { <-slots }()
							// The bubble fails the test when a goroutine started in it is still
							// blocked at its end.
							synctest.Test(t, func(t *testing.T) {
								res := sim.Run(cfg)
								is.True(t, res.Stats.Commits > 0, cfg.Name())
								if heavy {
									is.True(t, res.Stats.Crashes > 0, cfg.Name())
								}
							})
						})
					}
				}
			}
		}
	})
}

func TestRun_targetedFaults(t *testing.T) {
	t.Run("a compaction stall holds every maintainer between the output and the manifest write of a compaction, one compactor leaves the others to collect garbage, and GC passes see the outputs before the manifest has them", func(t *testing.T) {
		t.Parallel()
		var cfgs []sim.Config
		var mu sync.Mutex
		var lines []string
		for seed := uint64(1); seed <= 8; seed++ {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = sim.Bank
			cfg.Maintainers = 2
			cfg.CompactionStall = 900 * time.Millisecond
			cfg.OneCompactor = true
			cfg.Trace = func(line string) {
				if strings.Contains(line, "maintainer") {
					mu.Lock()
					lines = append(lines, line)
					mu.Unlock()
				}
			}
			cfgs = append(cfgs, cfg)
		}
		reached := 0
		for _, res := range runAll(cfgs) {
			is.True(t, !res.Failed(), res.Report())
			reached += res.Reached[reach.GCKeepsLiveGarbage]
		}
		stalls := 0
		for _, line := range lines {
			is.True(t, !strings.Contains(line, "maintainer 1: compact"), line)
			_, after, ok := strings.Cut(line, "stalls for ")
			if !ok {
				continue
			}
			stalls++
			d, err := time.ParseDuration(strings.Fields(after)[0])
			is.NotError(t, err)
			is.True(t, d >= 450*time.Millisecond && d <= 900*time.Millisecond, line)
			is.True(t, strings.Contains(line, "maintainer 0: stalls for") && strings.Contains(line, "after writing db/seg/"), line)
		}
		is.True(t, stalls > 8, fmt.Sprint(stalls, " stalls"))
		is.True(t, reached > 0, "GC keeps a live segment that an expired garbage batch lists")
	})

	t.Run("tiered compactions leave every maintainer merging only the segments that the tiers make due, and a damage rate damages that share of the reads", func(t *testing.T) {
		t.Parallel()
		var cfgs []sim.Config
		var mu sync.Mutex
		count := map[bool]map[string]int{true: {}, false: {}}
		for seed := uint64(1); seed <= 8; seed++ {
			for _, targeted := range []bool{false, true} {
				cfg := sim.DefaultConfig(seed)
				cfg.Maintainers = 2
				cfg.StoreDamage = true
				if targeted {
					cfg.TieredCompactions = true
					cfg.DamageRate = 0.2
				}
				cfg.Trace = func(line string) {
					mu.Lock()
					defer mu.Unlock()
					switch {
					case strings.Contains(line, "compact the whole database true"):
						count[targeted]["whole"]++
					case strings.Contains(line, "compact the whole database false"):
						count[targeted]["tiers"]++
					case strings.Contains(line, ": damaged body"):
						count[targeted]["damaged"]++
					case strings.Contains(line, " s3 GET "):
						count[targeted]["reads"]++
					}
				}
				cfgs = append(cfgs, cfg)
			}
		}
		for _, res := range runAll(cfgs) {
			is.True(t, !res.Failed(), res.Report())
		}
		msg := fmt.Sprint(count)
		is.True(t, count[false]["whole"] > 0 && count[false]["tiers"] > 0, msg)
		is.True(t, count[true]["whole"] == 0 && count[true]["tiers"] > 0, msg)
		// The default damages one in fifty of the reads.
		rate := func(c map[string]int) float64 { return float64(c["damaged"]) / float64(c["reads"]) }
		is.True(t, rate(count[false]) > 0.01 && rate(count[false]) < 0.04, msg)
		is.True(t, rate(count[true]) > 0.15 && rate(count[true]) < 0.25, msg)
	})

	t.Run("commit partitions leave the outcome of commits unknown in processes that keep running", func(t *testing.T) {
		t.Parallel()
		var cfgs []sim.Config
		for seed := uint64(1); seed <= 8; seed++ {
			cfg := sim.DefaultConfig(seed)
			cfg.SQL = seed%2 == 0
			cfg.CommitPartitions = 0.4
			cfgs = append(cfgs, cfg)
		}
		var with, without sim.Stats
		for _, res := range runAll(cfgs) {
			is.True(t, !res.Failed(), res.Report())
			with.Add(res.Stats)
		}
		for i := range cfgs {
			cfgs[i].CommitPartitions = 0
		}
		for _, res := range runAll(cfgs) {
			without.Add(res.Stats)
		}
		is.True(t, with.LiveUnknown > 3*without.LiveUnknown, fmt.Sprint(with.LiveUnknown, " unknown outcomes without a crash with partitions, ", without.LiveUnknown, " without"))
	})
}

func TestRun_panic(t *testing.T) {
	t.Run("a panic in the SQL layer fails the run at once with the panic's value and stack, instead of hanging it in database/sql", func(t *testing.T) {
		verify.Covers(t, "a panic in the SQL layer under database/sql fails a simulation run within seconds, with the panic's value and the stack where it happened, instead of hanging it", "INV-DAMAGE-DETECTED")
		verify.Covers(t, "the simulator's SQL clients go through the driver's recovery of panics, which ends every call that panicked with an error, so that the task fails the run with the panic instead of hanging in database/sql", "INV-PANIC-RECOVERED")
		t.Parallel()
		// With seed 4, the bank workload panics in an audit, whose deferred rollback waits forever
		// for database/sql to release the connection unless the driver recovers the panic, and the
		// list-append workload panics in an autocommit statement.
		for _, w := range []sim.Workload{sim.Bank, sim.ListAppend} {
			cfg := sim.DefaultConfig(4)
			cfg.Workload = w
			cfg.SQL = true
			cfg.Maintainers = 2
			cfg.Maintenance = sim.MaintenanceLease
			cfg.SQLMutations = exec.Mutations{PanicInScan: true}
			done := make(chan sim.Result, 1)
			go func() { done <- runSim(cfg) }()
			var res sim.Result
			select {
			case res = <-done:
			case <-time.After(time.Minute):
				t.Fatalf("%v: the run hangs after a panic", cfg.Name())
			}
			is.True(t, res.Failed(), cfg.Name())
			is.EqualSlice(t, []string{sim.CheckerPanic}, res.Checkers())
			msg := res.Anomalies[len(res.Anomalies)-1].Error()
			is.True(t, strings.Contains(msg, `exec: a scan of "`), msg)
			is.True(t, strings.Contains(msg, "exec.(*Session).scanRows"), msg)
		}
	})
}

func TestRun_maintenancePanics(t *testing.T) {
	t.Run("panics that MaintenancePanics injects in the middle of the clients' compactions and GC passes are recovered, every checker passes, and maintenance resumes after each backoff", func(t *testing.T) {
		verify.Covers(t, "with panics injected at points the seed picks in the middle of the clients' compactions and GC passes, in every workload, through the engine and SQL, in every maintenance mode, with crashes and with maintainers, every checker passes, steps back off after a panic, and maintenance changes the database again after it", "INV-MAINTENANCE-PANIC")
		t.Parallel()
		var cfgs []sim.Config
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank, sim.Unique, sim.ForeignKey} {
			for _, useSQL := range []bool{false, true} {
				if (w == sim.Unique || w == sim.ForeignKey) && !useSQL {
					continue
				}
				for _, mode := range []sim.MaintenanceMode{sim.MaintenanceLease, sim.MaintenanceNoLease, sim.MaintenanceOverlap} {
					cfg := sim.DefaultConfig(uint64(600 + len(cfgs)))
					cfg.Workload = w
					cfg.SQL = useSQL
					cfg.Maintenance = mode
					cfg.Maintainers = len(cfgs) % 2 * 2
					cfg.TxPerClient = 40
					cfg.Mutations = engine.Mutations{MaintenancePanics: true}
					cfgs = append(cfgs, cfg)
				}
			}
		}
		// Whether a run's steps draw a panic, back off, and resume before the run ends is up to its
		// seed, so a configuration whose run shows none of the three tries other seeds, up to four
		// in all. Every run must pass its checkers.
		results := make([]sim.Result, len(cfgs))
		var wg sync.WaitGroup
		for i, cfg := range cfgs {
			// shows counts how many of the three a run shows.
			shows := func(s sim.Stats) int {
				n := 0
				for _, ok := range []bool{s.MaintenancePanics > 0, s.MaintenanceBackoffs > 0, s.MaintenanceResumed > 0} {
					if ok {
						n++
					}
				}
				return n
			}
			wg.Go(func() {
				for try := range uint64(4) {
					c := cfg
					c.Seed += try * 1000
					res := runSim(c)
					if try == 0 || res.Failed() || shows(res.Stats) > shows(results[i].Stats) {
						results[i] = res
					}
					if results[i].Failed() || shows(results[i].Stats) == 3 {
						return
					}
				}
			})
		}
		wg.Wait()
		// A maintainer resumes when it changes the database again after a panic, before the run
		// ends or its process crashes, which a run with few steps of work need not reach.
		backedOff, resumed := 0, 0
		for _, res := range results {
			t.Log(res.Report())
			if res.Stats.MaintenanceBackoffs > 0 {
				backedOff++
			}
			if res.Stats.MaintenanceResumed > 0 {
				resumed++
			}
		}
		for _, res := range results {
			is.True(t, !res.Failed(), res.Report())
			is.True(t, res.Stats.MaintenancePanics > 0, res.Report())
		}
		msg := fmt.Sprintf("of %d runs, %d backed off after a panic, and in %d a maintainer resumed after one", len(results), backedOff, resumed)
		t.Log(msg)
		is.True(t, backedOff >= len(results)*3/4, msg)
		is.True(t, resumed >= len(results)/2, msg)
	})
}

func TestMutants(t *testing.T) {
	verify.Covers(t, "every mutant is detected by a checker in at least one simulator configuration, every configuration that cannot detect a mutant says why, and the kill matrix matches its golden file", "AC-2")
	t.Parallel()

	ms := mutants()
	for _, m := range ms {
		for c, reason := range m.cannot {
			if word := rateWord(reason); word != "" {
				t.Errorf("%s, %v: the reason why the configuration cannot detect the mutant mentions %q, but it must state why no checker can see the mutant's effect there, not how often one does: %s", m.id, c, word, reason)
			}
		}
	}
	// ran holds the matrix row of every mutant whose subtest ran. The subtests run in parallel, so
	// the whole matrix is compared once they are all done.
	ran := make([]*verify.KillMatrixMutant, len(ms))
	t.Cleanup(func() {
		km := verify.KillMatrix{Seeds: killMatrixSeeds, CannotSeeds: cannotSeeds, RateSeeds: rateSeeds, Floor: rateFloor}
		for _, c := range cells() {
			km.Cells = append(km.Cells, c.String())
		}
		for _, kmm := range ran {
			if kmm == nil {
				// Only some mutants ran, with -run, so there is no whole matrix to compare.
				return
			}
			km.Mutants = append(km.Mutants, *kmm)
		}
		path := filepath.Join("..", "..", verify.KillMatrixPath)
		if !allSeeds() {
			// The detections in all seeds are not counted, so they are those of the golden file,
			// for the cells that it has detected too.
			var golden verify.KillMatrix
			if b, err := os.ReadFile(path); err == nil {
				_ = json.Unmarshal(b, &golden)
			}
			detections := map[[2]string]int{}
			for _, m := range golden.Mutants {
				for _, r := range m.Results {
					detections[[2]string{m.ID, r.Cell}] = r.Detections
				}
			}
			for i, m := range km.Mutants {
				for j, r := range m.Results {
					if r.Status == verify.Detected {
						km.Mutants[i].Results[j].Detections = detections[[2]string{m.ID, r.Cell}]
					}
				}
			}
		}
		got, err := json.MarshalIndent(km, "", "  ")
		if err != nil {
			t.Errorf("marshaling the kill matrix: %v", err)
			return
		}
		got = append(got, '\n')
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Errorf("writing the kill matrix: %v", err)
				return
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("reading the kill matrix: %v", err)
			return
		}
		if string(got) != string(want) {
			t.Errorf("the kill matrix differs from %s; run `make verification` to regenerate it:\n%s", verify.KillMatrixPath, diff(string(want), string(got)))
		}
	})

	for i, m := range ms {
		t.Run(m.id, func(t *testing.T) {
			t.Parallel()
			kmm := verify.KillMatrixMutant{ID: m.id, Name: m.name, Replay: m.replay, Description: m.description}
			// Every cell tries its seeds in order until one detects the mutant, a cell that cannot
			// detect it only the first [cannotSeeds], and with [allSeeds] every cell tries all
			// [rateSeeds] and counts the ones that detect it. The cells run in parallel.
			cs := cells()
			results := make([]verify.KillMatrixResult, len(cs))
			var wg sync.WaitGroup
			for j, c := range cs {
				results[j] = verify.KillMatrixResult{Cell: c.String(), Status: verify.Undetected}
				if ok, reason := m.applicable(c); !ok {
					results[j].Status = verify.NotApplicable
					results[j].Reason = reason
					continue
				}
				seeds := uint64(killMatrixSeeds)
				_, cannot := m.cannot[c]
				switch {
				case allSeeds():
					seeds = rateSeeds
				case cannot:
					seeds = cannotSeeds
				}
				wg.Go(func() {
					r := &results[j]
					for seed := uint64(1); seed <= seeds; seed++ {
						res := runSim(m.config(c, seed))
						if !res.Failed() {
							continue
						}
						if r.Status != verify.Detected {
							r.Status, r.Seed, r.Checkers = verify.Detected, seed, res.Checkers()
						}
						r.Detections++
						if !allSeeds() {
							return
						}
					}
				})
			}
			wg.Wait()
			caught := map[string]bool{}
			for j, c := range cs {
				r := results[j]
				if r.Status == verify.NotApplicable {
					kmm.Results = append(kmm.Results, r)
					continue
				}
				for _, name := range r.Checkers {
					caught[name] = true
				}
				if r.Status == verify.Detected && c.maintainers == 0 && !slices.ContainsFunc(r.Checkers, func(name string) bool {
					return name == sim.CheckerListAppend || name == sim.CheckerDurability || name == sim.CheckerBank || name == sim.CheckerUnique || name == sim.CheckerIndex || name == sim.CheckerForeignKey || name == sim.CheckerCheck || name == sim.CheckerRealTime || name == sim.CheckerStorage || name == sim.CheckerDiskCache || name == sim.CheckerCache || name == sim.CheckerDamage || name == sim.CheckerCompaction ||
						(name == sim.CheckerPanic && slices.Contains(m.checkers, sim.CheckerPanic))
				}) {
					t.Errorf("%v: without maintainers, a history or invariant checker must find the mutant, but only %v did", c, r.Checkers)
				}
				reason, cannot := m.cannot[c]
				switch {
				case r.Status == verify.Detected && cannot:
					t.Errorf("%v: detected with seed %d by %v, but the mutant is said to be undetectable there: %s", c, r.Seed, r.Checkers, reason)
				case r.Status == verify.Undetected && !cannot:
					t.Errorf("%v: not detected in %d seeds, and no reason says why it cannot be", c, killMatrixSeeds)
				case r.Status == verify.Undetected:
					r.Reason = reason
				case allSeeds() && r.Seed > killMatrixSeeds:
					t.Errorf("%v: first detected with seed %d, after the %d seeds that `make test` tries", c, r.Seed, killMatrixSeeds)
				case allSeeds() && r.Detections < rateFloor:
					t.Errorf("%v: detected with %d of %d seeds, fewer than the floor of %d, so that a change that shifts the histories can leave it undetected in the first %d; make its interleaving likely with a fault scoped to the mutant, or a check see its effect more often", c, r.Detections, rateSeeds, rateFloor, killMatrixSeeds)
				}
				if !allSeeds() {
					// The golden file has the count, which only all seeds give.
					r.Detections = 0
				}
				verify.Record(t, verify.Fact{Mutant: &verify.Mutant{ID: m.id, Cell: c.String(), Detected: r.Status == verify.Detected, Seed: r.Seed, Checkers: r.Checkers}})
				kmm.Results = append(kmm.Results, r)
			}
			for _, name := range m.checkers {
				if !caught[name] {
					t.Errorf("the %s checker never detects the mutant", name)
				}
			}
			ran[i] = &kmm
		})
	}
}
