# Diary: Arithmetic on numeric aggregate results

Section 3, part 2 (`/docs/diary/2026-11-06-section-3-aggregates.md`) added `numeric`, but only as the result type of `SUM` over integers and `AVG`. Arithmetic on it is UNS. That arithmetic, such as `- SUM(col)` or `SUM(a) + SUM(b)`, is now the largest aggregate-related rejection in the sqllogictest corpus: 54,509 statements.

## Step 1: Set the scope and the verification

**Author:** main

### Prompt Context

**Verbatim prompt:** Build independently, this is an experiment
**Interpretation:** Part 2 is pushed and its builder named arithmetic on numeric as the largest remaining rejection. The lead makes the scope calls.
**Inferred intent:** Widen the SQL surface without weakening the guarantee that every accepted statement gives the same result as Postgres.

### What I did
I wrote the scope and gave it to the builder `numeric-arithmetic`. It covers these operators where at least one operand is numeric and the other is numeric, smallint, integer or bigint: unary `-` and `+`, and binary `+`, `-`, `*`, `/` and `%`. The result is numeric, with Postgres's scale rules and its errors (22012 for division by zero, 22003 when the value goes out of range). A numeric with a `double precision` operand gives `double precision`, as in Postgres, which converts the numeric to float8 first. This is allowed only where that conversion is exact or rounds the same way that Postgres rounds.

These stay UNS: numeric literals such as `1.5`, numeric columns, casts to numeric, and functions that take numeric (`abs`, `round` and the rest). This is a new decision entry in `/docs/decisions.md` that widens the earlier numeric entry; the builder writes it by hand.

### Why
Reports often negate, add or divide sums. The arithmetic is exact, so plan order cannot change the result. That makes it cheap to support in a way that is provably identical to Postgres.

### What worked
Nothing to report yet.

### What didn't work
Nothing failed in this step.

### What I learned
Division is the only operator with unusual scale rules (`select_div_scale`), and part 2 already ported it for `AVG`.

### What was tricky
Postgres's conversion from numeric to float8 goes through text and `strtod`, so mixed numeric and float arithmetic must round in exactly the same way.

### What warrants review
The scale of `/` and `%`, the out-of-range limits, and the conversion from numeric to float8.

### Future work
Section 3, part 3: subqueries and non-recursive CTEs.

## Step 2: Numeric operators, their port, and the differential checks

**Author:** numeric-arithmetic

### Prompt Context

**Verbatim prompt:** You are the builder `numeric-arithmetic` for maragudb [...] ## Goal Arithmetic on numeric values. Today numeric exists only as the result type of SUM(int types) and AVG(int types) [...] In scope, where at least one operand is numeric and the other is numeric, smallint, integer or bigint: unary `-` and `+`; binary `+`, `-`, `*`, `/`, `%`. Result type numeric with Postgres 18's exact scale and display rules [...] numeric with double precision: [...] Support only where your conversion provably rounds identically [...] Out of scope (stay UNS): numeric literals (`1.5`), numeric columns, casts *to* numeric, every function taking numeric (abs, round, ...), numeric aggregates over numeric. [...] ## Verification required [...] Port-based property test [...] Differential: extend the pgdiff generator [...] Raise the pgdiff ratchet [...] and the sqllogictest baseline [...] Update UNS-NUMERIC [...] Decision entry [...] CI time [...]
**Interpretation:** Port Postgres's numeric operators for the values maragudb can hold, let the binder accept them on numeric results, check them against Postgres at the value level and through SQL, and raise both baselines.
**Inferred intent:** Accept the 54,509 corpus statements that negate, add, or divide aggregate results, without any accepted statement differing from Postgres.

### What I did

