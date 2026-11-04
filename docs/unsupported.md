# Unsupported Postgres SQL

maragudb accepts a strict subset of Postgres 18 SQL. For every statement that it accepts, the result is identical to Postgres. When it cannot promise that, it rejects the statement with `maragudb.ErrUnsupported` (SQLSTATE 0A000, feature_not_supported) before it changes anything, and the error message says what is unsupported and, where possible, what to write instead.

This document lists every class of statement that maragudb rejects. Each class has a stable `UNS-` ID and its own test in `/internal/pgdiff/unsupported_test.go`. The test runs examples of the class on both databases and asserts that Postgres accepts them and that maragudb rejects them, so the list cannot drift from the behavior.

The differential harness in `/internal/pgdiff` runs every generated statement on maragudb first. When maragudb rejects it, the harness skips it on Postgres and counts it as rejected. Otherwise it runs the same text on Postgres and compares the results.

## Features that maragudb does not have

- **The numeric type.** `UNS-NUMERIC` A numeric literal, such as `1.5`, `1e3`, or an integer beyond the bigint range, has type numeric in Postgres until its context converts it. maragudb supports the conversions to double precision, bigint, and text: `f + 1.5` on a double precision `f`, `1.5::float8`, an insert of `2.5` into a bigint column, and `'x' || 1.5`. Everywhere else, such as in `SELECT 1.5` or `1 + 1.5`, the result would be numeric, and maragudb rejects the statement. Write a cast, such as `1.5::float8`.
- **Other types.** `UNS-TYPES` The supported types are `bigint`, `double precision`, `boolean`, `text`, `bytea`, and `timestamptz`, with their aliases `int8`, `float8`, `float`, `bool`, and `timestamp with time zone`. Every other type name is rejected, among them `integer`, `smallint`, `real`, `numeric`, `varchar`, `date`, `timestamp`, `json`, and `uuid`, and so is a type modifier such as `timestamptz(3)`.
- **Timestamps outside ISO 8601 and the common era.** `UNS-TIMESTAMP-INPUT` maragudb reads a timestamptz in the ISO 8601 forms, such as `2024-01-02 03:04:05.123456+02` or `2024-01-02T03:04:05Z`, with or without a zone. It rejects the other forms that Postgres reads, such as `January 2, 2024`, special values such as `now` and `infinity`, named time zones such as `Europe/Oslo`, and instants before the year 1, which Postgres writes with a `BC` suffix.
- **Tables without a primary key.** `UNS-NO-PRIMARY-KEY` Every row lives at its primary key, so every table needs one. Add `PRIMARY KEY` to a column, or `PRIMARY KEY (columns)` to the table.
- **Other schema statements and constraints.** `UNS-DDL` The schema statements are `CREATE TABLE`, with `IF NOT EXISTS`, and `DROP TABLE` of one table, with `IF EXISTS`. The constraints are `PRIMARY KEY` and `NOT NULL`. maragudb rejects the rest, such as `UNIQUE`, `DEFAULT`, `CHECK`, `REFERENCES`, temporary tables, `CREATE INDEX`, `ALTER TABLE`, and a `DROP TABLE` of several tables.
- **Other statements and transaction modes.** `UNS-STATEMENTS` The statements are `CREATE TABLE`, `DROP TABLE`, `INSERT`, `UPDATE`, `DELETE`, `SELECT`, and `BEGIN`, `START TRANSACTION`, `COMMIT`, `END`, `ROLLBACK`, and `ABORT`. Every transaction is serializable, so maragudb rejects other isolation levels and `DEFERRABLE`. It also rejects savepoints, `WITH`, `EXPLAIN`, `SET`, `TRUNCATE`, a bare `VALUES`, and the other Postgres statements, and a string with several statements.
- **Other clauses.** `UNS-CLAUSES` A `SELECT` reads one table, or none, with `WHERE`, `ORDER BY`, `LIMIT`, and `OFFSET`. maragudb rejects `DISTINCT`, `GROUP BY`, `HAVING`, joins, subqueries, `UNION` and the other set operations, table aliases, qualified names such as `t.c`, `FOR UPDATE`, and `FETCH`. In writes, it rejects `RETURNING`, `ON CONFLICT`, `INSERT ... SELECT`, `DEFAULT`, and `UPDATE ... FROM`.
- **Other functions.** `UNS-FUNCTIONS` The functions are `abs`, `coalesce`, `length`, `lower`, and `upper`. As in Postgres with the `C` locale, `lower` and `upper` change only ASCII letters. maragudb rejects every other function name, including the aggregates and window functions.
- **Other operators and predicates.** `UNS-OPERATORS` The operators are `+`, `-`, `*`, `/`, `%`, `||`, `=`, `<>`, `!=`, `<`, `<=`, `>`, `>=`, `AND`, `OR`, `NOT`, `IS NULL`, `IS NOT NULL`, `BETWEEN`, `IN` with a list, `LIKE`, `CASE`, and `CAST` with its `::` and `type 'text'` forms. maragudb rejects the rest, such as `IS DISTINCT FROM`, `IS TRUE`, `ILIKE`, regular expressions, bit operators, arrays, row constructors, `LIKE ... ESCAPE`, `LIKE` on bytea, `BETWEEN SYMMETRIC`, `COLLATE`, and `AT TIME ZONE`.
- **Other forms of constants and identifiers.** `UNS-LITERALS` Strings are written in standard quotes, where a backslash is an ordinary character. maragudb rejects escape strings such as `E'a\n'`, bit strings such as `B'101'` and `X'1F'`, national strings, Unicode escapes, dollar quoting, a string continued on the next line, and unquoted identifiers with letters outside ASCII. Bytes are written as text cast to bytea, such as `'\x01ff'::bytea`.

