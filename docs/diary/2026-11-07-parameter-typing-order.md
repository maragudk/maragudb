# Diary: Type parameters in the order Postgres analyzes them

After the work on pgdiff mismatches, a parameter that gets a type in one context and none in another fails with `ErrUnsupported`. That rule also refuses `WHERE k = $1 OR $1 IS NULL`. It is a common way to write an optional filter, and Postgres accepts it. The goal is to accept every shape like it that Postgres accepts, and to give Postgres's result where Postgres fails.

## Step 1: Set the requirement and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** The refusal is legal under the subset contract, but it costs users a common idiom. Fix it now that CI time is under control.
**Inferred intent:** The subset should be as large as Postgres's rules allow when the cost is reasonable, especially for idioms that applications use every day.

### What I did
I wrote the requirement and gave it to the builder `parameter-typing`.
- **Measure Postgres first.** Postgres types a parameter in the first context that coerces it, in the order that it analyzes the statement. A later context then sees the type that was already set. For example, `$1 IS NULL OR k = $1` fails with 42P08, and `k = $1 OR $1 IS NULL` runs. The builder finds that analysis order for the clauses maragudb supports: the select list, FROM and JOIN ON, WHERE, GROUP BY, HAVING, ORDER BY, LIMIT and OFFSET, subqueries and CTEs, INSERT VALUES, UPDATE SET, and RETURNING.
- **Match it.** Use the same order. A parameter typed before an untyped context is accepted. A parameter used before it has a type gives Postgres's error class. Two different types give Postgres's result, which is either an error class or an implicit cast that Postgres allows.
- **Refuse only the rest.** A case stays `ErrUnsupported` only where matching would cost too much, with the UNS-PARAMETERS entry narrowed to name exactly what is left.
- **Verification.**
  - Tests on both systems for the optional-filter idioms: `k = $1 OR $1 IS NULL`, `($1 IS NULL OR k = $1)` with an explicit cast, and `coalesce($1, k)`.
  - The generator in the differential harness repeats parameters and skips numbers. The earlier builder listed this as a follow-up.
  - Three consecutive `make pgdiff PGDIFF_CASES=10000` runs pass.
  - The ratchet does not drop, and the full corpus still has zero mismatches.

### Why
Applications build optional filters with parameters all the time. A database that refuses them pushes users to write the SQL as text, which is worse for them.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
The order of contexts matters for Postgres's parameter types. It is not enough to collect the types and compare them.

### What was tricky
Nothing yet. The order in which Postgres analyzes the clauses may differ from the order in which they are written.

### What warrants review
That every case refused before is now either accepted with Postgres's result or still refused, and that none is accepted with a different result.

### Future work
None yet.

## Step 2: Type parameters in Postgres's order, and analyze again where a later reference has the type

**Author:** parameter-typing

### Prompt Context

**Verbatim prompt:** You are the builder `parameter-typing` for maragudb (/Users/maragubot/Developer/maragudb; work on main in that checkout; commit to main, do NOT push — the lead pushes). [...] The previous builder made a parameter whose contexts give it different types, or a type in one place and none in another, fail with `ErrUnsupported`. That refuses `WHERE k = $1 OR $1 IS NULL`, a very common optional-filter idiom that Postgres accepts. Postgres types a parameter at the first context that coerces it, in its analysis order; later contexts see that type. [...] Measure Postgres 18 [...] Match it [...] Keep `ErrUnsupported` only where matching is too costly, and narrow UNS-PARAMETERS to name exactly what remains. [...]
**Interpretation:** Find Postgres's analysis order and its per-context behaviour by measurement, reproduce its typing exactly (results and error classes), and keep refusing only the shapes that cannot be placed cheaply; extend the differential generator to repeat and skip parameters.
**Inferred intent:** Accept the everyday optional-filter idioms without giving up the strict-subset guarantee.

### What I did

