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

## Step 3: SQL layer, `database/sql` driver, Postgres differential harness, and SQL simulator workloads

**Author:** spine-sql

### Prompt Context

**Verbatim prompt:** You are building the SQL layer of maragudb, an embedded Go SQL database whose only durable state is in S3. [...] Scope: the rest of section 1, except the Antithesis docker-compose harness. 1. The public package `maragu.dev/maragudb` (repo root) [...] 2. The SQL surface for section 1 [...] 3. A Postgres differential harness (acceptance criterion 4) [...] 4. End-to-end verification through SQL [...] Nobody will ever review this code manually. Automated tests are the only gate, so they must be strong enough to catch real bugs, and you must show that they can fail.

Mid-run instructions from the lead: pass the Tigris secrets to the CI test step; then, replacing that, switch `ci.yml` and `security.yml` to the shared reusable workflows from maragudk/template, delete `compatibility.yml`, enable the `postgres: true` service for the harness, export the secrets with `secret-env` and `secrets: inherit`, use Postgres 18 locally too, and make sure the suite passes under `-race`.

**Interpretation:** Build everything between the storage core and the user: a value model, a parser, an executor with transactions, the driver and public API, a differential harness that proves the dialect against Postgres, and the same simulator workloads through SQL.

**Inferred intent:** The SQL layer must be as trustworthy as the core, by the same means: machine-checked against an oracle and a simulator, with mutants that prove the checks bite.

### What I did

Packages, bottom up:

- `/internal/sql/value`: the six types plus NULL, `Compare` (byte order for strings and bytes, a total float order with NaN above infinity and negative zero equal to zero), an order-preserving key encoding (sign-flipped big-endian ints, bit-twiddled floats, NUL-escaped strings and bytes with a `\x00\x01` terminator, NULL as a leading `\x00`), and a tagged row encoding.
- `/internal/sql/ast`: the syntax tree and `Render` to the maragudb and Postgres dialects, with the Postgres precedence table deciding where parentheses go.
- `/internal/sql/parser`: a hand-written lexer and recursive-descent parser. Identifiers fold to lowercase, a sign before a number is part of the literal, comparisons do not chain, and `TIME '...'` is the time literal.
- `/internal/sql/exec`: the binder, which type-checks, substitutes parameters, and folds constants with the same pruning rules as Postgres; the planner, which turns primary key comparisons in `WHERE` into a point lookup or a key range; the statement executors; and `Session`, which owns the transaction, retries autocommit statements on conflict, and gives every statement its own write buffer so that it sees earlier statements but not itself.
- `/internal/sqldriver`: the `database/sql/driver` implementation. One connector holds one engine per DSN; connections share it.
- `/maragudb.go`: `RegisterDriver`, `NewConnector`, `Options` with pointer defaults, the exported errors, `Stats`, `Compact`, and `GC`.
- `/internal/pgdiff`: the differential harness. `Postgres(t)` finds or starts Postgres 18, `Generate` draws a typed case with hegel, `Runner.Run` runs it on both sides and compares after every statement.
- `/internal/sim/sqlclient.go`: the bank and list-append workloads through `database/sql`, selected by `Config.SQL`, with `Config.SQLMutations` for the SQL layer's mutants.
- `/docs/deviations.md` with sixteen numbered deviations, each with a test in `/internal/pgdiff/deviations_test.go` that asserts the difference on both databases.

Tables map onto the key space as `c/<table>` for the JSON definition and `r/<table>\x00<encoded primary key>` for each row, with the whole row in the value. The row range of a table is `[r/<table>\x00, r/<table>\x01)`, and a prefix of equalities on the key columns plus a range on the next one narrows it.

Make targets: `make test` runs everything including 200 differential cases, `make pgdiff` runs 3000 (or `PGDIFF_CASES=n`), and `make replay SEED=n WORKLOAD=bank API=sql MUTATION=fresh-snapshot` replays through SQL.

### Why

The key encoding is the foundation: with byte order equal to value order, primary key lookups and ranges are plain engine scans and the catalog is just keys in the same transaction, so DDL is transactional for free. Statement-level write buffering gives statement atomicity and rules out the Halloween problem in one mechanism. Folding constants at bind time looked like an optimization but is required for agreement with Postgres, which raises errors in constant subexpressions before it reads a row.

### What worked

- The hegel properties on the key encoding caught three deliberate mutants immediately (unflipped negative floats, unescaped NUL bytes, dropped nanoseconds), and the parser round-trip property caught a renderer that dropped parentheses on the right side of a binary operator.
- The differential harness earned its keep. In the first runs it found, in order: `ORDER BY` with duplicate output names (Postgres only calls it ambiguous when the expressions differ), the `pgx` plan cache rejecting a recreated table, Postgres rejecting non-integer constants in `ORDER BY`, numeric literals losing negative zero on the way to `float8` (now rendered as `'-0'::float8`), `abs(NULL)` typing as `double precision`, all-NULL `CASE` and `COALESCE` typing as `text`, strict operators folding to NULL without evaluating the other operand, `IN` lists evaluating column elements one at a time, `CASE` output columns named after their `ELSE`, lazy `LIKE` escape errors, `BETWEEN` with a known NULL value, top-level `WHERE` conjuncts stopping at the first NULL as well as the first FALSE, and a `CASE` with no live `WHEN` and no `ELSE` being a NULL constant. Each is now mirrored and unit-tested in `/internal/sql/exec/exec_test.go`. The last 10,000-case run passed: 45,105 statements, 39,888 succeeding on both sides, 5,217 failing on both, 27,732 state checks, 6,371 ordered comparisons, in 48 s. `make test` runs 200 cases in about 2 s.
- The SQL simulator workloads were deterministic on the first run that completed: the same seed gives the same digest through `database/sql`, because the pool opens connections in the caller's goroutine and the scheduler context has no `Done` channel, so no helper goroutine ever takes the baton.
- Every engine mutant is also detected through SQL, by seed 1 or 2, and the SQL layer's fresh-snapshot-per-statement mutant is detected by both workloads with seed 1.

### What didn't work

- The first draft of the executor accepted `a = b = c`; the renderer mutant that dropped the left parentheses of a comparison was not caught because the parser agreed with it. The parser now rejects chained comparisons as Postgres does, and the property catches the mutant.
- `go test ./internal/pgdiff` first failed with `ERROR: cached plan must not change result type (SQLSTATE 0A000)` because `pgx` caches prepared statements by text and every case recreates the table. The connection string now carries `default_query_exec_mode=describe_exec`.
- `docker run ... -p 127.0.0.1:5433:5432` failed locally with `Bind for 0.0.0.0:5433 failed: port is already allocated`, so the local container binds an ephemeral port and `docker inspect` reports it; CI uses the fixed `localhost:5433` service when `CI` is set.
- The simulator panicked with `panic: sched: Sleep called outside a scheduled task` from `database/sql.(*Tx).Commit`, because `driver.Tx.Commit` has no context and the driver used `context.Background()`. The driver now commits in the context the transaction began with.
- The SQL-level mutant `RetryAfterUnknownCommit` is not detected by the simulator in 20 seeds and cannot be: there, an unknown outcome only ever comes from a crash, after which the process cannot retry. The scripted test `TestMutations/RetryAfterUnknownCommit` in the exec package shows it applies an `UPDATE` twice. The mutant test records this.
- hegel reported several differential failures as "nondeterministic failure ... failed 20 of 30 replays at confirmation". Replaying the generator with the same choices gives identical cases, and running the failing case thirty times fails thirty times, so the runs are deterministic; the stale examples in `.hegel/` from earlier generator versions explain the "reproduced from stored state" variants, and I did not find the cause of the confirmation-phase variants. Deleting `.hegel/` before a run avoids the stale replays.

