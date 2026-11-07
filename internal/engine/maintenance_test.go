package engine_test

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/verify"
)

func TestMaintainer_Step(t *testing.T) {
	t.Parallel()

	grace := 10 * time.Minute

	t.Run("compacts when four segments are in one tier, collects garbage after the grace period, and makes no requests in between", func(t *testing.T) {
		verify.Covers(t, "maintenance compacts when four segments are in one tier, deletes garbage after the grace period, and makes no requests when idle", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 4, FullRatio: 100}})
		for i := range 3 {
			put(t, db, fmt.Sprint("k", i), "v")
		}
		before := db.Stats().S3
		res, err := m.Step(t.Context())
		is.NotError(t, err)
		is.Equal(t, engine.StepResult{}, res, "with three segments in one tier there is no work")
		is.Equal(t, int64(0), db.Stats().S3.Sub(before).Total())

		put(t, db, "k3", "v")
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Attempted && res.Compacted && res.Ran, fmt.Sprint(res))
		is.Equal(t, 0, res.Deleted, "the garbage is only stamped")
		s := db.Stats()
		is.Equal(t, 1, s.Segments)
		is.Equal(t, 5, s.Garbage, "the four segments and their garbage batch")
		is.Equal(t, int64(1), s.MaintenanceRuns)
		is.Equal(t, int64(1), s.LeaseAcquisitions)
		is.Equal(t, int64(0), s.LeaseLosses)
		is.True(t, !m.Holds(), "the lease is given up after the step")

		// An idle client with everything stamped and within the grace period costs nothing.
		before = db.Stats().S3
		for range 10 {
			res, err := m.Step(t.Context())
			is.NotError(t, err)
			is.Equal(t, engine.StepResult{}, res)
		}
		is.Equal(t, int64(0), db.Stats().S3.Sub(before).Total(), "an idle maintainer makes no requests")

		w.clock.Sleep(t.Context(), grace)
		before = db.Stats().S3
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Attempted && !res.Compacted && res.Ran, fmt.Sprint(res))
		is.Equal(t, 5, res.Deleted)
		d := db.Stats().S3.Sub(before)
		// Lease read and write, the freshness check of GC, the listings of segments and garbage
		// batches, the deletes of the segments, the manifest write, the delete of the garbage
		// batch, which this process wrote and so need not read, and the lease release.
		is.Equal(t, s3.Counts{Get: 1, GetConditional: 1, PutConditional: 3, Delete: 5, List: 2}, d)
		is.Equal(t, 0, db.Stats().Garbage)
		is.Equal(t, int64(2), db.Stats().MaintenanceRuns)

		before = db.Stats().S3
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.Equal(t, engine.StepResult{}, res)
		is.Equal(t, int64(0), db.Stats().S3.Sub(before).Total())
		for _, key := range w.fake.Keys() {
			is.True(t, !strings.HasPrefix(key, "db/seg/") || db.Stats().Segments == 1, key)
		}
	})

	t.Run("only one of several maintainers does the work of a trigger", func(t *testing.T) {
		w := newWorld(t)
		const n = 5
		var dbs []*engine.DB
		var ms []*engine.Maintainer
		for i := range n {
			db := w.open(uint64(i+1), engine.Options{GCGracePeriod: &grace})
			dbs = append(dbs, db)
			ms = append(ms, db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 4, FullRatio: 100}}))
		}
		for i := range 4 {
			put(t, dbs[0], fmt.Sprint("k", i), "v")
		}
		// Every process learns of the new segments from its next freshness check.
		for _, db := range dbs[1:] {
			get(t, db, "k0")
		}
		runs, attempts := 0, 0
		for round := range 3 {
			for i, m := range ms {
				before := dbs[i].Stats().S3
				res, err := m.Step(t.Context())
				is.NotError(t, err)
				if res.Ran {
					runs++
				}
				if res.Attempted {
					attempts++
				}
				t.Logf("round %d maintainer %d: %+v, %+v", round, i, res, dbs[i].Stats().S3.Sub(before))
			}
		}
		is.Equal(t, 1, runs, "the work of one trigger is done once")
		var acquisitions int64
		for _, db := range dbs {
			acquisitions += db.Stats().LeaseAcquisitions
		}
		is.True(t, attempts <= n, fmt.Sprint("attempts ", attempts))
		is.True(t, acquisitions <= n, fmt.Sprint("acquisitions ", acquisitions))
	})

	t.Run("without the lease every maintainer attempts the work and the conditional writes keep it correct", func(t *testing.T) {
		w := newWorld(t)
		var dbs []*engine.DB
		var ms []*engine.Maintainer
		for i := range 3 {
			db := w.open(uint64(i+1), engine.Options{GCGracePeriod: &grace})
			dbs = append(dbs, db)
			ms = append(ms, db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 4, FullRatio: 100}, DisableLease: true}))
		}
		for i := range 4 {
			put(t, dbs[0], fmt.Sprint("k", i), fmt.Sprint(i))
		}
		for _, db := range dbs[1:] {
			get(t, db, "k0")
		}
		attempts, compactions := 0, 0
		for _, m := range ms {
			res, err := m.Step(t.Context())
			is.NotError(t, err)
			if res.Attempted {
				attempts++
			}
			if res.Compacted {
				compactions++
			}
		}
		is.Equal(t, 3, attempts)
		is.Equal(t, 1, compactions)
		for _, db := range dbs {
			is.Equal(t, int64(0), db.Stats().LeaseAcquisitions)
			is.Equal(t, 1, db.Stats().Segments)
			v, ok := get(t, db, "k3")
			is.True(t, ok)
			is.Equal(t, "3", v)
		}
		is.True(t, !strings.Contains(strings.Join(w.fake.Keys(), ","), "maintenance-lease"))
	})

	t.Run("renews the lease during a long compaction and stops when another maintainer took it over", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		lease := 100 * time.Millisecond
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", Tiers: engine.Tiers{FanIn: 2}, LeaseDuration: lease})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", Tiers: engine.Tiers{FanIn: 2}, LeaseDuration: lease})
		for i := range 4 {
			put(t, db1, fmt.Sprint("k", i), "v")
		}
		// Every block read takes most of a lease, so the compaction must renew before each read
		// or the lease would expire during the merge. The other maintainer tries to take the
		// lease over before every read and must never succeed.
		reads, renewals := 0, 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			switch {
			case kind == s3.KindGet && strings.HasPrefix(key, "db/seg/"):
				reads++
				w.clock.Sleep(t.Context(), lease*3/4)
				decide := w.decide
				w.decide = nil
				ok, err := m2.Acquire(t.Context())
				is.NotError(t, err)
				is.True(t, !ok, "the lease was renewed in time")
				w.decide = decide
			case kind == s3.KindPutConditional && strings.HasSuffix(key, "maintenance-lease"):
				renewals++
			}
			return s3fake.OutcomeNormal
		}
		res, err := m1.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Compacted)
		is.True(t, reads >= 4, fmt.Sprint("reads ", reads))
		// The acquisition, a renewal before every read but the first, the renewal before the
		// manifest write, and the release.
		is.True(t, renewals >= reads+2, fmt.Sprint("lease writes ", renewals, " for ", reads, " reads"))
		is.Equal(t, int64(0), db1.Stats().LeaseLosses)
		w.decide = nil

		// Now the holder stalls past its expiry while reading, and the other maintainer takes over.
		for i := range 4 {
			put(t, db1, fmt.Sprint("k", i), "w")
		}
		get(t, db2, "k0")
		stalled := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindGet && strings.HasPrefix(key, "db/seg/") && !stalled {
				stalled = true
				w.clock.Sleep(t.Context(), 2*lease)
				ok, err := m2.Acquire(t.Context())
				is.NotError(t, err)
				is.True(t, ok, "the expired lease is taken over")
			}
			return s3fake.OutcomeNormal
		}
		_, err = m1.Step(t.Context())
		is.Error(t, engine.ErrLeaseLost, err)
		is.Equal(t, int64(1), db1.Stats().LeaseLosses)
		is.True(t, !m1.Holds())
		is.True(t, m2.Holds())
		is.Equal(t, 5, db2.Stats().Segments, "the stalled compaction did not land")
		is.NotError(t, m2.Release(t.Context()))
	})

	t.Run("a maintainer that ignores expiry keeps working on an overlapping lease, and the result is still correct", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		db2 := w.open(2, engine.Options{GCGracePeriod: &grace, MemoryCacheBytes: new(int64)})
		lease := 100 * time.Millisecond
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", Tiers: engine.Tiers{FanIn: 2}, LeaseDuration: lease, IgnoreLeaseExpiry: true})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", Tiers: engine.Tiers{FanIn: 2}, LeaseDuration: lease})
		for i := range 4 {
			put(t, db1, fmt.Sprint("k", i), fmt.Sprint(i))
		}
		get(t, db2, "k0")
		taken := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindGet && strings.HasPrefix(key, "db/seg/") && !taken {
				taken = true
				w.clock.Sleep(t.Context(), 2*lease)
				res, err := m2.Step(t.Context())
				is.NotError(t, err)
				is.True(t, res.Compacted, "the other maintainer took the lease over and compacted")
			}
			return s3fake.OutcomeNormal
		}
		res, err := m1.Step(t.Context())
		is.Error(t, engine.ErrConflict, err, "the overlapping compaction loses its conditional write")
		is.True(t, res.Attempted && !res.Compacted)
		is.Equal(t, int64(1), db1.Stats().LeaseLosses, "noticed at release")
		is.True(t, !m1.Holds())
		is.True(t, !m2.Holds(), "the other maintainer released its own lease after its step")
		ok, err := m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "the stale release did not clobber the lease: it is free for the taking")
		is.NotError(t, m2.Release(t.Context()))
		for _, db := range []*engine.DB{db1, db2} {
			for i := range 4 {
				v, ok := get(t, db, fmt.Sprint("k", i))
				is.True(t, ok)
				is.Equal(t, fmt.Sprint(i), v)
			}
			is.Equal(t, 1, db.Stats().Segments)
		}
	})
}

