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
