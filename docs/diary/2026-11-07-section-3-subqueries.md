# Diary: Section 3, part 3: subqueries and non-recursive WITH

This part finishes section 3 of `/docs/requirements.md`. The section's last two items are `IN` and `EXISTS` subqueries with scalar subqueries, and non-recursive `WITH` (CTEs). Parts 1 and 2 added joins, DISTINCT, grouping and aggregates, and arithmetic on numeric (`/docs/diary/2026-11-06-section-3-joins.md`, `/docs/diary/2026-11-06-section-3-aggregates.md`, `/docs/diary/2026-11-06-numeric-arithmetic.md`). Subqueries are now the largest rejection reason in the sqllogictest corpus, at 92,443 statements.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Parts 1 and 2 of section 3 and the numeric arithmetic are pushed, so the last part of section 3 comes next. The lead makes the scope calls.
**Inferred intent:** Finish section 3 without weakening the guarantee that every accepted statement gives the same result as Postgres.

### What I did
I wrote the scope and gave it to the builder `section3-subqueries`.

In scope:
- `IN (subquery)`, `NOT IN (subquery)` with its NULL rules, `EXISTS`, `NOT EXISTS`, and scalar subqueries, with 21000 when a scalar subquery returns more than one row. They can be correlated or uncorrelated, and can appear in `WHERE`, `ON`, the select list, `HAVING`, and `ORDER BY`.
- Subqueries in `FROM` with an alias (derived tables), because a non-recursive CTE is the same thing with a name.
- Non-recursive `WITH` on a `SELECT`: several CTEs, a CTE that reads an earlier one, and column names.
- Subqueries in writes: in the `WHERE` of `UPDATE` and `DELETE`, in `SET`, and in `INSERT ... VALUES`, including subqueries that read the table being written.

These stay UNS:
- `ANY`, `SOME` and `ALL`, and row-valued `IN` such as `(a, b) IN (SELECT ...)`.
- `LATERAL`.
- `WITH RECURSIVE`, `MATERIALIZED` and `NOT MATERIALIZED`, data-modifying CTEs, and `WITH` in front of `INSERT`, `UPDATE` or `DELETE`.
- An outer aggregate inside a subquery, such as `SELECT (SELECT sum(t.x)) FROM t`.
- `INSERT ... SELECT`, `UNION` and views. Views are out of scope in the requirements.

The plan rule extends the join rule. Postgres can run an uncorrelated subquery once, lazily at first use, or never. It can turn `IN` and `EXISTS` into semi-joins or hashed subplans, and `NOT EXISTS` into an anti-join. A semi-join with a nested loop stops at the first match. So whether an error inside a subquery is raised, including the 21000 of a scalar subquery, depends on the plan. Accept only what gives the same outcome under every plan; otherwise reject with a new `UNS-PLAN-DEPENDENT-SUBQUERY`. The builder pins the rules with EXPLAIN and writes them as a decision entry.

Serializability: reads made by a subquery must count in the commit's conflict check. A simulator workload uses a write with a subquery, and a new mutant that leaves the subquery's reads out must be detected.

### Why
`WHERE id IN (SELECT ...)`, `EXISTS`, and a CTE for readability are common in application SQL, and they are the last items of section 3.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Most of the risk lies in which plan Postgres picks, and in the read set for serializability, not in the semantics. The semantics of subqueries are well defined.

### What was tricky
An uncorrelated subquery that is never evaluated, because no outer row needs it, cannot raise its error. Postgres may also evaluate it before the scan, for example when it is in an index condition.

### What warrants review
The plan-dependence rules for subqueries, and the read tracking of subqueries in writes.

### Future work
Section 3's acceptance criteria then decide whether section 3 is done.

## Step 2: Subqueries, subqueries in FROM, and non-recursive WITH, with a plan rule for subqueries

**Author:** section3-subqueries

### Prompt Context

