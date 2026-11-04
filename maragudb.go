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
// Placeholders are $1, $2, and so on. Parameters are int64 and int, float64, bool, string,
// []byte, time.Time, and nil. A nil []byte is NULL. A parameter of the wrong type for its column
// is an error and is never converted.
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
	// ErrSnapshotExpired is returned by a transaction older than the GC grace period.
	ErrSnapshotExpired = engine.ErrSnapshotExpired
	// ErrSyntax is a statement that does not parse.
	ErrSyntax = exec.ErrSyntax
	// ErrInvalid is a statement that cannot run: an unknown column or function, wrong arity,
	// a missing parameter, and the like.
	ErrInvalid = exec.ErrInvalid
	// ErrType is a type mismatch, including a parameter of the wrong type.
	ErrType = exec.ErrType
	// ErrNoTable is a reference to a table that does not exist.
	ErrNoTable = exec.ErrNoTable
	// ErrTableExists is a CREATE TABLE of a table that exists.
	ErrTableExists = exec.ErrTableExists
	// ErrDuplicateKey is an insert or update that would repeat a primary key.
	ErrDuplicateKey = exec.ErrDuplicateKey
	// ErrNotNull is a NULL in a NOT NULL column.
	ErrNotNull = exec.ErrNotNull
	// ErrDivisionByZero is a division or modulo by zero.
	ErrDivisionByZero = exec.ErrDivisionByZero
	// ErrOutOfRange is an int overflow, a float overflow or underflow, or a cast out of range.
	ErrOutOfRange = exec.ErrOutOfRange
	// ErrCast is an invalid cast.
	ErrCast = exec.ErrCast
	// ErrReadOnlyTx is a write in a read-only transaction.
	ErrReadOnlyTx = exec.ErrReadOnlyTx
	// ErrDSN is an invalid data source name.
	ErrDSN = sqldriver.ErrDSN
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
	// DiskCacheDir is a directory for a disk cache, which gets a subdirectory per DSN. Defaults
	// to no disk cache. The disk cache never evicts.
	DiskCacheDir *string
	// GCGracePeriod is how long garbage collection keeps an object that a snapshot might still
	// reference, and so the longest a transaction can live. Defaults to one hour. The period is
	// measured on the clocks of the processes that run transactions and garbage collection, so
	// the clock skew between processes must be much smaller than it.
	GCGracePeriod *time.Duration
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
		Exec:         exec.Options{Logger: opts.Logger},
		DiskCacheDir: *opts.DiskCacheDir,
		Logger:       opts.Logger,
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

// Compact the database behind db: every segment object is merged into one, and the old ones
// become garbage for [GC]. It returns [ErrConflict] when a commit happened meanwhile, and can
// then be retried. It is safe to run while other processes use the database.
func Compact(ctx context.Context, db *sql.DB) error {
	return withConn(ctx, db, func(c *sqldriver.Conn) error {
		return c.DB().Compact(ctx)
	})
}

// GC deletes objects that no transaction within the grace period can reference, and returns how
// many it deleted. It returns [ErrConflict] when a commit happened meanwhile, and can then be
// retried.
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
