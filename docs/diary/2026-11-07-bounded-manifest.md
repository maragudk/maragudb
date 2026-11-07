# Diary: Bound the size of the manifest that a commit writes

The builder of section 3, part 2 found a product defect in the sqllogictest corpus (`/docs/diary/2026-11-06-section-3-aggregates.md`). The manifest's garbage list keeps every object that leaves the segment list until GC deletes it, a whole grace period later (1 hour by default). Every commit rewrites the whole manifest, so on a busy database each commit encodes and uploads a manifest that grows with the number of commits in the last hour. `index/delete` made 6,500 commits and took 101 s. The corpus subset now runs with a 5 s grace period to avoid this.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Section 3 is pushed. The defect the corpus showed comes before new SQL features, because it makes commits slower the more an app writes.
**Inferred intent:** A database that is trustworthy in production, not only correct.

### What I did
I wrote the requirement and gave it to the builder `bounded-manifest`.
- **New criterion, `AC-6.4`, under Bounded storage:** the manifest that a commit writes has a size that does not grow with the number of commits within the grace period. Its size depends only on the live segments, the window of commit records, and the number of maintenance passes within the grace period. A test checks it in bytes, with many commits and the default 1-hour grace period.
- **The format:** the builder picks the design and records it as a decision entry. One example is garbage batches in immutable objects, with only a reference to each batch in the manifest. The project is pre-release, so manifests written before the change need not be read. A manifest in the old format must fail with a clear error, not give wrong data.
- **The other criteria:** AC-6.1 to AC-6.3, the GC decisions of 2026-11-03 and 2026-11-04, and every existing mutant must still hold. Any new object kind needs a mutant that the checkers detect, for example one that deletes a batch before its members or one that leaks it.
- **The workaround:** the corpus subset goes back to the default grace period, and its time must not grow.

### Why
A commit is O(1) PUTs (AC-5.1), but the size of one of those PUTs grew with the write rate. The more an app writes, the slower and costlier each commit becomes, and the request budgets do not show it.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
A count of requests is not enough as a budget: the bytes of each request matter too.

### What was tricky
Nothing yet. GC safety under rebased compaction and GC passes is where the design will be difficult.

### What warrants review
The proof that GC is still safe with the new format, and the bound in bytes.

### Future work
Tiered compaction, because compaction now merges every segment into one, and the benchmark suite in issue #3.

## Step 2: Chains of garbage batches, a storage checker, and the subset back on the default grace period

**Author:** bounded-manifest

### Prompt Context

**Verbatim prompt:** You are the builder `bounded-manifest` for maragudb [...] ## Goal The manifest a commit writes must not grow with the number of commits within the grace period. ## Requirements - Add `AC-6.4` to /docs/requirements.md under "Bounded storage" [...] - A deterministic test against the fake S3 measures the manifest bytes [...] It must fail on today's code. [...] - You choose the design [...] Write a decision entry by hand [...] including why GC stays safe. - No backwards compatibility [...] An old-format manifest must fail with a clear error, never wrong data. Test that. - Safety: AC-6.1, AC-6.2, AC-6.3, AC-5.1 and every existing invariant and mutant must still hold [...] Each new kind of object or step needs a mutant in the kill matrix that the checkers detect [...] The orphan sweep must handle any new object kinds. - Remove the workaround [...] - Tigris suite [...] must still pass.
**Interpretation:** Replace the manifest's garbage list with something whose size does not depend on commits, prove GC is still safe with tests and mutants the checkers kill, and take the sqllogictest subset back to the default grace period without making it slower.
**Inferred intent:** Commits on a busy database must stay as cheap after an hour as after a second, without any new way to lose or leak an object.

### What I did

