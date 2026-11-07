# Diary: Make every detected cell of the kill matrix robust

Since the work on rare detections, a `cannot` reason may not cite a rate. But some cells that are detected find their mutant in only a few of 100 seeds. In the list below, a rate is the share of 100 seeds that find the mutant.

| Mutant | Cell | Rate |
|---|---|---|
| `MUT-FILTER-UNCHECKED` | list-append through the engine, no maintainers | 2% |
| `MUT-COMPACT-DROP-TOMBSTONES` | foreign-key | 3% |
| `MUT-SKIP-FRESHNESS` | bank through the engine, with maintainers | 5% |
| `MUT-GC-CHAIN-FIRST-STAMP` | its cells | 6% to 17% |
| `MUT-RETRY-UNKNOWN` | foreign-key | 14% to 15% |

At a rate that low, the 20 seeds of `make test` miss the mutant often. A change elsewhere that shifts the histories then turns the cell into "not detected, and no reason says why it cannot be". The rule forbids a rate reason, so the next builder would have to fix it in the middle of unrelated work.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The rare-detections work fixed the cells that had already flipped. The cells that are about to flip come next, so that the kill matrix stops moving under unrelated changes.
**Inferred intent:** A kill matrix that stays stable when unrelated code changes, because every detection is robust.

### What I did
I wrote the requirement and gave it to the builder `fragile-detections`.
- **Goal.** Every detected cell finds its mutant in at least 20% of 100 seeds. The builder first measures every cell of the matrix, not only the five listed here.
- **How.** The builder finds why each cell is rare, as the rare-detections builder did. Either the checker does not look at the effect, or the interleaving is rare. It then fixes that cause with a stronger check or a targeted fault, not by choosing a seed. Faults stay scoped to the mutants that need them, so that other histories stay the same.
- **Guard against new fragile cells.** The builder decides how a cell that drops below the floor gets noticed:
  - a measurement of the rate in `make soak` or `make verification`, which fails below the floor;
  - or something cheaper with the same effect.

  The floor must not depend on seeds that someone chose by hand.
- **Antithesis.** `parallel_driver_sim` sometimes draws the three targeted faults, `CompactionStall`, `OneCompactor`, and `CommitPartitions`, as it already does `DiskFaults` and `StoreDamage`. A test checks the proportions.
- **No loss.** No cell's rate drops, every STATE is still reached, `make soak` passes, and the simulator's test time stays flat.

### Why
A kill matrix in which detections flip at random gets distrusted and eventually ignored. A robust floor keeps it a real signal.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
At a rate of p, 20 seeds miss the mutant with probability (1−p)^20. At 5% that is 36%, and at 20% it is about 1%.

### What was tricky
Nothing yet. A gate on the rate must itself be deterministic, or it becomes the flaky test.

### What warrants review
The floor guard, and any fault that applies to more than one mutant.

### Future work
None yet.

## Step 2: Raise every detected cell to 20 of 100 seeds, and make the floor a deterministic gate

**Author:** fragile-detections

### Prompt Context

**Verbatim prompt:** The lead's brief for the builder `fragile-detections`: measure the detection rate over 100 seeds of every applicable cell; raise every detected cell to at least 20% by fixing the cause (a check that sees the effect, preferred, or a fault scoped to the mutant), never by picking seeds, or give a structural `cannot` reason; make a drop below the floor get noticed deterministically, at a cost that keeps `make test` flat; have Antithesis's `simConfig` draw `CompactionStall`, `OneCompactor`, and `CommitPartitions` with a test of the proportions; lose no cell's rate, no STATE, no coverage; report timings before and after.
**Interpretation:** Measure first. For each fragile cell, measure the interleaving and the detection separately to tell a blind checker from a rare interleaving, fix that, and then put the measurement itself into `make verification` and `make soak`.
**Inferred intent:** A kill matrix whose detections do not flip when unrelated code shifts the histories.

