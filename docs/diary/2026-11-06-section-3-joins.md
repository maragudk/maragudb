# Diary: Section 3, part 1: joins, aliases and DISTINCT

Section 3 of `/docs/requirements.md` adds queries over more than one table. This part covers table aliases, qualified column names, several tables in FROM, INNER JOIN, LEFT JOIN and DISTINCT. In the sqllogictest corpus, DISTINCT, table aliases and qualified names are the three largest rejection reasons, together about 2.2 million statements. Aggregates with GROUP BY and HAVING are part 2. Subqueries and CTEs are part 3.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Section 2 is pushed, so section 3 comes next. The lead makes the scope calls.
**Inferred intent:** Widen the SQL surface where real queries need it most, without weakening the guarantee that every accepted statement gives the same result as Postgres.

### What I did
I split section 3 into three parts and wrote the scope for part 1. I handed it to the builder `section3-joins`, together with one small fix from section 2: a cascade recursed once per level, so a deep enough chain of foreign keys could exhaust the Go stack and crash the process. My scope calls:
- In scope: aliases (`AS` optional), qualified names `t.c`, `FROM a, b`, `CROSS JOIN`, `[INNER] JOIN ... ON` and `USING`, `LEFT [OUTER] JOIN`, self-joins, `SELECT DISTINCT`, and `t.*`.
- UNS: `RIGHT` and `FULL` joins (not in the requirements), `NATURAL`, `LATERAL`, `DISTINCT ON`, and `UPDATE ... FROM` and `DELETE ... USING`, unless they are cheap to make faithful.
- The main risk is plan dependence again. Postgres chooses the join order and method (nested loop, hash or merge) by cost, and that choice decides which rows an expression that can fail is evaluated on. Turning off sequential scans does not fix it. The rule must stay what it is for one table: maragudb accepts a statement only when every plan Postgres could choose gives the same outcome. Otherwise it rejects the statement statically or checks it at run time.

### Why
Joins are the most common query shape in an OLTP app after point lookups, and they open up most of the corpus.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Most of the corpus rejections that this part removes are for aliases and qualified names, not for joins themselves. Many of them should start to execute after just the name resolution.

### What was tricky
Postgres also chooses between nested loop, hash and merge joins. An ON condition that can fail may run on pairs that another join method never looks at.

### What warrants review
The plan-dependence rule for joins, and the tests that prove it on Postgres 18.

### Future work
Part 2: GROUP BY, HAVING and the aggregates. Part 3: subqueries and non-recursive CTEs.

## Step 2: Joins, aliases, qualified names, and DISTINCT, with a plan rule for joins

**Author:** section3-joins

### Prompt Context

**Verbatim prompt:** You are the builder `section3-joins` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). Commit directly to main, with no PRs and no pushing; I push after I verify. Load the fabrik:go skill before you touch Go. [...] ## Goal Section 3, part 1: table aliases, qualified column names, several tables in FROM, INNER JOIN, LEFT JOIN and DISTINCT, with every accepted statement giving the same result as Postgres 18. Aggregates/GROUP BY and subqueries/CTEs are later parts; keep them UNS. ## Requirements 0. **Small fix first (from section 2).** Foreign key cascades in /internal/sql/exec/foreignkey.go recurse once per level [...] 1. **SQL surface (Postgres 18 semantics):** [...] 2. **Plan dependence.** This is the hard part. [...] 3. **Execution.** [...] 4. **Results.** [...] 5. **Differential testing.** [...] 6. **Simulator.** [...] 7. **Docs.** [...] 8. **Test time.** [...] ## Hard rules [...] Continue the diary from Step 2, with `**Author:** section3-joins`, using the fabrik:diary skill. Self-review with the code-review skill at medium when done, and fix what it finds.
**Interpretation:** Fix the cascade recursion first, then add the FROM clause of Postgres (aliases, qualified names, lists of tables, CROSS, INNER, LEFT joins with ON, USING, NATURAL) and DISTINCT, find out on Postgres 18 how its join plans decide which rows a condition is evaluated on, and accept a join only when no plan could change the outcome, with tests on both systems, the generator, the corpus, a request budget, and the docs.
**Inferred intent:** Open up the query shapes that apps and the corpus use most, without weakening the guarantee that an accepted statement matches Postgres in every plan it could pick.

