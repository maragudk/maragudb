# Diary: Skip segments that cannot hold a key

Size-tiered compaction keeps O(log n) live segments, and a cold lookup reads the tree of each one. At 100,000 rows the tiered-compaction builder measured these GETs, tiered against fully compacted: a point lookup 7 against 4, a unique-index lookup 27 against 9, and a join 18 against 14. Each GET to S3 costs tens of milliseconds, so the extra reads make cold queries slower. A per-segment filter that says "this key is not here" lets a read skip most segments.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Background panics are pushed. The cold-read cost of tiered compaction was an accepted trade-off with this follow-up, and it comes next.
**Inferred intent:** Cold reads that cost about what a compacted database costs, without giving up cheap writes.

### What I did
I wrote the requirement and gave it to the builder `segment-filters`.
- **Target.** Cold GETs at 100,000 rows under tiered compaction, measured with the existing count of store requests. The comparison is with a fully compacted database of the same data.
  - A point lookup, a unique-index lookup, and a join each use at most 1.5 times the GETs of the compacted database, or the compacted count plus 2, whichever is larger.
  - No measured query uses more GETs than it does now.
  - The builder also measures a range scan and a non-unique index lookup, and reports them.
- **Design.** The builder decides what the filter is (a Bloom filter, a key range, or both), where it lives (the segment, the manifest, or a block of its own), and when it is read, and records the choice in `/docs/decisions.md`. The manifest stays bounded.
- **Constraints.**
  - Filters in the memory cache count toward its budget.
  - A warm cache must not add GETs.
  - The disk cache holds filters as it holds blocks.
  - Filters must be checked for damage as blocks are, by the segment checksums or one of their own.
  - Segments that an older version wrote, without a filter, must still read correctly.
- **Correctness.** A filter that says a key is absent when it is present returns wrong results. Two mutants must be detected:
  - a filter that leaves out some keys;
  - a damaged filter that is used without its check.

  The kill matrix records where each one applies.
- **Requirements.** The read budgets in `/docs/requirements.md` (the criteria for point lookups, index lookups, and joins) get the new, tighter bound if the measurements support one, with tests that check it.

### Why
Cold reads are the normal case for an embedded database on S3 after a restart or on a new host, and tiered compaction made them slower.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Nothing new yet.

### What was tricky
Nothing yet. A unique-index lookup reads the index entry and then the row, so it pays the cost of the segments twice. That is why its ratio was the worst.

### What warrants review
The filter's false-negative mutant, and whether the new read budgets are proven by tests or only measured.

### Future work
None yet.

## Step 2: Key filters per segment, and unique-index lookups as point reads

**Author:** segment-filters

### Prompt Context

**Verbatim prompt:** You are the builder `segment-filters` for maragudb [...] Target: cold-cache GETs at 100k rows under tiered compaction vs a fully compacted database of the same data, using the existing store-request counting. Point lookup, unique-index lookup, and join each ≤ max(1.5 × compacted, compacted + 2). Today: 7 vs 4, 27 vs 9, 18 vs 14. No measured query gets more GETs than today. Also measure and report a range scan and a non-unique index lookup. [...] Design is yours [...] Mutants that must be detected: (1) a filter that omits some keys; (2) a damaged filter used without its check. [...] tighten the read-budget criteria in /docs/requirements.md [...] with tests that assert the bound (not just measurements).
**Interpretation:** Give every segment a filter that a cold point lookup can consult before it reads the segment's blocks, without adding a GET anywhere, make the SQL lookups that the targets measure go through point lookups, prove the bound with tests, and prove with two mutants that the checkers notice a filter that lies.
**Inferred intent:** Cold reads after many commits that cost about what they cost on a compacted database, so that tiered compaction's cheap writes stop costing read latency.

