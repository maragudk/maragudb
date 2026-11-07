package verify_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

var update = flag.Bool("update", false, "write "+verify.DocPath)

var loaded struct {
	once sync.Once
	root string
	c    verify.Catalog
	src  *verify.Source
	err  error
}

// load the catalog and the source of the module, once for every test.
func load(t *testing.T) (string, verify.Catalog, *verify.Source) {
	t.Helper()
	loaded.once.Do(func() {
		if loaded.root, loaded.err = verify.FindRoot(); loaded.err != nil {
			return
		}
		if loaded.c, loaded.err = verify.LoadCatalog(loaded.root); loaded.err != nil {
			return
		}
		loaded.src, loaded.err = verify.ParseSource(loaded.root)
	})
	is.NotError(t, loaded.err)
	return loaded.root, loaded.c, loaded.src
}

func TestVerification(t *testing.T) {
	t.Run("every requirement and unsupported statement is verified by a test, and every test names IDs that exist", func(t *testing.T) {
		verify.Covers(t, "every acceptance criterion, invariant, class of unsupported statements, and remaining difference has a verifying test, and no test declares an unknown ID", "AC-4.4")
		_, c, src := load(t)
		covs, errs := src.Coverage()
		for _, err := range errs {
			t.Error(err)
		}
		ids := c.IDs()
		covered := map[string]bool{}
		for _, cov := range covs {
			for _, id := range cov.IDs {
				covered[id] = true
				if !ids[id] {
					t.Errorf("%s: %s %s declares the unknown ID %s", cov.Pos, cov.Package, cov.Test, id)
				}
			}
		}
		for _, r := range c.Requirements {
			if !covered[r.ID] {
				t.Errorf("no test verifies %s (%s): %s", r.ID, r.Source, r.Text)
			}
		}
		for _, s := range c.States {
			if !covered[string(s.State)] {
				t.Errorf("no test verifies that the corpus reaches %s", s.State)
			}
		}
		is.True(t, len(c.Requirements) > 60, "the documents define the IDs")
	})

	t.Run("every mutant is detected by the simulator or by a targeted test", func(t *testing.T) {
		verify.Covers(t, "no mutant in the kill matrix goes undetected by both the simulator and the targeted tests", "AC-2")
		_, c, src := load(t)
		covs, _ := src.Coverage()
		targeted := map[string]bool{}
		for _, cov := range covs {
			for _, id := range cov.IDs {
				targeted[id] = true
			}
		}
		is.True(t, len(c.KillMatrix.Mutants) >= 8, "the kill matrix has the mutants")
		for _, m := range c.KillMatrix.Mutants {
			is.True(t, strings.HasPrefix(m.ID, "MUT-"), m.ID)
			if !m.Detected() && !targeted[m.ID] {
				t.Errorf("mutant %s (%s) is detected by nothing", m.ID, m.Name)
			}
			is.Equal(t, len(c.KillMatrix.Cells), len(m.Results), m.ID)
			for _, r := range m.Results {
				if r.Status != verify.Detected && r.Reason == "" {
					t.Errorf("mutant %s is not detected in %s, and no reason says why", m.ID, r.Cell)
				}
			}
		}
	})

	t.Run("every test package runs through verify.Main, so that its facts are recorded", func(t *testing.T) {
		_, _, src := load(t)
		is.EqualSlice(t, nil, src.PackagesWithoutMain())
	})

	t.Run("every reached state is counted and marked by an Antithesis assertion", func(t *testing.T) {
		verify.Covers(t, "every declared state has a reach.Hit in the code and an Antithesis Sometimes or Reachable assertion with its message", "AC-8.1")
		_, c, src := load(t)
		consts := src.StringConstants("internal/reach")
		hits := src.Hits()
		messages := src.Assertions()
		marked := map[string]bool{}
		for _, kind := range []string{"Sometimes", "Reachable"} {
			for _, msg := range messages[kind] {
				marked[msg] = true
			}
		}
		for _, s := range c.States {
			var name string
			for n, v := range consts {
				if v == string(s.State) {
					name = n
				}
			}
			is.True(t, name != "", fmt.Sprintf("%s is a constant in package reach", s.State))
			is.True(t, hits[name], fmt.Sprintf("reach.%s is hit somewhere", name))
			is.True(t, marked[s.Assertion], fmt.Sprintf("an assertion marks %s with %q", s.State, s.Assertion))
		}
		is.True(t, len(messages["Always"]) >= 3, "Always assertions mark the invariants")
	})

	t.Run("the verification map is up to date", func(t *testing.T) {
		verify.Covers(t, "docs/verification.md equals what the tests and documents generate", "AC-4.4", "AC-2")
		root, c, src := load(t)
		covs, _ := src.Coverage()
		got := verify.GenerateDoc(c, covs)
		path := filepath.Join(root, verify.DocPath)
		if *update {
			is.NotError(t, os.WriteFile(path, []byte(got), 0o644))
		}
		want, err := os.ReadFile(path)
		is.NotError(t, err)
		if got != string(want) {
			t.Errorf("%s is stale; run `make verification` to regenerate it", verify.DocPath)
		}
	})
}

