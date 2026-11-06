# Diary: A primary-key condition fixes the plan to an index scan

This implements the decision of 8666dfc. maragudb accepts a condition that can fail next to a primary-key condition, such as `UPDATE acct SET b = b - $1 WHERE id = $2 AND b - $1 >= 0`, and evaluates the other conditions only on the rows that the key condition selects, as Postgres does with an index scan. Both differential harnesses run Postgres with `enable_seqscan = off`, and the leniency against a default Postgres is the documented difference DEV-20.

## Step 1: Index conditions, the harness setting, and DEV-20

**Author:** index-plan

### Prompt Context

**Verbatim prompt:** You are the builder `index-plan` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). Commit directly to main, with no PRs and no pushing; I push after I verify. Load the fabrik:go skill before you touch Go. [...] ## Goal Implement the decision of 8666dfc. maragudb must accept statements where a condition that can fail sits next to a primary-key condition, such as `UPDATE acct SET b = b - $1 WHERE id = $2 AND b - $1 >= 0`. It evaluates the other conditions only on the rows the key condition selects, as Postgres does with an index scan. ## Requirements 1. **Executor.** Remove the static rejection that UNS-PLAN-DEPENDENT-WHERE makes for this shape [...] Define exactly which key conditions select an index plan [...] Base that set on what Postgres 18 actually does with `enable_seqscan = off` on a btree primary key, and test it on both systems; don't assume. [...] 2. **Harness.** Both the pgdiff and the sqllogictest Postgres sessions must run with `enable_seqscan = off`. [...] add a test that asserts that both harnesses' connections have it. Check whether other plan settings are also needed for determinism, such as bitmap scans for IN [...] 3. **The DEV entry.** [...] DEV-20 unless the file says otherwise. [...] 4. **Requirements text.** [...] 5. **Ratchets.** [...] 6. **Workload.** [...] 7. **Verification.** [...] ## Hard rules [...] Start a new diary file, /docs/diary/2026-11-06-index-plan.md, with the fabrik:diary skill. Use `**Author:** index-plan` on your steps. Self-review with the code-review skill at medium when done, and fix what it finds.
**Interpretation:** Find out on Postgres 18 which WHERE conditions always become conditions of the primary key index scan with sequential scans off, make exactly those gate the rows in maragudb, keep rejecting what still depends on the plan, run both harnesses with the setting, document the leniency as DEV-20, and raise both ratchets.
**Inferred intent:** Accept the point updates that OLTP apps write most, without giving up the guarantee that every accepted statement matches Postgres in the configuration the harnesses compare against.

### What I did

I first asked Postgres. In a scratch database of the harness container (`docker exec maragudb-pgdiff psql`), `EXPLAIN (costs off)` with `enable_seqscan = off` on btree primary keys of every column type showed which conditions become index conditions:
- `=`, `<`, `<=`, `>`, `>=` of a key column with a constant in the column's type, on any key column, also the second column of a composite key alone (`b = 1` on `(a, b)`, a skip scan in Postgres 18), and also with a cross-type constant such as an integer literal on a bigint column or on a double precision column.
- `IN` of constants (`id = ANY('{1,3}'::bigint[])`), also with NULL elements.
- A bare boolean key column (`k = true`), `NOT k` (`k = false`), and `k > false`.
- `id = 1` derived from `id = n AND n = 1`, also through several equalities, and `id = 1` from `id = n + 0 AND n + 0 = 1`.
- Not index conditions: `<>`, `NOT IN`, `id::float8 = 1`, `id = 1.0` (numeric casts the column), a key column in an expression. These fall back to a sequential scan with `Disabled: true`.
- Uncertain ones: an OR of key conditions is a BitmapOr (ranges) or, new in Postgres 18, an array comparison (equalities on one column with constants of one type), but with ORDER BY on the key, or next to another key condition, Postgres can pick an index scan that checks the OR as a filter. A `LIKE` with a prefix becomes the range `s >= 'ab' AND s < 'ac'`, or `s = 'abc'` for a pattern without wildcard, and the LIKE itself stays a filter.

