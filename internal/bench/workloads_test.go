package bench_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/bench"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sched"
	"maragu.dev/maragudb/internal/tigristest"
	"maragu.dev/maragudb/internal/verify"
)

var update = flag.Bool("update", false, "rewrite the baseline in testdata/baseline.json")

func TestMain(m *testing.M) {
	verify.Main(m)
}

// loaded is the fake store of a database that [bench.Load] loaded in virtual time, and the
// virtual time when it was done.
type loaded struct {
	fake *s3fake.Client
	at   time.Time
}

func load(t *testing.T) loaded {
	t.Helper()
	s := sched.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	fake := s3fake.New()
	is.NotError(t, bench.Load(&bench.Env{Store: fake, Prefix: "db", Sched: s, Seed: 1}))
	return loaded{fake: fake, at: s.Now()}
}

// env with a copy of the loaded database, whose virtual clock goes on from when it was loaded.
func (l loaded) env(t *testing.T) *bench.Env {
	t.Helper()
	c := s3fake.New()
	for _, key := range l.fake.Keys() {
		body, _ := l.fake.Body(key)
		_, err := c.Put(t.Context(), key, body, s3.PutOptions{})
		is.NotError(t, err)
	}
	return &bench.Env{Store: c, Prefix: "db", Sched: sched.New(l.at), TempDir: t.TempDir, Seed: 1}
}

// run the workloads in parallel, each over its own copy of the loaded database.
func run(t *testing.T, l loaded, workloads []bench.Workload) []bench.Result {
	t.Helper()
	results := make([]bench.Result, len(workloads))
	t.Run("run", func(t *testing.T) {
		for i, w := range workloads {
			t.Run(w.Name, func(t *testing.T) {
				t.Parallel()
				res, err := bench.Run(l.env(t), w)
				is.NotError(t, err)
				results[i] = res
			})
		}
	})
	return results
}

func TestWorkloads(t *testing.T) {
	l := load(t)
	workloads := bench.Workloads()
	results := run(t, l, workloads)
	if t.Failed() {
		return
	}

	t.Run("no workload costs more per operation than the committed baseline", func(t *testing.T) {
		verify.Covers(t, "each benchmark workload, run in virtual time over the fake store, makes no more requests of each kind and moves no more bytes per operation, and has no lower cache hit rates, than testdata/baseline.json holds", "AC-5.9")
		path := filepath.Join("testdata", "baseline.json")
		got := bench.BaselineOf(results)
		if *update {
			b, err := json.MarshalIndent(got, "", "  ")
			is.NotError(t, err)
			is.NotError(t, os.WriteFile(path, append(b, '\n'), 0o644))
			return
		}
		b, err := os.ReadFile(path)
		is.NotError(t, err)
		var base bench.Baseline
		is.NotError(t, json.Unmarshal(b, &base))

		changes := map[[2]string]bench.Change{}
		for _, c := range bench.Compare(base, got) {
			changes[[2]string{c.Workload, c.Metric}] = c
			if c.Worse {
				t.Errorf("%s; make the change cheaper, or record the new value with `make bench-ratchet`", c)
			} else {
				t.Logf("%s; record it with `make bench-ratchet`", c)
			}
		}
		for _, r := range results {
			for _, m := range r.Metrics() {
				old, ok := base[r.Workload][m.Name]
				f := verify.Bench{Workload: r.Workload, Metric: m.Name, Measured: m.Value, Baseline: old, HasBaseline: ok}
				if c, ok := changes[[2]string{r.Workload, m.Name}]; ok {
					f.Change = "better"
					if c.Worse {
						f.Change = "worse"
					}
				}
				verify.Record(t, verify.Fact{Bench: &f})
			}
			t.Logf("%s: %d ops, %+v, $%.4f per million ops", r.Workload, r.Ops, r.Metrics(), r.DollarsPerMillionOps())
		}
		for w := range base {
			if _, ok := got[w]; !ok {
				t.Errorf("%s: the workload is in the baseline but did not run; remove it with `make bench-ratchet`", w)
			}
		}
	})

	t.Run("the workloads with several processes make the same requests on every run", func(t *testing.T) {
		verify.Covers(t, "a second run of the contending writers and of the writes with maintenance in virtual time, over another copy of the same database, gives the same requests, bytes, conflicts, retries, cache hits, and latencies", "AC-5.9")
		var again []bench.Workload
		var first []bench.Result
		for i, w := range workloads {
			if w.Name == "contending-writers" || w.Name == "writes-with-maintenance" {
				again = append(again, w)
				first = append(first, results[i])
			}
		}
		is.Equal(t, 2, len(again))
		second := run(t, l, again)
		for i := range again {
			is.Equal(t, fmt.Sprintf("%+v", first[i]), fmt.Sprintf("%+v", second[i]), again[i].Name)
		}
		is.True(t, first[0].Conflicts > 0 && first[0].Retries > 0, "the writers contend")
		is.True(t, first[1].CompactionBytesWritten > 0, "maintenance compacts while the writer writes")
	})

	t.Run("every workload has a summary and a unique name", func(t *testing.T) {
		names := map[string]bool{}
		for _, w := range workloads {
			is.True(t, !names[w.Name], w.Name)
			names[w.Name] = true
			is.True(t, strings.HasSuffix(w.Summary, "."), w.Name)
		}
	})
}