func TestParseRequirements(t *testing.T) {
	t.Run("finds IDs after the text they name, with the title of the enclosing item", func(t *testing.T) {
		doc := "# R\n\nIntro sentence. `INV-ONE` Second sentence. `INV-TWO`\n\n" +
			"- **Thing:** first part. `INV-THREE` Then more. `INV-FOUR`\n" +
			"  - nested `INV-FIVE`\n\n" +
			"1. **Criterion one.** Body text. `AC-1.1`\n" +
			"   - sub item `AC-1.2`\n"
		reqs, err := verify.ParseRequirements("docs/requirements.md", doc)
		is.NotError(t, err)
		var got []string
		for _, r := range reqs {
			got = append(got, r.ID+"="+r.Text)
		}
		is.EqualSlice(t, []string{
			"INV-ONE=Intro sentence.",
			"INV-TWO=Second sentence.",
			"INV-THREE=Thing: first part.",
			"INV-FOUR=Thing: Then more.",
			"INV-FIVE=Thing: nested",
			"AC-1.1=Criterion one. Body text.",
			"AC-1.2=Criterion one: sub item",
		}, got)
	})

	t.Run("takes the bold title as the text of an unsupported statement or a remaining difference", func(t *testing.T) {
		reqs, err := verify.ParseRequirements("docs/unsupported.md", "- **The numeric type.** `UNS-NUMERIC` Details.\n1. **No coercion.** `DEV-1` Details.\n")
		is.NotError(t, err)
		is.Equal(t, 2, len(reqs))
		is.Equal(t, "The numeric type.", reqs[0].Text)
		is.Equal(t, "UNS", reqs[0].Kind())
		is.Equal(t, "No coercion.", reqs[1].Text)
		is.Equal(t, "DEV", reqs[1].Kind())
	})

	t.Run("rejects an ID defined twice and a malformed ID", func(t *testing.T) {
		_, err := verify.ParseRequirements("docs/requirements.md", "a `AC-1` b `AC-1`\n")
		is.True(t, err != nil)
		_, err = verify.ParseRequirements("docs/unsupported.md", "1. **x** `DEV-A`\n")
		is.True(t, err != nil)
		_, err = verify.ParseRequirements("docs/unsupported.md", "- **x** `UNS-1`\n")
		is.True(t, err != nil)
		_, err = verify.ParseRequirements("docs/requirements.md", "- **x** `UNS-A`\n")
		is.True(t, err != nil, "an unsupported statement belongs in its own document")
	})
}

func TestSource_Coverage(t *testing.T) {
	t.Run("attributes Covers calls to their subtests and rejects calls it cannot read", func(t *testing.T) {
		dir := t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n"), 0o644))
		is.NotError(t, os.MkdirAll(filepath.Join(dir, "p"), 0o755))
		is.NotError(t, os.WriteFile(filepath.Join(dir, "p", "p_test.go"), []byte(`package p_test

func TestA(t *testing.T) {
	verify.Covers(t, "top", "AC-1")
	t.Run("a sub test", func(t *testing.T) {
		verify.Covers(t, "sub", "AC-2", "INV-X")
		t.Run(name, func(t *testing.T) {
			verify.Covers(t, "dynamic", "AC-3")
		})
	})
	verify.Covers(t, what, "AC-4")
	verify.Covers(t, "no id")
}

func helper(t *testing.T) {
	verify.Covers(t, "helper", "AC-5")
}
`), 0o644))
		src, err := verify.ParseSource(dir)
		is.NotError(t, err)
		covs, errs := src.Coverage()
		var got []string
		for _, c := range covs {
			got = append(got, fmt.Sprintf("%s %s %s %v", c.Package, c.Test, c.What, c.IDs))
		}
		sort.Strings(got)
		is.EqualSlice(t, []string{
			"p TestA top [AC-1]",
			"p TestA/a_sub_test sub [AC-2 INV-X]",
			"p TestA/a_sub_test/* dynamic [AC-3]",
		}, got)
		is.Equal(t, 3, len(errs), fmt.Sprint(errs))
		is.EqualSlice(t, []string{"p"}, src.PackagesWithoutMain())
	})
}

