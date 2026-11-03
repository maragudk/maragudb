# maragudb requirements

## What it is

maragudb is an embedded Go SQL database. Its only durable state is in S3-compatible object storage. Many processes can read and write the same database at the same time, with strict serializable ACID transactions. Caches in memory and on local disk are allowed (in the style of turbopuffer), but correctness never depends on them: losing every cache never loses or corrupts data.

Target workload: small OLTP app databases (web app backends, point lookups, small transactions, MBs to a few GBs). Commit latency of tens of milliseconds is acceptable.

The project is not reviewed manually. Automated tests are the only quality gate, so every requirement below must be checked by an automated test.

## Fixed product decisions

- **Form factor:** embedded Go library, module `maragu.dev/maragudb`, public on GitHub.
- **API:** a `database/sql` driver, registered explicitly with `maragudb.RegisterDriver(maragudb.Options{...})`. There is no `init` registration. Options use pointer fields with opinionated defaults, as in <https://github.com/maragudk/sqlite/blob/main/sqlite.go>. The DSN identifies a bucket and a prefix, for example `s3://bucket/prefix`. Placeholders use the `$1` style. A small native API exposes `Stats()`, with S3 request counts by kind and cache hits and misses, and also exposes cache options.
- **Concurrency:** multi-writer with optimistic concurrency. Commits are compare-and-swap writes through S3 conditional writes (`If-Match` / `If-None-Match`). No external lock service is used.
- **Isolation:** strict serializable. A read transaction sees every commit that was acknowledged before the transaction started.
  - In section 1, conflict detection is at database granularity: any concurrent write commit is a conflict. Finer granularity can come later.
  - A single statement outside an explicit transaction retries automatically on conflict.
  - An explicit transaction returns a typed, retryable serialization error, similar to Postgres SQLSTATE 40001. Callers detect it with `errors.Is` or `errors.As`.
  - Read-only transactions never conflict.
- **Durability:** a commit is acknowledged only after its S3 write is confirmed.
- **SQL flavor:** our own. It follows Postgres semantics where it is sensible and has none of SQLite's quirks: no type affinity, no implicit coercion, and values are type-checked.
- **Types:**

  | Type | Meaning |
  |------|---------|
  | `int` | int64. Overflow is an error. |
  | `float` | float64 |
  | `bool` | boolean |
  | `string` | UTF-8 text |
  | `bytes` | binary data |
  | `time` | UTC instant with nanosecond precision |

  NULL is supported.
- **Telemetry:** an optional `*slog.Logger` in Options, plus `Stats()`. OpenTelemetry is out of scope for now.
- **Repo bootstrap:** copy LICENSE, `.editorconfig`, `.github`, `.gitignore`, AGENTS.md, CLAUDE.md, and the Makefile from <https://github.com/maragudk/template>, then adapt them. Do not copy `template.go`.
- **Workflow:** no PRs. Commit directly to `main`.

## Sections

The project is built in three sections. A section is done only when all of its acceptance criteria pass.

### Section 1: Spine

- The storage engine on S3: an immutable data object format, a manifest or log committed through conditional writes, and snapshots.
- Compaction, and safe garbage collection of unreferenced objects.
- Memory and disk caches.
- SQL:
  - `CREATE TABLE` and `DROP TABLE`
  - `INSERT`, `UPDATE`, and `DELETE`
  - `SELECT` with `WHERE`, `ORDER BY`, and `LIMIT`/`OFFSET`
  - `BEGIN`, `COMMIT`, and `ROLLBACK`
  - `PRIMARY KEY` and `NOT NULL`
- All types.
- The `database/sql` driver.
- The test infrastructure: a fake S3, the deterministic simulator, the checkers, the Postgres differential harness, the Tigris suite, and the Antithesis harness.

### Section 2: Integrity

- `CREATE INDEX`, including `UNIQUE`. The planner uses indexes for lookups and range scans.
- The `UNIQUE`, `DEFAULT` (including expressions such as `now()`), and `CHECK` constraints.
- `FOREIGN KEY`, always enforced.

### Section 3: Queries

- `INNER JOIN` and `LEFT JOIN`
- `GROUP BY` and `HAVING`
- `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX`
- `DISTINCT`
- `IN` and `EXISTS` subqueries, and scalar subqueries
- Non-recursive `WITH` (CTEs)

## Acceptance criteria

These apply to every section, for the SQL surface that exists at that point.

1. **Serializability under faults.** A seeded deterministic simulation runs many concurrent clients against an in-process fake S3. The fake S3 injects these faults:
   - errors
   - latency
   - lost acknowledgements, where the write succeeded but the response was lost
   - client crashes at any step, including mid-commit and mid-compaction or mid-GC

   History checkers find zero anomalies across a fixed seed corpus (run in `make test`) and a longer soak run (a separate make target). At least two checkers are required:
   - a bank workload, where the total balance is invariant
   - an Elle-style list-append workload, which checks for strict serializability

   Each failure can be reproduced from its seed alone.
2. **The checkers can fail.** Deliberately broken builds must be detected by criterion 1. Examples: commit without CAS, GC that deletes too early, a cache served without a freshness check. This proves that the tests are not vacuous.
3. **Durability and atomicity.** Every acknowledged commit stays visible after any crash. An unacknowledged commit is either completely visible or completely invisible, never partial.
4. **Differential testing against Postgres.** Hegel generates schemas, data, and queries as an AST. The AST is rendered both to maragudb SQL and to Postgres SQL, and the Postgres side runs in Docker. Comparison rules:
   - When `ORDER BY` determines the order completely, results must match exactly. Otherwise they must match as multisets.
   - Both systems must agree on whether a statement succeeds or fails.
   - Intentional deviations are listed in `docs/deviations.md`, and each one has its own test.
5. **S3 request budgets.** These are deterministic and are checked against the fake S3 via `Stats()`:
   - A commit uses O(1) PUTs, independent of database size.
   - A transaction with a warm cache uses one conditional GET (or HEAD) for the freshness check, plus nothing else for cached data.
   - A point lookup with a cold cache uses O(log n) GETs.

   Latency against Tigris is measured and reported, but it does not gate.
6. **Bounded storage.** After compaction, the object count grows with data size, not with commit count. GC never deletes an object that a snapshot within the configured grace period still references. A reader older than the grace period gets a clear error, never wrong data.
7. **Real S3-compatible storage.** An integration suite passes against Tigris when Tigris credentials are in the environment, and it skips otherwise.
8. **Antithesis-ready.** Antithesis Go SDK assertions (Always, Sometimes, and Reachable) mark the key invariants. A docker-compose harness runs the simulator workloads as an Antithesis workload. Go cross-compiles to amd64.

## Out of scope for now

- A network server or wire protocol
- OpenTelemetry
- Recursive CTEs, window functions, and views
- `NUMERIC` and JSON types
- `ALTER TABLE`, unless a section needs it
- Row-level or range-level conflict detection
