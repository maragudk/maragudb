// Package maragudb is an embedded SQL database whose only durable state is in S3-compatible
// object storage. It is used through [database/sql]: register the driver once with
// [RegisterDriver], then open a database with a DSN of the form s3://bucket/prefix.
//
//	maragudb.RegisterDriver(maragudb.Options{})
//	db, err := sql.Open("maragudb", "s3://my-bucket/app")
//
// Credentials and the endpoint come from the standard AWS environment and configuration chain,
// including AWS_ENDPOINT_URL_S3 for S3-compatible stores.
//
// To use another object store, or an S3 client configured in code, set [Options.Store] to any
// value with these four methods, which only use types from this package and the standard library:
//
//	type myStore struct{ /* ... */ }
//
//	func (s *myStore) Get(ctx context.Context, key string, opts maragudb.GetOptions) ([]byte, string, error)
//	func (s *myStore) Put(ctx context.Context, key string, body []byte, opts maragudb.PutOptions) (string, error)
//	func (s *myStore) Delete(ctx context.Context, key string) error
//	func (s *myStore) List(ctx context.Context, prefix string) ([]string, error)
//
//	maragudb.RegisterDriver(maragudb.Options{Store: &myStore{}})
//
// The store must support conditional writes and return [ErrObjectNotFound],
// [ErrPreconditionFailed], and [ErrNotModified] for those outcomes. [Options.Store] documents the
// whole contract, and the example for [Options] is a complete store in memory.
//
// The SQL is a strict subset of Postgres 18: the same text runs on both, and every statement
// that maragudb accepts gives the same result as on Postgres. The types are bigint, double
// precision, boolean, text, bytea, and timestamptz, with microsecond precision. A statement
// that Postgres accepts and maragudb does not, or whose result on Postgres would depend on its
// plan, fails with [ErrUnsupported]. As in Postgres, a failed statement in a transaction aborts
// it: later statements fail with [ErrTxAborted], and a commit rolls it back.
//
// Placeholders are $1, $2, and so on. Parameters are int64 and int, float64, bool, string,
// []byte, time.Time, and nil, and a nil []byte is NULL. A parameter takes its type from its
// context, as in Postgres: a string is read like a quoted literal of that type, an int64
// converts to an integer or a double precision, and the other Go types are their own SQL type.
// Other conversions, which Postgres drivers make in different ways, fail with [ErrUnsupported].
//
// Transactions are strict serializable. A statement outside a transaction retries on conflict by
// itself. An explicit transaction whose commit loses to a concurrent commit fails with
// [ErrConflict], and the caller retries the whole transaction. A read-only transaction, begun
// with sql.TxOptions{ReadOnly: true}, never conflicts.
package maragudb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sqldriver"
)

