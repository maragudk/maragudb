package engine_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
)

// world is a fake store with a clock shared by several database instances, like several processes.
type world struct {
	t     *testing.T
	clock *s3fake.VirtualClock
	fake  *s3fake.Client
	// decide scripts request outcomes once set.
	decide func(kind s3.Kind, key string) s3fake.Outcome
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, clock: s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	w.fake = s3fake.New(s3fake.WithClock(w.clock), s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
		if w.decide == nil {
			return s3fake.OutcomeNormal
		}
		return w.decide(kind, key)
	}}))
	return w
}

func (w *world) open(seed uint64, opts engine.Options) *engine.DB {
	w.t.Helper()
	opts.Client = w.fake
	opts.Prefix = "db"
	opts.Now = w.clock.Now
	opts.Rand = rand.New(rand.NewPCG(seed, seed))
	db, err := engine.Open(w.t.Context(), opts)
	is.NotError(w.t, err)
	return db
}

func put(t *testing.T, db *engine.DB, kv ...string) {
	t.Helper()
	tx, err := db.Begin(t.Context())
	is.NotError(t, err)
	for i := 0; i < len(kv); i += 2 {
		is.NotError(t, tx.Put([]byte(kv[i]), []byte(kv[i+1])))
	}
	is.NotError(t, tx.Commit(t.Context()))
}

func get(t *testing.T, db *engine.DB, key string) (string, bool) {
	t.Helper()
	tx, err := db.BeginReadOnly(t.Context())
	is.NotError(t, err)
	v, ok, err := tx.Get(t.Context(), []byte(key))
	is.NotError(t, err)
	is.NotError(t, tx.Rollback())
	return string(v), ok
}

func scan(t *testing.T, tx *engine.Tx, start, end string) map[string]string {
	t.Helper()
	var s, e []byte
	if start != "" {
		s = []byte(start)
	}
	if end != "" {
		e = []byte(end)
	}
	it := tx.Scan(t.Context(), s, e)
	out := map[string]string{}
	var last string
	for it.Next() {
		k := string(it.Key())
		is.True(t, k > last, "scan is ordered")
		last = k
		out[k] = string(it.Value())
	}
	is.NotError(t, it.Err())
	return out
}

func equalMap(t *testing.T, want, got map[string]string) {
	t.Helper()
	is.Equal(t, fmt.Sprint(want), fmt.Sprint(got))
}

func TestOpen(t *testing.T) {
	t.Run("creates the manifest once and reuses it", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		is.Equal(t, uint64(0), db1.Stats().Seq)
		put(t, db1, "a", "1")
		db2 := w.open(2, engine.Options{})
		is.Equal(t, uint64(1), db2.Stats().Seq)
		v, ok := get(t, db2, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		is.EqualSlice(t, []string{"db/manifest", w.fake.Keys()[1]}, w.fake.Keys())
	})

	t.Run("requires a client", func(t *testing.T) {
		_, err := engine.Open(t.Context(), engine.Options{})
		is.True(t, err != nil)
	})
}