// amplification of compaction for commits on a database that loadKeys loaded with n keys, or an
// empty one for n of 0: the bytes that the compactions of maintenance read and write for each
// committed byte. Each commit inserts the given number of new keys, updates a random key, and
// deletes another, and maintenance then steps once. Without mutations, it checks after every step
// that fewer than four segments, the default fan-in, count in each tier, where a segment counts in
// the highest tier of itself and the newer segments.
func amplification(t *testing.T, n, commits, inserts int, mutations engine.Mutations) float64 {
	t.Helper()
	w := newWorld(t)
	db := w.open(1, engine.Options{Mutations: mutations})
	if n > 0 {
		loadKeys(t, db, n)
	}
	m := db.NewMaintainer(engine.MaintenanceOptions{DisableLease: true})
	rng := rand.New(rand.NewPCG(1, 2))
	before := db.Stats()
	most := 0
	for i := range commits {
		tx, err := db.Begin(t.Context())
		is.NotError(t, err)
		for j := range inserts {
			key := n + i*inserts + j
			is.NotError(t, tx.Put(fmt.Appendf(nil, "key-%08d", key), fmt.Appendf(nil, "value-%d", key)))
		}
		keys := n + (i+1)*inserts
		is.NotError(t, tx.Put(fmt.Appendf(nil, "key-%08d", rng.IntN(keys)), fmt.Appendf(nil, "updated-%d", i)))
		is.NotError(t, tx.Delete(fmt.Appendf(nil, "key-%08d", rng.IntN(keys))))
		is.NotError(t, tx.Commit(t.Context()))
		_, err = m.Step(t.Context())
		is.NotError(t, err)
		if mutations != (engine.Mutations{}) {
			continue
		}
		// Tier 0 is below 1 KiB, and every tier above is four times as large.
		perTier := map[int]int{}
		tier := 0
		sizes := segmentSizes(t, w)
		for _, size := range slices.Backward(sizes) {
			own := 0
			for limit := int64(1 << 10); size >= limit; limit *= 4 {
				own++
			}
			tier = max(tier, own)
			perTier[tier]++
			is.True(t, perTier[tier] < 4, fmt.Sprintf("commit %d: %d segments in tier %d: %v", i, perTier[tier], tier, sizes))
		}
		most = max(most, len(sizes))
	}
	if mutations == (engine.Mutations{}) {
		t.Logf("%d keys and %d commits: at most %d segments", n, commits, most)
	}
	s := db.Stats()
	moved := s.CompactionBytesRead + s.CompactionBytesWritten - before.CompactionBytesRead - before.CompactionBytesWritten
	return float64(moved) / float64(s.CommittedBytes-before.CommittedBytes)
}

