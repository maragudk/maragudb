# Diary: Add a benchmark suite

Issue #3 asks for a benchmark suite and leaves its scope open. maragudb's costs are S3 requests, bytes moved, and the latency of round trips to the store. Some of these can be measured exactly and some only against a real store. The read budgets in the requirements cover single queries. Nothing yet measures workloads, contention, cold starts, or maintenance under load. Nothing turns a regression in requests per operation into a failure either, except for the few queries that have a budget.

## Step 1: Scope the suite and set the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The verification work is in good shape. The benchmark suite is the open issue, and I scope it myself.
**Inferred intent:** Know what maragudb costs per operation, and notice when a change makes it cost more.

### What I did
I scoped the suite and gave it to the builder `benchmarks`. It has two parts, one deterministic and gated, and one measured and reported.

- **Workloads.** Each runs in the spirit of YCSB, scaled for small application databases. The builder picks the sizes, from about 10,000 to 100,000 rows.
  - Point reads.
  - Point writes, one autocommit statement each.
  - Range scans of 100 rows.
  - A mix of 95% reads and 5% writes.
  - Several writers that contend. Conflicts are whole-database, so the builder measures commits, retries, and conflicts per committed operation.
  - A cold open followed by the first query.
  - Writes while maintenance runs.
- **Deterministic part, gated in `make test`.**
  - The fake store measures GETs, PUTs, LISTs, DELETEs, and bytes per operation, plus cache hit rates.
  - Single-client workloads are deterministic as they are. Workloads with several clients run under the simulator's scheduler so that they are deterministic too, or they stay out of the gate.
  - A committed baseline holds the numbers. A worse number fails `make test`, and a better one is shown so that the baseline can be updated with a new `make bench-ratchet`.
  - The time this adds to `make test` is small.
- **Measured part, `make bench` against Tigris.** It is never run in CI. It measures:
  - p50 and p99 latency and throughput per workload;
  - cost in dollars per million operations, from Tigris's published prices per request and per GB, with the source and its date in the file.

  The results go into `/docs/benchmarks.md` with the date, the commit, the machine, and the region. The builder runs it once and commits the first results. Each run uses its own prefix in the bucket and deletes it at the end.
- **Out of scope.** Comparisons with other databases, TPC-C, and gates on latency.
- **Telemetry.** None.
- **Issue.** The builder references #3 in the commit message and closes it.

### Why
Requests per operation are this database's real cost model and they are exact, so they make a good gate. Latency and dollars are what a user asks about first, but they are noisy, so they are reported and not gated.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Nothing new yet.

### What was tricky
Nothing yet. A baseline that is exact turns every change that reads differently into a decision about the ratchet. The gate must say which number moved and by how much.

### What warrants review
The workloads that stay out of the gate and why, and the source of the price constants.

### Future work
None yet.

## Step 2: Build the workloads, the gate, and the Tigris run

**Author:** benchmarks

### Prompt Context

**Verbatim prompt:** You are the builder `benchmarks` for maragudb (/Users/maragubot/Developer/maragudb; work on main in that checkout; commit to main, do NOT push — the lead pushes). [...] Workloads (YCSB-spirit, small app DBs, sizes your call ~10k–100k rows): point reads; point writes (autocommit, one statement); range scans of 100 rows; 95/5 read/write mix; several contending writers (measure commits, retries, conflicts per committed op — conflicts are whole-database); cold open + first query; writes while maintenance runs. [...] Deterministic part, gated in `make test` [...] Measured part, `make bench` against Tigris (never in CI) [...]
**Interpretation:** Build the suite that Step 1 scoped: a package of workloads that runs either in virtual time over the fake store, where the numbers are exact and gated against a committed baseline, or in real time against Tigris, where latency, throughput, and dollars are measured and written into /docs/benchmarks.md.
**Inferred intent:** Know what each kind of operation costs, and make a change that costs more fail loudly, with the metric that moved.

### What I did
I added the package /internal/bench:

