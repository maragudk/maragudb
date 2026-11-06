# Project Decisions

This document records significant architectural and design decisions made throughout the project's development.

## 2026-11-03: Embedded SQL database with S3 as the only durable state

maragudb is an embedded Go library, like SQLite. All durable state lives in S3-compatible object storage. Caches in memory and on local disk are allowed in the style of turbopuffer, but correctness never depends on them. The target workload is small OLTP app databases.

Alternatives considered:
- A standalone server with a wire protocol. Rejected for now because it adds operational parts and makes testing harder.
- SQLite plus Litestream or LiteFS. This already exists and keeps a local disk as the source of truth, so it is not what this project is for.

## 2026-11-03: Multi-writer optimistic concurrency, strict serializable

Any number of processes can read and write the same database. A commit is a compare-and-swap write that uses S3 conditional writes (`If-Match` / `If-None-Match`), so no external lock service is needed. The isolation level is strict serializable, because it is the easiest guarantee to state and to verify with a history checker.

At first, conflicts are detected at database granularity: any concurrent write commit is a conflict. Single statements outside a transaction retry automatically. Explicit transactions get a typed retryable error. This costs throughput under contention, which is acceptable for small OLTP workloads, and finer granularity can come later.

Alternatives considered:
- Single writer with a lease. This is simpler, but it is close to what Litestream and LiteFS already do.
- Snapshot isolation. It allows more concurrency, but it permits write skew and is harder to test.

## 2026-11-03: Own SQL flavor with Postgres semantics and Go-like types

The SQL dialect is our own. It follows Postgres semantics where they are sensible, and it has none of SQLite's quirks: no type affinity and no implicit coercion. The types map to Go:

- `int` (int64, overflow is an error)
- `float`
- `bool`
- `string`
- `bytes`
- `time` (a UTC instant with nanosecond precision)

Features are added in sections, so the project does not try to reproduce all of Postgres.

Postgres is the differential test oracle. Generated query ASTs are rendered to both dialects, and the results are compared. Intentional deviations are documented and tested.

## 2026-11-03: `database/sql` driver with explicit registration

The API is a `database/sql` driver. It is registered with `maragudb.RegisterDriver(maragudb.Options{...})` instead of an `init` side effect, following the pattern in maragudk/sqlite. This keeps compatibility with sqlc and existing code, and the defaults are opinionated and explicit.

## 2026-11-03: Automated verification only, with in-house deterministic simulation testing

The code is not reviewed manually, so automated tests are the quality gate. Correctness is verified by:

- A seeded deterministic simulator. Concurrent clients run against a fault-injecting fake S3, and the faults include errors, latency, lost acknowledgements, and crashes.
- History checkers for strict serializability.
- Mutation checks that prove the checkers can fail.
- Hegel property-based tests.
- Differential testing against Postgres.
- An integration suite against Tigris.

The design is Antithesis-ready, with SDK assertions and a docker-compose harness, but the project does not depend on the Antithesis platform. This is because the platform is commercial and its instrumentation targets amd64 only.

## 2026-11-03: Storage layout: a CAS-replaced manifest over immutable B+tree segments

Context: the core must give strict serializable multi-writer commits with nothing but S3 conditional writes, O(1) PUTs per commit, and O(log n) GETs per cold point lookup.

Decision: one JSON manifest object at `<prefix>/manifest` is the root of the database. It lists the live segments, a garbage list, and a window of the last 128 commit records. Every commit writes one immutable segment object (a sorted run laid out as a B+tree of blocks, read with range GETs) and then replaces the manifest with `If-Match: <etag>`. Conflict detection is the manifest ETag, so it is at database granularity, as the requirements allow for section 1. Compaction merges every segment into one and moves the old ones to the garbage list. A commit whose answer is lost is resolved by reading the manifest back: the commit landed when the record at its sequence number carries its commit ID, it lost when another ID is there, and it is retried with the same body and ID while the manifest is unchanged, so a commit can never be applied twice. Only when the store stops answering altogether does the client get `ErrCommitUnknown`.

Alternatives considered:
- An append-only log of commit objects with the manifest as a pointer. Rejected because a reader would have to replay the log, and the number of objects would grow with commit count until compaction.
- A per-key or per-range conflict check. Deferred, as the requirements say; the manifest layout leaves room for it later because segments carry key ranges.

## 2026-11-03: Garbage collection by stamped grace period and sequence-number fencing

Context: GC must never delete an object that a reader within the grace period can still reach, and it must be able to clean up objects left by crashed commits, without any coordination between processes.