The executor in `/internal/sql/exec/plan.go`:
- `binder.indexCondition` takes the certain set: key comparisons (`binder.keyComparison`), `binder.keyIn`, and `binder.booleanKey` and its negation, when they cannot fail. `binder.impliedIndexConditions` adds `key = constant` for every key column in an equivalence class with a constant, and gives the known ones to `filter.keyed`, so `id = n AND n = 5` is now a point lookup.
- `filter.matches` evaluates the index conditions first when any conjunct can fail, and skips a row they reject without evaluating anything else on it.
- The static rejection is now only `binder.infallibleIndexConditions`: a conjunct that could be an index condition and can fail itself, which in practice is a LIKE on a key column with a trailing escape, because Postgres evaluates it only on its prefix range.
- `binder.canonicalQual` does what Postgres's canonicalize_qual does after folding: it drops FALSE and NULL arguments of an OR, and takes the conditions that every OR argument has in common out of the OR (`binder.factorOr`, like process_duplicate_ors). `binder.whereConjuncts` applies it to every conjunct.

UNS-ORDER-DEPENDENT-WHERE and UNS-PLAN-DEPENDENT-LIMIT stay as they are. Neither became obsolete: Postgres still orders the remaining filter conditions by cost on the rows the index selects, and under a LIMIT the index scan with sequential scans off still stops early in key order only when the ORDER BY matches the index, and otherwise sorts, so how many rows Postgres evaluates still depends on the plan.

The harness: `pgdiff.replaceDatabase` adds `enable_seqscan=off` to the connection string, which pgx sends as a startup parameter, so it is the default of every connection of the pool, survives `RESET`, and reaches the sqllogictest runner, whose connection setup I moved into `sqllogictest.fileRunner.postgres` so that a test can check it. `TestPostgres` in `/internal/pgdiff/postgres_internal_test.go` and `TestFileRunner_postgres` in `/internal/sqllogictest/run_test.go` assert the setting on several pooled connections; both failed with `Expected "off", but got "on"` before the change. Bitmap scans stay on: a bitmap heap scan checks its index conditions (its Recheck Cond) before its filter, so it gates rows like an index scan, and with `enable_bitmapscan = off` too, `EXPLAIN` showed that an OR of key ranges is then chosen by cost between two disabled plans (a sequential scan on a small table, a bitmap scan on a large one).

Tests on both systems: `/internal/pgdiff/plans_test.go` `TestIndexConditions` runs about 60 statements through `pgdiff.Session` on tables whose failing condition fails on a row that the key does not select, for every key type, composite keys, IN, ranges, boolean keys, implied equalities, UPDATE and DELETE, inside a transaction, the canonicalization cases, and the folding and type errors that must still be reported. `/internal/sql/exec/exec_test.go` has the maragudb side, including the parameter example from the decision. `TestUnsupported` for UNS-PLAN-DEPENDENT-WHERE now uses the LIKE examples. `TestDifferences` in `/internal/pgdiff/unsupported_test.go` covers DEV-20.

Docs: the UNS-PLAN-DEPENDENT-WHERE entry in `/docs/unsupported.md` lists only the LIKE, the plan section lists the index conditions, and "Remaining differences" has DEV-20 with its example. INV-PG-SUBSET in `/docs/requirements.md` now names the `DEV-` differences as its only exceptions, and AC-4.3 says that Postgres runs with `enable_seqscan = off`. AC-4.4 already required a `DEV-` ID and a test for any remaining difference, so I left it.

The generator in `/internal/pgdiff/gen.go` now writes a key comparison, an IN, or a range (`pgdiff.gen.keyCondition`), and lets half the conditions next to it fail outside LIMIT. Before, it produced this shape in about 20 statements per 10,000 cases.

Workloads: neither the bank nor the list-append workload avoided this shape. `/internal/workload/workload.go` reads the balance with SELECT and checks it before the UPDATE, and the simulator's bank in `/internal/sim/sqlclient.go` reads both balances because half its transfers overwrite them. Their condition is `id = $2` alone, so I left both as they are; a conditional debit would change what the workloads test, not remove a workaround.

### Why

The rule is sound because over-approximating the index conditions is the only dangerous direction. If maragudb skips a row that Postgres reads, Postgres can fail where maragudb succeeds. If maragudb reads a row that Postgres skips, the runtime check of UNS-ORDER-DEPENDENT-WHERE sees a condition reject the row and another fail on it, and rejects the statement. So an uncertain key condition, such as an OR or a LIKE, is treated as an ordinary condition, which is only ever stricter than Postgres. That also covers the lead's examples: an OR of key and non-key conditions was never a static rejection, and is a sequential scan in Postgres, where the order-dependence check already applies.

### What worked