### What I did

The cascade fix came first (651884e). On Postgres 18, a self-referencing `ON DELETE CASCADE` chain of 100,000 rows deletes in 226 ms without hitting `max_stack_depth`. The reason is in `ri_triggers.c`: `ri_PerformCheck` runs the action's query with `fire_triggers = false`, so the triggers of the rows the action deletes join the queue of the outer statement, behind what is queued there, and the outer loop of `AfterTriggerEndQuery` fires them. So Postgres's order is breadth first, not the depth-first order of the recursion. I built a case where that shows: a parent with a cascading child that a grandchild references with `RESTRICT`, and a second child with `SET NULL` on a `NOT NULL` column. Postgres fails with 23502, the old maragudb with 23001. `Session.fire` in `/internal/sql/exec/foreignkey.go` is now a worklist over `s.events`, and `SET DEFAULT`'s recheck runs right after its update, as `ri_set` does. The 100,000-row chain also showed that every pending write moved half of the sorted pending keys (`slices.Insert`), 1.4 s for the insert; `sortedKeys` in `/internal/sql/exec/session.go` keeps them in halving sorted runs. Tests: `TestForeignKeys` in `/internal/pgdiff/constraints_test.go` (the order case and a 10,000-row chain on both systems), and `TestSession_ForeignKeyCascades` in `/internal/sql/exec/foreignkey_test.go`, which limits the stack to 1 MiB with `debug.SetMaxStack` and crashed the test binary with `fatal error: stack overflow` on the old code.

Then the SQL (d0056a9):
- `/internal/sql/ast`: `ColumnRef.Table`, `SelectItem.Table` for `t.*`, `Select.Distinct`, `Select.From []TableExpr` with `TableRef` (name, alias, column names) and `Join` (kind, `Natural`, `On`, `Using`), and aliases on `Update` and `Delete`. The renderer writes a join on the right side in parentheses.
- `/internal/sql/parser`: `.` is a token now. `tableRef`, `join`, and `tablePrimary` follow Postgres's grammar, including that a join that needs a condition takes the joins before its condition into its right side (`a JOIN b JOIN c ON x ON y`), while `CROSS` and `NATURAL` joins associate to the left.
- `/internal/sql/exec/scope.go`: entries, namespaces, and `scope.resolve`, modeled on Postgres's `ParseNamespaceItem` with its `p_rel_visible` and `p_cols_visible`: the children of a join keep their names visible and lose their columns to the join's item, whose columns are the merged columns of `USING` first, then the left's, then the right's. `binder.from` builds them in Postgres's order of analysis, and binds the conditions of the joins there. `binder.normalize` writes every resolvable reference the same way, so that `t.a` and `a` compare equal in `canonical`, `factorOr`, and the X = X rule, as Postgres's `equal()` on Vars does.
- `/internal/sql/exec/select.go`: the new `selectStmt`, which keeps the single-table path as it was, and `DISTINCT` with its `ORDER BY` rule (42P10) and `distinctRows`.
- `/internal/sql/exec/join.go`: the join path. `joinPlan.place` pushes every conjunct down the join tree as Postgres's `distribute_qual_to_rels` does, into the scan of one entry where it reads one. The rows run as nested loops (`joinRun`), with the items of each tree of inner joins ordered greedily along the conditions, which keeps a 64-table chain of select5 linear, and with index lookups on equalities (`lookupsFor`).

