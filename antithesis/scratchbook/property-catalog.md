---
sut_path: /
updated: 2026-11-03
---

# Property catalog

| Property | Assertion | Where | Status |
|----------|-----------|-------|--------|
| The workload reaches the store at startup (bootstrap) | `Reachable("the workload reaches the object store at startup")` | `/antithesis/workload/main.go` | implemented |
| No anomalies against a real store: bank total conserved, list-append strict serializable, acknowledged writes durable, failed writes invisible | `Always("the history against a real object store has no anomalies")` | `/internal/workload` | implemented |
| A commit against a real store sometimes has an unknown outcome | `Sometimes(...)` reach claim | `/internal/workload` | implemented |
| A commit against a real store sometimes conflicts | `Sometimes(...)` reach claim | `/internal/workload` | implemented |
| No anomalies in a simulated history | `Always("the simulated history has no anomalies")` | `/internal/sim` | implemented |
| An unknown outcome happens in a process that keeps running | `Sometimes(...)` | `/internal/sim` | implemented |
| A reader younger than the grace period never loses its objects | `Always(...)` | `/internal/engine/blocks.go` | implemented |
| Lost manifest writes are resolved, conflicts happen, GC finds orphans and expired objects | `Sometimes` / `Reachable` | `/internal/engine` | implemented |
| Autocommit retries after a conflict, and reports an unknown outcome instead of retrying | `Sometimes(...)` | `/internal/sql/exec/session.go` | implemented |

Each `parallel_driver_s3_*` run checks its own history at the end, in a database prefix of its own, so no `finally_` command is needed. A run whose store stays unreachable for its whole five-minute budget exits cleanly without a verdict.