- `/internal/sql/value/decimal.go` (bb0495b, cleaned in 50d8b1a and ffcbffd): a numeric is parsed from its text into an unscaled `big.Int` and a scale (`decimal`). `DecimalAdd`, `DecimalSub`, and `DecimalRem` align both operands to the larger scale (`aligned`); `DecimalMul` is exact with the sum of the scales and rounds half away from zero to 16383 when the sum is larger; `DecimalQuo` generalises the earlier integer-only `DecimalDiv` (now gone; `AVG` calls `DecimalQuo`) with `select_div_scale` for operands with fractions: the weight and first digit in base 10000 aligned at the point (`firstDigit`), and the maximum with both input scales before the clamp to 1000. `DecimalNeg` keeps the scale and never signs a zero. Every result with more than 131072 digits before the point is `ErrOutOfRange`, and a zero divisor is the new `value.ErrDivisionByZero`. `DecimalToFloat` now also fails for a nonzero numeric that `ParseFloat` rounds to zero, as `float8in` does.
- `/internal/sql/exec/expr.go` (b9dfc0f): unary minus and plus, and `arithmetic` for `value.Decimal`, through `decimalArithmetic`. A sign is not fallible; the binary operators are, like the integer ones. The old `errDecimalArithmetic` became `errDecimalFunction`, for `abs` and for an aggregate of a numeric argument.
- Tests: `TestDecimalArithmetic` in `/internal/sql/value/decimal_test.go` has a table of 21 operand pairs with Postgres 18's results for all five operators, half-away ties, the clamp of a quotient to 1000 digits, the product cap at 16383, the 131072-digit boundary, and a hegel property against `big.Rat` (exact sums, differences, products, the negation, the remainder of the truncated quotient, the quotient within half a unit and rounded half away from zero). `TestDecimalToFloat` gained the subnormal boundary (`2.4703282292062328e-324` is `5e-324` and `...327e-324` fails, as Postgres showed) and a property against `big.Rat.Float64`, which is exact, on midpoints between adjacent doubles, shifted by 1e-1200 either way, and on numerics scaled from 1e-340 to 1e320. `TestNumericArithmetic` in `/internal/pgdiff/aggregates_test.go` runs 600 hegel cases of `($1::numeric op $2::numeric)::text` on Postgres and compares the text or error class with the `value` functions, with shapes chosen for the edges: zeros of several scales, about 1000 and about 8190 digits after the point, about 131072 before it, products whose scale sum is near 16383, and divisors that make ties. Two new subtests of `TestAggregates` run the operators in SQL over aggregate results: signs, all operators with integer, bigint, numeric, and double precision operands, 22012 and 22003, folding of constant numerics (`coalesce(5, sum(v)) / 0` on an empty table fails in both), HAVING, ORDER BY, DISTINCT, LIMIT, joins, and chains of 20 to 60 divisions and 17 multiplications that reach the scale limits and the double precision underflow and overflow.
- `/internal/pgdiff/gen.go` (9760688, 50d8b1a): `numericArithmetic` replaces the one `+ 1` the generator wrote on numerics: a sign, or an operator with an integer literal, a bigint literal, another `SUM`, `AVG`, or `COUNT(*)`, a double precision, or a numeric literal that maragudb rejects. Under a LIMIT it writes only a sign.
- `UNS-NUMERIC` in `/docs/unsupported.md` and its test now pin numeric literals next to numerics, quoted literals read as numeric, and functions of numeric. A writer agent drafted the decision entry in `/docs/decisions.md` and the edits to `/docs/unsupported.md` (also `UNS-DISTINCT-ZERO`), `/docs/requirements.md`, and `/README.md` from my brief; I checked them against the code and changed "exact results" in the requirements to "the digits and scale that Postgres gives them", since a quotient rounds.

### Why

The arithmetic is exact or rounds deterministically, so every plan gives the same value, and the plan rules for errors in grouped output already exist. What could differ from Postgres is only the text: the scales, the rounding, and the limits. So the checks concentrate on comparing the text with Postgres directly, with operands that a SQL query can hardly produce.

### What worked

- Postgres answered every scale question directly through `psql` in the harness's container (`docker exec maragudb-pgdiff psql -U test`), which gave the table of expected results and settled the limits: `('9'||repeat('0',131071))::numeric * 10` is `22003 value overflows numeric format`, a product of scales 8191 and 8194 is rounded to 16383 digits with the 49.95 rounding to 50, and a quotient of a 1101-scale dividend is clamped to scale 1000.
- Mutating `/internal/sql/value/decimal.go` by hand showed which test catches what: dropping the input scales from `divScale`, the base-10000 padding in `firstDigit`, or shifting the 1000 clamp or the 131072 limit fails `TestNumericArithmetic`; rounding ties down and shifting the 16383 cap failed only the `value` tests until I added the tie and cap shapes to the Postgres property, after which it catches all six at 600 cases.
- The full corpus: 5,763,846 of 5,939,714 statements executed, up from 5,709,337 (+54,509, exactly the arithmetic rejections), and 132,105 rejected, down from 186,614, with zero mismatches; `the_numeric_type_is_not_supported` fell to 11 statements, all literals.
- `make pgdiff PGDIFF_CASES=10000` on the final code (after deb0b16): 99,632 of 101,627 statements executed, 1,995 rejected, zero mismatches, against 98,447 of 100,486 before this work; the ratchet rose from 2,930 to 2,932 executed statements of 2,981. The full corpus ran again on the final code with the same counts and zero mismatches.

### What didn't work

