# Diary: Detect the mutants that the simulator finds only rarely

The kill matrix calls a cell undetectable when its `cannot` reason explains why no checker can find the mutant there. After the segment key filters landed, cold reads got cheaper, transactions got shorter, and the simulator produced less concurrency. Six cells lost their detection and got `cannot` reasons. Those reasons cite measured rates of 2 to 11 detections in 100 seeds. A mutant that is found 2 times in 100 is not undetectable; the simulator rarely reaches the interleaving it needs. The cells are in `MUT-COMMIT-WITHOUT-CAS`, `MUT-COMPACT-REBASE-ON-COMPACTION`, and the GC orphan cells of the compaction mutants.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The filter work pushed six cells into `cannot` with reasons that rest on rates, not on impossibility. The concurrency mutants are the ones that matter most for a database on S3, so the simulator must find them again.
**Inferred intent:** The kill matrix is the evidence that the checkers work. A `cannot` must mean cannot.

### What I did
I wrote the requirement and gave it to the builder `rare-detections`.
- **Goal.** Each of the six cells is detected within the seed budget of `make test`, which is 20 seeds for a cell that can detect its mutant. The test time of the simulator stays flat. Where a cell really cannot detect its mutant, its reason says why, with no rate.
- **How.** The builder decides. Some options:
  - Raise contention in the simulator, so that transactions overlap more: more delay in the simulated store, more clients, or longer transactions.
  - Put GC passes inside the window between a compaction's segment write and its manifest write.
  - Add configurations for these mutants.
  The builder explains why each change makes the interleaving likely, not just why it is possible.
- **Rule for reasons.** A `cannot` reason states a structural cause: no checker can see the effect in that workload or mode. A reason that cites a detection rate, such as "in N of the first 100 seeds", is not allowed. The builder checks every existing reason against this rule, not only the six.
- **No loss elsewhere.** No other cell loses its detection. Every STATE is still reached, and the soak still passes.

### Why
A kill matrix that writes down a weak detection as undetectable hides the very thing it is there to measure. The checkers can still be fine, but there is no evidence of it.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
When the code under test gets faster, a simulator can lose detection power. Concurrency mutants depend on overlap, and cheaper reads shorten the overlap.

### What was tricky
Nothing yet. If contention goes up across the board, every history changes, and other cells may move.

### What warrants review
The reasons that remain, and whether any detection relies on one seed in particular.

### Future work
None yet.

## Step 2: Make the six cells detect robustly, and every `cannot` reason structural

**Author:** rare-detections

### Prompt Context

**Verbatim prompt:** The lead's brief for the builder `rare-detections`: make each of the six cells (two of `MUT-COMMIT-WITHOUT-CAS`, two of `MUT-COMPACT-REBASE-ON-COMPACTION`, two GC-orphan cells of `MUT-GC-DELETE-REFERENCED`) detected within the 20-seed budget robustly, report the rates over 100 seeds before and after, audit every `cannot` reason against the rule that it states a structural cause, consider enforcing the rule in the meta-test, lose no detection elsewhere, keep the test time of `internal/sim` flat.
**Interpretation:** Find out why each cell is rare, fix the cause, not the seed; turn every `cannot` cell that some seed detects into a detected cell; reject rate reasons mechanically.
**Inferred intent:** A `cannot` in the kill matrix must mean cannot, and the concurrency mutants must be caught by the simulator, not only by luck.

### What I did
- Measured first. A scratch test (not committed) ran every applicable cell over 100 seeds, and every `cannot` cell over 200 and later 300 seeds. Baseline: the six cells at 2, 11, 3, 5, 2, and 5 in 100. The audit found three more `cannot` cells that some seed detects (`MUT-GC-DELETE-REFERENCED` in list-append through SQL, 16 in 200; `MUT-COMPACT-REBASE-ON-COMPACTION` in bank through the engine, 4 in 200, and in unique through SQL, 1 in 200), and one reason that cited "20 seeds" (`MUT-COMMIT-WITHOUT-CAS`, unique through SQL without maintainers, 0 in 200).
- Diagnosed with temporary counters in the engine (reverted). For the lost-commit mutants the interleaving was not rare: in more than half of the runs with two maintainers a compaction lost to another compaction, and `MUT-COMMIT-WITHOUT-CAS` drops commits in every run. The checkers were blind: the bank, unique, and foreign-key workloads did not check that acknowledged writes survive. For `MUT-GC-DELETE-REFERENCED` the interleaving itself was rare: 1 to 7 runs in 100 had a GC pass stamp a compaction output that then landed, because the two maintainers' compactions mostly lost to each other, and the stamped output must also still be live a grace period later.
- Added durability checks that change no history:
  - `history.CheckBankDurability` in `/internal/history/history.go`: final balances = initial + every acknowledged transfer + one subset of the transfers with an unknown outcome. Equal transfers form groups with a multiplicity; meet-in-the-middle over the groups, at most 2^20 combinations per half, else `history.ErrUndecided`, which the simulator counts in `Stats.Undecided` and the corpus test requires to be 0.
  - The unique workload (`/internal/sim/unique.go`) records every write (inserted rows with their fresh ids, the email it moves or deletes by, the ids a transaction moves) and every audit's rows. After the run, `userCheck` requires every row of an acknowledged insert in every later audit and in the final state, as inserted, unless a write that did not fail and was not acknowledged before the insert began names its email or found its id; and no row of a failed write anywhere. Audits are checked after the run because a write whose outcome is not yet known may be in a snapshot.
  - The foreign-key workload (`/internal/sim/foreignkey.go`): `foreignKeyCheck` does the same for children and toys with ids outside the key pool, which only a delete of their parent removes; and the counter must end at or above the highest acknowledged value, with no two acknowledged increments at the same value.
