# Diary: Driver hardening and small follow-ups

These are four follow-ups from the builders of the bounded manifest and of damaged data (`/docs/diary/2026-11-07-bounded-manifest.md`, `/docs/diary/2026-11-07-damaged-data.md`).

## Step 1: Set the scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Damaged data is pushed. Its builder left one open question and one difference from Postgres, and earlier builders left small gaps.
**Inferred intent:** Close the gaps before new features.

### What I did
I wrote the scope and gave it to the builder `driver-hardening`.
1. **Panics in the public driver.** A panic inside a driver call is a maragudb bug. Today it can hang the application forever on the lock of `database/sql`, as it hung the simulator. My call: the public driver recovers such a panic and returns an error that wraps a new exported `ErrInternal`. The error holds the panic value, and the stack goes to `Options.Logger`. The connection is then discarded. The driver must not return `driver.ErrBadConn` from a call that may have changed data, because `database/sql` would then run the statement again. A transaction that saw a panic cannot commit.
2. **Out-of-range timestamps.** A `time.Time` parameter outside the range of timestamptz is stored, where Postgres refuses it. This breaks `INV-PG-SUBSET`. maragudb must refuse it with Postgres's error class, which the builder checks on Postgres 18. The differential harness must cover it.
3. **Export `ErrUnsupportedManifest`** from the `maragudb` package, so that callers can detect an old format.
4. **The Antithesis workload** turns on the simulator's `DiskFaults` and `StoreDamage` some of the time.

### Why
A hang hides a bug and takes the application down with it. A stored value that Postgres refuses is a broken promise.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
`database/sql` retries a call that returns `driver.ErrBadConn`. That is safe only before anything was sent.

### What was tricky
Nothing yet.

### What warrants review
That no recovered panic can make a write happen twice or commit half of a transaction.

### Future work
None yet.

## Step 2: Recovered panics, parameter values that Postgres refuses, `ErrUnsupportedManifest`, and faults in the Antithesis sim runs

**Author:** driver-hardening

### Prompt Context

**Verbatim prompt:** You are the builder `driver-hardening` for maragudb [...] 1. Public driver recovers internal panics: every driver call recovers a panic and returns an error wrapping a new exported `maragudb.ErrInternal`, with the panic value in the message; the stack goes to `Options.Logger`. The connection is discarded [...] Never return `driver.ErrBadConn` from a call that may have changed data [...] prove with a test that a panicking autocommit write is not run twice. A transaction that saw a panic must not commit [...] 2. Out-of-range timestamps: a `time.Time` parameter outside timestamptz's range is stored today, where Postgres refuses it. Check on Postgres 18 exactly what it does [...] and match it. Cover it in the differential harness [...] Look for the same class of gap in other parameter types (float NaN/Inf, invalid UTF-8 strings, int overflows, []byte vs string) [...] 3. Export `maragudb.ErrUnsupportedManifest` [...] 4. Antithesis workload (/antithesis): turn on the simulator's `DiskFaults` and `StoreDamage` some of the time [...]
**Interpretation:** Make every driver call panic-safe without ever inviting database/sql's retry, make parameter values fail exactly where and how Postgres fails them, export one more error, and widen the faults of the Antithesis sim command.
**Inferred intent:** No bug inside maragudb may hang an application or run a write twice, and no parameter may store what Postgres refuses.

