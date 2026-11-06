# Diary: Section 3, part 2: GROUP BY, HAVING and aggregates

Section 3 of `/docs/requirements.md` asks for `GROUP BY`, `HAVING`, and `COUNT`, `SUM`, `AVG`, `MIN` and `MAX`. Part 1 (`/docs/diary/2026-11-06-section-3-joins.md`) added joins, aliases and DISTINCT. In the sqllogictest corpus, aggregates are now the largest rejection reason.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Part 1 of section 3 is pushed, so the aggregates come next. The lead makes the scope calls.
**Inferred intent:** Widen the SQL surface without weakening the guarantee that every accepted statement gives the same result as Postgres.

### What I did
I wrote the scope and handed it to the builder `section3-aggregates`. Two problems decide the shape of this part:
- **Result types.** In Postgres, `SUM(bigint)` and `AVG(bigint)` return `numeric`, a type that maragudb does not have. Rejecting them would refuse the most common aggregates in an app. My call: support `numeric` only as the result of these aggregates. It holds exact decimal values, with Postgres's scale rules, and reaches Go as a string, as Postgres drivers give it. Comparisons, ORDER BY, and casts to `bigint`, `double precision` and `text` are supported. Arithmetic on it, and numeric columns, stay UNS. This is a decision entry in `/docs/decisions.md`.
- **Order of floating-point sums.** `SUM` and `AVG` over `double precision` add in the order that Postgres reads the rows, and the plan decides that order. Floating-point addition is not associative, so the last bits can differ. My call: accept a float sum only when its result does not depend on the order of addition, checked at run time, for example when every partial sum is exact. Otherwise return ErrUnsupported.

Other calls: `COUNT(*)`, `COUNT(x)`, and `DISTINCT` inside an aggregate are in scope. `FILTER`, `ORDER BY` inside an aggregate, `GROUPING SETS`, `ROLLUP` and `CUBE` are UNS. Aggregates over joins work as for one table.

### Why
`COUNT`, `SUM` and `GROUP BY` are in almost every reporting query that an OLTP app makes.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Most of the difficulty lies in the result types and in the plan's row order, not in grouping.

### What was tricky
`AVG(bigint)` returns numeric with a scale that Postgres chooses in its division. It must be reproduced exactly.

### What warrants review
The numeric output rules, and the run-time order check for float sums.

### Future work
Part 3: subqueries and non-recursive CTEs.

## Step 2: GROUP BY, HAVING, the aggregates, numeric results, and cheaper tests

**Author:** section3-aggregates

### Prompt Context

**Verbatim prompt:** You are the builder `section3-aggregates` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). [...] ## Goal Section 3, part 2: GROUP BY, HAVING, and COUNT, SUM, AVG, MIN and MAX, with every accepted statement giving the same result as Postgres 18, over one table and over joins. ## Requirements 1. **SQL surface:** [...] 2. **numeric as a result type only** (my decision). [...] 3. **Float sums and plan order.** [...] 4. **Other plan dependence.** [...] 5. **Differential testing.** [...] 6. **Simulator.** [...] 7. **Docs.** [...] 8. **Test time.** [...] Mid-run, the lead added: CI is now very close to the 10-minute per-package timeout [...] cut the root package and internal/sqllogictest significantly, aiming for each under about 90 s locally with `GOMAXPROCS=4 go test -race`. Don't weaken any check. [...] Commit this separately, and report before/after timings.
**Interpretation:** Add grouping and the five aggregates with Postgres's analysis, errors, and results, numeric only as the result of SUM and AVG of integers, accept float sums only when their order cannot matter, find on Postgres 18 where grouping makes outcomes plan-dependent and reject those, and extend the generator, the corpus, the simulator, and the docs. First, cut the test time of the root package and the corpus subset.
**Inferred intent:** Open up the reporting queries apps write, which are the largest remaining rejection in the corpus, while every accepted statement still matches Postgres in every plan, and keep CI inside its timeout.

### What I did

