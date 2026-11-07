# Diary: Tiered compaction with bounded write amplification

Today, compaction merges every live segment into one when there are `CompactionThreshold` segments (default 8), and every commit adds a segment. So every 8 commits rewrite the whole database. On a 1 GB database that is 1 GB of GETs and PUTs per 8 commits, however small the commits are. The request budgets count requests, not bytes, so they don't show this.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Section 3 and the bounded manifest are pushed. The cost of compaction is the next production defect.
**Inferred intent:** A database that is trustworthy in production, not only correct.

### What I did
I wrote the requirement and gave it to the builder `tiered-compaction`.
- **New criterion, `AC-6.5`, under Bounded storage:** the bytes that compaction reads and writes for each committed byte grow at most logarithmically with the database size. A deterministic test against the fake S3 checks it, for example with the same stream of small commits on a small and on a 10 times bigger database. Today's code must fail it.
- **Read cost:** the number of live segments stays O(log n) of the data size, so that AC-5.3, AC-5.5 and AC-5.8 still hold. Their tests are made strong enough to show it on a database that has had many commits, not only right after a full compaction.
- **Design:** the builder picks the design, size-tiered or leveled, and writes it as a decision entry, including how `CompactionThreshold` changes. The design must keep these properties:
  - Newer segments shadow older ones, so only adjacent segments in age order can be merged.
  - A tombstone is dropped only when nothing older can hold the key.
  - The compaction rebase and the GC rules of 2026-11-04 and 2026-11-07 keep working.
- **Mutants:** the checkers must detect at least these:
  - merging segments that are not adjacent;
  - dropping tombstones in a merge that does not include the oldest segment;
  - a rebase that loses a segment added meanwhile.

### Why
In an S3 database, each byte that compaction moves costs time and money. A cost that grows with database size makes a big database unusable, even when it receives few writes.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
The previous step gave the same lesson: a budget must count bytes, not only requests.

### What was tricky
Nothing yet. Concurrent compactions and compaction rebase are where it gets hard, because the manifest can change between the read of the inputs and the write.

### What warrants review
The shadowing order of merged segments, and when tombstones are dropped.

### Future work
Eviction in the disk cache, which never evicts today.

## Step 2: Size-tiered compaction of adjacent segments

**Author:** tiered-compaction

### Prompt Context

**Verbatim prompt:** You are the builder `tiered-compaction` for maragudb [...] Compaction today merges every live segment into one when there are `CompactionThreshold` (default 8) segments, and each commit adds one. So every 8 commits rewrite the whole database: write amplification O(database size) per commit. [...] Add `AC-6.5` [...] Read cost: live segment count stays O(log n) of data size, so AC-5.3, AC-5.5, AC-5.8 keep holding. [...] Design is yours [...] Mutants [...] at least merging non-adjacent segments, dropping tombstones in a merge that excludes the oldest segment, and a rebase that loses a segment added meanwhile.
**Interpretation:** Replace the merge-everything compaction with a policy whose bytes per committed byte grow with log(size), keep the segment count logarithmic, keep shadowing, tombstones, rebase and GC correct, and prove each new step with a mutant that a checker kills.
**Inferred intent:** A database whose maintenance cost does not grow with its size, without giving up any correctness guarantee.

### What I did
- Wrote `AC-6.5` in `/docs/requirements.md`.
- `/internal/engine/compact.go`: `Tiers` (fan-in F, `BaseBytes` 1 KiB, `FullRatio` 1) and `Tiers.plan`. A segment's tier comes from its size, and counts as the highest tier of itself and of every newer segment, so tiers never rise toward the newest segment. A run of at least F adjacent segments in one tier is merged in windows from its oldest segment, each with as many segments as fit below the next tier's limit. With at least F segments, the whole database is merged when the newer segments are as large as the oldest, or hold tombstones for as many of its entries as remain. `compact` now takes a plan, merges every window (`mergeSegments`, which keeps tombstones unless the window starts at index 0), and swaps all outputs in one CAS (`replaceMerged`). The rebase is unchanged in spirit: only on a manifest that `extends` the base, with the newer segments after everything. `DB.Compact` still merges everything, and `DB.CompactTiers` is new.
- `/internal/engine/tx.go`: `Iterator.keepTombstones`. `/internal/engine/segment.go` and `/internal/engine/manifest.go`: `segmentMeta.Tombstones`, an optional field, so the manifest version stays 2.
- `/internal/engine/maintenance.go`: `MaintenanceOptions.Tiers` replaces `SegmentThreshold`. A step compacts again while a merge is due, up to 16 times. `/maragudb.go`: `Options.CompactionFanIn` (default 4) replaces `CompactionThreshold`. `Stats` gained `CommittedBytes`, `CompactionBytesRead`, and `CompactionBytesWritten`.
- Mutations: `CompactNonAdjacent`, `CompactDropTombstones`, `CompactRebaseLosesSegment`, `CompactRebaseOnCompaction` (kill matrix and engine `TestMutations`), and `CompactAll`, the old behavior, which only the `AC-6.5` test uses to show that it fails the old design.
- Tests: `/internal/engine/compact_internal_test.go` (plan examples, and two hegel properties: fewer than F segments per tier once nothing is due, whatever the merges discard; and bytes moved per committed byte below the per-tier bound for random commit streams). `TestDB_CompactTiers` in `/internal/engine/engine_test.go` (tombstones kept, windows on both sides of a segment left alone, partial rebase, `ErrConflict` against another compaction, the tombstone trigger). `TestMaintainer_Compaction` in `/internal/engine/maintenance_test.go` for `AC-6.5`, which also checks the per-tier segment bound from the manifest after every step. The root request budget tests now load through `maintain` and `churn` (150 small updates, inserts, and deletes with maintenance after each) and measure each cold read twice, tiered and after `Compact`.
- Simulator (`/internal/sim/sim.go`): `Config.Tiers` (fan-in 3, 64-byte base, full ratio 2), maintainers that alternate whole and tiered compactions, and an engine list-append workload that keeps each list in a packed key and an overflow key (`k00+`), which a pack deletes. Four new `STATE-COMPACTION-*` states in `/internal/reach/reach.go`.