- Added three targeted faults to `sim.Config` in `/internal/sim/sim.go`, off by default so every other history stays the same: `CompactionStall` (a maintainer stalls once per compaction, before the manifest write after its output, for half to all of the value), `OneCompactor` (only maintainer 0 compacts), and `CommitPartitions` (a client is partitioned for 300 ms right after a manifest write lands, with `s3fake.Client.ExtendUnavailable`, new, which never shortens an existing partition). Mutant fields set them: `MUT-GC-DELETE-REFERENCED` 900 ms with one compactor, `MUT-COMPACT-REBASE-ON-COMPACTION` 500 ms, `MUT-RETRY-UNKNOWN` 0.4. `make replay` takes `COMPACTION_STALL`, `ONE_COMPACTOR`, and `COMMIT_PARTITIONS`. Twelve corpus configurations (seeds 900 to 911) run them with correct code, and `TestRun_targetedFaults` checks that they do what they say.
- Why each makes its interleaving likely: the stall of 450 to 900 ms is longer than an iteration of the other maintainer (a sleep of up to 200 ms and a GC pass of up to about 100 ms) and long enough for several commits, so a GC pass reads a manifest past the compaction's base and lists the output before the swap; with one compactor the stalled compaction rebases instead of losing, so the stamped output lands and is usually still in the final manifest, which the storage check's GC passes then break. For the rebase mutant the 250 to 500 ms stall leaves time for the other maintainer's compaction to land and for commits after it, which the mutated rebase loses; longer stalls push compactions past half the grace period and lowered the rate. For the retry mutant, the engine resolves most lost acknowledgements by reading the manifest again, and only a partition right after the write keeps the outcome unknown in a live process.
- `TestMutants` now fails a `cannot` reason that has a digit or a frequency word (`rateWord`, tested by `TestRateWord`). Removed every `cannot` entry that a seed detects (19 cells, including `MUT-ACK-BEFORE-CONFIRM`, `MUT-SKIP-FRESHNESS`, `MUT-COMPACTED-AHEAD`, and `MUT-RETRY-UNKNOWN` cells that the new checks catch), the constants `bankConserves` and `ackConserves`, and rewrote the `MUT-SKIP-FRESHNESS` bank reason. The README and a decision entry were written by a writer sub-agent and reviewed.

### Why
The rate reasons hid that the simulator could see the effect and usually did not. Checks that see a lost acknowledged write in every workload make the lost-commit mutants detectable wherever the interleaving happens, which it does in most runs. Only the GC orphan race and the unknown-outcome retry needed faults, and those are scoped to the mutants so that other cells, some of which detect at 2% to 15%, keep their histories.

### What worked
- Rates over 100 seeds, before and after: `MUT-COMMIT-WITHOUT-CAS` bank through SQL without maintainers 2 → 99, unique through SQL with 2 maintainers 11 → 99 (and without maintainers 0 → 97); `MUT-GC-DELETE-REFERENCED` bank through the engine 3 → 55, foreign-key 5 → 57 (list-append through SQL 10 → 58, the others 54 to 62); `MUT-COMPACT-REBASE-ON-COMPACTION` bank through SQL 2 → 83, foreign-key 5 → 43 (bank through the engine 2 → 71, unique 0 → 58). Every one of them first detects at seed 1, 2, or 4.
- No cell of the whole matrix detects less often than before over 100 seeds; every remaining `cannot` cell is undetected in 300 seeds. `MUT-ACK-BEFORE-CONFIRM` and `MUT-COMMIT-WITHOUT-CAS` are now 12 of 12.
- A 90-second soak of 6,690 random runs with the new checks found no false alarm.
- `GOMAXPROCS=4 go test -race -count 1 ./...`: `internal/sim` 70.2 s before, 66.9 s after (whole suite 1:28 and 1:26 wall), because cells that ran five seeds to confirm a `cannot` now stop at seed 1.

