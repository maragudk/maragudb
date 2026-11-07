package bench_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/bench"
	"maragu.dev/maragudb/internal/s3"
)

func TestReport_Markdown(t *testing.T) {
	t.Run("gives where it ran, a row per workload, and what each workload does", func(t *testing.T) {
		r := bench.Report{
			Date:       time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
			Commit:     "abc123",
			Machine:    "a laptop",
			Store:      "a store",
			RoundTrip:  12340 * time.Microsecond,
			StoreBytes: 3 << 20,
			Workloads:  []bench.Workload{{Name: "reads", Summary: "Reads rows."}},
			Results: []bench.Result{{
				Workload: "reads", Ops: 2,
				Requests:  s3.Counts{Get: 1, GetConditional: 2, Put: 1, PutConditional: 1, List: 1},
				BytesRead: 2048, BytesWritten: 1024, Conflicts: 1,
				Latencies: []time.Duration{10 * time.Millisecond, 30 * time.Millisecond},
				Elapsed:   time.Second,
			}},
		}
		md := r.Markdown()
		for _, want := range []string{
			"Run on 2026-11-07 at commit `abc123`, on a laptop, against a store. A GET of a small object took 12.3ms at the median. At the end, the database and its garbage within the grace period took up 3.0 MiB in the store, which costs $0.0001 per month.",
			fmt.Sprintf("| reads | 2 | 10.0 ms | 30.0 ms | 2.0 | 1.50 | 1.00 | 0.50 | 1.0 | 0.5 | 0.50 | %.2f |", r.Results[0].DollarsPerMillionOps()),
			"- `reads`: Reads rows.",
		} {
			is.True(t, strings.Contains(md, want), fmt.Sprintf("%q in:\n%s", want, md))
		}
	})
}

func TestReplaceSection(t *testing.T) {
	t.Run("replaces what is between the markers and keeps the rest", func(t *testing.T) {
		got, err := bench.ReplaceSection("intro\n<!-- a -->\nold\n<!-- b -->\nend\n", "<!-- a -->", "<!-- b -->", "new\n")
		is.NotError(t, err)
		is.Equal(t, "intro\n<!-- a -->\nnew\n<!-- b -->\nend\n", got)
	})

	t.Run("finds the end marker after the start marker, also as the last line without a newline", func(t *testing.T) {
		got, err := bench.ReplaceSection("<!-- b -->\n<!-- a -->\nold\n<!-- b -->", "<!-- a -->", "<!-- b -->", "new\n")
		is.NotError(t, err)
		is.Equal(t, "<!-- b -->\n<!-- a -->\nnew\n<!-- b -->", got)
	})

	t.Run("fails without the markers in order", func(t *testing.T) {
		_, err := bench.ReplaceSection("<!-- b -->\n<!-- a -->\n", "<!-- a -->", "<!-- b -->", "new\n")
		is.True(t, err != nil)
	})
}
