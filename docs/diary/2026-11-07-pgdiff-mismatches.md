# Diary: Fix the differential mismatches that driver hardening found

The driver-hardening builder ran `make pgdiff PGDIFF_CASES=10000` three times. Two runs failed, each on a mismatch that was already in the code before its work, so the large run is flaky. It also found two smaller differences from Postgres. This task removes all four, so that maragudb stays a strict subset of Postgres 18.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Driver hardening is pushed. The differences from Postgres that it found come next, because each one breaks the subset contract.
**Inferred intent:** A database whose SQL a user can trust to behave as Postgres does, or to refuse with `ErrUnsupported`.

### What I did
I wrote the requirement and gave it to the builder `pgdiff-mismatches`. It has four parts.
- **Subquery that folding makes uncorrelated.** `DELETE FROM t AS o WHERE (SELECT CASE WHEN (SELECT FALSE FROM t AS s12 WHERE s12.c0 = o.c0 AND CAST('off' AS boolean) …) THEN … END FROM p AS s20 WHERE CAST(s20.c1 AS bigint) > …)` runs with `t` empty and `p.c1 = ''`. Postgres fails with 22P02, and maragudb succeeds. Postgres folds `… AND false` away. The subquery then has no correlation, and Postgres runs it once as an InitPlan even when there are no outer rows. maragudb treats it as correlated and never runs it.
- **Error order with a sort-key subquery.** `SELECT abs(o.c0), NULL FROM t AS o ORDER BY (SELECT min(s21.c0) FROM t AS s21 WHERE 10 / (s21.c0 - s21.c0) >= …), o.c0 DESC` runs over the rows MinInt64 and MaxInt64. Postgres gives 22012, and maragudb gives `ErrOutOfRange`.
- **Skipped parameter numbers.** With two values, Postgres fails `SELECT $2::bigint` with 42P18, and maragudb runs it.
- **Invalid UTF-8 in SQL text.** Postgres gives 22021, and maragudb's lexer gives `ErrSyntax`.
- **The rule for each fix.** The builder either matches Postgres or makes the case `ErrUnsupported` with a UNS entry in `/docs/unsupported.md`, tested on both systems. Matching Postgres comes first when the cost is reasonable. Making the plan-dependence rules looser is not allowed.
- **Verification.**
  - The saved hegel replays in the scratchpad reproduce both mismatches before the fix and pass after it.
  - The harness gets cases for each of the four parts.
  - Three consecutive runs of `make pgdiff PGDIFF_CASES=10000` pass.
  - The ratchet does not drop.

### Why
Each mismatch is a place where maragudb gives an answer that Postgres does not, which is the one thing the contract forbids. A flaky large run also hides new mismatches.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Postgres decides correlation after constant folding, not from the SQL text.

### What was tricky
Nothing yet. The sort-key case may need a plan rule instead of a match, because the order in which Postgres evaluates the output expressions and the sort keys depends on its plan.

### What warrants review
Whether each fix matches Postgres or refuses the case, and that no rule became looser.

### Future work
None yet.

## Step 2: Fold correlation, row order, parameter typing, and UTF-8 text the way Postgres does

**Author:** pgdiff-mismatches

### Prompt Context

**Verbatim prompt:** You are the builder `pgdiff-mismatches` for maragudb [...] ## Requirements — four Postgres differences to remove 1. **Subquery that constant folding makes uncorrelated.** [...] 2. **Error order between output expression and sort-key subquery.** [...] 3. **Skipped parameter numbers.** `SELECT $2::bigint` with two values: Postgres 42P18 [...] Check what Postgres does for other shapes (unused $1 that is typed elsewhere, etc.) and match. 4. **Invalid UTF-8 in SQL text.** [...] For each: match Postgres where the cost is reasonable; otherwise refuse the case with `ErrUnsupported` and a UNS entry tested on both systems. Do NOT loosen the existing plan-dependence rules. [...] Prefer a general fix to a patch for the exact shape; think about what class of queries each mismatch represents. [...]
**Interpretation:** Measure each difference on Postgres 18, find the class of statements behind it, and make maragudb match or reject that class, with tests on both systems, without making any rule looser and without lowering the ratchet or the corpus.
**Inferred intent:** A large differential run that passes every time, because the classes behind the failures are closed, not just the four statements.

### What I did

