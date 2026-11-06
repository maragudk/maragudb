# Diary: Section 2, part 2: DEFAULT, CHECK and FOREIGN KEY

Section 2 of `/docs/requirements.md` asks for the `DEFAULT` constraint, including expressions such as `now()`, the `CHECK` constraint, and an enforced `FOREIGN KEY`. Part 1 (`/docs/diary/2026-11-06-section-2-indexes.md`) added indexes and `UNIQUE`, which the foreign key checks build on.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Markus leaves the product calls to the lead. Part 1 of section 2 is pushed with CI green, so part 2 comes next.
**Inferred intent:** Finish section 2 with the same kind of automated evidence.

### What I did
I wrote the scope and verification and handed them to the builder `section2-constraints`. The full brief is in the spawn prompt. My scope calls:
- `now()` and its relatives (`CURRENT_TIMESTAMP`, `transaction_timestamp()`) are the transaction's start time, as in Postgres. `clock_timestamp()` and `statement_timestamp()` are the builder's call: faithful or UNS. Their values cannot be compared with Postgres, so the differential harness checks properties instead, for example that every use in one transaction gives the same value.
- Sequences, `serial` and identity columns stay unsupported (UNS). A sequence shared by many writers is a design question of its own, and it is noted as future work.
- FOREIGN KEY covers MATCH SIMPLE with NO ACTION, RESTRICT, CASCADE, SET NULL and SET DEFAULT, for ON DELETE and ON UPDATE. NO ACTION is checked at the end of the statement and RESTRICT at once, as in Postgres. MATCH FULL, DEFERRABLE and `DROP ... CASCADE` are the builder's call: faithful or UNS.
- New invariants with simulator checkers and mutants. The most important one: no committed state ever has a dangling reference, under concurrent writers and faults.

### Why
Small OLTP apps depend on these constraints for their data integrity, and a dangling reference across concurrent writers is exactly the kind of bug that tests must find, not users.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Database-wide conflict detection makes the concurrent foreign key case simple to get right. A parent delete and a child insert in two concurrent transactions always conflict, so one of them retries and sees the other. That is also why a mutant must prove that the checks actually run.

### What was tricky
The order of the checks. Postgres fires referential actions as triggers at the end of the statement, so which error comes first, and the state that a cascade sees, depend on trigger order.

### What warrants review
The foreign key checker in the simulator, and how the cascade order compares with Postgres.

### Future work
Sequences and identity columns, in a design that works with many writers.

## Step 2: DEFAULT, CHECK, FOREIGN KEY, their simulator workload, and the differential generator

**Author:** section2-constraints

### Prompt Context

**Verbatim prompt:** You are the builder `section2-constraints` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). Commit directly to main, with no PRs and no pushing; I push after I verify. Load the fabrik:go skill before you touch Go. [...] ## Goal Section 2, part 2: DEFAULT (including expressions such as now()), CHECK, and an always-enforced FOREIGN KEY, with the same evidence as part 1. When you are done, every Section 2 bullet in requirements.md is implemented and verified. ## Requirements 1. **DEFAULT.** [...] 2. **CHECK.** [...] 3. **FOREIGN KEY.** [...] 4. **Invariants and mutants.** [...] 5. **Differential testing.** [...] 6. **Docs.** [...] ## Verification (what I check when you report) [...] ## Hard rules [...] Continue the diary at /docs/diary/2026-11-06-section-2-constraints.md from Step 2, with `**Author:** section2-constraints`. Use the fabrik:diary skill. Self-review with the code-review skill at medium when done, and fix what it finds.
**Interpretation:** Build the three remaining constraints of section 2 as Postgres 18 has them, reject what cannot be made identical, and give them the evidence of part 1: invariants and mutants in the simulator, a request budget, both differential harnesses, and the docs.
**Inferred intent:** Finish section 2 so that a dangling reference or a violated check cannot slip through concurrency, faults, or a plan difference, and the tests prove it.

### What I did