The plan rule, in `joinPlan.candidates` and `joinPlan.check`: before a join runs, every condition that can fail is evaluated on every row and combination of rows that some plan could evaluate it on. The rows of an entry are those that a candidate scan of the several-indexes decision could read, where an index on any column that any condition or `ORDER BY` reads counts as a scan Postgres could pick, because a parameterized index scan in a nested loop, or a merge join, can read the whole index. An entry on the right side of a `LEFT JOIN` also gets a row of NULLs, because a WHERE condition above the join sees it. Each side of a comparison is checked alone (hash and merge keys, index scan parameters), and so is the OR that Postgres's `extract_restriction_or_clauses` takes out for one table. A failure anywhere rejects the statement with `UNS-PLAN-DEPENDENT-JOIN`, and so do more than 100,000 evaluations. When nothing fails, no plan fails, and the join's rows are the same in every plan. The output and `ORDER BY` are evaluated on every row of the join in every plan, so maragudb reports their error when every failing row fails with the same class, and rejects the statement when classes differ, because the harness cannot rerun a join in Postgres's order. A `LIMIT` on a join needs every primary key column of every table in `ORDER BY` and nothing that can fail. The decision is in `/docs/decisions.md`.

The Postgres experiments, in a scratch database of the harness container, which `TestJoinPlans` in `/internal/pgdiff/joins_test.go` pins:
- `SELECT * FROM a JOIN b ON a.id = b.aid WHERE 10 / b.w > 0`, with a row of `b` whose `w` is 0 and whose `aid` matches nothing: a merge join with `Filter: ((10 / w) > 0)` in the scan of `b`, and division by zero; with `enable_mergejoin` and `enable_hashjoin` off, a nested loop that scans `b` first, and division by zero again.
- `ON a.x + 0 = b.y + 0 AND a.z / b.w > 0`: a hash join evaluates the division as `Join Filter` only on pairs with equal keys and succeeds; a nested loop evaluates it on every pair and fails. With `ON a.x = b.y AND ...` the nested loop succeeds too, because Postgres orders the join filter by cost and the cheaper equality comes first.
- With an empty table on one side, `WHERE 10 / (a.z - 1) > 0` succeeds although the condition fails on every row of `a`: Postgres never builds the hash table.
- A condition on the right side of a `LEFT JOIN` goes into its scan; a strict WHERE condition on that side turns the join into an inner join; `IS NULL` keeps it a left join; `a.id = b.aid AND a.id = 1` derives `aid = 1`.
- DISTINCT of 0 and -0, in that physical order, outputs 0 with HashAggregate and with Sort and Unique.

The harnesses keep only `enable_seqscan = off`. The rule does not depend on the join method, so turning methods off would only make the comparison narrower than what Postgres can do.

The generator (022aea7), in `/internal/pgdiff/gen.go`: `gen.joinSelect` writes self-joins, joins with the referenced table on its foreign key, lists of tables, `CROSS`, inner, left, `USING`, and `NATURAL` joins, a third table on either side, qualified names, `x.*`, `DISTINCT`, conditions from `gen.failing` that divide by a column or read text as a bigint, and a `LIMIT` with every key in `ORDER BY`. `gen.aliased` gives single-table statements, updates, and deletes an alias and qualifies their names, now and then with the hidden name. Statements per case went from 1-6 to 1-8, because two of the eight kinds are joins now.

`AC-5.8` is new in `/docs/requirements.md`, with `TestRequestBudgets_Joins` in `/maragudb_test.go`: a join from one parent to its three children through `c_pid` makes 7 to 14 GETs with a cold cache at 100 to 100,000 rows, under a limit of one descent per table definition, parent, index range, and child. The simulator's foreign key workload now also looks for orphans with a `LEFT JOIN ... WHERE p.id IS NULL` per foreign key (`orphan` in `/internal/sim/foreignkey.go`), in every audit and in the final read, next to the existing check; `TestOrphan` shows that it finds each kind.

New UNS entries in `/docs/unsupported.md`: `UNS-JOINS` (RIGHT and FULL joins, LATERAL, aliases for joins and USING, subqueries and functions in FROM, schemas, whole-row references, repeated column aliases, more than 64 tables), `UNS-PLAN-DEPENDENT-JOIN`, and `UNS-DISTINCT-ZERO`; `UNS-CLAUSES` and `UNS-LIMIT-ORDER` are rewritten, and the errors section has the rule for the output of joins. NATURAL was cheap to make faithful, so it is supported. No DEV entry.