// segmentSizes of the live segments that the manifest in the store lists, oldest first.
func segmentSizes(t *testing.T, w *world) []int64 {
	t.Helper()
	body := manifestJSON(t, w)
	var man struct {
		Segments []struct {
			Size int64 `json:"size"`
		} `json:"segments"`
	}
	is.NotError(t, json.Unmarshal(body, &man))
	var sizes []int64
	for _, seg := range man.Segments {
		sizes = append(sizes, seg.Size)
	}
	return sizes
}

func TestMaintainer_Compaction(t *testing.T) {
	t.Parallel()

	t.Run("reads and writes about as many bytes per committed byte for the same commits on a database ten times as large", func(t *testing.T) {
		verify.Covers(t, "the same 1,000 commits of an insert, an update, and a delete each, with maintenance after every commit, make compaction read and write at most 1.5 times as many bytes per committed byte on a database of 20,000 keys as on one of 2,000, and the same 300 commits with the mutant that merges every segment, at least 5 times as many", "AC-6.5")
		verify.Covers(t, "1,000 commits on databases of 2,000 and 20,000 keys, with a maintenance step after every commit, leave fewer than four segments in each tier, where a segment counts in the highest tier of itself and the newer segments", "AC-5.3")
		t.Parallel()
		small := amplification(t, 2_000, 1_000, 1, engine.Mutations{})
		large := amplification(t, 20_000, 1_000, 1, engine.Mutations{})
		t.Logf("bytes moved per committed byte: %.1f at 2,000 keys, %.1f at 20,000", small, large)
		is.True(t, large <= 1.5*small, fmt.Sprintf("%.1f and %.1f", small, large))

		small = amplification(t, 2_000, 300, 1, engine.Mutations{CompactAll: true})
		large = amplification(t, 20_000, 300, 1, engine.Mutations{CompactAll: true})
		t.Logf("with every segment merged: %.1f at 2,000 keys, %.1f at 20,000", small, large)
		is.True(t, large >= 5*small, fmt.Sprintf("%.1f and %.1f", small, large))
	})

	t.Run("reads and writes a number of bytes per committed byte that grows with the logarithm of the size, while it grows a database tenfold", func(t *testing.T) {
		verify.Covers(t, "growing an empty database by commits of ten new keys, an update, and a delete each, with maintenance after every commit, from 300 commits to 3,000, makes compaction read and write at most two bytes more per committed byte for every fourfold of the size, plus two, and leaves fewer than four segments in each tier after every maintenance step", "AC-6.5", "AC-5.3")
		t.Parallel()
		small := amplification(t, 0, 300, 10, engine.Mutations{})
		large := amplification(t, 0, 3_000, 10, engine.Mutations{})
		t.Logf("bytes moved per committed byte: %.1f after 300 commits, %.1f after 3,000", small, large)
		// Every tier is four times as large as the one below, and a merge into the next tier reads
		// and writes a byte once. Merges of the whole database add a bounded number per byte.
		is.True(t, large <= small+2*(math.Log(10)/math.Log(4)+1), fmt.Sprintf("%.1f and %.1f", small, large))
	})
}

