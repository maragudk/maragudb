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
_, err = db.ExecContext(ctx, `CREATE TABLE accounts (id bigint PRIMARY KEY, balance bigint NOT NULL)`)
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

The types are the Postgres types `bigint`, `double precision`, `boolean`, `text`, `bytea`, and `timestamptz`, plus NULL. A `timestamptz` has microsecond precision, as in Postgres. The Go parameter types are `int64` and `int`, `float64`, `bool`, `string`, `[]byte`, `time.Time`, and `nil`. As in Postgres, a parameter takes its type from its context. A Go string reads like a quoted literal, so `"5"` is the bigint 5 next to a bigint. An `int64` converts to a bigint, an integer if it fits, or a double precision, and each other Go type is its own SQL type. Any other conversion fails with `maragudb.ErrUnsupported`, because Postgres drivers differ in what they do with it.

The SQL surface is `CREATE TABLE` and `DROP TABLE`; `CREATE [UNIQUE] INDEX` and `DROP INDEX`; `INSERT`, `UPDATE`, and `DELETE`; `SELECT` with `DISTINCT`, `WHERE`, `ORDER BY`, and `LIMIT`/`OFFSET`, on one table or on several, with table aliases, qualified names such as `t.c` and `t.*`, `CROSS JOIN`, `[INNER] JOIN` and `LEFT JOIN` with `ON`, `USING`, or `NATURAL`, and self-joins, and `GROUP BY` and `HAVING` with `COUNT`, `SUM`, `AVG`, `MIN`, and `MAX`, also with `DISTINCT`; subqueries with `IN`, `NOT IN`, `EXISTS`, and `NOT EXISTS`, and scalar subqueries, correlated or not, in queries and in the `WHERE`, `SET`, and `VALUES` of writes, subqueries in `FROM`, and non-recursive `WITH` in front of a `SELECT`; `BEGIN`, `COMMIT`, and `ROLLBACK`; and the `PRIMARY KEY`, `UNIQUE`, `NOT NULL`, `DEFAULT`, `CHECK`, and `FOREIGN KEY` constraints. A `DEFAULT` can be a constant or `now()`, which is the start of the transaction. A foreign key is checked at the end of each statement, and `ON DELETE` can be `NO ACTION`, `RESTRICT`, `CASCADE`, `SET NULL`, or `SET DEFAULT`, through other tables and within one. Queries use the indexes for equalities, ranges, `IN`, and an `ORDER BY` in index order, and a join looks up the rows of a table through an index on the columns it joins on, and indexes are written in the same commit as their rows. Expressions include `CASE`, `COALESCE`, `NULLIF`, `CAST`, `LIKE`, `BETWEEN`, `IN`, and the functions `length`, `lower`, `upper`, `abs`, and `now`. `SUM` of a bigint and `AVG` return Postgres's `numeric`, which reaches Go as a string, and arithmetic on these results works as in Postgres. The SQL is a strict subset of Postgres 18. The same text runs on both and gives identical results. maragudb is stricter only through `maragudb.ErrUnsupported` (SQLSTATE 0A000), whose message says what to write instead; [docs/unsupported.md](docs/unsupported.md) lists every rejected class. As in Postgres, a failed statement aborts its transaction until `ROLLBACK`.

`maragudb.Options` has a `Logger` for debug and warning messages, `MemoryCacheBytes` for the in-memory block cache (default 64 MiB), `DiskCacheDir` for a disk cache that never evicts, and `GCGracePeriod` (default one hour). The grace period is the longest a transaction may live, and it is measured on the clocks of the processes that run transactions and garbage collection, so the clock skew between processes must be much smaller than it. `Maintenance` turns the background maintenance described below on or off (default on), `MaintenanceInterval` is the time between its checks for work (default 10 seconds, with a jitter of half the interval either way), and `CompactionFanIn` is the number of segments of similar size that it merges into one (default 4, at least 2).

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

