# Deviations from Postgres

maragudb follows Postgres semantics, and the differential harness in `/internal/pgdiff` checks that generated statements give the same results on both. This document lists every intentional difference. Each one has its own test in `/internal/pgdiff/deviations_test.go`, which runs the statement on both databases and asserts the documented difference, so the list and the behavior cannot drift apart silently.

The type mapping the harness uses is `int` → `bigint`, `float` → `double precision`, `bool` → `boolean`, `string` → `text`, `bytes` → `bytea`, and `time` → `timestamptz`. Postgres runs with `COLLATE "C"`, so strings compare by byte order on both.

## Types and coercion

1. **No implicit coercion between int and float.** `DEV-1` Postgres casts `bigint` to `double precision` implicitly, so `int_col = 1.5` and `int_col + float_col` work there. In maragudb both are type errors. The one exception follows Postgres's literal typing: an integer literal, or a constant built only from literals, adapts to a float where a float is expected, so `float_col + 1` and `float_col = 2` work.
2. **No assignment casts.** `DEV-2` `INSERT INTO t (int_col) VALUES (1.5)` rounds to `2` in Postgres. In maragudb it is a type error.
3. **Parameters never adapt.** `DEV-3` A Go `int64` bound to a float column, or a `string` bound to an int column, is a type error. Postgres infers the parameter's type from the column and the client library converts the value.
4. **`||` only concatenates two strings or two bytes values.** `DEV-4` Postgres also accepts `text || anything` through its `anynonarray || text` operator, so `'a' || 1` gives `'a1'` there and is a type error here.
5. **`LIMIT` and `OFFSET` take an int.** `DEV-5` Postgres rounds `LIMIT 1.5` to `2`. maragudb rejects a float.
6. **Integer literals must fit in an int.** `DEV-6` `9223372036854775808` is a `numeric` literal in Postgres and a syntax error in maragudb.
7. **`time` has nanosecond precision and the range of Go's `time.Time`.** `DEV-7` Postgres keeps microseconds and rounds, so `TIME '2024-01-01T00:00:00.123456789Z'` reads back as `.123457` there and as `.123456789` here. The harness only generates microsecond times.

## Casts

8. **`CAST(time AS string)` renders RFC 3339 with nanoseconds** `DEV-8`, such as `2024-01-02T03:04:05.5Z`, and **`CAST(string AS time)` accepts only RFC 3339**, with a `T` or a space between date and time. Postgres renders `2024-01-02 03:04:05.5+00` and accepts many date formats.
9. **`CAST(bytes AS string)` decodes UTF-8 and `CAST(string AS bytes)` encodes it.** `DEV-9` Postgres renders bytes as the hex escape `\x6869` and parses that form back. In maragudb, bytes that are not valid UTF-8 cannot be cast to a string.
10. **`CAST(string AS int)` and `CAST(string AS float)` accept decimal notation only** `DEV-10`, with surrounding whitespace, and for floats exponents, `NaN`, and `Infinity`. Postgres 16 and later also accept `0x10`, `0o17`, `0b101`, and `1_000` for integers, and hexadecimal such as `0x10` for floats.

## Functions

11. **`lower` and `upper` fold case for all of Unicode** `DEV-11`, as Go does. Postgres with the `C` locale folds ASCII only, so `lower('ÄBC')` is `'äbc'` here and `'Äbc'` there. The harness generates `lower` and `upper` only for ASCII strings.

## Statements

12. **A failed statement does not abort the transaction.** `DEV-12` A statement that fails inside an explicit transaction has no effect, and the transaction stays usable: the next statement runs against the writes made before the failure. In Postgres the transaction is aborted and every later statement fails until `ROLLBACK`.
13. **Statements visit rows in key order.** `DEV-13` Postgres visits rows in heap order, which is usually insertion order. When an update moves keys onto each other, such as `UPDATE t SET id = id + 1`, which rows collide with a not-yet-moved row depends on that order, so the two can disagree on whether the statement fails. The harness never updates key columns. And when different rows fail for different reasons, the first row visited decides the error, so when the two fail for different reasons the harness runs the statement on Postgres again with the rows stored in key order, and accepts the error that run gives.
14. **Every table needs a primary key** `DEV-14`, and there is no hidden row id. Postgres allows tables without one.
15. **More words are reserved.** `DEV-15` `key`, `if`, `exists`, `set`, `values`, `begin`, `commit`, `rollback`, `start`, `transaction`, `nulls`, `limit`, `offset`, and the other words in the parser's keyword list cannot be used as unquoted column names, where Postgres allows some of them. Quoting with double quotes works.

## Output

16. **The output name of a cast of a constant is the maragudb type name** `DEV-16`, such as `int`, where Postgres uses its own, such as `int8`. Names of columns, functions, aliases, `case`, and `?column?` match Postgres.

## Errors

The harness also checks that when both databases fail, they fail for the same reason. Each maragudb error kind maps to the Postgres SQLSTATE codes that report the same failure, in `/internal/pgdiff/classes.go`, and `TestErrorClasses` exercises every pair. maragudb's kinds are coarser than SQLSTATE codes: `ErrType` covers 42804 and 42883, `ErrCast` covers 22P02, 22007, and 42846, and `ErrInvalid` covers the unresolved references of class 42 as well as the invalid arguments 22025 (a LIKE pattern that ends in its escape character), 2201W, and 2201X (a negative LIMIT or OFFSET).

17. **A string literal where another type is expected is a type error.** `DEV-17` Postgres reads an untyped string literal as the type the context expects, so `INSERT INTO t (int_col) VALUES ('a')` fails there with 22P02, invalid input syntax, and `VALUES ('1')` succeeds. In maragudb a string literal is a string, and both fail with `ErrType`.
18. **LIKE on a primary key column runs on every row the rest of the plan reads.** `DEV-18` Postgres turns `key_col LIKE 'pattern'` into an index condition on the pattern's fixed prefix and evaluates the LIKE only on the rows the index finds, so whether a pattern that ends in its escape character fails depends on the plan. maragudb evaluates the LIKE on every row, so `WHERE key_col LIKE '\'` fails as soon as a row is not empty. The harness never ends a LIKE pattern on a key column with the escape character.
19. **`WHERE` conjuncts are evaluated in the order they are written.** `DEV-19` Postgres's planner orders them by estimated cost, puts an equality with a constant after the others, and may turn some into index conditions that keep rows from being read at all. When one conjunct fails on a row that another conjunct rejects, the order decides whether the statement fails: `WHERE n = 2 AND s LIKE '\'` fails in Postgres on a row where `n` is 1, and returns nothing in maragudb. The harness only combines conjuncts that cannot fail.