I asked Postgres first, in a scratch database `scratch_fk` in the harness container, and wrote the code to its answers. The commits, in order:
- `DEFAULT` and `now()` (5792ce0). The parser reads `DEFAULT` as Postgres's restricted `b_expr`, so `DEFAULT 1 IS NULL` is a syntax error as there, and `/internal/sql/ast/render.go` parenthesizes in that mode. `ast.Default` is the keyword in `VALUES` and `SET`, and the binder rejects it anywhere else with 42601, as Postgres does. `now()`, `transaction_timestamp()`, and `CURRENT_TIMESTAMP` bind to a stable expression whose value is `engine.Tx.StartedAt`, so the simulator's virtual clock drives it. A default is stored as SQL text in the catalog. The parser also reads `CHECK` and `FOREIGN KEY` in this commit, which the executor still rejected.
- `CHECK` (47227ec), in `/internal/sql/exec/constraint.go`. Constraint names share a namespace across tables, so every name is also a key `k/<name>\x00<table>`, and the generated names of `PRIMARY KEY` and `UNIQUE` indexes now avoid constraint names too.
- `FOREIGN KEY` (2f900bd), in `/internal/sql/exec/foreignkey.go`: the catalog, the checks at `CREATE TABLE`, `DROP TABLE` and `DROP INDEX` dependencies, the event queue that emulates Postgres's referential triggers, the order analysis behind `UNS-FOREIGN-KEY-ORDER`, and `exec.CheckConstraints` for a whole store. `TestRequestBudgets_ForeignKeys` is `AC-5.7`.
- The simulator workload (5bb9b16), `/internal/sim/foreignkey.go`: parents, children that cascade, toys that cascade with their child, a tree of nodes with `SET NULL` and an owner with `RESTRICT`, all with checks, and a counter for real-time order. The four mutants and two states are in `/internal/sim/sim_test.go`.
- The generator (d92f18e, 47c14ee): a referenced table `p` in half of the cases, defaults, a `ts` column with a `now()` default, checks, and foreign keys with every supported action, to `p` and to `t` itself.
- The fixes of the self-review (see below).

The scope calls: `statement_timestamp()`, `clock_timestamp()`, `timeofday()`, `CURRENT_TIMESTAMP(p)`, `LOCALTIMESTAMP`, and `CURRENT_DATE` are UNS. Sequences, serial types, and identity columns are the new `UNS-SEQUENCES`. A default or check whose constant part fails is UNS, because Postgres folds it only when a statement uses it. `MATCH FULL`, `ON UPDATE CASCADE`, `SET NULL`, and `SET DEFAULT`, `NOT VALID`, `NOT ENFORCED`, `NO INHERIT`, `DEFERRABLE`, and foreign keys on double precision columns are UNS. A name for a `NOT NULL` constraint is UNS now too, because Postgres 18 gives NOT NULL constraints names that take part in the namespace. `DROP TABLE ... CASCADE` stays UNS. The decision is in `/docs/decisions.md`.

### Why

Postgres fires foreign keys as triggers, so its outcome depends on the order of the rows and of the triggers. maragudb fires the same events in the same order where that order is fixed, and rejects the statements where another order could give another outcome. That keeps the guarantee that every accepted statement matches Postgres without emulating object identifiers. The workload and mutants make the dangling reference that concurrency or a skipped check could cause visible.

### What worked

