# Diary: A strict subset of Postgres

maragudb's SQL dialect changes from "our own flavor with documented deviations" to a strict subset of Postgres 18: the same SQL text runs on both systems with identical results, and where maragudb is stricter it says so with `ErrUnsupported`.

## Step 1: The Postgres subset

**Author:** pg-subset

### Prompt Context

**Verbatim prompt:** You are switching maragudb's SQL dialect from "our own flavor with documented deviations" to a strict subset of Postgres. [...] ## The contract to implement 1. maragudb accepts a subset of Postgres 18 SQL, and the same SQL text runs on both systems. 2. For every statement maragudb accepts, the result is identical to Postgres: rows, values, column names, affected-row counts, and transaction state. 3. If Postgres fails a statement, maragudb fails it too. The error class must match wherever the error does not depend on evaluation order or physical row order. 4. maragudb may be stricter than Postgres. It may reject a statement that Postgres accepts, but only with a new explicit `ErrUnsupported` error (SQLSTATE 0A000, feature_not_supported) [...] 5. Irreducible semantic differences should be zero. [...] ## What to change - **Types.** Use Postgres names only [...] - **Differential harness.** Remove the dual-dialect renderer. [...] When maragudb returns `ErrUnsupported`, skip the statement on Postgres too [...] Add a ratchet [...] - **Each current deviation, `DEV-1` to `DEV-19`.** For each one, either eliminate it by matching Postgres exactly, or turn it into an explicit `ErrUnsupported` rejection. [...] - **Docs.** Replace /docs/deviations.md with /docs/unsupported.md. [...] - **Public API.** Export `maragudb.ErrUnsupported` [...] - **Simulator workloads.** [...] give the real-store SQL list-append workload in /internal/workload the same packed-list updates [...] - **CLAUDE.md.** Add `make verification` and `make report` to its Commands list.

**Interpretation:** Replace the dialect, not the engine: Postgres type names, Postgres literal typing, Postgres casts and output formats, Postgres transaction state after an error, and Postgres's evaluation order where it is defined. Where Postgres's result depends on its planner or on physical row order, reject the statement with `ErrUnsupported` rather than return something that might differ. Then prove it with one SQL text run on both systems, with the explicit rejections counted and ratcheted.

**Inferred intent:** A reader should be able to take any statement maragudb accepts, run it on Postgres, and get the same answer, so that Postgres knowledge, tools, and test corpora carry over, and so that the list of things to trust separately shrinks to the explicit rejections.

### What I did

(Filled in as the work proceeds.)

### Why

### What worked

### What didn't work

### What I learned

### What was tricky

### What warrants review

### Future work

## Step 2: Finishing the Postgres subset

**Author:** pg-subset-2

### Prompt Context

**Verbatim prompt:** You are finishing a half-done change in maragudb, an embedded Go SQL database whose only durable state is in S3. The change switches the SQL dialect from "our own flavor with documented deviations" to a strict subset of Postgres. [...] A previous builder ran out of API credits partway through. It committed nothing, and left uncommitted work in about 16 files [...] Its last words were "Now the exec tests, rewritten for the Postgres dialect and the new semantics". Treat its work as a draft. [...] Keep what is sound, and fix or redo what isn't. [...] ## The contract to implement 1. maragudb accepts a subset of Postgres 18 SQL. The same SQL text runs on both. 2. For every statement maragudb accepts, the result is identical to Postgres [...] 3. If Postgres fails a statement, maragudb fails it too. [...] 4. maragudb may be stricter, but only through a new explicit `ErrUnsupported` error (SQLSTATE 0A000, feature_not_supported). [...] 5. Irreducible semantic differences should be zero. [...] ## What to change - **Types.** [...] - **Differential harness.** Remove the dual-dialect renderer. [...] Add a ratchet [...] - **Deviations `DEV-1` to `DEV-19`.** [...] - **Docs.** Replace /docs/deviations.md with /docs/unsupported.md. [...] - **Public API.** Export `maragudb.ErrUnsupported` [...] - **Simulator workloads.** [...] - **CLAUDE.md.** Add `make verification` and `make report` to the Commands list. [...] ## Constraints - `make test`, `go vet ./...`, `make lint`, `go test -race ./...`, `make antithesis`, and `make verification` (with no diff) must all pass. - Run a heavy differential run, such as `make pgdiff` with 10,000 or more cases, and report the numbers.

