package bench

import (
	"fmt"
	"strings"
	"time"
)

// Report of a run of the workloads against a real store.
type Report struct {
	Date time.Time
	// Commit of the code that ran, and Dirty whether the tracked files had changes.
	Commit string
	Dirty  bool
	// Machine that ran the workloads.
	Machine string
	// Store that the workloads ran against, with its region.
	Store string
	// RoundTrip is the median latency of a GET of a small object from the store.
	RoundTrip time.Duration
	// StoreBytes are the bytes of the objects of the database when the workloads were done,
	// including the garbage that the grace period keeps.
	StoreBytes int64
	Workloads  []Workload
	Results    []Result
}

// Markdown of the report: what ran where, a table of the results, and what each workload does.
func (r Report) Markdown() string {
	var b strings.Builder
	dirty := ""
	if r.Dirty {
		dirty = " with uncommitted changes"
	}
	fmt.Fprintf(&b, "Run on %s at commit `%s`%s, on %s, against %s. A GET of a small object took %v at the median. At the end, the database and its garbage within the grace period took up %s in the store, which costs $%.4f per month.\n\n",
		r.Date.UTC().Format("2006-01-02"), r.Commit, dirty, r.Machine, r.Store, r.RoundTrip.Round(100*time.Microsecond), mebibytes(r.StoreBytes), StorageDollarsPerMonth(r.StoreBytes))
	b.WriteString("| Workload | Ops | p50 | p99 | Ops/s | GETs/op | PUTs/op | LISTs/op | KiB read/op | KiB written/op | Conflicts/op | $ per million ops |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, res := range r.Results {
		q := res.Requests
		perOp := func(n int64) float64 { return float64(n) / float64(max(res.Ops, 1)) }
		fmt.Fprintf(&b, "| %s | %d | %v | %v | %.1f | %.2f | %.2f | %.2f | %.1f | %.1f | %.2f | %.2f |\n",
			res.Workload, res.Ops, millis(res.Percentile(50)), millis(res.Percentile(99)), res.Throughput(),
			perOp(q.Get+q.GetConditional), perOp(q.Put+q.PutConditional), perOp(q.List),
			perOp(res.BytesRead)/1024, perOp(res.BytesWritten)/1024, perOp(res.Conflicts), res.DollarsPerMillionOps())
	}
	b.WriteString("\nGETs include the conditional GETs of the freshness checks, and PUTs the conditional PUTs of the manifest and the lease.\n\n")
	for _, w := range r.Workloads {
		fmt.Fprintf(&b, "- `%s`: %s\n", w.Name, w.Summary)
	}
	return b.String()
}

// millis renders a duration in milliseconds, with one decimal.
func millis(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
}

// mebibytes renders a size in MiB, with one decimal.
func mebibytes(n int64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}

// ReplaceSection of doc between the line start and the next line end with body, and return the
// result. It fails when doc does not have the line start with the line end after it.
func ReplaceSection(doc, start, end, body string) (string, error) {
	i := strings.Index(doc, start+"\n")
	if i < 0 {
		return "", fmt.Errorf("bench: the document has no line %q", start)
	}
	head := doc[:i+len(start)+1]
	j := strings.Index(doc[len(head):], end)
	if j < 0 {
		return "", fmt.Errorf("bench: the document has no line %q after %q", end, start)
	}
	return head + body + doc[len(head)+j:], nil
}