The driver runs compaction and garbage collection in the background for every database it opens. Every `MaintenanceInterval`, a process checks for work on the manifest it already has from its freshness checks, so an idle process makes no requests. Work means segments that the compaction tiers below make due for a merge, or garbage that is new or older than the grace period. Processes that share a database coordinate through a lease object at `<prefix>/maintenance-lease`, so the work of one trigger is done about once instead of once per process. The lease only saves work; correctness never depends on it, because compaction is a conditional commit like any other and a delete can safely happen twice. If two processes both believe they hold it, they duplicate work and nothing else, and no clock sync is needed.

Every commit adds a small segment. Compaction puts segments in tiers by size: tier 0 holds the segments below 1 KiB, and each tier above holds segments `CompactionFanIn` times larger than the tier below it. When `CompactionFanIn` adjacent segments are in one tier, maintenance merges them into one segment of the next tier, in their place. Only segments that are adjacent in age order are merged, because a newer segment shadows the older ones. When there are at least `CompactionFanIn` segments, and those after the oldest one are together as large as it, or delete at least as many of its rows as remain, maintenance merges the whole database into one segment, which drops the deleted and overwritten values. So compaction rewrites each byte about once per tier, and once maintenance has caught up with the commits, a database of n bytes has O(log n) tiers and fewer than `CompactionFanIn` segments in each. The work of compaction for each committed byte grows with the logarithm of the database size, not with the size. A lower fan-in keeps reads cheaper, because a cold read descends into every segment that can hold its key, and a higher one rewrites data less often.

Compaction and garbage collection together bound the objects in the store: an object that left the manifest, or that a failed commit or compaction left behind, is deleted after the grace period. A GC pass lists the garbage it stamps in a garbage batch object under `<prefix>/gc/`, and the manifest references only chains of those batches, so the manifest that every commit writes stays the same size however many commits the grace period holds. A database whose manifest an earlier version of maragudb wrote fails to open with an error that names the versions. `maragudb.Stats` returns request counts by kind, cache hits and misses, the number of segments and garbage objects, and `MaintenanceRuns`, `LeaseAcquisitions`, and `LeaseLosses`.

`maragudb.Compact` and `maragudb.GC` remain for manual use, for example from a periodic job with `Maintenance` off. `Compact` merges every segment into one, whatever the tiers, keeps the commits that happen meanwhile ahead of the merged segment, and returns `maragudb.ErrConflict` only when another compaction won. `GC` deletes the objects that no transaction within the grace period can reference and reapplies its pass on top of commits that land meanwhile. Both can be retried on `maragudb.ErrConflict`.

## How do we know it works?

