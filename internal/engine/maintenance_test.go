package engine_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// logRecords of a JSON log, one map per record.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var r map[string]any
		is.NotError(t, json.Unmarshal([]byte(line), &r))
		records = append(records, r)
	}
	return records
}

// number of a JSON log record's value, or zero for one that is no number.
func number(v any) float64 {
	f, _ := v.(float64)
	return f
}

// panicRecords of a JSON log: the records at the error level of the panics that maintenance
// recovered.
func panicRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, r := range logRecords(t, buf) {
		if r["level"] == "ERROR" && strings.Contains(fmt.Sprint(r["msg"]), "recovered a panic in a maintenance step") {
			records = append(records, r)
		}
	}
	return records
}

func TestMaintainer_Panics(t *testing.T) {
	t.Parallel()

	grace := 10 * time.Minute

	t.Run("recovers a panic in the middle of a compaction, logs it with its stack, backs off with a doubling wait up to the cap while reads and writes go on, and compacts once the panic stops", func(t *testing.T) {
		verify.Covers(t, "a maintenance step recovers a panic of the store in the middle of a compaction, logs its value and stack at the error level, does nothing until a backoff that doubles up to its cap has passed, keeps reads and writes working, and compacts once the panic stops, with every value kept", "INV-MAINTENANCE-PANIC")
		w := newWorld(t)
		var buf bytes.Buffer
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
		var panics []string
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 4, FullRatio: 100}, PanicBackoff: 10 * time.Second, MaxPanicBackoff: 40 * time.Second, OnPanic: func(value any, stack []byte) {
			panics = append(panics, fmt.Sprint(value))
			is.True(t, strings.Contains(string(stack), "mergeSegments"), string(stack))
		}})
		for i := range 4 {
			put(t, db, fmt.Sprint("k", i), "v")
		}
		// The store panics on the write of the merged segment, after the merge read every block.
		armed := true
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if armed && kind == s3.KindPut && strings.HasPrefix(key, "db/seg/") {
				panic("the store panics")
			}
			return s3fake.OutcomeNormal
		}
		res, err := m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		is.True(t, strings.Contains(err.Error(), "panic: the store panics"), err.Error())
		is.True(t, res.Attempted && !res.Ran, fmt.Sprint(res))
		is.True(t, !m.Holds(), "the maintainer forgets the lease")
		is.EqualSlice(t, []string{"the store panics"}, panics)
		records := panicRecords(t, &buf)
		is.Equal(t, 1, len(records))
		is.Equal(t, "the store panics", records[0]["panic"])
		is.True(t, strings.Contains(fmt.Sprint(records[0]["stack"]), "engine.(*DB).mergeSegments"), fmt.Sprint(records[0]["stack"]))
		is.Equal(t, float64(1), number(records[0]["panics"]))
		is.Equal(t, float64(10*time.Second), number(records[0]["backoff"]))

		// While maintenance backs off, reads and writes work, and a step does nothing.
		armed = false
		for i := range 4 {
			v, ok := get(t, db, fmt.Sprint("k", i))
			is.True(t, ok)
			is.Equal(t, "v", v)
		}
		put(t, db, "k4", "v")
		before := db.Stats().S3
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.Equal(t, engine.StepResult{BackingOff: true}, res)
		is.Equal(t, int64(0), db.Stats().S3.Sub(before).Total(), "a step that backs off makes no requests")
		is.Equal(t, 5, db.Stats().Segments)

		// A panic that repeats doubles the backoff up to the cap.
		armed = true
		for _, backoff := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
			w.clock.Sleep(t.Context(), backoff-time.Millisecond)
			res, err := m.Step(t.Context())
			is.NotError(t, err)
			is.True(t, res.BackingOff, fmt.Sprint(res))
			w.clock.Sleep(t.Context(), time.Millisecond)
			_, err = m.Step(t.Context())
			is.Error(t, engine.ErrPanicked, err)
		}
		records = panicRecords(t, &buf)
		is.Equal(t, 4, len(panics))
		is.Equal(t, 4, len(records))
		for i, backoff := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 40 * time.Second} {
			is.Equal(t, float64(i+1), number(records[i]["panics"]))
			is.Equal(t, float64(backoff), number(records[i]["backoff"]))
		}

		// Once the panic stops, the step after the backoff compacts, and nothing is lost.
		armed = false
		w.clock.Sleep(t.Context(), 40*time.Second-time.Millisecond)
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.BackingOff, "the backoff lasts to its end")
		w.clock.Sleep(t.Context(), time.Millisecond)
		res, err = m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Compacted, fmt.Sprint(res))
		is.Equal(t, 1, db.Stats().Segments)
		for i := range 5 {
			v, ok := get(t, db, fmt.Sprint("k", i))
			is.True(t, ok)
			is.Equal(t, "v", v)
		}

		// A step that got to work without a panic starts the backoff over.
		for i := range 4 {
			put(t, db, fmt.Sprint("k", i), "w")
		}
		armed = true
		_, err = m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		records = panicRecords(t, &buf)
		is.Equal(t, float64(1), number(records[len(records)-1]["panics"]))
		is.Equal(t, float64(10*time.Second), number(records[len(records)-1]["backoff"]))
	})

	t.Run("a step that fails without a panic between two panics keeps doubling the backoff, and a panic after the step's work counts its run once", func(t *testing.T) {
		w := newWorld(t)
		var buf bytes.Buffer
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 2, FullRatio: 100}})
		put(t, db, "k0", "v")
		put(t, db, "k1", "v")
		fail := ""
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			switch {
			case fail == "panic" && kind == s3.KindPut && strings.HasPrefix(key, "db/seg/"):
				panic("the store panics")
			case fail == "error" && kind == s3.KindGetConditional && key == "db/manifest":
				return s3fake.OutcomeError
			}
			return s3fake.OutcomeNormal
		}
		fail = "panic"
		_, err := m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		w.clock.Sleep(t.Context(), 10*time.Second)
		fail = "error"
		res, err := m.Step(t.Context())
		is.True(t, err != nil && !errors.Is(err, engine.ErrPanicked), fmt.Sprint(err))
		is.True(t, res.Attempted)
		fail = "panic"
		_, err = m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		records := panicRecords(t, &buf)
		is.Equal(t, 2, len(records))
		is.Equal(t, float64(20*time.Second), number(records[1]["backoff"]), "the failed step did not end the row")

		// The lease's release, after the compaction landed, panics.
		fail = ""
		w.clock.Sleep(t.Context(), 20*time.Second)
		leaseWrites := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/maintenance-lease" {
				if leaseWrites++; leaseWrites == 2 {
					panic("the store panics on the release")
				}
			}
			return s3fake.OutcomeNormal
		}
		res, err = m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		is.True(t, res.Compacted && res.Ran, fmt.Sprint(res))
		is.Equal(t, int64(1), db.Stats().MaintenanceRuns)
	})

	t.Run("keeps the backoff at its cap of 10 minutes by default, however many panics there are in a row", func(t *testing.T) {
		w := newWorld(t)
		var buf bytes.Buffer
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Logger: slog.New(slog.NewJSONHandler(&buf, nil))})
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 2}})
		put(t, db, "k0", "v")
		put(t, db, "k1", "v")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut && strings.HasPrefix(key, "db/seg/") {
				panic("the store panics")
			}
			return s3fake.OutcomeNormal
		}
		for range 70 {
			_, err := m.Step(t.Context())
			is.Error(t, engine.ErrPanicked, err)
			w.clock.Sleep(t.Context(), 10*time.Minute)
		}
		records := panicRecords(t, &buf)
		is.Equal(t, 70, len(records))
		for i, r := range records {
			want := min(10*time.Second<<min(i, 10), 10*time.Minute)
			is.Equal(t, float64(want), number(r["backoff"]), fmt.Sprint("panic ", i+1))
		}
	})

	t.Run("a panic between the segment write and the manifest write of a compaction, and one in the middle of garbage collection, lose nothing, and garbage collection then deletes everything they left behind", func(t *testing.T) {
		verify.Covers(t, "a maintenance step that panics after the write of its merged segment, or after garbage collection deleted some garbage, leaves every value readable, and later steps collect everything the panicked steps left behind", "INV-MAINTENANCE-PANIC")
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace})
		backoff := time.Second
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 4, FullRatio: 100}, PanicBackoff: backoff, MaxPanicBackoff: backoff})
		for i := range 4 {
			put(t, db, fmt.Sprint("k", i), fmt.Sprint(i))
		}
		check := func() {
			t.Helper()
			for i := range 4 {
				v, ok := get(t, db, fmt.Sprint("k", i))
				is.True(t, ok)
				is.Equal(t, fmt.Sprint(i), v)
			}
		}
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPutConditional && key == "db/manifest" {
				panic("the store panics on the manifest write")
			}
			return s3fake.OutcomeNormal
		}
		_, err := m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		w.decide = nil
		check()
		is.Equal(t, 4, db.Stats().Segments, "the compaction did not land")
		orphans := 0
		for _, key := range w.fake.Keys() {
			if strings.HasPrefix(key, "db/seg/") && !slices.Contains(segmentKeys(t, w), key) {
				orphans++
			}
		}
		is.Equal(t, 1, orphans, "the merged segment is in the store, and the manifest does not reference it")

		w.clock.Sleep(t.Context(), backoff)
		res, err := m.Step(t.Context())
		is.NotError(t, err)
		is.True(t, res.Compacted)
		check()

		// After the grace period, garbage collection panics after its first delete.
		w.clock.Sleep(t.Context(), grace)
		deletes := 0
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindDelete {
				if deletes++; deletes == 2 {
					panic("the store panics on the second delete")
				}
			}
			return s3fake.OutcomeNormal
		}
		_, err = m.Step(t.Context())
		is.Error(t, engine.ErrPanicked, err)
		w.decide = nil
		check()

		// Later steps finish the work: the store keeps only the manifest, the lease, and the
		// live segment.
		for range 4 {
			w.clock.Sleep(t.Context(), grace)
			_, err := m.Step(t.Context())
			is.NotError(t, err)
		}
		check()
		live := segmentKeys(t, w)
		is.Equal(t, 1, len(live))
		is.EqualSlice(t, []string{"db/maintenance-lease", "db/manifest", live[0]}, slices.Sorted(slices.Values(w.fake.Keys())))
	})

	t.Run("with MaintenancePanics, some steps panic with ErrInjectedPanic at points the random source picks, and the others make progress", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Mutations: engine.Mutations{MaintenancePanics: true}})
		var injected, inCompaction, inGC int
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 2}, PanicBackoff: time.Second, MaxPanicBackoff: time.Second, OnPanic: func(value any, stack []byte) {
			is.Equal(t, engine.ErrInjectedPanic, value.(error))
			injected++
			switch {
			case strings.Contains(string(stack), "engine.(*DB).compact("):
				inCompaction++
			case strings.Contains(string(stack), "engine.(*DB).gc("):
				inGC++
			}
		}})
		compactions := 0
		for i := range 40 {
			put(t, db, fmt.Sprint("k", i%5), fmt.Sprint(i))
			w.clock.Sleep(t.Context(), time.Second)
			res, err := m.Step(t.Context())
			if err != nil {
				is.Error(t, engine.ErrPanicked, err)
			}
			if res.Compacted && err == nil {
				compactions++
			}
		}
		is.True(t, injected >= 5, fmt.Sprint(injected, " panics"))
		is.True(t, inCompaction > 0 && inGC > 0, fmt.Sprint(inCompaction, " panics in compactions and ", inGC, " in GC passes"))
		is.True(t, compactions >= 5, fmt.Sprint(compactions, " compactions"))
		for k := range 5 {
			v, ok := get(t, db, fmt.Sprint("k", k))
			is.True(t, ok)
			is.Equal(t, fmt.Sprint(35+k), v)
		}
	})

	t.Run("with MaintenanceUnrecovered, a panic unwinds out of the step", func(t *testing.T) {
		w := newWorld(t)
		db := w.open(1, engine.Options{GCGracePeriod: &grace, Mutations: engine.Mutations{MaintenanceUnrecovered: true}})
		m := db.NewMaintainer(engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: 2}})
		put(t, db, "k0", "v")
		put(t, db, "k1", "v")
		w.decide = func(kind s3.Kind, key string) s3fake.Outcome {
			if kind == s3.KindPut && strings.HasPrefix(key, "db/seg/") {
				panic("the store panics")
			}
			return s3fake.OutcomeNormal
		}
		var p any
		func() {
			defer func() { p = recover() }()
			_, _ = m.Step(t.Context())
		}()
		is.Equal(t, "the store panics", p)
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
