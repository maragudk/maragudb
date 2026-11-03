# Diary: maragudb section 1 (Spine)

This diary covers section 1 of maragudb, an embedded Go SQL database whose only durable state is in S3. Section 1 builds the storage engine, optimistic multi-writer commits, compaction and GC, a minimal SQL layer, the `database/sql` driver, and the verification infrastructure:

- a fault-injecting fake S3
- a deterministic simulator with serializability checkers
- a Postgres differential harness
- a Tigris integration suite
- an Antithesis harness

The full requirements are in /docs/requirements.md, and the project-level decisions are in /docs/decisions.md.

## Step 1: Requirements interview and project setup

**Author:** main

### Prompt Context

**Verbatim prompt:** I'd like to build a new database with SQL query syntax but backed by S3 natively, no local storage. Similar in spirit to SQLite but without its quirks. Rethought from the ground up to work cloud-native. Interview me about requirements (don't use the brainstorm skill) and let's figure out a verifiable goal you can build against. I won't be reviewing any code in this project manually, we'll rely on automated testing (maybe that queries with the same data need to return the same results as for SQLite or something where applicable) and perhaps property-based testing (see your hegel skill).
**Interpretation:** Interview Markus, turn the answers into concrete requirements and automated acceptance criteria, and then delegate the building.
**Inferred intent:** Get a database whose correctness is proven by machines, because no human will read the code.

### What I did

I interviewed Markus over four rounds of questions. Then I ran `git init` on `main` in an empty directory and wrote /docs/requirements.md and /docs/decisions.md. I split section 1 so that the storage and transaction core, together with its simulator and checkers, comes first and the SQL layer comes after it.

### Why

The highest risk is ACID on S3 with several writers. If that core is wrong, nothing built on top of it matters, so it must be proven before the SQL surface grows.

### What worked

The answers converged quickly:

- an embedded library with explicit `RegisterDriver`
- multi-writer and strict serializable
- turbopuffer-style caches
- Postgres as the oracle, with an AST rendered to both dialects
- Go-like types
- request-count budgets as the performance gates
- Tigris for integration tests

### What didn't work

Nothing failed. The first version of the Antithesis question was rejected so that Markus could install the antithesis-skills plugin first. After that, he chose "Local DST + Antithesis-ready".

### What I learned

- Tigris supports conditional PUTs and returns 412 on a mismatch. The header `X-Tigris-Consistent: true` routes a request to the leader.
- The Antithesis Go SDK instrumentation is amd64-only, but Go cross-compiles, so this Mac does not block the Antithesis harness.

### What was tricky

The two requirements "without SQLite's quirks" and "same results as SQLite" contradicted each other. The fix was to make Postgres the oracle, compared through a shared query AST, with intentional deviations documented.

### What warrants review

Check the acceptance criteria in /docs/requirements.md. They are the only quality gate.

### Future work

- Add the GitHub remote after Markus creates the repo.
- Spawn the builder for the storage and transaction core.

## Step 2: Storage and transaction core, simulator, checkers, and Tigris suite

**Author:** spine-core

### Prompt Context

**Verbatim prompt:** You are building the storage and transaction core of maragudb, an embedded Go SQL database whose only durable state is in S3. [...] This step covers section 1's storage and transaction core plus its verification. It does NOT include the SQL parser, executor, `database/sql` driver, Postgres differential harness, or the docker-compose Antithesis harness. [...] Deliver: 1. An S3 abstraction with two implementations [...] 2. The storage engine [...] 3. A deterministic simulator [...] 4. History checkers [...] 5. Proof that the checkers can fail [...] 6. Request-budget tests [...] 7. Hegel property tests [...] 8. A Tigris integration suite [...] 9. Antithesis Go SDK assertions [...]

Mid-run changes from the lead: the Tigris suite must fail, not skip, when its environment variables are missing, naming them; it loads a gitignored `.env` from the repository root; the bucket variable is `BUCKET`. And an urgent one: commit 65ea456 had included `.env`, so remove it from history before any other commit and never use `git add -A` again.

**Interpretation:** Build the ordered key space on S3 with snapshot transactions and CAS commits, plus everything needed to prove it correct by machine: the fake, the simulator, the checkers, mutation tests, budget tests, property tests, and the Tigris suite.

**Inferred intent:** Nobody will read this code, so the tests have to be strong enough to catch the bugs a reviewer would, and they have to be shown to catch deliberate bugs.

### What I did

Bootstrapped the repo from maragudk/template (LICENSE, `.editorconfig`, `.github`, `.gitignore`, AGENTS.md with a CLAUDE.md symlink, Makefile) as module `maragu.dev/maragudb` on Go 1.27, and added these packages under `/internal`:

