package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

// world is a fake store with a clock shared by several database instances, like several processes.
type world struct {
	t     *testing.T
	clock *s3fake.VirtualClock
	fake  *s3fake.Client
	// decide scripts request outcomes once set.
	decide func(kind s3.Kind, key string) s3fake.Outcome
	// damage scripts the bodies of reads once set.
	damage func(key string, body []byte) []byte
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, clock: s3fake.NewVirtualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	w.fake = s3fake.New(s3fake.WithClock(w.clock), s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
		if w.decide == nil {
			return s3fake.OutcomeNormal
		}
		return w.decide(kind, key)
	}, Damage: func(key string, body []byte) []byte {
		if w.damage == nil {
			return body
		}
		return w.damage(key, body)
	}}))
	return w
}

func (w *world) open(seed uint64, opts engine.Options) *engine.DB {
	w.t.Helper()
	opts.Client = w.fake
	opts.Prefix = "db"
	opts.Now = w.clock.Now
	opts.Sleep = w.clock.Sleep
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

// manifestJSON of the manifest in the store, without its seal of a magic number and a checksum.
func manifestJSON(t *testing.T, w *world) []byte {
	t.Helper()
	body, ok := w.fake.Body("db/manifest")
	is.True(t, ok)
	is.True(t, len(body) > 8 && string(body[:4]) == "mdbm", "the manifest is sealed")
	return body[8:]
}

// garbageChain as the manifest in the store references it.
type garbageChain struct {
	Key       string    `json:"key"`
	Since     time.Time `json:"since"`
	StampedAt time.Time `json:"stampedAt"`
}

// garbageChains that the manifest in the store references.
func garbageChains(t *testing.T, w *world) []garbageChain {
	t.Helper()
	body := manifestJSON(t, w)
	var m struct {
		Batches []garbageChain `json:"batches"`
	}
	is.NotError(t, json.Unmarshal(body, &m))
	return m.Batches
}

// keysWith the prefix in the store.
func keysWith(w *world, prefix string) []string {
	var keys []string
	for _, key := range w.fake.Keys() {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys
}

// collectAll runs GC with db four times, a grace period apart, which stamps, expires, and deletes
// every piece of garbage, also the orphans that the first pass cannot stamp yet, and then checks
// that only the manifest and the live segments remain.
func collectAll(t *testing.T, w *world, db *engine.DB, grace time.Duration) {
	t.Helper()
	put(t, db, "collect", "all")
	for range 4 {
		_, err := db.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
	}
	is.Equal(t, 0, db.Stats().Garbage)
	is.Equal(t, db.Stats().Segments, len(keysWith(w, "db/seg/")), "only live segments")
	is.Equal(t, 0, len(keysWith(w, "db/gc/")), "no garbage batches")
}

func TestOpen(t *testing.T) {
	t.Parallel()

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

	t.Run("fails with ErrUnsupportedManifest on a manifest of version 1, which listed stamped garbage itself", func(t *testing.T) {
		verify.Covers(t, "a database whose manifest has the encoding of version 1 fails to open with an error that names the versions, and is left as it is", "AC-6.4")
		verify.Covers(t, "a database whose manifest has an encoding without checksums fails to open with an error that names the versions, and is left as it is", "INV-DAMAGE-DETECTED")
		w := newWorld(t)
		v1 := `{"version":1,"seq":3,"commitID":"c3","segments":[],"garbage":[{"key":"db/seg/0000000000000001-a","unreferencedAt":"2026-01-01T00:00:00Z"}],"commits":[{"seq":3,"id":"c3"}],"updatedAt":"2026-01-01T00:00:00Z"}`
		_, err := w.fake.Put(t.Context(), "db/manifest", []byte(v1), s3.PutOptions{})
		is.NotError(t, err)
		_, err = engine.Open(t.Context(), engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now})
		is.Error(t, engine.ErrUnsupportedManifest, err)
		is.True(t, strings.Contains(err.Error(), "the manifest has version 1, and this version of maragudb reads only version 3"), err.Error())
		body, ok := w.fake.Body("db/manifest")
		is.True(t, ok)
		is.Equal(t, v1, string(body))
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
		verify.Covers(t, "options left unset get their defaults: a memory cache and a one hour grace period", "INV-OPTIONS")
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
		verify.Covers(t, "the engine logs database creation and commits to the given slog logger", "INV-LOGGER")
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
		disk, closeDisk, err := cache.OpenDir(dir, 1<<20)
		is.NotError(t, err)
		db := w.open(1, engine.Options{DiskCache: disk})
		put(t, db, "a", "1")
		is.NotError(t, db.Close())
		closeDisk()

		disk2, closeDisk2, err := cache.OpenDir(dir, 1<<20)
		is.NotError(t, err)
		t.Cleanup(closeDisk2)
		is.True(t, disk2 != disk, "a new cache on the directory, as in a new process")
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
	t.Parallel()

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
	t.Parallel()

	t.Run("a commit from another process is visible to a transaction begun after it", func(t *testing.T) {
		verify.Covers(t, "a transaction in another process sees a commit acknowledged before it began, read from the store", "INV-STRICT-SERIALIZABLE", "INV-S3-ONLY")
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
		verify.Covers(t, "of two concurrent write transactions on different keys, the second commit gets ErrConflict and none of its writes are applied", "INV-DB-CONFLICTS", "INV-CAS-COMMIT")
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
		verify.Covers(t, "a read-only transaction commits after a concurrent write commit", "INV-READ-ONLY")
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
		verify.Covers(t, "a commit whose manifest write lost its acknowledgement is resolved as landed, reported once, and visible", "AC-1.3", "AC-3.1")
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
		verify.Covers(t, "a manifest write that failed before applying is retried and the commit succeeds", "AC-1.1")
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
		verify.Covers(t, "a lost acknowledgement followed by another process's commit is resolved from the commit records as success", "AC-1.3")
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
		verify.Covers(t, "a commit whose manifest write is never confirmed is not acknowledged and returns ErrCommitUnknown", "INV-DURABLE-ACK")
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
	t.Parallel()

	t.Run("merges all segments into one and keeps the data", func(t *testing.T) {
		verify.Covers(t, "compaction merges every segment into one and keeps every value", "AC-6.1")
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
		verify.Covers(t, "a compaction that lost to plain commits rebases on them, and their newer values stay visible", "AC-6.1")
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

	t.Run("gives up instead of rebasing once it is older than half the grace period, and its output is collected", func(t *testing.T) {
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
				w.clock.Sleep(t.Context(), grace/2)
			}
			return s3fake.OutcomeNormal
		}
		err := db2.Compact(t.Context())
		is.Error(t, engine.ErrSnapshotExpired, err)
		_, ok := get(t, db1, "c")
		is.True(t, ok)
		is.Equal(t, 3, db1.Stats().Segments, "nothing was compacted")
		is.Equal(t, 5, len(w.fake.Keys()), "manifest, three segments, and the orphan")
		_, err = db1.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		n, err := db1.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, n, "the orphan and the garbage batch that stamped it are collected after the grace period")
		is.Equal(t, 4, len(w.fake.Keys()))
	})
}

// del the keys in one commit.
func del(t *testing.T, db *engine.DB, keys ...string) {
	t.Helper()
	tx, err := db.Begin(t.Context())
	is.NotError(t, err)
	for _, key := range keys {
		is.NotError(t, tx.Delete([]byte(key)))
	}
	is.NotError(t, tx.Commit(t.Context()))
}

// putOldest commits 50 keys, k00 to k49, of about 20 bytes each, as the oldest segment, which is
// in a far higher tier than segments of a key or two with [smallTiers].
func putOldest(t *testing.T, db *engine.DB) {
	t.Helper()
	var kv []string
	for i := range 50 {
		kv = append(kv, fmt.Sprintf("k%02d", i), fmt.Sprintf("old-value-%02d", i))
	}
	put(t, db, kv...)
}

// smallTiers put a segment of one key and a value of 20 bytes in tier 1, and one with a shorter
// value or a tombstone in tier 0, and never merge the whole database.
var smallTiers = engine.Tiers{FanIn: 2, BaseBytes: 18, FullRatio: 100}

// segmentKeys of the live segments that the manifest in the store lists, oldest first.
func segmentKeys(t *testing.T, w *world) []string {
	t.Helper()
	body := manifestJSON(t, w)
	var m struct {
		Segments []struct {
			Key string `json:"key"`
		} `json:"segments"`
	}
	is.NotError(t, json.Unmarshal(body, &m))
	var keys []string
	for _, seg := range m.Segments {
		keys = append(keys, seg.Key)
	}
	return keys
}

// bodySize of an object in the store.
func bodySize(t *testing.T, w *world, key string) int64 {
	t.Helper()
	body, ok := w.fake.Body(key)
	is.True(t, ok, key)
	return int64(len(body))
}

// state of the whole database as a fresh process without a cache reads it.
func state(t *testing.T, w *world) map[string]string {
	t.Helper()
	fresh := w.open(99, engine.Options{MemoryCacheBytes: new(int64)})
	tx, err := fresh.BeginReadOnly(t.Context())
	is.NotError(t, err)
	return scan(t, tx, "", "")
}

// oldState of the database that putOldest writes, with the given changes, where an empty value
// is a deleted key.
func oldState(changes ...string) map[string]string {
	m := map[string]string{}
	for i := range 50 {
		m[fmt.Sprintf("k%02d", i)] = fmt.Sprintf("old-value-%02d", i)
	}
	for i := 0; i < len(changes); i += 2 {
		if changes[i+1] == "" {
			delete(m, changes[i])
			continue
		}
		m[changes[i]] = changes[i+1]
	}
	return m
}

func TestDB_CompactTiers(t *testing.T) {
	t.Parallel()

	t.Run("merges segments that exclude the oldest one, keeps their tombstones, which still hide older values, and counts the bytes it reads and writes", func(t *testing.T) {
		verify.Covers(t, "a tiered compaction of the two newest of three segments keeps the tombstone of one of them, which still hides the value in the oldest, and counts as read and written the bytes of its input and output objects", "AC-6.1", "AC-6.5")
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		putOldest(t, db)
		del(t, db, "k01")
		put(t, db, "k02", "new")
		before := segmentKeys(t, w)
		read := bodySize(t, w, before[1]) + bodySize(t, w, before[2])

		merged, err := db.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err)
		is.True(t, merged)
		after := segmentKeys(t, w)
		is.Equal(t, 2, len(after))
		is.Equal(t, before[0], after[0], "the oldest segment stays")
		is.Equal(t, read, db.Stats().CompactionBytesRead)
		is.Equal(t, bodySize(t, w, after[1]), db.Stats().CompactionBytesWritten)
		is.Equal(t, 2, db.Stats().Garbage)
		equalMap(t, oldState("k01", "", "k02", "new"), state(t, w))

		merged, err = db.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err)
		is.True(t, !merged, "nothing is due")
		is.NotError(t, db.Compact(t.Context()))
		equalMap(t, oldState("k01", "", "k02", "new"), state(t, w))
	})

	t.Run("puts each merged segment in the place of the merged ones, around a segment that it leaves alone, and merges several windows in one compaction", func(t *testing.T) {
		verify.Covers(t, "a tiered compaction that merges two windows of segments, one on each side of a segment that it leaves alone, keeps the older values shadowed by the merged segments, and the merged segments shadowed by newer ones", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		putOldest(t, db)
		// Tiers: far above 1, then 1, 1, 1, 0, 0. Three segments of tier 1 do not fit in tier 2, so
		// the first two merge, and the third stays between them and the merge of the last two.
		put(t, db, "k01", "aaaaaaaaaaaaaaaaaaaa")
		put(t, db, "k02", "bbbbbbbbbbbbbbbbbbbb")
		put(t, db, "k01", "cccccccccccccccccccc")
		del(t, db, "k02")
		put(t, db, "k03", "d")
		before := segmentKeys(t, w)

		merged, err := db.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err)
		is.True(t, merged)
		after := segmentKeys(t, w)
		is.Equal(t, 4, len(after))
		is.Equal(t, before[0], after[0])
		is.Equal(t, before[3], after[2], "the segment between the windows stays")
		equalMap(t, oldState("k01", "cccccccccccccccccccc", "k02", "", "k03", "d"), state(t, w))
	})

	t.Run("merges the whole database once the newer segments delete twice as many entries of the oldest one as remain, with a full ratio of 2, which reclaims the space of the deleted values", func(t *testing.T) {
		verify.Covers(t, "a tiered compaction with a full ratio of 2 merges the whole database, and drops its tombstones and deleted values, once three small segments after the oldest one hold tombstones for 34 of its 50 entries, and not before", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		putOldest(t, db)
		tiers := engine.Tiers{FanIn: 4, BaseBytes: 16, FullRatio: 2}
		var deleted []string
		for commit, n := range []int{11, 11, 12} {
			var keys []string
			for range n {
				keys = append(keys, fmt.Sprintf("k%02d", len(deleted)/2))
				deleted = append(deleted, keys[len(keys)-1], "")
			}
			del(t, db, keys...)
			merged, err := db.CompactTiers(t.Context(), tiers)
			is.NotError(t, err)
			is.Equal(t, commit == 2, merged, fmt.Sprint("after commit ", commit))
		}
		is.Equal(t, 1, len(segmentKeys(t, w)))
		equalMap(t, oldState(deleted...), state(t, w))
		is.Equal(t, 16, len(state(t, w)))
	})

	t.Run("rebases a merge of some segments on commits that landed meanwhile, which go after the segments it left alone", func(t *testing.T) {
		verify.Covers(t, "a tiered compaction that lost to a commit rebases on it, and the commit's value of a key that the merged segments deleted stays visible", "AC-6.1")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		putOldest(t, db1)
		del(t, db1, "k01")
		put(t, db1, "k02", "new")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				put(t, db1, "k01", "back")
			}
			return s3fake.OutcomeNormal
		}
		merged, err := db2.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err)
		is.True(t, merged)
		is.Equal(t, int64(1), db2.Stats().CompactionRebases)
		is.Equal(t, 3, len(segmentKeys(t, w)), "the oldest, the merged, and the newer segment")
		equalMap(t, oldState("k01", "back", "k02", "new"), state(t, w))
	})

	t.Run("fails with ErrConflict when another compaction merged segments meanwhile, and can be retried", func(t *testing.T) {
		verify.Covers(t, "a tiered compaction that lost to a compaction of other segments fails with ErrConflict, and the other compaction and the commits after it keep their values", "AC-6.1")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		putOldest(t, db1)
		del(t, db1, "k01")
		put(t, db1, "k02", "new")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				is.NotError(t, db1.Compact(t.Context()))
				put(t, db1, "k03", "x")
				put(t, db1, "k04", "y")
			}
			return s3fake.OutcomeNormal
		}
		_, err := db2.CompactTiers(t.Context(), smallTiers)
		is.Error(t, engine.ErrConflict, err)
		want := oldState("k01", "", "k02", "new", "k03", "x", "k04", "y")
		equalMap(t, want, state(t, w))
		merged, err := db2.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err)
		is.True(t, merged)
		equalMap(t, want, state(t, w))
	})
}