The design is in the decision entry of 2026-11-07 in `/docs/decisions.md`. In short:
- `/internal/engine/manifest.go`: manifest version 2. `Garbage` is now just the keys that compaction moved out and that no GC pass has stamped. `Batches` are references to chains of immutable garbage batch objects at `gc/<seq>-<id>` (`batchRef`: newest key, `Since`, `StampedAt`, `Objects`). `garbageBatch` lists the stamped keys and names the previous batch. `decodeManifest` reads the version on its own first, so a version 1 manifest fails with `ErrUnsupportedManifest` ("the manifest has version 1, and this version of maragudb reads only version 2") instead of a JSON type error. `objectBaseSeq` parses both `seg/` and `gc/` keys.
- `/internal/engine/compact.go`: `gc` reads every chain (`readChains`, with a per-process cache of the immutable batches), expires chains on their latest stamp, deletes an expired chain's garbage unless the manifest references it otherwise (live segment, unstamped garbage, garbage of a chain that has not expired, any chain's batch object), sweeps orphans under `seg/` and `gc/`, writes one batch per pass, joins the newest chain while its first stamp is less than grace/8 old, drops expired chains in its manifest write and only then deletes their batch objects. On a rebase it drops a chain only if the newer manifest has the same head, and writes its batch again when the chain it was to join changed. It gives up past grace/2 before any manifest write that references its batch. A missing batch on an unchanged manifest is a hard error plus `assert.Unreachable`. If the manifest moved on, it is `ErrConflict`. Compaction no longer filters its output out of the garbage list, because GC protects live segments instead.
- `/internal/sim/sim.go`: a new `storage` checker (`checkStorage`). After every run, a fresh, fault-free process with the run's mutations runs GC five times, a grace period apart. Then every segment object must be live, and no garbage and no batch may remain, apart from objects written against the final manifest. Readers now pause for up to the grace period in 1 of 25 pauses, so that early deletion shows.
- Mutants: `MUT-GC-BATCH-OBJECTS-FIRST`, `MUT-GC-SWEEP-SEGMENTS-ONLY`, `MUT-GC-CHAIN-FIRST-STAMP`, and `MUT-GC-DELETE-REFERENCED`, each with an engine test in `TestMutations`. The kill-matrix field `maintained` makes cells without maintainers not applicable. States: `STATE-GC-KEEPS-LIVE-GARBAGE`, `STATE-GC-BATCH-REWRITTEN`, and `STATE-GC-CHAIN-DROPPED`.
- Tests: `TestMaintenance/writes_manifests_no_larger_after_10,000_commits...` in `/maragudb_test.go` (`AC-6.4`, the driver with the default grace period and interval, a commit every 300 ms for 50 minutes of synctest time), a chain-bound test and eight GC tests in `TestDB_GC`, and version 1 tests in `TestOpen`, `TestDB`, and `TestManifest`.
- `/internal/sqllogictest/run_test.go`: `openMaragudb` no longer sets a grace period, for the subset or for the full corpus. The 20 ms interval stays, because it also keeps files from reading through hundreds of segments.

Measured: 10,000 commits 300 ms apart. Before, the manifest was 27,382 bytes after 100 commits and 1,106,676 after 10,000. After, the largest was 13.9 KB. Beside the live segments, it was 5,030 bytes after 1,000 commits and 6,128 after 10,000, from about seven chains.

`GOMAXPROCS=4 go test -race -count 1`, before and then after: root 37 s and 63 s (the new 10,000-commit test is 20 s of that under race), engine 27 s and 26 s, sim 75 s and 77 s, sqllogictest 57 s and 49 s (`TestCorpus` 55.5 s and 47.6 s).

### Why

One reference per GC pass would have been simpler. But at the default 10 s interval it puts 360 to 720 references into an hour's manifests. Chains bound the references to about ten, whatever the pass rate. Keeping orphans inline would still grow with conflicts, one orphan per lost commit.

### What worked

- The orphan sweep is a backstop. A reference lost in any way leaves its garbage unreferenced, and the sweep stamps it again with a later stamp. So the only real leaks are a sweep that misses a kind of object, or a GC that gets stuck. Both are what the storage checker catches.
- The storage checker also found that `MUT-COMMIT-WITHOUT-CAS` and `MUT-ACK-BEFORE-CONFIRM` corrupt storage in cells where they were listed as undetectable: a manifest regression, and a GC that believes its write landed and deletes batches the manifest still references. Those reasons were removed.
- The long reader pauses turned `MUT-GC-CHAIN-FIRST-STAMP` from 0 of 6 cells detected into 6 of 6. They also made `MUT-SKIP-FRESHNESS` detectable in bank through the engine with maintainers.