- `EXPLAIN (costs off)` on a scratch database answered every question about index conditions in a few minutes, including surprises: the boolean key column, the skip scan on a second key column, and Postgres 18 turning `id = 1 OR id = 3` into an array comparison.
- Heavy runs found a problem within one or two runs of 10,000 cases each, six in all (below), and every fix came with a test that fails without it.
- Heavy runs, `make pgdiff PGDIFF_CASES=10000`, with the final generator and without mismatches: 49,879 of 51,309, 49,987 of 51,541, and 49,815 of 51,299 statements executed before the self-review fixes, and 49,373 of 50,750 (1,377 rejected, 1 of them UNS-PLAN-DEPENDENT-WHERE, the LIKE) after them. The final run is in the self-review below.
- The ratchet of `make ratchet` rose from 1,542 to 1,548 of 1,587 statements. Of that, 1 statement is the executor alone with the old generator (1,543); with the new generator, the old executor executes 1,547.
- The first full `make sqllogictest`, before the self-review fixes: 2,761,861 of 5,922,406 statements executed, up from 2,761,594, so all 267 statements that UNS-PLAN-DEPENDENT-WHERE rejected now run, with zero mismatches. No UNS-PLAN-DEPENDENT-WHERE rejection remains in the corpus; UNS-ORDER-DEPENDENT-WHERE still rejects 2. The gain is in random/aggregates (1 to 6 per file), select1 (492 to 506), select2 (492 to 500), and select3 (1,477 to 1,559).

### What didn't work

- The first heavy run after the setting failed on a read-only transaction: `SELECT * FROM t WHERE CASE 1.7976931348623157e+308 / c1 WHEN c1 THEN c0 END` gave `postgres SQLSTATE 22003, maragudb division by zero`. The plan was a sequential scan in both configurations, so this was an older harness gap: in a read-only transaction the harness could not store the rows in key order again and reported any row-order difference as a mismatch. hegel's database replayed it on every run, so two more runs failed the same way. `pgdiff.sides.inKeyOrder` now ends the read-only transaction, reruns the query in one of its own, and leaves an aborted transaction in its place.
- `UPDATE t SET c0 = c0 WHERE NULL OR c0 LIKE '\'` on a text key: `postgres: 0 rows affected; maragudb: LIKE pattern must not end with escape character`. Postgres drops the NULL from the OR in a WHERE clause, takes the LIKE as an index condition `c0 = ''`, and never evaluates it on the row `'0'`. That led to `binder.canonicalQual`.
- My first canonicalQual lost errors twice. `UPDATE t SET c0 = c0 WHERE CAST(CAST('\' AS text) AS bytea) = NULL OR NULL` in a read-only transaction gave `postgres SQLSTATE 22P02, maragudb read-only transaction`: binding the arguments with `bindPruned` swallowed the folding error. Then `DELETE FROM t WHERE CAST('\x' AS bytea) IN ('\x', c0 || coalesce('\x'))` gave `postgres: operator does not exist: bytea = text (SQLSTATE 42883); maragudb: 0 rows affected`: the TRUE first argument dropped the mistyped second one. Now `binder.constantOf` binds normally and checks for a recorded folding error, which it then discards, and a deciding constant first type-checks every argument.
- `go build` after adding a `constant` type: `constant redeclared in this block`, and a `binder.canonical` already existed in `/internal/sql/exec/stmt.go`; I named mine `constness` and `canonicalQual`, and `factorOr` uses the existing `canonical` to compare conditions.
- A heavy run after the review fixes failed on `UPDATE t SET c1 = c0 WHERE c0 = c0 / c0`: `postgres SQLSTATE 23505, maragudb division by zero`. Also an older gap: `exec.Session.updateKeys` returned the WHERE error of a row at once, while Postgres in key order first moves the rows before it, and one of them was a duplicate.

### Self-review

I ran the code-review skill at medium over `fc20da2..HEAD`. It reported eight findings, unverified. I fixed six:
- `canonicalQual` turned an AND with a NULL argument into FALSE, so the `where` loop pruned the conjuncts after it and lost their folding errors: `((j = 1 AND NULL) OR NULL) AND 1 / 0 = 1` returned no rows where Postgres fails with division by zero. A result that is false only through NULL is now NULL, which does not prune. The new examples in `TestIndexConditions` failed before the fix.
- The read-only rerun could hide a mismatch when Postgres's error was 25P02 and maragudb's transaction was not aborted. `sides.compare` no longer reruns on 25P02; a new test in `/internal/pgdiff/run_test.go` aborts only the Postgres transaction and failed before.
- Repeated winners in `factorOr`, which Postgres's list_union removes, are dropped.
- `filter.matches` evaluated the written index conditions twice per row; `filter.indexed` marks them and the loop skips them.
- `factorOr` rendered every condition again on each comparison; it renders each once.
- `whereConjuncts` canonicalized the pieces of a canonicalized AND again, and `constantOf` bound arguments that read columns. It now splits the AND, and arguments that read columns are not constants without being bound.