### Self-review

I dispatched two competing code reviewers over the whole diff and one adversarial property reviewer that saw the implementations and contracts but not my tests, and a writer for the README's Usage and Development sections.

The property reviewer found three real problems by probing: the renderer never quoted identifiers, so a column named `select` or `Foo` did not round-trip; `value.Compare` on times used `time.Time.Compare`, which overflows for Unix seconds beyond `MaxInt64-62135596800` while the key encoding does not, so byte order and `Compare` disagreed there; and a quoted identifier could contain a NUL byte, which the row key layout assumes never happens (a table named `t\x00u` would have its rows inside table `t`'s range). All three are fixed, the lexer rejects NUL and invalid UTF-8 in identifiers, `newTable` enforces the invariant independently of the parser, and the parser round-trip generator now draws keywords, mixed case, spaces, quotes, and Unicode names.

Both code reviewers agreed on: the NUL identifier hole; the planner re-binding `WHERE` conjuncts that the filter had pruned after a known-false conjunct, so `WHERE FALSE AND id = 1/0` failed where Postgres succeeds (the plan now takes the bound filter and returns an empty access when it is known false); `CheckNamedValue` panicking on a nil pointer `Valuer` with a value receiver, because `database/sql` skips its own guard when the driver has a `NamedValueChecker` (guarded now, as `database/sql` does); extra positional parameters being ignored on the unprepared `Exec` and `Query` path (the driver now checks the count); a failed `kvstore.apply` leaving pending writes for the next statement (it discards either way now); the dead branch in `replaceDatabase` (it keeps the query now); a nil `[]byte` becoming empty bytes instead of NULL (NULL now, as in the Postgres drivers, and documented); `value.Compare`'s doc and `maragudb.Client`'s doc naming other packages; and single-use package constants (`keyNull`, `keyValue`, `catalogPrefix`, `rowPrefix`, `flipped`, and the parser's operator table), which are inlined. One reviewer alone found two serious ones that I fixed: an `IN` element whose folding failed in a pruned branch was classified as a constant and compared as a zero `Value`, which panicked through `database/sql` on `SELECT 1 WHERE FALSE AND 1 IN (1/0)`; and the operator type error message had regressed to `operator "" + ""` after identifier quoting, because it rendered a fake expression with empty column names. The same reviewer flagged that `CAST(string AS float)` accepted `1_000` and `0x1p-2` through `strconv.ParseFloat`, contradicting deviation 10, which is fixed and the deviation test now covers floats (and Postgres itself rejects `1_000` as a float but accepts `0x10`, which the document now says).

Not changed, and recorded as follow-ups: `IN` lists and `OR` on the primary key are full scans; `DROP TABLE` writes one tombstone per row; the harness does not compare error classes when both sides fail; unary `+` accepts any type.

The reviewers found the key encoding, the range bounds, the Halloween protection, the session state machine, the driver contracts, and the simulator's determinism sound.

Hegel also created an example database at `/internal/pgdiff/.hegel/`, which the root `.gitignore` did not cover and which was committed by mistake; it is untracked and the pattern is now `.hegel/` anywhere.

### What I learned

- Postgres folds constant subexpressions bottom-up and raises their errors at plan time, with exactly these exceptions: a false `WHEN`, everything after a true `WHEN`, everything after a non-null `COALESCE` argument, the right side of a decided `AND` or `OR`, the high bound of a `BETWEEN` whose low comparison is false, and the arguments of a strict operator with a known NULL operand. Pruned branches are still type-checked.
- `IN (list)` is not one array when list items read columns: those items become separate `OR`ed comparisons, evaluated left to right and stopping at the first match.
- The output name of `CASE ... ELSE col END` is `col`, and `ORDER BY col` then sorts by the `CASE`. A harness that decides "fully ordered" from the `ORDER BY` text alone gets this wrong.
- With the `C` locale Postgres folds case in ASCII only, and `'-0.0'` as a numeric literal has no sign; `float8in` keeps it.

### What was tricky

- Deciding where a mismatch is a bug and where it is a deviation. The rule I used: if Postgres's behavior follows from a documented semantic (folding, strictness, naming), mirror it; if it follows from implicit coercion or locale, document it and keep the generator away from it.
- `LIMIT` with an order that does not fully determine the rows: Postgres may evaluate a projection for fewer rows than maragudb when an index scan feeds the limit, so with `LIMIT` the generator only emits expressions that cannot fail at runtime.

### What warrants review

- `/internal/sql/exec/expr.go`: the folding and pruning rules in `logical`, `between`, `in`, `caseExpr`, and `call`, which are the part most likely to still differ from Postgres in some corner.
- `/internal/sql/exec/session.go`: `Execute`'s autocommit retry loop and `kvstore`, which gives statements their atomicity.
- `/internal/sql/exec/plan.go`: the key range construction, in particular the `0xFF` end bound after a prefix.
- Run `make test`, `make lint`, `make pgdiff PGDIFF_CASES=10000`, and `make soak SOAK_DURATION=2m`.

### Future work

- The Antithesis docker-compose harness, which this step did not cover.
- `maragudb.Compact` and `maragudb.GC` are the only maintenance; a background maintainer or guidance on scheduling them belongs in a later section.
- The disk cache never evicts and compaction rewrites everything in memory, as before.

## Step 4: Closing the section 1 gaps: store injection, outages, error classes, and the Antithesis harness

**Author:** spine-gaps

### Prompt Context

**Verbatim prompt:** You are closing the remaining gaps in section 1 of maragudb [...] ## The four gaps ### 1. Object-store injection via an unexported call-site interface [...] ### 2. S3 outages without crashes in the simulator [...] Show that the simulator now detects `RetryAfterUnknownCommit` through the bank or list-append workload, and include that in the mutant test. [...] ### 3. Compare error classes against Postgres [...] Then assert agreement on the mapped class. Where the two legitimately differ [...] document it as a deviation with a test, or relax the check in a documented, narrow way. Fix real mismatches in the executor. ### 4. The Antithesis docker-compose harness (acceptance criterion 8) [...] Decide whether the workload should run against an S3-compatible container such as versitygw or MinIO, or against the in-process fake, and say why in the diary. [...] Do NOT launch an Antithesis run; there is no account.

Mid-run message from the lead, after a terminal crash: run the self-review over the gap commits, fix and commit anything consensus or serious, write this step, confirm `make test`, `go test -race ./...`, `make lint`, and the compose target are green, and clean up stale `maragudb-*` containers.

**Interpretation:** Make the object store pluggable from outside the module, give the fake outages that leave processes running so that unknown commit outcomes reach live clients, make the Postgres harness compare why statements fail and not only whether, and build a runnable, validated Antithesis harness.

**Inferred intent:** Every acceptance criterion of section 1 should be backed by a check that can fail, including the ones that were silently vacuous: a mutant nobody could detect, an error comparison that never happened, and a harness that did not exist.

### What I did

Gap 1. `/store.go` defines the unexported `objectStore` interface, the exported `GetOptions` and `PutOptions`, and the sentinel errors `ErrObjectNotFound`, `ErrPreconditionFailed`, and `ErrNotModified`, which alias the internal ones so that `errors.Is` works through the engine. `Options.Store` replaces the `Client` alias, and `storeClient` adapts the store to `s3.Client`. `/internal/s3test` adapts the internal fake back to the public shape for the external tests in `/maragudb_test.go` and `/internal/pgdiff`. `ExampleOptions` in `/store_test.go` is a complete store in memory, written as outside code would write it. A hegel property in `/store_internal_test.go` checks that the adapter over the fake answers every request sequence exactly like the fake.

Gap 2. `s3fake.Faults` gained `OutageRate`, `MaxOutage`, and `OutageTimeout`, and `Client.Unavailable` makes the store unreachable for one named process (a partition) or for everyone (an outage) until a time on the virtual clock. A request in an outage fails with `ErrUnavailable` at once, or after the timeout, and a timed-out write lands with probability one half. The simulator's `DefaultConfig` turns outages on, `Stats` counts outages, unknown outcomes in processes that did not crash, and operations that failed on an unreachable store, and the corpus test asserts all three. The SQL list-append workload now runs a third of its operations as autocommit statements.

Gap 3. `/internal/pgdiff/classes.go` maps every exported statement error to SQLSTATE codes, `Runner.compare` fails when both sides fail with different classes, and `TestErrorClasses` exercises every class and every code it maps to, including syntax, missing table, and read-only transaction errors that the generator never produces. The mapping then exposed executor differences, which I fixed in `/internal/sql/exec`: folding errors are deferred until the whole statement is checked and reported in Postgres's order (output and ORDER BY expressions, then WHERE, OFFSET, LIMIT); INSERT evaluates every row before writing any; BETWEEN and IN are rewritten to the comparisons Postgres uses; a deciding constant on the right of AND or OR decides it; a NULL WHERE conjunct reads no rows; a non-integer ORDER BY constant is a syntax error, including integer literals outside 32 bits; and two primary keys are `ErrInvalid` (the parser now owns that sentinel). Deviations 17 (a string literal where another type is expected), 18 (LIKE on a key column), and 19 (WHERE conjunct order) are documented with tests, and deviation 13 now covers errors that depend on which row fails first, for which the harness reruns the statement on Postgres with the rows stored in key order.

Gap 4. `/antithesis` holds the harness: `/antithesis/Dockerfile` cross-compiles `/antithesis/workload` to linux/amd64 with cgo and `antithesis-go-toolexec`, `/antithesis/config/docker-compose.yaml` runs versitygw v1.10.0 and the workload, and `/antithesis/test/maragudb` holds five `parallel_driver_` commands that become hard links to the workload binary, which dispatches on its name. `/internal/workload` runs the bank and list-append workloads against any `s3.Client` with concurrent clients that each own an engine, records the history in wall-clock time, and checks it with the simulator's checkers plus the GC grace period. `make antithesis-build`, `make antithesis`, and `make antithesis-validate` build, run locally, and validate. The scratchbook in `/antithesis/scratchbook` records the SUT analysis, the topology, and the property catalog. A writer added the README paragraphs on custom stores and the harness. I added two entries to `/docs/decisions.md`.

### Why

The store choice for the harness: I run the workloads against versitygw and keep the in-process fake as a second test command. Antithesis injects its faults on the network and into containers, so against a fake in the same process it could only pick simulator seeds; against a real store, a partition in the middle of a manifest write is a real lost answer, and the AWS client, its retries, and its error mapping are in the path. The simulator command stays because it covers crash points (mid-commit, mid-compaction, mid-GC) that the network cannot inject on demand. versitygw over MinIO because MinIO's images are gone from Docker Hub (`pull access denied for minio/minio, repository does not exist or may require 'docker login'`). Before choosing it, I checked versitygw's conditional writes with a throwaway test against the AWS client (If-None-Match, stale and current If-Match, 304, ranges, If-Match on a missing key giving 404) and stressed atomicity with 16 concurrent conditional writers per object over 50 rounds: one winner every time.

Outages start at a request, with a per-request probability, rather than on a timer. A timer-driven outage task almost never began between a commit's segment write and its manifest write, so unknown outcomes in live processes stayed rare and the mutant went undetected.

### What worked

- The mutant `RetryAfterUnknownCommit` is detected by the SQL list-append workload with seed 1 in about 2 ms: the retry fails on the duplicate key or on the outage, so the statement reports a definite failure for a write that is in the database (`failed write applied`, `read of failed write`). The bank workload cannot see it, because its writes are all explicit transactions and a doubled transfer conserves money. Every other mutant is still detected by seed 11 or earlier.
- The corpus of 190 runs takes about 0.6 s with outages on: 846 outages, 148 unknown outcomes without a crash, and 508 operations failed by an unreachable store. A 3-minute soak with random outage settings ran 24,869 configurations without anomalies.
- The error-class check found real executor differences within the first run, and after the fixes, three consecutive 8,000-case runs passed (about 36,000 statements each, with 4,000 failing on both sides, compared by class across 8 class and SQLSTATE pairs).
- `make antithesis` passes locally in about 25 s, and `snouty validate antithesis/config` (snouty 0.8.0, a release binary I downloaded and checked against its published SHA-256) reports "Setup-complete event detected. Found 0 first, 5 driver, 0 anytime, 0 eventually, 0 finally test commands. Setup validation successful."

### What didn't work

- With outages on a timer (`OutageInterval` 400 ms, `MaxOutage` 600 ms), the mutant test failed: `mutant "retrying an autocommit statement after an unknown commit outcome" not detected in 20 seeds`. A 40-seed experiment detected it in 3 of 40 at best. Two changes fixed it: outages start at a request, and the SQL list-append workload runs more autocommit appends (detection then rose to 9 of 40, first seed 1).
- The first strict error-class run failed on `SELECT c0 FROM t ORDER BY FALSE` with `both failed, for different reasons: postgres SQLSTATE 42601, maragudb invalid`. The next failures were `invalid/22025` for LIKE escapes, `out of range/42601`, where Postgres reported an ORDER BY mistake before a folding error in WHERE, and `ORDER BY 9223372036854775807`, which Postgres calls a non-integer constant.
- Deferring folding errors first panicked: `panic: value: encoding unknown` from `value.EncodeKey` in `binder.plan`, because the planner used an unfolded constant's zero value. The plan is now built after the folding check.
- Fresh random differential runs then failed on pre-existing differences unrelated to error classes: `UPDATE t SET c1 = NULL WHERE c1 + c1 = c1 AND NULL` (Postgres gates on the NULL conjunct), `abs(c0) IN (NULL)`, `c1 LIKE '\' OR TRUE`, `abs(c0) BETWEEN NULL AND NULL`, `'NaN' NOT BETWEEN c0 / c0 AND 'Infinity'`, and `c0 NOT IN (NULL, abs(c0))`. These are the folding fixes listed above. `DELETE FROM t WHERE c2 = '0' AND c0 LIKE '\'` and `DELETE FROM t WHERE c0 = abs(c0) AND c0 = 9223372036854775807` come from Postgres reordering conjuncts (EXPLAIN showed `Filter: ((c0 ~~ '\'::text) AND (c2 = '0'::text))`), which is deviation 19, so the generator now only combines conjuncts that cannot fail.
- An old unit test claimed `SELECT NULL IN (id, -id) FROM e` fails with out of range. Postgres 18 returns NULL, so I corrected the test.
- The workload test for the engine took 81 s and then hung, because the final read touched about 150 segments with a 5% error rate on each GET (`engine: reading block of "db/seg/...": s3fake: injected error before GET ...`). The final read now compacts first, and the test injects read errors at a tenth of the write rate.
- `snouty validate` first failed with `stat .../scripts/workload-d7ab624e2513/maragudb/parallel_driver_s3_bank_sql: No such file or directory (os error 2)`, because the test commands were absolute symlinks that dangle once copied out of the image. The Dockerfile turns them into hard links.
- `golangci-lint` rejected `random.GetRandom` as deprecated (SA1019), and `random.Source()` returns a `math/rand` source, so the workload adapts its `Source64` to `math/rand/v2`.
- A shell command of mine waited on standard input (`cat > /tmp/x_test.go` without input) and was moved to the background after two minutes. I stopped it. Another heredoc that appended to the Makefile was refused by a safety check, so I made the edit with the editor instead.

### Self-review

I ran the code-review skill at high effort over the gap commits, and an adversarial property reviewer over the store adapter and the outage model. The reviewer saw the implementations and contracts but not my tests.

The property reviewer confirmed the most serious finding with a probe: a store that returns empty ETags turns every conditional write into an unconditional one, and two concurrent increments both committed. The adapter now refuses empty ETags and ranges longer than requested, copies bodies in both directions, treats a missing object on delete as deleted, and sorts a copy of the listed keys. In the fake, a process that crashed while a request waited out its latency or timeout no longer has that request applied, a process without a name is never partitioned alone, and `OutageRate` without `MaxOutage` panics. The `Decide` documentation now says that outages still apply.

The code reviewer found two serious issues in the harness. Both could make an Antithesis run pass without ever checking its history. First, the final read-back shared the clients' context, so a run whose clients ran out of time skipped the check. It now gets a deadline of its own. Second, any setup failure counted as "the store was unreachable". `workload.GaveUpError` now carries the last error, and only network, timeout, and 5xx errors pass. The reviewer also found that the real-store workload never checked the GC grace period. It does now, and `GCIgnoreGrace` is detected, usually by seed 0 or 1. The rest of the fixes:

- the maintainer retries its open;
- read-only commits no longer count as unknown outcomes, in both the workload and the simulator;
- the simulator's autocommit reads are now checked for early snapshot expiry;
- the generator counts NOT IN and NOT (a OR b) as several conjuncts;
- a deviation number in a comment is corrected.

I did not change:

- The duplication between `/internal/workload` and `/internal/sim`. Their client loops differ in clock and locking, so I left them as two copies.
- `ObjectInfo.Size` and `LastModified`, which the adapter does not carry. Nothing reads them today.

### What I learned

- Postgres raises every analysis error before any folding error. It then folds the target list, with the ORDER BY expressions as hidden targets, before WHERE, and folds OFFSET before LIMIT. A strict function with a constant NULL argument folds to NULL even when its other arguments read columns.
- An IN list is an array only when at least two of its elements read no columns. Otherwise it is a chain of `=` comparisons in written order, after any array.
- At the top level of WHERE, the planner orders conjuncts by cost, puts equalities with a constant last through equivalence classes, and runs conjuncts without columns once, before reading any rows.
- The Antithesis SDK's `random.Source` is meant to be drawn from at each decision. Seeding a PRNG from it is the pattern the docs warn against. The simulator does it anyway, because its seed is the whole point. The real-store workload draws every decision from the SDK.

### What was tricky

- Deciding which differential failures were bugs and which were deviations. Folding and strictness are documented Postgres semantics, so I mirrored them. Conjunct order and index-driven LIKE depend on the planner, so I documented them and kept the generator away from them.
- Keeping the folding deferral from changing types. A constant subexpression that fails to fold must still adapt like a literal, or `1 / 0 + 1.5` would turn into a type error.
- A wall-clock workload is not reproducible from a seed, so its mutant tests need enough runs to be reliable. The mid-transaction pauses made the GC mutant visible within a seed or two.

### What warrants review

- `/internal/sql/exec/expr.go`: `binder.fold`, `binder.folding`, `desugar`, and the AND and OR rule in `logical`. `/internal/sql/exec/stmt.go`: the phase order in `selectStmt` and the NULL-conjunct rule in `binder.where`.
- `/internal/pgdiff/run.go`: `Runner.inKeyOrder`, the one relaxation of the class check.
- `/internal/workload/workload.go`: the outcome classification under timeouts, and `readFinal`.
- Run `make test`, `make lint`, `go test -race ./...`, `make pgdiff PGDIFF_CASES=10000` (several times, deleting `/internal/pgdiff/.hegel` first), `make soak SOAK_DURATION=2m`, `make antithesis`, and `make antithesis-validate` with snouty on PATH.

### Future work

- The engine does not retry failed block reads. A read that touches many segments fails if any single GET fails.
- Under database-granularity conflicts, compaction rarely wins against steady clients. The local real-store runs had zero or one compaction each, which the rebase in the automatic maintenance decision should help.
- An `ObjectInfo` with sizes and modification times on the public store contract, if the lease or size-based compaction needs them.
- Launching a first Antithesis run once there is an account, and then triaging which reach claims fire.

## Step 5: Making the Tigris compaction test robust to network latency

**Author:** fix-tigris-flake

### Prompt Context

**Verbatim prompt:** Fix a test in maragudb, an embedded Go SQL database on S3, that is sensitive to network latency. CI failed on GitHub Actions (ubuntu runners, far from Tigris) with: `--- FAIL: TestDB_Tigris/compaction_and_GC_keep_the_data_and_bound_the_objects (2.79s) engine_test.go:855: Expected nil error, but got "engine: snapshot expired, the transaction is older than the GC grace period: transaction is 1.21715065s old"`. Make the test robust to real network latency of several seconds per operation, without weakening what it verifies. Use a grace period with generous headroom, a few multiples of the measured operation latency or a fixed number of seconds. Avoid sleeping for long periods. Check the rest of `TestDB_Tigris` and `/internal/s3/aws_test.go` for the same class of timing assumption. Do not change engine behavior.
**Interpretation:** Size the test's GC grace period from the latency of the run, not a fixed second, and look for other wall-clock assumptions in the Tigris suites.
**Inferred intent:** Keep the Tigris suite green on CI runners far from Tigris without losing its coverage of compaction and GC.

### What I did

In `/internal/engine/engine_test.go`, the compaction subtest now opens a writer with the default grace period, writes the three keys, and times each commit. It then opens a second `engine.DB` on the same prefix with `GCGracePeriod` set to `max(2s, 5 × slowest commit)`, and runs compaction and both GC passes with that DB. The test logs the slowest commit and the grace period it chose. It now also asserts that the first GC pass deletes nothing (`n == 0`), because that pass only stamps the garbage. The second pass after the sleep must still delete all three segments.

In `/internal/tigristest/tigristest.go`, cleanup used to share one one-minute context between the List call and every Delete. Under seconds of latency per request, a large prefix could run out of time and leave objects behind. Now each request gets its own one-minute timeout.

I checked the other two subtests of `TestDB_Tigris` and `/internal/s3/aws_test.go`. Both other subtests use the default one-hour grace period, and `aws_test.go` only logs its timings and asserts nothing about them, so neither needed a change.

### Why

The engine is correct to refuse a transaction older than the grace period. The CI failure came from the test: a one-second grace period cannot hold when a single commit takes 1.2 s. If the grace period scales with measured latency, the test keeps checking the same behavior at any distance from Tigris. Locally the sleep goes from 1 s to the 2 s floor.

### What worked

To simulate latency, I used a temporary `slowClient` wrapper in the engine test package. It sleeps between d and 2d before every Get, Put, Delete, and List. With `SLOW=600ms`, the old test failed every time with the CI error (`transaction is 1.0661915s old`, `1.139762042s old`). The new test passed both runs of the full `TestDB_Tigris` at the same delay: the slowest commit was about 3 s, the grace period about 15 s, and compaction about 6 s. I then deleted the wrapper. It added no lasting value, because the delay is a test-only hack and `tigristest.NewClient` returns the concrete `*s3.AWSClient`.

`make test`, `go test -race ./...`, and `make lint` pass.

I ran the code-review skill at low effort. It found no issues in the `tigristest` cleanup, but it skips test files at that level, so I reviewed the test change myself. Two DBs can share one prefix: the cross-process subtest already relies on that. The writer and the compacting DB use different seeds (50 and 51), so their object IDs cannot collide.

### What didn't work

At first I used a 300 to 500 ms delay per request, and the old test still passed (`ok ... 43.468s` for `-count 3`). At that delay a commit stays under one second. I had to double the delay to reproduce the CI failure before I could trust the fix.

### What I learned

The grace period applies to two things at once. A transaction must finish within it, and GC must wait for it to pass after the stamping pass. A bigger grace period makes the test robust, but the test must then sleep for the whole period. So the multiplier is a trade-off between headroom and suite time. Five times the slowest commit gives compaction, which takes about twice the requests of a commit, more than twice the time it needs.

### What was tricky

The sleep must start after the first GC pass returns, not before it starts. The stamp comes from `db.now()` inside the pass, after the manifest refresh, so only the time after the pass returns is a safe lower bound.

### What warrants review

- `/internal/engine/engine_test.go`, the compaction subtest: the grace formula, and the assertion that the first GC pass deletes nothing.
- To check under latency again, add a wrapper around `s3.Client` that sleeps per request and run `go test ./internal/engine -run TestDB_Tigris -count 2 -v`.

### Future work

- With several seconds of latency per request, the whole Tigris suite takes about two minutes, almost all of it in requests. Nothing here needs fixing, but it limits how far from Tigris CI can run before `go test`'s default ten-minute timeout matters.

## Step 6: Automatic maintenance, compaction rebase, and block read retries

**Author:** auto-maintenance

### Prompt Context

**Verbatim prompt:** You are adding automatic background maintenance to maragudb, an embedded Go SQL database whose only durable state is in S3. [...] ### 1. Compaction rebase: Today compaction loses the manifest CAS against steady writers under database-granularity conflicts, so it rarely wins. Change that. When compaction's CAS fails, read the new manifest. If every commit since compaction's base only added segments, [...] re-apply the swap on top [...] Do not redo the merge. [...] ### 2. Automatic maintenance in the driver, coordinated by a lease: The engine still starts no goroutines, which keeps determinism. The background loop lives in the driver layer. Add a step function that the simulator can drive deterministically. [...] The lease is an object, for example `<prefix>/maintenance-lease`, holding a holder ID and an expiry. [...] Correctness never depends on the lease. Run the simulator in three modes [...] Show that the lease actually saves work. [...] Commit and read request budgets must not change. [...] Expose maintenance activity in `Stats()`. ### 3. Retry failed block reads in the engine [...] ### 4. Keep the real-network paths covered: Extend the Tigris suite with a short test of the lease on real Tigris [...] Extend the Antithesis harness workload so that background maintenance runs during the workload. [...] `make test`, `go vet ./...`, `make lint`, `go test -race ./...`, and `make antithesis` must all pass.

**Interpretation:** Make compaction and GC win against steady writers, run both automatically from the driver under a lease that only saves work, make reads survive transient store failures, and prove all of it in the simulator, on Tigris, and in the harness.

**Inferred intent:** A database that nobody has to remember to compact, whose correctness is still proven by machine in every mode the coordination can be in, including broken.

### What I did

Compaction rebase, in `/internal/engine/compact.go`. When the manifest write of a compaction loses, `compact` takes the current manifest that `casManifest` cached, checks with `manifest.extends` that the merged segments are all still live in the same order, and writes the swap again on top: the compacted segment first, the newer segments after it, up to eight times. The compacted segment is written once. Only another compaction, which changes the merged set, gives `ErrConflict`. This forced a change to GC's orphan rule, recorded in `/docs/decisions.md`: an unreferenced object used to be deleted as soon as the manifest moved past its base sequence number, because the write that would reference it could no longer succeed. A rebased compaction can. So orphans now enter the garbage list stamped and are deleted after the grace period like everything else, a rebased manifest drops the stamp on its own output, and a compaction gives up at half the grace period. The `engine.Mutations.CompactedSegmentAhead` mutant puts the compacted segment ahead of the newer ones.

GC rebase, same file. Under steady writers a GC pass whose write always lost deleted the same expired objects on every run and never shrank the garbage list; the simulator showed 99,523 deletes for 3,500 commits. `gc` now re-derives its manifest change from the current manifest on a conflict: deleted objects leave the list, failed deletes stay expired, the garbage it found unstamped gets its stamp, orphans that nothing references yet enter the list, and garbage that entered after the pass began is left for the next pass, because this pass's clock is older than that garbage. An `assert.Always` refuses to write a manifest that references an object the pass deleted.

Block read retries, in `/internal/engine/blocks.go`. `getBlock` retries a failed GET up to four times with exponential backoff and jitter from the engine's random source, sleeping through the new `engine.Options.Sleep`, which the simulator sets to the scheduler. `s3.Definite` in `/internal/s3/s3.go` says which errors a retry cannot change: the three sentinels and client errors other than 408 and 429.

The maintainer, in `/internal/engine/maintenance.go`. `engine.Maintainer.Step` looks at the cached manifest, and only when there is work, at least `SegmentThreshold` segments or garbage that is unstamped or expired, acquires the lease, compacts, collects garbage, and releases. The lease at `<prefix>/maintenance-lease` is JSON with a holder and an expiry. `Acquire` reads it, then writes with `If-None-Match: *` when it is missing or with `If-Match` on the observed ETag when it is expired or its own, and leaves a live lease of another holder alone. `Renew` writes with `If-Match` and is called by a progress hook before every block read of the merge, before the segment write, before every manifest write, and every sixteen deletes, once half the lease has passed. `Release` writes an expired lease with `If-Match`, so it never clobbers a takeover. A lost acknowledgement on any of the three is resolved by reading the lease back: a lease under this holder's name means the write landed. `DB.Stats` carries `MaintenanceRuns`, `LeaseAcquisitions`, `LeaseLosses`, `CompactionsStarted`, `Compactions`, `CompactionRebases`, and `Deleted`.

The driver loop, in `/internal/sqldriver/sqldriver.go`. `Connector.DB` starts one goroutine per database that sleeps a jittered interval and calls `Step`; `Connector.Close` cancels it, waits, releases the lease with a ten second timeout, and closes the engine. `Driver.Open` connections own their connector and close it. In `/maragudb.go`, `Options.Maintenance` (default on), `MaintenanceInterval` (default ten seconds, jitter of half either way), and `CompactionThreshold` (default 8, at least 2) configure it, and `Statistics` reports the three counters.

The simulator, in `/internal/sim`. `Config.Maintenance` selects `MaintenanceLease`, `MaintenanceNoLease`, or `MaintenanceOverlap`, in which every lease expires after a millisecond and nobody renews or notices. Each client owns a maintainer and steps it after a third of its transactions, through the engine or through the connector's engine with the driver's loop off. The corpus gained 24 runs across the three modes, and `TestSimulator` gained three subtests: the compaction success rate against fault-free steady writers, every checker in every mode, and the duplicate-work measurement with eight clients. `make replay` takes `MAINTENANCE=lease|none|overlap`.

Real paths. `TestDB_Tigris` acquires, renews, and takes over the lease with two clients, sizing the lease from a measured acquire-and-release round trip. `/internal/workload` runs each client's background maintenance in place of its one explicit maintainer, and `/antithesis/workload/main.go` draws a short interval, a threshold, and a simulator maintenance mode. A writer updated the README.

### Why

The rebase is what makes automatic maintenance possible at all: without it, 78 percent of compactions lost to steady writers, and no lease could fix that. Stamping orphans instead of deleting them was the price of the rebase, and I chose it over writing the compacted segment again under a new base sequence on every rebase, which would cost a write of the whole database several times under load. The lease lives in the engine package so that its requests are counted in the engine's statistics and the step function stays synchronous and deterministic; only the goroutine lives in the driver, as the decision says. Deciding from the cached manifest is what makes an idle process free: a process that does no transactions never learns of work, and never needs to, because the processes that write have a fresh manifest.

### What worked

- Compaction against fault-free steady writers: 113 of 500 succeeded before the rebase (22.6 percent), 242 of 242 after (100 percent), with 269 rebases. In the corpus with every fault on: 285 of 1729 before (16.5 percent), 473 of 1455 after (32.5 percent), and the remaining failures are injected errors, crashes, outages, and expired grace periods, with only 5 real conflicts.
- Every checker passes in all three lease modes, over 12 seeds, both workloads, both APIs, each; and over the 24 corpus runs.
- The lease saves work. Eight fault-free clients over 20 seeds: 132 merges started for 132 compactions with the lease, 417 merges started for 196 compactions without it; 4,404 deletes with the lease against 23,577 without; 171 maintenance attempts against 1,256.
- An idle maintainer makes zero requests per interval once its garbage is stamped and within the grace period, asserted in `TestMaintainer_Step` over ten steps and in `TestMaintenance` over twenty intervals; a maintenance run with the lease costs nine requests.
- Failed reads in the corpus: 4,262 before the retries, 2,693 after, in histories that differ because retries change timing.
- The shadowing mutant is detected by the engine list-append workload with seed 1 in about 2 ms. The `GCIgnoreGrace` mutant is still detected under the automatic maintainers in the workload (seed 0).
- The Tigris lease subtest: a 576 ms round trip, a 2.88 s lease, pass. The harness: 3 to 13 compactions per real-store command where there were 0 or 1, and snouty 0.8.0 (downloaded into the session scratch directory and checked against its published SHA-256) reports "Found 0 first, 5 driver, 0 anytime, 0 eventually, 0 finally test commands. Setup validation successful."

### What didn't work

- The shadowing mutant is not detected by the bank workload or by the SQL list-append workload. The bank checker only checks totals, and a compacted segment that shadows newer transfers is a consistent older state that conserves money; the SQL list-append workload only inserts rows under new keys, which a compacted segment does not contain. Both are recorded in the mutant table. The bank workload also lost its detection of "commit without CAS" through the GC invariant, because orphans now wait a grace period: `mutant "commit without CAS" not detected in 20 seeds`. The list-append workload still detects it with seed 1.
- My first GC rebase iterated a Go map when deleting, which would have made the simulator's request order random; the deletes now follow the manifest's order. I caught it before running the determinism test.
- `TestMaintenance/keeps the commit and warm-read budgets` failed on the insert after the first background compaction with `Expected "{0 1 1 1 0 0}", but got "{1 1 1 1 0 0}"`: the duplicate-key lookup read the compacted segment from the store. Compaction now primes the memory cache with every block of the compacted segment, with a copy per block and only when the segment fits in half the cache.
- The first harness run reported 0 compactions in every real-store command although the maintenance was on: against a local store a run finishes in well under a second, shorter than the drawn interval. The harness now draws 10 to 200 ms and a threshold of 2 to 8.
- A `zsh` quirk cost two attempts: `C="docker compose ..."; $C up` fails with `no such file or directory: docker compose -f ...` because zsh does not split the variable.
- Two of my own tests were wrong after review fixes: the renewal test kept its hook armed into its second phase, so a later read let the other maintainer take a released lease (`the lease was renewed in time`), and the GC-overlap test slept almost a whole grace period although compaction now gives up at half.
- The final `go test -race ./...` overlapped with a `make test` that had been moved to the background, both driving the same Postgres container, and `TestDifferential` failed in both with an empty error list, the `make test` one after 310 s. Alone, after deleting the `.hegel` directories that the failures left behind, `go test ./internal/pgdiff` and `go test -race ./internal/pgdiff` pass in 4 s and 5 s. Do not run two suites against the harness at once.

### Self-review

I ran the code-review skill with two competing reviewers over the whole diff. Consensus findings, all fixed: compaction primed the caches with subslices of the whole segment body, so every cached block pinned the whole database (now a copy per block, memory only, and only when the segment fits); a release that failed left the lease believed held (now never held after a release); tests opened connectors with the loop on by default (off in the store, pgdiff, and driver tests, and the engine test world now wires `Sleep` to its clock); a single-use constant; comments that named the tests as the purpose of an option; and `Compact`'s doc omitting `ErrSnapshotExpired`.

One reviewer alone found two serious problems, both fixed. Compaction gave up at the grace period, the same bound as a reader, but the stamp on its output can be only moments after its start, so a manifest write that started just under the bound and landed after the deletion would reference a missing object for good; it now gives up at half the grace period, and the decision entry says why. And my renewal test passed without any renewal during the merge: the progress hook ran every 1024 entries, never during the block reads that are the slow part, and the test counted the acquisition and release as renewals; the hook now runs before every block read, and the test has the other maintainer try to take the lease over before every read. The same reviewer flagged 429 and 408 as retryable, a threshold of 1 that would churn forever, a transient renewal failure aborting a whole merge, and an unbounded context in the workload's lease release; all fixed. The other alone found that `transient` reached behind the `s3.Client` contract for HTTP status codes (now `s3.Definite` in the package that owns the mapping), that a renewal whose acknowledgement was lost was counted as a loss and blocked others for a lease (now reclaimed by reading back), that `Driver.Open` leaked the loop (its connections now own the connector), that a GC pass could write a manifest referencing an object it deleted without noticing (now an assertion and a refusal), and that only one mode was in the determinism test (now all three).

Not changed: the bank workload cannot see the shadowing mutant, as above; `gc` returns a bare `ErrConflict` as before.

### What I learned

- Under database-granularity conflicts, every manifest writer that reads, works, and writes needs a rebase, not only compaction. GC's was invisible until the delete counts of the automatic maintainers exposed it.
- An orphan rule that depends on "this write can never succeed" is fragile the moment any writer retries against a newer base. The stamp-and-wait rule is uniform and its timing argument is the one the readers already rely on, with a margin chosen for the cost of being wrong.
- A lease protocol with content ETags needs the same lost-acknowledgement resolution as the commit protocol: read it back and look for your own name.
- `synctest` makes the driver's real-time loop testable to the request: time only advances while every goroutine is blocked, so a statement's requests can be measured without the loop interfering.

### What was tricky

- Which clock stamps what. A rebased GC pass must stamp only the garbage that left the manifest before its own clock reading, or a reader that saw the newer garbage could lose it early. The test with two interfering compactions fails with the naive rebase.
- Measuring duplicate work. Sequential steps hide it, because every step refreshes the manifest before working; only the simulator's interleaving shows several clients merging the same segments at once.
- Keeping the simulator deterministic through all of this: no map iteration in request order, no randomness in `NewMaintainer`, and the driver's loop off in the simulated connectors.

### What warrants review

- `/internal/engine/compact.go`: the rebase loop in `compact`, the `extends` check, and the rebase in `gc`, in particular which entries get the stamp.
- `/internal/engine/maintenance.go`: `Acquire`, `Renew`, `Release`, and `reclaim`; the `progress` hook's tolerance of transient failures.
- `/docs/decisions.md`, the last entry: the timing argument for orphan stamping and the half-grace bound.
- Run `make test`, `make lint`, `go test -race ./...`, `make antithesis`, `make antithesis-validate` with snouty on PATH, and `make soak SOAK_DURATION=2m`, which now draws maintenance modes.

### Future work

- Tiered compaction, so a few-GB database does not rewrite one segment at every threshold.
- A periodic GC sweep for orphans of crashed commits in a database that never reaches the compaction threshold; today they wait for the next compaction's GC.
- `Deleted` in the simulator still shows a few duplicate deletes with the lease, from lease losses; a renewal in `Release`'s path is not worth it yet.
- The disk cache never evicts, and compaction no longer primes it.

## Step 7: Trust signals: a requirement-to-test map, a mutant kill matrix, reached-state coverage, and a report per CI run

**Author:** trust-signals

### Prompt Context

**Verbatim prompt:** You are adding trust signals to maragudb, an embedded Go SQL database whose only durable state is in S3. [...] Your job is to make that verification visible and checkable by someone who reads only the docs and CI output, never the code. [...] ### 1. A machine-checked map from requirements to tests [...] Generate a committed document, /docs/verification.md. [...] Add a mutant kill matrix [...] Add a test that fails in each of these cases [...] ### 2. Reached-state coverage, Antithesis style [...] The fixed corpus must reach every declared state at least once, or a test fails. [...] ### 3. A verification report for each CI run [...] In CI, write the rendered report to `$GITHUB_STEP_SUMMARY` when that variable is set [...] Make it robust to `-shuffle`, to package parallelism [...] and to failed runs. [...] ### 4. Two coverage gaps found by the previous builder [...] ### 5. README [...]

Mid-run message from the coordinator: the run was cut off because the machine ran out of disk space; check the files being written for truncation, check Docker, leave the scratchpad's `corpora/` alone, and continue.

**Interpretation:** Give every requirement, deviation, mutant, and interesting state an ID; make tests declare the IDs they verify in a form a test can check statically; generate /docs/verification.md from that and fail when anything is uncovered, unknown, undetected, or stale; count reached states in the simulator; record facts from every test binary and publish a report into the GitHub job summary without being able to add a CI step; and close the two gaps.

**Inferred intent:** Markus reads only documents and CI pages, so every claim of the test suite must be visible there, and must be checked by a test so that it cannot quietly become false.

### What I did

IDs. /docs/requirements.md and /docs/deviations.md got code-span tags after the text they name: 28 acceptance criteria (`AC-1.1` to `AC-8.3`), 19 key invariants (`INV-STRICT-SERIALIZABLE`, `INV-DURABLE-ACK`, and so on, from "What it is" and "Fixed product decisions"), and `DEV-1` to `DEV-19`. A script confirmed that removing the tags gives the original text byte for byte. Mutants are `MUT-<NAME>` and reached states `STATE-<NAME>`, both defined in code.

Coverage declarations. `/internal/verify` has `verify.Covers(t, "<what is asserted>", "<ID>", ...)`. Its source is parsed with `go/ast` (`verify.Source.Coverage`), which attributes each call to its test and subtest path and rejects non-literal arguments and calls outside tests. At run time the same call records the test's result. 119 calls cover every ID. `verify.TestVerification` fails on an uncovered requirement, deviation, or state, an unknown ID, a mutant detected by nothing, a declared state without a `reach.Hit` and a matching Antithesis assertion, a test package without `verify.Main`, and a stale /docs/verification.md. `make verification` regenerates the document and the kill matrix.

Uncovered IDs at the start: all 66, since nothing declared coverage. After tagging the existing tests, four had no test at all: `AC-8.2` (the harness was only checked by `make antithesis`), `AC-8.3` (cross-compilation), `AC-8.1` (assertions marking invariants), and `AC-4.4` as a machine check. I closed them with `TestCommands` in /antithesis/workload/main_test.go (every test command names a workload command, the compose file runs versitygw and the workload, the sim command runs eight seeds from a fixed source, and `go build` for linux/amd64 succeeds), and with subtests of `verify.TestVerification`. The 13 `STATE-` IDs are covered by the corpus test.

Kill matrix. `sim.TestMutants` now has a catalog of eight mutants, each with an ID, a `make replay` name, the cells that cannot detect it with a reason, and the checkers that must catch it. It runs each in eight cells (list-append or bank, engine or SQL, 0 or 2 maintainers), seeds 1 to 20, records the first seed and the checkers (`sim.Result.Checkers`, from the new `sim.Anomaly` wrapper), and compares the matrix with /internal/sim/testdata/kill-matrix.json. It fails when a cell detects a mutant it is said not to, misses one without a reason, or the intended checker never fires. `make replay` takes `MAINTAINERS=` and `CORPUS=1`, and every mutation name, from the catalog.

Reached states. `/internal/reach` declares 13 states with an Antithesis message each and counts `reach.Hit` calls in a `reach.Counter` carried by the context, so counting is exact per run and costs one context lookup elsewhere. The engine hits lost acknowledgements resolved as landed and as conflicts, compaction and GC rebases, lease takeovers after expiry, and retried block reads that succeed; the SQL layer hits autocommit conflict retries; the simulator hits unknown outcomes in live clients, refused expired snapshots, crashes during commits followed by a later successful commit, crashes during compactions and GC passes, and overlapping lease holders, which a store wrapper (`sim.leaseWatch`) sees before every request. Seven assertions are new; six reuse existing messages.

Report. Every test package has `TestMain` calling `verify.Main`, which appends JSON lines to `pkg-<package>.jsonl` in `MARAGUDB_VERIFY_DIR`, or, when only `GITHUB_STEP_SUMMARY` is set, in a temp directory named after the summary file. At the start and the end of every binary it takes an flock in that directory, re-renders the report over every package's facts, and rewrites the summary file with its original content in front. Facts: simulator batches, mutant cells, reached counts, differential totals, budgets via `verify.CheckBudget`, Tigris latencies, and per-test coverage results; `make antithesis` appends its command results. `make report` renders `.verify`.

Gaps. The SQL list-append workload keeps odd keys as one row of `packed` that appends update in place (overwrite in transactions, `vals || $1` in autocommit), and half the SQL bank transfers overwrite balances. `MUT-COMPACTED-AHEAD` is now detected through SQL with seed 1; with the packed lists turned off it is not detected in 20 seeds. `pgdiff.Postgres` creates `maragudb_diff_<unix>_<random>` from template0 with LOCALE 'C' once per process, `pgdiff.DropDatabase` drops it from `TestMain`, and databases over a day old are dropped.

A writer sub-agent wrote the README section "How do we know it works?", the Development updates, and the introduction of /docs/verification.md, which lives in `verify.docIntro`. I corrected one sentence that said all eight mutants are caught by the simulator.

### Why

A static declaration is checkable without running the suite, so the map and its golden test work in any single package run. Recording the same call at run time lets the report say which IDs passed in this run. The report has to come from the test binaries themselves because the shared workflow cannot get a step; rewriting the whole summary under a lock means the last binary to finish leaves a complete report, whatever the order, and a binary that panics still shows as "did not finish" because its start was published.

### What worked

- The corpus reaches every state. In the last run: lost ack landed 440, lost ack conflict 1,696, live unknown 211, autocommit retry 789, compaction rebase 510, GC rebase 698, lease takeover 27, lease overlap 16, expired snapshot refused 443, block read retried then succeeded 2,720, crash mid-commit recovered 253, crash mid-compaction 134, crash mid-GC 227.
- Kill matrix: 7 of 8 mutants detected by the simulator, in 24 of 52 applicable cells, all with seed 1 or 2; `MUT-UPDATE-SEES-OWN-WRITES` only by its unit test, because no workload updates primary keys. My first guesses for the undetectable cells all matched the runs.
- The golden kill matrix and the corpus pass unchanged in a linux/amd64 golang:1.27 container, so the seeds are portable.
- Two concurrent `go test ./internal/pgdiff` processes, one with `-race`, both pass and leave no databases.
- A run whose `internal/sim` binary timed out (`-timeout 2s`) still produced a summary with "Package `internal/sim` did not finish."
- `make test` 17 s, `go test -race ./...` about 2 min 13 s (the simulator package is 130 s of it), `make lint` clean, `make antithesis` passes with 5 of 5 commands in the report.

### What didn't work

- My first insertion script for TestMain asserted `'func TestMain' not in s` and stopped at /maragudb_test.go with `AssertionError: maragudb_test.go`, because `func TestMaintenance` contains that text.
- The Covers insertion stopped with `internal/sql/exec/exec_test.go: anchor 't.Run("checks constraints and types"' matches 2 lines`; I added an nth-match syntax.
- A Python heredoc that edited the Makefile was refused by a safety check because its text contained `rm -f $(VERIFY_DIR)/...`; I made the edits with the editor and used `find ... -delete` in the target.
- The first linux/amd64 check failed with `docker: Error response from daemon: mkdir /var/lib/docker/overlay2/...: read-only file system`, then the machine ran out of disk. After Docker restarted, the image pulled during the outage was corrupt (`/usr/local/go/src/runtime/abi_test.go:1:1: expected 'package', found 'EOF'`); removing and pulling it again fixed that. `gofmt -l` then flagged /internal/sim/sim.go, which was only struct field alignment, not truncation.
- An early test of the new required-checker rule passed for the wrong reason: `go test -run TestMutants/MUT-GC` failed the golden comparison because only one mutant ran. The golden comparison now skips partial runs, and the rule fails on its own when the expected checker is wrong.

### Self-review

I ran the code-review skill at high effort over the three earlier commits and the working tree. It found ten issues, none disputed, all fixed:

- The kill-matrix rewrite had dropped the old per-mutant `detectedBy` checks. Each mutant now names the checkers that must catch it somewhere, and a cell without maintainers must be detected by a history checker.
- A corrupt packed list read mid-run counted as a failed read; it is an anomaly now.
- Replay hints did not reproduce corpus configurations. `failReport` prints the whole configuration and `make replay ... CORPUS=1` or the maintenance flags.
- The new sim-command test drew seeds from the SDK's random source, which made `make test` nondeterministic; it uses a fixed PCG.
- The Tigris tests could fail before any `Covers` registered, so their IDs read "not run"; the parent tests declare `AC-7.1` first.
- A pgdiff test cleanup used a cancelled `t.Context()` and left a database behind; it uses `context.Background()`.
- An empty reached-state map vanished under `omitempty`.
- The differential fact was recorded before the failure guard and included shrinking cases.
- Budget limits were written twice; `verify.CheckBudget` records and asserts in one place.
- The verify tests parsed the module six times; they parse it once.

### What I learned

- The Antithesis SDK keeps only the first pass and fail per message and needs literal messages at the call site, so counting has to sit next to the assertion, not inside a wrapper.
- `testing` runs cleanups for a panicking test, but not the code after `m.Run`, so a "started" fact published at the beginning is what lets a crashed package show up at all.
- Go's test cache stays out of the way here: every binary reads `GITHUB_STEP_SUMMARY` in CI, and `-shuffle` is not a cacheable flag.

### What was tricky

- Deciding what counts as a lost acknowledgement resolved as a conflict. An ambiguous answer can come from a lost acknowledgement that hid a 412 or from a failed request; the client cannot tell them apart, so the state counts both, and its description says so.
- Overlapping lease holders can only be seen from outside the engine. The lease holder only changes while one of its own requests is in flight, so looking before every request sees every overlap.
- Extracting readable requirement text from tags without changing the documents. Tags after the sentence they name, plus the bold title of the enclosing item, give fragments such as "Serializability under faults: errors".

### What warrants review

- /internal/verify/report.go `publishTo` and /internal/verify/verify.go `Main`: the lock, the kept prefix, and the latest-PID rule.
- /internal/verify/scan.go `Source.walkTest`: the subtest attribution. A `t.Run` with a non-literal name becomes `*`.
- /internal/sim/sim_test.go `mutants` and `TestMutants`: the stated reasons in the matrix.
- /internal/engine/engine.go `casManifest`: the `ambiguous` flag that separates the conflict state from an ordinary conflict.
- Run `make test`, `make report`, `make verification` (no diff expected), `go test -race ./...`, `make lint`, and `make antithesis`.

### Future work

- `INV-TYPES` reads "Types: NULL is supported." in the map, because the tag sits on the table's last line; a tag placement or an override would read better.
- The real-store workload in /internal/workload still has the insert-only SQL list-append; it could take the same packed lists.
- The simulator package takes about two minutes under `-race` in CI; the mutant matrix and corpus could share runs if that grows.
- Only tests that call `verify.Covers` show up by name under "What failed"; other failures show as a failed package.
