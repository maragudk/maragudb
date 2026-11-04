// Package verify makes the automated verification visible. Tests declare which requirement,
// unsupported-statement, mutant, and reached-state IDs they verify with [Covers], and record facts about
// what they checked with [Record]. Every test binary runs through [Main], which collects the
// facts of its package as JSON lines in a directory, and renders the report of every package so
// far to the GitHub step summary when it runs in GitHub Actions.
//
// The facts directory is MARAGUDB_VERIFY_DIR when set. Otherwise, when GITHUB_STEP_SUMMARY is
// set, it is a directory in the temporary directory named after the summary file, so that the
// test binaries of one CI step share it. Otherwise nothing is recorded.
package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// module path, which package paths in the facts are relative to.
const module = "maragu.dev/maragudb"

// Fact recorded by a test binary. Exactly one of the pointer fields is set.
type Fact struct {
	// Package of the test binary, relative to the module root, such as "internal/sim".
	Package string `json:"package"`
	// PID of the test binary, which tells the facts of two runs of a package apart.
	PID  int       `json:"pid"`
	Time time.Time `json:"time"`
	// Test that recorded the fact, as [testing.T.Name] gives it.
	Test string `json:"test,omitempty"`

	Start      *Start         `json:"start,omitempty"`
	End        *End           `json:"end,omitempty"`
	Cover      *Cover         `json:"cover,omitempty"`
	Sim        *Sim           `json:"sim,omitempty"`
	Mutant     *Mutant        `json:"mutant,omitempty"`
	Reached    map[string]int `json:"reached"`
	Diff       *Diff          `json:"diff,omitempty"`
	Ratchet    *Ratchet       `json:"ratchet,omitempty"`
	Budget     *Budget        `json:"budget,omitempty"`
	Latency    *Latency       `json:"latency,omitempty"`
	Antithesis *Antithesis    `json:"antithesis,omitempty"`
}

// Start of a test binary.
type Start struct{}

// End of a test binary that returned from its tests, with the exit code of [testing.M.Run]. A
// binary that panicked or timed out has no End.
type End struct {
	Code    int           `json:"code"`
	Elapsed time.Duration `json:"elapsed"`
}

// Cover is the result of a test that declared IDs with [Covers].
type Cover struct {
	IDs    []string `json:"ids"`
	What   string   `json:"what"`
	Result Result   `json:"result"`
}

// Result of a test.
type Result string

// Results.
const (
	Passed  Result = "passed"
	Failed  Result = "failed"
	Skipped Result = "skipped"
)

// Sim is a batch of simulator runs.
type Sim struct {
	// Name of the batch, such as "corpus" or "soak".
	Name string `json:"name"`
	Runs int    `json:"runs"`
	// Seeds of the runs, as ranges such as "1-40, 100-119".
	Seeds        string        `json:"seeds"`
	Transactions int           `json:"transactions"`
	Commits      int           `json:"commits"`
	VirtualTime  time.Duration `json:"virtualTime"`
	Anomalies    int           `json:"anomalies"`
}

// Mutant result in one cell of the kill matrix.
type Mutant struct {
	ID   string `json:"id"`
	Cell string `json:"cell"`
	// Detected is true when a seed of the cell found an anomaly, first Seed, by the Checkers.
	Detected bool     `json:"detected"`
	Seed     uint64   `json:"seed,omitempty"`
	Checkers []string `json:"checkers,omitempty"`
}

// Diff is a differential run against Postgres.
type Diff struct {
	Cases      int `json:"cases"`
	Statements int `json:"statements"`
	// OK statements succeeded on both sides, and Errors failed on both, of which ClassMatches
	// failed for the same reason, across Classes distinct pairs of error class and SQLSTATE.
	OK           int `json:"ok"`
	Errors       int `json:"errors"`
	ClassMatches int `json:"classMatches"`
	Classes      int `json:"classes"`
	Ordered      int `json:"ordered"`
	// Executed statements ran on both sides; Rejected ones were rejected by maragudb as
	// unsupported and skipped on Postgres. Transactions counts explicit transactions.
	Executed     int `json:"executed"`
	Rejected     int `json:"rejected"`
	Transactions int `json:"transactions"`
}

// Ratchet is the fixed-seed differential run whose executed statements must not drop below
// the committed Baseline.
type Ratchet struct {
	Cases      int `json:"cases"`
	Statements int `json:"statements"`
	Executed   int `json:"executed"`
	Baseline   int `json:"baseline"`
}

// Budget is a measured S3 request count against its limit.
type Budget struct {
	// Name of what was measured, such as "commit PUTs".
	Name string `json:"name"`
	// Size of the database in keys or rows.
	Size     int   `json:"size"`
	Measured int64 `json:"measured"`
	Limit    int64 `json:"limit"`
}

// Latency measured against real S3-compatible storage.
type Latency struct {
	Name     string        `json:"name"`
	Duration time.Duration `json:"duration"`
}

