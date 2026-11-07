# Diary: Recover panics in background goroutines

Since the driver hardening work, a panic inside a driver call comes back as `ErrInternal`. A panic in a goroutine that maragudb starts itself is different. The background maintenance loop is one example: if it panics, the panic ends the host process. An embedded database must not take its host application down for a bug in maintenance work that the application never asked for.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The driver-hardening builder listed this as a follow-up. It is the last way left for a maragudb bug to end the process.
**Inferred intent:** The host application survives any maragudb bug, and the database stays correct.

### What I did
I wrote the requirement and gave it to the builder `background-panics`.
- **Every goroutine.** The builder lists every goroutine that maragudb starts: maintenance, compaction, GC, cache writes, lease renewal, and any others. Each one recovers a panic.
- **What a recovery does.**
  - It logs the panic value and the stack at error level to `Options.Logger`.
  - It marks the panic with an Antithesis `Unreachable` assertion.
  - It holds no lock afterwards.
- **What comes next.** The goroutine backs off and tries again, with an exponential backoff that has a cap, so that a panic that repeats cannot spin. If a goroutine cannot simply try again, the builder decides what it does instead, writes the choice in the diary, and tests it.
- **Correctness.**
  - Reads and writes keep working while maintenance is stopped or backing off.
  - A panic in the middle of compaction or GC leaves the store correct. Nothing is lost and nothing becomes visible that should not be.
  - Maintenance makes progress again once the panic stops.
- **Verification.**
  - A mutation that panics in the middle of compaction or GC runs through the simulator, and every checker still passes. A second mutant, which takes the recovery away, must be detected.
  - The simulator still fails a run on any panic. Recovering is for production; in tests a panic is a bug.
  - A test through the public driver shows that the process survives and the database keeps answering.
- **Telemetry.** Structured error logs through the existing `Options.Logger` are enough. No metrics are needed.

### Why
A panic is a bug, but whether the process lives through it decides how much damage that bug does. Correctness comes from atomic manifest swaps. Maintenance only has to be safe to stop at any point, which the crash tests already require.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Nothing new yet.

### What was tricky
Nothing yet. A recovered panic may leave the goroutine's own state, such as an in-memory plan for a merge, half-built. A retry must start from a clean state.

### What warrants review
The list of goroutines, the state each one starts from when it tries again, and the mutant that takes the recovery away.

### Future work
None yet.

## Step 2: Recover panics in maintenance, back off, and prove it in the simulator

**Author:** background-panics

### Prompt Context

**Verbatim prompt:** You are the builder `background-panics` for maragudb (/Users/maragubot/Developer/maragudb; work on main in that checkout; commit to main, do NOT push — the lead pushes). [...] Inventory every goroutine maragudb starts [...] Each recovers a panic [...] Then back off and retry with capped exponential backoff so a repeating panic can't spin. [...] A mutation that panics mid-compaction/GC (at a point chosen by the seed) runs through the simulator; all checkers pass and maintenance resumes. [...] A mutant that removes the recovery must be detected in the kill matrix [...]
**Interpretation:** Make every goroutine of maragudb survive a panic, with logging, an Antithesis assertion, capped exponential backoff, and a clean retry; prove it in the simulator, the kill matrix, and through the public driver.
**Inferred intent:** No bug in maintenance can take the host application down, and the store stays correct when maintenance stops anywhere.

### What I did

**The goroutine inventory.** `grep -rn 'go func\|go [a-z]' --include='*.go'` outside tests finds exactly one goroutine that the library starts:

| Goroutine | Where | Started by | On a panic |
|---|---|---|---|
| Background maintenance loop, one per database | `/internal/sqldriver/sqldriver.go`, `Connector.maintain` | `Connector.DB`, when maintenance is on | `engine.Maintainer.Step` recovers it: logs at error level, `Unreachable("a maintenance step panics")`, forgets the lease, backs off (10 s doubled per panic in a row, capped at 10 min), returns `engine.ErrPanicked`; the loop keeps going |