func TestDB_GC(t *testing.T) {
	t.Parallel()

	grace := 10 * time.Minute

	t.Run("deletes compacted segments only after the grace period, keeping old snapshots readable", func(t *testing.T) {
		verify.Covers(t, "GC deletes compacted segments only after the grace period, and a snapshot taken before compaction stays readable until then", "AC-6.2")
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
		is.Equal(t, 3, n, "the two segments and the garbage batch")
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
		is.Equal(t, 0, db.Stats().Garbage)

		_, _, err = old.Get(t.Context(), []byte("b"))
		is.Error(t, engine.ErrSnapshotExpired, err, "the old reader is past the grace period")
		v2, ok := get(t, reader, "b")
		is.True(t, ok)
		is.Equal(t, "2", v2)
	})

	t.Run("stamps orphans from failed commits once the manifest has moved on and deletes them after the grace period", func(t *testing.T) {
		verify.Covers(t, "segments of failed commits are stamped and deleted after the grace period, so failed commits do not grow the store", "AC-6.1")
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
		is.Equal(t, 2, db.Stats().Garbage, "the orphan and its garbage batch")
		is.Equal(t, 4, len(w.fake.Keys()), "manifest, the live segment, the orphan, and the garbage batch")
		w.clock.Sleep(t.Context(), grace)
		n, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, n)
		is.Equal(t, 0, db.Stats().Garbage)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and the live segment")
		_, ok := get(t, db, "a")
		is.True(t, !ok)
	})

	t.Run("never deletes the output of a compaction that rebases within the grace period", func(t *testing.T) {
		verify.Covers(t, "GC never deletes the segment of a compaction that is still rebasing within the grace period", "AC-6.2")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				// db2's merged segment is written. Before its manifest write, a commit lands and
				// GC runs twice almost half a grace period apart, the most a compaction that still
				// writes its manifest can be delayed.
				put(t, db1, "c", "3")
				_, err := db1.GC(t.Context())
				is.NotError(t, err)
				w.clock.Sleep(t.Context(), grace/2-time.Second)
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
		// The stamp from before stays in its garbage batch, and when the batch expires, GC keeps
		// the output, because the manifest it read has it as a live segment.
		_, err = db1.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		_, err = db1.GC(t.Context())
		is.NotError(t, err)
		v, ok := get(t, db1, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
	})

	t.Run("reapplies a pass on top of commits that landed meanwhile, without stamping their garbage", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		is.NotError(t, db1.Compact(t.Context()))
		is.Equal(t, 2, db1.Stats().Garbage)
		interfered := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			// Before each of the first two manifest writes of the GC pass, another process commits
			// and compacts, which adds garbage that this pass must not stamp with its older clock.
			if kind == s3.KindPutConditional && strings.HasSuffix(key, "manifest") && interfered < 2 {
				interfered++
				decide := w.decide
				w.decide = nil
				put(t, db2, "c", fmt.Sprint(interfered))
				is.NotError(t, db2.Compact(t.Context()))
				w.decide = decide
			}
			return s3fake.OutcomeNormal
		}
		n, err := db1.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, n)
		is.Equal(t, 2, interfered)
		w.decide = nil
		is.Equal(t, 7, db1.Stats().Garbage, "the two merged first and their garbage batch, plus two per later compaction")

		w.clock.Sleep(t.Context(), grace)
		n, err = db1.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 3, n, "only the garbage stamped by the rebased pass has expired, with its batch")
		is.Equal(t, 5, db1.Stats().Garbage)
		w.clock.Sleep(t.Context(), grace)
		n, err = db1.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 5, n)
		v, ok := get(t, db2, "c")
		is.True(t, ok)
		is.Equal(t, "2", v)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
	})

	t.Run("a reader older than the grace period gets ErrSnapshotExpired before reading anything", func(t *testing.T) {
		verify.Covers(t, "a transaction older than the grace period gets ErrSnapshotExpired, never data", "AC-6.3")
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

	t.Run("stamps the garbage of passes less than an eighth of the grace period apart into one chain of garbage batches, so the manifest references at most ten chains", func(t *testing.T) {
		verify.Covers(t, "with a compaction and a GC pass every 30 seconds for two hours and a one hour grace period, the manifest never references more than ten chains of garbage batches, each spanning less than an eighth of the grace period, and every chain is deleted with its garbage once its latest stamp is a grace period old", "AC-6.4", "AC-6.1")
		hour := time.Hour
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &hour})
		most := 0
		for i := range 240 {
			put(t, db, "a", fmt.Sprint(i))
			put(t, db, "b", fmt.Sprint(i))
			is.NotError(t, db.Compact(t.Context()))
			_, err := db.GC(t.Context())
			is.NotError(t, err)
			chains := garbageChains(t, w)
			most = max(most, len(chains))
			for _, c := range chains {
				is.True(t, c.StampedAt.Sub(c.Since) < hour/8, fmt.Sprintf("chain from %v to %v", c.Since, c.StampedAt))
				is.True(t, w.clock.Now().Sub(c.StampedAt) < hour+30*time.Second, "a chain is deleted at the first pass after it expires")
			}
			w.clock.Sleep(t.Context(), 30*time.Second)
		}
		is.True(t, most >= 8 && most <= 10, fmt.Sprint("at most ", most, " chains"))
		is.True(t, db.Stats().Garbage > 400, fmt.Sprint(db.Stats().Garbage, " garbage objects of the last hour"))
		w.clock.Sleep(t.Context(), hour)
		_, err := db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 0, len(garbageChains(t, w)))
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "239", v)
	})

	t.Run("deletes the garbage batches of an expired chain only after the manifest dropped it, so the next pass completes a pass whose delete failed", func(t *testing.T) {
		verify.Covers(t, "when a delete of expired garbage fails, its chain of garbage batches stays in the manifest with its batch objects, and the next pass, in another process, deletes the rest", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		is.NotError(t, db.Compact(t.Context()))
		_, err := db.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		failed := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindDelete && strings.HasPrefix(key, "db/seg/") && !failed {
				failed = true
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		n, err := db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 1, n, "one of the two expired segments")
		is.Equal(t, 1, len(garbageChains(t, w)), "the chain stays")
		is.Equal(t, 1, len(keysWith(w, "db/gc/")), "with its batch")
		other := w.open(2, engine.Options{GCGracePeriod: &grace})
		n, err = other.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, n, "the other segment and the batch")
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
	})

	t.Run("sweeps the garbage batch of a pass that crashed before its manifest write as an orphan", func(t *testing.T) {
		verify.Covers(t, "a garbage batch whose GC pass crashed before its manifest write is stamped as an orphan and deleted after the grace period", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		is.NotError(t, db.Compact(t.Context()))
		proc := &s3fake.Process{Name: "crasher"}
		ctx := s3fake.WithProcess(t.Context(), proc)
		crasher, err := engine.Open(ctx, engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now, GCGracePeriod: &grace, Rand: rand.New(rand.NewPCG(3, 3))})
		is.NotError(t, err)
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/manifest" {
				w.decide = nil
				return s3fake.OutcomeCrashBefore
			}
			return s3fake.OutcomeNormal
		}
		_, err = crasher.GC(ctx)
		is.Error(t, s3fake.ErrCrashed, err)
		is.Equal(t, 1, len(keysWith(w, "db/gc/")), "the batch of the crashed pass")
		is.Equal(t, 0, len(garbageChains(t, w)))
		collectAll(t, w, db, grace)
	})

	t.Run("writes its garbage batch again when it rebases on a pass that started a chain meanwhile, and the first batch is swept", func(t *testing.T) {
		verify.Covers(t, "a GC pass whose manifest write loses to another pass that started a chain of garbage batches joins that chain with a new batch, and every batch and piece of garbage is collected after the grace period", "AC-6.1", "AC-6.2")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		is.NotError(t, db1.Compact(t.Context()))
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/manifest" {
				w.decide = nil
				put(t, db2, "c", "3")
				is.NotError(t, db2.Compact(t.Context()))
				_, err := db2.GC(t.Context())
				is.NotError(t, err)
			}
			return s3fake.OutcomeNormal
		}
		_, err := db1.GC(t.Context())
		is.NotError(t, err)
		chains := garbageChains(t, w)
		is.Equal(t, 1, len(chains), "db1's batch joined db2's chain")
		is.Equal(t, 3, len(keysWith(w, "db/gc/")), "db2's batch, db1's first batch, and the one that joined the chain")
		collectAll(t, w, db1, grace)
		equalMap(t, map[string]string{"a": "1", "b": "2", "c": "3", "collect": "all"}, func() map[string]string {
			tx, err := db2.BeginReadOnly(t.Context())
			is.NotError(t, err)
			return scan(t, tx, "", "")
		}())
	})

	t.Run("gives up instead of rebasing once it is older than half the grace period, and its garbage batch is collected", func(t *testing.T) {
		verify.Covers(t, "a GC pass older than half the grace period writes no manifest that references its garbage batch, which is then swept as an orphan", "AC-6.2", "AC-6.1")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		is.NotError(t, db1.Compact(t.Context()))
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/manifest" {
				w.decide = nil
				put(t, db2, "c", "3")
				w.clock.Sleep(t.Context(), grace/2)
			}
			return s3fake.OutcomeNormal
		}
		_, err := db1.GC(t.Context())
		is.Error(t, engine.ErrSnapshotExpired, err)
		is.Equal(t, 0, len(garbageChains(t, w)))
		is.Equal(t, 1, len(keysWith(w, "db/gc/")))
		collectAll(t, w, db2, grace)
	})

	t.Run("runs passes of one process at the same time, which share the cache of garbage batches", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		var wg sync.WaitGroup
		for i := range 4 {
			wg.Go(func() {
				for j := range 20 {
					// Commits of the goroutines conflict with each other, which only matters here
					// for the segments and orphans they leave.
					tx, err := db.Begin(t.Context())
					is.NotError(t, err)
					is.NotError(t, tx.Put([]byte(fmt.Sprint("k", i)), []byte(fmt.Sprint(j))))
					_ = tx.Commit(t.Context())
					_ = db.Compact(t.Context())
					_, _ = db.GC(t.Context())
				}
			})
		}
		wg.Wait()
		collectAll(t, w, db, grace)
	})

	t.Run("fails with ErrConflict when another pass deleted a garbage batch after the manifest was read, and a retry succeeds", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		is.NotError(t, db1.Compact(t.Context()))
		_, err := db1.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		// db2 has no garbage batch cached, so it reads the one the manifest references, which
		// db1 deletes just before.
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindGet && strings.HasPrefix(key, "db/gc/") {
				w.decide = nil
				n, err := db1.GC(t.Context())
				is.NotError(t, err)
				is.Equal(t, 3, n)
			}
			return s3fake.OutcomeNormal
		}
		_, err = db2.GC(t.Context())
		is.Error(t, engine.ErrConflict, err)
		_, err = db2.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, len(w.fake.Keys()), "manifest and one segment")
	})

	t.Run("fails with an error that names the garbage batch when the manifest references a missing one", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		is.NotError(t, db1.Compact(t.Context()))
		_, err := db1.GC(t.Context())
		is.NotError(t, err)
		batches := keysWith(w, "db/gc/")
		is.Equal(t, 1, len(batches))
		is.NotError(t, w.fake.Delete(t.Context(), batches[0]))
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace})
		_, err = db2.GC(t.Context())
		is.True(t, err != nil && !errors.Is(err, engine.ErrConflict), fmt.Sprint(err))
		is.True(t, strings.Contains(err.Error(), "references the missing garbage batch \""+batches[0]+"\""), err.Error())
	})
}