Nobody reads this code, so the tests have to show their work. They do so in two places: [docs/verification.md](docs/verification.md), generated from the tests themselves, and a verification report on the summary page of every [CI run](https://github.com/maragudk/maragudb/actions/workflows/ci.yml) with the numbers of that run.

**Every requirement maps to tests.** Each acceptance criterion and invariant in [docs/requirements.md](docs/requirements.md) has an ID, such as `AC-1.5` or `INV-STRICT-SERIALIZABLE`, and so does each class of statement that maragudb rejects. Tests declare which IDs they verify and, in one line, what they assert. docs/verification.md lists the tests behind every ID. A test fails if an ID has no test, if a test names an ID that does not exist, or if the document is stale. So a reader can check that every promise has a test without opening one.

**The unsupported list is tested.** Each class of rejected statements has a `UNS-` ID and a test that runs examples of the class on both databases. The test asserts that Postgres accepts them and maragudb rejects them. The differential harness runs one SQL text on both sides. It skips statements that maragudb rejects and counts the executed and the rejected statements. A committed baseline, `internal/pgdiff/testdata/ratchet.json`, fails the test when the executed count drops; `make ratchet` rewrites it.

**An outside corpus checks the same contract.** The SQLite sqllogictest corpus, millions of queries that other people wrote, runs on both databases with Postgres as the only oracle, and SQLite's recorded results are ignored. `make test` runs a fixed set of its files and `make soak` all of them, and a per-file baseline of the statements that run on both sides keeps its coverage from dropping.

**Mutants prove the checkers bite.** A passing checker might be one that never fires. So fifteen deliberate bugs, such as committing without compare-and-swap, acknowledging a commit before the store confirms it, or skipping the check of a unique index or of a foreign key, run through the simulator in up to twelve configurations until a checker catches them. The one bug that no workload can trigger, an UPDATE that revisits rows it moved, is caught by a targeted test instead. The kill matrix records the first seed that did and the checkers that caught it. Where a configuration cannot catch a mutant, it states why, and a wrong reason fails the test.

**Reached states prove the hard cases happen.** Zero anomalies mean nothing if the simulator never lost an acknowledgement or crashed mid-commit. Seventeen such states are declared, and the fixed corpus must reach each at least once. The CI report shows how often.

## Development

`make test` runs everything, including the simulator corpus, the Postgres differential harness, and the Tigris suite, and records the verification facts in `.verify/`; `make report` renders them as the report CI shows. `make verification` regenerates the kill matrix and docs/verification.md. `make lint` runs golangci-lint. `make soak` runs the simulator with random seeds for a while and then the whole sqllogictest corpus, and `make replay SEED=<n>` replays one seed with a full trace; add `WORKLOAD=bank`, `WORKLOAD=unique`, or `WORKLOAD=foreign-key`, `API=sql`, `CORPUS=1` for the corpus configuration of that seed, `MAINTAINERS=<n>` (default 2), `MAINTENANCE=lease|none|overlap` for the clients' background maintenance, or `MUTATION=<name>` with a name from the kill matrix to replay that configuration. `make pgdiff` runs a longer differential run, `make ratchet` rewrites the baseline of statements that the harness executes on both sides, `make sqllogictest` runs the sqllogictest corpus, which it downloads into the user cache directory on first use, `make sqllogictest-ratchet` rewrites its baseline, and `make tigris` runs only the Tigris suite.

The Postgres harness needs Docker locally. It starts a `postgres:18` container named `maragudb-pgdiff` on an ephemeral port and leaves it running for later runs. Set `MARAGUDB_PGDIFF_URL` to a Postgres superuser URL to use another server instead. The harness fails rather than skips when neither is available. Each test process creates a database of its own on the server, from `template0` with the C locale, and drops it at the end, so concurrent runs on one server do not collide; databases left behind for over a day are dropped too.

The Tigris suite reads `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL_S3`, `AWS_REGION`, and `BUCKET` from the environment or from a gitignored `.env` file at the repository root. It fails when any but `AWS_REGION`, which defaults to `auto`, is missing.

The Antithesis harness is in `antithesis`: a Dockerfile, `config/docker-compose.yaml`, the test template in `test/maragudb`, the workload command in `workload`, and notes in `scratchbook`. It runs two containers: versitygw v1.10.0 as the S3-compatible store, and the workload image, cross-compiled to linux/amd64 with cgo and instrumented with `antithesis-go-toolexec`. The test commands `parallel_driver_s3_bank_engine`, `parallel_driver_s3_bank_sql`, `parallel_driver_s3_list_append_engine`, and `parallel_driver_s3_list_append_sql` run the workloads against versitygw over the network, and `parallel_driver_sim` runs simulator seeds against the in-process fake. `make antithesis-build` builds the images. `make antithesis` brings the harness up locally with docker compose, runs every test command once, and tears it down. `make antithesis-validate` runs `snouty validate antithesis/config`, which needs snouty, the Antithesis CLI, on `PATH`. Running the harness locally needs no Antithesis account; launching a run does.

Made with ✨sparkles✨ by [maragu](https://www.maragu.dev/): independent software consulting for cloud-native Go apps & AI engineering.

[Contact me at markus@maragu.dk](mailto:markus@maragu.dk) for consulting work, or perhaps an invoice to support this project?
