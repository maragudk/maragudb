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

`maragudb.Options` has a `Logger` for debug and warning messages, `MemoryCacheBytes` for the in-memory block cache (default 64 MiB), `DiskCacheDir` for a disk cache that never evicts, and `GCGracePeriod` (default one hour). The grace period is the longest a transaction may live, and it is measured on the clocks of the processes that run transactions and garbage collection, so the clock skew between processes must be much smaller than it. `Maintenance` turns the background maintenance described below on or off (default on), `MaintenanceInterval` is the time between its checks for work (default 10 seconds, with a jitter of half the interval either way), and `CompactionThreshold` is the number of segments at which it compacts (default 8; every commit adds one segment).

To use another object store, or an S3 client configured in code, set `Options.Store` to any value with these four methods. The store then serves every DSN in place of the default AWS SDK client; the DSN's bucket is ignored, and its prefix still applies.

```go
type myStore struct{ /* ... */ }

func (s *myStore) Get(ctx context.Context, key string, opts maragudb.GetOptions) ([]byte, string, error)
func (s *myStore) Put(ctx context.Context, key string, body []byte, opts maragudb.PutOptions) (string, error)
func (s *myStore) Delete(ctx context.Context, key string) error
func (s *myStore) List(ctx context.Context, prefix string) ([]string, error)

maragudb.RegisterDriver(maragudb.Options{Store: &myStore{}})
```

The store must support conditional writes (`PutOptions.IfNoneMatch` for `If-None-Match: *`, and `PutOptions.IfMatch`), conditional reads (`GetOptions.IfNoneMatch`), and byte ranges (`GetOptions.Offset` and `Length`, where a `Length` of zero reads the whole object), and it must return `maragudb.ErrObjectNotFound`, `maragudb.ErrPreconditionFailed`, and `maragudb.ErrNotModified`, possibly wrapped, for those outcomes. Any other error means the request failed, and a failed write may or may not have been applied. The ETag that `Put` returns must equal the one a later `Get` of that version returns, and a read with `GetOptions.Consistent` must see every write that completed before it started. The godoc of `Options.Store` has the whole contract, and the `ExampleOptions` example is a complete store in memory.

The driver runs compaction and garbage collection in the background for every database it opens. Every `MaintenanceInterval`, a process checks for work on the manifest it already has from its freshness checks, so an idle process makes no requests. Work means at least `CompactionThreshold` segments, or garbage that is new or older than the grace period. Processes that share a database coordinate through a lease object at `<prefix>/maintenance-lease`, so the work of one trigger is done about once instead of once per process. The lease only saves work; correctness never depends on it, because compaction is a conditional commit like any other and a delete can safely happen twice. If two processes both believe they hold it, they duplicate work and nothing else, and no clock sync is needed.

Compaction and garbage collection together bound the objects in the store: an object that left the manifest, or that a failed commit or compaction left behind, is deleted after the grace period. `maragudb.Stats` returns request counts by kind, cache hits and misses, the number of segments and garbage objects, and `MaintenanceRuns`, `LeaseAcquisitions`, and `LeaseLosses`.

`maragudb.Compact` and `maragudb.GC` remain for manual use, for example from a periodic job with `Maintenance` off. `Compact` merges every segment into one, keeps the commits that happen meanwhile ahead of the merged segment, and returns `maragudb.ErrConflict` only when another compaction won. `GC` deletes the objects that no transaction within the grace period can reference and reapplies its pass on top of commits that land meanwhile. Both can be retried on `maragudb.ErrConflict`.

## Development

`make test` runs everything, including the simulator corpus, the Postgres differential harness, and the Tigris suite. `make lint` runs golangci-lint. `make soak` runs the simulator with random seeds for a while, and `make replay SEED=<n>` replays one seed with a full trace; add `MAINTENANCE=lease|none|overlap` to replay it with the clients' background maintenance in that mode. `make pgdiff` runs a longer differential run, and `make tigris` runs only the Tigris suite.

The Postgres harness needs Docker locally. It starts a `postgres:18` container named `maragudb-pgdiff` on an ephemeral port and leaves it running for later runs. Set `MARAGUDB_PGDIFF_URL` to a Postgres superuser URL to use another server instead. The harness fails rather than skips when neither is available.

The Tigris suite reads `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, `AWS_REGION`, and `BUCKET` from the environment or from a gitignored `.env` file at the repository root. It fails when any but `AWS_REGION`, which defaults to `auto`, is missing.

The Antithesis harness is in `antithesis`: a Dockerfile, `config/docker-compose.yaml`, the test template in `test/maragudb`, the workload command in `workload`, and notes in `scratchbook`. It runs two containers: versitygw v1.10.0 as the S3-compatible store, and the workload image, cross-compiled to linux/amd64 with cgo and instrumented with `antithesis-go-toolexec`. The test commands `parallel_driver_s3_bank_engine`, `parallel_driver_s3_bank_sql`, `parallel_driver_s3_list_append_engine`, and `parallel_driver_s3_list_append_sql` run the workloads against versitygw over the network, and `parallel_driver_sim` runs simulator seeds against the in-process fake. `make antithesis-build` builds the images. `make antithesis` brings the harness up locally with docker compose, runs every test command once, and tears it down. `make antithesis-validate` runs `snouty validate antithesis/config`, which needs snouty, the Antithesis CLI, on `PATH`. Running the harness locally needs no Antithesis account; launching a run does.

Made with ✨sparkles✨ by [maragu](https://www.maragu.dev/): independent software consulting for cloud-native Go apps & AI engineering.

[Contact me at markus@maragu.dk](mailto:markus@maragu.dk) for consulting work, or perhaps an invoice to support this project?
