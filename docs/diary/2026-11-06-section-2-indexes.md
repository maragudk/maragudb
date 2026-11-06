# Diary: Section 2, part 1: indexes and UNIQUE

Section 2 of `/docs/requirements.md` adds integrity features. This part covers secondary indexes: `CREATE INDEX` and `DROP INDEX`, `UNIQUE` indexes and `UNIQUE` constraints, and the planner's use of indexes for lookups and range scans. `DEFAULT`, `CHECK` and `FOREIGN KEY` are part 2, for a later builder.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Markus leaves the product calls to the lead. After the Postgres subset, the corpus and the index-plan rule, the next step in the plan is section 2.
**Inferred intent:** Keep moving through the sections, with the same automated evidence for each feature as for section 1.

### What I did
I split section 2 in two, because indexes change the storage layout, the planner and the plan-dependence rules, while DEFAULT, CHECK and FK mostly build on top of them. I wrote the scope and verification for part 1 and handed it to the builder `section2-indexes`. The full brief is in the spawn prompt; its main points:
- Postgres 18 syntax and semantics for `CREATE [UNIQUE] INDEX`, `DROP INDEX` and table and column `UNIQUE`, including Postgres's index names, its shared namespace for tables and indexes, and NULLS DISTINCT. Everything else, such as expression indexes, partial indexes and CONCURRENTLY, is rejected with ErrUnsupported and listed in `/docs/unsupported.md`.
- Index entries are ordinary keys in the existing key space, written in the same commit as the row.
- New invariants: every index always matches the table it indexes, and UNIQUE holds under concurrent writers and faults. Both are checked in the simulator, and each has a mutant that the checkers must detect.
- The request budget for a cold point lookup through a secondary index is O(log n) GETs.
- The pgdiff generator writes indexes and UNIQUE. The sqllogictest corpus stops dropping CREATE INDEX and stops skipping UNIQUE tables, and both ratchets go up.

### Why
Indexes are the base for the rest of section 2: UNIQUE and FK checks need fast lookups.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
With a secondary index, Postgres with sequential scans off can still choose between the primary key and the secondary index. So which conditions act as index conditions can depend on the plan again, and the index-plan rule from `/docs/decisions.md` (2026-11-04) must be extended with care.

### What was tricky
Postgres's unique checks and their error order follow the order rows are written, so a statement that inserts or updates many rows can report a different row's violation, depending on order.

### What warrants review
The index-consistency invariant and its mutant, and how the plan rule treats several usable indexes.

### Future work
Part 2: DEFAULT (including `now()`), CHECK and enforced FOREIGN KEY.

## Step 2: Indexes, UNIQUE, the plan rule for several indexes, and their evidence

**Author:** section2-indexes

### Prompt Context

**Verbatim prompt:** You are the builder `section2-indexes` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). Commit directly to main, with no PRs and no pushing; I push after I verify. Load the fabrik:go skill before you touch Go. [...] ## Goal Section 2, part 1: secondary indexes and UNIQUE, with the same automated evidence as section 1. DEFAULT, CHECK and FOREIGN KEY are out of scope; a later builder does them. ## Requirements 1. **SQL surface (Postgres 18 syntax and semantics):** [...] 2. **Storage.** [...] 3. **Planner.** [...] 4. **Invariants and mutants (verification map).** [...] 5. **Request budgets.** [...] 6. **Differential testing.** [...] 7. **Docs.** [...] ## Verification (what I check when you report) [...] ## Hard rules [...] Continue the diary at /docs/diary/2026-11-06-section-2-indexes.md from Step 2, with `**Author:** section2-indexes`. Use the fabrik:diary skill. Self-review with the code-review skill at medium when done, and fix what it finds.
**Interpretation:** Build CREATE INDEX, DROP INDEX, and UNIQUE as Postgres 18 has them, keep index entries as ordinary keys in one commit with their rows, use them in the planner without giving up the guarantee that every accepted statement matches Postgres under every plan Postgres could pick, and give it the same evidence as section 1: invariants with checkers and mutants in the simulator, budgets, both differential harnesses, and the docs.
**Inferred intent:** Make section 2 rest on indexes that are provably consistent and a unique constraint that provably holds under concurrency and faults, without new deviations from Postgres.