**Verbatim prompt:** You are the builder `section3-subqueries` for maragudb (/Users/maragubot/Developer/maragudb, module maragu.dev/maragudb), [...] Read first: CLAUDE.md, /docs/requirements.md, /docs/decisions.md [...] and the diaries [...]. My Step 1 in the last one is your scope — read it carefully. [...] ## Contract INV-PG-SUBSET [...] ## Scope (from Step 1, restated) [...] ## Plan rule (the hard part) [...] ## Serializability Reads made by subqueries (and CTEs/derived tables) must be part of the transaction's read set for the commit conflict check, including in writes. Add or extend a simulator workload [...] add a mutant field to `sim.Config` (e.g. `MUT-SUBQUERY-NO-READS`) that drops subquery reads from conflict tracking [...] ## Verification required [...] ## Rules [...] ## Report [...] (and, mid-run: "Keep sim's growth near zero: any new simulator workload or mutant cell must replace time elsewhere, or run in the soak rather than in `make test`. Report sim's before/after timing under `GOMAXPROCS=4 go test -race`.")
**Interpretation:** Add IN, NOT IN, EXISTS, NOT EXISTS, scalar subqueries, subqueries in FROM, and WITH on SELECT, in reads and writes, prove on Postgres 18 which plans decide whether an expression in or around a subquery is evaluated, accept only what every plan agrees on, make the generator, the corpus, the simulator, and the docs cover it, and keep CI time flat.
**Inferred intent:** Finish section 3 without weakening INV-PG-SUBSET or serializability, and leave evidence a reviewer who never reads the code can trust.

### What I did

The SQL, in c2df23d:
- `/internal/sql/ast`: `Subquery` (scalar, EXISTS, IN with `Not`), `Derived` for a subquery in FROM, `Select.With` with `CTE`, the renderer, and `ast.Selects`, `ast.SelectExprs`, and `ast.SubqueriesOf`. `ast.Walk` and `ast.Map` do not descend into a subquery's query, only into the left side of IN, so `HasAggregate` and the column helpers stay at one level.
- `/internal/sql/parser`: subqueries in expressions, `IN ((SELECT ...))` as Postgres's grammar reads it, derived tables with an alias, and `WITH` in front of SELECT and inside subqueries. Rejected: `WITH RECURSIVE`, `MATERIALIZED`, a writing query in WITH, WITH in front of a write, VALUES as a query, a subquery in FROM without an alias, and a subquery in an IN list.
- `/internal/sql/exec/subquery.go`: a subquery is bound where it is written, by running its query in an analysis mode (`binder.analyzing`) that stops after folding, so its errors come in Postgres's analysis order. A name that the subquery's FROM does not have resolves in the queries around, nearest first (`binder.columnRef`), and the reference reads the row the query around evaluates the subquery on; the binder of the query around is snapshotted, so the namespace of an ON clause stays as it was. Evaluation runs the query again (`binder.evaluator`), memoized by the row of the query around, or once per statement for an uncorrelated one in `Session.subqueries`. The outer references of each subquery are recorded in `stmtRun.refs`, which `tablesOf`, `columnsOf`, and `touch` read, so a join places a correlated conjunct at the right node and an index on a correlated column counts as a scan Postgres could pick. A `sublink` flag keeps folding from evaluating a subquery at bind time.
- `/internal/sql/exec/derived.go`: a subquery in FROM and a CTE are tables of the statement (`table.derived`) whose rows are evaluated before the statement reads any row, with the primary key being every column; CTEs are an immutable chain on the binder, read before tables of the same name.
- Subqueries read the statement's snapshot without its own writes (`Session.snapshotReads` routes point lookups to the transaction), so `UPDATE sa SET v = (SELECT coalesce(x.v, 0) + sa.id FROM sa x WHERE x.id = 1)` reads the old row 1 on every row, as Postgres does.