// Antithesis is the result of one test command of the Antithesis harness, run locally.
type Antithesis struct {
	Command string `json:"command"`
	Passed  bool   `json:"passed"`
}

// Covers declares that the test verifies the given IDs, and what it asserts about them, in one
// line. The verification map in /docs/verification.md is generated from the source of these
// calls, so what and the IDs must be string literals, and the call must be inside a test
// function, in the subtest it belongs to. Call it first, before the test can skip, so that the
// report says when a covering test was skipped.
func Covers(t testing.TB, what string, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		result := Passed
		switch {
		case t.Failed():
			result = Failed
		case t.Skipped():
			result = Skipped
		}
		Record(t, Fact{Cover: &Cover{IDs: ids, What: what, Result: result}})
	})
}

// CheckBudget records a measured request count against its limit, and fails the test when the
// measurement is over it, so that the report and the test cannot disagree.
func CheckBudget(t testing.TB, name string, size int, measured, limit int64) {
	t.Helper()
	Record(t, Fact{Budget: &Budget{Name: name, Size: size, Measured: measured, Limit: limit}})
	if measured > limit {
		t.Errorf("%s at size %d: %d requests, over the budget of %d", name, size, measured, limit)
	}
}

// Record a fact for the test t. It does nothing when there is no facts directory.
func Record(t testing.TB, f Fact) {
	if t != nil {
		f.Test = t.Name()
	}
	record(f)
}

var state struct {
	once sync.Once
	dir  string
	mu   sync.Mutex
	pkg  string
}

// factsDir is the directory facts are recorded in, or "" when nothing is recorded.
func factsDir() string {
	state.once.Do(func() {
		if dir := os.Getenv("MARAGUDB_VERIFY_DIR"); dir != "" {
			state.dir = dir
			return
		}
		if summary := os.Getenv("GITHUB_STEP_SUMMARY"); summary != "" {
			sum := sha256.Sum256([]byte(summary))
			state.dir = filepath.Join(os.TempDir(), "maragudb-verify-"+hex.EncodeToString(sum[:8]))
		}
	})
	return state.dir
}

// packageOf the function name of a test or of TestMain, relative to the module.
func packageOf(function string) string {
	// The package path ends at the first dot after the last slash.
	slash := strings.LastIndex(function, "/")
	dot := strings.Index(function[slash+1:], ".")
	if dot < 0 {
		return function
	}
	pkg := strings.TrimSuffix(function[:slash+1+dot], "_test")
	if pkg == module {
		return "."
	}
	return strings.TrimPrefix(pkg, module+"/")
}

// testPackage of the running test binary, found from the call stack, or set by [Main].
func testPackage() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.pkg != "" {
		return state.pkg
	}
	pcs := make([]uintptr, 32)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		frame, more := frames.Next()
		// Facts recorded in a cleanup have no frame of the test package, only of the testing
		// package, so this only finds the package for facts recorded by the test itself.
		if strings.HasPrefix(frame.Function, module) && !strings.HasPrefix(frame.Function, module+"/internal/verify.") {
			state.pkg = packageOf(frame.Function)
			return state.pkg
		}
		if !more {
			return "unknown"
		}
	}
}

// factsFile of a package in dir.
func factsFile(dir, pkg string) string {
	name := strings.ReplaceAll(pkg, "/", "_")
	if pkg == "." {
		name = "root"
	}
	return filepath.Join(dir, "pkg-"+name+".jsonl")
}

// record appends the fact to the package's facts file, in one write, so that a crash later
// loses nothing already recorded.
func record(f Fact) {
	dir := factsDir()
	if dir == "" {
		return
	}
	f.Package = testPackage()
	f.PID = os.Getpid()
	f.Time = time.Now().UTC()
	line, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	file, err := os.OpenFile(factsFile(dir, f.Package), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.Write(append(line, '\n'))
}

// Main runs the tests of a package through [testing.M], records the start and the end of the
// binary, publishes the report, and runs the after functions before it exits.
func Main(m *testing.M, after ...func()) {
	pc, _, _, ok := runtime.Caller(1)
	if fn := runtime.FuncForPC(pc); ok && fn != nil {
		state.mu.Lock()
		state.pkg = packageOf(fn.Name())
		state.mu.Unlock()
	}
	start := time.Now()
	record(Fact{Start: &Start{}})
	publish()
	code := m.Run()
	for _, f := range after {
		f()
	}
	record(Fact{End: &End{Code: code, Elapsed: time.Since(start)}})
	publish()
	os.Exit(code)
}

// SeedRanges formats seeds as sorted, merged ranges, such as "1-40, 100-119, 300".
func SeedRanges(seeds []uint64) string {
	sorted := slices.Clone(seeds)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var parts []string
	for i := 0; i < len(sorted); {
		j := i
		for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprint(sorted[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", sorted[i], sorted[j]))
		}
		i = j + 1
	}
	if len(parts) > 8 {
		parts = append(parts[:8], fmt.Sprintf("and %d more", len(parts)-8))
	}
	return strings.Join(parts, ", ")
}