Decision: segment keys embed the manifest sequence number they were written against (`seg/<seq>-<id>`). An object that is in neither the segment list nor the garbage list is an orphan, and it is deleted as soon as the manifest sequence has moved past its base sequence, because the conditional write that would have referenced it can no longer succeed. Objects that leave the segment list enter the garbage list unstamped; the first GC pass that sees them stamps them with its own clock, taken after it read the manifest, and a later pass deletes them once the stamp is older than the grace period. Readers sample their start time before their freshness read, so a reader younger than the grace period always precedes the stamp and never loses an object. A reader older than the grace period gets `ErrSnapshotExpired`.

Alternative considered: stamping at compaction time. Rejected because the compactor samples its clock before its conditional write lands, which lets a reader that began in between lose objects slightly before its grace period ends.

## 2026-11-03: Deterministic simulation with a cooperative scheduler in virtual time

Context: the simulator must reproduce a history exactly from its seed, but Go schedules goroutines nondeterministically.

Decision: simulated processes run as goroutines under `sched.Scheduler`, which hands a single baton to one task at a time. The only yield points are the fake store's requests, which sleep for a seeded virtual latency, and explicit pauses. Fault decisions, latencies, commit identifiers, and the engine's clock all derive from the seed, and the engine starts no goroutines of its own. The same seed therefore produces an identical history, and a failure is replayed from the seed alone.

Alternative considered: `testing/synctest` with a fake clock. Rejected because the order in which goroutines wake at the same instant is still up to the Go scheduler, so histories would not be byte-identical across runs.

## 2026-11-03: SQL layer: hand-written parser, rows keyed by primary key, statement-level atomicity, strict typing

Context: section 1 needs a SQL surface over the key-value core, and the project's own dialect follows Postgres semantics without SQLite's quirks.

Decisions:
- The lexer, parser, binder, and executor are written by hand in `/internal/sql`. A parser dependency would bring a dialect that is not ours and far more syntax than the sections need, and a hand-written recursive-descent parser with the Postgres precedence table is small.
- Every table has a primary key and there is no hidden row id. A row lives at `r/<table>\x00<key>`, where the key is the order-preserving encoding of the primary key columns, and the table definition at `c/<table>`. Point lookups and range scans on the primary key fall out of the key encoding, and the catalog is transactional because it is just more keys.
- Statements are atomic on their own: a statement buffers its writes and applies them to the transaction only when it succeeds, so a failed statement leaves the transaction usable. Postgres aborts the whole transaction instead; this is recorded as an intentional deviation.
- Types are strict: there is no implicit coercion, a parameter of the wrong Go type is an error, and the only adaptation is the one Postgres applies to literals, an integer literal next to a float. Everything else that Postgres coerces implicitly is a type error here, documented in `/docs/deviations.md`.

Alternatives considered: a hidden row id, as SQLite has, which would make primary keys optional but puts every lookup through a secondary structure; and Postgres's transaction abort on error, which is safer for scripts but makes `database/sql` callers handle an aborted state they rarely expect.

## 2026-11-03: Postgres is the oracle through a shared AST, with Postgres 18 in Docker and `COLLATE "C"`

Context: acceptance criterion 4 asks for differential testing against Postgres, and the two dialects differ in syntax and type names.

Decision: hegel generates a schema, rows, and statements as the `ast` package's syntax tree, which renders to both dialects; the harness in `/internal/pgdiff` runs each statement on both and compares results, exactly when `ORDER BY` determines the order and as multisets otherwise. Postgres 18 runs in a Docker container locally and as the shared workflow's service in CI, with a database created from `template0` with the `C` locale so that strings compare by byte order on both sides. The harness fails rather than skips when neither is available. `pgx` is the test dependency for the Postgres side. Every intentional difference is listed in `/docs/deviations.md` with its own test that asserts the difference on both databases, so the list cannot drift from the behavior.

Alternative considered: comparing against SQLite, which the requirements interview rejected because the dialect must not inherit SQLite's quirks.

## 2026-11-03: Automatic maintenance coordinated by an S3 lease that only saves work

The driver runs compaction and GC in the background. Maintenance is on by default and can be turned off. Clients coordinate through a lease object in S3, with a timeout. A client acquires the lease with `If-None-Match: *` and renews it or takes it over with `If-Match`.

