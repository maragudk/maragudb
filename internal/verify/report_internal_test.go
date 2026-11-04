package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"
)

func TestPublishTo(t *testing.T) {
	t.Run("keeps what was in the summary and ends with the report over every package, when many publish at once", func(t *testing.T) {
		dir := t.TempDir()
		summary := filepath.Join(t.TempDir(), "summary.md")
		is.NotError(t, os.WriteFile(summary, []byte("before\n"), 0o644))

		var wg sync.WaitGroup
		for i := range 16 {
			wg.Go(func() {
				line := fmt.Sprintf(`{"package":"p%02d","pid":%d,"start":{}}`+"\n"+`{"package":"p%02d","pid":%d,"end":{"code":0}}`+"\n", i, i, i, i)
				is.NotError(t, os.WriteFile(factsFile(dir, fmt.Sprintf("p%02d", i)), []byte(line), 0o644))
				is.NotError(t, publishTo(summary, dir, Catalog{}))
			})
		}
		wg.Wait()
		// The last one to publish saw every package.
		is.NotError(t, publishTo(summary, dir, Catalog{}))

		b, err := os.ReadFile(summary)
		is.NotError(t, err)
		got := string(b)
		is.True(t, strings.HasPrefix(got, "before\n## maragudb verification report\n"), got)
		is.Equal(t, 1, strings.Count(got, "## maragudb verification report"))
		is.True(t, strings.Contains(got, "All 16 test packages passed."), got)
	})
}

func TestPackageOf(t *testing.T) {
	tests := []struct{ function, want string }{
		{"maragu.dev/maragudb/internal/sim_test.TestMain", "internal/sim"},
		{"maragu.dev/maragudb/internal/sql/exec_test.TestSession.func1", "internal/sql/exec"},
		{"maragu.dev/maragudb_test.TestMain", "."},
		{"maragu.dev/maragudb.TestStoreClient", "."},
		{"maragu.dev/maragudb/antithesis/workload.TestMain", "antithesis/workload"},
	}
	for _, test := range tests {
		t.Run(test.function, func(t *testing.T) {
			is.Equal(t, test.want, packageOf(test.function))
		})
	}
}