### What I did
- `/internal/engine/filter.go`: `keyFilter`, a Bloom filter of 10 bits per key and 7 probes over every key of a segment, values and tombstones, hashed with FNV-1a and the MurmurHash3 finalizer and double hashing; `decodeFilterBlock` and `parseFilterBlock` for filter blocks.
- `/internal/engine/segment.go`: `segmentBuilder` collects the key hashes, and `finish` puts the filter in `segmentMeta.Filter` (the manifest) when it is at most a sixteenth of a block, or writes it as a block of kind 2 right after the root block, located by `segmentMeta.FilterBlock`, when the tree has more than one level and the block is at most four blocks; otherwise the segment has no filter. `/internal/engine/manifest.go`: the two optional fields, their validation, and `segmentMeta.dataBytes`, which `/internal/engine/compact.go` now uses for the tiers, so filter blocks move no segment between tiers.
- `/internal/engine/blocks.go`: `lookupFetcher` and `readFilter`. A filter in the manifest is tested directly; a filter block comes from a cache, or alone from the store when a cache has the root, or else in one range GET with the root block, each checked with its own checksum and cached on its own copy; the fetcher then serves that root without reading it again. The cache and store paths of blocks and filters share `cachedEntry` and `storedEntry`. `/internal/engine/tx.go`: `Tx.Get` hashes the key once and skips a segment whose filter says no; `Commit` primes the filter block with the root.
- `/internal/sql/exec/plan.go` and `/internal/sql/exec/stmt.go`: `access.entry` marks the lookup of a unique index whose columns all have equalities with non-NULL values, and `Session.scanAccess` reads that one entry with `Tx.Get` instead of scanning `[P, P+0xFF)`.
- Mutations `FilterOmitsKeys` and `FilterUnchecked`, the states `STATE-FILTER-SKIP` and `STATE-FILTER-BLOCK-READ`, `sim.Config.BlockSize`, 16 corpus configurations with blocks of 128 and 256 bytes (seeds 800 to 815), a block size of 64 to 511 bytes in one soak run of four, and `make replay BLOCK_SIZE=<n>`.
- Tests: `TestKeyFilter` (a hegel property that a filter holds every key it was built over, and the false-positive rate) and `FuzzFilterBlock` in `/internal/engine/filter_internal_test.go`; placement in `TestSegment`; manifest cases in `TestManifest`; `TestDB_KeyFilters` in `/internal/engine/engine_test.go` (GETs per segment, every key through filters cold, warm, and with the filters stripped from the manifest as an older version writes it, the disk cache, the memory budget, a filter read alone after a scan, damage from the store and from a cache); the two mutations in `TestMutations`. In `/maragudb_test.go`, `tieredBudget` and a check of it at every size for the point lookup, the unique-index lookup, the join, the insert, and the insert with a foreign key, plus measurements of a lookup through a non-unique index and a range scan.
- `/docs/requirements.md`: the tiered bound, defined in prose in criterion 5, and held by AC-5.3, AC-5.5 (unique indexes), AC-5.7, and AC-5.8; AC-5.3 also says that a segment whose filter says no costs at most one GET, and none with the filter in the manifest. `/docs/decisions.md` and `/README.md` came from a writer sub-agent from my brief, and I corrected the read rules after the review.

### Why
The targets are about cold GETs, and the only bytes a cold lookup has before its first GET are the manifest's. A small filter in the manifest saves every GET of a small segment. A larger one fetched with the root saves the levels below the root in a deeper segment. The unique-index lookup was a range scan, which no filter of whole keys can help, though only one key can be in its range.