func TestRender(t *testing.T) {
	c := verify.Catalog{
		Requirements: []verify.Requirement{{ID: "AC-1"}, {ID: "AC-2"}, {ID: "AC-3"}},
		States:       reach.Declarations()[:2],
	}

	t.Run("shows what ran, and what failed in a failing run", func(t *testing.T) {
		facts := []verify.Fact{
			{Package: "a", PID: 1, Start: &verify.Start{}},
			{Package: "a", PID: 1, Test: "TestA", Cover: &verify.Cover{IDs: []string{"AC-1"}, What: "a works", Result: verify.Passed}},
			{Package: "a", PID: 1, End: &verify.End{Code: 0}},
			{Package: "b", PID: 2, Start: &verify.Start{}},
			{Package: "b", PID: 2, Test: "TestB", Cover: &verify.Cover{IDs: []string{"AC-2"}, What: "b works", Result: verify.Failed}},
			{Package: "b", PID: 2, End: &verify.End{Code: 1}},
			{Package: "c", PID: 3, Start: &verify.Start{}},
			{Package: "c", PID: 3, Test: "TestC", Sim: &verify.Sim{Name: "corpus", Runs: 3, Seeds: "1-3", Transactions: 30}},
			{Package: "c", PID: 3, Test: "TestC", Reached: map[string]int{string(reach.LostAckLanded): 4}},
			{Package: "c", PID: 3, Test: "TestM", Mutant: &verify.Mutant{ID: "MUT-A", Cell: "x", Detected: true, Seed: 1}},
			{Package: "c", PID: 3, Test: "TestM", Mutant: &verify.Mutant{ID: "MUT-B", Cell: "x"}},
			{Package: "c", PID: 3, Test: "TestB", Cover: &verify.Cover{IDs: []string{"MUT-B"}, Result: verify.Passed}},
		}
		report := verify.Render(facts, c)
		for _, want := range []string{
			"**Failed.** 2 of 3 test packages failed or did not finish.",
			"| Requirements | 1 of 3 IDs verified by passing tests |",
			"- Package `b` failed (exit code 1).",
			"- Package `c` did not finish.",
			"- `b` `TestB` failed. It verifies AC-2: b works",
			"| Simulator corpus | 3 runs, seeds 1-3, 30 transactions",
			"| Reached states | 1 of 2 reached by the corpus |",
			"| `AC-2` | failed |",
			"| `AC-3` | not run |",
			"| Differential | not run |",
			"| Mutants | 2 of 2 detected: 1 by the simulator, in 1 of 2 configurations, and MUT-B only by targeted tests |",
		} {
			is.True(t, strings.Contains(report, want), fmt.Sprintf("report contains %q:\n%s", want, report))
		}
	})

	t.Run("shows the sqllogictest corpus runs and their reasons", func(t *testing.T) {
		facts := []verify.Fact{
			{Package: "s", PID: 1, Start: &verify.Start{}},
			{Package: "s", PID: 1, Test: "TestCorpus", Corpus: &verify.Corpus{Name: "make test subset", Files: 2, Statements: 100, Executed: 60, Rejected: 30, Skipped: 10, SetupFailed: 3, Baseline: 55,
				RejectedFor: map[string]int{"DISTINCT_is_not_supported": 20, "a|b": 10}, SkippedFor: map[string]int{"rejected: UNIQUE": 10}}},
			{Package: "s", PID: 1, End: &verify.End{Code: 0}},
		}
		report := verify.Render(facts, c)
		for _, want := range []string{
			"| sqllogictest make test subset | 2 files, 100 statements: 60 executed on both sides (60%), 30 rejected as unsupported (30%), 10 skipped because they depend on a table whose creation was rejected or failed; 3 setup statements failed on Postgres and maragudb alike; **0 mismatches**; 0 files below their baseline of executed statements, which sums to 55 |",
			"| rejected | `DISTINCT_is_not_supported` | 20 |",
			"| rejected | `a\\|b` | 10 |",
			"| skipped | `rejected: UNIQUE` | 10 |",
		} {
			is.True(t, strings.Contains(report, want), fmt.Sprintf("report contains %q:\n%s", want, report))
		}
		is.True(t, strings.Contains(verify.Render(nil, c), "| sqllogictest corpus | not run |"), "not run")
	})

	t.Run("shows the benchmark metrics and how many differ from the baseline", func(t *testing.T) {
		facts := []verify.Fact{
			{Package: "b", PID: 1, Start: &verify.Start{}},
			{Package: "b", PID: 1, Test: "TestWorkloads", Bench: &verify.Bench{Workload: "reads", Metric: "GETs", Measured: 0.5, Baseline: 0.5, HasBaseline: true}},
			{Package: "b", PID: 1, Test: "TestWorkloads", Bench: &verify.Bench{Workload: "reads", Metric: "PUTs", Measured: 1.25, Baseline: 2, HasBaseline: true, Change: "better"}},
			{Package: "b", PID: 1, Test: "TestWorkloads", Bench: &verify.Bench{Workload: "writes", Metric: "PUTs", Measured: 3, Change: "worse"}},
			{Package: "b", PID: 1, Test: "TestWorkloads", Bench: &verify.Bench{Workload: "writes", Metric: "bytes written", Measured: 1e6, Baseline: 0.9402285379431585, HasBaseline: true, Change: "worse"}},
			{Package: "b", PID: 1, End: &verify.End{Code: 1}},
		}
		report := verify.Render(facts, c)
		for _, want := range []string{
			"<details><summary>Benchmark metrics: 3 of 4 differ from the baseline</summary>",
			"| writes | bytes written | 1000000 | 0.9402 | worse |",
			"| reads | GETs | 0.5 | 0.5 |  |",
			"| reads | PUTs | 1.25 | 2 | better |",
			"| writes | PUTs | 3 | none | worse |",
		} {
			is.True(t, strings.Contains(report, want), fmt.Sprintf("report contains %q:\n%s", want, report))
		}
	})

	t.Run("uses only the latest run of a package", func(t *testing.T) {
		facts := []verify.Fact{
			{Package: "a", PID: 1, Start: &verify.Start{}},
			{Package: "a", PID: 1, End: &verify.End{Code: 1}},
			{Package: "a", PID: 2, Start: &verify.Start{}, Time: time.Unix(1, 0)},
			{Package: "a", PID: 2, End: &verify.End{Code: 0}},
		}
		report := verify.Render(facts, c)
		is.True(t, strings.Contains(report, "**Passed.** All 1 test packages passed."), report)
	})
}

