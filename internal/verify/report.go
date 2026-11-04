package verify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadFacts from every facts file in dir. Lines that do not decode, such as a line cut short by
// a crash, are skipped.
func ReadFacts(dir string) ([]Fact, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var facts []Fact
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var f Fact
			if json.Unmarshal(scanner.Bytes(), &f) == nil {
				facts = append(facts, f)
			}
		}
		err = scanner.Err()
		_ = file.Close()
		if err != nil {
			return nil, err
		}
	}
	return facts, nil
}

// latestRuns keeps, for every package, only the facts of its latest test binary, told apart by
// the PID of its start. Facts of packages without a start, such as the Antithesis harness's,
// are all kept.
func latestRuns(facts []Fact) []Fact {
	starts := map[string]Fact{}
	for _, f := range facts {
		if f.Start == nil {
			continue
		}
		if cur, ok := starts[f.Package]; !ok || f.Time.After(cur.Time) {
			starts[f.Package] = f
		}
	}
	var kept []Fact
	for _, f := range facts {
		if s, ok := starts[f.Package]; ok && f.PID != s.PID {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// packageRun is the outcome of one package's test binary.
type packageRun struct {
	pkg     string
	started bool
	end     *End
}

func (p packageRun) status() string {
	switch {
	case p.end == nil:
		return "did not finish"
	case p.end.Code == 0:
		return "passed"
	default:
		return fmt.Sprintf("failed (exit code %d)", p.end.Code)
	}
}

// Render the facts as a Markdown report. The catalog gives the IDs that must be verified; an
// empty catalog leaves out what depends on it.
func Render(facts []Fact, c Catalog) string {
	facts = latestRuns(facts)
	runs := map[string]*packageRun{}
	var covers []Fact
	sims := map[string]*Sim{}
	var simNames []string
	mutants := map[string]bool{}
	cells, cellsDetected := 0, 0
	reached := map[string]int{}
	reachedRecorded := false
	var diff *Diff
	var ratchet *Ratchet
	var budgets []Fact
	var latencies []Fact
	var antithesis []Fact
	for _, f := range facts {
		run := func() *packageRun {
			if runs[f.Package] == nil {
				runs[f.Package] = &packageRun{pkg: f.Package}
			}
			return runs[f.Package]
		}
		switch {
		case f.Start != nil:
			run().started = true
		case f.End != nil:
			run().end = f.End
		case f.Cover != nil:
			covers = append(covers, f)
		case f.Sim != nil:
			s, ok := sims[f.Sim.Name]
			if !ok {
				s = &Sim{Name: f.Sim.Name}
				sims[f.Sim.Name] = s
				simNames = append(simNames, f.Sim.Name)
			}
			s.Runs += f.Sim.Runs
			s.Transactions += f.Sim.Transactions
			s.Commits += f.Sim.Commits
			s.VirtualTime += f.Sim.VirtualTime
			s.Anomalies += f.Sim.Anomalies
			s.Seeds = joinNonEmpty(s.Seeds, f.Sim.Seeds)
		case f.Mutant != nil:
			cells++
			if f.Mutant.Detected {
				cellsDetected++
			}
			mutants[f.Mutant.ID] = mutants[f.Mutant.ID] || f.Mutant.Detected
		case f.Reached != nil:
			reachedRecorded = true
			for s, n := range f.Reached {
				reached[s] += n
			}
		case f.Diff != nil:
			if diff == nil {
				diff = &Diff{}
			}
			diff.Cases += f.Diff.Cases
			diff.Statements += f.Diff.Statements
			diff.OK += f.Diff.OK
			diff.Errors += f.Diff.Errors
			diff.ClassMatches += f.Diff.ClassMatches
			diff.Classes = max(diff.Classes, f.Diff.Classes)
			diff.Ordered += f.Diff.Ordered
			diff.Executed += f.Diff.Executed
			diff.Rejected += f.Diff.Rejected
			diff.Transactions += f.Diff.Transactions
		case f.Ratchet != nil:
			ratchet = f.Ratchet
		case f.Budget != nil:
			budgets = append(budgets, f)
		case f.Latency != nil:
			latencies = append(latencies, f)
		case f.Antithesis != nil:
			antithesis = append(antithesis, f)
		}
	}

	var pkgs []*packageRun
	for _, r := range runs {
		if r.started {
			pkgs = append(pkgs, r)
		}
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].pkg < pkgs[j].pkg })

	var b strings.Builder
	b.WriteString("## maragudb verification report\n\n")
	var bad []*packageRun
	for _, p := range pkgs {
		if p.status() != "passed" {
			bad = append(bad, p)
		}
	}
	switch {
	case len(pkgs) == 0:
		b.WriteString("No test package has run yet.\n")
	case len(bad) == 0:
		fmt.Fprintf(&b, "**Passed.** All %d test packages passed.\n", len(pkgs))
	default:
		fmt.Fprintf(&b, "**Failed.** %d of %d test packages failed or did not finish.\n", len(bad), len(pkgs))
	}

	// The status of every ID from the tests that declared it.
	status := map[string]Result{}
	var failedCovers []Fact
	for _, f := range covers {
		if f.Cover.Result == Failed {
			failedCovers = append(failedCovers, f)
		}
		for _, id := range f.Cover.IDs {
			status[id] = better(status[id], f.Cover.Result)
		}
	}
	var unverified []string
	for _, r := range c.Requirements {
		if status[r.ID] != Passed {
			unverified = append(unverified, r.ID)
		}
	}

	b.WriteString("\n| Check | Result |\n|---|---|\n")
	if len(c.Requirements) > 0 {
		fmt.Fprintf(&b, "| Requirements | %d of %d IDs verified by passing tests |\n", len(c.Requirements)-len(unverified), len(c.Requirements))
	}
	if len(simNames) == 0 {
		b.WriteString("| Simulator | not run |\n")
	}
	for _, name := range simNames {
		s := sims[name]
		fmt.Fprintf(&b, "| Simulator %s | %d runs, seeds %s, %d transactions (%d commits), %v virtual time, **%d anomalies** |\n", s.Name, s.Runs, s.Seeds, s.Transactions, s.Commits, s.VirtualTime.Round(time.Millisecond), s.Anomalies)
	}
	if len(mutants) == 0 {
		b.WriteString("| Mutants | not run |\n")
	} else {
		bySim := 0
		var byTests []string
		for id, d := range mutants {
			switch {
			case d:
				bySim++
			case status[id] == Passed:
				byTests = append(byTests, id)
			}
		}
		sort.Strings(byTests)
		total := max(len(c.KillMatrix.Mutants), len(mutants))
		fmt.Fprintf(&b, "| Mutants | %d of %d detected: %d by the simulator, in %d of %d configurations", bySim+len(byTests), total, bySim, cellsDetected, cells)
		if len(byTests) > 0 {
			fmt.Fprintf(&b, ", and %s only by targeted tests", strings.Join(byTests, ", "))
		}
		b.WriteString(" |\n")
	}
	if !reachedRecorded {
		b.WriteString("| Reached states | not run |\n")
	} else {
		n := 0
		for _, s := range c.States {
			if reached[string(s.State)] > 0 {
				n++
			}
		}
		fmt.Fprintf(&b, "| Reached states | %d of %d reached by the corpus |\n", n, len(c.States))
	}
	if diff == nil {
		b.WriteString("| Differential | not run |\n")
	} else {
		fmt.Fprintf(&b, "| Differential | %d cases, %d statements: %d executed on both sides (%s), %d rejected as unsupported (%s); of the executed, %d succeeded on both and %d failed on both, of which %d with the same error class, across %d class pairs; %d explicit transactions |\n",
			diff.Cases, diff.Statements, diff.Executed, percent(diff.Executed, diff.Statements), diff.Rejected, percent(diff.Rejected, diff.Statements), diff.OK, diff.Errors, diff.ClassMatches, diff.Classes, diff.Transactions)
	}
	if ratchet == nil {
		b.WriteString("| Differential ratchet | not run |\n")
	} else {
		fmt.Fprintf(&b, "| Differential ratchet | %d cases with a fixed seed: %d of %d statements executed (%s), baseline %d |\n", ratchet.Cases, ratchet.Executed, ratchet.Statements, percent(ratchet.Executed, ratchet.Statements), ratchet.Baseline)
	}
	if len(budgets) == 0 {
		b.WriteString("| Request budgets | not run |\n")
	} else {
		within := 0
		for _, f := range budgets {
			if f.Budget.Measured <= f.Budget.Limit {
				within++
			}
		}
		fmt.Fprintf(&b, "| Request budgets | %d of %d measurements within budget |\n", within, len(budgets))
	}
	if len(latencies) == 0 {
		b.WriteString("| Tigris latencies | not run |\n")
	} else {
		var ds []time.Duration
		for _, f := range latencies {
			ds = append(ds, f.Latency.Duration)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		fmt.Fprintf(&b, "| Tigris latencies | %d measured, median %v, max %v |\n", len(ds), ds[len(ds)/2].Round(time.Millisecond), ds[len(ds)-1].Round(time.Millisecond))
	}
	if len(antithesis) == 0 {
		b.WriteString("| Antithesis harness | not run |\n")
	} else {
		passed := 0
		for _, f := range antithesis {
			if f.Antithesis.Passed {
				passed++
			}
		}
		fmt.Fprintf(&b, "| Antithesis harness | %d of %d test commands passed locally |\n", passed, len(antithesis))
	}

	if len(bad) > 0 || len(failedCovers) > 0 {
		b.WriteString("\n### What failed\n\n")
		for _, p := range bad {
			fmt.Fprintf(&b, "- Package `%s` %s.\n", p.pkg, p.status())
		}
		for _, f := range failedCovers {
			fmt.Fprintf(&b, "- `%s` `%s` failed. It verifies %s: %s\n", f.Package, f.Test, strings.Join(f.Cover.IDs, ", "), f.Cover.What)
		}
	}

	if len(pkgs) > 0 {
		b.WriteString("\n<details><summary>Packages</summary>\n\n| Package | Result | Time |\n|---|---|---|\n")
		for _, p := range pkgs {
			elapsed := "-"
			if p.end != nil {
				elapsed = p.end.Elapsed.Round(time.Millisecond).String()
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", p.pkg, p.status(), elapsed)
		}
		b.WriteString("\n</details>\n")
	}
	if len(unverified) > 0 && len(covers) > 0 {
		b.WriteString("\n<details><summary>Requirements not verified by a passing test in this run</summary>\n\n| ID | Status |\n|---|---|\n")
		for _, id := range unverified {
			s := string(status[id])
			if s == "" {
				s = "not run"
			}
			fmt.Fprintf(&b, "| `%s` | %s |\n", id, s)
		}
		b.WriteString("\n</details>\n")
	}
	if reachedRecorded {
		b.WriteString("\n<details><summary>Reached states</summary>\n\n| State | Count |\n|---|---|\n")
		for _, s := range c.States {
			fmt.Fprintf(&b, "| `%s` | %d |\n", s.State, reached[string(s.State)])
		}
		b.WriteString("\n</details>\n")
	}
	if len(mutants) > 0 {
		b.WriteString("\n<details><summary>Mutants</summary>\n\n| Mutant | Configuration | Result |\n|---|---|---|\n")
		for _, f := range facts {
			if m := f.Mutant; m != nil {
				result := "not detected"
				if m.Detected {
					result = fmt.Sprintf("detected with seed %d by %s", m.Seed, strings.Join(m.Checkers, ", "))
				}
				fmt.Fprintf(&b, "| `%s` | %s | %s |\n", m.ID, m.Cell, result)
			}
		}
		b.WriteString("\n</details>\n")
	}
	if len(budgets) > 0 {
		b.WriteString("\n<details><summary>Request budgets</summary>\n\n| Measurement | Database size | Requests | Budget |\n|---|---|---|---|\n")
		for _, f := range budgets {
			fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", f.Budget.Name, f.Budget.Size, f.Budget.Measured, f.Budget.Limit)
		}
		b.WriteString("\n</details>\n")
	}
	if len(latencies) > 0 {
		b.WriteString("\n<details><summary>Tigris latencies</summary>\n\n| Operation | Latency |\n|---|---|\n")
		for _, f := range latencies {
			fmt.Fprintf(&b, "| %s | %v |\n", f.Latency.Name, f.Latency.Duration.Round(time.Millisecond))
		}
		b.WriteString("\n</details>\n")
	}
	if len(antithesis) > 0 {
		b.WriteString("\n<details><summary>Antithesis harness</summary>\n\n| Test command | Result |\n|---|---|\n")
		for _, f := range antithesis {
			result := "failed"
			if f.Antithesis.Passed {
				result = "passed"
			}
			fmt.Fprintf(&b, "| `%s` | %s |\n", f.Antithesis.Command, result)
		}
		b.WriteString("\n</details>\n")
	}
	return b.String()
}

// better of two results for one ID: a failure shows over everything, and a pass over a skip.
func better(a, b Result) Result {
	rank := map[Result]int{"": 0, Skipped: 1, Passed: 2, Failed: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

func joinNonEmpty(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + ", " + b
}

// publish the report of every package so far to the GitHub step summary, when there is one.
func publish() {
	summary := os.Getenv("GITHUB_STEP_SUMMARY")
	dir := factsDir()
	if summary == "" || dir == "" {
		return
	}
	var c Catalog
	if root, err := FindRoot(); err == nil {
		c, _ = LoadCatalog(root)
	}
	_ = publishTo(summary, dir, c)
}

// publishTo replaces the report in the summary file with one over every fact in dir. Test
// binaries of several packages publish at once, so they take turns under a lock in dir. The
// summary's content from before the first report is kept in front of it.
func publishTo(summary, dir string, c Catalog) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(dir, "lock"))
	if err != nil {
		return err
	}
	defer unlock()
	prefixPath := filepath.Join(dir, "summary-prefix")
	prefix, err := os.ReadFile(prefixPath)
	if errors.Is(err, fs.ErrNotExist) {
		prefix, err = os.ReadFile(summary)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.WriteFile(prefixPath, prefix, 0o644); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	facts, err := ReadFacts(dir)
	if err != nil {
		return err
	}
	return os.WriteFile(summary, append(prefix, Render(facts, c)...), 0o644)
}

// percent of n in total, rounded to a whole number.
func percent(n, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(total))
}