### What worked
- Cold GETs at 100,000 rows, tiered against compacted, before and after: point lookup 7/4 to 4/4; unique index 27/9 to 12/9 (the bound allows 13); join 24/14 to 14/14 (the lead's 18 is from before later commits); non-unique index lookup 19/10 to 10/10; range scan of 100 rows 5/4 to 5/4; insert with a foreign key 11/8 to 8/8; insert 3/3 both. The bound holds at 100, 1,000, and 10,000 rows too, and the 100,000-row tests run in `make test` in about a second.
- Size: in a database of 100,000 rows with a unique index, filter blocks are 0.08% to 0.12% of the segment bytes, because the largest segments have none. After 300 small updates the manifest is 6,878 bytes against 6,241, with six filters of 395 bytes. A segment with a filter block grows by about 2%.
- `MUT-FILTER-OMITS-KEYS` is detected in 12 of 12 cells from seed 1, by eight checkers. `MUT-FILTER-UNCHECKED` in 10 of 10 applicable cells from seed 1, by the cache integrity checker; the bank workload through the engine never has a filter block.
- `GOMAXPROCS=4 go test -race -count 1 ./...`, before (0885c8f) and after the review fixes: 1 min 29.7 s and 1 min 31.4 s wall. Root 21.9 s and 20.5 s, engine 20.3 s and 17.3 s, sim 64.7 s and 71.8 s, sql/exec 34.1 s and 31.4 s, sqllogictest 46.4 s and 45.6 s, pgdiff 40.1 s and 38.8 s, reach 1.2 s both.
- `make test` passes, Tigris included, in 26 s. `make lint` reports 0 issues, and `go mod tidy` and `make verification` leave no diff. `make soak` passes in 13 min 41 s: the whole kill matrix, 21,478 simulator runs in 5 minutes without anomalies, and the full sqllogictest corpus in 8 min 34 s, with 5,860,887 of 5,939,714 statements executed, as before, and no mismatch.

### What didn't work
- The first measurement after the filters showed the range scan at 8 GETs against 5 before: filter blocks made segments larger, so the tiers fell differently and the database had 9 segments instead of 4. Measuring tiers by `dataBytes` brought back the old segments and 5 GETs.
- `TestSegment/every block fails with ErrCorrupt...` failed with `engine: corrupt data: block 780+57 of "db/seg/0000000000000001-test": unknown block kind 2`, since it decoded every block as a tree block. It now decodes the filter block with its own decoder, and checks that each decoder refuses the other kind.
- `MUT-FILTER-UNCHECKED` was first detected in only 3 of 10 cells, because the combined read checked the root first, and most damage to the range failed there before the filter was looked at. Checking the filter first made it 10 of 10; correct code does not care about the order.
- The kill matrix moved in cells I did not touch, because fewer GETs change every later fault draw and shorten transactions: `MUT-COMMIT-WITHOUT-CAS` 11 to 9 of 12 cells, `MUT-COMPACT-REBASE-ON-COMPACTION` 4 to 2 of 6, `MUT-COMPACT-DROP-TOMBSTONES` 3 to 4 of 4, `MUT-FRESH-SNAPSHOT` 7 to 8 of 8. I ran the cells that lost their mutant over 100 seeds and wrote reasons with the rates, 2 to 11 in 100, for example "only the storage checker finds the objects of a dropped commit that GC cannot collect, in 2 of the first 100 seeds, the first at seed 80".
- `TestRun_maintenancePanics` failed with `sim_test.go:1230: Not true` for seed 612, a run in which no step drew a panic. With 50 transactions per client, seed 615 failed instead. Each configuration now tries up to four seeds until a run shows a panic, a backoff, and a resume, and now 18 of 18 back off and 17 of 18 resume, against thresholds of three quarters and half.

### What I learned
- A filter in the object can only save GETs below the root, so a segment of one block needs its filter in the manifest or none at all. That is what decides the placement, not the filter's size alone.
- Making reads cheaper weakens the simulator a little: transactions get shorter, fewer of them overlap, and mutants that need concurrency show less often.

### What was tricky
- The cold tests run with the memory cache off, so a root block read together with the filter cannot go through the cache. The lookup must take the root it already read, or it reads it twice.
- The unique-index rewrite must be exact: the scan `[P, P+0xFF)` holds only `P` when every column has an equality with a non-NULL value, because the unique entry has no suffix and the encodings are self-delimiting. `scanRows` returned nil, not the order-dependent error, when its visitor stopped early, and the shared helper keeps that.

### What warrants review
- Self-review with the code-review skill at medium found eight issues. I fixed five:
  - `make replay` did not pass `BLOCK_SIZE`, though the replay line printed it.
  - `readFilter` skipped the filter whenever a cache held the root. After a scan, every lookup of an absent key in a deeper tree would have descended, and the filter would never have been loaded again. It now reads the filter block alone, and `TestDB_KeyFilters` covers it.
  - The block and filter cache paths were copies. They now share `cachedEntry` and `storedEntry`.
  - The key was hashed once per segment. `Tx.Get` now hashes it once.
  - `validate` ran twice in the manifest check.
- The review also noted that the 128-byte corpus configurations could not keep a filter in the manifest, since the smallest filter of 9 bytes is larger than 128/16. Half of them now use 256 bytes.
- I left two: a damaged filter fails the lookup with `ErrCorrupt` as any damaged block does, which the requirement allows; and a cold lookup counts a cache miss for both the filter and the root, which are two entries.
- Run `go test ./internal/engine -run 'TestKeyFilter|TestDB_KeyFilters|TestSegment|TestManifest|TestMutations/Filter' -v` and `go test . -run TestRequestBudgets -v | grep 'n=100000'`.

### Future work
- Scans, and lookups through a non-unique index, do not use the filters. Key ranges per segment in the manifest, or prefix filters that the SQL layer defines, could prune them.
- The largest segments have no filter. A database of a few GB has several such segments, each costing a descent.
- The sim package grew by about 7 s under race, from the 16 corpus configurations, the two mutants, and the retries of the panic test.