## Parameters

- **A parameter whose Go value would need a conversion.** `UNS-PARAMETERS` A parameter has no type of its own. As in Postgres, it takes the type its context gives it, the way a quoted literal does. A Go string is read exactly like a quoted literal, so `"5"` is the bigint 5 next to a bigint. An int64 is a bigint, an integer if it fits in 32 bits, or a double precision, and each other Go type is its own SQL type. Where the conversion is not one of these, such as a float64 used as a bigint or an int64 used as text, Postgres drivers differ in what they do, and maragudb rejects the statement. Pass a value of the matching Go type, or cast the parameter, such as `$1::float8`.

## Statements whose outcome depends on Postgres's plan

Postgres decides some outcomes in its planner or by the physical order of rows, which maragudb cannot reproduce. A statement where that would show is rejected.

- **LIMIT and OFFSET without a total order.** `UNS-LIMIT-ORDER` Which rows Postgres returns under `LIMIT` or `OFFSET` depends on its plan unless `ORDER BY` sorts by every primary key column. Add the primary key columns to `ORDER BY`.
- **LIMIT and OFFSET with an expression that can fail.** `UNS-PLAN-DEPENDENT-LIMIT` Under a `LIMIT`, an index scan stops early and a sequential scan with a sort does not, so how many rows Postgres evaluates an expression on depends on its plan. maragudb rejects `LIMIT` and `OFFSET` when an output, `ORDER BY`, or `WHERE` expression can fail at run time, such as an addition, which can overflow, or a cast of text. Select the rows first, and compute the expression on the result.
- **A condition that can fail next to a primary key condition.** `UNS-PLAN-DEPENDENT-WHERE` An index scan on the primary key evaluates the other conditions of a `WHERE` clause only on the rows the index finds, and a sequential scan on every row. maragudb rejects a `WHERE` clause with a condition that can fail at run time next to a condition that an index scan could use, such as `id = 1 AND n + 1 > 0`, including one that Postgres derives from equalities, such as `id = n AND n = 5`. It also rejects a `LIKE` on a key column whose pattern can fail. Write the condition that can fail inside `CASE WHEN <key condition> THEN <condition> END`, or rewrite it so that it cannot fail, such as `n > -1`.
- **A condition that fails on a row that another condition rejects.** `UNS-ORDER-DEPENDENT-WHERE` Postgres evaluates the conditions of a `WHERE` clause in an order its planner picks and stops at the first that is not true. When one condition fails on a row that another rejects, such as `m <> 0 AND 10 / m > 1` on a row where `m` is 0, whether the statement fails depends on that order, as the Postgres documentation warns. maragudb evaluates every condition that can fail on such a row, and rejects the statement when the outcome would depend on the order. It considers the equalities that Postgres derives from the written ones too. The statement still fails with the row's own error when every order fails. Write `CASE WHEN m <> 0 THEN 10 / m > 1 END` to fix the order.
- **A conversion between text and timestamptz that reads no columns and can fail.** `UNS-STABLE-CONSTANT` Postgres does not fold a conversion between timestamptz and text into a constant, because it depends on the session's time zone. It evaluates such an expression when the statement runs, but also while it plans, to estimate how many rows a condition matches, depending on where the expression is. When the expression can fail, such as `CAST(CAST('2024-01-02' AS text) AS timestamptz)`, whether the statement fails depends on the plan. Write the timestamptz as a quoted literal, such as `'2024-01-02 00:00:00+00'`.
- **An UPDATE that moves a row onto a key that another row leaves.** `UNS-KEY-UPDATE-ORDER` Postgres checks the primary key of each row as it updates it, in the order it stores the rows, so `UPDATE t SET id = id + 1` succeeds or fails depending on that order. maragudb computes every new key first. It rejects the statement when a row moves onto the key of a row that the same statement moves away, and otherwise succeeds or fails exactly as Postgres does in every order. Update such rows in two statements.