The lease is only an efficiency optimization, and correctness never depends on it. Compaction is a conditional commit like any other, and GC needs no coordination, because a delete can safely happen twice. If two clients both believe they hold the lease, they duplicate work and nothing else. This is why the lease needs no fencing tokens and no assumption about clock sync. The simulator must show that correctness holds with the lease disabled and with leases that overlap.

Compaction also rebases on a conflict. When the commits that won against it only added segments, it reapplies its swap on top of them instead of redoing the merge. This means steady write traffic no longer starves compaction, which is a problem the lease cannot solve.

Alternatives considered:
- Manual `Compact` and `GC` only. This is easy to forget, and the database slowly degrades.
- Every client maintaining on its own, with no lease. This is safe but wastes requests and work when there are many clients.
- A lease that correctness depends on. This would need fencing, and it would make clock skew a safety issue.

## 2026-11-04: Custom object stores through an unexported interface on `Options.Store`

`Options.Store` accepts any value with four methods, `Get`, `Put`, `Delete`, and `List`, whose signatures use only exported types from the `maragudb` package (`GetOptions`, `PutOptions`) and builtin types. The interface itself is unexported, following the pattern of private interfaces on the consuming side. The exported sentinel errors `ErrObjectNotFound`, `ErrPreconditionFailed`, and `ErrNotModified` are part of the contract. An adapter inside the package translates to the engine's internal client and refuses answers that would silently break correctness, such as an empty ETag.

Alternatives considered:
- Exporting the internal client interface and its types. This would make the internal storage contract public and hard to change.
- An exported interface type. It adds surface without letting callers do anything more.

## 2026-11-04: The Antithesis harness runs against a real S3-compatible store and the in-process simulator

The docker-compose harness in `/antithesis` has two containers: versitygw as the S3-compatible store, and an instrumented workload. Test commands run the bank and list-append workloads through the engine and through SQL against versitygw over the network, so that Antithesis's own faults (partitions, resets, pauses, killed containers) reach the real AWS client and the commit protocol. Another test command runs the deterministic simulator against the in-process fake, with seeds from the Antithesis SDK, to cover crash points that the network cannot inject on demand.

Alternatives considered:
- The in-process fake only. Antithesis's network faults would not reach a store in the same process, so the run would only explore simulator seeds.
- MinIO. Its images are no longer published on Docker Hub.

## 2026-11-04: Orphans are stamped like other garbage, because compaction rebases

Context: the garbage collection decision of 2026-11-03 deleted an object that no manifest referenced as soon as the manifest sequence had moved past the object's base sequence, because the conditional write that would have referenced it could no longer succeed. The compaction rebase in the automatic maintenance decision breaks that reasoning: when its manifest write loses to commits that only added segments, compaction reapplies its swap on top of the newer manifest, so its output can still enter the manifest after the sequence has moved past its base.

Decision: an unreferenced object is no longer deleted at once. The first GC pass that sees it adds it to the garbage list with a stamp, and a later pass deletes it after the grace period, exactly like an object that left the segment list. A compaction gives up instead of rebasing once it is older than half the grace period, and a rebased manifest drops the stamp on its own output. The stamp is taken after the pass read a manifest newer than the one the compaction read, so it is later than the compaction's start, and the deletion comes a whole grace period after the stamp, so the compaction's last conditional write has at least half a grace period to land. Readers get the full grace period, because a reader that loses an object only fails one transaction, while a manifest that references a deleted object would be permanent. GC reapplies its pass on top of commits that landed meanwhile for the same reason compaction does: under steady writers a pass whose write always lost would delete the same expired objects on every run and never shrink the garbage list. Only the garbage it found unstamped gets its stamp; garbage that entered the list after the pass began waits for the next pass.

Alternatives considered:
- Writing the compacted segment again under the new base sequence on every rebase. This keeps the old orphan rule but costs a write of the whole database per rebase, several times under steady writers.
- Deleting orphans at once unless they come from a compaction, told apart by their key. This keeps crashed commits' orphans short-lived but adds a second rule for the same question; one rule is easier to show correct.

## 2026-11-04: A strict subset of Postgres instead of our own SQL flavor

This replaces "Own SQL flavor with Postgres semantics and Go-like types" from 2026-11-03.

maragudb accepts a subset of Postgres SQL, and the same SQL text runs on both systems. For every statement that maragudb accepts, the result must be identical to Postgres. When Postgres fails a statement, maragudb fails it too, and with the same error class wherever the error does not depend on evaluation order. The planner may differ, because only results are compared.

