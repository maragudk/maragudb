# maragudb requirements

## What it is

maragudb is an embedded Go SQL database. Its only durable state is in S3-compatible object storage. `INV-S3-ONLY` Many processes can read and write the same database at the same time, with strict serializable ACID transactions. `INV-MULTI-WRITER` Caches in memory and on local disk are allowed (in the style of turbopuffer), but correctness never depends on them: losing every cache never loses or corrupts data. `INV-CACHE-LOSS`

Target workload: small OLTP app databases (web app backends, point lookups, small transactions, MBs to a few GBs). Commit latency of tens of milliseconds is acceptable.

The project is not reviewed manually. Automated tests are the only quality gate, so every requirement below must be checked by an automated test.

## Fixed product decisions

- **Form factor:** embedded Go library, module `maragu.dev/maragudb`, public on GitHub.
- **API:** a `database/sql` driver, registered explicitly with `maragudb.RegisterDriver(maragudb.Options{...})`. There is no `init` registration. `INV-REGISTER` Options use pointer fields with opinionated defaults, as in <https://github.com/maragudk/sqlite/blob/main/sqlite.go>. `INV-OPTIONS` The DSN identifies a bucket and a prefix, for example `s3://bucket/prefix`. `INV-DSN` Placeholders use the `$1` style. `INV-PLACEHOLDERS` A small native API exposes `Stats()`, with S3 request counts by kind and cache hits and misses, and also exposes cache options. `INV-STATS`
- **Concurrency:** multi-writer with optimistic concurrency. Commits are compare-and-swap writes through S3 conditional writes (`If-Match` / `If-None-Match`). No external lock service is used. `INV-CAS-COMMIT`
- **Isolation:** strict serializable. A read transaction sees every commit that was acknowledged before the transaction started. `INV-STRICT-SERIALIZABLE`
  - In section 1, conflict detection is at database granularity: any concurrent write commit is a conflict. `INV-DB-CONFLICTS` Finer granularity can come later.
  - A single statement outside an explicit transaction retries automatically on conflict. `INV-AUTOCOMMIT-RETRY`
  - An explicit transaction returns a typed, retryable serialization error, similar to Postgres SQLSTATE 40001. Callers detect it with `errors.Is` or `errors.As`. `INV-RETRYABLE-ERROR`
  - Read-only transactions never conflict. `INV-READ-ONLY`
- **Durability:** a commit is acknowledged only after its S3 write is confirmed. `INV-DURABLE-ACK`
- **SQL flavor:** a strict subset of Postgres 18. The same SQL text runs on both, and for every statement that maragudb accepts, the result is identical to Postgres: rows, values, column names, affected-row counts, and transaction state. When Postgres fails a statement, maragudb fails it too, with the same error class wherever the error does not depend on evaluation order or physical row order. The only exceptions are the differences that `docs/unsupported.md` lists with a `DEV-` ID, an example, and a test each. There is one: DEV-20, where Postgres scans the whole table and fails on a row that the primary key condition does not select, and maragudb does not fail. `INV-PG-SUBSET` maragudb is stricter than Postgres only through `ErrUnsupported`, SQLSTATE 0A000 (feature_not_supported), whose message says what is unsupported and, where possible, what to write instead. Every class of statement it rejects is listed in `docs/unsupported.md`. `INV-UNSUPPORTED`
- **Types:**

  | Type | Aliases | Meaning |
  |------|---------|---------|
  | `bigint` | `int8` | 64-bit integer. Overflow is an error. `INV-INT-OVERFLOW` |
  | `double precision` | `float8`, `float` | 64-bit float |
  | `boolean` | `bool` | boolean |
  | `text` | | UTF-8 text |
  | `bytea` | | binary data |
  | `timestamptz` | `timestamp with time zone` | instant with microsecond precision, from 0001-01-01 to the end of 294276, read and written in UTC |

  As in Postgres, an integer literal that fits in 32 bits has type `integer`, and arithmetic on it overflows at 32 bits; `integer` itself is not a column type. Postgres's `numeric` is the type of `SUM` of bigint and of `AVG` of integers, and of arithmetic on those results with each other and with integers and bigints, and of nothing else: it holds their results with the digits and scale that Postgres gives them, which reach Go as text. NULL is supported. `INV-TYPES`
- **Telemetry:** an optional `*slog.Logger` in Options, plus `Stats()`. `INV-LOGGER` OpenTelemetry is out of scope for now.
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
  - After any committed history, every index holds exactly the entries derived from its table's rows. `INV-INDEX-CONSISTENT`
  - No committed state ever has two rows with equal non-NULL values in a UNIQUE index. `INV-UNIQUE`
- The `UNIQUE`, `DEFAULT` (including expressions such as `now()`), and `CHECK` constraints.
  - No committed row ever violates a CHECK constraint of its table. `INV-CHECK`
