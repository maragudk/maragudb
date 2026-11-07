# Diary: Damaged stored bytes give a clear error

The builder of the disk cache (`/docs/diary/2026-11-07-disk-cache-eviction.md`) found three gaps:
- A damaged table definition makes `(*table).couldScan` in `/internal/sql/exec/plan.go` panic.
- That panic hangs the simulator instead of failing the run: the deferred `tx.Rollback` blocks on the lock of `database/sql`.
- Segments have no checksum of their own, so the disk cache's check is the only one between damaged bytes and the engine.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The bounded disk cache is pushed. Next come the robustness gaps that it found.
**Inferred intent:** Never give wrong data, and fail loudly and diagnosably, even when stored bytes are damaged.

### What I did
I wrote the requirement and gave it to the builder `damaged-data`.
- **New invariant, `INV-DAMAGE-DETECTED`.** When an object in the store or in a cache has damaged bytes, the result is a clear error that wraps a new exported `ErrCorrupt`. It is never a panic, a hang, or wrong data. This covers segments, the manifest, garbage batches, and the catalog.
- **Checksum.** The segment format gets a checksum per block, which a range GET can check without reading the whole object. The manifest and garbage batches get a check too, unless JSON decoding with validation already catches every damage. Old formats fail with a clear error, as in the earlier version bumps; the project is pre-release.
- **Decoders.** Every decoder is fuzzed (Go native fuzz, with seed corpora run in `make test`) and never panics on any input. After decoding, the catalog is validated, for example that an index has columns.
- **The simulator.** A panic in a workload fails the run with the panic and its stack, and never hangs it. A targeted test proves this.
- **Mutants.** The SQL cells of `MUT-DISK-CACHE-UNVERIFIED` must become applicable and be detected. A new mutant that skips the block checksum must be detected when the store returns damaged bytes. To make that possible, the fake S3 gets a fault that damages the bytes of a GET.

### Why
Silent wrong data is the worst thing a database can do. A panic or a hang is the second worst, because it hides the cause. S3 rarely damages data. Bugs in offsets and range reads are more likely, and checksums catch both.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Nothing new yet.

### What was tricky
Nothing yet.

### What warrants review
That every path from stored bytes to a value goes through a check.

### Future work
None yet.

## Step 2: Checksums, a validated catalog, fuzzed decoders, damaged reads in the simulator, and panics that fail a run

**Author:** damaged-data

### Prompt Context

**Verbatim prompt:** You are the builder `damaged-data` for maragudb [...] New invariant `INV-DAMAGE-DETECTED` in /docs/requirements.md: damaged bytes in any stored or cached object (segments, manifest, garbage batches, catalog) give a clear error wrapping a new exported `maragudb.ErrCorrupt`; never a panic, a hang, or wrong data. [...] Segment format: a checksum per block, verified on every read [...] Manifest and garbage batches: add a checksum unless you can show JSON decoding plus validation catches every damage [...] Decoders [...]: Go native fuzz tests whose seed corpora run in `make test` [...] Fix `(*table).couldScan` [...] Simulator: a panic in a workload or client must fail the run with the panic value and stack, never hang [...] Mutants: the SQL cells of `MUT-DISK-CACHE-UNVERIFIED` become applicable and detected [...] New `MUT-SEGMENT-UNCHECKED` [...] add a fault to the fake S3 that damages GET bodies [...] AC-5.x request budgets and AC-6.x keep passing.
**Interpretation:** Give every stored object a check that a single range read can run, refuse damaged bytes everywhere before they are served or cached, validate what the SQL layer decodes, fuzz every decoder, make a panic in the simulator a failed run instead of a hang, and make the simulator damage reads so that a mutant without the block check is caught.
**Inferred intent:** Silent wrong data is the worst outcome of damage, a hang the second worst; both must become a loud, typed error that the tests can see.