**Interpretation:** Get the draft compiling and passing, keep the parts of it that match Postgres, replace the parts that emulate Postgres's planner, finish the harness, the docs, the public API, and the workloads, and prove the result with one SQL text on both systems.

**Inferred intent:** Any statement that maragudb accepts can be run on Postgres with the same answer, and every place where maragudb cannot promise that is an explicit, documented, tested rejection.

### What I did

The draft compiled in `/internal/sql` and failed one exec test; `/internal/pgdiff` did not compile. I kept most of it: the value types `value.Int4` and `value.Numeric` for literals, unknown-typed quoted literals, Postgres input and output functions in `/internal/sql/value/parse.go`, the Postgres keyword list, the lexer and parser rejections, implicit and assignment casts, transaction abort, ASCII `lower` and `upper`, the static LIMIT rule, and the runtime check for key-moving UPDATEs.

I replaced the draft's WHERE planner emulation (cost-sorted conjuncts and equivalence-class derived clauses in `/internal/sql/exec/plan.go`) with a check that needs no cost model. The filter evaluates conjuncts in written order. When a conjunct fails on a row, the row's error stands only when every other conjunct, and every equality implied by the equivalence classes, holds or fails too; when a conjunct rejects a row, any later conjunct that could fail is evaluated, and a failure makes the statement order-dependent. Order-dependent rows do not stop the scan, so a later row that fails in every order still gives its own error. Index plans are handled statically: a condition that can fail next to a condition that an index scan on the primary key could use is rejected.

Then I ran the harness and fixed what it found, in this order: a read-only transaction check that came before analysis (Postgres analyzes and plans DML before refusing it); `CASE` with an unknown operand, which Postgres makes text; a stack overflow in `negate` for `NOT (x IN (constants))`; numeric literal conversions that raised errors at bind time instead of folding, so pruned branches failed; numeric literals written as text (`01.50` is `1.50`, `1.5e1` is `15`); `+5` folded into the literal (Postgres folds only minus, so `ORDER BY +1` is no position); minus folded through parentheses (`-(-2147483648)` is a bigint, `-2147483648` an integer, and not an ORDER BY position); an `IN` whose constants have no common type, which Postgres compares one by one; stable functions (the timestamptz and text conversions), which Postgres does not fold; INSERT and UPDATE analysis order (each INSERT row is analyzed and checked before the next; UPDATE analyzes WHERE, then all SET expressions, then converts them); `multiple assignments` and wrong VALUES lengths as syntax errors; the bytea hex error code 22023; and ORDER BY ambiguity, which compares expressions after dropping casts to the same type.

Parameters now take their type from their context, like Postgres: a Go string is an unknown literal, an int64 converts to bigint, integer, or double precision, and every other conversion is `ErrUnsupported`. Integer and float input now scan like `pg_strtoint64` and glibc's `strtod`, so overflow is reported before trailing junk, and timestamps outside the ISO forms are unsupported rather than invalid.

The harness in `/internal/pgdiff` renders once, runs on one connection per side so that it can generate explicit transactions (including read-only ones), skips rejected statements on Postgres and aborts the Postgres transaction instead, reruns in key order inside transactions through a savepoint, and counts executed and rejected statements by reason. The generator takes its choices from a `pgdiff.Source`, either hegel or a seeded PRNG; the ratchet test runs 300 PRNG cases and compares the executed count with `/internal/pgdiff/testdata/ratchet.json`. New property tests in `/internal/pgdiff/casts_test.go` compare text casts to every type, and parameters of every Go type in many contexts, with Postgres and pgx. `/internal/pgdiff/unsupported_test.go` has one test per `UNS-` ID that asserts Postgres accepts the examples and maragudb rejects them.

Docs: `/docs/unsupported.md` replaces `/docs/deviations.md`, with 17 `UNS-` IDs, the retired DEV table, and an empty "Remaining differences". `/docs/requirements.md` has the new types table, `INV-PG-SUBSET` and `INV-UNSUPPORTED`, reworded `AC-4.1`, `AC-4.3`, and `AC-4.4`, a new `AC-4.5` for the ratchet, and a "Retired IDs" section for INV-STRICT-TYPES and DEV-1 to DEV-19. The verify package parses `UNS-` and `DEV-` IDs from `/docs/unsupported.md`, and the report shows executed, rejected, and ratchet numbers. A writer sub-agent updated the README and the introduction of `/docs/unsupported.md`; I fixed one sentence about `ErrType` that the parameter change made wrong.