func TestDB_ReadRetries(t *testing.T) {
	t.Parallel()

	// Each database here reads without caches, so every block read goes to the store.
	cold := func(t *testing.T, w *world, seed uint64) *engine.DB {
		t.Helper()
		return w.open(seed, engine.Options{MemoryCacheBytes: new(int64), Sleep: w.clock.Sleep})
	}
	isSegmentGet := func(kind s3.Kind, key string) bool {
		return kind == s3.KindGet && strings.HasPrefix(key, "db/seg/")
	}

	t.Run("retries a block read that fails transiently, backing off on the clock", func(t *testing.T) {
		verify.Covers(t, "a block read that fails transiently is retried with backoff on the clock and succeeds", "AC-1.1")
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

// damagedCache flips a bit of every value it returns while damaged is set.
type damagedCache struct {
	cache.Cache
	damaged bool
}

func (c *damagedCache) Get(key string) ([]byte, bool) {
	v, ok := c.Cache.Get(key)
	if ok && c.damaged && len(v) > 0 {
		v = bytes.Clone(v)
		v[len(v)/2] ^= 4
	}
	return v, ok
}

func TestDB_Damage(t *testing.T) {
	t.Parallel()

	flip := func(key string, body []byte) []byte {
		body[len(body)/2] ^= 1
		return body
	}

	t.Run("a block read from the store with damaged bytes fails with ErrCorrupt, is cached nowhere, and a later read of whole bytes succeeds", func(t *testing.T) {
		verify.Covers(t, "a read of a block whose bytes the store damaged fails with ErrCorrupt and caches nothing, and the next read of whole bytes succeeds", "INV-DAMAGE-DETECTED")
		w := newWorld(t)
		put(t, w.open(1, engine.Options{}), "a", "1")
		disk := cache.NewMemory(0)
		db := w.open(2, engine.Options{DiskCache: disk})
		w.damage = func(key string, body []byte) []byte {
			if strings.HasPrefix(key, "db/seg/") {
				return flip(key, body)
			}
			return body
		}
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrCorrupt, err)
		is.True(t, strings.Contains(err.Error(), "fails its checksum"), err.Error())
		is.Equal(t, 1, disk.Len(), "only the manifest is in the disk cache")
		w.damage = nil
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		is.Equal(t, 2, disk.Len())
	})

	t.Run("a manifest read from the store with damaged bytes fails the transaction with ErrCorrupt, and a later one succeeds", func(t *testing.T) {
		verify.Covers(t, "a transaction whose freshness read gets a manifest that the store damaged fails with ErrCorrupt, and keeps the manifest it had", "INV-DAMAGE-DETECTED")
		w := newWorld(t)
		writer := w.open(1, engine.Options{})
		put(t, writer, "a", "1")
		db := w.open(2, engine.Options{})
		put(t, writer, "a", "2")
		w.damage = func(key string, body []byte) []byte {
			if key == "db/manifest" {
				return flip(key, body)
			}
			return body
		}
		_, err := db.BeginReadOnly(t.Context())
		is.Error(t, engine.ErrCorrupt, err)
		w.damage = nil
		v, _ := get(t, db, "a")
		is.Equal(t, "2", v)
	})

	t.Run("a database whose manifest in the store is damaged fails to open with ErrCorrupt and is left as it is", func(t *testing.T) {
		w := newWorld(t)
		put(t, w.open(1, engine.Options{}), "a", "1")
		body, _ := w.fake.Body("db/manifest")
		_, err := w.fake.Put(t.Context(), "db/manifest", flip("", bytes.Clone(body)), s3.PutOptions{})
		is.NotError(t, err)
		_, err = engine.Open(t.Context(), engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now})
		is.Error(t, engine.ErrCorrupt, err)
		after, _ := w.fake.Body("db/manifest")
		is.True(t, !bytes.Equal(body, after) && len(body) == len(after), "the damaged manifest is still there")
	})

	t.Run("a GC pass that reads a damaged garbage batch fails with ErrCorrupt, and a later one succeeds", func(t *testing.T) {
		verify.Covers(t, "a GC pass whose read of a garbage batch gets damaged bytes fails with ErrCorrupt and deletes nothing, and a later pass collects the garbage", "INV-DAMAGE-DETECTED")
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		put(t, db, "a", "2")
		is.NotError(t, db.Compact(t.Context()))
		_, err := db.GC(t.Context())
		is.NotError(t, err)
		is.True(t, len(keysWith(w, "db/gc/")) == 1, "the stamped garbage is in a batch")
		w.clock.Sleep(t.Context(), 2*time.Hour)
		fresh := w.open(2, engine.Options{})
		w.damage = func(key string, body []byte) []byte {
			if strings.HasPrefix(key, "db/gc/") {
				return flip(key, body)
			}
			return body
		}
		n, err := fresh.GC(t.Context())
		is.Error(t, engine.ErrCorrupt, err)
		is.Equal(t, 0, n)
		w.damage = nil
		n, err = fresh.GC(t.Context())
		is.NotError(t, err)
		is.True(t, n >= 2, fmt.Sprint(n))
	})

	t.Run("damaged bytes from the memory or the disk cache are a miss, counted, and read from the store", func(t *testing.T) {
		verify.Covers(t, "a block or manifest that a cache returns damaged is refused, counted, and read from the store, which serves the right value", "INV-DAMAGE-DETECTED", "INV-CACHE-LOSS")
		w := newWorld(t)
		disk := &damagedCache{Cache: cache.NewMemory(0)}
		db := w.open(1, engine.Options{DiskCache: disk, MemoryCacheBytes: new(int64)})
		put(t, db, "a", "1")
		disk.damaged = true
		before := db.Stats().S3.Get
		v, ok := get(t, db, "a")
		is.True(t, ok)
		is.Equal(t, "1", v)
		s := db.Stats()
		is.Equal(t, int64(1), s.DiskCacheRefused)
		is.Equal(t, int64(0), s.DiskCacheHits)
		is.Equal(t, before+1, s.S3.Get, "the block came from the store")

		// A process that starts on a damaged record of the manifest reads it from the store.
		db2 := w.open(2, engine.Options{DiskCache: disk})
		is.Equal(t, int64(1), db2.Stats().DiskCacheRefused)
		v, _ = get(t, db2, "a")
		is.Equal(t, "1", v)

		mem := w.open(3, engine.Options{})
		put(t, mem, "b", "2")
		v, _ = get(t, mem, "b")
		is.Equal(t, "2", v)
		is.Equal(t, int64(0), mem.Stats().MemoryCacheRefused)
	})

	t.Run("checksums add no requests: a warm read makes only the freshness read, and a cold point lookup one GET per level", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{})
		put(t, db, "a", "1")
		before := db.Stats().S3
		get(t, db, "a")
		d := db.Stats().S3.Sub(before)
		is.Equal(t, int64(1), d.Get+d.GetConditional, "the freshness read only")
		cold := w.open(2, engine.Options{MemoryCacheBytes: new(int64)})
		before = cold.Stats().S3
		get(t, cold, "a")
		d = cold.Stats().S3.Sub(before)
		is.Equal(t, int64(2), d.Get+d.GetConditional, "the freshness read and the root block of the one segment")
	})
}