### Why

The rule had to stay the one of section 2: accept only what every plan agrees on. For a single table, some row is read by every plan, so a failure there is certain. For a join, the empty-side experiment shows that no row is read by every plan, so the only sound acceptance is "nothing fails anywhere a plan could look", which is an over-approximation that is only ever stricter than Postgres.

### What worked

- The join path catches its own mistakes: the first TestJoinPlans run rejected four statements I had expected to succeed, and in each case maragudb was right, because `jb_aid` makes a parameterized scan of `jb` possible that reads the row where the condition fails.
- `make sqllogictest`, the full corpus, zero mismatches: 4,730,139 of 5,939,714 statements executed, up from 2,779,188, and 1,165,812 rejected, down from 3,116,763, in 13 minutes 17 seconds. No rejection for DISTINCT, aliases, qualified names, or several tables remains, and the corpus has no `UNS-PLAN-DEPENDENT-JOIN` rejection at all: its conditions that can fail never fail on these tables. select5, the join file, went from 704 to 1,436 executed statements and select4 from 1,043 to 2,857. The largest reasons left are aggregates (count alone 516,214), GROUP BY, nullif, and subqueries.
- `make pgdiff PGDIFF_CASES=3000`: 26,522 of 27,011 statements executed, zero mismatches, 55 of them rejected as `UNS-PLAN-DEPENDENT-JOIN`.
- The pgdiff ratchet rose from 2,343 to 2,577 executed statements, of 2,640.

### What didn't work

- My first DISTINCT key panicked in a test that then hung: `value.EncodeKey` has no case for `integer`, the type of `SELECT DISTINCT 1`. The panic left the pgx connection locked, and `go test` sat for two minutes before I ran it with `-timeout 40s` to see the stack. `distinctRows` widens an integer to a bigint first.
- A WHERE condition on the right side of a LEFT JOIN, `WHERE jb.id IS NULL`, returned 4 rows where Postgres returned 2: `place` had attached it to the join node, where `joinRun.item` read it as part of the ON condition. Conditions that filter a left join's rows after the NULLs are now `joinPlan.post`.
- Several of my own expectations in `/internal/pgdiff/joins_test.go` were wrong, and Postgres said so: `a JOIN b USING (s) JOIN c USING (id)` is 42702 because both sides keep their `id`, `SELECT CAST(jb.s AS bigint), 10 / jb.w` fails with one class on every row because each row fails at its first output column, and an `ORDER BY jb.id` alone does not make a `jb LEFT JOIN ja ON ja.id = jb.aid` totally ordered under my rule, although it is in fact.
- The first generator dropped the ratchet from 2,343 to 2,333, because the failing conditions were rejected often; they are rarer now, and each case has more statements.

### Self-review

I ran the code-review skill at medium over `06f12ef..HEAD`. It reported ten findings, unverified, and found no correctness bug in the join executor, the placement of conditions, the NULL rows of a LEFT JOIN, or the worklist. I fixed eight:
- The new `.` token had turned a table name with a schema in INSERT, CREATE TABLE, CREATE INDEX, REFERENCES, and DROP from `ErrUnsupported` into a syntax error. `parser.tableName` rejects it everywhere now, with parser tests and an INSERT example in `UNS-JOINS`.
- The harness never compared the order of a join's rows when ORDER BY named a qualified column, because `sortedColumns` stopped at it. It now maps a qualified ORDER BY column to an output column of the same reference, and `gen.joinSelect` puts those columns in the output when no ORDER BY position would then name another one. The sort-key comparisons of the ratchet run rose from 66 to 85. My first two attempts, prepending and appending the column wherever ORDER BY had one, dropped the ratchet to 2,575 and 2,576: an `ORDER BY 3` that had been 42P10 now named a stable constant, which is rejected.
- A join kept every row of the join, all columns of every entry, and projected afterwards; it projects in the callback now. The groups of a node and their order are cached in `joinPlan.groupAt`, the NULL rows are made once, `binder.forEntry` starts from `binder.fresh`, and `binder.columnsOf` is the one walk over the columns of an entry.
- `t.*` decides with `parser.continuesExpression` whether an expression follows, instead of a list of the keywords that may follow, so `SELECT t.* FROM t UNION ...` reports UNION.

