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
