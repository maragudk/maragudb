package bench

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Metric of a workload, per operation unless its name says otherwise.
type Metric struct {
	Name  string
	Value float64
	// HigherIsBetter is true for the hit rates of caches; for every other metric, lower is better.
	HigherIsBetter bool
}

// Metrics of the result, which a run in virtual time gives exactly the same every time. A
// metric whose value would be zero is left out, and so is a hit rate of a cache without lookups.
func (r Result) Metrics() []Metric {
	if r.Ops == 0 {
		return nil
	}
	perOp := func(n int64) float64 { return float64(n) / float64(r.Ops) }
	var ms []Metric
	lower := func(name string, v float64) {
		if v != 0 {
			ms = append(ms, Metric{Name: name, Value: v})
		}
	}
	rate := func(name string, hits, misses int64) {
		if hits+misses > 0 {
			ms = append(ms, Metric{Name: name, Value: float64(hits) / float64(hits+misses), HigherIsBetter: true})
		}
	}
	lower("GETs", perOp(r.Requests.Get))
	lower("conditional GETs", perOp(r.Requests.GetConditional))
	lower("PUTs", perOp(r.Requests.Put))
	lower("conditional PUTs", perOp(r.Requests.PutConditional))
	lower("DELETEs", perOp(r.Requests.Delete))
	lower("LISTs", perOp(r.Requests.List))
	lower("bytes read", perOp(r.BytesRead))
	lower("bytes written", perOp(r.BytesWritten))
	lower("manifest bytes written", perOp(r.ManifestBytesWritten))
	lower("conflicts", perOp(r.Conflicts))
	lower("retries", perOp(r.Retries))
	rate("memory cache hit rate", r.MemoryCacheHits, r.MemoryCacheMisses)
	rate("disk cache hit rate", r.DiskCacheHits, r.DiskCacheMisses)
	if r.CommittedBytes > 0 {
		lower("compaction bytes written per committed byte", float64(r.CompactionBytesWritten)/float64(r.CommittedBytes))
	}
	lower("segments at the end", float64(r.Segments))
	return ms
}

// Baseline holds the metrics of each workload by name.
type Baseline map[string]map[string]float64

// BaselineOf the results.
func BaselineOf(results []Result) Baseline {
	b := Baseline{}
	for _, r := range results {
		m := map[string]float64{}
		for _, metric := range r.Metrics() {
			m[metric.Name] = metric.Value
		}
		b[r.Workload] = m
	}
	return b
}

// Change of a metric from the baseline.
type Change struct {
	Workload, Metric string
	Old, New         float64
	// Worse is true when the change is a regression, or when the metric is missing on one side,
	// and false when it is an improvement.
	Worse bool
	// Missing is true for a metric of the baseline that is no longer measured, and Added for a
	// metric without a baseline.
	Missing, Added bool
}

func (c Change) String() string {
	switch {
	case c.Added:
		return fmt.Sprintf("%s: %s is %s and has no baseline", c.Workload, c.Metric, format(c.New))
	case c.Missing:
		return fmt.Sprintf("%s: %s was %s and is no longer measured", c.Workload, c.Metric, format(c.Old))
	case c.Worse:
		return fmt.Sprintf("%s: %s got worse, from %s to %s", c.Workload, c.Metric, format(c.Old), format(c.New))
	}
	return fmt.Sprintf("%s: %s got better, from %s to %s", c.Workload, c.Metric, format(c.Old), format(c.New))
}

// format a metric with up to four decimals.
func format(v float64) string {
	s := fmt.Sprintf("%.4f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// Compare got with the baseline base, and return every metric that changed, in the order of
// the workloads and the metric names. A metric on only one side is a change for the worse: a
// count that falls to zero, which [Result.Metrics] leaves out, looks the same as a count that
// is no longer measured, so the baseline must record it. A workload of the baseline that is
// missing from got is left out, so that a run of some workloads can be compared.
func Compare(base, got Baseline) []Change {
	var changes []Change
	workloads := make([]string, 0, len(got))
	for w := range got {
		workloads = append(workloads, w)
	}
	sort.Strings(workloads)
	for _, w := range workloads {
		old := base[w]
		var names []string
		for n := range old {
			names = append(names, n)
		}
		for n := range got[w] {
			if _, ok := old[n]; !ok {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			o, inOld := old[n]
			v, inNew := got[w][n]
			switch {
			case !inNew:
				changes = append(changes, Change{Workload: w, Metric: n, Old: o, Worse: true, Missing: true})
			case !inOld:
				changes = append(changes, Change{Workload: w, Metric: n, New: v, Worse: true, Added: true})
			case !same(o, v):
				changes = append(changes, Change{Workload: w, Metric: n, Old: o, New: v, Worse: (v < o) == strings.HasSuffix(n, "hit rate")})
			}
		}
	}
	return changes
}

// same reports whether two metrics are equal, but for the rounding of their encoding.
func same(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(math.Abs(a), math.Abs(b))
}

// Tigris's prices for the Standard tier in a global or single region, from
// https://www.tigrisdata.com/pricing/ (also served as https://www.tigrisdata.com/pricing.md),
// read on 2026-11-07. The free allowance of each month is left out.
const (
	// classAPerRequest is the price of a Class A request, such as PutObject and ListObjectsV2:
	// $0.005 per 1,000.
	classAPerRequest = 0.005 / 1000
	// classBPerRequest is the price of a Class B request, such as GetObject and HeadObject,
	// including a conditional GET that returns 304: $0.0005 per 1,000.
	classBPerRequest = 0.0005 / 1000
	// storagePerGiBMonth is the price of storage: $0.02 per GB-month, where Tigris's GB is 2^30
	// bytes. DELETE requests and data transfer out are free.
	storagePerGiBMonth = 0.02
)

// DollarsPerMillionOps that the requests of a million operations cost at Tigris's prices.
func (r Result) DollarsPerMillionOps() float64 {
	if r.Ops == 0 {
		return 0
	}
	q := r.Requests
	dollars := float64(q.Put+q.PutConditional+q.List)*classAPerRequest + float64(q.Get+q.GetConditional)*classBPerRequest
	return dollars / float64(r.Ops) * 1e6
}

// StorageDollarsPerMonth that storing the given bytes costs at Tigris's prices.
func StorageDollarsPerMonth(bytes int64) float64 {
	return float64(bytes) / (1 << 30) * storagePerGiBMonth
}

// Percentile of the latencies, by the nearest rank, for p from 0 to 100.
func (r Result) Percentile(p float64) time.Duration {
	if len(r.Latencies) == 0 {
		return 0
	}
	sorted := slices.Clone(r.Latencies)
	slices.Sort(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

// Throughput in operations per second.
func (r Result) Throughput() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Ops) / r.Elapsed.Seconds()
}