The plan rule: the evaluation is complete, and any failure in it rejects the statement as `UNS-PLAN-DEPENDENT-SUBQUERY`, except the 21000 of a scalar subquery with two rows (`ErrCardinality`, new, mapped to 21000 in `/internal/pgdiff/classes.go`). For the statement around, a subquery that can fail is an expression that can fail, so the existing WHERE, join, grouping, and LIMIT rules decide where it is evaluated. On top, where Postgres can evaluate a subquery without a row of the query around: an uncorrelated one that can fail in WHERE, ON, or HAVING is evaluated before any row; a correlated EXISTS that can fail is also run with NULL for the columns of the query around (`binder.nullRun`), which makes every row of its tables pass through the order-dependence check; a correlated IN that can fail in a condition is rejected; a subquery in FROM whose query can fail binds with `wide`, which takes no index conditions; a condition of the query around that reads a derived column is rejected when it, or the derived query, can fail; and folding errors inside a subquery are rejected. The decision is in `/docs/decisions.md`.

The experiments, in a scratch database `sqx` of the harness container, which `TestSubqueryPlans` in `/internal/pgdiff/subqueries_test.go` pins:
- `SELECT * FROM e WHERE id = (SELECT x FROM two)` on an empty `e`: `Index Cond: (id = (InitPlan 1).col1)` and 21000; the same on a column without an index is a Filter and succeeds.
- `a IN (SELECT b FROM u)` is a Merge Semi Join, `NOT EXISTS` a Merge Anti Join, `NOT IN` a `hashed SubPlan`, an uncorrelated EXISTS a `One-Time Filter` with an InitPlan, and, new in Postgres 18, a correlated IN is a semi-join too.
- `a IN (SELECT b FROM u WHERE u.id = 1) AND 10 / w > 0` on 1,000 rows is a Merge Join whose scan of `t` filters `(10 / w) > 0` on every row, and fails, while the one row the IN accepts does not.
- `EXISTS (SELECT 10 / w FROM two)` succeeds, `EXISTS (SELECT count(10 / w) FROM two)` fails; `SELECT 1 FROM (SELECT 10 / w AS q FROM two) s` and the CTE read once succeed, the CTE read twice is a `CTE Scan` and fails, and `WITH c AS (SELECT 1 / 0) SELECT 1` succeeds.
- `SELECT (SELECT 10 / w FROM three ORDER BY id)` on `1, 1, 0` fails with 21000: the scalar subquery stops at its second row.
- Not pinned, because the empty side prevented it: a `Hash Right Semi Join` that scans the subquery's table first, with `enable_nestloop` and `enable_mergejoin` off, returned no error on an empty outer table.

The generator (dcf3ba3): `gen.subquery`, `gen.subquerySelect`, and `gen.subqueryWrite` in `/internal/pgdiff/gen.go`, mostly infallible, now and then correlated, nested, failing, over a derived table or a CTE read once or twice, and in UPDATE, DELETE, and INSERT. Statements per case went from 1-10 to 1-12 because two of twelve kinds are new.

Serializability (f46702e): conflicts are at database granularity (`INV-DB-CONFLICTS`), so there is no read set to drop reads from; a commit conflicts with every commit since its snapshot. What can go wrong is a subquery that reads from another snapshot than the one the commit checks. So the mutant is `MUT-SUBQUERY-STALE-READS` (`exec.Mutations.SubqueryStaleAcrossRetries`), which keeps an autocommit statement's uncorrelated subquery results across its retries. The list-append workload through SQL now appends to a packed list with `UPDATE packed SET vals = (SELECT p.vals FROM packed AS p WHERE p.k = $2) || $1 WHERE k = $2` for even values, chosen by the value so that the RNG stream is unchanged. The durability checker finds the mutant at seed 3 without maintainers and seed 1 with two, and `TestMutations` in `/internal/sql/exec/exec_test.go` shows the lost write directly. The kill matrix now marks cells of workloads that never run the mutated code as not applicable (`mutant.workloads`), so the mutant costs four simulator runs instead of 124.