The test time came first (c3245ff). Profiling `TestRequestBudgets_Indexes` showed that every hit of the memory cache decoded its 16 KiB block again, about 400 allocations per block and dozens of blocks per unique check, which `-race` makes about 15 times slower. `cache.Memory` now keeps a parsed value next to the bytes of an entry (`GetParsed`, `SetParsed`), dropped when the entry is replaced or evicted and not counted in its bytes, and `DB.readBlock` in `/internal/engine/blocks.go` keeps the decoded block there, so the hits and misses that `Stats` counts are unchanged. The test then loads each size once and copies the fake store for each subtest (`world.clone`, `loadedWorlds` in `/maragudb_test.go`); its update subtest warms the leaves that the measured update reads, and asserts zero GETs, which it observed before but did not assert. In the corpus subset, `index/delete/10/slt_good_0.test` took 101 s of 110: its 6,500 commits fed a garbage list that GC never emptied within the one-hour grace period, so every commit encoded a larger manifest. The subset now runs with maintenance every 20 ms and a grace period of 5 s (`openMaragudb` in `/internal/sqllogictest/run_test.go`). `GOMAXPROCS=4 go test -race`: the root package 168 s to 38 s (`TestRequestBudgets_Indexes` 145 s to 15 s), sqllogictest 110 s to 44 s.

Then the feature (725c580, fixed in d7f0316):
- `/internal/sql/value/decimal.go`: `value.Decimal`, Postgres's numeric as a value, held as its output text, compared by value whatever its scale. `DecimalDiv` ports `select_div_scale` and rounds half away from zero; `DecimalToInt` and `DecimalToFloat` are the casts.
- `/internal/sql/ast` and `/internal/sql/parser`: `Select.GroupBy` and `Having`, `Call.Star` and `Distinct`, `ALL` dropped as Postgres does, NULLIF as a special form with exactly two arguments, and UNS for ROLLUP, CUBE, GROUPING SETS, `()`, `GROUP BY ALL` and `DISTINCT`, and ORDER BY in an aggregate.
- `/internal/sql/exec/group.go`: `selectGrouped` follows Postgres's analysis order: every clause on its own (output, WHERE, HAVING, ORDER BY), then GROUP BY (a position, a FROM column before an output name, or an expression), then the check of `parseCheckAggregates`, which binds output, ORDER BY, and HAVING in group rows, where a subexpression equal to a GROUP BY expression reads its slot, an aggregate reads its own arguments, and a column is 42803 unless the GROUP BY has its table's primary key. Folding errors keep Postgres's phases, with `phaseHaving` between WHERE and OFFSET. HAVING conjuncts without a live aggregate after folding move into WHERE, as `subquery_planner` does, and without GROUP BY also stay.
- Cexprs now carry `aggs`, the aggregates they read, so an aggregate that folding drops, such as in `CASE WHEN false THEN sum(10 / x) END` or `sum(x) = NULL`, is never computed, as Postgres computes only the Aggrefs left after `preprocess_expression`.
- `/internal/sql/exec/aggregate.go`: `aggRun` groups the rows (also those of a join, from `selectJoin`), collects every failure of a WHERE condition, GROUP BY expression, argument, HAVING, and output, and reports one only when all have the same class. `orderFreeSum` decides whether a float SUM or AVG is the same in every order.

The rules, from experiments on Postgres 18 that `TestAggregatePlans` in `/internal/pgdiff/aggregates_test.go` pins:
- `HAVING g > 1 AND count(*) > 0` puts `g > 1` into the index scan as `Index Cond`.
- `SELECT g, sum(10 / x) ... GROUP BY g ORDER BY g LIMIT 1` is Limit over GroupAggregate over an index scan, and succeeds where the query without LIMIT divides by zero. So LIMIT on groups needs ORDER BY on every GROUP BY expression and nothing that can fail.
- `SELECT min(id) FROM dev WHERE 10 / x > 0` is an InitPlan with Limit over the primary key index, and succeeds where `min(id), count(*)` fails. So a WHERE failure in a one-table query whose aggregates are all MIN and MAX is rejected (`UNS-PLAN-DEPENDENT-AGGREGATE`).
- `enable_hashagg` and `enable_sort` switch between GroupAggregate and HashAggregate, and the SUM of 1, 1e16, -1e16 is 0 or 1 by the physical order of the rows. `avg` of 1e200 and -1e200 fails with 22003 although the sum does not overflow, because `float8_accum` also adds the squares of the deviations; after an infinity, the sum never overflows.
- `min(boolean)` does not exist (42883), `sum('1')` is 42725, `count()` and `now(*)` are 42809, `abs(DISTINCT s)` is 42883.