Everything else the requirement named runs in someone else's goroutine. Compaction and GC run inside `Maintainer.Step`, in the loop's goroutine. Lease renewal runs inline in the step's progress hook (`Maintainer.progress`), not in a goroutine of its own. The disk cache in `/internal/cache` writes synchronously in the caller's goroutine. The engine's default `Sleep` and the loop use `time.After`, which starts no goroutine. `/maragudb.go` and `/store.go` start none. The other `go` statements are in test tooling: `/internal/sched` (the simulator's scheduler), `/internal/sim`, `/internal/workload` and `/antithesis/workload` (the Antithesis workload's clients), and `/internal/sqllogictest`. They are out of scope, because in tests a panic is a bug and must fail. database/sql's own goroutines and the AWS SDK's are not ours.

**Where the recovery lives.** I put it in `engine.Maintainer.Step` (`/internal/engine/maintenance.go`), not in the driver's goroutine, because the simulator calls `Step` from its clients and never runs the driver's loop. A recovery in the loop would be invisible to every checker. `Step` defers a recovery that calls `Maintainer.recovered`, which takes the stack before unwinding, logs `panic`, `stack`, `holder`, `panics`, and `backoff` at the error level through the engine's logger (the driver passes `Options.Logger` down), calls `assert.Unreachable` (except for the tests' injected panic), calls the new observer `MaintenanceOptions.OnPanic`, and returns an error that wraps `engine.ErrPanicked`. The driver's loop skips its own warning for that error, so a panic is logged once.

**Clean state and backoff.** The merges and GC state of a step live in the step's own calls, and every engine lock is released by a deferred unlock (I grepped every `Lock()` in `/internal/engine` and `/internal/cache`), so nothing is half-built and nothing stays locked. The maintainer forgets the lease instead of writing a release from a state that just failed; the lease names the holder, so it re-acquires at once, and others wait at most a lease duration. After a panic, `Step` returns `StepResult{BackingOff: true}` with no request until `retryAt`. A step that gets to work and finishes without an error resets the count. The backoff is a gate, not a sleep, so the loop's only wait is still its interval select on the context, and `Close` stays prompt. New options `PanicBackoff` and `MaxPanicBackoff` (defaults 10 s and 10 min) are internal only; `maragudb.Options` does not expose them.

**Simulator and kill matrix.** New engine mutations: `MaintenancePanics` panics with `engine.ErrInjectedPanic` in one in three steps that get to work, at one of the first eight calls of the progress hook, picked by the database's seeded random source. That covers before block reads, before the merged segment's write, before the manifest CAS, and between GC's reads, writes, and deletes. `MaintenanceUnrecovered` removes the deferred recovery. In `/internal/sim/sim.go`, each client's maintainer gets an `OnPanic` (`client.maintenancePanicked`) that counts the injected panic and hands any other to `run.recovered`, so the existing `repanic` after the step fails the run with the panic checker as before. New stats are `MaintenancePanics`, `MaintenanceBackoffs`, and `MaintenanceResumed`. The sim uses a 400 ms backoff capped at 1.6 s, so that backoffs and resumptions happen within a run's few seconds of virtual time. `TestRun_maintenancePanics` runs 18 configurations, every workload through the engine and SQL in all three maintenance modes, half with two maintainers, all with crashes and faults. All 18 pass every checker and recover panics, 17 back off, and in 12 a maintainer changes the database again after a panic. The new mutant `MUT-MAINTENANCE-UNRECOVERED` runs both mutations with `MaintenanceLease` (a new `mutant.maintenance` field, also used by `make replay MUTATION=maintenance-unrecovered`). The panic checker detects it in 12 of 12 cells at seed 1. The kill matrix's rule that cells without maintainers must be detected by a history or invariant checker now also accepts the panic checker for a mutant that names it in `checkers`, because that mutant's bug is exactly an escaping panic.

**Tests.** `/internal/engine/maintenance_test.go` has a new `TestMaintainer_Panics`:
- A store panic on the merged segment's write: the step returns `ErrPanicked`, the log has value and stack, the lease is forgotten, reads and a write work, and backing-off steps make no requests. The backoff goes 10, 20, 40, 40 s with a 40 s cap and lasts to the millisecond. Once the panic stops, the step compacts with every value kept, and a clean step resets the backoff to 10 s.
- A panic on the manifest CAS after the segment write, which leaves exactly one orphan, and a panic on GC's second delete. Every value stays readable, and later steps leave the store with only the manifest, the lease, and the live segment.
- `MaintenancePanics` panics in both compaction and GC, and maintenance still makes progress.
- `MaintenanceUnrecovered` lets the panic unwind.
- 70 panics in a row with the defaults stay at the 10 min cap.

In `/maragudb_test.go`, a synctest `TestMaintenance` subtest goes through the public driver. A store panic mid-compaction leaves the process alive and the database answering. The log has one error record with the stack and no extra warning. Maintenance does nothing for the backoff and compacts after it. `db.Close()` during a backoff takes zero fake time, and the synctest bubble shows the goroutine exited. `TestClient_maintenancePanicked` in `/internal/sim/sim_internal_test.go` checks the sim's filter.

**Docs.** `INV-MAINTENANCE-PANIC` is in `/docs/requirements.md`, with a decision in `/docs/decisions.md`. I added the `Unreachable` to `/antithesis/scratchbook/property-catalog.md`. `/maragudb.go`'s package doc and `Options.Maintenance` doc describe the behaviour, and a writer sub-agent added a passage to `/README.md`, which I checked against the code. The `make replay` mutation list is in `/Makefile`. I regenerated `/docs/verification.md` and the kill matrix.