func TestTx(t *testing.T) {
	t.Run("reads its own writes and sees them after commit", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		_, ok, err := tx.Get(t.Context(), []byte("a"))
		is.NotError(t, err)
		is.True(t, !ok)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		v, ok, err := tx.Get(t.Context(), []byte("a"))
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "1", string(v))
		is.NotError(t, tx.Commit(t.Context()))
		v2, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v2)
	})

	t.Run("deletes hide values across segments and scans skip them", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1", "b", "2", "c", "3")
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Delete([]byte("b")))
		_, ok, err := tx.Get(t.Context(), []byte("b"))
		is.NotError(t, err)
		is.True(t, !ok)
		equalMap(t, map[string]string{"a": "1", "c": "3"}, scan(t, tx, "", ""))
		is.NotError(t, tx.Commit(t.Context()))
		_, ok = get(t, db, "b")
		is.True(t, !ok)
		tx, err = db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "1", "c": "3"}, scan(t, tx, "", ""))
		equalMap(t, map[string]string{"c": "3"}, scan(t, tx, "b", ""))
		equalMap(t, map[string]string{"a": "1"}, scan(t, tx, "", "b"))
		equalMap(t, map[string]string{}, scan(t, tx, "b", "c"))
	})

	t.Run("newer segments win over older ones", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1", "b", "1")
		put(t, db, "a", "2")
		put(t, db, "b", "3", "c", "3")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "2", "b": "3", "c": "3"}, scan(t, tx, "", ""))
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "2", v)
	})

	t.Run("rollback discards writes and a done transaction rejects use", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		is.NotError(t, tx.Rollback())
		is.Error(t, engine.ErrTxDone, tx.Put([]byte("a"), []byte("1")))
		is.Error(t, engine.ErrTxDone, tx.Commit(t.Context()))
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrTxDone, err)
		_, ok := get(t, db, "a")
		is.True(t, !ok)
		is.Equal(t, int64(0), db.Stats().S3.Put+db.Stats().S3.PutConditional-1, "only the manifest creation was written")
	})

	t.Run("read-only transactions cannot write", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		is.Error(t, engine.ErrReadOnly, tx.Put([]byte("a"), []byte("1")))
		is.Error(t, engine.ErrReadOnly, tx.Delete([]byte("a")))
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("a transaction without writes commits without touching the store", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		before := db.Stats().S3
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, int64(1), db.Stats().S3.Sub(before).Total(), "only the freshness check")
	})
}

func TestTx_Commit(t *testing.T) {
	t.Run("a commit from another process is visible to a transaction begun after it", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		v, ok := get(t, db2, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		put(t, db2, "a", "2")
		v, ok = get(t, db1, "a")
		is.True(t, ok)
		is.Equal(t, "2", v)
	})

	t.Run("a snapshot does not see commits made after it began", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		put(t, db1, "a", "2")
		v, ok, err := tx.Get(t.Context(), []byte("a"))
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "1", string(v))
	})

	t.Run("concurrent write transactions conflict and the loser's writes are not applied", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		tx1, err := db1.Begin(t.Context())
		is.NotError(t, err)
		tx2, err := db2.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx1.Put([]byte("a"), []byte("1")))
		is.NotError(t, tx2.Put([]byte("b"), []byte("2")))
		is.NotError(t, tx1.Commit(t.Context()))
		err = tx2.Commit(t.Context())
		is.Error(t, engine.ErrConflict, err)
		_, ok := get(t, db1, "b")
		is.True(t, !ok)
		is.Equal(t, uint64(1), db2.Stats().Seq, "the loser learned the new manifest")
	})

	t.Run("read-only transactions never conflict", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		put(t, db1, "a", "1")
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("a write transaction that only read does not conflict", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		tx, err := db2.Begin(t.Context())
		is.NotError(t, err)
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.NotError(t, err)
		put(t, db1, "a", "1")
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("a commit whose manifest write lost its acknowledgement is reported as success exactly once", func(t *testing.T) {
		lost := false
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && !lost {
				lost = true
				return s3fake.OutcomeLostAck
			}
			return s3fake.OutcomeNormal
		}
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		is.NotError(t, tx.Commit(t.Context()))
		is.True(t, lost)
		is.Equal(t, uint64(1), db.Stats().Seq)
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
	})

	t.Run("a commit whose manifest write failed before applying is retried and succeeds", func(t *testing.T) {
		failures := 0
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && failures < 3 {
				failures++
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		put(t, db, "a", "1")
		is.Equal(t, 3, failures)
		is.Equal(t, uint64(1), db.Stats().Seq)
	})

	t.Run("a commit whose segment write lost its acknowledgement still succeeds", func(t *testing.T) {
		lost := 0
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut && lost < 2 {
				lost++
				return s3fake.OutcomeLostAck
			}
			return s3fake.OutcomeNormal
		}
		put(t, db, "a", "1")
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
	})

	t.Run("a lost acknowledgement followed by another process's commit is resolved as success", func(t *testing.T) {
		var db2 *engine.DB
		phase := 0
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 = w.open(2, engine.Options{})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			switch {
			case kind == s3.KindPutConditional && phase == 0:
				phase = 1
				return s3fake.OutcomeLostAck
			case kind == s3.KindGet && phase == 1:
				// db1 is about to read the manifest back. Another process commits first.
				phase = 2
				put(t, db2, "b", "2")
			}
			return s3fake.OutcomeNormal
		}
		tx, err := db1.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, 2, phase)
		is.Equal(t, uint64(2), db1.Stats().Seq)
		v, ok := get(t, db2, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		v, ok = get(t, db1, "b")
		is.True(t, ok)
		is.Equal(t, "2", v)
	})

	t.Run("a commit returns ErrCommitUnknown when the store stops answering after the write", func(t *testing.T) {
		started := false
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional {
				started = true
			}
			if started {
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		err = tx.Commit(t.Context())
		is.Error(t, engine.ErrCommitUnknown, err)
		is.True(t, !errors.Is(err, engine.ErrConflict))
	})
}