### What I did
- **Measured.** A scratch test, not committed, ran every applicable cell over seeds 1 to 100 (about 43 s on 10 cores without the race detector). 19 detected cells were under 20 in 100, more than the five the lead listed: `MUT-FILTER-UNCHECKED` in 6 of its 10 cells (2 to 15), `MUT-GC-CHAIN-FIRST-STAMP` in all 6 (6 to 17), `MUT-COMPACT-DROP-TOMBSTONES` in list-append through the engine (11) and foreign-key (3), `MUT-SKIP-FRESHNESS` in the two bank cells with maintainers (5, 15), `MUT-FRESH-SNAPSHOT` in foreign-key with maintainers (16), `MUT-RETRY-UNKNOWN` in both foreign-key cells (14, 15), and `MUT-CHECK-SKIP-UPDATE` in foreign-key with maintainers (17). Under 30 too: the other four `MUT-FILTER-UNCHECKED` cells (22 to 26), `MUT-FRESH-SNAPSHOT` foreign-key without maintainers (23), `MUT-CHECK-SKIP-UPDATE` foreign-key without maintainers (28), `MUT-UNIQUE-OWN-WRITES` unique with maintainers (26).
- **Diagnosed** with temporary counters in the engine and the SQL layer, reverted: for `MUT-FILTER-UNCHECKED`, damaged filter reads happened in 25 to 50 of 100 runs, but 85% to 90% of filter reads from the store are by the clients without caches; for `MUT-CHECK-SKIP-UPDATE`, a violating UPDATE was attempted in 39 and 28 of 100 runs, and the checker caught nearly every one that committed.
- **Checks that change no history** (all in `/internal/sim` unless named):
  - `history.CheckBankAudits` in `/internal/history/history.go`: an audit's balances are the initial ones plus every transfer acknowledged before it began plus one subset of the others that began before it ended and did not fail, by the same meet in the middle as `CheckBankDurability` (now sharing `explainBalances`), with a budget of 2^10 per half per audit; undecided audits count in the new `Stats.UndecidedAudits`, which the corpus requires to be 0. Checker: real-time order.
  - `commitWatch` and `client.record` in `/internal/sim/sim.go`: the client's manifest writes that reach the store (succeeded, or lost answer and the store holds the body) are recorded per transaction; a recorded transaction that failed with one, or that has two, is a durability anomaly.
  - `run.watchStore`, fed by the new `s3fake.Client.OnChange` in `/internal/s3fake/s3fake.go`: it follows the manifest's live segments; a segment deleted while live, or less than a grace period after it left the manifest, is a grace period anomaly; a manifest write that takes segments out must leave `engine.ManifestContents` the same, the new checker `compaction`. `engine.ManifestSegments` and `engine.ManifestContents` are new exports in `/internal/engine/manifest.go`.
  - `checkedDisk`: every block an engine puts in a client's disk cache must equal the store's bytes at that range (cache integrity).
  - `/internal/sim/foreignkey.go`: an audit records `snapshotBy`, the history clock when its first statement returned; no child or toy of a write that began after it may be in the audit, and the counter it reads may not exceed the highest value of an increment that began by then and did not fail (`fkTick`).
- **Configurations scoped to the mutant:** `MUT-FILTER-UNCHECKED` with `BlockSize` 64 and the new `sim.Config.DamageRate` 0.1; `MUT-COMPACT-DROP-TOMBSTONES` with the new `sim.Config.TieredCompactions`; `MUT-CHECK-SKIP-UPDATE` with `keys: 1`. `make replay` takes `DAMAGE_RATE` and `TIERED_COMPACTIONS`; `mutant.apply` now builds both the kill matrix's and the replay's configuration. The `cannot` of `MUT-SKIP-FRESHNESS` is gone: its reason said the bank workload checks no audit, which is no longer true, and those cells are at 100.
- **The floor gate:** with `-update` or `MARAGUDB_SOAK`, `TestMutants` runs seeds 1 to 100 (`rateSeeds`) in every applicable cell, records `detections` per detected cell and `rateSeeds`/`floor` in `/internal/sim/testdata/kill-matrix.json`, and fails below 20 (`rateFloor`), on a first detection after seed 20, or on any detection of a `cannot` cell. `make test` still stops at the first detection and fills the counts from the golden file. `/internal/verify/doc.go` renders the count per cell and the fewest per mutant in `/docs/verification.md`. A dated entry in `/docs/decisions.md`.
- **Tests:** `TestCheckBankAudits` (cases and a Hegel property), `TestManifestSegments`, `TestManifestContents`, `TestClient_OnChange` (also the order under 8 goroutines), `TestClient_View`, `TestClient_record`, `TestCommitWatch_Put`, `TestClient_checkCached`, `TestRun_watchStore`, new `TestForeignKeyCheck` cases, a targeted-fault test for `TieredCompactions` and `DamageRate`, 8 corpus configurations (seeds 920 to 927) with both, and the soak draws all targeted faults some of the time. `MUT-COMPACT-DROP-TOMBSTONES` now names the compaction checker, and `MUT-SKIP-FRESHNESS` and `MUT-FRESH-SNAPSHOT` the real-time checker, so those checks must keep biting.
- **Antithesis:** `simConfig` in `/antithesis/workload/main.go` draws a quarter each of `CompactionStall` (250 ms to 1 s), `OneCompactor`, `TieredCompactions`, and `CommitPartitions` (up to 0.5), independently, after the existing draws so that earlier fields keep their values; a test checks each is between a sixth and a third of 400 seeds. The README was updated by a writer sub-agent and reviewed.

### Why
Most fragile cells had a blind checker, not a rare interleaving: the mutant's effect happened in many runs and no checker looked at it. Checks are better than faults because they leave every history as it was and cover every workload at once. Where the effect itself was rare (damage that reaches a caching client, a tombstone that a merge without the oldest segment meets, a committed violating UPDATE), a configuration of that mutant alone makes it likely without touching other cells.