### Why
The maintenance loop was the last way for a maragudb bug to end the host process. Recovering at the step is what makes the simulator, the kill matrix, and the driver all exercise the same code.

### What worked
Panicking from the fake store's `Decide` hook is safe, because the fake holds no lock there. That gave precise panic points in the engine tests and through the public driver without new hooks. synctest made the public test with a 10 s backoff instant, and it checks for leftover goroutines on its own.

### What didn't work
- The first version of the backoff loop in the engine test assumed the wrong clock arithmetic. It failed with `maintenance_test.go:372: Expected nil error, but got "engine: a maintenance step panicked: panic: the store panics"`, because a step I expected to back off had already passed its backoff. I rewrote it as "sleep backoff minus 1 ms, expect backing off, sleep 1 ms, expect the panic".
- `is.Equal(t, float64(1), records[0]["panics"])` failed vet: `type any of records[0]["panics"] does not match inferred type float64 for T`. A `number` helper fixed it.
- With a 20 ms sim backoff and one panic in two steps, almost no step backed off and no maintainer resumed (`8 injected maintenance panics recovered (0 steps backed off, 1 maintainers resumed)`), because client steps are hundreds of milliseconds of virtual time apart. A 400 ms base, one panic in three steps, and 40 transactions per client gave meaningful numbers.
- My first backoff formula `PanicBackoff << shift` with `shift < 32` overflowed: 10 s is 1e10 ns, and shifted by 31 it wraps negative. The new 70-panic test caught it with the old code (`maintenance_test.go:433: panic 31`). The fix doubles in a loop that stops at the cap.

### What I learned
The kill matrix demands that cells without maintainers are detected by a history or invariant checker. A mutant whose bug is the panic itself needs the panic checker allowed there. I made that explicit through the mutant's `checkers` rather than marking those cells not applicable, because they do run the code.

### What was tricky
- Deciding where the recovery lives: only the step is shared by the driver and the simulator.
- Keeping the panic checker meaningful while recovering in production. `OnPanic` is an observer, and the sim accepts only `ErrInjectedPanic` from it. Anything else, recovered or not, still fails the run.
- The lease after a panic: releasing it means a write from a broken state, so the maintainer forgets it and relies on expiry and on its own name.
- A panic in the application's own logger cannot be handled by logging. Like the driver's `driver.Valuer` exclusion, it is not recovered. That is the only code in the loop outside `Step`.

### What warrants review
- `/internal/engine/maintenance.go`, `Step` and `recovered`: the count resets only after a step that got to work and returned no error, and a `res.Ran` from before a panic counts its run exactly once.
- `/internal/sim/sim_test.go`, the relaxed "without maintainers" rule and the thresholds of `TestRun_maintenancePanics`: per run no anomaly and at least one panic; at least 3/4 of runs back off, and at least half resume. Per-maintainer resumption is shown exactly in the engine and driver tests.
- Replay: `make replay SEED=1 MUTATION=maintenance-unrecovered` fails with the panic checker.

**Self-review.** The code-review skill, at medium, found eight issues. I fixed five:
- The backoff shift overflowed at the 31st panic in a row; I had also seen this myself. The new 70-panic test covers it.
- The sim counted `MaintenanceResumed` on the very step that panicked when that step had compacted before the panic, so the resume assertion could pass with no later step. Now a step that returned `ErrPanicked` never counts as resuming. The 18-run test still has 11 resuming runs, against a threshold of 9.
- `MaintenanceRuns` was counted twice when a panic came after the step had counted its run, for example in the lease release. Now a `counted` flag guards it, and the new subtest panics on the release and expects 1, where the old code gave 2.
- Any step that got to work without a panic reset the panic count, even one that failed at once, so a panic behind passing errors stayed at the minimum backoff. Only a step without an error resets it now. The new subtest fails with `Expected "2e+10", but got "1e+10"` under the old reset.
- `Unreachable` fired for the injected panic too. It is skipped for `ErrInjectedPanic` now.

I left three:
- Forgetting the lease, rather than releasing it, can make other processes wait up to one lease duration, also after `Close`. This is accepted in the decision: the lease only saves work.
- The recovery sequence is duplicated between `/internal/sqldriver/sqldriver.go` and the engine. It is a few lines in two packages, and the engine cannot import the driver.
- The injection draws from the database's random source, which shifts later IDs only in runs with the mutation on. No test compares such a run with a run without it.

### Future work
- The lease is not released after a panic, also not at `Close`. A best-effort release at `Close`, after reading the lease back under the holder's name, would free it sooner for the other processes.