`orderFreeSum` accepts a float sum when every value and every partial sum is exact: in the unit of the smallest lowest set bit, the magnitudes add up to at most 2^53. NaN and infinities are accepted when the finite values cannot overflow, and AVG additionally bounds the squares by `n (M n + S)^2 < 2^1000`. A property test in `/internal/sql/exec/aggregate_internal_test.go` adds the accepted values in random orders with a port of `float8pl` and `float8_accum` and finds the same bits and no overflow.

The generator (b03e1d1): `gen.groupSelect` writes GROUP BY of columns, expressions, positions, and primary keys, aggregates in the output, HAVING, and ORDER BY, with DISTINCT, over the table and over joins through `gen.joinFrom`, which I split out of `joinSelect`, with failing arguments, failing HAVING, a column outside the GROUP BY, numeric arithmetic, and LIMIT with every key in ORDER BY. The generator's float values are often multiples of 1/4 and sometimes random or special, so float sums go both ways. `FullyOrdered` treats a grouped query as ordered when ORDER BY sorts by every GROUP BY item. Statements per case went from 1-8 to 1-10.

The simulator's bank audits through SQL now also read `SELECT sum(balance), count(*)` in the same snapshot and compare it with the rows (fe889fc), which exercises the numeric text through the driver. The second read made the freshness mutant detectable by the grace period checker in the bank workload through SQL with maintainers, so that cell is no longer listed as undetectable.

New UNS entries: `UNS-AGGREGATES`, `UNS-FLOAT-SUM`, and `UNS-PLAN-DEPENDENT-AGGREGATE`. Widened: `UNS-NUMERIC` (arithmetic on numeric, quoted and numeric literals next to it), `UNS-DISTINCT-ZERO` (numerics of different scales, GROUP BY, MIN, MAX, and SUM(DISTINCT) of -0 and 0), `UNS-LIMIT-ORDER`, `UNS-PLAN-DEPENDENT-LIMIT`, `UNS-ORDER-DEPENDENT-WHERE` (HAVING), `UNS-CLAUSES`, and `UNS-FUNCTIONS`. No DEV entry. The decision entry, the requirements sentence, and the README were drafted by a writer agent from my brief, and I checked them against the tests.

### Why

The plan rules had to stay those of the earlier parts: accept a statement only when every plan Postgres could pick gives the same outcome. Grouping adds two new orders, hashed and sorted, and an early stop under LIMIT, so the only safe error to report is one whose class every failure shares, and the only safe float sum is one that is exact in every order.

### What worked

- The full corpus, with zero mismatches: 5,709,337 of 5,939,714 statements executed, up from 4,730,139, and 186,614 rejected, down from 1,165,812. No rejection for count, sum, avg, min, max, GROUP BY, HAVING, or nullif remains; 54,509 statements are rejected for arithmetic on numeric, such as `- SUM(col)`, which is the largest aggregate-related rejection left.
- `make pgdiff PGDIFF_CASES=10000`: 98,447 of 100,486 statements executed, 2,039 rejected, zero mismatches. The ratchet rose from 2,577 to 2,930 executed statements, of 2,981.
- `make soak` passed: 22,975 simulator runs in 5 minutes without anomalies, and the full corpus in 12 minutes 15 seconds with zero mismatches and 5,709,337 executed statements.
- pgx returns numeric as a string with the scale Postgres prints, so the harness compares numeric results as text without any change.

### What didn't work