I measured with a scratch probe test in `/internal/pgdiff` (copied in per run from the scratchpad, never committed) and psql in the harness container, then fixed each class. All code is in `/internal/sql/exec`.

- **Part 1, correlation after folding.** Postgres plans a subquery after it folds it, so a reference to the query around in `x.c0 = o.c0 AND false` is gone, and the subquery is an uncorrelated InitPlan in a `One-Time Filter`, evaluated before the scan of an empty table. Each reference that `binder.columnRef` makes to a query around is now an `outerUse`, carried on `cexpr.outer`, propagated by `binder.fold` and the conversions, and recorded on every `subqueryInfo` it passes through. `dropped` marks uses dead where folding drops the expression: `bindPruned` (pruned CASE branches, COALESCE arguments, the right side of a decided AND/OR), the left side when the right side decides AND/OR, and a WHERE or HAVING that a FALSE conjunct decides (`filterOf`, `binder.having`; a NULL conjunct drops nothing, as in Postgres). `subqueryInfo.live` gives the folded view, which only the plan rules in `binder.subquery` use: a scalar subquery in a condition whose uses are all dead is evaluated once with NULL for the query around (`nullRun`, which now returns rows), a failure rejects it with `UNS-PLAN-DEPENDENT-SUBQUERY`, and one whose live uses reach only further out is rejected as before. Memoization stays on the written correlation, so a wrong liveness can only make maragudb stricter.
- **Part 2, row order.** The measurement showed that the mismatch is not outputs against sort keys (Postgres evaluates the output before the sort keys within a row, as maragudb does) but the order of the rows: Postgres drops the constant sort key (the uncorrelated subquery) and scans the primary key backward for `o.c0 DESC`. `orderFixed` in `/internal/sql/exec/select.go` says when Postgres reads a single-table SELECT in key order: no descending sort key and no subquery in FROM or CTE as the source. Otherwise `binder.query` evaluates every row, collecting WHERE failures through `filter.failures` and output and sort-key failures in `visit`, and `sameFailure` rejects rows that fail with different classes, as joins and grouping already did.
- **Part 2's neighbour, found by my first 10,000-case run.** `SELECT sum(DISTINCT c0)::double precision FROM t WHERE CAST(c1 AS bigint) > 1 GROUP BY c2, c0 HAVING 10 / c2 > 0`: Postgres 22012, maragudb 22P02. Postgres evaluates WHERE conditions in the order of their cost (EXPLAIN shows `Filter: (((10 / q.j) > 0) AND ((q.s)::bigint > 1))` for the written order the other way round), so `filter.failed` in `/internal/sql/exec/plan.go` now rejects a row on which two conditions fail with different classes. It does not wrap `errOrderDependent`, so a later row's certain failure cannot hide it.
- **Part 3, parameters.** The skipped number was one shape of a larger class: Postgres gives each parameter one type for the statement, from the first context that coerces it, and reads the values at Bind, after the analysis, in number order. Probes found more mismatches with maragudb's literal-like reading: `SELECT $1::bigint, $1 = 'abc'` (Postgres 22P02, maragudb a row), `WHERE i = $1 AND s = $1` (42883 against no error), `SELECT $1::bigint, nope` with "abc" (42703 against 22P02, because maragudb converted the value during analysis), `$1 IS NULL` and `count($1)` (42P18). `paramLedger` in `/internal/sql/exec/expr.go` records per parameter the types its contexts give it (in `resolve`) and the contexts that give none (`IS NULL`, `count()`), and the values Postgres refuses; `paramLedger.check` runs once in the top binder's `folding`: a skipped number or an untyped parameter is `ErrType` (42P18, now in the harness's type class), several types or a type and none are `ErrUnsupported`. Conversion errors of a parameter's value now wait like the UTF-8 refusals did, with NULL standing in, and are reported for the lowest number, pgx's encode refusals first. `Statement.used` records the referenced numbers. A pending refusal skips eager subquery evaluation.
- **Part 4, SQL text.** `checkText` in `Prepare` refuses text that is not UTF-8 or holds a NUL, anywhere, with `ErrInvalidUTF8`, before parsing. Postgres gives 22021, and 08P01 for a NUL (its protocol ends the text there), which the harness now maps to the same class.
- **A further neighbour, found by the second round of runs.** `UPDATE t SET ... WHERE coalesce(... IN (CASE '1970-01-01 00:00:00+00' WHEN c0 THEN ... END, ..., CAST('' AS bigint)))` failed with 42883 on Postgres and 22P02 on maragudb, also before my changes (reproduced on the old code). Minimal: `SELECT 1 FROM t WHERE 0::bigint IN (CASE 'x' WHEN c0 THEN 1::bigint END, 2::bigint, CAST('' AS bigint))`. Postgres analyzes every element of an IN in written order before it splits the constants into an array; maragudb's desugared form bound the constants first. `binder.commonInType` now binds the left side and every element in order and returns the first error, and `whereConjuncts` keeps an IN that fails as it is.
- Tests on both systems: `TestRowOrder` in `/internal/pgdiff/plans_test.go` (outcomes, and EXPLAIN pins of the backward scan, the dropped constant key, incremental sort, the full sort for `NULLS LAST`, output before sort keys, and cost order of conditions), new cases and EXPLAIN pins in `TestSubqueryPlans`, a hegel property in `TestParameters` that draws repeated and skipped parameters next to analysis and folding errors, IN cases in `TestIndexConditions`, and `TestErrorClasses` cases for 42P18, 22021 in text, and 08P01. `TestPrepare` checks the text refusal. Docs: a writer sub-agent drafted the UNS-PARAMETERS, UNS-PLAN-DEPENDENT-SUBQUERY, and "Errors that depend on order" edits, the requirements sentences, and three decisions; I reviewed them, moved the parameter paragraph into `/README.md` too, and added the cost-order rule to UNS-ORDER-DEPENDENT-WHERE and the decision.

### Why

Each mismatch was an instance of a class: correlation decided before folding, row order assumed to be key order, parameters read as independent literals, and text checked by the lexer instead of at receipt. Patching the four statements would have left the generator to find the neighbours, which it did twice while I worked.

### What worked

- Both saved replays fail on the old code (`postgres: error: ERROR: invalid input syntax for type bigint: "" (SQLSTATE 22P02)` / `maragudb: 0 rows affected`, and `postgres: error: ERROR: division by zero (SQLSTATE 22012)` / `maragudb: error: value out of range: bigint out of range`) and pass with the fix, run by copying each `.hegel` into `/internal/pgdiff/.hegel`.
- Red checks for every new test: switching off the new rule, or running the new test on a worktree of the old code, fails it.
- Keeping memoization on the written correlation made the liveness tracking safe in both directions: a missed propagation is the old behaviour, a wrong kill only rejects more.

### What didn't work

- My first pin `(SELECT max(CASE WHEN (SELECT FALSE FROM dev_two x WHERE x.id = dev_e.id ...) THEN 1 END) FROM dev_two ...)` failed on Postgres with `ERROR: aggregate functions are not allowed in WHERE (SQLSTATE 42803)`: an aggregate whose argument's only columns are inside a subquery of the outer level belongs to the outer query.
- A pin I expected, `WHERE a = 1 ORDER BY a, b DESC` scanning backward, came out as `Bitmap Heap Scan` plus `Sort` on a tiny table, which is why the rule rejects instead of emulating the backward scan.
- The ratchet first dropped from 3,228 to 3,227: `DELETE FROM t AS o WHERE (SELECT EXISTS (SELECT * FROM t AS s11 WHERE s11.c0 <> o.c0 AND (FALSE AND TRUE)) FROM t AS s20 WHERE EXISTS (...))` failed with 21000 on both before and was now rejected, because the folded subquery returns two rows. Postgres fails with 21000 before any row or at the first row that reaches it, so the statement is now accepted when it does fail with `ErrCardinality` and rejected otherwise (`Session.uncertain`, set only outside subquery analysis and applied only once the statement passed its analysis and folding, as the review asked).
- `exec_test.go` stored `'x\u0000'` in a text key; the new text check refused it (`invalid byte sequence for encoding "UTF8": 0x00 at byte 50 of the statement`). Postgres cannot store a NUL in text, so the case uses `\u0001`, and a new bytea key case keeps the coverage of the NUL escape in key encoding (it fails when `appendEscaped` stops escaping 0x00).
- I ran `git stash push internal/sql/exec/expr.go internal/sql/exec/plan.go` to check a test red, which stashed all my changes in those files, and popped it at once; the red check then ran on the old-code worktree instead. The same mistake as the subquery builder's.
- My first baseline timing run measured the working tree while I edited it; I stopped it and timed a detached worktree of `a368208` at `/.baseline` instead (not committed, removed at the end).

### What I learned

- Postgres decides correlation after folding for SubPlans, but pulls up IN and EXISTS into joins by the written correlation, before folding.
- A sort key that is constant, including an uncorrelated subquery, is dropped from the pathkeys, so `ORDER BY (SELECT ...), id DESC` is a backward index scan.
- Postgres orders WHERE conditions by estimated cost: a conversion through text costs more than a division.
- `$1 IS NULL OR k = $1` fails in Postgres with 42P08, while `k = $1 OR $1 IS NULL` works.

### What was tricky

- Probe binds: plan code rebinds parts of conditions with `bindPruned`, which made "every reference typed" unusable as the test for an untyped parameter; the ledger therefore records untyped contexts explicitly and treats a parameter with no recorded type and no untyped context as unsupported.
- Deferring a parameter's conversion error meant the eager subquery evaluations during analysis would turn it into a rejection; they are skipped while a refusal is pending.

### What warrants review

- `/internal/sql/exec/subquery.go`: `outerUse`, `subqueryInfo.live`, `dropped`, and the new cases in `binder.subquery`; `/internal/sql/exec/expr.go`: `paramLedger`, `resolve`, `binder.folding`, `commonInType`; `/internal/sql/exec/select.go`: `orderFixed`, `sameFailure`; `/internal/sql/exec/plan.go`: `filter.failed`; `/internal/sql/exec/session.go`: `checkText`, `Session.uncertain`, `Session.running`.
- Validate with `go test ./internal/pgdiff -run 'TestRowOrder|TestSubqueryPlans|TestParameters|TestErrorClasses|TestIndexConditions'` and `make pgdiff PGDIFF_CASES=10000`.
- Self-review (code-review skill at medium) gave ten findings. Fixed: `Session.uncertain` overwrote errors that Postgres reports for certain before it runs the statement (refused parameters, 42P18, folding errors, 25006), so it now applies only once the top binder's folding passed (`Session.running`) and never over `ErrReadOnlyTx`; `uncertain` was set inside subqueries that are only analyzed, which Postgres may never run, so it is set only outside analysis; `count(DISTINCT $1)` recorded the parameter both untyped and as text, so count() now drops the reference like IS NULL; and the NUL key-encoding coverage, moved to a bytea key. Left, as they only make maragudb stricter or follow the project's key-order convention: the four items in Future work, the unwrapped error of `filter.failed` (deliberate, see above), and `bindPruned` marking the references of planning rebinds dead, which are fresh objects of their own.
- Checks on the final code: three consecutive `make pgdiff PGDIFF_CASES=10000` runs passed (107,135 to 108,403 of about 110,000 statements executed, 2,299 to 2,328 rejected), after one run before the review fixes passed and two found the neighbours above; `make ratchet` stays at 3,228 of 3,328; `make test`, `make lint` (0 issues), `make verification` (only the regenerated covers, which are committed), and `go mod tidy` (no diff) pass, and `make sqllogictest` on the full corpus, 622 files in 11 minutes 31 seconds, executes 5,860,887 of 5,939,714 statements with zero mismatches, as before. `GOMAXPROCS=4 go test -race -count 1` per package, wall time including the build, before (worktree of `a368208`) and after: `internal/sql/exec` 27 s and 28 s, `internal/pgdiff` 21 s and 21 s, `internal/sql/parser` 9 s and 9 s, `internal/sim` 87 s and 87 s, the root package 42 s and 42 s.

### Future work

- Self-review left: `orderFixed` still assumes key order for other single-table SELECTs, which the harness's key-order rerun and the requirement's allowance for order-dependent classes cover; an eager subquery evaluation can still run before a later clause refuses a parameter, which only rejects; `check` reports an unsupported parameter before a lower skipped number, which only rejects; failures are kept per row in the collected scans rather than only the first two kinds.
- `filter.matches` lets a certain failure on a later row win over an earlier row that is rejected or fails depending on the order of conditions; when the classes differ, Postgres may report the earlier row's error. Not seen in the runs.
- The generator never repeats or skips a parameter; the property in `TestParameters` covers that, but the generator could too.
