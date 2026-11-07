# Diary: Bound the disk cache in bytes

The README describes `Options.DiskCacheDir` as "a disk cache that never evicts". A long-running process with a disk cache therefore fills its disk, because compaction keeps writing new segments and old segments are never removed from the cache.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Tiered compaction is pushed. The cache that grows without bound is the next production defect.
**Inferred intent:** A database that is trustworthy in production, not only correct.

### What I did
I wrote the requirement and gave it to the builder `disk-cache-eviction`.
- **New option and criterion.** `Options.DiskCacheBytes` gets a sensible default. A new criterion under section 1's caches, `INV-DISK-CACHE-BOUND`, says that the bytes in the disk cache never exceed the bound by more than one entry. A deterministic test checks it.
- **Eviction policy.** The builder picks it and writes it as a decision entry, including what happens with objects that GC deleted.
- **Correctness.** `INV-CACHE-LOSS` keeps holding: the cache can lose any entry at any time, even during a read, and never serves wrong data. Mutants that the checkers must detect:
  - serving an entry that eviction removed in the middle of a read;
  - serving a truncated or corrupt file;
  - an entry that is never counted towards the bound.
- **Processes.** Several processes might use the same directory. The builder decides whether that is supported, which needs safe eviction across processes, or prevented with a lock and a clear error. The choice goes in the decision entry and is tested.

### Why
A cache that fills the disk takes down the host, and it doesn't stop at the database.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Nothing new yet.

### What was tricky
Nothing yet. Eviction races with reads, and that is where the risk is.

### What warrants review
The bound, and reads that race with eviction.

### Future work
None yet.

## Step 2: Bound the disk cache, check its files, lock its directory, and give the simulator real disk caches

**Author:** disk-cache-eviction

### Prompt Context

**Verbatim prompt:** You are the builder `disk-cache-eviction` for maragudb [...] The disk cache never evicts (README says so). Compaction keeps creating new immutable segments, so a long-running process fills its disk. [...] `Options.DiskCacheBytes` with a sensible default [...] New invariant `INV-DISK-CACHE-BOUND` [...] Eviction policy is yours [...] Several processes on one directory: decide whether to support it [...] or prevent it [...] INV-CACHE-LOSS must keep holding [...] Mutants [...] all detected: serve an entry eviction removed mid-read; serve a truncated or corrupt file (no or skipped integrity check); an entry not counted toward the bound. If the simulator doesn't use the disk cache today, decide whether it should [...] keep sim time flat.
**Interpretation:** Replace the unbounded `cache.Disk` with one that holds at most `DiskCacheBytes`, never serves wrong bytes however its files are damaged or removed, survives restarts, and refuses a second process; prove each property with tests and the three mutants with tests or the kill matrix.
**Inferred intent:** A disk cache that a long-running production process can leave on, without the disk filling and without a new way to serve wrong data.