// Errors callers detect with [errors.Is].
var (
	// ErrConflict is returned by a transaction commit that lost to a concurrent commit. The
	// transaction had no effect and can be retried.
	ErrConflict = engine.ErrConflict
	// ErrCommitUnknown is returned when the outcome of a commit could not be determined because
	// the store stopped answering. The commit may or may not have been applied.
	ErrCommitUnknown = engine.ErrCommitUnknown
	// ErrSnapshotExpired is returned by a transaction older than the GC grace period, and by
	// [Compact] and [GC] when they took longer than half of it.
	ErrSnapshotExpired = engine.ErrSnapshotExpired
	// ErrCorrupt is returned when an object in the store has damaged bytes: a block of a
	// segment, the manifest, or a garbage batch that fails its checksum or does not decode, or a
	// table definition or row that does not decode or does not hold up. The damaged data is never
	// served. Damaged bytes in the memory or the disk cache are not served either, and the read
	// goes to the store instead.
	ErrCorrupt = engine.ErrCorrupt
	// ErrUnsupported is a statement that Postgres accepts and maragudb does not, such as one
	// that uses a feature maragudb does not have, or one whose result in Postgres would depend
	// on its query plan or on the physical order of rows. Its message says what is unsupported
	// and, where possible, what to write instead. It corresponds to the Postgres SQLSTATE 0A000,
	// feature_not_supported, and it is the only way in which maragudb is stricter than Postgres.
	ErrUnsupported = exec.ErrUnsupported
	// ErrSyntax is a statement that does not parse.
	ErrSyntax = exec.ErrSyntax
	// ErrInvalid is a statement that cannot run: an unknown column or function, wrong arity,
	// a missing parameter, and the like.
	ErrInvalid = exec.ErrInvalid
	// ErrType is a type mismatch, such as an operator or function that does not exist for its
	// argument types.
	ErrType = exec.ErrType
	// ErrNoTable is a reference to a table that does not exist.
	ErrNoTable = exec.ErrNoTable
	// ErrTableExists is a CREATE TABLE or CREATE INDEX of a name that a table or index has.
	// Tables and indexes share one namespace, as in Postgres.
	ErrTableExists = exec.ErrTableExists
	// ErrNoIndex is a DROP INDEX of an index that does not exist.
	ErrNoIndex = exec.ErrNoIndex
	// ErrWrongObjectType is a statement on a relation of the wrong kind, such as a SELECT from an
	// index or a DROP INDEX of a table.
	ErrWrongObjectType = exec.ErrWrongObjectType
	// ErrDependentObjects is a DROP INDEX of an index that a PRIMARY KEY or UNIQUE constraint of
	// its table needs; dropping the table drops it.
	ErrDependentObjects = exec.ErrDependentObjects
	// ErrProgramLimit is a statement beyond a limit of Postgres, such as an index of more than
	// 32 columns.
	ErrProgramLimit = exec.ErrProgramLimit
	// ErrDuplicateKey is an insert or update that would repeat a primary key or the values of a
	// unique index, or a CREATE UNIQUE INDEX on rows that do.
	ErrDuplicateKey = exec.ErrDuplicateKey
	// ErrCheck is an insert or update of a row for which a CHECK constraint is false.
	ErrCheck = exec.ErrCheck
	// ErrDuplicateObject is a constraint whose name another constraint of its table has.
	ErrDuplicateObject = exec.ErrDuplicateObject
	// ErrForeignKey is an insert or update of a row whose foreign key has no row in the
	// referenced table, or an update or delete of a row that a foreign key with NO ACTION still
	// references.
	ErrForeignKey = exec.ErrForeignKey
	// ErrRestrict is an update or delete of a row that a foreign key with RESTRICT references.
	ErrRestrict = exec.ErrRestrict
	// ErrInvalidForeignKey is a foreign key that references columns without a primary key or
	// unique index, or a different number of columns than it has.
	ErrInvalidForeignKey = exec.ErrInvalidForeignKey
	// ErrNotNull is a NULL in a NOT NULL column.
	ErrNotNull = exec.ErrNotNull
	// ErrGrouping is a grouped SELECT that reads a column outside of its GROUP BY expressions and
	// its aggregates, or an aggregate where none is allowed, such as in WHERE or in the arguments
	// of another aggregate.
	ErrGrouping = exec.ErrGrouping
	// ErrCardinality is a scalar subquery that returns more than one row.
	ErrCardinality = exec.ErrCardinality
	// ErrDivisionByZero is a division or modulo by zero.
	ErrDivisionByZero = exec.ErrDivisionByZero
	// ErrOutOfRange is an integer or bigint overflow, a float overflow or underflow, or a cast
	// or timestamp out of range.
	ErrOutOfRange = exec.ErrOutOfRange
	// ErrCast is an invalid cast.
	ErrCast = exec.ErrCast
	// ErrReadOnlyTx is a write in a read-only transaction.
	ErrReadOnlyTx = exec.ErrReadOnlyTx
	// ErrTxAborted is a statement in a transaction in which an earlier statement failed. As in
	// Postgres, every statement fails with it until the transaction ends, and a COMMIT of such a
	// transaction rolls it back.
	ErrTxAborted = exec.ErrTxAborted
	// ErrDSN is an invalid data source name.
	ErrDSN = sqldriver.ErrDSN
	// ErrDiskCacheInUse is a disk cache directory that another process uses. See
	// [Options.DiskCacheDir].
	ErrDiskCacheInUse = cache.ErrInUse
)

