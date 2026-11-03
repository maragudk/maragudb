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