### What I did
- Panics (`aeb5ef6`, and fixes in `5ba4328`): `/internal/sqldriver/sqldriver.go` recovers in every call that does work and returns an error (`Connector.Connect` and `Close`, the connection's Prepare, BeginTx, Exec, Query, Ping, ResetSession, Close, the statement's calls, Commit, Rollback, `Rows.Next` and `Close`). The call returns `fmt.Errorf("%w: panic: %v", ErrInternal, p)`; the value is never wrapped, so that a value wrapping `driver.ErrBadConn` cannot trigger a retry. The logger gets the value and `debug.Stack()` at the error level, taken in the deferred call so it still shows the panicking frames, and an Antithesis `Unreachable` marks it. A broken connection reports `IsValid` false, so database/sql discards it on release; afterwards Prepare, Exec, and Query give `ErrInternal`, BeginTx, Ping, and ResetSession give `driver.ErrBadConn` (nothing has happened yet), Commit rolls back and gives `ErrInternal`, Rollback rolls back. `Options.OnPanic` is now an observer, which `/internal/sim/sim.go` uses to record and re-panic. `exec.Mutations.PanicAfterCommit` in `/internal/sql/exec/session.go`. The mutexes of `/internal/engine/engine.go`, `/internal/engine/compact.go`, `/internal/engine/blocks.go`, and `/internal/cache/disk.go` that were unlocked by hand are now released by deferred calls in small functions. `/maragudb.go` exports `ErrInternal` and builds its driver through `driverOptions`, which `TestDriverOptions` in `/maragudb_internal_test.go` uses. New `INV-PANIC-RECOVERED` in `/docs/requirements.md`.
- `ErrUnsupportedManifest` (`c79fc81`): exported, and `TestDB` in `/maragudb_test.go` checks it with `errors.Is` and that the manifest stays as it was.
- Parameters (`c2ee6da`): I probed Postgres 18 through pgx with a scratch test on both sides (boundaries to the nanosecond, BC years, wraparound, NaN and infinity, invalid UTF-8 and NUL in every context, bind errors in a transaction, error order). `value.TimeInRange` in `/internal/sql/value/parse.go` is the Postgres range, and row decoding refuses a time outside it. `paramError` in `/internal/sql/exec/expr.go` checks a parameter when the binder meets it: a time outside the range is `ErrOutOfRange`, a string that is not UTF-8 or holds a NUL is the new `ErrInvalidUTF8` (22021). The error is recorded in `Session.bindErr`, the parameter stands in as NULL or a time in range, and `binder.folding` reports it before folding errors but not while a subquery is only analyzed; `Session.run` reports it if the statement would succeed. `/internal/pgdiff/classes.go` maps 22008 to `ErrOutOfRange` too and 22021 to `ErrInvalidUTF8`. The generator in `/internal/pgdiff/gen.go` puts parameters into INSERT values and UPDATE assignments, with `Case.Args`, and `/internal/pgdiff/run.go` passes them on both sides. `TestParameters` draws boundary times and invalid strings; the requirements' type table, `/docs/unsupported.md`, and the ratchet (3187 to 3228 executed) changed with it.
- A pre-existing mismatch that the reshuffled generator found (`274be8d`): `SELECT DISTINCT FALSE FROM t AS x, t AS y WHERE x.c0 = y.c0 AND x.c0 = FALSE ORDER BY CAST('no' AS boolean)` ran on Postgres and failed on maragudb with "for SELECT DISTINCT, ORDER BY expressions must appear in select list". `binder.canonical` in `/internal/sql/exec/stmt.go` now writes a quoted literal cast to a type, and a boolean or bigint constant, as the value it reads into, as Postgres compares constants; cases in `/internal/pgdiff/joins_test.go` show both matches and non-matches (`5` against `'5'::bigint`, `1.5::float8` against `'1.5'::float8`).
- Antithesis (`e862afe`): `simConfig` in `/antithesis/workload/main.go` turns on `DiskFaults` and `StoreDamage` in a quarter of the runs each, independently, a test checks the proportions over 400 seeds, and `/antithesis/scratchbook/property-catalog.md` and `deployment-topology.md` list the panic and damage assertions.
- Docs (`167cd68`): a writer sub-agent wrote the two decisions in `/docs/decisions.md` and the README edits from my brief; I reviewed them and corrected them with the review fixes.

### Why
A panic that unwinds through database/sql leaves its locks held, and `driver.ErrBadConn` is the one error that makes database/sql run a call again, so the error of a recovered panic must be anything but that. Postgres reads parameter values at Bind, between analysis and planning, so matching its error class also means matching that place in the order.

