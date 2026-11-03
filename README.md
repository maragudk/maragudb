# maragudb

> [!WARNING]
> **This is an experiment, not production software. Do not trust it with data you care about.**
>
> The experiment asks: can you build an S3-backed SQL database you can trust when no human ever reads the code? An AI writes the code, and the author, Markus, deliberately never looks at it. Trust has to come from automated verification alone: deterministic simulation testing with fault injection, strict serializability history checkers, mutation tests that prove the checkers catch deliberate bugs, property-based tests, differential testing against Postgres, and integration tests against real S3-compatible storage (Tigris).
>
> APIs, the storage format, and everything else may change without notice.

[![Docs](https://pkg.go.dev/badge/maragu.dev/maragudb)](https://pkg.go.dev/maragu.dev/maragudb)
[![CI](https://github.com/maragudk/maragudb/actions/workflows/ci.yml/badge.svg)](https://github.com/maragudk/maragudb/actions/workflows/ci.yml)

An embedded Go SQL database whose only durable state is in S3-compatible object storage.

## Usage

Register the driver once, then open a database with a DSN of the form `s3://bucket/prefix`. Credentials and the endpoint come from the standard AWS environment: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, and `AWS_ENDPOINT_URL_S3` for S3-compatible stores such as Tigris.

```go
maragudb.RegisterDriver(maragudb.Options{})
db, err := sql.Open("maragudb", "s3://my-bucket/app")
```

To skip registration, open through a connector instead:

```go
c, err := maragudb.NewConnector(maragudb.Options{}, "s3://my-bucket/app")
db := sql.OpenDB(c)
```

Every table needs a primary key. Placeholders are `$1`, `$2`, and so on.

```go
_, err = db.ExecContext(ctx, `CREATE TABLE accounts (id int PRIMARY KEY, balance int NOT NULL)`)
_, err = db.ExecContext(ctx, `INSERT INTO accounts (id, balance) VALUES ($1, $2)`, 1, 100)

var balance int64
err = db.QueryRowContext(ctx, `SELECT balance FROM accounts WHERE id = $1`, 1).Scan(&balance)
```

Transactions are strict serializable. A single statement outside a transaction retries on conflict by itself. An explicit transaction whose commit loses to a concurrent commit fails with `maragudb.ErrConflict`, which `errors.Is` or `maragudb.IsRetryable` detects, and the caller runs the whole transaction again. A read-only transaction, begun with `sql.TxOptions{ReadOnly: true}`, never conflicts.

```go
for {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET balance = balance - $1 WHERE id = $2`, 10, 1); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET balance = balance + $1 WHERE id = $2`, 10, 2); err != nil {
		_ = tx.Rollback()
		return err
	}
	err = tx.Commit()
	if maragudb.IsRetryable(err) {
		continue
	}
	return err
}
```

The types are `int`, `float`, `bool`, `string`, `bytes`, and `time`, plus NULL. The Go parameter types are `int64` and `int`, `float64`, `bool`, `string`, `[]byte`, `time.Time`, and `nil`. A parameter of the wrong type for its column is an error and is never converted.

The SQL surface is `CREATE TABLE` and `DROP TABLE`; `INSERT`, `UPDATE`, and `DELETE`; `SELECT` with `WHERE`, `ORDER BY`, and `LIMIT`/`OFFSET`; `BEGIN`, `COMMIT`, and `ROLLBACK`; and the `PRIMARY KEY` and `NOT NULL` constraints. Expressions include `CASE`, `COALESCE`, `CAST`, `LIKE`, `BETWEEN`, `IN`, and the functions `length`, `lower`, `upper`, and `abs`. Semantics follow Postgres; the differences are listed in [docs/deviations.md](docs/deviations.md).

`maragudb.Options` has a `Logger` for debug and warning messages, `MemoryCacheBytes` for the in-memory block cache (default 64 MiB), `DiskCacheDir` for a disk cache that never evicts, and `GCGracePeriod` (default one hour). The grace period is the longest a transaction may live, and it is measured on the clocks of the processes that run transactions and garbage collection, so the clock skew between processes must be much smaller than it.

There is no background maintenance; the application runs it, for example from a periodic job. `maragudb.Stats` returns request counts by kind, cache hits and misses, and the number of segments and garbage objects. `maragudb.Compact` merges every segment into one, so call it when the segment count grows. `maragudb.GC` deletes the objects that no transaction within the grace period can reference, so call it after compaction and whenever garbage accumulates. Both return `maragudb.ErrConflict` when a commit happens meanwhile, and can then be retried.

## Development

`make test` runs everything, including the simulator corpus, the Postgres differential harness, and the Tigris suite. `make lint` runs golangci-lint. `make soak` runs the simulator with random seeds for a while, and `make replay SEED=<n>` replays one seed with a full trace. `make pgdiff` runs a longer differential run, and `make tigris` runs only the Tigris suite.

The Postgres harness needs Docker locally. It starts a `postgres:18` container named `maragudb-pgdiff` on an ephemeral port and leaves it running for later runs. Set `MARAGUDB_PGDIFF_URL` to a Postgres superuser URL to use another server instead. The harness fails rather than skips when neither is available.

The Tigris suite reads `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, `AWS_REGION`, and `BUCKET` from the environment or from a gitignored `.env` file at the repository root. It fails when any but `AWS_REGION`, which defaults to `auto`, is missing.

Made with ✨sparkles✨ by [maragu](https://www.maragu.dev/): independent software consulting for cloud-native Go apps & AI engineering.

[Contact me at markus@maragu.dk](mailto:markus@maragu.dk) for consulting work, or perhaps an invoice to support this project?
