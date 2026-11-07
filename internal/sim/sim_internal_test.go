package sim

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
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sched"
	"maragu.dev/maragudb/internal/verify"
)

func TestClient_checkError(t *testing.T) {
	newClient := func(t *testing.T, useSQL bool) (*client, *s3fake.Client) {
		t.Helper()
		damage := false
		fake := s3fake.New(s3fake.WithFaults(s3fake.Faults{Damage: func(key string, body []byte) []byte {
			if damage {
				body[0] ^= 1
			}
			return body
		}}))
		_, err := fake.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		c := &client{run: &run{cfg: Config{SQL: useSQL}, fake: fake}, proc: &s3fake.Process{Name: "client0"}}
		c.damagedAt = fake.Damaged("client0")
		damage = true
		return c, fake
	}
	read := func(t *testing.T, c *client, fake *s3fake.Client) {
		t.Helper()
		_, err := fake.Get(s3fake.WithProcess(context.WithoutCancel(t.Context()), c.proc), "k", s3.GetOptions{})
		is.NotError(t, err)
	}
	corrupt := fmt.Errorf("%w: a block fails its checksum", engine.ErrCorrupt)

	t.Run("finds an ErrCorrupt of an operation none of whose reads the store damaged", func(t *testing.T) {
		c, _ := newClient(t, false)
		c.checkError(corrupt)
		is.Equal(t, 1, len(c.run.anoms))
		var a *Anomaly
		is.True(t, errors.As(c.run.anoms[0], &a))
		is.Equal(t, CheckerDamage, a.Checker)
	})

	t.Run("accepts an ErrCorrupt of an operation one of whose reads the store damaged", func(t *testing.T) {
		c, fake := newClient(t, true)
		read(t, c, fake)
		c.checkError(corrupt)
		is.Equal(t, 0, len(c.run.anoms))
	})

	t.Run("finds an error of no kind that the engine or the store gives through the engine, but not through SQL", func(t *testing.T) {
		c, fake := newClient(t, false)
		read(t, c, fake)
		c.checkError(errors.New("engine: reading key: unexpected EOF"))
		is.Equal(t, 1, len(c.run.anoms))
		for _, err := range []error{fmt.Errorf("x: %w", engine.ErrConflict), engine.ErrCommitUnknown, engine.ErrSnapshotExpired, s3fake.ErrInjected, s3fake.ErrUnavailable, s3fake.ErrCrashed, nil} {
			c.checkError(err)
		}
		is.Equal(t, 1, len(c.run.anoms))
		c, _ = newClient(t, true)
		c.checkError(errors.New("exec: duplicate key"))
		is.Equal(t, 0, len(c.run.anoms))
	})
}

func TestClient_maintenancePanicked(t *testing.T) {
	t.Run("counts the panic that MaintenancePanics injects, and records any other panic, so that the run fails with it", func(t *testing.T) {
		verify.Covers(t, "the simulator expects only the panic that the fault injection of maintenance panics with, and fails a run with the value and the stack of any other panic that a maintenance step recovered", "INV-MAINTENANCE-PANIC")
		c := &client{run: &run{}}
		c.maintenancePanicked(engine.ErrInjectedPanic, []byte("stack"))
		is.Equal(t, 1, c.run.stats.MaintenancePanics)
		is.True(t, c.panicked)
		is.True(t, c.run.panicked == nil)

		c.maintenancePanicked(errors.New("a bug"), []byte("the stack"))
		is.Equal(t, 1, c.run.stats.MaintenancePanics)
		is.True(t, c.run.panicked != nil)
		is.Equal(t, "a bug", fmt.Sprint(c.run.panicked.Value))
		is.Equal(t, "the stack", string(c.run.panicked.Stack))
		func() {
			defer func() {
				p := recover()
				is.True(t, p == c.run.panicked, fmt.Sprint(p))
			}()
			c.run.repanic()
		}()
	})
}

// checkers of the anomalies of r.
func checkers(r *run) []string {
	var names []string
	for _, err := range r.anoms {
		var a *Anomaly
		if errors.As(err, &a) {
			names = append(names, a.Checker)
		}
	}
	return names
}

func TestClient_record(t *testing.T) {
	t.Run("finds a transaction that failed although one of its manifest writes reached the store, and one with two that did, and forgets them after", func(t *testing.T) {
		c := &client{run: &run{}}
		c.landed = map[string]bool{"m1": true}
		c.record(history.Tx{Outcome: history.Failed})
		is.EqualSlice(t, []string{CheckerDurability}, checkers(c.run))
		c.record(history.Tx{Outcome: history.Failed})
		is.Equal(t, 1, len(c.run.anoms))

		c.landed = map[string]bool{"m1": true, "m2": true}
		c.record(history.Tx{Outcome: history.OK})
		is.EqualSlice(t, []string{CheckerDurability, CheckerDurability}, checkers(c.run))
	})

	t.Run("accepts a transaction that succeeded or has an unknown outcome with one manifest write that reached the store, and a failed one with none", func(t *testing.T) {
		c := &client{run: &run{}}
		for _, outcome := range []history.Outcome{history.OK, history.Unknown} {
			c.landed = map[string]bool{"m1": true}
			c.record(history.Tx{Outcome: outcome})
		}
		c.record(history.Tx{Outcome: history.Failed})
		is.Equal(t, 0, len(c.run.anoms))
		is.Equal(t, 3, len(c.run.txs))
	})
}