### What worked
- The red step for the retry: with the recovery changed to `fmt.Errorf("%w: %w: panic: %v", ErrInternal, driver.ErrBadConn, p)`, `TestDriver_panics` failed with `Expected "1", but got "3" (type int64)`: database/sql ran the panicking autocommit update three times, twice on pooled connections and once on a new one.
- The harness catches both gaps: with `paramError` returning nil, `TestParameters` failed with `maragudb invalid cast: invalid input syntax for type bigint: "\xff"; postgres SQLSTATE 22021`, and with only the time check off with `maragudb time.Date(-4713, time.November, 23, 23, 59, 59, 999999000, time.UTC), <nil>; postgres <nil>, ERROR: timestamp out of range (SQLSTATE 22008)`. The new exec test fails in 21 subtests without the check.
- Postgres facts that shaped the design: `SELECT $1::timestamptz, nope` gives 42703, `SELECT $1::timestamptz, 1/0` gives 22008, `SELECT $1::timestamptz WHERE false` gives 22008, and after a refused parameter in a transaction the next statement gives 25P02.

### What didn't work
- My first edit of `/internal/sqldriver/sqldriver_test.go` replaced everything from `TestOptions_OnPanic` to the end of the file, so the old test with its `INV-DAMAGE-DETECTED` cover went away; I added that cover to the new first subtest.
- The first version of the exec test unquoted its strings with `strconv.Unquote`, which turned the raw byte 0xff into U+FFFD, valid UTF-8, so five cases failed with "expected an error from SELECT $1::text"; the cases now use Go escapes directly.
- The first differential run with parameters failed on `INSERT INTO t VALUES (DEFAULT, (SELECT count(*) FROM t AS s11), DEFAULT) -- parameters []interface {}{interface {}(nil)}`: `subqueryWrite` replaced the cell that held the parameter by a subquery. It keeps parameters now, and `Generate` panics on a statement that does not use each of its parameters.
- `time.Hour * 24 * 365 * 1000` overflowed `time.Duration` at compile time in a test; `AddDate` does it.
- `git stash push -- $P2` in zsh passed the whole list as one path, `error: pathspec ... did not match any file(s) known to git`; the paths go inline now. A `go build ./antithesis/workload` left a `workload` binary at the root, which I deleted.

### What I learned
- pgx floors a `time.Time` to the microsecond and wraps around beyond about 292,277 years from 2000, so Postgres accepts the year 600000 as 15445-12-13. maragudb refuses such times; the harness only draws times that pgx encodes faithfully.
- pgx sends a Go string as text for every parameter type, so Postgres's encoding check applies to strings everywhere, while a `[]byte` for bytea is binary and unchecked.
- In Postgres, an unknown literal cast to a type becomes a constant at parse time, so `FALSE` and `'no'::boolean` are equal expressions, while `1.5::float8`, a numeric cast at plan time, is not equal to `'1.5'::float8`.

### What was tricky
- Placing the parameter error between analysis and folding without an analysis phase of its own: the stand-in value must analyze the same, which is why a refused string becomes NULL (every type reads it) and a refused time stays a time (any other context is already `ErrUnsupported`, which must win, since Postgres would accept the time sent as text there).
- Self-review (code-review skill at medium) findings:
  - Fixed: `Conn.Close` skipped closing the connector of a `Driver.Open` connection when the rollback of its broken session panicked again; the owner closes in a deferred call now, with a test.
  - Fixed: `Connector.Close` and `Rows.Close` had no recovery although the docs promised every call; they recover, and the docs say that calls without an error only read fields.
  - Fixed: the harness took any Postgres error without a SQLSTATE as a parameter that pgx refused, and accepted any maragudb error there; it now wants pgx's "failed to encode" and maragudb's `ErrOutOfRange`.
  - Fixed: `ErrInternal` blamed maragudb alone, while a panic in a custom `Options.Store` lands there too; its text and docs say so.
  - Fixed: the generator compiled a regexp per parameter, and the doc of `canonical` did not say that every quoted literal cast is written as its value.
  - Left: decoding refuses stored times outside the range, which earlier versions stored; the project is pre-release, and the decision says so.
  - Left: a single scan of all parameters before the statement, instead of the check in the binder, would report the error before analysis errors and would turn `ErrUnsupported` for a time in a text context into `ErrOutOfRange`, where Postgres succeeds.
  - Left: an execution error with a stand-in value before the statement's folding is not reachable, because every statement folds before it reads rows.