### Why
The cost of a compaction must be paid by the bytes it merges climbing a tier, not by the size of the whole database, and the tiers must keep the segment count logarithmic so that cold reads stay bounded.

### What worked
- The hegel write-amplification property caught the old policy at once: with the plan replaced by "merge everything at F segments", it fails with a reproduction blob. That made the property a good guard before any engine code ran.
- `AC-6.5`, measured on the same 1,000 commits (an insert, an update, and a delete each, with a maintenance step after each): 11.7 bytes read and written per committed byte at 2,000 keys and 9.4 at 20,000. With `CompactAll`, the old design: 522.6 and 5,008.1, about ten times as much. Growing an empty database from 300 commits of ten new keys to 3,000 takes it from 7.0 to 10.9, with at most 10 and 16 segments (an earlier run with 500 and 5,000 commits gave 8.0 and 12.1, with 12 and 17).
- The probe of the base size: bytes per committed byte at 2,000 keys were 81.6 with a 16 KiB base, 25.9 with 4 KiB, 16.7 with 2 KiB, 11.7 with 1 KiB, and 9.6 with 256 bytes, against at most 6, 8, 10, 11, and 14 segments. 1 KiB is the knee.
- Cold reads at 100,000 rows, tiered against compacted: point lookup by key 7 against 4 GETs, through a unique index 27 against 9, the join of a parent to its three children 18 against 14, and a cold insert with a foreign key 10 against 8.
- The new mutants are easy to kill: non-adjacent merges in every cell with maintainers, the lost rebase segment too, the dropped tombstones in four cells (the engine list-append workload's overflow keys and the SQL index checker), and the rebase on another compaction in four.

### What didn't work
- My first window rule took exactly F segments per window. `TestMaintenance/writes_manifests...` then failed with `maragudb_test.go:575: 72 segments`, because 30 to 50 commits pile up between two maintenance steps at the default 10 s interval, and windows of four only cut such a backlog by a factor of four per step. Windows of as many segments as fit below the next tier's limit fixed it.
- The first `CompactNonAdjacent` could skip every member of a window, and the simulator panicked: `panic: runtime error: index out of range [0] with length 0` at `compact.go:330`. `compact` now returns early when no merge is left.
- The first `CompactRebaseOnCompaction` rebased only when the newer manifest had at least as many segments. It was almost never detected, so it now rebases on any newer manifest, and keeps the segments after as many as it began with.
- The kill matrix is sensitive to the dynamics. With the maintainers' whole compactions at a quarter, `MUT-GC-CHAIN-FIRST-STAMP` went undetected in three SQL cells and `MUT-SKIP-FRESHNESS` in the bank engine cell, because tiered merges make less garbage for old readers to lose. At half, those came back, `MUT-GC-DELETE-REFERENCED` became detectable in more SQL cells, and `MUT-FRESH-SNAPSHOT` lost its foreign key cell with two maintainers (8 of 100 seeds detect it, the first at 31). I tried fan-ins 2 to 4 and bases 32 to 128 bytes, with no better result, and wrote honest reasons for the undetected cells.
- After the review fixes, the lease test failed with `sim_test.go:394: Not true` because a step can now land several compactions while the simulator still counted one per step. It counts the engine's landed compactions now.
- My first tombstone trigger, tombstones·R ≥ entries, got more eager with a larger ratio, and three `TestDB_CompactTiers` subtests failed because `smallTiers` (ratio 100) then merged everything. The condition is now tombstones·(R+1) ≥ R·entries.

### What I learned
- With size tiers, the tier of a segment must not be its own alone. A segment that deletes made smaller than a newer one would otherwise never merge again. Counting it in the tier of the newer one, the highest tier of itself and all newer segments, makes the tiers monotonic and gives the per-tier bound for any history.
- The mutant kill matrix measures detection power of the whole simulator, so a change to compaction moves cells of GC and SQL mutants that I never touched.

### What was tricky
- Tier 0. Segments below the base merge again and again until their merge leaves the tier, so a large base costs a constant factor per committed byte (82 at 16 KiB), and a small one adds tiers of one-block segments that cold reads must visit.
- Whether a cold read stays O(log n). It descends into every segment whose key range holds the key, so the honest bound is segments times levels, O(log² n) in theory. The tests now bound GETs by the segment count times the depth, and the segment count by three per tier.
- Rebasing on another compaction looks safe when both merged contiguous windows of the same age order, but I kept the old rule that gives up, because it needs its own proof.

### What warrants review
- `Tiers.plan` in `/internal/engine/compact.go`: the window growth and the whole-database trigger. Run `go test ./internal/engine -run 'TestTiers|TestMaintainer_Compaction|TestDB_CompactTiers|TestMutations' -v`.
- `mergeSegments`: tombstones are dropped only when `members[0] == 0`.
- `replaceMerged` and the rebase in `compact`: outputs at the oldest member, newer segments after everything, and no rebase unless `extends`.
- The undetected kill-matrix cells and their reasons in `/internal/sim/sim_test.go`, and the AC-6.4 segment bound, now 68 (`maragudb_test.go`), which the review fixes brought to at most 56 in practice.

### Future work
- Merge the windows of one compaction in parallel. Today they run one after another, which brings a large compaction closer to the half-grace-period cutoff.
- Rebase on a manifest in which another compaction merged other segments, with its own proof.
- Cheaper cold reads across many segments, for example key filters per segment.

## Step 3: Self-review and the checks

**Author:** tiered-compaction

### Prompt Context

**Verbatim prompt:** Run the code-review skill at medium on your diff and fix what's real.
**Interpretation:** Review the diff with competing reviewers, fix the real findings, then run every check the lead listed.
**Inferred intent:** No defect that a reviewer could have found goes to the lead.

### What I did
The code review at medium returned nine findings. I fixed six:
- Space of mass deletes was never reclaimed: tombstones are small, so the newer segments never outweighed the oldest. Segments now record their tombstones (`segmentMeta.Tombstones`), and the whole database is merged when the newer segments delete FullRatio times as many of the oldest's entries as remain. `TestDB_CompactTiers` covers it.
- A tiny database with steady updates merged everything on every step, because one new segment already outweighed the oldest. The merge of the whole database now needs at least F segments.
- The `AC-6.5` test checked only the total segment count. It now parses the manifest after every step and checks fewer than four segments per counted tier, and the hegel property checks per tier too.
- Merges that cascade waited one interval per tier. A step now compacts again while a merge is due, up to 16 times.
- The window growth `size+segs[e].Size < limit` could overflow, and `tier` was quadratic. Both are rewritten.
I left three: the windows of a compaction run one after another (a follow-up), the removal of `CompactionThreshold` (pre-release, as the lead allowed), and the two maps in `replaceMerged` (needed for the non-adjacent mutant).

Then I cut the `AC-6.5` tests from 37 s to 8 s under race, with parallel subtests, 300 commits for the mutant runs, and 300 to 3,000 commits for the growth test, and marked the two kill-matrix cells that never meet a tombstone as not applicable.

### Why
The mass-delete finding was a real product defect, the old design reclaimed that space within eight commits. The others made the tests weaker than their claims or made maintenance slower than it needed to be.

### What worked
- `make test` passes, Tigris included. `make lint` reports 0 issues. `make verification` and `go mod tidy` leave no diff.
- `make soak`: 24,702 simulator runs in 5 minutes without anomalies, and the full sqllogictest corpus in 11 min 33 s (11 min 26 s before): 5,860,887 of 5,939,714 statements executed, as before, with zero mismatches. Total 16 min 35 s.
- `GOMAXPROCS=4 go test -race -shuffle on -count=1 ./...`, before (90366b2) and after the first two commits: root 63.1 s and 41.2 s, engine 28.1 s and 65.3 s, sim 95.5 s and 101.4 s, sqllogictest 68.1 s and 66.4 s, pgdiff 28.7 s and 31.1 s, workload 9.4 s and 11.3 s, the others unchanged. After the last commit, engine alone is 30.5 s to 33.3 s, and sim alone 74.6 s before and 81.0 s after, the cost of the four new mutants.

### What didn't work
- The fix for the tombstone trigger first had the ratio backwards, see Step 2.
- Marking the cells that never meet a tombstone as not applicable did not measurably shorten the sim package (81.0 s both times); the four new mutants cost about 6 s under race.

### What I learned
A trigger in bytes alone misses deletes, whose bytes are small and whose effect is large. Counting entries, not bytes, for tombstones is the cheap fix.

### What was tricky
Keeping the kill matrix stable while the review fixes changed maintenance. The step loop changed client maintenance only, so the dedicated maintainers of the kill-matrix cells kept their dynamics, and only `MUT-GC-DELETE-REFERENCED` lost its bank SQL cell.

### What warrants review
- The tombstone condition `tombstones*(FullRatio+1) >= FullRatio*Count` in `Tiers.plan`, and that a manifest without `tombstones` reads as none.
- The step loop in `Maintainer.maintain`, bounded at 16 compactions.

### Future work
- Merge the windows of one compaction in parallel.
- The sim package grew by about 6 s under race, which CI's 600 s budget absorbs.