### What I did

I asked Postgres first, in a scratch database `scratch_idx` of the harness container, with `EXPLAIN (COSTS OFF)` and verbose errors. It answered the naming and namespace questions: `t_pkey`, `t_a_key`, `t_a_b_key`, `t_a_idx`, then `t_a_idx1`, and `t_a_a1_idx` for `(a, a)`; a `UNIQUE` on the same columns as an earlier constraint adds no index and gives its name to the earlier one if that has none, so `CREATE TABLE t (id bigint PRIMARY KEY, CONSTRAINT u UNIQUE (id))` names the primary key index `u`; names are chosen one index at a time, so an unnamed `UNIQUE (a)` takes `t_a_key` before a later `CONSTRAINT t_a_key UNIQUE (b)` fails with 42P07. The errors are 42809 for a SELECT, INSERT, or DROP TABLE of an index (DROP TABLE IF EXISTS too) and DROP INDEX of a table, 42704 for a missing index, 2BP01 for DROP INDEX of a constraint's index, and 23505 for `CREATE UNIQUE INDEX` on repeated rows. Columns are checked before the name, so `CREATE INDEX IF NOT EXISTS t ON t (nope)` fails with 42703. An `UPDATE q SET j = j + 1` on a unique `j` succeeds or fails depending on the physical order, as the documentation says for constraints that are not deferrable.

The pieces, in the order of the commits:
- Syntax: `/internal/sql/ast/ast.go` has `ast.CreateIndex`, `ast.DropIndex`, and `ast.CreateTable.Unique` and `PrimaryKeyName`, rendered in `/internal/sql/ast/render.go`. `/internal/sql/parser/parser.go` parses `CREATE [UNIQUE] INDEX [IF NOT EXISTS] [name] ON t (col [ASC|DESC] [NULLS FIRST|LAST], ...) [NULLS [NOT] DISTINCT]`, `DROP INDEX [IF EXISTS] name`, and column and table `UNIQUE [NULLS [NOT] DISTINCT]` with `CONSTRAINT name`. It rejects as `UNS-INDEX` expressions, `WHERE`, `INCLUDE`, `USING`, `CONCURRENTLY`, `ON ONLY`, operator classes, `COLLATE`, `WITH`, `TABLESPACE`, several names in DROP INDEX, and `DEFERRABLE`, `NOT DEFERRABLE`, and `INITIALLY` on a constraint, which used to be a syntax error after a table-level `PRIMARY KEY (a)`.
- Storage: `/internal/sql/value/value.go` `value.EncodeIndexKey` encodes a column for its direction and NULL placement: NULL is 0x00 first or 0x02 last, and a descending value has its self-delimiting payload inverted. An entry is `i/<index>\x00<columns>`, followed by the row's key unless it is a unique entry, which a unique index has when no column is NULL or it is NULLS NOT DISTINCT. Its value is the row's key, so a unique check is a point read that also sees the statement's pending writes. The catalog in `/internal/sql/exec/catalog.go` keeps the indexes in the table's definition, and every index name, the primary key's too, at `c/<name>` with `IndexOf`, which makes the namespace shared. Names follow Postgres's ChooseRelationName, makeObjectName with its 63-byte shortening, and ChooseIndexColumnNames.
- Writes: `/internal/sql/exec/index.go` has CREATE and DROP INDEX, `exec.Session.writeEntries`, and `exec.CheckIndexes`. `exec.Session.updateKeys` in `/internal/sql/exec/stmt.go` now covers every key constraint, the primary key and the unique indexes. A cycle of rows moving onto each other's keys fails in every order, so it is 23505 now instead of `UNS-KEY-UPDATE-ORDER`; only a chain stays rejected. `UPDATE k SET b = 3 - b`, a swap that `exec_test.go` expected to be rejected, now fails as in Postgres.
- Planner, in `/internal/sql/exec/plan.go`: the certain index conditions of the index-plan step now apply to any indexed column, and `table.candidates` decides which rows can be skipped. That rule is below and in `/docs/decisions.md`. Without a condition that can fail, `table.plan` reads the narrowest index, or one in ORDER BY order, which avoids the sort and stops at the LIMIT. With one, it reads the union of the candidates' rows. Rows are always visited in key order when something can fail.
- Harness: `/internal/pgdiff/run.go` `sides.inKeyOrder` now also marks the table's other indexes invalid in `pg_index` for its rerun. `/internal/pgdiff/gen.go` writes UNIQUE constraints with names that cannot clash, CREATE and DROP INDEX with names that do clash, and key conditions on any index. `/internal/sqllogictest/rewrite.go` no longer drops CREATE INDEX.
- Simulator: `/internal/sim/unique.go` is a workload on `users (id bigint PRIMARY KEY, email text UNIQUE, n bigint NOT NULL)` with an index on `n`, with emails from a pool of `2 * Keys`. Its audits check every snapshot for a repeated email and compare lookups through both indexes, and a scan in the order of the email index, with the table. After every run through SQL, `run.checkIndexes` runs `exec.CheckIndexes` on the store with a fresh, fault-free process. The SQL bank got `CREATE INDEX accounts_balance`, so that every transfer moves entries.