## Errors that depend on order

When different rows of a statement fail for different reasons, Postgres reports the error of the row it reaches first, which depends on the physical order of the rows. maragudb visits rows in primary key order. Both fail, but the error class may differ. The differential harness accepts a different class only when Postgres, given the rows stored in key order, reports the same class as maragudb.

## Remaining differences

None. maragudb has no known difference from Postgres for the statements it accepts.

The deviations of the former own SQL flavor, DEV-1 to DEV-19 in its deviations document, are gone:

| Former deviation | Now |
|---|---|
| DEV-1, no implicit coercion between integers and floats | eliminated: Postgres's implicit casts |
| DEV-2, no assignment casts | eliminated: Postgres's assignment casts |
| DEV-3, parameters never adapt | eliminated, except UNS-PARAMETERS |
| DEV-4, `\|\|` only for two strings or two bytes | eliminated: text with any type |
| DEV-5, LIMIT and OFFSET only take an integer | eliminated: Postgres's conversion to bigint |
| DEV-6, integer literals must fit in 64 bits | UNS-NUMERIC |
| DEV-7, nanosecond times | eliminated: microseconds, rounded as in Postgres |
| DEV-8, RFC 3339 time casts | eliminated: Postgres's output format, and UNS-TIMESTAMP-INPUT |
| DEV-9, bytes cast as UTF-8 | eliminated: Postgres's hex and escape formats |
| DEV-10, decimal-only number casts | eliminated: Postgres's input formats |
| DEV-11, Unicode case folding | eliminated: ASCII only, as with the `C` locale |
| DEV-12, a failed statement keeps the transaction usable | eliminated: the transaction is aborted until it ends |
| DEV-13, rows visited in key order | UNS-KEY-UPDATE-ORDER, and errors that depend on order |
| DEV-14, every table needs a primary key | UNS-NO-PRIMARY-KEY |
| DEV-15, more reserved words | eliminated: Postgres's reserved words |
| DEV-16, output names of casts | eliminated: Postgres's names, such as `int8` |
| DEV-17, a string literal is a string | eliminated: a quoted literal takes the type of its context |
| DEV-18, LIKE on a primary key column | UNS-PLAN-DEPENDENT-WHERE |
| DEV-19, WHERE conjuncts in written order | UNS-ORDER-DEPENDENT-WHERE |