### What warrants review
- `/internal/sqldriver/sqldriver.go`: that no path returns `driver.ErrBadConn` after it may have written; only `BeginTx`, `Ping`, and `ResetSession` do, before anything runs.
- `/internal/sql/exec/expr.go` `paramError`, `paramStandIn`, and `binder.folding`, and `Session.run` in `/internal/sql/exec/session.go`.
- `/internal/pgdiff/gen.go` `parameterize`, `paramValue`, `paramTime`, and the pgx branch of `sides.compare` in `/internal/pgdiff/run.go`.
- Validate with `go test ./internal/sqldriver -run TestDriver_panics`, `go test ./internal/sql/exec -run 'TestSession_Select/refuses'`, `go test ./internal/pgdiff -run 'TestParameters|TestErrorClasses|TestJoins'`, and `make pgdiff PGDIFF_CASES=10000`.
- Checks: `make test` with Tigris passes (after the review fixes, with the local hegel database of `/internal/pgdiff` moved aside, see below), `make lint` 0 issues, `make verification` no diff, `go mod tidy` no diff. `make soak`: 23,335 simulator runs in 5 minutes without anomalies, and the full corpus of 622 files in 11 minutes 30 seconds with 5,860,887 statements executed and zero mismatches. `make pgdiff PGDIFF_CASES=10000` passed once before the review fixes (107,777 statements executed, 482 agreements on 22021 and 302 on 22008), and twice after them failed on the two mismatches below, which involve no parameter.
- Timings with `GOMAXPROCS=4 go test -race -count 1`, wall time including the build, before and after: the root package 42 s and 43 s, `internal/sqldriver` 2 s and 2 s, `internal/sql/exec` 26 s and 27 s, `internal/sql/value` 6 s and 6 s, `internal/engine` 33 s and 36 s, `internal/cache` 7 s and 7 s, `internal/pgdiff` 20 s and 19 s, `antithesis/workload` 3 s and 3 s, `internal/sim` 85 s and 86 s.

### Future work
- Two mismatches that the longer differential runs found, both without parameters and through no code that this work changed; the first one reproduces with `/internal/sql/exec/stmt.go` reverted, and I did not replay the second on the old code: `DELETE FROM t AS o WHERE (SELECT CASE WHEN (SELECT FALSE FROM t AS s12 WHERE s12.c0 = o.c0 AND CAST('off' AS boolean) ...) THEN ... END FROM p AS s20 WHERE CAST(s20.c1 AS bigint) > -9223372036854775808)` on an empty `t` and a row of `p` whose `c1` is `''` fails on Postgres with 22P02 and succeeds on maragudb: Postgres folds `s12.c0 = o.c0 AND false` away, so the subquery loses its correlation and runs once as an InitPlan, even over no rows, while maragudb treats it as correlated. And `SELECT abs(o.c0), NULL FROM t AS o ORDER BY (SELECT min(s21.c0) FROM t AS s21 WHERE 10 / (s21.c0 - s21.c0) >= -9223372036854775808), o.c0 DESC` over the rows -9223372036854775808 and 9223372036854775807 fails with 22012 on Postgres and with `ErrOutOfRange` on maragudb, an order of evaluation between an output expression and a sort key subquery. The hegel databases that replay them are moved out of `/internal/pgdiff/.hegel`, which is gitignored, so that `make test` is not red locally.
- A statement whose parameters skip a number, such as `SELECT $2::bigint` with two values, fails on Postgres with 42P18, "could not determine data type of parameter $1", and runs on maragudb.
- A literal in SQL text that is not valid UTF-8 is `ErrSyntax` in maragudb's lexer, where Postgres gives 22021.
- A panic in the background maintenance loop still ends the process.