- `/internal/s3`: the `s3.Client` interface (`Get` with `If-None-Match` and byte ranges, `Put` with `If-None-Match: *` or `If-Match`, `Delete`, `List`), request counting by kind, and `s3.AWSClient` on aws-sdk-go-v2 that sends `X-Tigris-Consistent: true` on conditional operations and consistent reads.
- `/internal/s3fake`: the fake with content ETags (MD5, like S3), faithful 412s, request counts, and seeded faults: errors before apply, lost acks after apply, crashes before or after apply (per `s3fake.Process` carried in the context), and virtual latency through a `Clock`. `Faults.Decide` scripts outcomes for targeted tests.
- `/internal/sched`: a baton-passing cooperative scheduler in virtual time. One task runs at a time; a task yields only in `Sleep`, which the fake calls for latency.
- `/internal/cache`: an LRU memory cache bounded by bytes and a disk cache.
- `/internal/engine`: the core. `engine.Open`, `DB.Begin`/`BeginReadOnly`, `Tx.Get`/`Put`/`Delete`/`Scan`/`Commit`/`Rollback`, `DB.Compact`, `DB.GC`, `DB.Stats`, and `engine.Mutations` for the deliberate bugs. The design is in /docs/decisions.md.
- `/internal/history`: the bank, list-append, and durability checkers.
- `/internal/sim`: `sim.Run(cfg)` with clients, maintainers, crashes and restarts, mid-transaction pauses, and varied caching per client; the tests run the corpus, check determinism, replay a seed, soak, and assert that every mutant is detected.
- `/internal/tigristest`: `.env` loading and the required-variable check for the Tigris suite, used by `/internal/s3/aws_test.go` and `TestDB_Tigris` in `/internal/engine/engine_test.go`.

Commands: `make test` (about 10 s on this machine, including the Tigris suite), `make soak SOAK_DURATION=5m`, `make replay SEED=123 [WORKLOAD=bank] [MUTATION=cas|gc|freshness|ack]`, `make tigris`, `make lint`.

### Why

The spec's highest risk is ACID on S3 with several writers, and the only gate is automated. So the engine stayed small (no SQL, no background goroutines, injected clock and random source) and the testing stack got most of the effort: a deterministic simulator that reproduces from a seed, checkers that are themselves tested, and mutants that prove the checkers bite.

### What worked

- The baton scheduler gave byte-identical digests across runs on the first try, with no `synctest` needed.
- Hegel found two real bugs within seconds of each property being written: `decodeBlock` preallocated from an untrusted entry count (an out-of-memory crash on a random 20-byte input), and the index-level loop in `segmentBuilder.finish` never terminated when the block size was smaller than two index entries (every level kept the same number of blocks).
- The simulator corpus (130 runs) plus a 45 s soak (3199 random configurations) found zero anomalies, and every mutant was caught by seed 9 or earlier, usually seed 1, in under 35 ms.
- The Tigris suite ran against the real bucket: conditional PUTs and `If-Match` behave exactly like the fake, and the PUT ETag equals the GET ETag.

### What didn't work

- First engine test run: every test failed with `engine: transaction already committed or rolled back` because `Tx.Commit` set `done` before calling `check()`, which reports `ErrTxDone`. Fixed by checking first.
- The scripted-fault tests failed with `engine: creating manifest: s3fake: injected error after PUT-conditional db/manifest (lost ack)` because the script also fired during `Open`. The test world now arms the script only after opening.
- `go test ./internal/engine` hung for over two minutes on the segment property tests. Cause: the infinite loop in `finish` described above, when hegel drew a block size of 1.
- The JSON round-trip property failed with `json: error calling MarshalJSON for type *time.Time: year outside of range [0,9999]`; the generator drew times beyond year 9999, a test bug, bounded the draw.
- The history property "accept every serial history" failed with `durability: read of failed write: tx 1 read value 1 of a failed transaction` because a failed transaction read its own write. The durability checker now ignores a transaction's own reads.
- The "reject a truncated read" property was wrong when the dropped value came from an unknown transaction, which has no completion time and may legitimately be ordered later. The generator now only truncates acknowledged values.
- The first `make lint` reported one `errcheck` on `fmt.Sscanf` in the cycle reporter; replaced the parse with a map from node to transaction ID.
- The GC-grace mutant was not detected in 20 seeds at first. Reads were served from caches, and the window between compaction and deletion was shorter than a transaction. Three changes to the simulator fixed it without touching the engine: a third of the clients run with no cache at all, maintainers run GC right after a compaction, and clients pause mid-transaction now and then. The detection was then seed 3 and seed 9 (17 ms and 32 ms).
- After the reviews, the mutants are still detected by seed 9 or earlier: commit without CAS (seed 1, 4 ms), GC ignoring the grace period (seeds 2 and 9, 13 ms and 34 ms), serving the cache without a freshness check (seed 1, 5 ms), acking before confirmation (seed 1, 5 ms).
- The commit-without-CAS mutant with the bank workload and no maintainers is not detected, and cannot be: an unconditional stale manifest write drops a whole commit, which keeps money conserved. The list-append checker catches it (`lost acknowledged write`, `incompatible orders`), and with maintainers the GC invariant catches it in the bank workload too. The mutant test records this.