Also (e2cc345): the 10,000-case run found an older bug: Postgres folds UPDATE's SET expressions in column order, because its rewriter sorts the target list, and maragudb folded them in written order (`SET c1 = CAST('' AS bigint), c0 = CAST('NaN'::float8 AS bigint)`: Postgres 22003, maragudb 22P02).

New UNS entries: `UNS-SUBQUERIES` and `UNS-PLAN-DEPENDENT-SUBQUERY`; rewritten: `UNS-STATEMENTS` (WITH is supported), `UNS-CLAUSES` (subqueries supported, set operations in subqueries rejected), and `UNS-JOINS` (subqueries in FROM supported, VALUES and functions not). README and the decision are updated; the requirements already name subqueries and WITH in section 3.

### Why

The rule had to stay the one of the join decision: accept only what every plan agrees on. Postgres has more ways to plan a subquery than a join, and some evaluate a subquery without any row of the query around, so the only sound acceptance is "the subquery evaluated completely does not fail, wherever a plan could look", plus the existing rules for where the query around evaluates it.

### What worked

- select1 to select5 of the corpus, which are full of correlated scalar subqueries, ran with zero mismatches on the first try: select1 went from 506 to 1,031 executed statements, select2 from 500 to 1,031, and select3 from 1,559 to 3,351.
- The full corpus, `make sqllogictest-ratchet` on the code before the review fixes: 5,860,887 of 5,939,714 statements executed, up from 5,763,846, and 35,064 rejected, down from 132,105, in 13 minutes 45 seconds, with zero mismatches. No statement is rejected for a subquery any more (92,443 before). What remains: DROP VIEW 14,582, UNION 10,239, CREATE VIEW 9,724, EXCEPT 249, INTERSECT 232, CREATE TRIGGER 13, numeric literals 13, INSERT ... SELECT 5, REINDEX 2, bit strings 2, order-dependent WHERE 2, DROP TRIGGER 1.
- `make pgdiff PGDIFF_CASES=10000` (after e2cc345): 107,538 of 109,929 statements executed (97.8%), 2,391 rejected, zero mismatches, 50 of them agreeing on 21000. The ratchet rose from 2,932 of 2,981 to 3,187 of 3,273.
- The kill matrix for the new mutant runs in 0.2 seconds.
- `make soak` on the final code (d76e6b1): 22,649 simulator runs in 5 minutes without anomalies, and the full corpus in 12 minutes 41 seconds with the same 5,860,887 executed statements and zero mismatches.

### What didn't work

- My first `SELECT id, (SELECT max(w) FROM sb WHERE aid = sa.id) ...` came back with the column `?column?` where Postgres says `max`: a scalar subquery takes the name of its query's column, which `FigureColname` reads from the analyzed subquery. `outputNameStrength` now gets the names that binding records.
- `WITH c AS (SELECT 1 / 0) SELECT 1` failed with division by zero where Postgres succeeds: the analysis of a CTE returned its folding error. Folding errors inside a subquery are now rejected.
- `SELECT id FROM sa WHERE id = (SELECT aid FROM sb)` was not rejected: the eager evaluation looked at the rows, not at the 21000 that the scalar subquery makes of two of them.
- `SELECT sa.id, sb.id FROM sa JOIN sb ON ... AND EXISTS (...)` was rejected as `a_subquery_in_LIMIT_or_OFFSET_is_not_supported`, because the join rebinds its conditions after the binder's phase has moved to LIMIT; that check moved into `binder.limit`, and the GROUP BY one into `binder.grouping`.
- The first 1,000-case run found `SELECT coalesce(length((SELECT "s2 2".c1 FROM p AS "s2 2"))) FROM (SELECT * FROM t) AS o` failing with 21000 on an empty `o`: `binder.fold` evaluated the call around the uncorrelated subquery at bind time. The `sublink` flag fixes it. The odd alias came from `fmt.Sprint("s", 2, 2)`, which puts a space between two numbers.
- The first ratchet run dropped from 2,932 to 2,890 executed statements, then to 2,914 after tuning the generator, because the new kinds replaced old ones in a fixed number of statements; one to twelve statements per case gives the old kinds their old frequency.
- The mutant's first kill-matrix run said `the list-append checker never detects the mutant`: the lost append shows as a missing acknowledged write, which the durability checker reports.
- A mistake of mine: to check the snapshot-read test I ran `git stash push internal/sql/exec/stmt.go`, which stashed the whole file, and popped it at once.
- Several expectations of mine were wrong and Postgres said so, such as `ORDER BY id` with a subquery column named `id` (42702), and `FROM (SELECT ...) x WHERE id > (SELECT min(id) FROM x)` (42P01: a derived table is no relation in a subquery).

