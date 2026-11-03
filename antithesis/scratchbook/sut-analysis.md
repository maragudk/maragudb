---
sut_path: /
commit: (see git log for antithesis/)
updated: 2026-11-03
external_references:
  - docs/requirements.md
  - docs/decisions.md
---

# SUT analysis

maragudb is an embedded Go SQL database. It is a library, not a service: the only process is the application that embeds it, and the only durable state is in an S3-compatible object store. Many processes may share one database. Commits are compare-and-swap writes of one manifest object with `If-Match`, so correctness rests on the store's conditional writes and on the engine's handling of answers that never arrive.

What can go wrong, and what the harness looks for:

- A commit whose answer is lost (a timeout, a reset connection) must be resolved by reading the manifest back, and reported as `ErrCommitUnknown` only when that read also fails. Reporting a definite outcome that is wrong breaks durability or atomicity.
- Two processes must never both win the same manifest version (lost update, write skew).
- A reader must see every commit acknowledged before it started (strict serializability).
- Garbage collection must never delete an object a reader within the grace period still needs.
- A process that dies mid-commit leaves orphaned segments, which must not become visible.

The library already carries Antithesis SDK assertions in `/internal/engine` (`Sometimes` for lost-write resolution, conflicts, and GC paths; `Always` for the grace period), in `/internal/sql/exec` (autocommit retries and unknown outcomes), and in `/internal/sim` (`Always` no anomalies in a simulated history).

The language is Go, built with `antithesis-go-toolexec` for coverage and assertion cataloging. cgo is on, as the SDK requires.