- The experiments settled most questions in minutes. CHECK constraints are evaluated in the order of their names (`c8`: `x` before `z`'s division by zero), after every NOT NULL and before the keys. Names follow the referenced column when there is one (`t_a_check`, `t_check`, `t_b_check1`), and are numbered past every constraint of every table in the schema (`y_a_check1` because another table has a constraint `y_a_check`, and `y2_pkey1` because of a CHECK named `y2_pkey`). RESTRICT fails with 23001, not 23503. `REFERENCES p (x)` from a bigint to a double precision is accepted. A `NOT VALID` foreign key in a table constraint is accepted, and `ON UPDATE SET NULL (b)` is Postgres's own 0A000.
- `TestSession_ForeignKeysKeepInvariants`, a hegel test in `/internal/sql/exec/foreignkey_test.go`, runs random writes on a parent, a child with each ON DELETE action, and a self-referencing table, in two sessions, one with indexes on the referencing columns. It checks `exec.CheckConstraints` and `exec.CheckIndexes` after every statement and that both sessions agree.
- The real-time counter in the foreign key workload turned four engine mutants from "cannot detect" into detected (CAS, freshness, ack, compacted-ahead). Without it, `MUT-SKIP-FRESHNESS` in the cell with maintainers flipped between detected and not with small workload changes.
- Numbers. `make pgdiff PGDIFF_CASES=10000` passed with 79,193 of 80,238 statements executed and 1,045 rejected, among them 3,276 foreign key violations, 1,846 check violations, and 47 RESTRICT violations on both, and 48 DELETEs rejected as `UNS-FOREIGN-KEY-ORDER`. The pgdiff ratchet rose from 1,783 to 2,343 of 2,376; it now counts the generated `CREATE TABLE` statements too, about 450 of them, so without them the rise is from 1,783 to about 1,890. The full sqllogictest corpus ran with zero mismatches and 2,779,188 of 5,939,714 statements executed, 3,116,763 rejected, the same as the baseline: the corpus has no DEFAULT, CHECK, or REFERENCES, so its ratchet cannot rise from this work. Every new mutant is detected at seed 1 to 9 in both foreign key cells; the kill matrix is in `/docs/verification.md`. The corpus reaches `STATE-FK-AFTER-CONFLICT` 10 times and `STATE-FK-CASCADE-TABLES` 6 times.

### What didn't work

- I misread my first scratch run. `CREATE TABLE d4 (..., b bigint DEFAULT 1/0)` succeeds in Postgres; the `ERROR: 22012` came from the INSERT after it. maragudb first failed the CREATE TABLE, and the pgdiff test showed `postgres: 0 rows affected, maragudb: error: division by zero`. It is UNS now.
- `INSERT INTO cd VALUES (8, DEFAULT, DEFAULT, DEFAULT, DEFAULT, 3), ...` into a table of nine columns failed on maragudb with `INSERT has more target columns than expressions`, and succeeded on Postgres, which fills the first columns when there is no column list. That was an old difference that the generator never wrote; it is fixed.
- My first foreign key tests put several actions on one child table, which the order analysis rightly rejects; Postgres's result there does depend on which foreign key it created first. In a scratch run, `e2` (CASCADE created before NO ACTION) deleted the parent, and `e1` failed with 23503.
- `DELETE FROM cp WHERE id = 1` gave different classes on the two sides: maragudb did not run Postgres's NO ACTION recheck after SET DEFAULT (`ri_restrict` in `ri_triggers.c`), went on to a `SET NULL` that failed a CHECK with 23514, and Postgres failed with 23503. Then the key-order rerun of the harness failed with `postgres: DELETE FROM cp: ERROR: update or delete on table "cp" violates foreign key constraint "ch_pid_fkey"`, because it deletes and inserts the rows again with the triggers on. The rerun now sorts every table with `session_replication_role = replica`.
- The hegel invariant test passed at once, but reached no RESTRICT in 1,000 cases: the referencing tables were always empty, because my seed inserts never matched after gofmt reindented the code my edit script was looking for. I added asserts on the outcomes that the test must reach.
- The first FK workload committed few writes, most child inserts failed on a missing parent, and `MUT-FK-OWN-WRITES` was undetected, because the multi-row node insert itself fails under that mutant. A transaction that inserts two fresh nodes and then runs the move fixed it.
- `go test ./internal/pgdiff ./internal/sql/exec ... && git add ... && git commit` committed d92f18e although pgdiff failed, because `tail` swallowed the exit status: `TestRunner_Run` expected 6 statements and counted 7 with the CREATE TABLE. 47c14ee fixes the counts and tests the runner's new paths.

### Self-review

I ran the code-review skill at medium over `48e97bc..HEAD`. It found no correctness bug, and ten findings about cost and duplication. I fixed eight:
- `kvstore.scanLatest` walked the whole pending map on every call, once per deleted parent row. It now keeps the pending keys sorted and merges them with the transaction's iterator as it goes, and a NO ACTION or RESTRICT check stops at the first referencing row. A self-referencing cascade of 4,000 rows takes 0.13 s with an index on the referencing column, linear; without one it stays quadratic, as Postgres's plan is.
- Defaults were parsed and bound once per row of an INSERT and once per row of a SET DEFAULT action. They are bound once now.
- A `DEFAULT` item went through `binder.assignable` a second time, and one past the target columns reported `DEFAULT is not allowed in this context` instead of too many expressions.
- `writeRow` and `updateKeys` each had their own NOT NULL and CHECK steps. `Session.checkNewRow` is the one place now.
- The three name choosers share `chooseNumbered`, and DELETE no longer copies each row key twice.

After the fixes, `make test`, `go test -race -shuffle on ./...`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass. A second `make pgdiff PGDIFF_CASES=10000` passed with 79,424 of 80,462 statements executed and 1,038 rejected. `make soak` passed: 23,735 simulator runs in 5 minutes without anomalies, and then the full corpus with zero mismatches and the same 2,779,188 executed statements.

I left the recursion of `Session.fire` and `Session.act`, which grows the Go stack with the depth of a cascade, because the depth-first order is Postgres's, and a chain long enough to exhaust the stack is far beyond the target workload. It is under future work.

### What I learned

- Postgres orders the triggers of a table by name, `RI_ConstraintTrigger_a_<oid>` before `RI_ConstraintTrigger_c_<oid>`, so for each row all actions come before all checks, and actions among themselves go by object identifier as text.
- The check of an updated child runs even when its key did not change, if the transaction wrote the row before (`RI_FKey_fk_upd_check_required`), and is skipped when a later write of the statement changed the row again. Both show in a self-referencing UPDATE that moves a parent: rows inserted in the same transaction fail with 23503 in key order and 23001 in the other.
- NOT NULL constraints have names in Postgres 18, `t_a_not_null`, and an explicit `CONSTRAINT t_a_not_null UNIQUE (b)` fails with 42710.

### What was tricky

- Finding a condition under which the trigger order cannot matter. Checks read columns and actions write them; when no check reads what an action writes, and no table is written through two foreign keys other than by deletes, every order gives the same rows and the same success. Re-existence checks of NO ACTION are only affected by writes that could add a key, and SET DEFAULT's own recheck is not affected by its own writes.
- Keeping the now() column of the generator out of every raw comparison: no `*`, no ORDER BY, no index, and a state check that shows `ts <= now()`.

### What warrants review

- `/internal/sql/exec/foreignkey.go`: `Session.fire`, `Session.act`, `Session.checkReference`, `Session.referencing`, and above all `Session.checkActionOrder`.
- `/internal/sql/exec/session.go` `kvstore.scanLatest` and the sorted pending keys.
- `/internal/pgdiff/run.go` `sides.inKeyOrder` with the replication role, and `/internal/pgdiff/gen.go` `gen.constraints`, `gen.foreignKey`, and `gen.references`.
- Validate with `go test ./internal/sql/exec -run 'ForeignKeys|Defaults'`, `go test ./internal/pgdiff -run 'Defaults|Checks|ForeignKeys|Unsupported|Runner'`, `make pgdiff PGDIFF_CASES=10000`, and `make replay SEED=1 WORKLOAD=foreign-key MUTATION=fk-own-writes MAINTAINERS=0`.

### Future work

- `ON UPDATE CASCADE`, `SET NULL`, and `SET DEFAULT`, which move keys in other tables with the order questions of `UNS-KEY-UPDATE-ORDER`, and `MATCH FULL`.
- A worklist instead of the recursion of cascades, for chains deeper than the Go stack.
- A NO ACTION or RESTRICT check without an index on the referencing columns still scans the referencing table once per deleted row.
- Sequences and identity columns, which the lead noted already.

## Step 3: Get the simulator package under the CI timeout again

**Author:** ci-sim-timeout

### Prompt Context

**Verbatim prompt:** You are the builder `ci-sim-timeout` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). Commit directly to main, with no PRs and no pushing; I push after I verify. Load the fabrik:go skill before you touch Go. ## Problem CI on main is red. Run https://github.com/maragudk/maragudb/actions/runs/42257099456 for commit 5440681 failed with `panic: test timed out after 10m0s` / `FAIL maragu.dev/maragudb/internal/sim 600.664s`. [...] ## Goal `go test -race -shuffle on ./...` finishes on the GitHub runner with clear headroom: internal/sim well under 10 minutes, aim for under 6. Keep the verification strength the same. [...] Acceptable approaches, roughly in order of preference: - run the independent corpus cells and mutant cells in parallel (`t.Parallel()`) [...] - make the slow cells cheaper without removing coverage [...] - split heavy kill-matrix work [...] Do not just cut seeds, skip under -race, or use testing.Short. [...] Measure before and after: `time go test -race -shuffle on ./internal/sim/` locally. Use `GOMAXPROCS=4` too, to mimic the 4-core runner. Report both. [...] Add a short step to /docs/diary/2026-11-06-section-2-constraints.md, as Step 3 with `**Author:** ci-sim-timeout`, using the fabrik:diary skill. [...]
**Interpretation:** Make `/internal/sim` fast enough under `-race` on a 4-core runner without dropping a seed, a mutant cell, or a reached state, and prefer parallelism over anything that changes what is run.
**Inferred intent:** Get CI green with headroom, so that the next section's workload does not tip it over again, without weakening the only quality gate the project has.