**Measurements** (Postgres 18 through pgx, with a scratch probe test that ran each statement on both sides and printed the outcome and SQLSTATE; about 300 statements). Postgres's order: the WITH clause, each CTE in order; FROM, with each join's ON after both of its sides and derived tables in place; the output list; WHERE; HAVING; ORDER BY; GROUP BY (after ORDER BY, as in `transformSelectStmt`); DISTINCT; OFFSET; LIMIT; then unknown output columns become text. A subquery is analyzed where it is, and the query of an `IN (subquery)` before its left side. Within an expression operands come before the operator that coerces them, except that AND, OR, NOT and the WHEN of CASE coerce each operand to boolean right after it, and the operand of a simple CASE becomes text right after it. BETWEEN is `x >= a AND x <= b` with x analyzed twice. The left side of `IN (list)` is one node compared with each item, unless two or more items read no columns of the query, which makes an array with one coercion; inside a subquery, a column of the query around counts as such an item (`(SELECT $1 IN (o.c0, NULL))` works, `$1 IN (c0, NULL)` is 42P08). INSERT analyzes each VALUES row, then coerces it, row by row; UPDATE analyzes WHERE before SET, and every SET item before coercing them. A later reference is a typed value: `$1 + 1, $1 + 5000000000` works (integer + bigint), `k = $1 AND s = $1` is 42883, `f = $1 AND k = $1` works, `$1::bigint, $1 || 'x'` gives `1x`, `$1::text, abs($1)` is 42883, `$1::bigint, $1 = 'abc'` is 22P02 at analysis. A reference made before typing and coerced to another type, or read in `IS NULL` or `count()`, is 42P08; `count(DISTINCT $1)` types text; `+'2'` is double precision 2 and `+NULL` is NULL.

**Code.**
- `/internal/sql/exec/param.go` (new; the ledger moved here from expr.go): `paramOrder` walks the statement at Prepare in that order and gives each reference a creation and a coercion position, each expression a position, marks list-IN left sides, and sets `ast.Between.Copy`, a copy of a BETWEEN operand with parameters, which `desugar` uses for the second comparison. `paramLedger.typing` replays Postgres's typing over the binder's records (`paramRef`: types, untyped, pretyped); `check` returns `errRetype` with `next` when a reference made after typing got another type, 42P08 as `ErrType`, the conservative refusal for parameters the order cannot place (`loose`), and 42P18. `failed` handles a statement whose analysis failed, using the error's position (`located`, set by `binder.bind` and `binder.locate`) to decide whether a 42P08 comes first and whether unreached references could matter.
- `/internal/sql/exec/session.go`: `Session.run` loops, analyzing again with the ledger's `next`, at most once per reference.
- `/internal/sql/exec/expr.go`: `resolve` defers every conversion error of a parameter (unsupported Go conversions too) so that only the final type's conversion is reported; `bind` binds a pretyped reference with its type; IS NULL of a parameter reads only its nullness; unary plus of unknown is double precision; CASE evaluates its operand before its WHEN values (found by pgdiff, pre-existing); errors of WHEN, AND/OR/NOT, WHERE, HAVING, LIMIT and ORDER/GROUP BY items are located.
- `/internal/sql/exec/group.go`: count() of a parameter is a constant of its nullness, count(DISTINCT) types text; ungrouped-column errors carry `errUngrouped` and get no position, as Postgres reports them at the end of the query.
- `/internal/sql/exec/subquery.go`: the left side of an IN with a subquery, in WHERE or ON and not NOT IN, is evaluated also on rows for which the subquery is empty, and fails there as plan-dependent (pgdiff found this, pre-existing; EXPLAIN shows `Hash Cond: ((SubPlan 3) = s20.c0)`).
- `/internal/sql/exec/plan.go`: a WHERE decided by a NULL conjunct drops the subquery's references to the query around (EXPLAIN: `One-Time Filter: false`), and `negate` keeps `Between.Copy`.
- `/internal/pgdiff/gen.go`: parameters in expression leaves (mostly the text of a value, sometimes NULL), repeats of earlier parameters, the optional filter in both orders, and skipped numbers (a `skipped` value that `add` accepts unreferenced), all drawn from `gen.aux`, a stream of its own seeded by the schema for a `RandSource`, so the fixed-seed ratchet's other draws stay as they were.
- `/internal/pgdiff/classes.go`: 42P08 in the type class.
- Tests: `TestParameters` gets about 100 deterministic both-sides cases and a hegel property over SELECTs with parameters across clauses; `TestErrorClasses` two 42P08 cases; `TestUnsupported` the remaining UNS-PARAMETERS shapes, the IN-left and NULL-conjunct UNS-PLAN-DEPENDENT-SUBQUERY cases; `TestSession_Select` an ordering test and a CASE operand test that need no Postgres.
- Docs: a writer sub-agent drafted UNS-PARAMETERS, the UNS-PLAN-DEPENDENT-SUBQUERY additions, the INV-PLACEHOLDERS sentence, the README paragraph and the decision; I reviewed them, fixed "the parser's tree" (the walk sets the copy) and `count()` without DISTINCT, and added the later fixes.

### Why
Matching the order exactly needs the binder's per-context types and Postgres's positions; the binder's clause order differs from Postgres's and it binds expressions several times for planning, so the order is computed separately and replayed, and the binder only has to be rerun in the uncommon case where a later reference has another type than its context gives.