maragudb may be stricter than Postgres. It can reject statements that Postgres accepts, such as those that depend on implicit casts, as long as it rejects them with a clear error. A stricter rejection never changes what an accepted statement returns. Unsupported features are listed as unsupported. They are not semantic deviations.

Consequences:
- Type names are Postgres names: `bigint`, `double precision`, `boolean`, `text`, `bytea`, `timestamptz`. The Go-like names are dropped.
- `timestamptz` has microsecond precision, as in Postgres.
- The rest of the behavior must also match Postgres exactly. For example, a failed statement aborts the transaction until rollback, and `lower` and `upper` follow the C locale.
- The differential harness runs one SQL string on both sides, so there is no second renderer.
- External Postgres-syntax test corpora become usable as additional oracles.
- sqlc's PostgreSQL engine and existing Postgres knowledge carry over.

Some Postgres features will deliberately never be supported, because they are common antipatterns. The `money` type is one example. That list is decided later.

Alternatives considered:
- Keep our own flavor with documented deviations. Rejected because every deviation is something a reader must trust separately, and because the second renderer is extra code that itself needs verification.
- Postgres semantics with Go-like aliases for the type names. Rejected because it breaks the guarantee that the same text runs on both systems.

## 2026-11-04: A machine-checked map from requirements to tests, and a report for every run

The code is never reviewed, so trust rests on whether the verification is complete. The goal of this mechanism is that a reader can judge that completeness from the docs and the CI output alone.

Every claim gets a stable ID:
- `AC-` for acceptance criteria
- `INV-` for key invariants
- `DEV-` for documented differences from Postgres
- `MUT-` for deliberate bugs
- `STATE-` for hard states that the simulator must reach, such as a lost acknowledgement or a lease takeover

A test declares which IDs it verifies with `verify.Covers`, using string literals only, so that the declarations can be parsed statically. A meta-test fails when any of these hold:
- an ID has no verifying test
- a test names an unknown ID
- a deliberate bug is detected by nothing
- a declared state goes unreached in the fixed corpus
- the generated `docs/verification.md`, together with its table of which checkers catch which bug, is out of date

Each test run records facts such as runs, anomalies, deliberate bugs caught, differential statements, request budgets, and latencies. The run renders them as a report: `make report` locally, and in the GitHub Actions job summary in CI.

Every future change must keep this map complete. New requirements, deviations, deliberate bugs, and hard states get IDs and verifying tests in the same change.

Alternative considered: a hand-maintained list of what is tested. Rejected because nothing would catch it going stale, and without that check the list would not be a trust signal.

## 2026-11-04: Reject outcomes that depend on Postgres's plan, instead of emulating its planner

Context: under the strict-subset decision, every accepted statement must give the same result as Postgres. Some Postgres outcomes depend on its planner or on the physical order of rows: the order in which it evaluates WHERE conditions, whether an index scan skips rows that would fail, which rows a LIMIT reads, the order in which an UPDATE checks moved keys, and whether it evaluates a stable expression while planning.

Decision: maragudb does not reproduce Postgres's cost model or plan choice. It rejects such a statement with `ErrUnsupported`, statically where the shape decides it, such as a condition that can fail next to a primary key condition or under a LIMIT, and at run time where the data decides it, such as a condition that fails on a row that another condition rejects. A statement that fails in every order still fails with its own error. Parameters take their type from their context as in Postgres, and a Go value that drivers would convert in different ways is rejected too.

Alternatives considered:
- Emulating the planner's conjunct order by estimated cost and equivalence classes, as the first draft did. Rejected because it depends on cost constants and plan choices that change between Postgres versions and table statistics, so the emulation could silently diverge.
- Evaluating every row in a full scan to decide plan dependence exactly. Rejected because it would turn point lookups into full scans whenever a condition can fail.

## 2026-11-04: A primary-key condition fixes the plan to an index scan

Context: the previous decision rejects a condition that can fail when it sits next to a primary-key condition. That rejects natural OLTP statements such as `UPDATE acct SET b = b - $1 WHERE id = $2 AND b - $1 >= 0`. With an index scan, Postgres evaluates `b - $1` only on the row that the key selects. With a sequential scan, it can raise an overflow error on another row.

Decision: maragudb accepts these statements and behaves as Postgres does with the primary-key index. It evaluates the other conditions only on the rows that the key condition selects. The rules for WHERE order and LIMIT stay as they are. The differential harness runs Postgres with `enable_seqscan = off`, so Postgres uses the same plan and the comparison stays deterministic. Postgres in its default configuration can choose a sequential scan, for example on a small table, and then fail on a row that the key does not select. In that case maragudb is more lenient than Postgres. This is a documented `DEV-` difference with its own test.