### What I did

`gh run view 42257099456 --log-failed` showed `TestSimulator` at 2m47s when the binary hit 10 minutes, so `TestMutants` had used about 7 of them, and 1,032 goroutines parked for 9 minutes in `database/sql.(*DB).connectionOpener`. The previous green run had the package at 302 s. Locally the package took 256.6 s at 109 % CPU: every simulation ran one after another on one core.

Every run is self-contained: its scheduler, fake store, RNGs, reach counter, and mutations all live in the `sim.Config` and the `run`, and nothing in `/internal/engine`, `/internal/sql/exec`, or `/internal/sqldriver` has mutable package state. The mutants are fields of the config, not global switches, so they can run in parallel as well. So in `/internal/sim/sim_test.go`:

- `runSim` takes a slot from a package-wide semaphore of `GOMAXPROCS` slots, and `runAll` runs a batch of configurations at once and returns the results in input order, so every sum, report, and failure message is in the same order as before.
- `TestSimulator`, `TestMutants`, and the expensive subtests call `t.Parallel()`. Each subtest builds its configurations first and then runs them through `runAll`.
- The corpus runs once, in `corpusResults`, a `sync.OnceValue`. "the SQL workloads exercise the same paths" used to rerun the 150 SQL configurations of the corpus; it now sums the shared results.
- The determinism test runs its four copies of each configuration at the same time, which also checks that concurrent runs do not leak into each other.
- In `TestMutants` the cells of a mutant run in parallel, each trying its 20 seeds in order until one detects the mutant, as before, and the results are checked in cell order afterwards. The mutants run in parallel too, so the comparison with `/internal/sim/testdata/kill-matrix.json` moved into a `t.Cleanup`, which runs after all parallel subtests.