func TestDB_Compact(t *testing.T) {
	t.Run("merges all segments into one and keeps the data", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		for i := range 5 {
			put(t, db, fmt.Sprint("k", i), fmt.Sprint("v", i), "shared", fmt.Sprint(i))
		}
		put(t, db, "k0", "updated")
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Delete([]byte("k1")))
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, 7, db.Stats().Segments)

		is.NotError(t, db.Compact(t.Context()))
		is.Equal(t, 1, db.Stats().Segments)
		is.Equal(t, 7, db.Stats().Garbage)

		tx, err = db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"k0": "updated", "k2": "v2", "k3": "v3", "k4": "v4", "shared": "4"}, scan(t, tx, "", ""))
		is.NotError(t, db.Compact(t.Context()), "compacting a single segment is a no-op")
		is.Equal(t, 1, db.Stats().Segments)
	})

	t.Run("compacting everything deleted leaves no segments", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Delete([]byte("a")))
		is.NotError(t, tx.Commit(t.Context()))
		is.NotError(t, db.Compact(t.Context()))
		is.Equal(t, 0, db.Stats().Segments)
		_, ok := get(t, db, "a")
		is.True(t, !ok)
	})

	t.Run("conflicts with a concurrent commit and can be retried", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			// db2 has read the manifest and is writing the merged segment. Another process commits first.
			if kind == s3.KindPut {
				w.decide = nil
				put(t, db1, "c", "3")
			}
			return s3fake.OutcomeNormal
		}
		err := db2.Compact(t.Context())
		is.Error(t, engine.ErrConflict, err)
		is.NotError(t, db2.Compact(t.Context()))
		is.Equal(t, 1, db2.Stats().Segments)
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "1", "b": "2", "c": "3"}, scan(t, tx, "", ""))
	})
}