### What I did
- `/internal/cache/disk.go`: a new `Disk` over a small `FS` interface (read, write a temporary file, rename, remove, list). Exact LRU with a list and a map under a mutex; every file counts as its size rounded up to 4 KiB (`FileBytes`). `Put` writes a temporary file, renames it under the mutex, counts it, and evicts; the writes take turns through `putMu`, so at most one file beyond the bound exists. Each file has a 16-byte header (magic, CRC-32C, key length, value length), then the key and the value, and `Get` checks all of it; a refused file is a miss and leaves the cache, and a failed read only drops the entry that it read. A `Put` that cannot store drops the key's older value. Failed removals stay counted as stuck. `NewDisk` lists and stats the directory, orders by modification time, removes `tmp-` files, and evicts to the bound. `DiskMutations` has `Unverified`, `ServeCutReads`, and `UncountedAtOpen`.
- `/internal/cache/dir.go` and `/internal/cache/lock_*.go`: `OpenDir` with a per-process registry keyed by the resolved path, and an exclusive lock on `<dir>/lock` (`flock` on the flock platforms, `LockFileEx` on Windows, none elsewhere), failing with `cache.ErrInUse`. `golang.org/x/sys` became a direct dependency for Windows.
- `/internal/fsfake/fsfake.go`: a deterministic file system in memory with `OnRead` (a hook in the middle of a read), `CutReads` (a read that a removal or replacement overlaps ends early), and `RemoveErr`.
- `/internal/sqldriver/sqldriver.go`, `/maragudb.go`: `DiskCacheBytes` (default 1 GiB, zero off), the connector opens the cache with `OpenDir` and closes it, `maragudb.ErrDiskCacheInUse`.
- `/internal/sim/sim.go`: every client's disk tier is a real `cache.Disk` over its own `fsfake.FS`, reopened at every restart. `Config.DiskFaults` bounds it to 2 to 16 blocks and damages files at crashes and between transactions. A new checker, `disk cache bound`, runs after every client operation. Two new states, `STATE-DISK-CACHE-EVICTION` and `STATE-DISK-CACHE-REFUSED`. `decodeList` returns an error instead of panicking on a corrupt list. 24 corpus configurations (seeds 700 to 723) run with `DiskFaults`, the soak turns it on half the time, and `make replay DISK_FAULTS=1` replays it.
- `/internal/sim/sim_test.go`: three mutants in the kill matrix, with `diskMutations`, `unsimulated` (no cell applies), and `engineOnly` (the SQL cells do not apply).
- Tests: `/internal/cache/disk_test.go` (a hegel model test of random operations with damage, restarts, and evictions and replacements in the middle of reads; the three mutants found by seeded runs of the same operations; a realistic read-through workload on `fsfake` and on a real directory; a concurrent workload; damage of every byte and length; restarts; stuck removals; `OpenDir` across processes through a helper process that is killed to show that a crash releases the lock), `/maragudb_test.go` `TestDiskCache`, `/maragudb_internal_test.go` for the default.
- Docs: `INV-DISK-CACHE-BOUND` in `/docs/requirements.md`, the decision in `/docs/decisions.md`, `/README.md` (options, the disk cache paragraph, `DISK_FAULTS`, the stale counts of mutants and states), `/Makefile` (replay names), and the regenerated `/docs/verification.md` and kill matrix.

### Why
The disk had no bound and no check on what it served. A checksum is the only thing that makes "an entry may vanish mid-read" and "a crash can tear a file" safe without a sync per write, and a lock is the cheapest way to make a per-process count a real bound.

### What worked
- Keeping the simulator's old configurations on caches that never fill, and turning eviction and damage on only with `DiskFaults`: the kill matrix of the 24 existing mutants stayed byte-identical, so no `cannot` reason had to be rewritten.
- `fsfake.OnRead` made a reproducible eviction in the middle of a read possible without goroutines.
- The realistic workload: 20,000 operations wrote 56.8 MB through 1 MiB, the files never took more than 1,048,576 bytes, and 12,874 of 18,000 hot reads (71.5%) hit.
- `MUT-DISK-CACHE-UNCOUNTED` is detected in all 12 cells at seed 1 or 2, and `MUT-DISK-CACHE-UNVERIFIED` in the four engine cells at seed 2 or 3.

### What didn't work
- Turning bounded caches and damage on for every simulator run changed every history, and the kill matrix of existing mutants moved: `bank through the engine, no maintainers: not detected in 20 seeds, and no reason says why it cannot be` and `foreign-key through SQL, 2 maintainers: detected with seed 20 by [index consistency], but the mutant is said to be undetectable there`. That is why `DiskFaults` exists.
- With `Unverified`, the list-append workload panicked: `panic: sim: corrupt list "12884901889,21474836483,12,13,171798691=8,12884901906"`. `decodeList` now returns an error, which becomes a workload anomaly.
- With `Unverified` through SQL, a run hung: a damaged table definition made `(*table).couldScan` in `/internal/sql/exec/plan.go` index `x.Columns[0]` of an index without columns, and the deferred `tx.Rollback` in `(*client).tick` then blocked forever on database/sql's lock. So the mutant's SQL cells do not apply, with that reason.
- The first realistic workload failed with `a hit of db/seg/131@-1 returned other bytes than were put`, a bug in the test: commits put root blocks of a random size and reads expected 1,024 bytes.
- The helper process printed `in use\nPASS`, so the tests compare the first line only.