// Options for [RegisterDriver] and [NewConnector]. Every field has a default.
type Options struct {
	// Name of the driver for sql.Open. Defaults to "maragudb".
	Name *string
	// Logger for debug and warning messages. Defaults to discarding them.
	Logger *slog.Logger
	// MemoryCacheBytes bounds the in-memory block cache per database. Defaults to 64 MiB.
	// Zero disables it.
	MemoryCacheBytes *int64
	// DiskCacheDir is a directory for a disk cache of the blocks that reads fetch, which gets a
	// subdirectory per DSN. Defaults to no disk cache. Only one process can use the subdirectory
	// of a DSN: opening a database whose subdirectory another process uses fails with
	// [ErrDiskCacheInUse], so give each process a directory of its own. The databases of one DSN
	// in one process share its disk cache.
	DiskCacheDir *string
	// DiskCacheBytes bounds the disk cache of each database, in the bytes its files take up, each
	// rounded up to a block of 4 KiB. The least recently used blocks are evicted to stay within
	// it. Defaults to 1 GiB. Zero turns the disk cache off.
	DiskCacheBytes *int64
	// GCGracePeriod is how long garbage collection keeps an object that a snapshot might still
	// reference, and so the longest a transaction can live. Defaults to one hour. The period is
	// measured on the clocks of the processes that run transactions and garbage collection, so
	// the clock skew between processes must be much smaller than it.
	GCGracePeriod *time.Duration
	// Maintenance runs compaction and garbage collection in the background, for every database
	// opened through the driver, whenever there is work: segments that CompactionFanIn makes due
	// for a merge, or garbage that is new or older than the grace period. Defaults to true. The processes that share a database
	// coordinate through a lease object at <prefix>/maintenance-lease, so that the work of one
	// trigger is done about once rather than once per process. The lease only saves work;
	// correctness never depends on it. A process checks for work on the manifest it already
	// has, so an idle process makes no requests. Closing the sql.DB stops the loop, waits for
	// a running step to stop, and releases the lease, which is one write to the store. With it
	// off, call [Compact] and [GC] now and then instead.
	Maintenance *bool
	// MaintenanceInterval is the time between two checks for maintenance work, with a jitter of
	// half the interval either way. Defaults to 10 seconds.
	MaintenanceInterval *time.Duration
	// CompactionFanIn is the number of segments of similar size that background maintenance
	// merges into one. Every commit adds a small segment, and segments are in tiers by size, each
	// tier CompactionFanIn times larger than the one below it. Maintenance merges CompactionFanIn
	// adjacent segments of one tier into a segment of the next tier, and merges the whole database
	// into one segment when the segments after the oldest one are together as large as it, or
	// delete at least as many of its rows as remain. So a
	// byte is rewritten about once per tier, and the database has a number of segments that grows
	// with the logarithm of its size. Defaults to 4, and is at least 2. A lower fan-in keeps reads
	// cheaper, and a higher one rewrites data less often.
	CompactionFanIn *int
	// Store is the object store for every DSN, in place of the default AWS SDK client for the
	// DSN's bucket. The DSN's bucket is then ignored and its prefix still applies. Any value with
	// these methods can be set:
	//
	//	// Get an object, or the byte range [opts.Offset, opts.Offset+opts.Length) of it when
	//	// opts.Length is positive, and its ETag.
	//	Get(ctx context.Context, key string, opts maragudb.GetOptions) (body []byte, etag string, err error)
	//	// Put an object and return its new ETag.
	//	Put(ctx context.Context, key string, body []byte, opts maragudb.PutOptions) (etag string, err error)
	//	// Delete an object. Deleting a missing object is not an error.
	//	Delete(ctx context.Context, key string) error
	//	// List the keys of the objects whose keys start with prefix, in any order.
	//	List(ctx context.Context, prefix string) ([]string, error)
	//
	// The store must be safe for concurrent use and return [ErrObjectNotFound],
	// [ErrPreconditionFailed], and [ErrNotModified], possibly wrapped, for those outcomes. Any
	// other error means the request failed, and a failed write may or may not have been applied.
	// An ETag is never empty and identifies the content of one version of an object: Put returns
	// the ETag that a later Get of that version returns. A read with [GetOptions.Consistent] must see every
	// write that completed before it started. Correctness depends on all of this.
	Store objectStore
}

