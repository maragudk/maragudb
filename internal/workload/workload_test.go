package workload_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/verify"
	"maragu.dev/maragudb/internal/workload"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func config(seed uint64, w sim.Workload, useSQL bool, faults s3fake.Faults) workload.Config {
	faults.Rand = rand.New(rand.NewPCG(seed, 1))
	return workload.Config{
		Client:      s3fake.New(s3fake.WithFaults(faults)),
		Prefix:      "db",
		Workload:    w,
		SQL:         useSQL,
		Clients:     5,
		TxPerClient: 30,
		Keys:        3,
		MaxTimeout:  time.Second,
		Rand:        rand.New(rand.NewPCG(seed, 2)),
	}
}

func TestRun(t *testing.T) {
	for _, useSQL := range []bool{false, true} {
		for _, w := range []sim.Workload{sim.ListAppend, sim.Bank} {
			t.Run(fmt.Sprintf("the %v workload through SQL %v finds no anomalies under errors and lost acknowledgements", w, useSQL), func(t *testing.T) {
				verify.Covers(t, "the real-store workload that the Antithesis harness runs finds no anomalies under errors and lost acknowledgements, with commits, conflicts, and background compactions", "AC-8.2")
				var total workload.Stats
				for seed := range uint64(4) {
					res, err := workload.Run(t.Context(), config(seed, w, useSQL, s3fake.Faults{ErrorRate: 0.05, LostAckRate: 0.05}))
					is.NotError(t, err)
					if len(res.Anomalies) > 0 {
						t.Fatal(res.Report())
					}
					total.Commits += res.Stats.Commits
					total.Conflicts += res.Stats.Conflicts
					total.Failed += res.Stats.Failed
					total.Compactions += res.Stats.Compactions
					total.MaintenanceRuns += res.Stats.MaintenanceRuns
					total.LeaseAcquisitions += res.Stats.LeaseAcquisitions
				}
				t.Logf("%+v", total)
				is.True(t, total.Commits > 20, "commits")
				is.True(t, total.Conflicts > 0, "conflicts")
				is.True(t, total.Compactions > 0, "background compactions")
				is.True(t, total.LeaseAcquisitions > 0, "lease acquisitions")
			})
		}
	}

	t.Run("finds the lost writes of a commit without CAS", func(t *testing.T) {
		verify.Covers(t, "the real-store workload's checkers find the lost writes of the mutant", "MUT-COMMIT-WITHOUT-CAS")
		for seed := range uint64(10) {
			cfg := config(seed, sim.ListAppend, false, s3fake.Faults{})
			cfg.Mutations = engine.Mutations{CommitWithoutCAS: true}
			res, err := workload.Run(t.Context(), cfg)
			is.NotError(t, err)
			for _, a := range res.Anomalies {
				var anomaly *history.Anomaly
				if errors.As(a, &anomaly) {
					t.Logf("detected with seed %d: %v", seed, a)
					return
				}
			}
		}
		t.Fatal("not detected in 10 seeds")
	})

	t.Run("finds snapshots lost before the grace period ends, when GC ignores it", func(t *testing.T) {
		verify.Covers(t, "the real-store workload finds snapshots lost early with the mutant", "MUT-GC-IGNORE-GRACE")
		// The run is in wall-clock time, so the seed does not decide it, and detection varies.
		for seed := range uint64(60) {
			cfg := config(seed, sim.ListAppend, seed%2 == 0, s3fake.Faults{})
			cfg.Mutations = engine.Mutations{GCIgnoreGrace: true}
			cfg.GracePeriod = time.Minute
			res, err := workload.Run(t.Context(), cfg)
			is.NotError(t, err)
			for _, a := range res.Anomalies {
				if strings.Contains(a.Error(), "grace period") {
					t.Logf("detected with seed %d: %v", seed, a)
					return
				}
			}
		}
		t.Fatal("not detected in 60 seeds")
	})

	t.Run("finds old values that shadow newer ones when a rebased compaction puts its segment ahead, through the packed lists of SQL", func(t *testing.T) {
		verify.Covers(t, "the real-store SQL list-append workload, whose packed lists update rows in place, finds the stale reads of the mutant", "MUT-COMPACTED-AHEAD")
		// The run is in wall-clock time, so the seed does not decide it, and detection varies.
		for seed := range uint64(60) {
			cfg := config(seed, sim.ListAppend, true, s3fake.Faults{})
			cfg.Mutations = engine.Mutations{CompactedSegmentAhead: true}
			res, err := workload.Run(t.Context(), cfg)
			is.NotError(t, err)
			for _, a := range res.Anomalies {
				var anomaly *history.Anomaly
				if errors.As(a, &anomaly) {
					t.Logf("detected with seed %d: %v", seed, a)
					return
				}
			}
		}
		t.Fatal("not detected in 60 seeds")
	})

	t.Run("checks the history even when the clients stopped because the context ended", func(t *testing.T) {
		cfg := config(1, sim.ListAppend, false, s3fake.Faults{})
		cfg.TxPerClient = 1 << 30
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		res, err := workload.Run(ctx, cfg)
		is.NotError(t, err)
		is.True(t, len(res.History) > 0)
		is.Equal(t, 0, len(res.Anomalies))
	})

	t.Run("reports that it gave up, with the last error, when the store never answers", func(t *testing.T) {
		cfg := config(1, sim.Bank, true, s3fake.Faults{ErrorRate: 1})
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, err := workload.Run(ctx, cfg)
		var gaveUp *workload.GaveUpError
		is.True(t, errors.As(err, &gaveUp), fmt.Sprint(err))
		is.Error(t, s3fake.ErrInjected, gaveUp.Err)
	})

}