### What didn't work

- The first version-1 test failed with `engine: decoding manifest: json: cannot unmarshal object into manifest.garbage.0 of type string`, because the new `Garbage []string` does not decode the old objects. The fix was to decode the version first.
- `GCDeleteReferenced` with a hook on the segment PUT never triggered: the fake decides before it applies a write, so the GC in the hook did not see the merged segment yet. The hook moved to the compaction's manifest write.
- The first AC-6.4 measure, the whole manifest at its largest among 100 commits, was flaky under `make test`: `11790 bytes after 100 commits, 14027 after 10,000`. The real-random jitter of the maintenance interval decides how many segments are live at a commit. The test now bounds the segments apart (at most 60) and compares everything else.
- Without long pauses, `MUT-GC-CHAIN-FIRST-STAMP` was detected in no cell.

Checks: `make test` passes, Tigris included. `make lint` reports 0 issues. `make verification` and `go mod tidy` leave no diff. `make soak` ran 25,082 simulator runs in 5 minutes without anomalies, and the full corpus with the default grace period took 11 min 26 s: 5,860,887 of 5,939,714 statements executed, with zero mismatches and no `ErrSnapshotExpired`.

### What I learned

The sim's runs are about 4.7 s of virtual time, with a 2 s grace period, so in-run expiry does happen. But readers with snapshots older than a few hundred milliseconds were rare, so the grace checker could not see an early deletion that was less than a grace period early.

### What was tricky

- An orphan stamp in an immutable batch cannot be dropped when a compaction's rebase makes the orphan live, as the old manifest entry was. A segment that was stamped as an orphan, went live, and was compacted away would then expire on its first, too-early stamp. GC therefore deletes expired garbage only when nothing else in the manifest it read references it.
- `MUT-GC-BATCH-OBJECTS-FIRST` is invisible within one process, because the batch cache serves the deleted batch. Its test, and the simulator, use a fresh process.

### What warrants review

- `gc`, `readChains`, and `getGarbageBatch` in `/internal/engine/compact.go`, against the safety argument in the decision entry.
- `checkStorage` in `/internal/sim/sim.go`.
- The kill-matrix reasons that rest on seeds rather than structure: `MUT-GC-DELETE-REFERENCED` in three SQL cells, and `MUT-COMMIT-WITHOUT-CAS` in unique through SQL without maintainers. A sim change can flip them.
- Validate with `go test ./internal/engine -run 'TestDB_GC|TestMutations|TestOpen'`, `go test . -run TestMaintenance`, and `make verification`.

### Self-review

I ran the code-review skill at medium over the uncommitted diff. It reported nine findings. I fixed four:
- `readChains` read the cache map outside its lock while `cacheBatch` wrote to it, a real data race between concurrent passes in one process. Every lookup now takes the lock. A new test, `TestDB_GC/runs_passes_of_one_process_at_the_same_time...`, reports `DATA RACE` under `-race` without the fix.
- I had made expired garbage deletion depend on the listing. A listing that misses an object would then have leaked it forever, because the chain is dropped anyway. Every expired key is now deleted, and only listed ones count as deleted.
- On a cold cache, `readChains` reads every batch in series without calling the progress hook that renews the lease. It now calls the hook before each read.
- The doc comments of `DB.GC`, `maragudb.GC`, and `ErrSnapshotExpired` did not mention the new `ErrSnapshotExpired` and `ErrConflict` cases. They do now.

I left five:
- `Stats.Garbage` can count a key twice when it is in two chains. That is rare, and it only overstates.
- The batches of a chain whose post-write deletes failed wait for the next GC pass. That is the same as orphans before.
- Two listings per pass instead of one listing of the prefix. One listing would also list nested databases.
- Two small redundancies, the `live` and `removed` maps.

### Future work

- Read a cold chain's batches in parallel, or have a batch list the keys of the whole chain, so that a fresh maintainer does not make hundreds of serial GETs.
- Export `ErrUnsupportedManifest` from `maragudb` if callers need to detect it.