func TestCommitWatch_Put(t *testing.T) {
	t.Run("records the manifest writes that reach the store, also when their answer is lost, and not those that fail or other writes", func(t *testing.T) {
		lose := false
		fake := s3fake.New(s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
			if lose {
				return s3fake.OutcomeLostAck
			}
			return s3fake.OutcomeNormal
		}}))
		c := &client{run: &run{fake: fake}}
		w := commitWatch{Client: fake, c: c}
		etag, err := w.Put(t.Context(), "db/manifest", []byte("m1"), s3.PutOptions{})
		is.NotError(t, err)
		_, err = w.Put(t.Context(), "db/seg/1", []byte("s1"), s3.PutOptions{})
		is.NotError(t, err)
		_, err = w.Put(t.Context(), "db/manifest", []byte("m2"), s3.PutOptions{IfMatch: `"other"`})
		is.Error(t, s3.ErrPreconditionFailed, err)
		lose = true
		_, err = w.Put(t.Context(), "db/manifest", []byte("m3"), s3.PutOptions{IfMatch: etag})
		is.Error(t, s3fake.ErrInjected, err)
		_, err = w.Put(t.Context(), "db/manifest", []byte("m4"), s3.PutOptions{IfMatch: etag})
		is.Error(t, s3fake.ErrInjected, err)
		is.Equal(t, 2, len(c.landed))
		is.True(t, c.landed["m1"] && c.landed["m3"])
	})
}

func TestClient_checkCached(t *testing.T) {
	t.Run("finds bytes put in the disk cache at a block's key that differ from the store's, and nothing at other keys or for objects the store does not have", func(t *testing.T) {
		fake := s3fake.New()
		_, err := fake.Put(t.Context(), "db/seg/1", []byte("abcdef"), s3.PutOptions{})
		is.NotError(t, err)
		c := &client{run: &run{fake: fake}}
		c.checkCached("db/seg/1@1+3", []byte("bcd"))
		c.checkCached("db/manifest@v3", []byte("anything"))
		c.checkCached("db/seg/2@0+3", []byte("xyz"))
		is.Equal(t, 0, len(c.run.anoms))
		c.checkCached("db/seg/1@1+3", []byte("bcX"))
		c.checkCached("db/seg/1@4+3", []byte("ef"))
		is.EqualSlice(t, []string{CheckerCache, CheckerCache}, checkers(c.run))
	})
}

func TestRun_watchStore(t *testing.T) {
	// newRun watches a fake store with an engine on it that committed a and b in a segment each,
	// and then compacted them into one, and returns the manifests after each commit.
	newRun := func(t *testing.T, grace time.Duration) (*run, *s3fake.Client, [][]byte) {
		t.Helper()
		r := &run{cfg: Config{GracePeriod: grace}, sched: sched.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), live: map[string]bool{}, left: map[string]time.Time{}}
		r.fake = s3fake.New()
		r.fake.OnChange(r.watchStore)
		db, err := engine.Open(t.Context(), engine.Options{Client: r.fake, Prefix: "db", Now: r.sched.Now, Rand: rand.New(rand.NewPCG(1, 2))})
		is.NotError(t, err)
		var manifests [][]byte
		for _, k := range []string{"a", "b"} {
			tx, err := db.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.Put([]byte(k), []byte(k)))
			is.NotError(t, tx.Commit(t.Context()))
			body, _ := r.fake.Body("db/manifest")
			manifests = append(manifests, body)
		}
		is.NotError(t, db.Compact(t.Context()))
		is.Equal(t, 0, len(r.anoms))
		return r, r.fake, manifests
	}
	segments := func(t *testing.T, body []byte) []string {
		t.Helper()
		keys, err := engine.ManifestSegments("db/manifest", body)
		is.NotError(t, err)
		return keys
	}

	t.Run("finds a segment deleted within the grace period after it left the manifest, or while the manifest references it", func(t *testing.T) {
		r, fake, manifests := newRun(t, time.Hour)
		left := segments(t, manifests[1])
		current, _ := fake.Body("db/manifest")
		live := segments(t, current)
		is.NotError(t, fake.Delete(t.Context(), left[0]))
		is.NotError(t, fake.Delete(t.Context(), live[0]))
		is.NotError(t, fake.Delete(t.Context(), "db/seg/never-live"))
		is.EqualSlice(t, []string{CheckerGracePeriod, CheckerGracePeriod}, checkers(r))
	})

	t.Run("accepts a segment deleted a grace period after it left the manifest", func(t *testing.T) {
		r, fake, manifests := newRun(t, 0)
		is.NotError(t, fake.Delete(t.Context(), segments(t, manifests[1])[0]))
		is.Equal(t, 0, len(r.anoms))
	})

	t.Run("finds a manifest write that takes segments out of the manifest and changes what the database holds", func(t *testing.T) {
		r, fake, manifests := newRun(t, 0)
		// The manifest after the first commit, without b, written again over the compacted one.
		_, err := fake.Put(t.Context(), "db/manifest", manifests[0], s3.PutOptions{})
		is.NotError(t, err)
		is.EqualSlice(t, []string{CheckerCompaction}, checkers(r))
		is.True(t, strings.Contains(r.anoms[0].Error(), `"b"`), r.anoms[0].Error())
	})
}