### What worked
- The model (create position against the first coercion's position) predicted every probe, including outputs typed at the end of the query, `ORDER BY 1` typing an output early (`SELECT $1 FROM dev ORDER BY 1 LIMIT $1` is 42804), and HAVING before GROUP BY.
- Red checks: walking WHERE before the output list fails the new property, 4 deterministic cases, and the 3000-case harness (`SELECT DISTINCT $1 FROM t WHERE $1`: Postgres 42P08, maragudb 0 rows); switching off the second analysis fails all three parameter properties and the harness (`UPDATE t SET c0 = $1, ts = DEFAULT WHERE $1`); the exec ordering test fails with the wrong order too.
- Keeping the generator's main stream unchanged kept the ratchet's 3,328 statements identical, so the executed count measures only the effect of the new parameters: 3,230 (from 3,228).

### What didn't work
- My first probe table used `integer` columns: `unsupported at position 42: integer is not supported, use bigint`.
- IS NULL resolved a Go int64 to text before: `unsupported: parameter $1 is of type text here, and a Go int64 is not converted to it`, which hid most measurements until IS NULL stopped converting.
- First generator version drew from the main stream: the ratchet fell from 3,228 to 3,133 because the cases changed (3,239 statements instead of 3,328), not because anything was rejected.
- Go-typed values in expression parameters were rejected as text conversions (`parameter $1 is of type text here, and a Go bool is not converted to it`), so expression parameters carry text or NULL.
- `failed` first treated a reached reference with no type yet as unbound, which made `SELECT -$1 ...` style errors unsupported; and it located errors of rewritten nodes (conjuncts, desugared IN) nowhere. The fix places a rewritten node at the smallest written expression that holds its subexpressions, which for an IN is the IN, as Postgres compares only after all items (`'\x'::bytea IN ('\x' || $2, CAST($1 AS bytea))`: Postgres 42846, maragudb had `bytea = text`).
- 10,000-case runs failed six times before three passed: `count(DISTINCT $1)` (Postgres text, maragudb 42P18); `+$1` (`operator is not unique: + unknown`); `coalesce(CAST($1 AS bigint), CAST(-$1 AS bigint))` (pending reference); the grouping error position; `(SELECT coalesce($1 IN (o.c0, NULL)) ...)` (array IN in a subquery); and three pre-existing ones without parameters, reproduced on a worktree of 7c7b99f: `10 / s11.c1` in a correlated IN's left side (22012 vs 0 rows), the NULL conjunct (21000 vs 0 rows), and the CASE operand (22003 vs 22012).

### What I learned
Postgres's hook scribbles the type onto the Param node, so a reference made after typing is a typed node, while one made before stays unknown until its own coercion; every rule above follows from where the nodes are created and coerced.

### What was tricky
Error order: an analysis error in maragudb's binder order is compared against Postgres's positions, and errors raised outside `bind` or on rewritten expressions have no natural position. Grouping errors have none in Postgres either.

### What warrants review
- `/internal/sql/exec/param.go`: `paramOrder.expr` and `query` against the order above; `typing` (first, after, pending, lists); `failed` and `unboundBefore`; `enclosing`.
- `/internal/sql/exec/session.go` `Session.run`: the loop discards writes and the subquery cache between analyses.
- Validate with `go test ./internal/pgdiff -run 'TestParameters|TestUnsupported|TestErrorClasses'` and `make pgdiff PGDIFF_CASES=10000`.
- Self-review (code-review skill at medium) found ten items. Fixed: `negate` dropped `Between.Copy`; the IN-left check also hit NOT IN (now only IN; IN under OR stays rejected, which is allowed and documented); the count(DISTINCT) comment; the order walk now runs only for statements with parameters. Left: the last-resort check can rerun a statement that already executed, for statements that never check at folding (none of the parameter statements that reach it today; writes are discarded); `Between.Copy` is a field that new code building a Between must carry (only `negate` does); a BETWEEN operand with a subquery gets no copy and stays refused; `withAux` copies the generator by value, which is safe because the methods it calls only append through `*g.args`; verification.md is regenerated below.

### Future work
- `x IS NULL::bigint` is a syntax error in maragudb's parser, where Postgres parses `(x IS NULL)::bigint`.
- An IN under OR whose left side can fail is rejected although Postgres keeps it a subplan; telling top-level conjuncts apart would accept it.
- Statement-level errors raised outside `bind` (VALUES column types, DISTINCT/ORDER BY checks) have no position, so a statement with repeated parameters that fails there is rejected.