The plan rule. EXPLAIN showed that every certain condition on any column of an index is a condition of every scan of that index, also on a column that is not the first, and that Postgres picks by cost between indexes: `WHERE b = 3 AND id = 3 AND 10 / a > 0` scans `tb`, with `id = 3` as a filter. It also scans a whole index without conditions: `WHERE id < 3 AND 10 / b > 0 ORDER BY a LIMIT 1` scans `ta` in order with `id < 3` as a filter, and `SELECT a FROM t WHERE 10 / a > 0` does a bitmap scan of all of `ta`. An `OR` over two indexes becomes a BitmapOr. So an index is a candidate when it has a certain condition, or when a condition could use it, also inside an OR, when ORDER BY sorts by its first column or a column equal to it, or when it covers every column of a SELECT. A row is skipped only when every candidate rejects it by one of its certain conditions. If one candidate has none, no row is skipped. A BitmapOr arm reads only rows that its own index's conditions accept, and that index is a candidate itself, so the rule covers it. For a table with only a primary key, the rule gives exactly the old behavior.

### Why

Postgres's choice between indexes depends on statistics, so maragudb models the plans instead of picking one. The rule has the same safety argument as the index-plan step. A row that maragudb reads but some plan skips makes the order-dependence check reject the statement. So only skipping a row that some plan reads is dangerous, and the rule never does that. Visiting rows in key order keeps the error classes comparable with the harness's key-order rerun. The rerun had to set the other indexes aside, because an index scan in Postgres visits rows in index order whatever the physical order.

### What worked