func (o Options) withDefaults() Options {
	if o.Name == nil {
		o.Name = new("maragudb")
	}
	if o.GCGracePeriod == nil {
		o.GCGracePeriod = new(time.Hour)
	}
	if o.DiskCacheDir == nil {
		o.DiskCacheDir = new("")
	}
	if o.DiskCacheBytes == nil {
		o.DiskCacheBytes = new(int64(1 << 30))
	}
	if o.Maintenance == nil {
		o.Maintenance = new(true)
	}
	if o.MaintenanceInterval == nil {
		o.MaintenanceInterval = new(10 * time.Second)
	}
	if o.CompactionFanIn == nil {
		o.CompactionFanIn = new(4)
	}
	return o
}

func newDriver(opts Options) *sqldriver.Driver {
	opts = opts.withDefaults()
	var client s3.Client
	if opts.Store != nil {
		client = storeClient{store: opts.Store}
	}
	return sqldriver.New(sqldriver.Options{
		Engine: engine.Options{
			Client:           client,
			Logger:           opts.Logger,
			MemoryCacheBytes: opts.MemoryCacheBytes,
			GCGracePeriod:    opts.GCGracePeriod,
		},
		Exec:           exec.Options{Logger: opts.Logger},
		DiskCacheDir:   *opts.DiskCacheDir,
		DiskCacheBytes: *opts.DiskCacheBytes,
		Maintenance: sqldriver.MaintenanceOptions{
			Disabled: !*opts.Maintenance,
			Interval: *opts.MaintenanceInterval,
			Engine:   engine.MaintenanceOptions{Tiers: engine.Tiers{FanIn: *opts.CompactionFanIn}},
		},
		Logger: opts.Logger,
		NewClient: func(ctx context.Context, bucket string) (s3.Client, error) {
			return s3.NewAWSClient(ctx, s3.AWSOptions{Bucket: bucket})
		},
	})
}

// RegisterDriver with [database/sql] under opts.Name. It panics when the name is registered
// already, as sql.Register does.
func RegisterDriver(opts Options) {
	opts = opts.withDefaults()
	sql.Register(*opts.Name, newDriver(opts))
}

// NewConnector for sql.OpenDB, which needs no registration. The DSN is s3://bucket/prefix.
func NewConnector(opts Options, dsn string) (driver.Connector, error) {
	return sqldriver.NewConnector(newDriver(opts), dsn)
}

// Statistics of a database.
type Statistics struct {
	// Requests to the object store so far, by kind.
	Requests Requests
	// Cache hits and misses per tier.
	MemoryCacheHits, MemoryCacheMisses int64
	DiskCacheHits, DiskCacheMisses     int64
	// Seq is the latest known commit sequence number.
	Seq uint64
	// Segments referenced by the latest known manifest.
	Segments int
	// Garbage objects awaiting their grace period.
	Garbage int
	// MaintenanceRuns counts the background maintenance steps that compacted or collected
	// garbage, LeaseAcquisitions the times this process acquired the maintenance lease, and
	// LeaseLosses the times another process took it over after it expired.
	MaintenanceRuns, LeaseAcquisitions, LeaseLosses int64
}

// Requests to the object store, by kind. Conditional reads are freshness checks, and
// conditional writes are commits.
type Requests struct {
	Get, GetConditional int64
	Put, PutConditional int64
	Delete, List        int64
}

