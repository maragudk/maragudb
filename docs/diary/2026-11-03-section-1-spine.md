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