In `/internal/sim/sim.go`, `client.loop` now closes its `database/sql` handle when it ends, through `client.closeSQL`, which the crash path uses as well. The handle's opener goroutine had kept the whole run, store contents included, alive for the rest of the test binary. `TestRun` checks this in a `testing/synctest` bubble per configuration, which fails when a goroutine started in it is still blocked at its end; it failed before the fix with `panic: deadlock: main bubble goroutine has exited but blocked goroutines remain`.

### Why

CPU parallelism was the cheapest lever by far: the work is embarrassingly parallel and was using one core of four. It changes nothing about which configurations run, so the corpus, the kill matrix, and the reached states stay exactly what they were.

### What worked

Timings on this machine, an M-series Mac with 10 cores:

| Command | Before | After |
| --- | --- | --- |
| `time go test -race -shuffle on ./internal/sim/` | 256.6 s (4:17 wall, 267 s user) | 55.5 s (56 s wall, 432 s user) |
| `time GOMAXPROCS=4 go test -race -shuffle on ./internal/sim/` | 256.2 s (4:16 wall, 263 s user) | 70.9 s (71 s wall, 269 s user) |
| `time GOMAXPROCS=4 go test -race -shuffle on ./...`, the 4-core runner's mix | sim 263.5 s, 4:37 wall | sim 96.2 s, 2:43 wall |

CI took about 2.3 times the local time of the package before (302 s on CI for the green run). At that ratio the package now takes about 3.5 to 4 minutes on the runner. The root package, at 164 s locally and 400 s on the failed CI run, is now the longest package of the suite.

`make verification` gives no diff in `/docs/verification.md` or `/internal/sim/testdata/kill-matrix.json`, and `make report` still shows 274 corpus runs, 15 of 15 mutants detected, and 17 of 17 states reached. The race detector found nothing with the runs in parallel.

### What didn't work