### What worked
- Rates over 100 seeds, before and after, for every cell under 30 before: `MUT-FILTER-UNCHECKED` 2→47, 6→48, 13→56, 12→58, 22→90, 26→86, 25→84, 25→82, 11→68, 15→67; `MUT-GC-CHAIN-FIRST-STAMP` 14→90, 15→89, 6→80, 17→93, 7→81, 8→76; `MUT-COMPACT-DROP-TOMBSTONES` list-append engine 11→36, foreign-key 3→45; `MUT-SKIP-FRESHNESS` bank 5→100 and 15→100, and the two bank cells without maintainers, `cannot` before, 100; `MUT-FRESH-SNAPSHOT` foreign-key 23→37, 16→28; `MUT-RETRY-UNKNOWN` foreign-key 14→64, 15→56; `MUT-CHECK-SKIP-UPDATE` foreign-key 28→48, 17→37; `MUT-UNIQUE-OWN-WRITES` unique with maintainers 26→26. The fewest of any detected cell is now 26; the latest first detection is seed 5.
- No cell's count dropped (a script compared all 298 applicable cells before and after; 73 rose). The kill matrix's counts equal the scratch measurement exactly, which also shows the counts are deterministic.
- `make soak`: the 100-seed matrix matched the golden file, and 19,783 random runs in 5 minutes, now drawing every targeted fault, found no anomaly.

### What didn't work
- My first bank audit check found nothing for `MUT-SKIP-FRESHNESS`: `BankAudit.TxID` was always 0, because `client.record` set the ID on its own copy of the transaction. `record` now returns the ID.
- Longer pauses for `MUT-FRESH-SNAPSHOT` (20 ms to 200 ms before every statement) moved nothing or made things worse (16 to 9). Longer runs (60 transactions per client) reached 42 and 32 but at twice the cost; the counter bound got there instead.
- Checking the store's constraints after every acknowledged foreign-key write raised `MUT-CHECK-SKIP-UPDATE` only from 28/17 to 29/19 and made those runs half again as long, so I reverted it; one key did it.
- The first bank audit check took `MUT-RETRY-UNKNOWN`'s bank cells from 2.9 s to 30 s under race: with commit partitions there are many unknown transfers, and every audit searched up to 2^20 per half. A budget of 2^10 per audit fixed it.
- The first `ManifestSegments` decoded the whole manifest, which made `watchStore` 3.7% of the simulator's CPU and `internal/sim` 54 s against 51 s; decoding only the segment keys brought it to 1.7% and the time back to flat.
- `GOOS=linux GOARCH=amd64 go build ./antithesis/...` writes the `workload` binary into the working directory; I deleted it, and use `-o /dev/null` for the check.
- A Python splice in `TestSimulator`'s replay matched an earlier `if !found {` and duplicated 80 lines; I repaired it by line numbers and checked the diff.

### What I learned
- Of all reads of a filter block from the store, most come from the clients without caches, which read one on every lookup. A mutant that caches unchecked bytes can only show where something is cached.
- Half of the simulator's maintainer compactions merge the whole database, which removes every tombstone legitimately, so tiered merges rarely meet one.
- An audit's statements after the first are a long window: table scans in an uncached client take many requests, and every write that begins after the first statement returned must be invisible.

### What was tricky
- Every new check had to be silent on correct code under all faults: lost answers that land (commit watch compares the stored body, and dedupes retries of the same body), GC stamps taken after the manifest read (grace check), a manifest written by a compaction that rebased (the compared "before" is exactly the manifest the CAS replaced), and stamps that can run ahead of virtual time (`snapshotBy` takes `max(Elapsed, lastStamp)`).
- `OnChange` first ran under the store's lock; the compaction check needs to read objects, so it now runs after the change under a second lock that only serializes changes and notifications.

### What warrants review
- Self-review with the code-review skill at medium found six things. I fixed five: `OnChange` could report changes out of order under real concurrency (`changeMu`); `watchStore` kept a stale manifest after one that did not decode; `checkCompaction` decoded the database twice per compaction (it reuses the last "after"); `checkCached`, `commitWatch`, and `ManifestContents` cloned whole segments (new `s3fake.Client.View`); and the replay duplicated `mutant.config` field by field (`mutant.apply`). I left one: the audit budget of 2^10 can make an audit undecided if a configuration puts more than about 20 distinct transfers in its window. The corpus requires 0 and passes, so a future change that trips it fails loudly rather than silently.
- Run `go test ./internal/sim -run 'TestRun_watchStore|TestClient_record|TestCommitWatch_Put|TestClient_checkCached|TestForeignKeyCheck|TestRun_targetedFaults' -count 1`, `go test ./internal/history -run TestCheckBankAudits`, `make verification` (no diff), and `make replay SEED=2 WORKLOAD=foreign-key MUTATION=check-skip-update`.
- The decision entry argues that the manifest checks are not white-box; a reviewer may disagree.

### Future work
- `make verification` now takes about 50 to 60 s instead of 7 s.
- `MUT-UNIQUE-OWN-WRITES` in unique with maintainers (26) and `MUT-FRESH-SNAPSHOT` in foreign-key with maintainers (28) are the closest to the floor.