- The first grouped test hung for 180 s: `groupKey.ce` was nil when `groupedExpr` read its type, the panic left the pgx connection locked, and only `-timeout 40s` showed the stack. The keys are now bound in `grouping` before the output.
- A heavy pgdiff run hung the same way on `SELECT sum(DISTINCT c1) <> CAST(...) ... GROUP BY c1`: a rejected float sum left a zero `value.Value` in the group row, which `value.Compare` panics on. A rejected aggregate now leaves a NULL.
- The first full corpus run had two mismatches. `HAVING NOT + - COALESCE(- + 65, + col0, + COUNT(*) / + 91) <= ...` moved into WHERE, where binding met the folded-away COUNT and raised 42803; the moved conjuncts now bind with aggregates allowed. And select4 failed with `ErrSnapshotExpired` after 5 s, so the full corpus keeps the one-hour grace period and only the subset uses 5 s.
- `SELECT DISTINCT g FROM ga GROUP BY FALSE, g ORDER BY v` gave 42P10 where Postgres gives 42601, because Postgres checks DISTINCT's ORDER BY after GROUP BY.
- A fifth of my own expectations in the new tests were wrong and Postgres said so, such as `SELECT v AS g ... GROUP BY g`, which groups by the column g and fails on v.

### Self-review

I ran the code-review skill at medium over c3245ff..HEAD. It reported ten findings, unverified. I fixed eight in d7f0316 and the subset commit: `count()` and `now(*)` are 42809 and `abs(DISTINCT s)` 42883, as Postgres finds the function first; a numeric argument of an aggregate is rejected instead of adding as 0 (unreachable today, because aggregates do not nest); ambiguity is a sentinel error instead of a string match; `cmpOr` is `cmp.Or`; an integer sum adds in an int64 until it overflows instead of allocating a big.Int per row; the canonical text of a subexpression is computed once per grouped query; the numeric property test runs through `ht`, and checks that every quotient is within half a unit of its last digit with at least 16 significant digits; and the subset's name is a constant. I left two. A grouped query in which one row's WHERE outcome depends on the order of the conditions is rejected even when another row fails for certain, which is stricter than the single-table rule but never wrong, because the order-dependent row can fail with another class in some orders, and the key-order rerun of the harness cannot reproduce a sorted grouping. And `length(DISTINCT 'a', 'b')` is 42883 where Postgres finds `length(bytea, name)` and reports 42809; neither the generator nor the corpus writes it.

Checks: `make test`, `make lint` (0 issues), `make verification` (no diff), `go mod tidy` (no diff). `GOMAXPROCS=4 go test -race -count 1` per package alone: the root package 38 s, sim 73 s, sqllogictest 52 s with the aggregates and groupby files added to its subset, exec 26 s, engine 23 s, pgdiff 16 s. `GOMAXPROCS=4 go test -race -shuffle on ./...` takes 1:50 wall, down from 2:58, with sim at 92 s, sqllogictest at 69 s, and the root package at 39 s in that mix.

### What I learned

- Postgres moves HAVING into WHERE after it folds constants, so a conjunct whose aggregate folds away moves too, and it computes only the aggregates left after folding.
- `float8_accum` makes AVG of double precision fail in cases where SUM succeeds, through its sum of squares.
- `float8smaller` and `float8larger` keep the later of two equal values, which makes MIN and MAX of -0 and 0 depend on the order of the rows; the comment in Postgres's `planagg.c` says the same about numerics of different scales.

### What was tricky

- The circular analysis: GROUP BY refers to output columns, and the output is checked against GROUP BY. The output is bound twice, once for its types and errors in Postgres's order, and once in the group rows.
- Error order across plans. In hashed grouping every argument is evaluated before any HAVING; in sorted grouping every GROUP BY expression comes first and each group finishes in turn. Collecting every failure and comparing classes was simpler and sound.

### What warrants review

- `/internal/sql/exec/group.go`: `selectGrouped`, `grouping`, `groupedExpr`, `having`.
- `/internal/sql/exec/aggregate.go`: `aggRun.rows`, `orderFreeSum`, `exactSum`.
- `/internal/sql/value/decimal.go`: `DecimalDiv`.
- Validate with `go test ./internal/pgdiff -run 'TestAggregates|TestGroupBy|TestAggregatePlans|TestUnsupported'`, `make pgdiff PGDIFF_CASES=10000`, and `make sqllogictest`.

### Future work

- Arithmetic on numeric: 54,509 corpus statements, mostly a sign or an integer added to a SUM. Addition, subtraction, negation, and multiplication are exact with simple scale rules, and division already exists.
- The garbage list of the manifest grows with every compaction for the whole grace period, so a busy database encodes a large manifest on every commit; the corpus showed it.
- Subqueries are the largest rejection left (92,443), for part 3.