- /internal/bench/bench.go has `bench.Env`, where workloads run: a store, a prefix, and an optional `sched.Scheduler`. With the scheduler, every process's requests go through a `meter` that sleeps a fixed virtual latency per kind (`bench.VirtualLatency`, 15 ms for a conditional GET up to 50 ms for a conditional PUT), and the engine gets the virtual clock and a `Rand` seeded from the process name. The meter also counts requests, GET and PUT body bytes, manifest bytes, and failed conditional PUTs of the manifest, which are the conflicts. Each client is a "process": its own `sqldriver.Driver`, connector, engine, and caches, with one connection, opened the way /internal/sim/sqlclient.go opens its SQL clients.
- /internal/bench/workloads.go has `bench.Load` (100,000 rows of about 130 bytes in batches of 5,000, with maintenance after each), `bench.Run`, and eight workloads: `point-reads`, `point-writes`, `range-scans`, `read-write-mix`, `contending-writers` (4 writers, 50 commits each, alternately an autocommit UPDATE and a read-modify-write transaction retried on `engine.ErrConflict`), `cold-open`, `disk-cache-restart`, and `writes-with-maintenance`. Keys come from `rand.NewZipf` with skew 1.1, scrambled over the ids by a multiplier coprime to `bench.Rows`. The read workloads use a memory cache of 4 MiB so that they miss sometimes.
- /internal/bench/metrics.go turns a `bench.Result` into `bench.Metric`s per operation, compares a `bench.Baseline` with `bench.Compare`, and prices requests with Tigris's prices (source URL and date next to the constants: https://www.tigrisdata.com/pricing/, read as /pricing.md on 2026-11-07; Class A $0.005/1000, Class B $0.0005/1000, DELETE and egress free, storage $0.02/GiB-month).
- /internal/bench/report.go renders the Tigris run as Markdown and replaces the generated section of /docs/benchmarks.md.
- /internal/bench/workloads_test.go: `TestWorkloads` loads once, runs every workload in parallel over its own copy of the fake store, compares with /internal/bench/testdata/baseline.json, fails on a worse metric with the workload, metric, and both values, logs a better one, and records every metric as a new `verify.Bench` fact, which the CI report renders as a table. A second subtest reruns the two multi-process workloads and requires identical results. `TestTigris` runs only with `MARAGUDB_BENCH=1`, through `tigristest.NewClient`, whose prefix is unique and deleted at the end.
- `make bench-ratchet` rewrites the baseline, and `make bench` runs `TestTigris`. I added `AC-5.9` to /docs/requirements.md, a decisions entry, a README paragraph, /docs/benchmarks.md, and `s3.Counts.Add`.

I committed the suite as 41d1275, ran `make bench` once at that commit, and committed the results.

### Why
The fake store plus the scheduler makes every run byte-for-byte the same, also with four contending writers, so the gate can compare exact numbers and the multi-client workloads need not be left out. Running the same workload code in real time against Tigris makes the two parts comparable: with the same seed, the single-client workloads make the same requests on Tigris as in the gate (point reads: 1.29 GETs per read on Tigris, 1 conditional plus 0.292 in the gate).

### What worked
- The gate is cheap. The whole package takes about 1 s without the race detector and about 10 s with it alone; in `go test -race -shuffle on ./...` on 10 cores it took 34.5 s, against 80 s for /internal/sim, and the whole suite took 85 s.
- `go test -race -shuffle on -count 3 ./internal/bench` passed: the numbers do not move between runs.
- The first Tigris results, at 41d1275, Apple M4, AWS_REGION=auto, GET round trip 28.5 ms at the median:

  | Workload | p50 | p99 | Ops/s | $ per million ops |
  |---|--:|--:|--:|--:|
  | point-reads | 29.5 ms | 255.3 ms | 19.6 | 0.65 |
  | point-writes | 160.8 ms | 322.8 ms | 6.1 | 10.77 |
  | range-scans | 106.8 ms | 618.5 ms | 10.4 | 1.06 |
  | read-write-mix | 29.2 ms | 247.7 ms | 18.5 | 1.26 |
  | contending-writers | 179.5 ms | 3130.1 ms | 6.1 | 32.74 |
  | cold-open | 229.5 ms | 366.3 ms | 4.0 | 2.90 |
  | disk-cache-restart | 29.7 ms | 471.7 ms | 18.7 | 0.66 |
  | writes-with-maintenance | 138.3 ms | 478.6 ms | 6.3 | 12.26 |