func TestReadFacts(t *testing.T) {
	t.Run("skips lines that do not decode, such as one cut short by a crash", func(t *testing.T) {
		dir := t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(dir, "pkg-a.jsonl"), []byte(`{"package":"a","start":{}}`+"\n"+`{"package":"a","cov`), 0o644))
		facts, err := verify.ReadFacts(dir)
		is.NotError(t, err)
		is.Equal(t, 1, len(facts))
		is.True(t, facts[0].Start != nil)
	})
}

func TestCatalog_IDs(t *testing.T) {
	t.Run("has every requirement, mutant, and state", func(t *testing.T) {
		_, c, _ := load(t)
		ids := c.IDs()
		for _, id := range []string{"AC-1.5", "INV-STRICT-SERIALIZABLE", "INV-PG-SUBSET", "UNS-NUMERIC", "DEV-20", "MUT-COMMIT-WITHOUT-CAS", string(reach.LeaseOverlap)} {
			is.True(t, ids[id], id)
		}
		var kinds []string
		for _, r := range c.Requirements {
			if !slices.Contains(kinds, r.Kind()) {
				kinds = append(kinds, r.Kind())
			}
		}
		is.EqualSlice(t, []string{"INV", "AC", "UNS", "DEV"}, kinds)
	})
}

func TestSeedRanges(t *testing.T) {
	t.Run("merges consecutive seeds into sorted ranges", func(t *testing.T) {
		is.Equal(t, "1-3, 5, 7-8", verify.SeedRanges([]uint64{8, 1, 2, 3, 5, 7, 2}))
		is.Equal(t, "", verify.SeedRanges(nil))
	})

	t.Run("summarizes many ranges", func(t *testing.T) {
		is.Equal(t, "0, 2, 4, 6, 8, 10, 12, 14, and 2 more", verify.SeedRanges([]uint64{0, 2, 4, 6, 8, 10, 12, 14, 16, 18}))
	})
}
