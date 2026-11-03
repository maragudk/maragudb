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