### What didn't work
- The first version of `writes-with-maintenance` ran the maintainer in a process of its own. It never compacted: the baseline showed 403 segments at the end and `compaction bytes written per committed byte` was absent. A process checks for maintenance work on the manifest it already has, and a process that only runs maintenance never refreshes it. The determinism subtest caught it with `maintenance compacts while the writer writes`. The maintainer now runs in the writer's process, as the driver's loop does.
- The first maintainer loop slept on a context derived from the main task's context, not its own. `sched.Scheduler.Sleep` identifies the task by the context, so that would have parked the wrong task. I replaced it with `bench.Env.sleepUnless`, which takes a stop channel and uses the task's own context.
- `TestGroup` in real time failed under `-race` with `race detected during execution of test`, because the test counted finished tasks in a plain int from two goroutines. It uses an `atomic.Int64` now.
- `make test` after the first build failed with `docs/verification.md is stale; run make verification to regenerate it`, as expected for a new `AC-5.9`; `make verification` fixed it and left the kill matrix unchanged.

### What I learned
- Opening a process with `db.PingContext` costs one conditional GET more than an application's first statement does, because Ping begins a read-only transaction right after the open read the manifest. The workloads open through `sqldriver.Connector.DB` instead, so `cold-open` measures what an application pays: one unconditional GET of the manifest, then the statement's freshness check.
- Compaction is a manifest write with its own sequence number, so with whole-database conflicts a commit that overlaps a compaction loses and the autocommit statement retries. In `writes-with-maintenance` that is 25 failed manifest writes in 400 writes from a single writer.

### What was tricky
- Counting conflicts. Failed conditional PUTs include the lease, so the meter counts only those of the manifest key. They still include a compaction that loses and rebases, which the field's comment says.
- `go test` in package-list mode hides the logs of passing tests, so "a better number is shown" needed more than `t.Logf`: the `verify.Bench` fact puts every metric with its baseline and change into the report that `make report` and CI render.

### What warrants review
The self-review (code-review skill at medium) found ten things. I fixed:
- A panic in a non-main task in virtual time would have crashed the test binary; `bench.Env.run` now recovers `*sched.TaskPanic` and returns it as an error, with a test.
- The contending writers retried forever on conflict; they now fail after 100 conflicts in a row.
- A metric that disappeared counted as better, so a meter that stopped counting would have passed. Any metric on only one side of the comparison is now a failure; the docs say that a count that falls to zero needs a ratchet.
- `bench.ReplaceSection` searched for the end marker from the start of the document.
- The report table printed floats with `%v` (`1e+06`, 16 digits); it uses four decimals now.
- `counts.add` duplicated `s3.Counts`; I added `s3.Counts.Add`.
- The group poll was 1 ms of virtual time; it is 10 ms now.

I left two as they are:
- A better number passes, so the baseline can go stale and a later regression back up to the old value passes too. That is what the scope asked for ("a better number is shown"), mirroring `make ratchet`; failing on any difference would close the hole at the cost of a ratchet with every improvement. That is a decision for the lead.
- In virtual time, the maintainer sleeps its full 10 s before it sees that the writer is done, which inflates `Elapsed` of `writes-with-maintenance`. Virtual elapsed time is neither gated nor reported.

To validate: `go test -race -shuffle on -count 3 ./internal/bench`, then change `smallCacheBytes` in /internal/bench/workloads.go to `2 << 20` and watch the gate fail with the hit rates and GETs it names.

### Future work
- Each commit rewrites the manifest, and it dominates write bytes: 24.6 KB of the 24.8 KB written per point write without maintenance, and 15.9 KB per write with maintenance every 10 s. The manifest is about 2 KB after a compaction and grows by roughly 240 bytes per segment that maintenance has not merged yet, plus garbage entries. The manifest write is on the commit path, so it adds latency and every conflict rewrites it again (89 KB written per committed write in `contending-writers`).
- Compactions conflict with commits, because with whole-database conflicts a maintenance manifest write is a concurrent commit. An explicit transaction then gets `ErrConflict` from maintenance alone. A commit could rebase on a manifest that only maintenance changed.
- With four writers, each committed write made 1.98 conflicts on Tigris, its p99 was 3.1 s, and it cost $32.74 per million ops, three times a single writer's. That is the whole-database conflict granularity the decisions accept for now, measured.
- A point write takes about 160 ms on Tigris, against 28.5 ms for a GET: three or four requests in a row, the freshness check, a GET for the row in about half of the writes, the segment PUT, and the manifest PUT, which cannot start until the segment has landed, and PUTs are slower than GETs.