// Total requests.
func (r Requests) Total() int64 {
	return r.Get + r.GetConditional + r.Put + r.PutConditional + r.Delete + r.List
}

// Add returns r plus o.
func (r Requests) Add(o Requests) Requests {
	return Requests{
		Get: r.Get + o.Get, GetConditional: r.GetConditional + o.GetConditional,
		Put: r.Put + o.Put, PutConditional: r.PutConditional + o.PutConditional,
		Delete: r.Delete + o.Delete, List: r.List + o.List,
	}
}

// Sub returns r minus o, for measuring an operation.
func (r Requests) Sub(o Requests) Requests {
	return Requests{
		Get: r.Get - o.Get, GetConditional: r.GetConditional - o.GetConditional,
		Put: r.Put - o.Put, PutConditional: r.PutConditional - o.PutConditional,
		Delete: r.Delete - o.Delete, List: r.List - o.List,
	}
}

// Stats of the database behind db, which must have been opened with this driver. The
// statistics cover every connection to the same DSN through the same driver.
func Stats(ctx context.Context, db *sql.DB) (Statistics, error) {
	var s Statistics
	err := withConn(ctx, db, func(c *sqldriver.Conn) error {
		es := c.DB().Stats()
		s = Statistics{
			Requests: Requests{
				Get: es.S3.Get, GetConditional: es.S3.GetConditional,
				Put: es.S3.Put, PutConditional: es.S3.PutConditional,
				Delete: es.S3.Delete, List: es.S3.List,
			},
			MemoryCacheHits: es.MemoryCacheHits, MemoryCacheMisses: es.MemoryCacheMisses,
			DiskCacheHits: es.DiskCacheHits, DiskCacheMisses: es.DiskCacheMisses,
			Seq: es.Seq, Segments: es.Segments, Garbage: es.Garbage,
			MaintenanceRuns: es.MaintenanceRuns, LeaseAcquisitions: es.LeaseAcquisitions, LeaseLosses: es.LeaseLosses,
		}
		return nil
	})
	return s, err
}

// withConn runs fn with the driver connection behind db.
func withConn(ctx context.Context, db *sql.DB, fn func(c *sqldriver.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(func(dc any) error {
		c, ok := dc.(*sqldriver.Conn)
		if !ok {
			return fmt.Errorf("maragudb: %T is not a maragudb connection", dc)
		}
		return fn(c)
	})
}

// Compact the database behind db: every segment object is merged into one, whatever the tiers
// of [Options.CompactionFanIn], and the old ones become garbage for [GC]. Commits that happen
// meanwhile are kept ahead of the merged segment, and only a competing compaction makes it return
// [ErrConflict], after which it can be retried. It returns [ErrSnapshotExpired] when it took
// longer than half the grace period. It is safe to run while other processes use the database.
// Background maintenance merges segments by tiers on its own unless [Options.Maintenance] is
// off, and merges every segment when the newer segments are together as large as the oldest,
// or delete at least as many of its rows as remain.
func Compact(ctx context.Context, db *sql.DB) error {
	return withConn(ctx, db, func(c *sqldriver.Conn) error {
		return c.DB().Compact(ctx)
	})
}

// GC deletes objects that no transaction within the grace period can reference, and returns how
// many it deleted. Commits that happen meanwhile do not fail it; it returns [ErrConflict] only
// when it kept losing to them, or to another GC pass, and [ErrSnapshotExpired] when it took longer
// than half the grace period, and it can be retried after either. Background maintenance runs it
// on its own unless [Options.Maintenance] is off.
func GC(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := withConn(ctx, db, func(c *sqldriver.Conn) error {
		var err error
		n, err = c.DB().GC(ctx)
		return err
	})
	return n, err
}

// IsRetryable reports whether err is a conflict that a retry of the transaction can resolve.
func IsRetryable(err error) bool {
	return errors.Is(err, ErrConflict)
}