The real-store SQL list-append workload in `/internal/workload` now has the simulator's packed lists, through helpers exported from `/internal/sim/sqlclient.go`, and a test shows `MUT-COMPACTED-AHEAD` detected there. `make ratchet` is new, and AGENTS.md (which CLAUDE.md links to) lists the verification targets.

### Why

The draft's cost emulation encoded Postgres's planner constants and EC derivations. Its own test showed the problem: it expected `1 / m = 1 AND (id = 1 OR id = 2)` to succeed because the OR is "cheaper", while the two costs are equal in Postgres and an index plan would change the outcome anyway. A rejection rule that does not need to know the order is correct by construction, and the Postgres documentation itself says WHERE evaluation order is not defined.

### What worked

- The harness found almost every bug above within a few hundred to a few thousand cases, each with a short shrunk case.
- The cast property found the overflow-before-junk rule on its first run: `SELECT CAST(CAST('92233720368547758070-' AS bigint) AS text): maragudb invalid cast ...; postgres SQLSTATE 22003`, and then the same for floats with `'11e308-'`.
- The parameter property found that pgx sends a `[]byte` for a bigint parameter as text: `SELECT $1::bigint with []byte{}: maragudb type mismatch ...; postgres SQLSTATE 22P02`.
- The packed lists make `MUT-COMPACTED-AHEAD` visible against the real-store workload: detected with seed 4 here, and not in 60 seeds with the packed lists turned off.
- The final heavy runs, three times `MARAGUDB_PGDIFF_CASES=10000 go test ./internal/pgdiff -run TestDifferential/generated` after the review fixes, all passed. The last: 50,887 statements, 49,408 executed (97.1%), 1,479 rejected (2.9%), 41,185 succeeded on both, 8,223 failed on both with the same class across 13 class pairs, 1 agreeing only in key order, 3,086 explicit transactions, 27,951 state checks, 4,925 ordered comparisons, 40 s. The rejections were 560 text and timestamptz conversions without columns, 423 LIMITs without a total order, 344 numeric results, 113 LIMITs with expressions that can fail, 20 plan-dependent WHERE clauses, 10 key-moving UPDATEs, and 9 LIKEs on bytea.
- The ratchet baseline is 1,542 of 1,587 statements executed (97%) for 300 cases with seed 1.

### What didn't work

- `go build ./...` on the draft: `internal/pgdiff/gen.go:378:49: too many arguments in call to ast.RenderExpr ... undefined: ast.Maragudb`.
- The draft's failing test: `exec_test.go:535: Expected nil error, but got "division by zero"` for `SELECT id FROM w WHERE 1 / m = 1 AND (id = 1 OR id = 2)`.
- `fatal error: stack overflow` in `exec.negate`, which called `negate(simplify(x))` on an IN of constants that `simplify` returned unchanged.
- The first read-only harness failure: `UPDATE t SET c0 = coalesce('\x')` gave `postgres SQLSTATE 42804, maragudb read-only transaction`.
- Treating `'a' || 1` as stable because `anytextcat` is marked stable in `pg_proc` was wrong: Postgres folded `CAST('x' || 1 AS bigint)` on an empty table (`ERROR: invalid input syntax for type bigint: "x1"`), because it inlines `anytextcat` into an immutable cast. Only the timestamptz conversions stay stable.
- Not folding stable expressions was not enough either: Postgres also evaluates them while estimating selectivity, so `SELECT 1 FROM e WHERE ts = CAST('2024-x'::text AS timestamptz)` fails on an empty table and `... WHERE s || CAST('2024-x'::text AS timestamptz) = 'a'` does not. That led to `UNS-STABLE-CONSTANT`.
- hegel with `hegel.WithSeed(1)`, `WithDerandomize(true)`, and `WithPhases(hegel.PhaseGenerate)` still generated different cases in different processes (statement counts 948, 902, 920), although single generators were deterministic; the divergence began mid-case at case 11. The ratchet uses a seeded PRNG instead.
- The first `make ratchet` run before the Source refactor wrote a baseline that the next run did not reproduce.
- The first README writer sub-agent stopped with `You're out of usage credits`; a second one with another model finished.
- `SELECT 1 FROM lists LIMIT 1` in the workload's seeded check became unsupported (no total order), which I changed to `WHERE FALSE`.

### Self-review