After the fixes, one more heavy run failed on `UPDATE t SET c1 = NULL WHERE coalesce('' || CAST('\x' AS bytea) IN (c4, '\', CAST('' AS text)))`: `postgres SQLSTATE 42883, maragudb cast`. This one is older too and has nothing to do with plans. Without a common type for the constants of an IN, Postgres compares the elements in the order they are written, so `bytea = text` for `c4` comes first, while maragudb's `desugar` had moved the constants to the front and parsed `'\'` as bytea first. `binder.commonInType` now decides while binding whether the constants become an array, `simplify` leaves such a mixed IN alone, and `binder.whereConjuncts` splits it into conjuncts only when they do. The next heavy run passed: 49,446 of 50,911 statements executed, 1,465 rejected, 8 of them UNS-PLAN-DEPENDENT-WHERE.

The full `make sqllogictest` after all fixes passed with zero mismatches and the same counts as `make sqllogictest-ratchet`: 2,761,861 executed, 3,116,739 rejected, 0 UNS-PLAN-DEPENDENT-WHERE and 2 UNS-ORDER-DEPENDENT-WHERE rejections. `make test`, `go test -race -shuffle on ./...`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass.

I left two. The finding that BETWEEN and NOT are not desugared before factoring is wrong: `simplify` already maps itself through AND and OR, and `(i BETWEEN 2 AND 3 AND 10 / j >= 0) OR (i >= 2 AND i <= 3 AND j + 1 > 0)` now has a test on both databases. The finding that `MARAGUDB_PGDIFF_URL` could already set `enable_seqscan=on` is real but needs a user to set it, and `TestPostgres` then fails, so the harness never runs silently with sequential scans on.

### What I learned

- In Postgres 18 with sequential scans off, every condition that matches the index goes into every index path, plain or bitmap, because there is one path per index with all matching clauses; the only alternatives are BitmapOr paths for ORs, and a full index scan in key order when ORDER BY matches, which then evaluates the filter on every row like a sequential scan.
- With `enable_seqscan = on`, the planner disables nothing and compares costs, and the filter of a sequential scan is ordered by cost: an IN of five constants costs 2.5 operators and `b - 10 >= 0` costs 2, so the subtraction runs first on every row. That is why the DEV-20 example uses an IN with five keys; `id = 2` alone costs 1 and runs first even in a sequential scan.
- canonicalize_qual treats NULL as FALSE only after eval_const_expressions has folded the whole qual, so the two must be kept apart: a FALSE from folding prunes later conjuncts, a NULL never does.

### What was tricky

- Getting the canonicalization to change only plans and never errors. Postgres type-checks the whole statement, then folds from the left and stops at a deciding constant, and only then canonicalizes. Each of the three bugs in it came from mixing these phases.
- Telling new mismatches from old ones. Three of the mismatches the heavy runs found were older gaps that neither the setting nor the new rule caused: the read-only rerun, the key-moving UPDATE, and the IN order. I checked that the plan was a sequential scan in both configurations, or that no plan was involved, and fixed them because the run must pass anyway.

### What warrants review

- `/internal/sql/exec/plan.go`: `binder.indexCondition` against the list above, `binder.impliedIndexConditions`, `filter.matches`, and `binder.canonicalQual` with `binder.constantOf`, `binder.typeChecks`, and `binder.factorOr`.
- `/internal/sql/exec/stmt.go` `Session.updateKeys`: the pseudo-move for a scan error.
- `/internal/sql/exec/expr.go` `binder.commonInType` and `inChain`, and their use in `binder.bind` and `binder.whereConjuncts`.
- `/internal/pgdiff/run.go` `sides.inKeyOrder` in a read-only transaction.
- Validate with `go test ./internal/pgdiff -run 'TestIndexConditions|TestDifferences|TestPostgres'`, `make pgdiff PGDIFF_CASES=10000`, and `make sqllogictest`.

### Future work

- An OR of key conditions, and a LIKE with a prefix, are not index conditions in maragudb, so a condition that fails on a row they reject is still rejected at run time where Postgres succeeds. Making them index conditions needs certainty about when Postgres checks them as a filter instead.
- `IN` and OR on the key are still full scans in maragudb, although `filter.index` now skips the rows quickly.