- Two of my own expectations in the new SQL tests were wrong, and Postgres agreed with maragudb: `SELECT (sum(v) * 9223372036854775807 * ... 20 times)::float8` fails with 22003 because the value exceeds 1e308 (I cut it to 15 factors), and `(avg(v) * 3)::bigint` overflows for the group of two `9223372036854775807`s.
- `SELECT g, sum(v) / 4 FROM ga GROUP BY g ORDER BY g LIMIT 2` is rejected with `LIMIT_and_OFFSET_on_groups_with_an_expression_that_can_fail_are_not_supported`, because numeric division counts as fallible. I kept that and pinned it, and made the signs infallible so that `-sum(v)` works under a LIMIT.
- `SELECT DISTINCT avg(v) * 0 FROM ga GROUP BY g` is rejected under `UNS-DISTINCT-ZERO`: the average of the group of two maximal bigints has scale 0 and the others scale 16, so the zeros print differently. That is correct and now pinned.
- The first midpoint property panicked in `big.Rat.Add` with a nil operand: `new(big.Rat).SetFloat64(math.Nextafter(f, math.Inf(1)))` is nil for the infinity after `math.MaxFloat64`. It now takes the neighbour toward zero.
- A boundary test of mine expected `-999...9 (131072 nines) - 0.1` to overflow; it does not, as Postgres confirmed (`131075` characters), because the integer part keeps 131072 digits. The test now asserts that and that `- 1` fails.
- The second `make pgdiff PGDIFF_CASES=10000`, on the final numeric code, failed on a statement without any numeric: `UPDATE t SET c0 = DEFAULT WHERE NULL AND CASE '\x' WHEN NULL THEN c2 WHEN c3 THEN NULL END OR NULL` gave `postgres: error: ERROR: operator does not exist: text = bytea (SQLSTATE 42883)` and `maragudb: 0 rows affected`. The commit before my work fails the same way. `canonicalQual` in `/internal/sql/exec/plan.go` turned an AND with a NULL argument into NULL without binding the other arguments, so neither their types nor their constants were checked; `(NULL AND j > 1 / 0) OR NULL` succeeded too where Postgres fails with 22012. It now binds them first (`bindsCleanly`) and otherwise binds the clause as it is, which reports the error (deb0b16, with both statements and two more in `TestIndexConditions`). The first 10000-case run had not drawn it; hegel draws differently every run.
- The fix of the review's generator finding first changed the random stream (`g.typedText` draws once), and the ratchet dropped from 2932 to 2931 (`executed statements dropped from 2932 to 2931`). Wrapping the numeric literal that `g.literal` returns in a cast, without new draws, kept the stream.

### Self-review

I ran the code-review skill at medium over d5ba28e..HEAD. It found no correctness bug and checked the scale, rounding, cap, sign, and overflow rules against Postgres's code. Of its six cleanup findings I fixed four in 50d8b1a: the generator's double precision operand was a numeric literal a fifth of the time, which maragudb rejects, so those cases tested nothing; `DecimalAdd`, `DecimalSub`, and `DecimalRem` share `aligned`; the three spellings of the zero test share `decimalZero`; and `DecimalDiv` is gone in favour of `DecimalQuo`. I left two performance findings: every operator parses its operands' text and formats its result, and `firstDigit` counts digits through `String()`. That is quadratic only for values of thousands of digits, which no realistic query reaches, and keeping the text as the value's form keeps comparisons, hashing, and output unchanged. A mutant of my own then showed that the zero check in `decimalText` could never fire, since only a negative `big.Int` sets the sign, and I removed it in ffcbffd.

Checks on the final code, after deb0b16: `make test` passes, `make lint` reports 0 issues, `make verification` and `go mod tidy` leave no diff. `GOMAXPROCS=4 go test -race -count 1`, each package alone, before and after: pgdiff 15.1 s to 16.7 s (with 600 cases of the property; 2,000 cases made it 22.2 s), value 4.7 s to 5.7 s; exec 27.6 s to 29.3 s and sqllogictest 57.0 s to 57.1 s, measured in one parallel run.

### What I learned

- Postgres's `float8in` fails on underflow only when `strtod` returns zero or infinity, so subnormals convert, and Go's `strconv.ParseFloat` returns `0, nil` on underflow where glibc sets ERANGE, so the check has to be explicit.
- `select_div_scale` takes the maximum with both input scales before the clamp to 1000, so a dividend of scale 2000 divides to scale 1000, and the exponent of the scaling can be negative.
- `mod_var` computes `x - trunc(x / y) * y` exactly, so its scale is simply the larger input scale and its sign that of the dividend.

### What was tricky

- The double precision question. Both sides must round the exact decimal correctly, and the only evidence I trust is a property against an exact oracle (`big.Rat.Float64`) on midpoints, plus Postgres's own behaviour at the subnormal boundary, both of which are now in tests.
- Getting the Postgres property to reach the cap and the ties: random operands almost never make a product of scale 16384 or a quotient ending in an exact 5, so the property forces both shapes a tenth of the time each.

### What warrants review

- `/internal/sql/value/decimal.go`: `divScale`, `firstDigit`, `quotient`, `DecimalMul`, and `DecimalToFloat`.
- `/internal/sql/exec/expr.go`: `unary` (signs are not fallible) and `decimalArithmetic`.
- Validate with `go test ./internal/sql/value -run 'TestDecimal'`, `go test ./internal/pgdiff -run 'TestNumericArithmetic|TestAggregates|TestUnsupported'`, `MARAGUDB_PGDIFF_CASES=5000 go test ./internal/pgdiff -run TestNumericArithmetic`, and `make sqllogictest`.

### Future work

- Numeric literals (`SUM(x) * 1.5`) and functions of numeric (`abs`, `round`) are the next numeric step, but the corpus rejects only 11 statements for numeric now, so they are not urgent.
- Numeric division or `%` by a nonzero integer constant cannot fail, so it could be allowed under a LIMIT on groups.
- Subqueries remain the largest rejection (92,443), then views (24,306 for CREATE and DROP VIEW) and UNION (9,979).