### What I did
- `/internal/engine/segment.go`: every block ends with a CRC-32C of the object key, the block's offset (8 bytes), and the block's bytes. `segmentBuilder.finish(key)` fills the checksums in once the key is known, so the random draws of the engine keep their order. `decodeBlock(key, ref, b)` checks length and checksum, then `parseBlock` checks structure: at least one entry, strictly increasing keys, index children after the child before them and before the index block. The iterator checks that each data block lies after the previous one and that keys increase across blocks. All errors wrap the new `engine.ErrCorrupt`.
- `/internal/engine/manifest.go`: manifest version 3 and garbage batch version 2 are sealed (`mdbm`/`mdbg`, CRC-32C of key and JSON, then the JSON), the manifest is validated after decoding, versions 1 and 2 fail with `ErrUnsupportedManifest` naming the versions, and a batch cannot name itself as its predecessor.
- `/internal/engine/blocks.go`, `/internal/engine/engine.go`: blocks from the memory cache, the disk cache, and the store are checked before they are served or cached; refused cache bytes are a miss counted in `Stats.MemoryCacheRefused` and `Stats.DiskCacheRefused` (Antithesis `Unreachable`), damaged store bytes are `ErrCorrupt`, never cached, and hit `STATE-DAMAGED-READ-REFUSED`. The disk record of the manifest moved to `<prefix>manifest@v3`. `Mutations.SegmentUnchecked` skips the block checksum and caches before decoding. `/maragudb.go` exports `ErrCorrupt`.
- `/internal/sql/exec/catalog.go`, `/internal/sql/exec/foreignkey.go`: `decodeTable` validates every definition (name, primary key, columns, indexes, foreign keys, index-name entries), `(*foreignKey).link` checks a foreign key against its parent when it is followed, rows, defaults, and checks that do not decode wrap `ErrCorrupt`, and `CheckIndexes` decodes through `decodeTable`. `/internal/sql/exec/plan.go` `couldScan` treats an index without columns as scannable whole. `/internal/sql/value/value.go` rejects text that is not UTF-8 and times finer than a microsecond.
- Fuzz targets, seed corpora in `make test`: `FuzzSegment` and `FuzzBlock` in `/internal/engine/segment_internal_test.go`, `FuzzManifest` and `FuzzGarbageBatch` in `/internal/engine/manifest_internal_test.go`, `FuzzDisk` in `/internal/cache/disk_test.go`, `FuzzDecodeRow` in `/internal/sql/value/value_test.go`, and `FuzzCatalog` in the new `/internal/sql/exec/catalog_test.go`, which runs fourteen statements and both checks on a database whose definition of a table is arbitrary bytes.
- `/internal/sched/sched.go`: a panicking task makes `Run` panic at once with `*sched.TaskPanic` (task, value, stack); other tasks stay parked. `/internal/sqldriver/sqldriver.go`: `Options.OnPanic` recovers panics in every driver call, breaks the connection, and a broken transaction rolls back on commit. `/internal/sql/exec/session.go`: `Mutations.PanicInScan` for the test.
- `/internal/s3fake/s3fake.go`: `Faults.DamageRate` (a flipped bit, a cut, or another object's bytes at the same range) and the scripted `Faults.Damage`, counted per process by `Damaged`. `/internal/sim/sim.go`: `Config.StoreDamage` (2% of reads), the checkers `cache integrity`, `damaged reads`, and `panic`; `sim.Run` turns a task panic into a failed result. 38 of 298 corpus configurations run with damage, the soak half the time, `make replay STORE_DAMAGE=1`. `/internal/sim/sim_test.go`: `MUT-SEGMENT-UNCHECKED`, `MUT-DISK-CACHE-UNVERIFIED` without `engineOnly`, and `TestRun_panic`; `/internal/sim/sim_internal_test.go` tests the damaged reads checker directly.
- Docs: `INV-DAMAGE-DETECTED` in `/docs/requirements.md`, the decision in `/docs/decisions.md` and the edits of `/README.md` written by a writer sub-agent from my brief and reviewed by me, `/Makefile`, and the regenerated `/docs/verification.md` and kill matrix.

### Why
A checksum that covers the key and the offset turns every way a read can return the wrong bytes (a flip, a cut, another object, a wrong range) into one check that a single range GET can run, so no request is added. Validating decoded definitions turns the remaining class, bytes that a buggy writer wrote, into errors rather than panics.

### What worked
- The structural checks plus `FuzzSegment` and `FuzzBlock`: the fuzzers found two real gaps within two minutes (below), and both crashers are now seed corpus files in `/internal/engine/testdata/fuzz/`.
- `TestCatalog` reproduces the original `couldScan` panic: with validation and the guard switched off, it fails with `panic: runtime error: index out of range [0] with length 0`.
- `MUT-SEGMENT-UNCHECKED` is detected in 12 of 12 cells (cache integrity, and bank, list-append, unique, index consistency, workload invariants in some), and `MUT-DISK-CACHE-UNVERIFIED` in 12 of 12 by cache integrity. The other mutants' kill-matrix results stayed the same apart from a few first seeds.
- Every bit flip and every truncation of every block, manifest, and garbage batch fails with `ErrCorrupt` (exhaustive tests), and a cold point lookup still makes the freshness read plus one GET per level.

### What didn't work
- `FuzzSegment` failed with `segment_internal_test.go:358: the scan yields "000" after "000"`: checksummed data blocks in increasing offset order can still repeat a key across blocks. The iterator now checks that keys increase across blocks.
- `FuzzBlock` failed with `child {Offset:0 Length:6} of index entry 0 of the block at -17`: `parseBlockBody` compared against `uint64(ref.Offset)` of a negative offset. It now refuses a negative offset itself.
- The first catalog validation refused indexes that repeat a column: `TestSession_CreateIndex` failed with `engine: corrupt data: definition of relation "t": index "t_a_a1_idx" has column 1 of 3, or twice`. Postgres allows `(a, a)`, so only the primary key may not repeat a column.
- The first value validation refused times outside 0001 to 294276: `TestEncodeRow` failed with `time -62135596801 out of range`. A `time.Time` parameter out of that range is stored today, so rejecting it on decode would refuse stored data; only the microsecond check stayed.
- `TestDB_CompactTiers` failed with `Expected "4", but got "3"`: the 4 bytes per block moved a 20-byte value segment from tier 1 into tier 2 of `smallTiers`, so its `BaseBytes` went from 16 to 18.
- A `MUT-MANIFEST-UNCHECKED` that skipped the manifest's checksum was detected only by the grace period and final read checkers in some cells, and missed in five cells within 20 seeds, so I dropped it. It did show why the manifest needs its checksum: the damaged manifest had the real ETag, so the client's next commit wrote the damage back into the store.
- A first version of the panic test passed even without `OnPanic`: with seed 1 the panic happened outside an explicit transaction. Seeds 4 and 5 hang without `OnPanic` (`the run hangs after a panic`), so the test uses seed 4, where an audit's deferred `tx.Rollback` waits on database/sql.
- A first scheduler design resumed the other tasks after a panic so that they end; it would have unwound them through database/sql's locks, the very hang being fixed, so they stay parked.

### What I learned
- The disk-cache mutant can no longer show as wrong data, because the engine now refuses what the disk cache serves. Detecting it means checking that a cache never returns bytes that fail the engine's check, which is also what catches `MUT-SEGMENT-UNCHECKED` first: a damaged block it cached comes back and fails to parse.
- database/sql only hangs on a panic in a driver call when a later call needs the lock that the panic skipped releasing, typically a deferred `Rollback` of the same transaction; an autocommit statement unwinds fine.

### What was tricky
- Keeping existing histories was impossible: 4 more bytes per block change segment sizes, and the simulator's tiers start at 64 bytes, so every history changed. The kill matrix stayed stable anyway.
- Self-review (code-review skill at medium) findings:
  - Fixed: after a recovered panic, `Tx.Commit` still committed the transaction's earlier writes; a broken connection's commit now rolls back and returns `driver.ErrBadConn`, with a test that fails without the fix.
  - Fixed: `OnPanic`'s doc now says that the engine may be left with locks held, so it is for callers that end soon after, such as the simulator.
  - Fixed: the simulator's own panic type duplicated `sched.TaskPanic`; it reuses it now. `gofmt` of `/internal/sched/sched.go`.
  - Left, and stated in the decision: the maintenance lease is not sealed, because a damaged lease can only make two maintainers duplicate work; checksums include the prefix, as the manifest's segment keys already did.
  - Left: no retry of a read whose bytes fail; the decision explains why.
  - Left: `fk.link` runs per row event; it is a few slice scans next to the key-value read that every event makes.
  - Left: the `couldScan` guard is unreachable with validation; the lead asked for it, and it costs a branch.
  - Left: the Antithesis `Unreachable` for refused cache bytes is gated on engine mutations only, so a run with a disk-cache mutant would trip it; Antithesis runs no mutants.

### What warrants review
- `/internal/engine/blocks.go` `readBlock`: every path checks before it serves or caches, and the mutant path caches first.
- `/internal/sql/exec/catalog.go` `validate` and `/internal/sql/exec/foreignkey.go` `link`: that they refuse nothing a valid schema can hold.
- `/internal/sim/sim.go` `checkError` and `checkCache`, and the run's recovery of `*sched.TaskPanic`.
- Fuzzing, 3 minutes per target with 4 workers on the code before the two fixes, then 3 more minutes of `FuzzSegment` and `FuzzBlock` after them: `FuzzManifest` 9.8 million executions, `FuzzGarbageBatch` 8.4 million, `FuzzDisk` 21.2 million, `FuzzDecodeRow` 22.2 million, `FuzzCatalog` 1.9 million, `FuzzSegment` 16.5 million and `FuzzBlock` 11.3 million after the fixes; nothing beyond the two findings above.
- Checks: `make test` (with Tigris), `make lint` (0 issues), `make verification` (no diff), `go mod tidy` (no diff), and `make soak`: 23,303 simulator runs in 5 minutes without anomalies, and the full corpus of 622 files in 11 minutes 34 seconds with 5,860,887 statements executed and zero mismatches. The intermediate commits do not each pass the verification test on their own, because the IDs of the new mutant and state reach the kill matrix and the verification map only in the simulator commit.
- Timings with `GOMAXPROCS=4 go test -race -count 1`, wall time including the build, before and after: `internal/engine` 37 s and 36 s, `internal/cache` 8 s and 8 s, `internal/s3fake` 2 s and 2 s, `internal/sched` 1 s and 1 s, `internal/sql/exec` 27 s and 29 s, `internal/sql/value` 6 s and 6 s, `internal/sqldriver` 2 s and 2 s, the root package 40 s and 42 s, `internal/sim` 83 s and 87 s; the simulator grew by one mutant in the kill matrix and two targeted tests, and its corpus has the same 298 runs.
- Validate with `go test ./internal/engine -run 'TestDB_Damage|TestSegment|TestManifest'`, `go test ./internal/sim -run 'TestRun_panic|TestMutants/MUT-SEGMENT'`, and `make replay SEED=8 CORPUS=1 STORE_DAMAGE=1`.

### Future work
- Whether the public driver should recover panics too, so that an application that recovers a handler panic does not leak a connection forever; it would change what applications see.
- `time.Time` parameters outside 0001 to 294276 are stored, where Postgres refuses them.
- The Antithesis workload does not turn on `DiskFaults` or `StoreDamage` yet.
