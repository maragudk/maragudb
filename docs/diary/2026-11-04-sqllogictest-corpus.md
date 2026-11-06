# Diary: SQLite sqllogictest corpus as an external Postgres oracle

maragudb's SQL is a strict subset of Postgres 18 (see `/docs/requirements.md`, INV-PG-SUBSET). The generated differential harness in `/internal/pgdiff` only checks the statements our own generator writes. This feature adds an external corpus that nobody here wrote: the SQLite sqllogictest queries, run live against maragudb and Postgres 18 under the same contract. SQLite's recorded results are ignored, and Postgres is the oracle. A fixed subset of files runs in `make test` and the full corpus in `make soak`. A committed per-file baseline of executed statements makes sure the coverage can only go up.

## Step 1: Set the goal and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Postgres names outright. But we might want to consider some things we don't want to support, common Postgres antipatterns, such as the money type and more. But we can decide that later. I like the idea of external test suites as well.
**Interpretation:** After the switch to a strict Postgres subset, bring in external SQL test suites as more evidence that maragudb gives the same results as Postgres.
**Inferred intent:** More trust signals that do not depend on our own generator's blind spots. If the generator and the executor share a misunderstanding, an outside corpus can still catch it.

### What I did
A research sub-agent (pg-corpora-research) compared the candidate corpora earlier today. Its report is saved at `/private/tmp/claude-502/-Users-maragubot-Developer-maragudb/9f1f3c00-52d2-42e8-b183-1c05f5a1e4ae/scratchpad/corpora/RESEARCH.md`, and the downloaded corpora are next to it. The conclusions:
- Use SQLite sqllogictest first. It can be taken under MIT. Do not vendor the GPL C runner.
- Pin a mirror commit SHA, download it in the test, and cache it. Do not commit the roughly 1 GB of data.
- Rewrite the types: INTEGER to bigint, FLOAT to double precision. In the `random/` files, add a composite primary key over all columns. Drop CREATE INDEX.
- Run each statement on both databases. If maragudb returns ErrUnsupported, skip it on Postgres. Otherwise the results or error classes must match.
- Ratchet the executed count per file.
- The Postgres regression suite comes second. The CockroachDB and Materialize suites have incompatible licenses.

Then I started this diary and handed the work to the builder `sqllogictest`.

### Why
The generated pgdiff only covers what its generator can write. sqllogictest holds millions of queries written by other people, many of them in shapes our generator never makes.

### What worked
The research came back with a clear first choice and a license path.

### What didn't work
Nothing failed in this step.

### What I learned
Most of the corpus uses SQL that section 1 does not support yet: joins, aggregates, subqueries. So the share that executes will be low at first. That is why the ratchet works per file and only goes up. Sections 2 and 3 raise the numbers.

### What was tricky
sqllogictest's expected results come from SQLite and are not correct for Postgres. They must never be used as the oracle.

### What warrants review
Check that the harness never compares against SQLite's recorded results. Check that a rewrite of a statement is applied in the same way on both sides.

### Future work
- The Postgres regression suite, as a second corpus.
- Raise the baselines as sections 2 and 3 land.

## Step 2: Run the corpus live on both databases

**Author:** sqllogictest

### Prompt Context

**Verbatim prompt:** You are the builder `sqllogictest` in /Users/maragubot/Developer/maragudb (Go module maragu.dev/maragudb, branch main). [...] ## Goal Run the SQLite sqllogictest corpus live against maragudb and Postgres 18 under that contract, as a second differential oracle next to the generated pgdiff. [...] ## Requirements 1. **License.** Use the sqllogictest test files under the MIT option of their COPYRIGHT.md. Do not vendor or port the GPL C runner. Write your own Go parser for the .test format. 2. **Data.** Do not commit the corpus. Pin a mirror commit SHA, and download and verify it (with a checksum) into a gitignored cache, or the user cache dir, on first use. [...] 3. **Rewrites** apply once, to the text, and the same rewritten text then runs on both sides [...] 4. **Oracle.** Postgres 18 is the only oracle. [...] 5. **Skips.** [...] 6. **Ratchet.** [...] 7. **Runs.** [...] 8. **Bugs found.** [...] Do NOT change the plan-dependence rules [...] 9. **Verification map.** [...] 10. **CI.** [...] 11. **Docs.** [...] (Resumed on 2026-11-06: "One change since then: I recorded a decision at the end of docs/decisions.md (commit 8666dfc). A primary-key condition will fix the plan to an index scan, and the harness will run Postgres with enable_seqscan=off. A separate builder implements that after you; for you, the instruction stands not to change the plan-dependence rules. Also check your uncommitted plan.go edit against that.")
**Interpretation:** Build a Go harness that parses the corpus, rewrites it minimally, runs every statement through the existing pgdiff comparison, counts executed, rejected, setup-failed, and skipped statements per file, ratchets the executed count per file, and fixes what it finds in maragudb.
**Inferred intent:** A second oracle whose SQL nobody here wrote, so that blind spots shared by our generator and our executor show up, with numbers that a reader of the CI report can trust.