func TestDB_GC(t *testing.T) {
	grace := 10 * time.Minute

	t.Run("deletes compacted segments only after the grace period, keeping old snapshots readable", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		reader := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		old, err := reader.BeginReadOnly(t.Context())
		is.NotError(t, err)
		is.NotError(t, db.Compact(t.Context()))
		is.Equal(t, 4, len(w.fake.Keys()), "manifest, two old segments, one new")

		n, err := db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n, "first pass stamps")
		w.clock.Sleep(t.Context(), grace/2)
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n, "within grace")
		v, ok, err := old.Get(t.Context(), []byte("a"))
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "1", string(v))

		w.clock.Sleep(t.Context(), grace/2)
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, n)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
		is.Equal(t, 0, db.Stats().Garbage)

		_, _, err = old.Get(t.Context(), []byte("b"))
		is.Error(t, engine.ErrSnapshotExpired, err, "the old reader is past the grace period")
		v2, ok := get(t, reader, "b")
		is.True(t, ok)
		is.Equal(t, "2", v2)
	})

	t.Run("deletes orphans from failed commits once the manifest has moved on", func(t *testing.T) {
		crash := false
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		proc := &s3fake.Process{Name: "crasher"}
		ctx := s3fake.WithProcess(t.Context(), proc)
		crasher, err := engine.Open(ctx, engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now, Rand: rand.New(rand.NewPCG(3, 3))})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut && crash {
				crash = false
				return s3fake.OutcomeCrashAfter
			}
			return s3fake.OutcomeNormal
		}
		is.NotError(t, err)
		tx, err := crasher.Begin(ctx)
		is.NotError(t, err)
		is.NotError(t, tx.Put([]byte("a"), []byte("1")))
		crash = true
		is.Error(t, s3fake.ErrCrashed, tx.Commit(ctx))
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and the orphan")

		n, err := db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n, "the orphan may belong to a commit in flight")
		put(t, db, "b", "2")
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 1, n)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and the live segment")
		_, ok := get(t, db, "a")
		is.True(t, !ok)
	})

	t.Run("a reader older than the grace period gets ErrSnapshotExpired before reading anything", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		put(t, db, "a", "1")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrSnapshotExpired, err)
		it := tx.Scan(t.Context(), nil, nil)
		is.True(t, !it.Next())
		is.Error(t, engine.ErrSnapshotExpired, it.Err())
	})
}

func TestDB_Stats(t *testing.T) {
	t.Run("counts cache hits and misses per tier", func(t *testing.T) {
		w := newWorld(t)
		disk := cache.NewMemory(0)
		db := w.open(1, engine.Options{DiskCache: disk})
		put(t, db, "a", "1")
		s := db.Stats()
		is.Equal(t, int64(0), s.MemoryCacheMisses)
		get(t, db, "a")
		s = db.Stats()
		is.Equal(t, int64(1), s.MemoryCacheHits, "the committed block was cached")

		db2 := w.open(2, engine.Options{DiskCache: disk})
		get(t, db2, "a")
		s = db2.Stats()
		is.Equal(t, int64(1), s.MemoryCacheMisses)
		is.Equal(t, int64(1), s.DiskCacheMisses)
		get(t, db2, "a")
		s = db2.Stats()
		is.Equal(t, int64(1), s.MemoryCacheHits)

		db3 := w.open(3, engine.Options{DiskCache: disk})
		get(t, db3, "a")
		s = db3.Stats()
		is.Equal(t, int64(1), s.DiskCacheHits)
		is.Equal(t, int64(0), s.S3.Get, "served from disk")
	})

	t.Run("a stale disk-cached manifest is refreshed on open", func(t *testing.T) {
		w := newWorld(t)
		disk := cache.NewMemory(0)
		db := w.open(1, engine.Options{DiskCache: disk})
		put(t, db, "a", "1")
		other := w.open(2, engine.Options{})
		put(t, other, "a", "2")
		db2 := w.open(3, engine.Options{DiskCache: disk})
		v, ok := get(t, db2, "a")
		is.True(t, ok)
		is.Equal(t, "2", v)
	})
}

// loadKeys commits n keys in batches and compacts.
func loadKeys(t *testing.T, db *engine.DB, n int) {
	t.Helper()
	const batch = 1000
	for i := 0; i < n; i += batch {
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		for j := i; j < min(i+batch, n); j++ {
			is.NotError(t, tx.Put(fmt.Appendf(nil, "key-%08d", j), fmt.Appendf(nil, "value-%d", j)))
		}
		is.NotError(t, tx.Commit(t.Context()))
	}
	is.NotError(t, db.Compact(t.Context()))
}