// The markers of the generated section of docs/benchmarks.md.
const (
	resultsStart = "<!-- The results below are generated by `make bench`. -->"
	resultsEnd   = "<!-- End of the generated results. -->"
)

// TestTigris runs every workload in real time against Tigris and writes the results into
// docs/benchmarks.md. It runs only with MARAGUDB_BENCH set, as `make bench` sets it.
func TestTigris(t *testing.T) {
	if os.Getenv("MARAGUDB_BENCH") == "" {
		t.Skip("set MARAGUDB_BENCH=1, as `make bench` does, to run the benchmark against Tigris")
	}
	client, cfg := tigristest.NewClient(t)
	// The seed of the gate, so that the workloads choose the same rows as there.
	e := &bench.Env{Store: client, Prefix: cfg.Prefix, TempDir: t.TempDir, Seed: 1}

	start := time.Now()
	is.NotError(t, bench.Load(e))
	t.Logf("loaded %d rows in %v", bench.Rows, time.Since(start).Round(time.Millisecond))

	workloads := bench.Workloads()
	var results []bench.Result
	for _, w := range workloads {
		res, err := bench.Run(e, w)
		is.NotError(t, err, w.Name)
		t.Logf("%s: %d ops in %v, p50 %v, p99 %v, %+v, $%.2f per million ops", w.Name, res.Ops, res.Elapsed.Round(time.Millisecond),
			res.Percentile(50), res.Percentile(99), res.Requests, res.DollarsPerMillionOps())
		results = append(results, res)
	}

	ping := cfg.Prefix + "/ping"
	_, err := client.Put(t.Context(), ping, []byte("p"), s3.PutOptions{})
	is.NotError(t, err)
	var trips bench.Result
	for range 21 {
		start := time.Now()
		_, err := client.Get(t.Context(), ping, s3.GetOptions{})
		is.NotError(t, err)
		trips.Latencies = append(trips.Latencies, time.Since(start))
	}
	infos, err := client.List(t.Context(), cfg.Prefix+"/")
	is.NotError(t, err)
	var size int64
	for _, info := range infos {
		size += info.Size
	}

	hash, dirty := commit(t)
	r := bench.Report{
		Date:       time.Now(),
		Commit:     hash,
		Dirty:      dirty,
		Machine:    machine(),
		Store:      fmt.Sprintf("Tigris with AWS_REGION=%s", cfg.Region),
		RoundTrip:  trips.Percentile(50),
		StoreBytes: size,
		Workloads:  workloads,
		Results:    results,
	}
	path := filepath.Join("..", "..", "docs", "benchmarks.md")
	doc, err := os.ReadFile(path)
	is.NotError(t, err)
	out, err := bench.ReplaceSection(string(doc), resultsStart, resultsEnd, "\n"+r.Markdown()+"\n")
	is.NotError(t, err)
	is.NotError(t, os.WriteFile(path, []byte(out), 0o644))
	t.Log(r.Markdown())
}

// commit that is checked out, and whether tracked files have changes.
func commit(t *testing.T) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	is.NotError(t, err)
	status, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output()
	is.NotError(t, err)
	return strings.TrimSpace(string(out)), len(status) > 0
}

// machine that runs the test: its operating system, architecture, processor, and cores.
func machine() string {
	cpu := ""
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			cpu = strings.TrimSpace(string(out))
		}
	case "linux":
		if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for line := range strings.Lines(string(b)) {
				if name, ok := strings.CutPrefix(line, "model name"); ok {
					cpu = strings.TrimSpace(strings.TrimLeft(name, " \t:"))
					break
				}
			}
		}
	}
	m := fmt.Sprintf("%s/%s with %d cores", runtime.GOOS, runtime.GOARCH, runtime.NumCPU())
	if cpu != "" {
		m = fmt.Sprintf("%s, %s", cpu, m)
	}
	return m + ", " + runtime.Version()
}
