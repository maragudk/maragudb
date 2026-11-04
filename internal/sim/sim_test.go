package sim_test

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/sql/exec"
)

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
	api := ""
	if res.Config.SQL {
		api = " API=sql"
	}
	t.Errorf("%s\n\nreplay with:\n  make replay SEED=%d WORKLOAD=%v%s\nor:\n  MARAGUDB_SEED=%d MARAGUDB_WORKLOAD=%v MARAGUDB_API=%s go test ./internal/sim -run TestSimulator/replay -v",
		res.Report(), res.Config.Seed, res.Config.Workload, api, res.Config.Seed, res.Config.Workload, strings.TrimPrefix(api, " API="))
}

func TestSimulator(t *testing.T) {
	t.Run("corpus finds no anomalies", func(t *testing.T) {
		var totals sim.Stats
		reached := map[reach.State]int{}
		start := time.Now()
		for _, cfg := range corpus() {
			res := sim.Run(cfg)
			if res.Failed() {
				failReport(t, res)
			}
			totals.Add(res.Stats)
			for s, n := range res.Reached {
				reached[s] += n
			}
		}
		t.Logf("corpus of %d runs in %v: %+v", len(corpus()), time.Since(start).Round(time.Millisecond), totals)
		for _, d := range reach.Declarations() {
			t.Logf("reached %v %d times", d.State, reached[d.State])
			is.True(t, reached[d.State] > 0, fmt.Sprintf("the corpus reaches %v: %s", d.State, d.Description))
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
	})

	t.Run("compaction succeeds against steady writers by rebasing on their commits", func(t *testing.T) {
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
		seedVar := os.Getenv("MARAGUDB_SEED")
		if seedVar == "" {
			t.Skip("set MARAGUDB_SEED to replay a seed")
		}
		seed, err := strconv.ParseUint(seedVar, 10, 64)
		is.NotError(t, err)
		cfg := sim.DefaultConfig(seed)
		if os.Getenv("MARAGUDB_WORKLOAD") == "bank" {
			cfg.Workload = sim.Bank
		}
		cfg.SQL = os.Getenv("MARAGUDB_API") == "sql"
		cfg.Maintainers = 2
		switch os.Getenv("MARAGUDB_MAINTENANCE") {
		case "lease":
			cfg.Maintenance = sim.MaintenanceLease
		case "none":
			cfg.Maintenance = sim.MaintenanceNoLease
		case "overlap":
			cfg.Maintenance = sim.MaintenanceOverlap
		}
		switch os.Getenv("MARAGUDB_MUTATION") {
		case "cas":
			cfg.Mutations.CommitWithoutCAS = true
		case "gc":
			cfg.Mutations.GCIgnoreGrace = true
		case "freshness":
			cfg.Mutations.SkipFreshnessCheck = true
		case "ack":
			cfg.Mutations.AckBeforeConfirm = true
		case "retry-unknown":
			cfg.SQL = true
			cfg.SQLMutations.RetryAfterUnknownCommit = true
		case "fresh-snapshot":
			cfg.SQL = true
			cfg.SQLMutations.FreshSnapshotPerStatement = true
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
		for time.Now().Before(deadline) {
			seed := rng.Uint64()
			cfg := sim.DefaultConfig(seed)
			cfg.Workload = sim.Workload(rng.IntN(2))
			cfg.SQL = rng.IntN(2) == 0
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
			if res.Failed() {
				t.Errorf("config %+v", cfg)
				failReport(t, res)
				return
			}
		}
		t.Logf("soak: %d runs in %v without anomalies", runs, duration)
	})
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

func TestMutants(t *testing.T) {
	mutants := []struct {
		name         string
		mutations    engine.Mutations
		sqlMutations exec.Mutations
		workloads    []sim.Workload
		// sql runs the workloads through the SQL layer.
		sql bool
		// maintainers to run; the history checkers alone must catch the mutants that run without.
		maintainers int
		// detectedBy is a substring of the anomaly that must be among those found.
		detectedBy string
	}{
		// Without CAS a stale manifest write drops whole commits, which conserves money, so the bank
		// checker cannot see it; the list-append checker can.
		{name: "commit without CAS", mutations: engine.Mutations{CommitWithoutCAS: true}, workloads: []sim.Workload{sim.ListAppend}},
		{name: "commit without CAS", mutations: engine.Mutations{CommitWithoutCAS: true}, workloads: []sim.Workload{sim.ListAppend}, maintainers: 2},
		{name: "GC ignoring the grace period", mutations: engine.Mutations{GCIgnoreGrace: true}, workloads: []sim.Workload{sim.ListAppend, sim.Bank}, maintainers: 2, detectedBy: "grace period"},
		{name: "serving the cache without a freshness check", mutations: engine.Mutations{SkipFreshnessCheck: true}, workloads: []sim.Workload{sim.ListAppend}, detectedBy: "cycle"},
		{name: "acking before the write is confirmed", mutations: engine.Mutations{AckBeforeConfirm: true}, workloads: []sim.Workload{sim.ListAppend}},
		// A rebased compaction that shadows the commits it rebased on resurrects the old values of
		// the keys it merged. The engine list-append workload rewrites a list under one key, so a
		// resurrected list loses appends. The bank workload cannot see it, because the resurrected
		// balances are a consistent older state that conserves money, and the SQL list-append
		// workload cannot either, because it only inserts rows under new keys, which a compacted
		// segment does not contain.
		{name: "the compacted segment ahead of newer ones after a rebase", mutations: engine.Mutations{CompactedSegmentAhead: true}, workloads: []sim.Workload{sim.ListAppend}, maintainers: 2},
		// The engine mutants are also visible through SQL.
		{name: "commit without CAS", mutations: engine.Mutations{CommitWithoutCAS: true}, workloads: []sim.Workload{sim.ListAppend}, sql: true},
		{name: "GC ignoring the grace period", mutations: engine.Mutations{GCIgnoreGrace: true}, workloads: []sim.Workload{sim.ListAppend, sim.Bank}, sql: true, maintainers: 2, detectedBy: "grace period"},
		{name: "serving the cache without a freshness check", mutations: engine.Mutations{SkipFreshnessCheck: true}, workloads: []sim.Workload{sim.ListAppend}, sql: true, detectedBy: "cycle"},
		{name: "acking before the write is confirmed", mutations: engine.Mutations{AckBeforeConfirm: true}, workloads: []sim.Workload{sim.ListAppend}, sql: true},
		// The SQL layer's own mutants. A fresh snapshot per statement lets an audit straddle a
		// transfer and lets a list reader see two different states.
		{name: "a fresh snapshot per statement", sqlMutations: exec.Mutations{FreshSnapshotPerStatement: true}, workloads: []sim.Workload{sim.ListAppend, sim.Bank}, sql: true},
		// Retrying an autocommit statement after an unknown commit outcome needs a process that
		// keeps running after the outcome became unknown, which outages provide. When the first
		// attempt landed, the retry fails, on the duplicate primary key or on the outage, and the
		// statement reports a definite failure for a write that is in the database. Only the
		// list-append workload has autocommit writes.
		{name: "retrying an autocommit statement after an unknown commit outcome", sqlMutations: exec.Mutations{RetryAfterUnknownCommit: true}, workloads: []sim.Workload{sim.ListAppend}, sql: true, detectedBy: "failed"},
	}
	for _, m := range mutants {
		for _, w := range m.workloads {
			api := "engine"
			if m.sql {
				api = "SQL"
			}
			t.Run(fmt.Sprintf("%s is detected by the %v workload through %s with %d maintainers", m.name, w, api, m.maintainers), func(t *testing.T) {
				start := time.Now()
				const maxSeeds = 20
				for seed := uint64(1); seed <= maxSeeds; seed++ {
					cfg := sim.DefaultConfig(seed)
					cfg.Workload = w
					cfg.SQL = m.sql
					cfg.Mutations = m.mutations
					cfg.SQLMutations = m.sqlMutations
					cfg.Maintainers = m.maintainers
					res := sim.Run(cfg)
					if !res.Failed() {
						continue
					}
					t.Logf("detected with seed %d after %v, %d anomalies:", seed, time.Since(start).Round(time.Millisecond), len(res.Anomalies))
					var checker, named bool
					for _, a := range res.Anomalies {
						t.Logf("  %v", a)
						var anomaly *history.Anomaly
						if errors.As(a, &anomaly) {
							checker = true
						}
						if m.detectedBy != "" && strings.Contains(a.Error(), m.detectedBy) {
							named = true
						}
					}
					if m.maintainers == 0 {
						is.True(t, checker, "a history checker found the mutant")
					}
					if m.detectedBy != "" {
						is.True(t, named, fmt.Sprintf("an anomaly mentions %q", m.detectedBy))
					}
					return
				}
				t.Fatalf("mutant %q not detected in %d seeds", m.name, maxSeeds)
			})
		}
	}
}