### What I did

The new package `/internal/sqllogictest` has four parts:
- `parse.go`: my own parser of the `.test` format. It keeps the SQL of `statement` and `query` records, their `skipif`/`onlyif` conditions, and `halt`, and drops the results, hashes, `hash-threshold`, and comments. The `ok`/`error` marks and the type strings are never read.
- `rewrite.go`: the rewrites, all listed in the doc comment of `rewrite`. CREATE INDEX is dropped; column types and CAST targets become bigint, double precision, or text; a CREATE TABLE without a primary key gets one over all its columns; and `INSERT INTO a SELECT * FROM b` becomes `INSERT INTO a VALUES` with b's rows, when b was filled only by INSERT ... VALUES and has the same column definitions. A small tokenizer keeps strings, quoted identifiers, and comments intact, so a rewrite never touches text inside them.
- `corpus.go`: the pinned corpus, `gregrahn/sqllogictest` at `c93ac416f05635ff5cc490009add936d9753d01d`, which keeps SQLite's COPYRIGHT.md (the dolthub mirror the research used has regenerated results and no COPYRIGHT.md). It downloads the codeload tarball, unpacks only the names in the committed `/internal/sqllogictest/testdata/manifest.txt` into `os.UserCacheDir()/maragudb/sqllogictest/<commit>` through a temporary directory and a rename, and checks every file against its SHA-256, at download and again at every read. With no cache and no network the test fails with a message that names the cache directory, the mirror, the commit, and `MARAGUDB_SQLLOGICTEST_DIR`.
- `run.go`: runs a file with a fresh maragudb and a fresh Postgres schema per worker, through `pgdiff.Session`. A table or view whose creation was rejected or failed is remembered, and every later statement that names it is skipped and counted, until it is created; a skipped CREATE marks its own name too.

To share the comparison, I exported `pgdiff.Session` (commit `c29b4fa`): `Session.Exec` takes SQL text, parses it with maragudb's parser for the AST, and keeps a map of primary keys per table, so the key-order rerun works on whichever table a statement touches. `Runner.Run` uses the same code. Then I made the comparison stronger (commit `74b2cda`): `FullyOrdered` understands `ORDER BY <position>`, a key column in the output no longer shadows itself, and when ORDER BY sorts by output columns but does not fix the order, those leading sort-key columns must come out in the same order on both sides, with -0 and 0 rendered alike. Rejection reasons drop the parser's position and the condition text of UNS-PLAN-DEPENDENT-WHERE, so they aggregate.

`TestCorpus` runs the `make test` subset by default (the 12 evidence files, `index/{between,delete,in,orderby,random}/10/slt_good_0.test`, `random/{expr,select}/slt_good_0.test`, and `select1.test`), the full corpus with `MARAGUDB_SQLLOGICTEST=full`, and globs with `MARAGUDB_SQLLOGICTEST_FILES`. It compares every file with `/internal/sqllogictest/testdata/baseline.json`, which also names the corpus commit, records a `verify.Corpus` fact, and covers `AC-4.6`. `make sqllogictest` and `make sqllogictest-ratchet` are new, and `make soak` runs the full corpus after the simulator. AC-4.6 is in `/docs/requirements.md` word for word, the report has a corpus row and a details table of reasons, and AGENTS.md and README.md mention the targets and the corpus.

Decisions I made on the way, which I kept in this diary rather than in `/docs/decisions.md`:
- No committed subset of files. The smallest useful index file is 1 to 4 MB, and the download is 116 MB once per machine, or once per CI job. So `make test` downloads too, and fails rather than skips when it cannot.
- `onlyif <other engine>` records do not run, because they are in another dialect. `skipif postgresql` is ignored, because its notes are stale ("PostgreSQL requires AS when renaming output columns") and Postgres is the oracle anyway. `halt` follows its conditions, so `skipif sqlite` + `halt` stops `evidence/slt_lang_aggfunc.test`.
- "Setup failed on Postgres" is a `statement` record that failed on both sides with the same class. It counts as executed, and is also counted by reason. I did not use SQLite's `statement ok` mark to decide it, to stay clear of SQLite's recorded outcomes.
- Instead of skipping the rest of a file when its setup diverges, only statements that name a table or view whose creation was rejected or failed are skipped. A file where one insert fails still runs its queries on the same rows on both sides.