I ran the code-review skill at high effort over `3013b91..HEAD`. It reported ten findings, and confirmed most with a probe test. I fixed nine:

- A statement that failed in `exec.Prepare` never reached `exec.Session.Execute`, so `BEGIN; INSERT ...; SELEC 1; COMMIT` committed the insert. The driver now prepares through `exec.Session.Prepare`, which aborts the transaction.
- A write that failed in `kvstore.apply` did not abort the transaction either.
- `CASE WHEN true THEN 1.5::float8 ELSE 1e400 END` and `coalesce(1.5::float8, 1e400)` failed with out of range, because the pruned branch was unified with the live ones after `binder.bindPruned` returned. A `cexpr.pruned` flag now carries the branch's pruning into `binder.numericTo`.
- `'2024-01-02 03:04:05+05:99'::timestamptz` was accepted; Postgres reports 22009.
- `0x_1F` was a syntax error; Postgres allows an underscore after the base prefix.
- `BEGIN READ ONLY,`, `START`, and `START WORK` began a transaction; Postgres rejects them.
- `"coalesce"(1)` ran as COALESCE; for Postgres it is an unknown function.
- `'a' -- c` followed by a newline and `'b'` was a syntax error; Postgres joins the strings, so it is unsupported now.
- `left('abc', 1)` was a syntax error, because `left` is a type_func_name keyword; it is unsupported now.

I did not do the tenth, a refactor of the four cast tables into one coercion function by context, beyond removing a dead branch in `binder.unary`. It is real duplication, but folding the four rules together is a change of its own.

After the fixes, a heavy run failed once on `UPDATE t SET c4 = CAST(c2 AS bigint)`: `postgres SQLSTATE 23505, maragudb out of range`. With a row whose new values fail, `exec.Session.updateKeys` reported that row's error, while Postgres in key order first hits a duplicate key from an earlier row. It now visits the rows in key order like Postgres when any row fails. A new property, which casts values of every type to every type, then showed that Postgres 18 casts `integer` to `boolean` and has casts between bytea and integers; the first works now, and the second is unsupported.

### What I learned

- In Postgres a minus folds into a numeric constant even through parentheses, a plus never does, and `ORDER BY` positions are decided on the raw token, so `-2147483648` is an integer constant but not a position.
- Unknown literals are converted when the statement is analyzed, but numeric literals through functions that are folded, so only the latter are pruned by a false branch.
- An `IN` list is an array only when its non-column elements have a common type; otherwise it is OR-ed comparisons.
- UPDATE analyzes WHERE before SET, and folds SET before WHERE.
- pgx encodes a Go string or `[]byte` as text for any parameter type and refuses an int64 for text.

### What was tricky

- Deciding where a failing-on-Postgres statement may become `ErrUnsupported`. I report errors that Postgres raises for certain (analysis and folding errors) before plan-dependence rejections, but let a parser-level rejection win over a later Postgres error, which the harness accepts because it never runs rejected statements on Postgres.
- Keeping the harness in step inside transactions: a rejection aborts maragudb's transaction, so the harness runs `SELECT 1 / 0` on Postgres, and the key-order rerun has to go through a savepoint and abort the transaction again.

### What warrants review

- `/internal/sql/exec/plan.go`: `binder.where`, `filter.matches`, `filter.failed`, `binder.planIndependent`, and `binder.indexable`. The safety of the order rule rests on `cexpr.fallible` being set for everything that can fail at run time.
- `/internal/sql/exec/expr.go`: `binder.fold` with `stable`, `resolveParam`, and `binder.numericTo`.
- `/internal/sql/value/parse.go`: `parseInt`, `parseFloat`, `floatPrefix`, and `ParseTime`.
- `/internal/pgdiff/run.go`: `sides.compare` and `sides.inKeyOrder` inside transactions.
- Run `make test`, `make pgdiff PGDIFF_CASES=10000`, `MARAGUDB_PGDIFF_CASES=30000 go test ./internal/pgdiff -run 'TestCasts|TestParameters'`, and `make verification`.

### Future work

- The generator rarely produces several conditions that can fail in one WHERE clause, so `UNS-ORDER-DEPENDENT-WHERE` is mostly exercised by its unit and unsupported tests.
- The parser's choice between a syntax error and `ErrUnsupported` for statements outside the subset is only checked by examples; the sqllogictest corpus will test it at scale.
- `IN` lists and `OR` on the primary key are still full scans.