### What I learned
- With a checksum over the whole file, a cut read and a corrupt file are caught by the same check, so "serve a read that an eviction cut short" can only be a mutant that skips the check for short files. That is what `ServeCutReads` is, and why only `TestDisk` can detect it: in the simulator no disk read yields, so none overlaps an eviction, and the engine fails to decode a cut block anyway.
- A per-process count with the rename under the mutex means the files and the count always agree after an operation, so the tests can check "the files take no more than the count" as well as the bound, which catches an uncounted entry at once instead of after the directory fills.

### What was tricky
- A read that fails must drop only the entry that it read: a `Put` of the same key may have replaced it meanwhile, and dropping by name would leave the new file uncounted. `TestDisk/keeps_counting_a_newer_file_of_a_key_when_a_read_of_the_older_one_fails_in_the_middle` covers it.
- The simulator's damage must never make a file larger, or the check "files take no more than the count" would fire on the damage itself; a swap only copies a file whose rounded size is no larger.
- Self-review findings, all from the code-review skill at medium:
  - Fixed: `//go:build unix` includes solaris and aix, which have no `syscall.Flock`; the tag now names the flock platforms, and `GOOS=solaris` and `GOOS=aix` build.
  - Fixed: the simulator's checker and the driver test allowed the bound plus one entry between operations, when no write runs; both now require the bound itself, and so do the sequential workload tests. The kill matrix did not change.
  - Fixed: `Put` held the mutex across the removals of evicted files; it now removes them after releasing it, which is safe because writes take turns.
  - Fixed: the `Disk` doc claimed open stats at most the bound's worth of files; it stats every file.
  - Fixed: the engine test closed the disk cache while its database was open; it closes the database first.
  - Fixed: the helper processes got an empty environment, which breaks Windows; they now inherit it without `MARAGUDB_VERIFY_DIR`.
  - Left: `putMu` serializes the writes of temporary files. It is what keeps the files within one entry of the bound under concurrency, and a write follows a store read of milliseconds; reserving bytes before the write would allow parallel writes at the cost of more code.
  - Left: a second `OpenDir` of an open directory with another bound keeps the first bound, as its doc says; an error would be an alternative.
  - Left: `sqldriver.Options.DiskCacheBytes` of zero turns the cache off; only `maragudb.Options` has defaults, and it sets 1 GiB.
  - Left: on Windows, removals that a concurrent read blocks become stuck and are retried on every eviction; correct, but it could shrink the cache under heavy concurrent reads there.

### What warrants review
- `/internal/cache/disk.go` `Get` and `Put`: the identity check of the entry after a failed read, and the order of rename, count, eviction, and removal.
- The reasons in the kill matrix for `MUT-DISK-CACHE-SERVE-CUT-READ` (no cell applies) and the SQL cells of `MUT-DISK-CACHE-UNVERIFIED`.
- Validate with `go test ./internal/cache`, `go test ./internal/sim -run 'TestMutants/MUT-DISK'`, and `make replay SEED=700 CORPUS=1`.
- Timings with `GOMAXPROCS=4 go test -race -count 1`, wall time including the build, before and after: `internal/cache` 2.2 s and 7.6 s, `internal/sqldriver` 1.7 s and 2.4 s, `internal/engine` 34.0 s and 34.1 s, the root package 38.6 s and 40.8 s, `internal/sim` 79.9 s and 83.6 s.

### Future work
- The SQL layer panics on an index without columns in a table definition (`/internal/sql/exec/plan.go`, `couldScan`), which only corrupt storage can produce. A check when the catalog is decoded would make such a definition an error.
- Engine segments have no per-block checksum, so the disk cache's check is the only one between a damaged file and the engine; a checksum in the segment format would also cover damage in the store.