Alternatives considered:
- Keep rejecting. Rejected because it refuses the statements that small OLTP apps write most.
- Evaluate the conditions on every row, as a sequential scan does. Rejected because every point update would cost O(table).

Also decided: `integer` stays a type for literals only. It is not a column type for now, so the column types stay a small set to test.

Markus left both choices to the lead.

## 2026-11-06: With several indexes, a row is skipped only when every scan Postgres could pick skips it

Context: the previous decision fixes the plan to a scan of the primary key index, because with `enable_seqscan = off` Postgres always scans it when a condition can use it. With secondary indexes, Postgres chooses by cost between the primary key, the other indexes, and bitmap combinations of them. Each scan has its own index conditions, so the rows on which Postgres evaluates the other conditions depend on the plan again. EXPLAIN on Postgres 18 shows that it can also scan a whole index without index conditions, when `ORDER BY` sorts by the index's first column or when the index has every column a query reads.

Decision: maragudb models the plans instead of choosing one. A condition that cannot fail and compares a column with a constant, an `IN` of constants, or a boolean column is an index condition of every scan of every index on that column, as EXPLAIN shows. An index is a candidate scan when it has such a condition, or when Postgres could scan it without one: another condition could use it, also inside an `OR`, `ORDER BY` sorts by its first column, or it covers every column of a `SELECT`. When a condition can fail, maragudb skips a row only when every candidate rejects it by one of its index conditions, and when some candidate has no index condition, it skips no row. On every other row, the runtime check of `UNS-ORDER-DEPENDENT-WHERE` applies as before. So a statement whose outcome differs between candidate plans is rejected at run time, and one whose outcome is the same in all of them gives Postgres's result. maragudb itself reads the union of the candidates' rows in that case, and otherwise the narrowest index. It visits the rows in key order whenever a row can fail, and in index order only when nothing can fail, such as for an `ORDER BY` with `LIMIT` that an index sorts.

The differential harness reruns a statement whose error class differs with the rows stored in key order, as before, and now also with the table's other indexes marked invalid in `pg_index` for the rerun, so that Postgres scans the table or its primary key. An invalid index is still updated and still enforces uniqueness.

Alternatives considered:
- Emulating Postgres's cost model to pick the same index. Rejected for the same reasons as in the decision of 2026-11-04: it depends on statistics and cost constants.
- Treating only the conditions common to every index as index conditions. Rejected because a row that one index's conditions reject can still be read by another index, so it is not sound.
- Rejecting every condition that can fail on a table with secondary indexes. Rejected because it refuses the point updates that the previous decision exists for.

## 2026-11-06: Foreign keys fire in Postgres's trigger order, and a DELETE whose actions could depend on that order is rejected

Context: Postgres implements foreign keys with triggers. At the end of each statement it fires, for each row in the order it visited them, the actions and checks of every foreign key that references the row's table, in the order of the triggers' names, and then the checks of the row's own foreign keys. An action such as `ON DELETE CASCADE` is a statement of its own, whose triggers fire when it ends. The trigger names contain object identifiers, so their order follows the creation order of the foreign keys only while the identifiers have the same number of digits. Experiments on Postgres 18 show that the outcome can depend on that order: with a child that references a parent by one column with `NO ACTION` and by another with `ON DELETE CASCADE`, deleting a parent that one child references by both fails or succeeds depending on which foreign key was created first, and with several parents it depends on the order of the rows too.

Decision: maragudb fires the same events in the same order: rows in key order, the referencing foreign keys in the order they were created, and the row's own foreign keys after them. An action's own events join the end of the statement's queue, because Postgres runs an action's query without firing its triggers when it ends (`ri_PerformCheck` passes `fire_triggers = false`), so they fire after every event queued before them. maragudb works the queue as a list, without recursion, so a cascade down a chain of any length does not grow the stack, and Postgres handles a chain of 100,000 rows too. It follows Postgres's details: NO ACTION first looks for another row with the old key, RESTRICT does not, SET DEFAULT checks the old key again as NO ACTION does, right after its update, a check is skipped for a row that a later write of the statement changed, and the check of an updated row runs also for unchanged keys when the transaction wrote the row before. Where the order could change the outcome, maragudb rejects the statement with `UNS-FOREIGN-KEY-ORDER`: a DELETE whose actions could write columns that a check of the same DELETE reads, or that could write one table through two foreign keys other than by deleting rows. Under those conditions every order gives the same outcome, and only the class of the error can differ between orders when several things fail, which is an error that depends on order, as for the rows of a statement. maragudb supports only `NO ACTION` and `RESTRICT` for `ON UPDATE`, only `MATCH SIMPLE`, and no foreign key on double precision columns, so that keys never change through an action and equal keys are equal by their encoding. The differential harness stores every table in key order, with the triggers off, for its rerun.