The CPU profile under `-race` on macOS was misleading at first: with the default `GOMAXPROCS`, 77 % of the samples were in `runtime.pthread_cond_signal`, which suggested the scheduler's baton passing. `GOMAXPROCS=1` took the same 17.6 s for one mutant, so it was not. A micro-benchmark of `Scheduler.Sleep` gave 2.1 µs per handoff under `-race`, far too little to matter. With `GOMAXPROCS=1` the profile showed what it really is: most time is in the race runtime itself (`__tsan::MemoryRangeSet`, `__tsan_read`, `__tsan::MetaMap::FreeRange`) spread over the engine, the SQL layer, and the JSON manifests. A run is 14 to 16 times slower under `-race` than without (85 ms against 5 ms for a list-append run through the engine). `GOGC=400` and `GOGC=off` changed nothing. There is no single hot spot in the foreign key workload; it costs what the other SQL workloads cost per run, about 125 ms under `-race`.

### What I learned

- `go test ./...` runs up to `GOMAXPROCS` packages at once, so on the 4-core runner the simulator, the root package, and the sqllogictest package each got about one core. Parallelism inside the package only pays off as far as the other packages leave cores free, which is why the full-suite measurement with `GOMAXPROCS=4` is the one that predicts CI.
- A `t.Cleanup` on the parent is the place for work that must come after parallel subtests, without renaming the subtests. The names matter here, because `/internal/verify` finds the `verify.Covers` calls by scanning the test source, and `/docs/verification.md` lists the test names.
- `testing/synctest` is a goroutine-leak detector for free: the bubble panics when its goroutines are still blocked after the function returns.

### What was tricky

Keeping every output deterministic while the runs finish in any order. Results go into slices by index, the checks run after `wg.Wait()` in the original order, and the kill matrix rows go into a slice by mutant index, so the golden file is byte-identical. I checked that the golden comparison still fires by changing one seed in the JSON file: `the kill matrix differs from internal/sim/testdata/kill-matrix.json`.

### What warrants review

- `/internal/sim/sim_test.go`: `runSim`, `runAll`, `corpusResults`, and the `t.Cleanup` of `TestMutants`.
- `/internal/sim/sim.go`: `client.closeSQL` and the deferred call in `client.loop`. Closing the handle reaches neither the store nor the clock, so it does not change any digest.
- Validate with `time GOMAXPROCS=4 go test -race -shuffle on -count 1 ./internal/sim/` and `make verification` followed by `git status`.

### Self-review

I ran the code-review skill at medium over the diff. It found no correctness bug, and eight findings. I fixed six:
- The leak test ran one seed in one maintenance mode. It now runs every workload through the engine and SQL in all four maintenance modes, with the default faults and with heavy ones (crashes, outages, ten clients), as 48 parallel subtests, and still fails without the fix.
- `corpusResults` kept the full result of every corpus run, histories included, for the rest of the binary. It now keeps a `corpusSummary`: the sums, the reached states, the verification batch, and only the results of failed runs.
- A panic in a run now happens in a goroutine that no test owns. `runSim` recovers it and panics again with the configuration's name, seed, configuration, and the original stack.
- The "different seeds" and soak subtests called `sim.Run` past the semaphore. They use `runSim` now. Replay keeps `sim.Run`, since it runs alone.
- The corpus log line measured the wait for the shared results. The duration is now measured inside `corpusResults`.
- The close-and-clear of the handle was copied from `client.afterCrash`. Both now call `client.closeSQL`.

I left two. A goroutine leak makes the synctest bubble panic, which ends the whole test binary instead of failing one test. A leak is a bug that should stop the suite, and there is no other leak check that is not timing-dependent. The other finding was that package state created lazily in the bubble, such as a channel behind a `sync.Once`, would be bound to it and break later tests in an order-dependent way. Nothing in the engine, the SQL layer, the driver, or the Antithesis SDK's `assert` package creates a channel or timer lazily, so I left it, but a new lazy cache in those packages would have to be created outside the bubble.

After the fixes, `make test`, `go test -race -shuffle on ./...`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass. `make fmt` reformats three lines of `/maragudb_test.go` from an earlier commit. They are not part of this change, so I left them out.

### Future work

- The root package took 400 s on the failed CI run and is now the longest package. It and `/internal/sqllogictest` (350 s on CI) are the next to watch as the workloads grow.
- The race detector makes a simulation 15 times slower, mostly per allocation. Fewer allocations in the manifest and block encoding would make every package faster under `-race`.