### Self-review

I ran the code-review skill at medium over b55a9d4..HEAD. It probed about 60 queries against Postgres without a difference and reported nine findings. I fixed five in d76e6b1: `Prepare` did not count parameters in GROUP BY and HAVING (an older gap that my rewrite kept, confirmed: `SELECT k FROM u GROUP BY k HAVING count(*) > $1` had 0 parameters); the conversion of an IN subquery's values now counts toward the subquery's own fallibility; a derived table or CTE with a repeated column name is rejected, as for a table; the NULL run counts toward the cap on evaluations; and the outer references are recorded once each instead of once per evaluation. I left four: re-analysis of nested subqueries on every evaluation and the rebinding in `derivedConditions` cost time but not correctness, an aggregate whose argument is an uncorrelated subquery is rejected although Postgres accepts it, which is only stricter, and a redundant check in `binder.limit`, which I removed.

Checks on the final code: `make test`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass. `GOMAXPROCS=4 go test -race -count 1 -shuffle on` per package alone, before (b55a9d4) and after: the root package 37.5 s and 37.6 s, sim 73.1 s and 74.1 s, sqllogictest 52.6 s and 55.1 s, pgdiff 16.2 s and 18.6 s, exec 24.6 s and 25.4 s, parser 8.7 s and 8.8 s.

### What I learned

- Postgres 18 pulls up a correlated IN into a semi-join, which earlier versions did not; the planner comment that only uncorrelated IN is converted is out of date for 18.
- An InitPlan in an index condition is evaluated when the scan starts, also on an empty table, while the same InitPlan in a filter is never evaluated without a row.
- Postgres folds the SET expressions of an UPDATE in the order of the columns, not as written.

### What was tricky

- Name resolution and row layouts across levels: the binder of the query around changes its namespace, layout, and grouping while it binds, so each subquery keeps a snapshot of it, and reads the outer value through the binder that bound it, which also handles grouped rows and merged USING columns.
- Telling the AST helpers about correlation without changing the AST, which prepared statements reuse: the analysis records the outer references by subquery, and the helpers that need them look them up.
- Finding which evaluations no plan reaches: rejecting more is always safe, so where the rules of a plan were unclear, the code rejects.

### What warrants review

- `/internal/sql/exec/subquery.go`: `binder.subquery` with its eager evaluation and NULL run, `binder.columnRef`, `binder.evaluator`.
- `/internal/sql/exec/derived.go`: `binder.derived` and `binder.derivedConditions`.
- `/internal/sql/exec/plan.go` `binder.touch`, `/internal/sql/exec/expr.go` `binder.fold` with `sublink`.
- Validate with `go test ./internal/pgdiff -run 'TestSubquer|TestCTEs|TestUnsupported'`, `make pgdiff PGDIFF_CASES=10000`, and `make sqllogictest`.

### Future work

- Bind a correlated subquery's query once and run it many times; each evaluation now rebinds it and analyzes its nested subqueries again.
- A correlated subquery that can fail in a join is checked by evaluating it on every combination of candidate rows, capped at 20,000 evaluations; a check of its tables joined with the query around, as the join check does, would accept more.
- Section 3 is complete; views, UNION, and INSERT ... SELECT are what the corpus rejects now.