- `FOREIGN KEY`, always enforced.
  - No committed state ever has a child row whose key has no NULL and no matching parent row (MATCH SIMPLE). `INV-FK`

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
   - errors `AC-1.1`
   - latency `AC-1.2`
   - lost acknowledgements, where the write succeeded but the response was lost `AC-1.3`
   - client crashes at any step, including mid-commit and mid-compaction or mid-GC `AC-1.4`

   History checkers find zero anomalies across a fixed seed corpus (run in `make test`) `AC-1.5` and a longer soak run (a separate make target). `AC-1.6` At least two checkers are required:
   - a bank workload, where the total balance is invariant `AC-1.7`
   - an Elle-style list-append workload, which checks for strict serializability `AC-1.8`

   Each failure can be reproduced from its seed alone. `AC-1.9`
2. **The checkers can fail.** Deliberately broken builds must be detected by criterion 1. Examples: commit without CAS, GC that deletes too early, a cache served without a freshness check. This proves that the tests are not vacuous. `AC-2`
3. **Durability and atomicity.** Every acknowledged commit stays visible after any crash. `AC-3.1` An unacknowledged commit is either completely visible or completely invisible, never partial. `AC-3.2`
4. **Differential testing against Postgres.** Hegel generates schemas, data, transactions, and statements as an AST, which is rendered once to Postgres SQL. Both systems run the same text, and the Postgres side runs in Docker. `AC-4.1` Comparison rules:
   - When `ORDER BY` determines the order completely, results must match exactly. Otherwise they must match as multisets. `AC-4.2`
   - Both systems must agree on whether a statement succeeds or fails, and on the error class when both fail. Postgres runs with `enable_seqscan = off`, so that the differences with a `DEV-` ID do not show. `AC-4.3`
   - Every class of statement that maragudb rejects with `ErrUnsupported` is listed in `docs/unsupported.md` with a stable `UNS-` ID and a test that shows Postgres accepting it and maragudb rejecting it. Any remaining difference from Postgres is listed there too, with a `DEV-` ID and its own test. `AC-4.4`
   - A statement that maragudb rejects with `ErrUnsupported` is not run on Postgres, and the harness counts it as rejected. With a fixed seed, at least 70% of the generated statements run on both systems, and the number that do never drops below a committed baseline. `AC-4.5`
   - The SQLite sqllogictest corpus runs live on both systems under the same rules, with Postgres as the only oracle. A fixed set of its files runs in `make test` and the full corpus in `make soak`. For each file, the number of statements that run on both systems never drops below a committed baseline. `AC-4.6`
5. **S3 request budgets.** These are deterministic and are checked against the fake S3 via `Stats()`:
   - A commit uses O(1) PUTs, independent of database size. `AC-5.1`
   - A transaction with a warm cache uses one conditional GET (or HEAD) for the freshness check, plus nothing else for cached data. `AC-5.2`
   - A point lookup with a cold cache uses O(log n) GETs. `AC-5.3`
   - A point lookup through a secondary index with a cold cache uses O(log n) GETs. `AC-5.5`
   - A commit that changes indexed columns still uses O(1) PUTs, independent of database size and of the number of indexes. `AC-5.6`
   - An insert of a row with a foreign key with a warm cache uses no request beyond those of any other commit, and with a cold cache O(log n) GETs for the lookup of the referenced row. `AC-5.7`
   - A join from one row of a table to the rows that reference it, through an index on the referencing columns, with a cold cache uses O(log n) GETs for each row it reads. `AC-5.8`

   Latency against Tigris is measured and reported, but it does not gate. `AC-5.4`
6. **Bounded storage.** After compaction, the object count grows with data size, not with commit count. `AC-6.1` GC never deletes an object that a snapshot within the configured grace period still references. `AC-6.2` A reader older than the grace period gets a clear error, never wrong data. `AC-6.3`
7. **Real S3-compatible storage.** An integration suite passes against Tigris. `AC-7.1` If the Tigris credentials are not in the environment or in a gitignored `.env` file, the suite fails and does not skip. The failure message names the missing variables. `AC-7.2`
8. **Antithesis-ready.** Antithesis Go SDK assertions (Always, Sometimes, and Reachable) mark the key invariants. `AC-8.1` A docker-compose harness runs the simulator workloads as an Antithesis workload. `AC-8.2` Go cross-compiles to amd64. `AC-8.3`

## Out of scope for now

- A network server or wire protocol
- OpenTelemetry
- Recursive CTEs, window functions, and views
- `NUMERIC` columns and literals, and JSON types
- `ALTER TABLE`, unless a section needs it
- Row-level or range-level conflict detection

## Retired IDs

These IDs named requirements that no longer hold. They are not reused.

- INV-STRICT-TYPES: the own SQL flavor without implicit coercion. Replaced by INV-PG-SUBSET and INV-UNSUPPORTED, because maragudb now applies Postgres's implicit and assignment casts.
- DEV-1 to DEV-19: the deviations from Postgres in the former `docs/deviations.md`. Each one is either eliminated or an explicit rejection, as `docs/unsupported.md` lists.