func TestRequestBudgets(t *testing.T) {
	sizes := []int{100, 1_000, 10_000, 100_000}

	t.Run("a commit uses two PUTs and one GET regardless of database size", func(t *testing.T) {
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(1, engine.Options{})
			loadKeys(t, db, n)
			before := db.Stats().S3
			tx, err := db.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.Put([]byte("new"), []byte("v")))
			is.NotError(t, tx.Commit(t.Context()))
			d := db.Stats().S3.Sub(before)
			t.Logf("n=%d commit: %+v", n, d)
			is.Equal(t, s3.Counts{GetConditional: 1, Put: 1, PutConditional: 1}, d)
		}
	})

	t.Run("a transaction with a warm cache uses one conditional GET and nothing else", func(t *testing.T) {
		for _, n := range sizes[:3] {
			w := newWorld(t)
			db := w.open(1, engine.Options{})
			loadKeys(t, db, n)
			key := fmt.Appendf(nil, "key-%08d", n/2)
			get(t, db, string(key))
			before := db.Stats().S3
			tx, err := db.BeginReadOnly(t.Context())
			is.NotError(t, err)
			_, ok, err := tx.Get(t.Context(), key)
			is.NotError(t, err)
			is.True(t, ok)
			it := tx.Scan(t.Context(), key, nil)
			for i := 0; i < 10 && it.Next(); i++ {
			}
			is.NotError(t, it.Err())
			is.NotError(t, tx.Commit(t.Context()))
			d := db.Stats().S3.Sub(before)
			t.Logf("n=%d warm transaction: %+v", n, d)
			is.Equal(t, s3.Counts{GetConditional: 1}, d)
		}
	})

	t.Run("a point lookup with a cold cache uses a logarithmic number of GETs", func(t *testing.T) {
		blockSize := 1024
		for _, n := range sizes {
			w := newWorld(t)
			db := w.open(1, engine.Options{BlockSize: &blockSize})
			loadKeys(t, db, n)
			cold := w.open(2, engine.Options{BlockSize: &blockSize, MemoryCacheBytes: new(int64)})
			before := cold.Stats().S3
			v, ok := get(t, cold, fmt.Sprintf("key-%08d", n/2))
			is.True(t, ok)
			is.Equal(t, fmt.Sprint("value-", n/2), v)
			d := cold.Stats().S3.Sub(before)
			t.Logf("n=%d cold lookup: %+v", n, d)
			// One manifest read plus one block per tree level. Blocks of 1 KiB hold at least 16
			// of these entries, so the depth is at most 1 + log16(n).
			maxDepth := 1
			for m := n; m > 1; m = (m + 15) / 16 {
				maxDepth++
			}
			is.True(t, d.Get <= int64(maxDepth), fmt.Sprintf("n=%d: %d GETs, max depth %d", n, d.Get, maxDepth))
			is.Equal(t, int64(1), d.GetConditional, "one manifest freshness check")
		}
	})
}

func TestMutations(t *testing.T) {
	t.Run("CommitWithoutCAS lets a stale commit overwrite a newer one", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{Mutations: engine.Mutations{CommitWithoutCAS: true}})
		db2 := w.open(2, engine.Options{})
		tx1, err := db1.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx1.Put([]byte("a"), []byte("1")))
		put(t, db2, "b", "2")
		is.NotError(t, tx1.Commit(t.Context()), "the mutant does not notice the conflict")
		_, ok := get(t, db2, "b")
		is.True(t, !ok, "the mutant lost db2's commit")
	})

	t.Run("AckBeforeConfirm reports success for a write that never happened", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{Mutations: engine.Mutations{AckBeforeConfirm: true}})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional {
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		put(t, db, "a", "1")
		other := w.open(2, engine.Options{})
		_, ok := get(t, other, "a")
		is.True(t, !ok)
	})
}

func TestContext(t *testing.T) {
	t.Run("a cancelled context fails the request", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := db.Begin(ctx)
		is.True(t, err != nil)
	})
}