- Results. The sqllogictest corpus ran in full with zero mismatches: 2,779,188 of 5,939,714 statements executed, up from 2,761,861, and 3,116,763 rejected. In `make test`, the subset rose in `index/delete/10/slt_good_0.test` from 9,184 to 10,730 executed statements, partly because its CREATE INDEX now runs. The pgdiff ratchet rose from 1,548 to 1,783 of 1,822 statements. Before the self-review, two heavy runs of 10,000 cases passed with 59,653 of 60,817 and 60,225 of 61,555 statements executed.
- EXPLAIN on a scratch database settled every plan question in minutes, and `TestIndexPlans` in `/internal/pgdiff/indexes_test.go` now asserts its findings against Postgres: the conditions of `dev_ask (a, s, k)` for `a = 1`, `1 < a`, `a IN (1, 2)`, `s >= 'x'`, `k`, `NOT k`, and `a = j AND j = 3`, the choice between `dev_pkey` and `dev_ask` after ANALYZE, and a whole scan of `dev_ask` for `ORDER BY a LIMIT 1`.
- The hegel model test `TestSession_IndexesMatchModel` in `/internal/sql/exec/index_test.go` passed 300 cases on its first run. It runs random inserts, updates, and deletes against a Go model and runs `exec.CheckIndexes` and lookups after every statement.
- The first run of the kill matrix took 5 seconds and showed two strong side effects. With the balance index, the SQL bank now detects `MUT-COMPACTED-AHEAD`, whose resurrected entries no longer match the rows, so its old reason "resurrected balances are a consistent older state" was wrong for that cell. And the unique audits detect `MUT-FRESH-SNAPSHOT`, because its full scan and its lookups see different snapshots.
- New mutants, all detected at seed 1 unless stated otherwise: `MUT-INDEX-STALE-DELETE` in the SQL bank and the unique workload, by index consistency; `MUT-UNIQUE-NO-CHECK` in the unique workload, by unique and index consistency; and `MUT-UNIQUE-OWN-WRITES` in the unique workload at seed 4, by both. The corpus reaches `STATE-UNIQUE-AFTER-CONFLICT` 122 times and `STATE-UNIQUE-VALUE-REUSED` 75 times, with 880 unique violations.

### What didn't work

- The first heavy pgdiff run failed: `UPDATE t SET c0 = 9223372036854775807 / c0` on the rows `-9223372036854775808` and `0` gave `postgres SQLSTATE 23505, maragudb division by zero`. In my generalized `updateKeys`, the key-order simulation counted only the rows without an error as holding their old keys, but row `0` holds key 0 until it is visited and fails. The old code had it right. I fixed it, and `TestSession_Unique` now has the case.
- `TestRequestBudgets_Indexes` took 100 seconds at first, 78% of it in GC. Every unique check descends every segment whose key range spans the index, and the load added 100 segments without compaction. It now compacts every four batches, which brings it to 9 seconds. In steady state, maintenance bounds the segments.
- The first rerun fix, `SET LOCAL enable_indexscan = off` and `enable_indexonlyscan = off`, did not work for `SELECT 10 / n + CAST(s AS bigint) FROM t ORDER BY a`. EXPLAIN still showed `Index Scan using t2_a` with `Disabled: true`, because a sequential scan plus a sort is disabled too, and costs more. Marking the index invalid with `UPDATE pg_index SET indisvalid = false` works. The planner ignores the index, but `INSERT` still failed with 23505 on it, and the rollback restores it.
- The pgdiff ratchet dropped from 1548 to 1488 at first. The generator's index statements replaced others in the random stream. Adding up to two CREATE INDEX after the first insert, instead of in place of other statements, raised it to 1783.
- In `TestUnsupported`, a `REFERENCES dev (i)` example left a foreign key that made `DROP TABLE IF EXISTS dev` fail with `cannot drop table dev because other objects depend on it (SQLSTATE 2BP01)` in every later example; the example references its own table now, and `newBoth` drops `dev_view` too.
- My own test expectations were wrong a few times, and both databases agreed against me each time. For example, `UPDATE ixu SET a = a - 1 WHERE a >= 2` moves 2 onto 1, which stays, so it is 23505 and not a rejection.

### Self-review