I left two. The finding that a fallible ON condition without columns is never checked is not possible: such a condition is stable, and `binder.fold` already rejects a stable expression that can fail. The k-way merge for `sortedKeys.between` would only matter with many runs and large ranges; the sort is over the keys of one range.

After the review, `TestRequestBudgets_Joins` took 31.6 s of the root package's 188 s under `-race` with `GOMAXPROCS=4`, because each of the 100,000 child inserts checked its foreign key, and every check decodes a 16 KiB block. Most children now have a NULL parent, which MATCH SIMPLE does not check, and still fill the index: 13.6 s.

Final numbers, after the fixes: `make pgdiff PGDIFF_CASES=10000` passed with 88,763 of 90,374 statements executed and 1,611 rejected, 154 of them `UNS-PLAN-DEPENDENT-JOIN`, and zero mismatches. `make soak` passed: 22,493 simulator runs in 5 minutes without anomalies, and the full corpus with zero mismatches and 4,730,139 executed statements. `make test`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass. `GOMAXPROCS=4 go test -race -count 1` per package alone: the root package 168 s (188 s before the budget fix; `TestRequestBudgets_Indexes` is 145 s of it, unchanged), sqllogictest 108 s, sim 73 s, exec 35 s, pgdiff 19 s. `GOMAXPROCS=4 go test -race -shuffle on ./...` takes 2:58 wall, with the root package at 178 s, sqllogictest at 121 s, and sim at 103 s in that mix. The make test subset of the corpus is unchanged; it grew with the statements that now execute but stays at 108 s alone, so I did not rebalance it.

### What I learned

- Postgres fires the triggers of an RI action's query in the outer statement's queue (`fire_triggers = false`), so cascades are breadth first and never recurse in C either.
- Postgres orders the join filter of a nested loop by cost too, so which pairs a failing ON condition is evaluated on depends on the costs of the other conditions as well as on the method.
- The merged column of `USING` is the left column, converted to the common type, for both INNER and LEFT joins, and Postgres finds both sides of every USING column before it checks any type.

### What was tricky

- Finding a sound superset of what any plan evaluates. Pushed-down conditions are evaluated on rows that never join, hash and merge keys on rows alone, ORs leak conditions into scans, and an empty side can stop a scan before it starts. Checking every candidate row and combination, with NULL rows for nullable tables, covers all of them; the cost is a full read of a table whose indexes a join could use, when a condition on it can fail.
- Keeping Postgres's order of errors: FROM and its ON conditions are analyzed before the output, but folded after it, so the binder got a phase between output and WHERE.

### What warrants review

- `/internal/sql/exec/join.go`: `joinPlan.check`, `joinPlan.candidates`, `place` and `descend`, and `joinRun.item` with `post`.
- `/internal/sql/exec/scope.go`: `scope.resolve`, `binder.fromItem`, `binder.using`, `binder.normalize`.
- `/internal/sql/exec/foreignkey.go` `Session.fire` and `/internal/sql/exec/session.go` `sortedKeys`.
- Validate with `go test ./internal/pgdiff -run 'TestJoins|TestJoinPlans|TestForeignKeys|TestUnsupported'`, `make pgdiff PGDIFF_CASES=10000`, and `make sqllogictest`.

### Future work

- A condition that can fail on a table of a join that has an index on a joined column makes maragudb read the whole table to check it, and rejects the statement above 100,000 rows. Knowing which parameterized scans Postgres can build would narrow that.
- `ORDER BY` with LIMIT on a join requires every key of every table, also of a table whose key the join fixes, such as the parent of a foreign key.
- RIGHT and FULL joins, and aliases for joins.