### What didn't work
- Global contention: a stall before every manifest write (rates 0.1 to 0.5, up to 1 s), faster maintainers (sleeps of 50 and 20 ms), a stall after every segment write of every process, and a stall before a GC pass lists: the six cells stayed at 1% to 17%. Long stalls pushed compactions past half the grace period ("compaction is 1.067927572s old").
- A first shell loop to sweep settings silently ran without them: zsh does not split `$cfg` in `for cfg in "0.1 300ms"; do set -- $cfg`, so every row equalled the baseline. Switched to `IFS=: read`.
- Waking the other maintainer for a GC pass during the stall reached 20% to 38% until I saw in a trace that the stalled compaction lost to the other maintainer's compaction ("compaction conflict attempt 0 cur 17 base 11 extends false"); one compactor gave 47% to 69% without any waking, so the waking was dropped.
- A shorter grace period for the GC mutant made it worse (0% to 10%).
- The first Hegel property for the bank check failed without a message: it drew the history length in the loop condition and called `ht.Assume` per transaction. Drawing once and deciding about unknown outcomes up front fixed it.
- The baseline timing run first failed with `FAIL maragu.dev/maragudb/internal/sim [build failed]`, because `git stash` keeps untracked files and the new `unique_internal_test.go` stayed behind.

### What I learned
- When the cause of a rare detection is a blind checker, more contention does nothing; measure the interleaving and the detection separately.
- The free-running maintainers race each other far more than the docs suggest: two compactions overlap in more than half of the runs.
- `MUT-GC-DELETE-REFERENCED` needs the output to survive a grace period after the stamp, so the storage check after the run, which deletes stamped outputs that are still live, is what finds it most of the time.

### What was tricky
- The new row checks must allow every write whose outcome or order is uncertain: unknown outcomes, writes that overlap the insert, and audits whose snapshot may hold a write not yet recorded. Checking audits after the run solved the last.
- Client 0's `c.value()` ids are 1, 2, 3, ..., inside the foreign-key key pool, so only ids at or above twice `Keys` count as fresh.
- Stronger checks turned several `cannot` cells into detected ones, so the reason audit had to be redone after the checks, not before.

### What warrants review
- Self-review with the code-review skill at medium found ten issues; I fixed seven: the bank check silently skipped more than 40 unknown transfers (now grouped, and `ErrUndecided` is counted and must be 0 in the corpus); a commit partition could shorten an existing partition (`ExtendUnavailable`); the row checks re-scanned all writes for every audit (excuses are now computed once in `newUserCheck` and `newForeignKeyCheck`, and no-op writes are not recorded); the meet-in-the-middle materialized every subset without grouping equal transfers; `CompactionStall` stalled after every output segment rather than once per compaction (it now stalls once, before the manifest write); and `OneCompactor` claimed to cover the clients' maintenance (its comment now says it does not). I left three: `rateWord` rejects any digit, which is deliberate, since a reason can spell a number; the unique and foreign-key checks follow one pattern in two copies, kept apart because their excuses differ; and the quadratic setup per run, which is small.
- Run `go test ./internal/sim -run 'TestUserCheck|TestForeignKeyCheck|TestRun_targetedFaults|TestRateWord|TestMutants' -count 1`, `go test ./internal/history -run TestCheckBankDurability`, and `make replay SEED=1 WORKLOAD=bank MUTATION=gc-delete-referenced` to see the stall and the stamped output.

### Future work
- `MUT-RETRY-UNKNOWN` in the foreign-key cells is detected at 14 and 15 in 100 (first seeds 2 and 1), below the target: only inserts of children with fresh ids show a statement applied twice, about 8% of the statements.
- Cells that were already fragile before this work and keep their histories: `MUT-FILTER-UNCHECKED` (2 in 100 in list-append through the engine without maintainers, first seed 20), `MUT-COMPACT-DROP-TOMBSTONES` in foreign-key (3 in 100), `MUT-SKIP-FRESHNESS` in bank through the engine with maintainers (5 in 100), and `MUT-GC-CHAIN-FIRST-STAMP` (6 to 17 in 100).
- The Antithesis `parallel_driver_sim` could draw the three new faults too.