func TestDB_Stats(t *testing.T) {
	t.Parallel()

	t.Run("counts cache hits and misses per tier", func(t *testing.T) {
		verify.Covers(t, "Stats counts memory and disk cache hits and misses", "INV-STATS")
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
		verify.Covers(t, "a stale manifest in the disk cache is replaced by the store's newer one on open", "INV-CACHE-LOSS")
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
	t.Parallel()

	sizes := []int{100, 1_000, 10_000, 100_000}

	t.Run("a commit uses two PUTs and one GET regardless of database size", func(t *testing.T) {
		verify.Covers(t, "a commit makes one conditional GET, one PUT, and one conditional PUT at 100 to 100,000 keys", "AC-5.1")
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
			verify.CheckBudget(t, "engine commit: requests", n, d.Total(), 3)
			is.Equal(t, s3.Counts{GetConditional: 1, Put: 1, PutConditional: 1}, d)
		}
	})

	t.Run("a transaction with a warm cache uses one conditional GET and nothing else", func(t *testing.T) {
		verify.Covers(t, "a read-only transaction with a warm cache makes exactly one conditional GET at 100 to 10,000 keys", "AC-5.2")
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
			verify.CheckBudget(t, "engine warm transaction: requests", n, d.Total(), 1)
			is.Equal(t, s3.Counts{GetConditional: 1}, d)
		}
	})

	t.Run("a point lookup with a cold cache uses a logarithmic number of GETs", func(t *testing.T) {
		verify.Covers(t, "a cold point lookup makes one freshness check and at most one GET per tree level, 1 + log16(n), at 100 to 100,000 keys", "AC-5.3")
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
			verify.CheckBudget(t, "engine cold point lookup: GETs", n, d.Get, int64(maxDepth))
			is.True(t, d.Get <= int64(maxDepth), fmt.Sprintf("n=%d: %d GETs, max depth %d", n, d.Get, maxDepth))
			is.Equal(t, int64(1), d.GetConditional, "one manifest freshness check")
		}
	})
}

