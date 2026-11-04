package engine_test

import (
	"fmt"
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
	grace := 10 * time.Minute

	t.Run("compacts at the segment threshold, collects garbage after the grace period, and makes no requests in between", func(t *testing.T) {
		verify.Covers(t, "maintenance compacts at the segment threshold, deletes garbage after the grace period, and makes no requests when idle", "AC-6.1")
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		m := db.NewMaintainer(engine.MaintenanceOptions{SegmentThreshold: 4})
		for i := range 3 {
			put(t, db, fmt.Sprint("k", i), "v")
		}
		before := db.Stats().S3
		res, err := m.Step(t.Context())
		is.NotError(t, err)
		is.Equal(t, engine.StepResult{}, res, "below the threshold there is no work")
		is.Equal(t, int64(0), db.Stats().S3.Sub(before).Total())

		put(t, db, "k3", "v")
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Attempted && res.Compacted && res.Ran, fmt.Sprint(res))
		is.Equal(t, 0, res.Deleted, "the garbage is only stamped")
		s := db.Stats()
		is.Equal(t, 1, s.Segments)
		is.Equal(t, 4, s.Garbage)
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
		is.Equal(t, 4, res.Deleted)
		d := db.Stats().S3.Sub(before)
		// Lease read and write, the freshness check of GC, the listing, the deletes, the manifest
		// write, and the lease release.
		is.Equal(t, s3.Counts{Get: 1, GetConditional: 1, PutConditional: 3, Delete: 4, List: 1}, d)
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
			ms = append(ms, db.NewMaintainer(engine.MaintenanceOptions{SegmentThreshold: 4}))
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
			ms = append(ms, db.NewMaintainer(engine.MaintenanceOptions{SegmentThreshold: 4, DisableLease: true}))
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
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", SegmentThreshold: 2, LeaseDuration: lease})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", SegmentThreshold: 2, LeaseDuration: lease})
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
		m1 := db1.NewMaintainer(engine.MaintenanceOptions{Holder: "one", SegmentThreshold: 2, LeaseDuration: lease, IgnoreLeaseExpiry: true})
		m2 := db2.NewMaintainer(engine.MaintenanceOptions{Holder: "two", SegmentThreshold: 2, LeaseDuration: lease})
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

func TestMaintainer_Lease(t *testing.T) {
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