The bug the corpus found: `SELECT 31 AS col0 FROM tab0 WHERE NOT ( NULL ) = NULL` (index/random/10/slt_good_0.test:7539) and `SELECT ALL * FROM tab1 WHERE NOT NULL = ( NULL )` (random/select/slt_good_0.test:64700) returned every row on maragudb and none on Postgres. In `/internal/sql/exec/plan.go`, `simplify` rewrites `x = x` to `x IS NOT NULL`, which is right only where NULL and false are the same (a WHERE conjunct), and the NOT case called `negate(simplify(x))`, so `NOT (NULL = NULL)` became `NULL IS NULL`. The same happened through `x IN (x)` and `(x = x) = false`. Now NOT negates its operand without simplifying it first, `boolEquality` does the same, and `negate` handles BETWEEN and IN by desugaring them. `TestRegressions` in `/internal/sqllogictest/run_test.go` runs these statements on both databases; it fails without the fix. I checked the change against the 2026-11-06 instruction: it changes no plan-dependence rule. It only changes which conjuncts a negated comparison becomes, and `NOT (x = x)` is now `x <> x`, which is also what Postgres's negate_clause makes of it.

### Why

The research recommended exactly this shape, and the existing pgdiff comparison already implements the contract, so the corpus only needed a parser, rewrites, and bookkeeping around `pgdiff.Session`. Skipping only the statements that name a missing object keeps as much of a file comparable as possible, while still never running a statement against a table that only SQLite would have.

### What worked

- The first sample of 30 files found the NOT bug in two files within seconds.
- The full corpus (`make sqllogictest-ratchet`, 5m29s here): 622 files, 7,420,713 records, of which 1,480,999 are for other engines and 17,308 were dropped by the rewrites. Of the 5,922,406 statements, 2,761,594 ran on both sides (2,758,373 succeeded on both, 3,221 failed on both with the same class), 3,117,006 were rejected, and 43,806 were skipped. 643,497 SELECTs were compared in exact order and 129,893 with their sort keys in order. Zero mismatches.
- Executed by directory (files, executed, rejected): index/between 13, 107,008, 17,441; index/commute 52, 489,743, 23,890; index/delete 14, 146,261, 0; index/in 13, 114,607, 18,135; index/orderby 31, 301,999, 10,560; index/orderby_nosort 49, 475,687, 18,840; index/random 26, 81,153, 126,665; index/view 16, about 30,900, 44,130; random/aggregates 130, 329,330, 963,020; random/expr 120, 524,386, 675,980; random/groupby 14, 169, 133,169; random/select 127, 160,889, 1,083,434; select1-5 5, 4,192, 6,498; evidence 12, 145, 104. (The index/view count is from the run before the view-dependency fix; the baseline has the exact numbers.)
- Top rejections: DISTINCT 939,807; table aliases 909,846; count 409,966; qualified names 332,547; subqueries 92,415; sum 74,606; min 74,323; max 73,573; nullif 73,455; several tables in FROM 56,092; GROUP BY 25,535; DROP VIEW 14,582; avg 13,328; UNION 9,979; CREATE VIEW 9,724; subqueries in FROM 6,440. The plan-dependence rules rejected 267 statements under UNS-PLAN-DEPENDENT-WHERE (all in random/ and index/random, conditions such as `col0 <= -col1` next to a key condition) and 2 under UNS-ORDER-DEPENDENT-WHERE; UNS-PLAN-DEPENDENT-LIMIT rejected none.
- Setup that failed on Postgres and maragudb alike: 36 inserts of a NULL into a column that the primary key rewrite made NOT NULL (select1-5 and evidence), 4 syntax errors, 2 unknown columns, 2 unknown tables, and 1 multiple assignment, all in the evidence files. Skipped: 43,750 statements on views (CREATE VIEW is unsupported), 43 on a table with UNIQUE, and 13 on triggers.
- The `make test` subset: 20 files, 70,655 statements, 46,468 executed (66%), 24,121 rejected, 66 skipped, 19 setup failures, 0 mismatches, in about 11 s alone. `make test` took 15.2 s with it and 16.4 s before; `go test -race` of the package takes 52 s.
- A heavy `MARAGUDB_PGDIFF_CASES=3000` pgdiff run after the ordering change passed: 15,219 statements, 1,766 ordered comparisons and 745 comparisons of sort keys in order.

### What didn't work