### Self-review

I spawned two competing code reviewers over the full diff and one adversarial property-test reviewer that saw the implementations and contracts but not my tests. The property reviewer found ten issues by running its own randomized probes: the fake panicked on a byte range whose end overflowed `int64`; the fake's random source was shared across goroutines without a lock; a lost acknowledgement still leaked a definite 412; the list-append checker never noticed an observed value missing from the final state when its writer was unknown; a history with `Complete < Invoke` produced a false cycle; duplicate append values were not validated; coincident timestamps lost real-time edges; the memory cache's doc overstated its bound; empty values changed representation between a pending write and a committed one; and a corrupt index that pointed at itself looped forever. All of these are fixed and have regression tests.

The two code reviewers agreed on: a stale disk-cached manifest with a higher sequence number could never be replaced by the store's answer (fixed, store answers always win); the simulator mapped every non-conflict error to "unknown", which weakened the checkers (fixed, only `ErrCommitUnknown` is ambiguous, and `Tx.Commit` now documents that any other error means not applied); the reported cycle was nondeterministic (fixed, sorted insertion); retries continued after context cancellation (fixed); `If-Match` on a missing key should answer 404 like S3 (fixed); the Tigris subtests depended on each other (fixed, one database per subtest); `go mod tidy` had not been run; and a single-use `commitWindow` constant (inlined). One reviewer alone found the most serious issue: failed transactions' reads were dropped from the dependency graph, so a stale snapshot seen only by a transaction that later conflicted was invisible to the checker. They now join the graph as readers with no completion edge; a new test shows the stale read is caught. The same reviewer flagged that `Compact` sampled its snapshot time after the manifest read, which could make the engine's own grace-period assertion fire falsely; it samples before now. The other alone flagged that a transient 404 on an eventually consistent read path would be misreported as an expired snapshot; the engine now retries once with a consistent read before concluding that. Corrupt cache entries now fall through to the store, the disk tier is primed on commit, and the Antithesis `Always` assertions are gated off under mutations.

Not changed, and recorded as known costs: compaction rewrites the whole database in memory; a GC stamping pass and a compaction each take a manifest sequence number, so they conflict with in-flight commits (the corpus has more conflicts than commits for that reason); `.github/workflows/ci.yml` runs the Tigris suite without credentials, so CI is red by the requirement that missing variables fail the suite.

### What I learned

- A lost acknowledgement must be resolved against the manifest's commit records, not against the ETag alone: with content ETags, "the ETag changed" does not say who changed it.
- Stamping garbage at GC time rather than at compaction time closes a window where a reader that began during the compactor's PUT could lose objects before its own grace period ended.
- Under database-granularity conflicts, compaction loses to writers most of the time; the maintainer retries up to five times.
- aws-sdk-go-v2 logs a DEBUG line per response about missing checksums against Tigris unless `RequestChecksumCalculation` and `ResponseChecksumValidation` are set to when-required.

### What was tricky

- Crash semantics: a crashed process must not be able to make any further request, including the engine's internal retries, or the "crash" would not be one. The fake refuses every request from a crashed `Process`.
- The commit-ID window: an ambiguous write is resolved only if the manifest still records who took the sequence number. With 128 records, resolution gives `ErrCommitUnknown` only when 128 other commits happened during one retry loop.
- The freshness check and the ambiguity read are both consistent reads, and the cached manifest is only ever replaced by a newer sequence number, so a process that resolved a lost ack cannot later start a transaction on an older manifest.

### What warrants review

- `/internal/engine/engine.go` `casManifest`: the resolution cases. The tests in `/internal/engine/engine_test.go` under `TestTx_Commit` cover lost acks, errors before apply, a lost ack followed by another process's commit, and the unknown outcome.
- `/internal/engine/compact.go` `GC`: the stamping and the orphan rule.
- `/internal/history/history.go` `CheckListAppend`: the barrier construction for real-time edges.
- Run `make test`, `make lint`, and `make soak SOAK_DURATION=2m`.

### Future work

- Tiered compaction, so a few-GB database does not rewrite one segment on every compaction; multipart upload for large segments.
- Row- or range-level conflict detection, as the requirements defer.
- Disk cache eviction (the disk cache never evicts).
- The SQL layer, the `database/sql` driver, the Postgres differential harness, and the docker-compose Antithesis harness, which this step did not cover.
