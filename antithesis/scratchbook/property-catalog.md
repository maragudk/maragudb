---
sut_path: /
updated: 2026-11-07
---

# Property catalog

| Property | Assertion | Where | Status |
|----------|-----------|-------|--------|
| The workload reaches the store at startup (bootstrap) | `Reachable("the workload reaches the object store at startup")` | `/antithesis/workload/main.go` | implemented |
| No anomalies against a real store: bank total conserved, list-append strict serializable, acknowledged writes durable, failed writes invisible | `Always("the history against a real object store has no anomalies")` | `/internal/workload` | implemented |
| A reader against a real store younger than the GC grace period never loses its snapshot | anomaly in the history check above | `/internal/workload` | implemented |
| A run against a real store reaches its verdict | `Sometimes(...)` reach claim | `/antithesis/workload/main.go` | implemented |
| A commit against a real store sometimes has an unknown outcome | `Sometimes(...)` reach claim | `/internal/workload` | implemented |
| A commit against a real store sometimes conflicts | `Sometimes(...)` reach claim | `/internal/workload` | implemented |
| No anomalies in a simulated history | `Always("the simulated history has no anomalies")` | `/internal/sim` | implemented |
| An unknown outcome happens in a process that keeps running | `Sometimes(...)` | `/internal/sim` | implemented |
| A reader younger than the grace period never loses its objects | `Always(...)` | `/internal/engine/blocks.go` | implemented |
| Lost manifest writes are resolved, conflicts happen, GC finds orphans and expired objects | `Sometimes` / `Reachable` | `/internal/engine` | implemented |
| The manifest never references a garbage batch that is gone | `Unreachable("the manifest references a missing garbage batch")` | `/internal/engine/compact.go` | implemented |
| GC keeps a live segment that an expired garbage batch lists, writes a garbage batch again for another chain, and drops expired chains | `Sometimes(...)` | `/internal/engine/compact.go` | implemented |
| Autocommit retries after a conflict, and reports an unknown outcome instead of retrying | `Sometimes(...)` | `/internal/sql/exec/session.go` | implemented |
| A call into the driver never panics; the driver recovers a panic as `ErrInternal` | `Unreachable("a call into the driver panics")` | `/internal/sqldriver/sqldriver.go` | implemented |
| A background maintenance step never panics; the step recovers a panic, logs it, and maintenance backs off and tries again | `Unreachable("a maintenance step panics")` | `/internal/engine/maintenance.go` | implemented |
| Damaged bytes from the store fail with `ErrCorrupt` and are never served (the sim command turns on `StoreDamage` in a quarter of its runs) | `Sometimes("a read of damaged bytes from the store fails with ErrCorrupt")` | `/internal/engine/blocks.go` | implemented |
| A cache never returns bytes that fail their check | `Unreachable("a cache returns bytes that fail their check")` | `/internal/engine/blocks.go` | implemented |
| A small disk cache evicts to stay within its bound, and refuses a file that a crash damaged (the sim command turns on `DiskFaults` in a quarter of its runs) | `Sometimes("a disk cache evicts to stay within its bound")`, `Sometimes("a disk cache refuses a damaged file")` | `/internal/sim` | implemented |

Each `parallel_driver_s3_*` run checks its own history at the end, in a database prefix of its own, so no `finally_` command is needed. A run whose store stays unreachable for its whole five-minute budget exits cleanly without a verdict; any other failure to set up or read back the database fails the command. The final read-back has a deadline of its own, so the history is checked even when the clients ran out of time.