- `head -c 600 RESEARCH.md` showed that the research file is the hand-back text of the research agent, not a separate report; there was nothing more to read.
- The first runner test expected line 9 for a record on line 8: `parse_test.go:41: Expected "9", but got "8" (type int)`. My count was wrong, not the parser.
- The regression test first inserted a NULL into a table without a key; the primary key rewrite turned that into a setup failure, so the NULL row was never there. It now declares the key itself.
- The first full run had `no table/42P01=4862`: `CREATE VIEW view_3_tab0_153 AS SELECT pk FROM view_1_tab0_153` was skipped because view_1 was missing, but view_3 was not marked missing, so its queries ran and failed on both sides. Skipped CREATEs now mark their name; the count fell to 2, both intended `DROP TABLE` errors in evidence/slt_lang_droptable.test.
- Rejection reasons came out as `at_position__:_DISTINCT_is_not_supported`, because pgdiff's pattern `at position _` could only match after the digits were already replaced. Fixed in `rejectionReason`.
- The dirty-table bookkeeping of the INSERT ... SELECT rewrite first missed `DROP TABLE IF EXISTS` and a trailing semicolon after VALUES; neither occurs in the corpus where it matters, but both are handled and tested now.
- `staticcheck` rejected `-0.0` in a test: `SA4026: in Go, the floating-point literal '-0.0' is the same as '0.0'`. The test uses `math.Copysign(0, -1)`.

### Self-review

I ran the code-review skill at medium effort over `8666dfc..HEAD`. It reported nine findings, without a verification pass. I fixed six:
- `FullyOrdered(sel, s.keys[sel.From])` returned true for a table with no known key, because the loop over a nil key never runs. A SELECT from a table whose key is unknown is now never fully ordered.
- `CREATE TABLE IF NOT EXISTS` of an existing table overwrote the tracked key. It no longer does, and DROP TABLE no longer deletes the key either, so a DROP that rolls back keeps it. The new runner test "compares in order only by the real primary key of a table" fails without this: `Expected "1", but got "2"` ordered comparisons.
- A mismatch in the middle of a transaction could return the Postgres connection to the pool with the transaction still open, because the cleanup only rolled back when the session knew of one. It now always runs ROLLBACK.
- The rerun in key order decided again from the text prefix whether a statement is a query; it now takes the flag of the first run.
- `runFiles` went on handing out files after the first harness error; it now stops.
- The new BETWEEN and IN branches of `negate` repeated `simplify`; they are now `simplify` of the inverted predicate, which is exact in three-valued logic.

I left three as they are. The INSERT ... SELECT rewrite expands rows from the text even when one of the source inserts failed, which makes the expanded insert fail as a whole; that only costs coverage and shows in the setup failures, and the rewrite must not depend on run-time outcomes, so the doc comment says so now. `dependsOn` counts any word that is the missing name, also a column of that name; it errs towards skipping, and the comment says so. `topReasons` in `/internal/verify/report.go` and `topCounts` in the corpus test do the same sort in two packages; it is a few lines each, and I did not add an export for it.

After the fixes, `make test`, `go test -race -shuffle on ./...`, `make lint`, `make verification` (no diff), and `go mod tidy` (no diff) pass, and a second full `make sqllogictest` run passed with zero mismatches.

### What I learned

- Most of the corpus that maragudb cannot run yet is section 3 (aggregates, DISTINCT, subqueries, joins via table aliases and qualified names) and views. Sections 2 and 3 will raise the baseline a lot.
- Under `C` collation and with the sort-key projection, the corpus's partial ORDER BYs (`ORDER BY 1 DESC` on a non-key column) can be checked in order without any false positive in 5.9M statements.
- The corpus never needs SQLite's expected results to be useful: every interesting result in this run came from the two databases disagreeing.

### What was tricky

- Rewrites must be decided on text before anything runs, while "is this table only filled by VALUES" depends on the file's history. The pre-pass tracks it per table and resets on CREATE and DROP, and since both sides run the result, a wrong guess can only cost coverage, never hide a difference.
- The download must be safe for concurrent test processes and partial failures: it unpacks into a temporary directory next to the cache and renames it, writing a marker with the manifest's hash last, and only ever writes names that the manifest contains.

### What warrants review

- `/internal/sqllogictest/run.go` `fileRunner.run`: what counts as other-engine, skipped, setup failure, and executed, and that every statement lands in exactly one of executed, rejected, or skipped.
- `/internal/sqllogictest/rewrite.go`: that no rewrite depends on anything but the text, and that strings stay untouched.
- `/internal/pgdiff/run.go` `sides.compare`: the sort-key check, and the key-order rerun with a table per statement.
- `/internal/sql/exec/plan.go` `simplify` and `negate`.
- Validate with `make test`, `make sqllogictest` (zero mismatches), `make pgdiff PGDIFF_CASES=10000`, and `make verification`.

### Future work

- Raise the baselines with `make sqllogictest-ratchet` as sections 2 and 3 land; CREATE INDEX can then stop being dropped.
- When the enable_seqscan=off change lands in pgdiff, check that it reaches the corpus's connections too: `fileRunner` takes its connection from `pgdiff.Postgres` and sets its own `search_path`, so a setting applied per connection in `Runner.Run` would not reach it.
- The Postgres regression suite as a third corpus, as Step 1 planned.
