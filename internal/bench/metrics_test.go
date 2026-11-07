package bench_test

import (
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/bench"
	"maragu.dev/maragudb/internal/s3"
)

func TestResult_Metrics(t *testing.T) {
	t.Run("gives each count per operation, the hit rates, and leaves out zeros and caches without lookups", func(t *testing.T) {
		r := bench.Result{
			Ops:             4,
			Requests:        s3.Counts{Get: 2, GetConditional: 4, PutConditional: 1},
			BytesRead:       10,
			MemoryCacheHits: 3, MemoryCacheMisses: 1,
			Segments: 5,
		}
		got := map[string]bench.Metric{}
		for _, m := range r.Metrics() {
			got[m.Name] = m
		}
		is.Equal(t, 6, len(got))
		is.Equal(t, 0.5, got["GETs"].Value)
		is.Equal(t, 1.0, got["conditional GETs"].Value)
		is.Equal(t, 0.25, got["conditional PUTs"].Value)
		is.Equal(t, 2.5, got["bytes read"].Value)
		is.Equal(t, bench.Metric{Name: "memory cache hit rate", Value: 0.75, HigherIsBetter: true}, got["memory cache hit rate"])
		is.Equal(t, 5.0, got["segments at the end"].Value)
	})

	t.Run("gives the bytes that compactions wrote per committed byte", func(t *testing.T) {
		r := bench.Result{Ops: 1, CommittedBytes: 100, CompactionBytesWritten: 250}
		is.EqualSlice(t, []bench.Metric{{Name: "compaction bytes written per committed byte", Value: 2.5}}, r.Metrics())
	})

	t.Run("gives no metrics without operations", func(t *testing.T) {
		is.Equal(t, 0, len(bench.Result{Requests: s3.Counts{Get: 1}}.Metrics()))
	})
}

func TestCompare(t *testing.T) {
	base := bench.Baseline{"reads": {"GETs": 1, "bytes read": 100, "memory cache hit rate": 0.9}}

	t.Run("reports nothing when every metric equals its baseline", func(t *testing.T) {
		is.Equal(t, 0, len(bench.Compare(base, base)))
	})

	t.Run("reports a higher count and a lower hit rate as worse, naming the workload, the metric, and both values", func(t *testing.T) {
		changes := bench.Compare(base, bench.Baseline{"reads": {"GETs": 1.5, "bytes read": 100, "memory cache hit rate": 0.8}})
		is.Equal(t, 2, len(changes))
		is.Equal(t, bench.Change{Workload: "reads", Metric: "GETs", Old: 1, New: 1.5, Worse: true}, changes[0])
		is.Equal(t, "reads: GETs got worse, from 1 to 1.5", changes[0].String())
		is.Equal(t, bench.Change{Workload: "reads", Metric: "memory cache hit rate", Old: 0.9, New: 0.8, Worse: true}, changes[1])
	})

	t.Run("reports a lower count and a higher hit rate as better", func(t *testing.T) {
		changes := bench.Compare(base, bench.Baseline{"reads": {"GETs": 0.5, "bytes read": 100, "memory cache hit rate": 0.95}})
		is.Equal(t, 2, len(changes))
		is.True(t, !changes[0].Worse && !changes[1].Worse)
		is.Equal(t, "reads: GETs got better, from 1 to 0.5", changes[0].String())
	})

	t.Run("reports a count that is no longer measured, which includes one that fell to zero, and one that appears as worse", func(t *testing.T) {
		changes := bench.Compare(base, bench.Baseline{"reads": {"bytes read": 100, "PUTs": 0.25, "memory cache hit rate": 0.9}})
		is.Equal(t, 2, len(changes))
		is.Equal(t, bench.Change{Workload: "reads", Metric: "GETs", Old: 1, Worse: true, Missing: true}, changes[0])
		is.Equal(t, "reads: GETs was 1 and is no longer measured", changes[0].String())
		is.Equal(t, bench.Change{Workload: "reads", Metric: "PUTs", New: 0.25, Worse: true, Added: true}, changes[1])
	})

	t.Run("reports a hit rate that is no longer measured as worse, and one without a baseline", func(t *testing.T) {
		changes := bench.Compare(base, bench.Baseline{"reads": {"GETs": 1, "bytes read": 100, "disk cache hit rate": 1}})
		is.Equal(t, 2, len(changes))
		is.Equal(t, bench.Change{Workload: "reads", Metric: "disk cache hit rate", New: 1, Worse: true, Added: true}, changes[0])
		is.Equal(t, bench.Change{Workload: "reads", Metric: "memory cache hit rate", Old: 0.9, Worse: true, Missing: true}, changes[1])
	})

	t.Run("reports every metric of a workload without a baseline as worse", func(t *testing.T) {
		changes := bench.Compare(base, bench.Baseline{"writes": {"PUTs": 1}})
		is.EqualSlice(t, []bench.Change{{Workload: "writes", Metric: "PUTs", New: 1, Worse: true, Added: true}}, changes)
		is.Equal(t, "writes: PUTs is 1 and has no baseline", changes[0].String())
	})

	t.Run("ignores a difference in the last bits of a float", func(t *testing.T) {
		is.Equal(t, 0, len(bench.Compare(base, bench.Baseline{"reads": {"GETs": 1 + 1e-12, "bytes read": 100, "memory cache hit rate": 0.9}})))
	})
}

func TestResult_DollarsPerMillionOps(t *testing.T) {
	t.Run("prices PUTs and LISTs as Class A, GETs as Class B, and DELETEs as free", func(t *testing.T) {
		r := bench.Result{Ops: 2, Requests: s3.Counts{Put: 1, PutConditional: 1, List: 2, Get: 10, GetConditional: 10, Delete: 100}}
		// 4 Class A requests at $5 and 20 Class B requests at $0.50 per million, for 2 operations.
		is.True(t, abs(r.DollarsPerMillionOps()-(4*5+20*0.5)/2) < 1e-9, r.DollarsPerMillionOps())
	})
}

func TestStorageDollarsPerMonth(t *testing.T) {
	t.Run("prices storage at 2 cents per GiB and month", func(t *testing.T) {
		is.True(t, abs(bench.StorageDollarsPerMonth(5<<30)-0.1) < 1e-12)
	})
}

func TestResult_Percentile(t *testing.T) {
	t.Run("gives the latency at the nearest rank", func(t *testing.T) {
		var r bench.Result
		for i := 100; i >= 1; i-- {
			r.Latencies = append(r.Latencies, time.Duration(i)*time.Millisecond)
		}
		is.Equal(t, 50*time.Millisecond, r.Percentile(50))
		is.Equal(t, 99*time.Millisecond, r.Percentile(99))
		is.Equal(t, 100*time.Millisecond, r.Percentile(100))
		is.Equal(t, 1*time.Millisecond, r.Percentile(0))
		is.Equal(t, time.Duration(0), bench.Result{}.Percentile(50))
	})
}

func TestResult_Throughput(t *testing.T) {
	t.Run("gives operations per second", func(t *testing.T) {
		is.Equal(t, 50.0, bench.Result{Ops: 100, Elapsed: 2 * time.Second}.Throughput())
		is.Equal(t, 0.0, bench.Result{Ops: 100}.Throughput())
	})
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