Also decided: `now()`, `transaction_timestamp()`, and `CURRENT_TIMESTAMP` are the start of the transaction on the engine's clock, which is the virtual clock in the simulator, and the other clocks are unsupported. A `DEFAULT` or `CHECK` whose constant part fails is rejected when the table is created, because Postgres folds it only when a statement uses it and fails then in an order maragudb does not reproduce. Constraint names share one namespace across tables, recorded at `k/<name>\x00<table>`, because Postgres chooses the names of new constraints, and of the indexes of `PRIMARY KEY` and `UNIQUE`, to differ from every constraint in the schema.

Alternatives considered:
- Final-state semantics: apply every action first and check every constraint at the end. Rejected because Postgres's checks see intermediate states, so the outcomes differ exactly in the cases where the order matters.
- Rejecting every foreign key action. Rejected because cascades and `SET NULL` through chains of tables and self-referencing trees are what applications need, and they are order-independent.
- `ON UPDATE CASCADE`, `SET NULL`, and `SET DEFAULT`. Deferred: a cascaded key update moves keys in other tables, with the order questions of `UNS-KEY-UPDATE-ORDER` in every table it reaches.

## 2026-11-06: A join is accepted only when no condition that can fail fails on any row or combination of rows that a plan could evaluate it on

Context: Postgres chooses the order of the joins of a query and the method of each, a nested loop, a hash join, or a merge join, by cost, and turning off sequential scans does not fix the choice. EXPLAIN on Postgres 18 shows that it pushes a condition on one table into the scan of that table, also on the right side of a LEFT JOIN, that a strict WHERE condition on that side turns the join into an inner join, and that equalities derive conditions on other tables. Which rows and pairs of rows a condition is evaluated on then depends on the plan: a hash join evaluates the other conditions of a join only on the pairs with equal keys, a nested loop with an index scan only on the rows the index finds, a merge join stops at the end of either side, and a join with an empty side may never scan the other side. The same statement fails with one method and succeeds with another, also when the condition fails on every row of one table, and `enable_nestloop`, `enable_hashjoin`, and `enable_mergejoin` each change the outcome.

Decision: maragudb keeps the rule of the single-table decisions, that it accepts a statement only when every plan Postgres could choose gives the same outcome, and checks it at run time. Before a join runs, it evaluates every condition of WHERE and ON that can fail on every row and combination of rows that a plan could evaluate it on: the rows that some scan of each table could read, by the candidate scans of the several-indexes decision, where an index on any column that a condition or ORDER BY reads is a scan Postgres could pick, with a row of NULLs for a table on the right side of a LEFT JOIN, and also each side of a comparison alone, which is the key of a hash join or a merge join or the value of an index scan, and the condition that Postgres takes out of an OR for the scan of one table. It rejects the statement with `UNS-PLAN-DEPENDENT-JOIN` when one of them fails, or when they are more than 100,000. When none fails, no plan fails, and the join gives the same rows in every plan, so maragudb runs it in any order: nested loops with index lookups on equalities, in an order that follows the conditions. The output and ORDER BY are evaluated on every row of the join in every plan, so a failure there fails in every plan; maragudb reports it when every row that fails fails with the same class, and rejects the statement otherwise, because which row Postgres reaches first depends on its plan. A LIMIT on a join needs an ORDER BY on every primary key column of every table, and nothing that can fail.

The harnesses keep only `enable_seqscan = off`. Turning off join methods would not make the comparison deterministic, because the rule does not depend on which method Postgres picks, and a harness that compared against one method could hide a statement that fails with another.

Alternatives considered:
- Rejecting every condition that can fail in a query on several tables. Rejected because it refuses the joins whose conditions add or multiply columns, which are common and almost never fail, as the sqllogictest corpus shows.
- Treating a condition that fails on a row that every plan reads as a certain failure, as the single-table rule does. Rejected because no row is read by every plan of a join: a join with an empty side may never scan the other one.
- Emulating Postgres's choice of join order and method. Rejected for the reasons of the decision of 2026-11-04: it depends on statistics and cost constants.
