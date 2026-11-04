package engine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/tigristest"
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
		keys := w.fake.Keys()
		is.Equal(t, 2, len(keys))
		is.Equal(t, "db/manifest", keys[0])
		is.True(t, strings.HasPrefix(keys[1], "db/seg/"), keys[1])
	})

	t.Run("requires a client", func(t *testing.T) {
		_, err := engine.Open(t.Context(), engine.Options{})
		is.True(t, err != nil)
	})

	t.Run("rejects a non-positive block size", func(t *testing.T) {
		w := newWorld(t)
		_, err := engine.Open(t.Context(), engine.Options{Client: w.fake, BlockSize: new(int)})
		is.True(t, err != nil)
	})

	t.Run("defaults to a memory cache and a one hour grace period", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		get(t, db, "a")
		is.True(t, db.Stats().MemoryCacheHits > 0, "memory cache on by default")
		w.clock.Sleep(t.Context(), time.Hour)
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrSnapshotExpired, err)
	})

	t.Run("logs creation and commits to the given logger", func(t *testing.T) {
		w := newWorld(t)
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		db := w.open(1, engine.Options{Logger: logger})
		put(t, db, "a", "1")
		is.True(t, strings.Contains(buf.String(), "created database"), buf.String())
		is.True(t, strings.Contains(buf.String(), "committed"), buf.String())
	})

	t.Run("serves blocks and the manifest from a disk cache directory across restarts", func(t *testing.T) {
		w := newWorld(t)
		dir := t.TempDir()
		disk, err := cache.NewDisk(dir)
		is.NotError(t, err)
		db := w.open(1, engine.Options{DiskCache: disk})
		put(t, db, "a", "1")

		disk2, err := cache.NewDisk(dir)
		is.NotError(t, err)
		db2 := w.open(2, engine.Options{DiskCache: disk2, MemoryCacheBytes: new(int64)})
		before := db2.Stats().S3
		v, ok := get(t, db2, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		s := db2.Stats()
		is.Equal(t, int64(1), s.DiskCacheHits)
		is.Equal(t, s3.Counts{GetConditional: 1}, s.S3.Sub(before), "only the freshness check went to the store")
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

	t.Run("rebases on commits that only added segments, which keep shadowing the compacted one", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			// db2 has read the manifest and is writing the merged segment. Another process commits
			// twice first, deleting a key the merge kept and overwriting another.
			if kind == s3.KindPut {
				w.decide = nil
				tx, err := db1.Begin(t.Context())
				is.NotError(t, err)
				is.NotError(t, tx.Delete([]byte("a")))
				is.NotError(t, tx.Commit(t.Context()))
				put(t, db1, "b", "3", "c", "3")
			}
			return s3fake.OutcomeNormal
		}
		before := db2.Stats().S3
		is.NotError(t, db2.Compact(t.Context()))
		d := db2.Stats().S3.Sub(before)
		is.Equal(t, int64(1), d.Put, "the merged segment is written once")
		is.Equal(t, int64(1), db2.Stats().CompactionRebases)
		is.Equal(t, 3, db2.Stats().Segments, "the compacted segment and the two newer ones")
		is.Equal(t, 2, db2.Stats().Garbage)
		for _, db := range []*engine.DB{db1, db2} {
			tx, err := db.BeginReadOnly(t.Context())
			is.NotError(t, err)
			equalMap(t, map[string]string{"b": "3", "c": "3"}, scan(t, tx, "", ""))
			_, ok := get(t, db, "a")
			is.True(t, !ok, "the newer tombstone hides the compacted value")
		}
		is.NotError(t, db2.Compact(t.Context()))
		is.Equal(t, 1, db2.Stats().Segments)
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"b": "3", "c": "3"}, scan(t, tx, "", ""))
	})

	t.Run("rebases again when a commit lands during the rebase", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		phase := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			switch {
			case kind == s3.KindPut && phase == 0:
				phase = 1
				put(t, db1, "c", "3")
				phase = 2
			case kind == s3.KindPutConditional && phase == 2:
				// db2's first manifest write is about to fail. Another commit lands before its rebase.
				phase = 3
				put(t, db1, "d", "4")
				phase = 4
			case kind == s3.KindPutConditional && phase == 4:
				phase = 5
				put(t, db1, "e", "5")
				phase = 6
			}
			return s3fake.OutcomeNormal
		}
		is.NotError(t, db2.Compact(t.Context()))
		is.Equal(t, 6, phase)
		is.Equal(t, 4, db2.Stats().Segments, "the compacted segment and three newer ones")
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}, scan(t, tx, "", ""))
	})

	t.Run("fails with ErrConflict when another compaction won, and can be retried", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				is.NotError(t, db1.Compact(t.Context()))
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

	t.Run("gives up instead of rebasing once it is older than the grace period", func(t *testing.T) {
		grace := 10 * time.Minute
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				put(t, db1, "c", "3")
				// Its output may have been collected as an orphan by now.
				w.clock.Sleep(t.Context(), grace)
			}
			return s3fake.OutcomeNormal
		}
		err := db2.Compact(t.Context())
		is.Error(t, engine.ErrSnapshotExpired, err)
		_, ok := get(t, db1, "c")
		is.True(t, ok)
		is.Equal(t, 3, db1.Stats().Segments, "nothing was compacted")
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

	t.Run("stamps orphans from failed commits once the manifest has moved on and deletes them after the grace period", func(t *testing.T) {
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
		is.Equal(t, 0, db.Stats().Garbage)
		put(t, db, "b", "2")
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n, "the orphan may belong to a compaction that is about to rebase, so it is only stamped")
		is.Equal(t, 1, db.Stats().Garbage)
		is.Equal(t, 3, len(w.fake.Keys()), "manifest, the live segment, and the orphan")
		w.clock.Sleep(t.Context(), grace)
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 1, n)
		is.Equal(t, 0, db.Stats().Garbage)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and the live segment")
		_, ok := get(t, db, "a")
		is.True(t, !ok)
	})

	t.Run("never deletes the output of a compaction that rebases within the grace period", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				// db2's merged segment is written. Before its manifest write, a commit lands and
				// GC runs twice almost a grace period apart, which is the most GC may delete.
				put(t, db1, "c", "3")
				_, err := db1.GC(t.Context())
				is.NotError(t, err)
				w.clock.Sleep(t.Context(), grace-time.Second)
				_, err = db1.GC(t.Context())
				is.NotError(t, err)
			}
			return s3fake.OutcomeNormal
		}
		is.NotError(t, db2.Compact(t.Context()))
		is.Equal(t, 2, db2.Stats().Segments)
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "1", "b": "2", "c": "3"}, scan(t, tx, "", ""))
		// A GC pass after the rebase sees the output as live and the stamp from before is dropped.
		_, err = db1.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		_, err = db1.GC(t.Context())
		is.NotError(t, err)
		v, ok := get(t, db1, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
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

func TestDB_ReadRetries(t *testing.T) {
	// Each database here reads without caches, so every block read goes to the store.
	cold := func(t *testing.T, w *world, seed uint64) *engine.DB {
		t.Helper()
		return w.open(seed, engine.Options{MemoryCacheBytes: new(int64), Sleep: w.clock.Sleep})
	}
	isSegmentGet := func(kind s3.Kind, key string) bool {
		return kind == s3.KindGet && strings.HasPrefix(key, "db/seg/")
	}

	t.Run("retries a block read that fails transiently, backing off on the clock", func(t *testing.T) {
		w := newWorld(t)
		db := cold(t, w, 1)
		put(t, db, "a", "1")
		failures := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if isSegmentGet(kind, key) && failures < 3 {
				failures++
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		before := db.Stats().S3
		start := w.clock.Now()
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		is.Equal(t, 3, failures)
		is.Equal(t, int64(4), db.Stats().S3.Sub(before).Get, "three failures and a success")
		is.True(t, w.clock.Now().Sub(start) >= 70*time.Millisecond, "backed off between attempts")
	})

	t.Run("gives up after a bounded number of attempts", func(t *testing.T) {
		w := newWorld(t)
		db := cold(t, w, 1)
		put(t, db, "a", "1")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if isSegmentGet(kind, key) {
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		before := db.Stats().S3
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, s3fake.ErrInjected, err)
		is.Equal(t, int64(4), db.Stats().S3.Sub(before).Get)
	})

	t.Run("does not retry a missing object beyond the one consistent read", func(t *testing.T) {
		grace := 10 * time.Minute
		w := newWorld(t)
		// The reader runs with the mutation that lets GC delete too early, so that the engine's
		// own invariant assertion stays out of a test that deletes objects behind its back.
		db := w.open(1, engine.Options{MemoryCacheBytes: new(int64), Sleep: w.clock.Sleep, GCGracePeriod: &grace, Mutations: engine.Mutations{GCIgnoreGrace: true}})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		is.NotError(t, db.Compact(t.Context()))
		// Delete the old segments behind the reader's back. Keys sort by base sequence number,
		// so the compacted segment is the last one.
		keys := w.fake.Keys()
		for _, key := range keys[:len(keys)-1] {
			if strings.HasPrefix(key, "db/seg/") {
				is.NotError(t, w.fake.Delete(t.Context(), key))
			}
		}
		before := db.Stats().S3
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrSnapshotExpired, err)
		is.Equal(t, int64(2), db.Stats().S3.Sub(before).Get, "one eventually consistent read and one consistent read")
	})

	t.Run("does not retry once the context is cancelled", func(t *testing.T) {
		w := newWorld(t)
		db := cold(t, w, 1)
		put(t, db, "a", "1")
		ctx, cancel := context.WithCancel(t.Context())
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if isSegmentGet(kind, key) {
				cancel()
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		before := db.Stats().S3
		_, _, err = tx.Get(ctx, []byte("a"))
		is.True(t, err != nil)
		is.Equal(t, int64(1), db.Stats().S3.Sub(before).Get)
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
		is.Equal(t, int64(1), s.DiskCacheHits, "the committed block was written to the disk tier too")
		is.Equal(t, int64(0), s.S3.Get, "served from disk")
		get(t, db2, "a")
		s = db2.Stats()
		is.Equal(t, int64(1), s.MemoryCacheHits, "promoted to memory")

		db3 := w.open(3, engine.Options{DiskCache: cache.NewMemory(0)})
		before := db3.Stats().S3
		get(t, db3, "a")
		s = db3.Stats()
		is.Equal(t, int64(1), s.MemoryCacheMisses)
		is.Equal(t, int64(1), s.DiskCacheMisses)
		is.Equal(t, int64(1), s.S3.Sub(before).Get, "fetched from the store")
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

	t.Run("CompactedSegmentAhead lets a rebased compaction hide newer commits", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{Mutations: engine.Mutations{CompactedSegmentAhead: true}})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				tx, err := db1.Begin(t.Context())
				is.NotError(t, err)
				is.NotError(t, tx.Delete([]byte("a")))
				is.NotError(t, tx.Put([]byte("b"), []byte("3")))
				is.NotError(t, tx.Commit(t.Context()))
			}
			return s3fake.OutcomeNormal
		}
		is.NotError(t, db2.Compact(t.Context()))
		tx, err := db1.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"a": "1", "b": "2"}, scan(t, tx, "", "")) // the mutant resurrected the old values
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

func TestDB_Tigris(t *testing.T) {
	client, cfg := tigristest.NewClient(t)
	// Each subtest gets its own database under the run's prefix, so they do not depend on each other.
	open := func(t *testing.T, seed uint64, opts engine.Options) *engine.DB {
		t.Helper()
		opts.Client = client
		opts.Prefix = cfg.Prefix + "/" + strings.ReplaceAll(t.Name(), "/", "-")
		opts.Rand = rand.New(rand.NewPCG(seed, seed))
		db, err := engine.Open(t.Context(), opts)
		is.NotError(t, err)
		return db
	}

	t.Run("commits are visible across processes and latencies are reported", func(t *testing.T) {
		db1 := open(t, 1, engine.Options{})
		db2 := open(t, 2, engine.Options{})
		var commitLatency, readLatency []time.Duration
		for i := range 5 {
			tx, err := db1.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.Put(fmt.Appendf(nil, "key-%d", i), fmt.Appendf(nil, "value-%d", i)))
			start := time.Now()
			is.NotError(t, tx.Commit(t.Context()))
			commitLatency = append(commitLatency, time.Since(start))

			start = time.Now()
			v, ok := get(t, db2, fmt.Sprint("key-", i))
			readLatency = append(readLatency, time.Since(start))
			is.True(t, ok)
			is.Equal(t, fmt.Sprint("value-", i), v)
		}
		t.Logf("commit latency: %v", commitLatency)
		t.Logf("read latency (begin + point lookup, cold then warm): %v", readLatency)
		t.Logf("db1 stats: %+v", db1.Stats())
	})

	t.Run("concurrent writers conflict and retry until every write lands", func(t *testing.T) {
		const writers = 3
		const perWriter = 4
		var wg sync.WaitGroup
		conflicts := make([]int, writers)
		for w := range writers {
			db := open(t, uint64(10+w), engine.Options{})
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range perWriter {
					for {
						tx, err := db.Begin(t.Context())
						if err != nil {
							t.Errorf("writer %d: begin: %v", w, err)
							return
						}
						v, _, err := tx.Get(t.Context(), []byte("counter"))
						if err != nil {
							t.Errorf("writer %d: get: %v", w, err)
							return
						}
						n, _ := strconv.Atoi(string(v))
						_ = tx.Put([]byte("counter"), []byte(strconv.Itoa(n+1)))
						_ = tx.Put(fmt.Appendf(nil, "writer-%d-%d", w, i), []byte("done"))
						err = tx.Commit(t.Context())
						if err == nil {
							break
						}
						if errors.Is(err, engine.ErrConflict) {
							conflicts[w]++
							continue
						}
						t.Errorf("writer %d: commit: %v", w, err)
						return
					}
				}
			}()
		}
		wg.Wait()
		t.Logf("conflicts per writer: %v", conflicts)
		db := open(t, 99, engine.Options{})
		v, ok := get(t, db, "counter")
		is.True(t, ok)
		is.Equal(t, strconv.Itoa(writers*perWriter), v, "every increment landed exactly once")
	})

	t.Run("compaction and GC keep the data and bound the objects", func(t *testing.T) {
		// Write with the default grace period and time each commit, so the grace period below is
		// sized from this run's network latency instead of assumed.
		writer := open(t, 50, engine.Options{})
		var slowest time.Duration
		for i := range 3 {
			start := time.Now()
			put(t, writer, fmt.Sprint("k", i), fmt.Sprint("v", i))
			slowest = max(slowest, time.Since(start))
		}
		// Compaction takes about twice the requests of a commit and must finish within the grace
		// period, so five times the slowest commit leaves room for latency spikes.
		grace := max(2*time.Second, 5*slowest)
		t.Logf("slowest commit: %v, grace period: %v", slowest.Round(time.Millisecond), grace.Round(time.Millisecond))
		db := open(t, 51, engine.Options{GCGracePeriod: &grace})
		start := time.Now()
		for {
			err := db.Compact(t.Context())
			if !errors.Is(err, engine.ErrConflict) {
				is.NotError(t, err)
				break
			}
		}
		t.Logf("compaction: %v", time.Since(start).Round(time.Millisecond))
		is.Equal(t, 1, db.Stats().Segments)
		n, err := db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n, "the first pass only stamps the garbage")
		// The stamp is taken during the first pass, so a grace period after it returns has elapsed.
		time.Sleep(grace)
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 3, n, "expired segments were deleted")
		infos, err := client.List(t.Context(), cfg.Prefix+"/"+strings.ReplaceAll(t.Name(), "/", "-"))
		is.NotError(t, err)
		is.Equal(t, 2, len(infos), "manifest and one segment")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"k0": "v0", "k1": "v1", "k2": "v2"}, scan(t, tx, "", ""))
	})
}
