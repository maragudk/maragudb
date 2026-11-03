package workload_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/history"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sim"
	"maragu.dev/maragudb/internal/workload"
)

func config(seed uint64, w sim.Workload, useSQL bool, faults s3fake.Faults) workload.Config {
	faults.Rand = rand.New(rand.NewPCG(seed, 1))
	if faults.ErrorRate < 1 && (faults.ErrorRate > 0 || faults.LostAckRate > 0) {
		// The engine does not retry failed reads, and the final read touches every segment, so
		// reads fail less often than writes, as on a network that is up.
		rng := rand.New(rand.NewPCG(seed, 3))
		var mu sync.Mutex
		errorRate, lostAckRate := faults.ErrorRate, faults.LostAckRate
		faults.Decide = func(kind s3.Kind, _ string) s3fake.Outcome {
			mu.Lock()
			defer mu.Unlock()
			read := kind == s3.KindGet || kind == s3.KindGetConditional || kind == s3.KindList
			switch {
			case read && rng.Float64() < errorRate/10:
				return s3fake.OutcomeError
			case !read && rng.Float64() < errorRate:
				return s3fake.OutcomeError
			case !read && rng.Float64() < lostAckRate:
				return s3fake.OutcomeLostAck
			}
			return s3fake.OutcomeNormal
		}
	}
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
				}
				t.Logf("%+v", total)
				is.True(t, total.Commits > 20, "commits")
				is.True(t, total.Conflicts > 0, "conflicts")
			})
		}
	}

	t.Run("finds the lost writes of a commit without CAS", func(t *testing.T) {
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

	t.Run("fails when the store never answers", func(t *testing.T) {
		cfg := config(1, sim.Bank, true, s3fake.Faults{ErrorRate: 1})
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		_, err := workload.Run(ctx, cfg)
		is.True(t, err != nil, "an error")
	})
}