func TestMaintainer_Lease(t *testing.T) {
	t.Parallel()

	t.Run("acquire, renew, take over after expiry, and release", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		lease := time.Minute
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", LeaseDuration: lease})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", LeaseDuration: lease})

		before := db1.Stats().S3
		ok, err := m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, s3.Counts{Get: 1, PutConditional: 1}, db1.Stats().S3.Sub(before), "a read and a write with If-None-Match")
		is.True(t, m1.Holds())
		ok, err = m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "acquiring a held lease is a no-op")

		before = db2.Stats().S3
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok, "held by another")
		is.Equal(t, s3.Counts{Get: 1}, db2.Stats().S3.Sub(before), "a loser only reads")

		w.clock.Sleep(t.Context(), lease/2)
		is.NotError(t, m1.Renew(t.Context()))
		w.clock.Sleep(t.Context(), lease*3/4)
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok, "the renewal extended the lease")

		w.clock.Sleep(t.Context(), lease/2)
		before = db2.Stats().S3
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "taken over after expiry")
		is.Equal(t, s3.Counts{Get: 1, PutConditional: 1}, db2.Stats().S3.Sub(before), "a read and a write with If-Match")
		is.Equal(t, int64(1), db2.Stats().LeaseAcquisitions)

		is.Error(t, engine.ErrLeaseLost, m1.Renew(t.Context()))
		is.True(t, !m1.Holds())
		is.Equal(t, int64(1), db1.Stats().LeaseLosses)
		is.NotError(t, m1.Release(t.Context()), "releasing a lost lease is a no-op")
		is.Error(t, engine.ErrLeaseLost, m1.Renew(t.Context()), "a lease that is not held cannot be renewed")

		is.NotError(t, m2.Release(t.Context()))
		is.True(t, !m2.Holds())
		ok, err = m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "a released lease is taken over at once")
		is.NotError(t, m1.Close(t.Context()))
		is.True(t, !m1.Holds())
	})

	t.Run("two maintainers that race for a free lease get it exactly once", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one"})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two"})
		raced := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			// Both read a missing lease; the second one writes first.
			if kind == s3.KindPutConditional && strings.HasSuffix(key, "maintenance-lease") && !raced {
				raced = true
				ok, err := m2.Acquire(t.Context())
				is.NotError(t, err)
				is.True(t, ok)
			}
			return s3fake.OutcomeNormal
		}
		ok, err := m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok)
		is.True(t, raced)
		is.True(t, m2.Holds() && !m1.Holds())
	})

	t.Run("a lost acknowledgement on acquisition, renewal, or release is resolved by reading the lease back", func(t *testing.T) {
		w := newWorld(t)
		db1 := w.open(1, engine.Options{})
		db2 := w.open(2, engine.Options{})
		lease := time.Minute
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", LeaseDuration: lease})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", LeaseDuration: lease})
		loseNext := false
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && strings.HasSuffix(key, "maintenance-lease") && loseNext {
				loseNext = false
				return s3fake.OutcomeLostAck
			}
			return s3fake.OutcomeNormal
		}
		loseNext = true
		ok, err := m1.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "the acquisition landed, and the read-back found it under this holder")
		is.True(t, m1.Holds())
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok)

		w.clock.Sleep(t.Context(), lease/2)
		loseNext = true
		is.NotError(t, m1.Renew(t.Context()), "the renewal landed and is reclaimed")
		is.Equal(t, int64(0), db1.Stats().LeaseLosses)
		w.clock.Sleep(t.Context(), lease*3/4)
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, !ok, "the reclaimed renewal counts")
		is.NotError(t, m1.Renew(t.Context()), "the ETag of the reclaimed renewal is the current one")

		loseNext = true
		is.NotError(t, m1.Release(t.Context()), "the release landed and is reclaimed")
		is.True(t, !m1.Holds(), "not held after a release, whatever happened")
		is.Equal(t, int64(0), db1.Stats().LeaseLosses)
		ok, err = m2.Acquire(t.Context())
		is.NotError(t, err)
		is.True(t, ok, "the release landed")
		is.NotError(t, m2.Release(t.Context()))
	})
}