I ran the code-review skill at medium over `968f071..HEAD`. It reported seven findings, and I fixed five:
- When an index could stream ORDER BY, `table.plan` always took it, so `SELECT * FROM t WHERE id = 5 ORDER BY b` read every entry of the index on `b` instead of one row. Now the stream is used only when no other index scores higher.
- An index or constraint of more than 32 columns succeeded, where Postgres fails with 54011. It is the new `maragudb.ErrProgramLimit` now, which CREATE INDEX checks after the table and before the columns, and CREATE TABLE after its own name and before each index's name, as Postgres does. `TestErrorClasses` and `TestIndexes` show this on both.
- The unique workload counted an email as freed by an UPDATE or DELETE that changed no row, so `STATE-UNIQUE-VALUE-REUSED` could be reached without any reuse. It checks the affected rows now.
- `exec.Session.scanRows` had two copies of the range-scan loop. They are one loop now.
- `btoi` in the planner duplicated the one in the value package; the score is written without it.

I left two. A loose range on a secondary index reads one row per entry, which is the cost of having no statistics. It is under future work. And tables from before this change have no primary key index name, which only matters for a stored format that is not stable yet.

After the fixes, two more heavy runs of `make pgdiff PGDIFF_CASES=10000` passed: 60,186 of 61,439 and 59,834 of 61,094 statements executed. `make test`, `go test -race -shuffle on ./...`, `make lint` (0 issues), `make verification` (no diff), and `go mod tidy` (no diff) pass. `make soak` passed too: 25,356 simulator runs in 5 minutes without anomalies, and then the full corpus with zero mismatches and the same 2,779,188 executed statements.

### What I learned

- In Postgres, an invalid index (`indisvalid = false`) is invisible to the planner but still maintained and still unique. That is a clean way to take index scans out of a rerun.
- With seqscan off, disabling more plan types does not force an order: Postgres compares the number of disabled nodes and then the costs.
- A secondary index changes which rows a plan reads even when the statement does not mention it, through ORDER BY or an index-only scan. So a candidate can come from the SELECT list.

### What was tricky

- Keeping the plan rule sound with BitmapOr. An OR over two indexes is a scan whose rows neither index's certain conditions describe. The rule works because each arm uses an index that is a candidate by itself, either with its own certain conditions or with none, and then nothing is skipped.
- Generalizing `updateKeys` without changing its old answers. It needed several key constraints at once, a duplicate in every order reported before an order-dependent outcome, cycles, and NULLs that never collide.

### What warrants review

- `/internal/sql/exec/plan.go`: `table.candidates`, `table.couldScan`, `filter.skipped`, `binder.touch`, `table.span` for descending columns, and `table.plan` with `table.orderedScan` and `table.sortsBy`.
- `/internal/sql/exec/stmt.go`: `exec.Session.updateKeys`, `cycle`, `exec.Session.scanRows`, and `exec.Session.rowKeys`.
- `/internal/sql/exec/index.go`: `table.indexConstraints`, `exec.Session.addIndexes`, `exec.Session.writeEntries`, and `exec.CheckIndexes`.
- `/internal/pgdiff/run.go` `sides.inKeyOrder`.
- Validate with `go test ./internal/sql/exec ./internal/pgdiff -run 'Index|Unique|Unsupported|ErrorClasses|Runner'`, `make pgdiff PGDIFF_CASES=10000`, `make sqllogictest`, and `make replay SEED=1 WORKLOAD=unique MUTATION=unique-no-check`.

### Future work

- Tables written before this change have no primary key index name in the catalog. The format is not stable yet, so nothing migrates them.
- A loose range on a secondary index, such as `b >= 0`, reads one row per entry where a scan of the table would be cheaper. Without statistics, maragudb always prefers an index range.
- A unique check decodes a block of every segment whose key range spans the index. With many segments before compaction, that is slow.
- maragudb does not shorten identifiers longer than 63 bytes the way Postgres does. It only shortens the names it generates.
- A mutant where the unique check reads an older snapshot than the transaction's is not a real risk in this design. Every read goes through the transaction's snapshot, and any concurrent commit is a conflict. The real risk was the statement's own pending writes, which a range scan would not see, so that is the mutant.