func TestMutations(t *testing.T) {
	t.Parallel()

	t.Run("SegmentUnchecked serves a block whose bytes the store damaged, and keeps serving it from the cache", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a block whose value the store damaged is served as a wrong value, and served again from the memory cache after the store returns whole bytes", "MUT-SEGMENT-UNCHECKED")
		w := newWorld(t)
		put(t, w.open(1, engine.Options{}), "a", "1")
		db := w.open(2, engine.Options{Mutations: engine.Mutations{SegmentUnchecked: true}})
		w.damage = func(key string, body []byte) []byte {
			if strings.HasPrefix(key, "db/seg/") {
				// The value is the byte before the flags and the length of the value, and the
				// checksum after it.
				body[len(body)-5] = '7'
			}
			return body
		}
		v, _ := get(t, db, "a")
		is.Equal(t, "7", v)
		w.damage = nil
		v, _ = get(t, db, "a")
		is.Equal(t, "7", v)
	})

	t.Run("CommitWithoutCAS lets a stale commit overwrite a newer one", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a stale commit overwrites another process's newer commit", "MUT-COMMIT-WITHOUT-CAS")
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
		verify.Covers(t, "with the mutant, a rebased compaction resurrects values that a newer commit deleted and changed", "MUT-COMPACTED-AHEAD")
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

	t.Run("GCBatchObjectsFirst leaves a chain of garbage batches without its batch after a failed delete, so GC fails for good", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a failed delete of expired garbage leaves the manifest referencing a garbage batch that is gone, and every later pass in another process fails on it and deletes nothing", "MUT-GC-BATCH-OBJECTS-FIRST")
		grace := 10 * time.Minute
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Mutations: engine.Mutations{GCBatchObjectsFirst: true}})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		is.NotError(t, db.Compact(t.Context()))
		_, err := db.GC(t.Context())
		is.NotError(t, err)
		w.clock.Sleep(t.Context(), grace)
		failed := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindDelete && strings.HasPrefix(key, "db/seg/") && !failed {
				failed = true
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		_, err = db.GC(t.Context())
		is.NotError(t, err)
		is.Equal(t, 1, len(garbageChains(t, w)))
		is.Equal(t, 0, len(keysWith(w, "db/gc/")), "the mutant deleted the batch")
		other := w.open(2, engine.Options{GCGracePeriod: &grace})
		for range 3 {
			n, err := other.GC(t.Context())
			is.True(t, err != nil && !errors.Is(err, engine.ErrConflict), fmt.Sprint(err))
			is.Equal(t, 0, n)
		}
		is.Equal(t, 2, len(keysWith(w, "db/seg/")), "the segment whose delete failed is never deleted")
	})

	t.Run("GCSweepSegmentsOnly never deletes the garbage batch of a pass that crashed", func(t *testing.T) {
		verify.Covers(t, "with the mutant, the garbage batch of a GC pass that crashed before its manifest write stays in the store after every later pass", "MUT-GC-SWEEP-SEGMENTS-ONLY")
		grace := 10 * time.Minute
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Mutations: engine.Mutations{GCSweepSegmentsOnly: true}})
		put(t, db, "a", "1")
		put(t, db, "b", "2")
		is.NotError(t, db.Compact(t.Context()))
		proc := &s3fake.Process{Name: "crasher"}
		ctx := s3fake.WithProcess(t.Context(), proc)
		crasher, err := engine.Open(ctx, engine.Options{Client: w.fake, Prefix: "db", Now: w.clock.Now, GCGracePeriod: &grace, Rand: rand.New(rand.NewPCG(3, 3))})
		is.NotError(t, err)
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/manifest" {
				w.decide = nil
				return s3fake.OutcomeCrashBefore
			}
			return s3fake.OutcomeNormal
		}
		_, err = crasher.GC(ctx)
		is.Error(t, s3fake.ErrCrashed, err)
		orphan := keysWith(w, "db/gc/")
		is.Equal(t, 1, len(orphan))
		put(t, db, "c", "3")
		for range 4 {
			_, err := db.GC(t.Context())
			is.NotError(t, err)
			w.clock.Sleep(t.Context(), grace)
		}
		is.Equal(t, 0, db.Stats().Garbage)
		is.Equal(t, fmt.Sprint(orphan), fmt.Sprint(keysWith(w, "db/gc/")), "the orphaned batch is never collected")
	})

	t.Run("GCChainKeepsFirstStamp deletes garbage before it is a grace period old", func(t *testing.T) {
		verify.Covers(t, "with the mutant, garbage that joins a chain of garbage batches nine minutes old is deleted a minute later with the chain, and a reader a minute old loses its snapshot", "MUT-GC-CHAIN-FIRST-STAMP")
		grace := 10 * time.Minute
		read := func(t *testing.T, mutations engine.Mutations) error {
			w := newWorld(t)
			db := w.open(1, engine.Options{GCGracePeriod: &grace, Mutations: mutations})
			reader := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
			put(t, db, "a", "1")
			put(t, db, "b", "2")
			is.NotError(t, db.Compact(t.Context()))
			_, err := db.GC(t.Context())
			is.NotError(t, err)
			w.clock.Sleep(t.Context(), grace-time.Minute)
			put(t, db, "c", "3")
			tx, err := reader.BeginReadOnly(t.Context())
			is.NotError(t, err)
			is.NotError(t, db.Compact(t.Context()))
			_, err = db.GC(t.Context())
			is.NotError(t, err)
			w.clock.Sleep(t.Context(), time.Minute)
			_, err = db.GC(t.Context())
			is.NotError(t, err)
			_, _, err = tx.Get(t.Context(), []byte("a"))
			return err
		}
		is.NotError(t, read(t, engine.Mutations{}))
		is.Error(t, engine.ErrSnapshotExpired, read(t, engine.Mutations{GCChainKeepsFirstStamp: true}))
	})

	t.Run("GCDeleteReferenced deletes the output of a compaction that GC stamped as an orphan before its rebase landed", func(t *testing.T) {
		verify.Covers(t, "with the mutant, GC deletes the live output of a rebased compaction when the stamp it got as an orphan expires, and stops at its own check that a deleted object is not live", "MUT-GC-DELETE-REFERENCED")
		grace := 10 * time.Minute
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64), Mutations: engine.Mutations{GCDeleteReferenced: true}})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		put(t, db1, "a", "1")
		put(t, db1, "b", "2")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			// The merged segment is written, and db2 is about to write the manifest. A commit lands
			// first, and a GC pass stamps the merged segment as an orphan.
			if kind == s3.KindPutConditional && key == "db/manifest" {
				w.decide = nil
				put(t, db1, "c", "3")
				_, err := db1.GC(t.Context())
				is.NotError(t, err)
				is.Equal(t, 1, len(garbageChains(t, w)))
			}
			return s3fake.OutcomeNormal
		}
		is.NotError(t, db2.Compact(t.Context()))
		is.Equal(t, int64(1), db2.Stats().CompactionRebases)
		w.clock.Sleep(t.Context(), grace)
		_, err := db1.GC(t.Context())
		is.True(t, err != nil && strings.Contains(err.Error(), "is referenced by the manifest"), fmt.Sprint(err))
		tx, err := db2.BeginReadOnly(t.Context())
		is.NotError(t, err)
		_, _, err = tx.Get(t.Context(), []byte("a"))
		is.Error(t, engine.ErrSnapshotExpired, err, "the compacted segment is gone")
	})

	t.Run("CompactNonAdjacent leaves a segment behind a merge of the ones around it, which then shadows newer values", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a tiered compaction merges the first and the third of the newer segments, and the second, left behind the merged one, hides the third's newer value", "MUT-COMPACT-NON-ADJACENT")
		read := func(t *testing.T, mutations engine.Mutations) map[string]string {
			w := newWorld(t)
			db := w.open(1, engine.Options{Mutations: mutations})
			putOldest(t, db)
			// Tiers 1, 1, and 0, which counts as 1 too.
			put(t, db, "k01", "aaaaaaaaaaaaaaaaaaaa")
			put(t, db, "k01", "bbbbbbbbbbbbbbbbbbbb")
			put(t, db, "k01", "c")
			merged, err := db.CompactTiers(t.Context(), smallTiers)
			is.NotError(t, err)
			is.True(t, merged)
			return state(t, w)
		}
		equalMap(t, oldState("k01", "bbbbbbbbbbbbbbbbbbbb"), read(t, engine.Mutations{CompactNonAdjacent: true}))
		equalMap(t, oldState("k01", "c"), read(t, engine.Mutations{}))
	})

	t.Run("CompactDropTombstones lets a merge that excludes the oldest segment bring a deleted value back", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a tiered compaction of the two newest of three segments drops a tombstone, and the deleted value of the oldest segment shows again", "MUT-COMPACT-DROP-TOMBSTONES")
		read := func(t *testing.T, mutations engine.Mutations) map[string]string {
			w := newWorld(t)
			db := w.open(1, engine.Options{Mutations: mutations})
			putOldest(t, db)
			del(t, db, "k01")
			put(t, db, "k02", "new")
			merged, err := db.CompactTiers(t.Context(), smallTiers)
			is.NotError(t, err)
			is.True(t, merged)
			return state(t, w)
		}
		equalMap(t, oldState("k02", "new"), read(t, engine.Mutations{CompactDropTombstones: true}))
		equalMap(t, oldState("k01", "", "k02", "new"), read(t, engine.Mutations{}))
	})

	t.Run("CompactRebaseLosesSegment loses a commit that landed during a compaction", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a tiered compaction that rebases on a commit leaves the commit's segment out of the manifest, and the committed value is gone", "MUT-COMPACT-REBASE-LOSES-SEGMENT")
		read := func(t *testing.T, mutations engine.Mutations) map[string]string {
			w := newWorld(t)
			db1 := w.open(1, engine.Options{})
			db2 := w.open(2, engine.Options{Mutations: mutations})
			putOldest(t, db1)
			del(t, db1, "k01")
			put(t, db1, "k02", "new")
			w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
				if kind == s3.KindPut {
					w.decide = nil
					put(t, db1, "k03", "committed")
				}
				return s3fake.OutcomeNormal
			}
			merged, err := db2.CompactTiers(t.Context(), smallTiers)
			is.NotError(t, err)
			is.True(t, merged)
			return state(t, w)
		}
		equalMap(t, oldState("k01", "", "k02", "new"), read(t, engine.Mutations{CompactRebaseLosesSegment: true}))
		equalMap(t, oldState("k01", "", "k02", "new", "k03", "committed"), read(t, engine.Mutations{}))
	})

	t.Run("CompactRebaseOnCompaction rebases on another compaction and loses the commits after it", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a tiered compaction rebases on a manifest in which another compaction merged every segment, and the commits after that one are gone", "MUT-COMPACT-REBASE-ON-COMPACTION")
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{Mutations: engine.Mutations{CompactRebaseOnCompaction: true}})
		putOldest(t, db1)
		del(t, db1, "k01")
		put(t, db1, "k02", "new")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut {
				w.decide = nil
				is.NotError(t, db1.Compact(t.Context()))
				put(t, db1, "k03", "x")
				put(t, db1, "k04", "y")
			}
			return s3fake.OutcomeNormal
		}
		merged, err := db2.CompactTiers(t.Context(), smallTiers)
		is.NotError(t, err, "the mutant rebases")
		is.True(t, merged)
		equalMap(t, oldState("k01", "", "k02", "new"), state(t, w))
	})

	t.Run("AckBeforeConfirm reports success for a write that never happened", func(t *testing.T) {
		verify.Covers(t, "with the mutant, a commit whose manifest write failed is reported as successful and is invisible", "MUT-ACK-BEFORE-CONFIRM")
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
	t.Parallel()

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
	t.Parallel()

	verify.Covers(t, "the engine's Tigris suite can run: the credentials are there and Tigris answers", "AC-7.1")
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
		verify.Covers(t, "on Tigris, commits of one process are visible to another, and commit and read latencies are recorded", "AC-7.1", "AC-5.4")
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
		for _, d := range commitLatency {
			verify.Record(t, verify.Fact{Latency: &verify.Latency{Name: "engine commit", Duration: d}})
		}
		for i, d := range readLatency {
			name := "engine begin and point lookup, warm"
			if i == 0 {
				name = "engine begin and point lookup, cold"
			}
			verify.Record(t, verify.Fact{Latency: &verify.Latency{Name: name, Duration: d}})
		}
		t.Logf("commit latency: %v", commitLatency)
		t.Logf("read latency (begin + point lookup, cold then warm): %v", readLatency)
		t.Logf("db1 stats: %+v", db1.Stats())
	})

	t.Run("concurrent writers conflict and retry until every write lands", func(t *testing.T) {
		verify.Covers(t, "on Tigris, concurrent writers conflict and retry until every write is in the database", "AC-7.1", "INV-DB-CONFLICTS")
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

	t.Run("the maintenance lease is acquired, renewed, and taken over after it expires", func(t *testing.T) {
		verify.Covers(t, "on Tigris, the maintenance lease is acquired, renewed, and taken over after it expires", "AC-7.1")
		db1 := open(t, 60, engine.Options{})
		db2 := open(t, 61, engine.Options{})
		// A probe measures how long acquiring and releasing takes on this run's network, and
		// the lease is sized from that, so that renewals and the takeover do not depend on a
		// fixed latency. The probe also leaves an expired lease behind, as a closed process does.
		probe := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "probe", LeaseDuration: time.Hour})
		start := time.Now()
		ok, err := probe.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "acquired a lease that did not exist")
		is.NotError(t, probe.Release(t.Context()))
		roundTrip := time.Since(start)
		lease := max(2*time.Second, 5*roundTrip)
		t.Logf("acquire and release: %v, lease: %v", roundTrip.Round(time.Millisecond), lease.Round(time.Millisecond))

		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", LeaseDuration: lease})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", LeaseDuration: lease})
		ok, err = m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "took over the expired lease")
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok, "held by the other process")
		is.NotError(t, m1.Renew(t.Context()))
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok, "still held after the renewal")

		// The renewal set the expiry a lease ahead of a clock reading before its request, so a
		// lease after it returned has passed the expiry.
		time.Sleep(lease)
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "taken over after expiry")
		is.Error(t, engine.ErrLeaseLost, m1.Renew(t.Context()))
		is.Equal(t, int64(1), db1.Stats().LeaseLosses)
		is.Equal(t, int64(1), db2.Stats().LeaseAcquisitions)
		is.NotError(t, m2.Release(t.Context()))
		ok, err = m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "a released lease is free at once")
		is.NotError(t, m1.Close(t.Context()))
	})

	t.Run("compaction and GC keep the data and bound the objects", func(t *testing.T) {
		verify.Covers(t, "on Tigris, compaction and GC keep the data and leave one segment", "AC-7.1", "AC-6.1")
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
		is.Equal(t, 4, n, "expired segments and their garbage batch were deleted")
		infos, err := client.List(t.Context(), cfg.Prefix+"/"+strings.ReplaceAll(t.Name(), "/", "-"))
		is.NotError(t, err)
		is.Equal(t, 2, len(infos), "manifest and one segment")
		tx, err := db.BeginReadOnly(t.Context())
		is.NotError(t, err)
		equalMap(t, map[string]string{"k0": "v0", "k1": "v1", "k2": "v2"}, scan(t, tx, "", ""))
	})
}
