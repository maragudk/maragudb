package sim_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

// corpus of fixed seeds and configurations run in `make test`.
func corpus() []sim.Config {
	var cfgs []sim.Config
	for seed := uint64(1); seed <= 40; seed++ {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = w
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
	// The same through the SQL layer and the database/sql driver.
	for seed := uint64(300); seed < 330; seed++ {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = w
			cfg.SQL = true
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
	t.Run("corpus finds no anomalies", func(t *testing.T) {
		verify.Covers(t, "the fixed corpus, with injected errors, latency, lost acknowledgements, outages, and crashes, finds no anomalies with any checker and exercises every fault", "AC-1.1", "AC-1.2", "AC-1.3", "AC-1.4", "AC-1.5", "AC-1.7", "AC-1.8", "AC-3.1", "AC-3.2", "AC-6.2", "INV-MULTI-WRITER", "INV-STRICT-SERIALIZABLE", "INV-CACHE-LOSS", "INV-S3-ONLY")
		verify.Covers(t, "the fixed corpus, with the unique workload under the same faults, finds no two rows with the same unique value in any snapshot a reader saw or in the final state, and after every run through SQL every index of the store matches its table, and lookups through the indexes find the rows of the table", "INV-UNIQUE", "INV-INDEX-CONSISTENT")
		verify.Covers(t, "the fixed corpus reaches every declared state at least once, among them crashes during commits, compactions, and GC passes, lost acknowledgements resolved both ways, and unique violations that only a concurrent commit causes",
			"AC-1.3", "AC-1.4", "STATE-LOST-ACK-LANDED", "STATE-LOST-ACK-CONFLICT", "STATE-LIVE-UNKNOWN", "STATE-AUTOCOMMIT-RETRY",
			"STATE-COMPACTION-REBASE", "STATE-GC-REBASE", "STATE-LEASE-TAKEOVER", "STATE-LEASE-OVERLAP",
			"STATE-SNAPSHOT-EXPIRED-REFUSED", "STATE-BLOCK-READ-RETRY-SUCCEEDED", "STATE-CRASH-MID-COMMIT-RECOVERED",
			"STATE-CRASH-MID-COMPACTION", "STATE-CRASH-MID-GC", "STATE-UNIQUE-AFTER-CONFLICT", "STATE-UNIQUE-VALUE-REUSED")
		var totals sim.Stats
		batch := simBatch{name: "corpus"}
		reached := map[string]int{}
		start := time.Now()
		for _, cfg := range corpus() {
			res := sim.Run(cfg)
			if res.Failed() {
				failReport(t, res)
			}
			totals.Add(res.Stats)
			batch.add(res)
			for s, n := range res.Reached {
				reached[string(s)] += n
			}
		}
		batch.record(t)
		verify.Record(t, verify.Fact{Reached: reached})
		t.Logf("corpus of %d runs in %v: %+v", len(corpus()), time.Since(start).Round(time.Millisecond), totals)
		for _, d := range reach.Declarations() {
			t.Logf("reached %v %d times", d.State, reached[string(d.State)])
			is.True(t, reached[string(d.State)] > 0, fmt.Sprintf("the corpus reaches %v: %s", d.State, d.Description))
		}
		// The corpus must exercise every interesting path, or passing means nothing.
		is.True(t, totals.Commits > 1000, "commits")
		is.True(t, totals.Conflicts > 100, "conflicts")
		is.True(t, totals.Unknown > 50, "unknown outcomes")
		is.True(t, totals.LiveUnknown > 50, "unknown outcomes in processes that keep running")
		is.True(t, totals.Outages > 200, "outages and partitions")
		is.True(t, totals.Unavailable > 100, "operations failed by an unreachable store")
		is.True(t, totals.Crashes > 50, "crashes")
		is.True(t, totals.Compactions > 50, "compactions")
		is.True(t, totals.Deleted > 50, "deleted objects")
		is.True(t, totals.UniqueViolations > 200, "unique violations")
	})

	t.Run("compaction succeeds against steady writers by rebasing on their commits", func(t *testing.T) {
		verify.Covers(t, "at least 90 percent of compactions succeed against fault-free steady writers, so storage stays bounded under load", "AC-6.1")
		var totals sim.Stats
		for seed := uint64(1); seed <= 40; seed++ {
			cfg := sim.DefaultConfig(seed)
			cfg.ErrorRate, cfg.LostAckRate, cfg.CrashRate, cfg.OutageRate = 0, 0, 0, 0
			cfg.Clients = 8
			cfg.TxPerClient = 40
			res := sim.Run(cfg)
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
		for _, mode := range []sim.MaintenanceMode{sim.MaintenanceLease, sim.MaintenanceNoLease, sim.MaintenanceOverlap} {
			var totals sim.Stats
			for seed := uint64(1); seed <= 12; seed++ {
				for _, useSQL := range []bool{false, true} {
					cfg := sim.DefaultConfig(seed)
					cfg.Workload = sim.Workload(seed % 2)
					cfg.SQL = useSQL
					cfg.Maintenance = mode
					cfg.Maintainers = 0
					res := sim.Run(cfg)
					if res.Failed() {
						failReport(t, res)
					}
					totals.Add(res.Stats)
				}
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
		run := func(mode sim.MaintenanceMode) sim.Stats {
			var totals sim.Stats
			for seed := uint64(1); seed <= 20; seed++ {
				cfg := sim.DefaultConfig(seed)
				cfg.ErrorRate, cfg.LostAckRate, cfg.CrashRate, cfg.OutageRate = 0, 0, 0, 0
				cfg.Clients = 8
				cfg.TxPerClient = 40
				cfg.Maintainers = 0
				cfg.Maintenance = mode
				res := sim.Run(cfg)
				if res.Failed() {
					failReport(t, res)
				}
				totals.Add(res.Stats)
			}
			t.Logf("8 clients, maintenance %v: %d merges started for %d compactions, %d GC runs, %d deletes, %d attempts", mode, totals.MergesStarted, totals.Compactions, totals.GCs, totals.Deleted, totals.MaintenanceAttempts)
			return totals
		}
		lease, none := run(sim.MaintenanceLease), run(sim.MaintenanceNoLease)
		is.True(t, lease.Compactions > 50, "compactions")
		is.True(t, lease.MergesStarted <= lease.Compactions*11/10, "with the lease, almost every merge that starts is the one that lands")
		is.True(t, none.MergesStarted >= 2*lease.MergesStarted, "without the lease, clients merge the same segments concurrently")
		is.True(t, none.Deleted >= 3*lease.Deleted, "without the lease, clients delete the same objects over and over")
		is.True(t, none.MaintenanceAttempts >= 3*lease.MaintenanceAttempts, "without the lease, every client attempts the work")
	})

	t.Run("the same seed produces the same history", func(t *testing.T) {
		verify.Covers(t, "the same seed gives a byte-identical history, final state, and statistics, through the engine and SQL, in every maintenance mode", "AC-1.9")
		for _, useSQL := range []bool{false, true} {
			for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
				cfg := sim.DefaultConfig(7)
				cfg.Workload = w
				cfg.SQL = useSQL
				cfg.Maintainers = 2
				cfg.Maintenance = sim.MaintenanceMode(1 + int(w) + 2*len(fmt.Sprint(useSQL))%3)
				first := sim.Run(cfg)
				for range 3 {
					again := sim.Run(cfg)
					if first.Digest != again.Digest {
						t.Fatalf("%v: digests differ:\n%s", cfg.Name(), diff(first.Digest, again.Digest))
					}
				}
				is.True(t, len(first.History) > 0)
			}
		}
	})

	t.Run("the SQL workloads exercise the same paths as the engine workloads", func(t *testing.T) {
		verify.Covers(t, "the SQL corpus exercises commits, conflicts, unknown outcomes, outages, crashes, compactions, and deletes", "AC-1.5")
		var totals sim.Stats
		for _, cfg := range corpus() {
			if !cfg.SQL {
				continue
			}
			res := sim.Run(cfg)
			totals.Add(res.Stats)
		}
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
		a := sim.Run(sim.DefaultConfig(1))
		b := sim.Run(sim.DefaultConfig(2))
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
		}
		cfg.SQL = os.Getenv("MARAGUDB_API") == "sql" || cfg.Workload == sim.Unique
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
					cfg.Mutations = m.mutations
					cfg.SQLMutations = m.sqlMutations
					if m.sqlMutations != (exec.Mutations{}) {
						cfg.SQL = true
					}
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
			cfg.Workload = sim.Workload(rng.IntN(3))
			cfg.SQL = rng.IntN(2) == 0 || cfg.Workload == sim.Unique
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
			res := sim.Run(cfg)
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
	// cannot says, for every cell that cannot detect the mutant, why not.
	cannot map[cell]string
	// checkers that must each detect the mutant in at least one cell, so that the checker meant
	// to catch it keeps doing so.
	checkers []string
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

// cells of the kill matrix, in order. The unique workload runs only through SQL.
func cells() []cell {
	var cs []cell
	for _, sql := range []bool{false, true} {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank, sim.Unique} {
			if w == sim.Unique && !sql {
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
	bankConserves     = "an unconditional manifest write drops whole concurrent commits, a dropped transfer conserves money, and the bank checker only checks totals"
	noGC              = "without maintainers nothing is ever deleted, so no reader can lose an object"
	noCompaction      = "without maintainers nothing compacts, so there is never a rebase"
	sqlOnly           = "the mutant is in the SQL layer, which the engine workloads do not use"
	noAutocommitWrite = "the bank workload writes only in explicit transactions, so no autocommit statement is ever retried, and a doubled transfer conserves money anyway"
	noSecondaryIndex  = "the tables of the list-append workload have no index besides the primary key"
	noUniqueIndex     = "the workload has no unique index besides the primary key"
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
			cannot:      merge(bankCells(bankConserves), workloadCells(sim.Unique, "an unconditional manifest write drops whole concurrent commits, which leaves a consistent state in which no value repeats, and the unique workload does not check that acknowledged writes survive")),
			checkers:    []string{sim.CheckerListAppend},
		},
		{
			id: "MUT-GC-IGNORE-GRACE", replay: "gc", name: "GC ignoring the grace period",
			description: "`engine.Mutations.GCIgnoreGrace` deletes unreferenced objects at once, so a reader younger than the grace period can lose its snapshot.",
			mutations:   engine.Mutations{GCIgnoreGrace: true},
			cannot:      noMaintainers(noGC),
			checkers:    []string{sim.CheckerGracePeriod},
		},
		{
			id: "MUT-SKIP-FRESHNESS", replay: "freshness", name: "serving the cache without a freshness check",
			description: "`engine.Mutations.SkipFreshnessCheck` starts transactions from the cached manifest without asking the store, so a transaction can miss commits acknowledged before it began.",
			mutations:   engine.Mutations{SkipFreshnessCheck: true},
			cannot: merge(bankCells("a stale snapshot makes a transfer conflict or read an older consistent state, which conserves money"),
				map[cell]string{{workload: sim.Unique, sql: true}: "a stale snapshot is an older consistent state in which no value repeats, and a write from it conflicts at commit; without maintainers no reader loses an object either"}),
			checkers: []string{sim.CheckerListAppend},
		},
		{
			id: "MUT-ACK-BEFORE-CONFIRM", replay: "ack", name: "acking before the write is confirmed",
			description: "`engine.Mutations.AckBeforeConfirm` reports a commit as successful without waiting for the store's answer.",
			mutations:   engine.Mutations{AckBeforeConfirm: true},
			cannot:      merge(bankCells("an acknowledged transfer that never landed is missing as a whole, which conserves money"), workloadCells(sim.Unique, "an acknowledged write that never landed is missing as a whole, which leaves a consistent state, and the unique workload does not check that acknowledged writes survive")),
			checkers:    []string{sim.CheckerDurability},
		},
		{
			id: "MUT-COMPACTED-AHEAD", replay: "compacted-ahead", name: "the compacted segment ahead of newer ones after a rebase",
			description: "`engine.Mutations.CompactedSegmentAhead` puts the compacted segment ahead of the segments committed during the compaction when it rebases, so that old values shadow newer ones.",
			mutations:   engine.Mutations{CompactedSegmentAhead: true},
			cannot:      merge(noMaintainers(noCompaction), map[cell]string{{workload: sim.Bank, maintainers: 2}: "resurrected balances are a consistent older state, which conserves money, and the engine workload has no index"}),
			checkers:    []string{sim.CheckerListAppend},
		},
		{
			id: "MUT-FRESH-SNAPSHOT", replay: "fresh-snapshot", name: "a fresh snapshot per statement",
			description:  "`exec.Mutations.FreshSnapshotPerStatement` starts every statement of an explicit transaction that has not written yet from the latest commit, so an audit can straddle a transfer and a reader can see two states.",
			sqlMutations: exec.Mutations{FreshSnapshotPerStatement: true},
			checkers:     []string{sim.CheckerBank, sim.CheckerListAppend},
		},
		{
			id: "MUT-RETRY-UNKNOWN", replay: "retry-unknown", name: "retrying an autocommit statement after an unknown commit outcome",
			description:  "`exec.Mutations.RetryAfterUnknownCommit` retries an autocommit statement whose commit outcome is unknown, which can apply it twice.",
			sqlMutations: exec.Mutations{RetryAfterUnknownCommit: true},
			cannot:       merge(bankCells(noAutocommitWrite), workloadCells(sim.Unique, "a retried insert fails on its own primary key, and a retried move or delete finds its row moved or gone, so a statement applied twice leaves a consistent state")),
			checkers:     []string{sim.CheckerDurability},
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
			cannot:       merge(workloadCells(sim.ListAppend, noUniqueIndex), workloadCells(sim.Bank, noUniqueIndex)),
			checkers:     []string{sim.CheckerUnique, sim.CheckerIndex},
		},
		{
			id: "MUT-UNIQUE-OWN-WRITES", replay: "unique-own-writes", name: "a unique check that misses the statement's own writes",
			description:  "`exec.Mutations.UniqueOwnWrites` makes the unique check read the transaction without the statement's own writes, so that an INSERT of two rows with the same value lets both in.",
			sqlMutations: exec.Mutations{UniqueOwnWrites: true},
			cannot:       merge(workloadCells(sim.ListAppend, noUniqueIndex), workloadCells(sim.Bank, noUniqueIndex)),
			checkers:     []string{sim.CheckerIndex},
		},
		{
			id: "MUT-UPDATE-SEES-OWN-WRITES", replay: "update-sees-own-writes", name: "an UPDATE that revisits the rows it wrote",
			description:  "`exec.Mutations.UpdateSeesOwnWrites` restarts an UPDATE's scan after every row it writes, the Halloween problem. It only shows when an UPDATE moves rows to keys it has not visited yet.",
			sqlMutations: exec.Mutations{UpdateSeesOwnWrites: true},
			cannot: func() map[cell]string {
				m := map[cell]string{}
				for _, c := range cells() {
					m[c] = "the workloads never update primary key columns, so no row moves ahead of the scan"
				}
				return m
			}(),
		},
	}
}

// applicable reports whether the mutant can run in the cell: a mutant in the SQL layer needs SQL.
func (m mutant) applicable(c cell) bool {
	return c.sql || m.sqlMutations == (exec.Mutations{})
}

var update = flag.Bool("update", false, "write the kill matrix to "+verify.KillMatrixPath)

// killMatrixSeeds tried in every cell.
const killMatrixSeeds = 20

func TestMutants(t *testing.T) {
	verify.Covers(t, "every mutant is detected by a checker in at least one simulator configuration, every configuration that cannot detect a mutant says why, and the kill matrix matches its golden file", "AC-2")

	km := verify.KillMatrix{Seeds: killMatrixSeeds}
	for _, c := range cells() {
		km.Cells = append(km.Cells, c.String())
	}
	for _, m := range mutants() {
		t.Run(m.id, func(t *testing.T) {
			kmm := verify.KillMatrixMutant{ID: m.id, Name: m.name, Replay: m.replay, Description: m.description}
			caught := map[string]bool{}
			for _, c := range cells() {
				r := verify.KillMatrixResult{Cell: c.String(), Status: verify.Undetected}
				if !m.applicable(c) {
					r.Status = verify.NotApplicable
					r.Reason = sqlOnly
					kmm.Results = append(kmm.Results, r)
					continue
				}
				for seed := uint64(1); seed <= killMatrixSeeds; seed++ {
					cfg := sim.DefaultConfig(seed)
					cfg.Workload = c.workload
					cfg.SQL = c.sql
					cfg.Maintainers = c.maintainers
					cfg.Mutations = m.mutations
					cfg.SQLMutations = m.sqlMutations
					res := sim.Run(cfg)
					if res.Failed() {
						r.Status, r.Seed, r.Checkers = verify.Detected, seed, res.Checkers()
						break
					}
				}
				for _, name := range r.Checkers {
					caught[name] = true
				}
				if r.Status == verify.Detected && c.maintainers == 0 && !slices.ContainsFunc(r.Checkers, func(name string) bool {
					return name == sim.CheckerListAppend || name == sim.CheckerDurability || name == sim.CheckerBank || name == sim.CheckerUnique || name == sim.CheckerIndex
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
				}
				verify.Record(t, verify.Fact{Mutant: &verify.Mutant{ID: m.id, Cell: c.String(), Detected: r.Status == verify.Detected, Seed: r.Seed, Checkers: r.Checkers}})
				kmm.Results = append(kmm.Results, r)
			}
			for _, name := range m.checkers {
				if !caught[name] {
					t.Errorf("the %s checker never detects the mutant", name)
				}
			}
			km.Mutants = append(km.Mutants, kmm)
		})
	}

	if len(km.Mutants) != len(mutants()) {
		// Only some mutants ran, with -run, so there is no whole matrix to compare.
		return
	}
	got, err := json.MarshalIndent(km, "", "  ")
	is.NotError(t, err)
	got = append(got, '\n')
	path := filepath.Join("..", "..", verify.KillMatrixPath)
	if *update {
		is.NotError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	is.NotError(t, err)
	if string(got) != string(want) {
		t.Errorf("the kill matrix differs from %s; run `make verification` to regenerate it:\n%s", verify.KillMatrixPath, diff(string(want), string(got)))
	}
}
