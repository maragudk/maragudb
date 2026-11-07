// Package sqldriver implements a [database/sql/driver] over the SQL executor. One connector holds
// one storage engine per DSN, which every connection shares, so caches and statistics are per
// database.
package sqldriver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/url"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sql/value"
)

// ErrDSN is an invalid data source name.
var ErrDSN = errors.New("sqldriver: invalid DSN, want s3://bucket/prefix")

// ErrInternal is a bug in maragudb: a call into the driver panicked, and the driver recovered the
// panic. The message holds the panic's value. The connection is discarded, and a transaction in
// which a call panicked rolls back. A statement outside a transaction, or a commit, that fails
// with it may or may not have taken effect, as with [engine.ErrCommitUnknown].
var ErrInternal = errors.New("sqldriver: internal error, a bug in maragudb")

// errBroken is the error of a call on a connection that an earlier call broke with a panic.
var errBroken = fmt.Errorf("%w: an earlier call on the connection panicked, and the connection is discarded", ErrInternal)

// Options for a [Driver].
type Options struct {
	// Engine options. Client is used for every DSN when set, otherwise NewClient opens one for the
	// DSN's bucket. Prefix comes from the DSN.
	Engine engine.Options
	// Exec options for sessions.
	Exec exec.Options
	// NewClient opens an object store client for a bucket. Required when Engine.Client is nil.
	NewClient func(ctx context.Context, bucket string) (s3.Client, error)
	// DiskCacheDir holds a disk cache per DSN when set, in a subdirectory of its own.
	DiskCacheDir string
	// DiskCacheBytes bounds each disk cache, as [cache.OpenDir] counts them. A disk cache needs a
	// positive bound, and zero turns the disk cache off.
	DiskCacheBytes int64
	// Maintenance in the background for every database.
	Maintenance MaintenanceOptions
	Logger      *slog.Logger
	// OnPanic, when set, is called with the value and the stack of every panic that the driver
	// recovers, after the panic is logged. The driver recovers a panic in every call into a
	// connector, a connection, a statement, a transaction, or rows, so that it never unwinds
	// through database/sql, whose locks it would leave held. The call returns an error that wraps
	// [ErrInternal], and the connection is broken: its transaction cannot commit, and database/sql
	// discards it.
	OnPanic func(value any, stack []byte)
}

// MaintenanceOptions for the background loop that compacts and garbage collects a database.
type MaintenanceOptions struct {
	// Disabled turns the loop off.
	Disabled bool
	// Interval between checks for work. Each wait is drawn uniformly from the half-open interval
	// [Interval/2, 3*Interval/2), so that the clients of one database do not check in lockstep.
	// Defaults to 10 seconds.
	Interval time.Duration
	// Engine options of the maintainer: the tiers of compaction and the lease.
	Engine engine.MaintenanceOptions
}

// Driver implements [driver.Driver] and [driver.DriverContext].
type Driver struct {
	opts Options
}

// New driver.
func New(opts Options) *Driver {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Driver{opts: opts}
}

// Open a connection, for [driver.Driver]. The connection owns its connector and closes it,
// with the connector's background maintenance, when it is closed.
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	c, err := NewConnector(d, dsn)
	if err != nil {
		return nil, err
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	conn.(*Conn).owner = c
	return conn, nil
}

// OpenConnector for a DSN, for [driver.DriverContext].
func (d *Driver) OpenConnector(dsn string) (driver.Connector, error) {
	return NewConnector(d, dsn)
}

// ParseDSN splits s3://bucket/prefix into bucket and prefix.
func ParseDSN(dsn string) (bucket, prefix string, err error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "s3" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("%w: %q", ErrDSN, dsn)
	}
	return u.Host, strings.TrimPrefix(u.Path, "/"), nil
}

// Connector opens connections to one database. It implements [driver.Connector] and [io.Closer].
type Connector struct {
	d      *Driver
	dsn    string
	bucket string
	prefix string

	mu sync.Mutex
	db *engine.DB
	// maintainer and the loop that drives it, when maintenance is on.
	maintainer *engine.Maintainer
	stop       context.CancelFunc
	stopped    chan struct{}
	// closeDisk closes the disk cache that the connector opened, if any.
	closeDisk func()
}

// NewConnector for a DSN.
func NewConnector(d *Driver, dsn string) (*Connector, error) {
	bucket, prefix, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &Connector{d: d, dsn: dsn, bucket: bucket, prefix: prefix}, nil
}

// DB opens the storage engine on first use and returns it.
func (c *Connector) DB(ctx context.Context) (*engine.DB, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db != nil {
		return c.db, nil
	}
	opts := c.d.opts.Engine
	opts.Prefix = c.prefix
	if opts.Logger == nil {
		opts.Logger = c.d.opts.Logger
	}
	if opts.Client == nil {
		if c.d.opts.NewClient == nil {
			return nil, errors.New("sqldriver: no object store client")
		}
		client, err := c.d.opts.NewClient(ctx, c.bucket)
		if err != nil {
			return nil, err
		}
		opts.Client = client
	}
	if opts.DiskCache == nil && c.d.opts.DiskCacheDir != "" && c.d.opts.DiskCacheBytes != 0 {
		sum := sha256.Sum256([]byte(c.dsn))
		disk, closeDisk, err := cache.OpenDir(filepath.Join(c.d.opts.DiskCacheDir, hex.EncodeToString(sum[:8])), c.d.opts.DiskCacheBytes)
		if err != nil {
			return nil, fmt.Errorf("sqldriver: opening disk cache: %w", err)
		}
		opts.DiskCache = disk
		c.closeDisk = closeDisk
	}
	db, err := engine.Open(ctx, opts)
	if err != nil {
		c.releaseDisk()
		return nil, err
	}
	c.db = db
	if !c.d.opts.Maintenance.Disabled {
		c.maintainer = db.NewMaintainer(c.d.opts.Maintenance.Engine)
		loopCtx, cancel := context.WithCancel(context.Background())
		c.stop = cancel
		c.stopped = make(chan struct{})
		go c.maintain(loopCtx)
	}
	return db, nil
}

// maintain runs maintenance steps at jittered intervals until ctx ends.
func (c *Connector) maintain(ctx context.Context) {
	defer close(c.stopped)
	interval := c.d.opts.Maintenance.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	for {
		wait := interval/2 + rand.N(interval)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		res, err := c.maintainer.Step(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
		case errors.Is(err, engine.ErrConflict), errors.Is(err, engine.ErrSnapshotExpired), errors.Is(err, engine.ErrLeaseLost):
			// Another process did the work, or is doing it; the next step sees the result.
			c.d.opts.Logger.Debug("background maintenance yielded", "dsn", c.dsn, "error", err)
		case err != nil:
			c.d.opts.Logger.Warn("background maintenance failed", "dsn", c.dsn, "error", err)
		case res.Ran:
			c.d.opts.Logger.Debug("background maintenance ran", "dsn", c.dsn, "compacted", res.Compacted, "deleted", res.Deleted)
		}
	}
}

// Connect opens a connection.
func (c *Connector) Connect(ctx context.Context) (_ driver.Conn, err error) {
	defer c.d.recoverPanic(&err)
	db, err := c.DB(ctx)
	if err != nil {
		return nil, err
	}
	return &Conn{d: c.d, db: db, session: exec.NewSession(db, c.d.opts.Exec)}, nil
}

// recoverPanic, deferred in a call into the driver, recovers a panic as [Driver.recovered]
// describes.
func (d *Driver) recoverPanic(err *error) {
	if p := recover(); p != nil {
		d.recovered(p, err)
	}
}

// recovered handles the panic p that a deferred function recovered: it logs the panic with the
// stack where it happened, tells [Options.OnPanic], and sets err to an error that wraps
// [ErrInternal]. It runs in the deferred function, before the stack unwinds.
func (d *Driver) recovered(p any, err *error) {
	stack := debug.Stack()
	d.opts.Logger.Error("sqldriver: recovered a panic in a call into the driver, which is a bug in maragudb; the connection is discarded",
		"panic", fmt.Sprint(p), "stack", string(stack))
	assert.Unreachable("a call into the driver panics", map[string]any{"panic": fmt.Sprint(p), "stack": string(stack)})
	if d.opts.OnPanic != nil {
		d.opts.OnPanic(p, stack)
	}
	// The panic's value is in the message only, never wrapped: a value that wraps
	// driver.ErrBadConn would make database/sql run the statement again.
	*err = fmt.Errorf("%w: panic: %v", ErrInternal, p)
}

// Driver of the connector.
func (c *Connector) Driver() driver.Driver {
	return c.d
}

// Close the connector. It stops background maintenance, waiting for a running step to notice,
// and releases the maintenance lease if it is held.
func (c *Connector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	var err error
	if c.maintainer != nil {
		c.stop()
		<-c.stopped
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = c.maintainer.Close(ctx)
		cancel()
		c.maintainer = nil
	}
	if cerr := c.db.Close(); cerr != nil && err == nil {
		err = cerr
	}
	c.db = nil
	c.releaseDisk()
	return err
}

// releaseDisk closes the disk cache of the connector, if it has one. c.mu must be held.
func (c *Connector) releaseDisk() {
	if c.closeDisk != nil {
		c.closeDisk()
		c.closeDisk = nil
	}
}

var _ io.Closer = (*Connector)(nil)

// Conn is one connection with its own session and transaction state.
type Conn struct {
	d       *Driver
	db      *engine.DB
	session *exec.Session
	// owner is the connector to close with the connection, when the connection was opened
	// without one.
	owner *Connector
	// broken is set once a call on the connection panicked. The session may be in any state
	// then, so the connection runs nothing more: database/sql discards it, and its transaction
	// rolls back.
	broken bool
}

// recoverPanic, deferred in every call on the connection, recovers a panic as
// [Driver.recoverPanic] does and breaks the connection.
func (c *Conn) recoverPanic(err *error) {
	if p := recover(); p != nil {
		c.broken = true
		c.d.recovered(p, err)
	}
}

// DB is the storage engine the connection belongs to.
func (c *Conn) DB() *engine.DB {
	return c.db
}

// Prepare a statement.
func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext parses the statement.
func (c *Conn) PrepareContext(_ context.Context, query string) (_ driver.Stmt, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, errBroken
	}
	st, err := c.session.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{conn: c, st: st}, nil
}

// Close the connection.
func (c *Conn) Close() (err error) {
	defer c.recoverPanic(&err)
	if c.session.InTx() {
		err = c.session.Rollback()
	}
	if c.owner != nil {
		if cerr := c.owner.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}

// Begin a transaction, for [driver.Conn].
func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx begins a transaction. Read-only transactions never conflict. The isolation level must
// be the default or serializable, which is the only level there is. On a broken connection it
// returns [driver.ErrBadConn], with which database/sql begins on another connection, because
// nothing has happened yet.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (_ driver.Tx, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, driver.ErrBadConn
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelSerializable:
	default:
		return nil, fmt.Errorf("sqldriver: isolation level %v is not supported, transactions are serializable", sql.IsolationLevel(opts.Isolation))
	}
	if err := c.session.Begin(ctx, opts.ReadOnly); err != nil {
		return nil, err
	}
	return &Tx{conn: c, ctx: ctx}, nil
}

// ExecContext runs a statement without preparing it separately.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Result, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, errBroken
	}
	st, err := c.session.Prepare(query)
	if err != nil {
		return nil, err
	}
	return c.exec(ctx, st, args)
}

// QueryContext runs a query without preparing it separately.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (_ driver.Rows, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, errBroken
	}
	st, err := c.session.Prepare(query)
	if err != nil {
		return nil, err
	}
	return c.query(ctx, st, args)
}

// exec runs a statement. A broken connection fails it with [errBroken], never with
// [driver.ErrBadConn]: database/sql discards a broken connection as soon as it is released, so
// only a transaction or a [sql.Conn] that holds it gets here, and its caller has seen the panic.
func (c *Conn) exec(ctx context.Context, st *exec.Statement, args []driver.NamedValue) (_ driver.Result, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, errBroken
	}
	params, err := toValues(st, args)
	if err != nil {
		return nil, err
	}
	_, res, err := c.session.Execute(ctx, st, params)
	if err != nil {
		return nil, err
	}
	return Result{affected: res.RowsAffected}, nil
}

func (c *Conn) query(ctx context.Context, st *exec.Statement, args []driver.NamedValue) (_ driver.Rows, err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return nil, errBroken
	}
	params, err := toValues(st, args)
	if err != nil {
		return nil, err
	}
	rows, _, err := c.session.Execute(ctx, st, params)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = &exec.Rows{}
	}
	return &Rows{conn: c, rows: rows}, nil
}

// CheckNamedValue accepts the supported Go parameter types without converting them. It calls the
// Value method of a [driver.Valuer], whose panic is the caller's own and is not recovered.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	if nv.Name != "" {
		return errors.New("sqldriver: named parameters are not supported, use $1")
	}
	v := nv.Value
	if valuer, ok := v.(driver.Valuer); ok {
		// A nil pointer whose Value method has a value receiver is NULL, as database/sql
		// treats it, instead of a panic.
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer && rv.IsNil() && rv.Type().Elem().Implements(reflect.TypeFor[driver.Valuer]()) {
			v = nil
		} else {
			var err error
			if v, err = valuer.Value(); err != nil {
				return err
			}
		}
	}
	switch v.(type) {
	case nil, int64, int, float64, bool, string, []byte, time.Time:
		nv.Value = v
		return nil
	}
	return fmt.Errorf("sqldriver: unsupported parameter type %T, want int64, int, float64, bool, string, []byte, time.Time, or nil", nv.Value)
}

// Ping checks that the database is reachable. A broken connection gives [driver.ErrBadConn].
func (c *Conn) Ping(ctx context.Context) (err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return driver.ErrBadConn
	}
	tx, err := c.db.BeginReadOnly(ctx)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

// ResetSession rolls back a transaction left open with a SQL BEGIN and discards the connection,
// as it discards a broken one. database/sql resets a connection before it reuses it, so the
// [driver.ErrBadConn] makes it take another before anything runs.
func (c *Conn) ResetSession(context.Context) (err error) {
	defer c.recoverPanic(&err)
	if c.broken {
		return driver.ErrBadConn
	}
	if c.session.InTx() {
		_ = c.session.Rollback()
		return driver.ErrBadConn
	}
	return nil
}

// IsValid reports whether the connection can be reused, which it cannot once it recovered a
// panic.
func (c *Conn) IsValid() bool {
	return !c.broken
}

func toValues(st *exec.Statement, args []driver.NamedValue) ([]value.Value, error) {
	if len(args) != st.NumParams() {
		return nil, fmt.Errorf("sqldriver: statement has %d parameters, %d given", st.NumParams(), len(args))
	}
	params := make([]value.Value, len(args))
	for i, a := range args {
		if a.Ordinal != i+1 {
			return nil, fmt.Errorf("sqldriver: parameter %d has ordinal %d", i+1, a.Ordinal)
		}
		switch v := a.Value.(type) {
		case nil:
			params[i] = value.NewNull(value.Unknown)
		case int64:
			params[i] = value.NewInt(v)
		case int:
			params[i] = value.NewInt(int64(v))
		case float64:
			params[i] = value.NewFloat(v)
		case bool:
			params[i] = value.NewBool(v)
		case string:
			// A string is text of no type yet, which takes the type the parameter's context
			// infers, as a quoted literal does.
			params[i] = value.NewUnknown(v)
		case []byte:
			if v == nil {
				// A nil slice is NULL, as in the common Postgres drivers.
				params[i] = value.NewNull(value.Unknown)
				continue
			}
			params[i] = value.NewBytes(append([]byte{}, v...))
		case time.Time:
			params[i] = value.NewTime(v)
		default:
			return nil, fmt.Errorf("sqldriver: unsupported parameter type %T", a.Value)
		}
	}
	return params, nil
}

// Stmt is a prepared statement.
type Stmt struct {
	conn *Conn
	st   *exec.Statement
}

// Close the statement.
func (s *Stmt) Close() error {
	return nil
}

// NumInput is the number of parameters.
func (s *Stmt) NumInput() int {
	return s.st.NumParams()
}

// Exec the statement, for [driver.Stmt].
func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

// Query the statement, for [driver.Stmt].
func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

// ExecContext runs the statement.
func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.exec(ctx, s.st, args)
}

// QueryContext runs the statement.
func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.query(ctx, s.st, args)
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, a := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return out
}

// Tx is an open transaction. Its commit runs in the context the transaction began with, because
// [driver.Tx] has no context of its own.
type Tx struct {
	conn *Conn
	ctx  context.Context
}

// Commit the transaction. It returns [engine.ErrConflict] when the transaction lost to a
// concurrent commit. When a call in the transaction panicked, it rolls the transaction back and
// returns an error that wraps [ErrInternal].
func (t *Tx) Commit() (err error) {
	defer t.conn.recoverPanic(&err)
	if t.conn.broken {
		_ = t.conn.session.Rollback()
		return fmt.Errorf("%w: the transaction was rolled back, because a call in it panicked", ErrInternal)
	}
	return t.conn.session.Commit(t.ctx)
}

// Rollback the transaction.
func (t *Tx) Rollback() (err error) {
	defer t.conn.recoverPanic(&err)
	if t.conn.broken {
		// The transaction may have ended in the call that panicked.
		_ = t.conn.session.Rollback()
		return nil
	}
	return t.conn.session.Rollback()
}

// Result of a statement.
type Result struct {
	affected int64
}

// LastInsertId is not supported, because there is no hidden row id.
func (Result) LastInsertId() (int64, error) {
	return 0, errors.New("sqldriver: LastInsertId is not supported")
}

// RowsAffected by the statement.
func (r Result) RowsAffected() (int64, error) {
	return r.affected, nil
}

// Rows of a query.
type Rows struct {
	conn *Conn
	rows *exec.Rows
	next int
}

// Columns names.
func (r *Rows) Columns() []string {
	names := make([]string, len(r.rows.Columns))
	for i, c := range r.rows.Columns {
		names[i] = c.Name
	}
	return names
}

// Close the rows.
func (r *Rows) Close() error {
	return nil
}

// Next row into dest.
func (r *Rows) Next(dest []driver.Value) (err error) {
	defer r.conn.recoverPanic(&err)
	if r.next >= len(r.rows.Rows) {
		return io.EOF
	}
	row := r.rows.Rows[r.next]
	r.next++
	for i, v := range row {
		dest[i] = toDriverValue(v)
	}
	return nil
}

func toDriverValue(v value.Value) driver.Value {
	if v.Null {
		return nil
	}
	switch v.Type {
	case value.Int, value.Int4:
		return v.I
	case value.Float:
		return v.F
	case value.Bool:
		return v.B
	case value.String, value.Decimal:
		// A numeric is its text, as Postgres drivers give it.
		return v.S
	case value.Bytes:
		return v.Raw
	case value.Time:
		return v.T
	}
	return nil
}

// ColumnTypeDatabaseTypeName is the Postgres type name in upper case, such as INT8 or
// TIMESTAMPTZ, as the Postgres drivers report it.
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(r.rows.Columns[i].Type.OutputName())
}

// ColumnTypeNullable reports whether the column can be NULL.
func (r *Rows) ColumnTypeNullable(i int) (bool, bool) {
	return r.rows.Columns[i].Nullable, true
}

// ColumnTypeScanType is the Go type the column scans into.
func (r *Rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.rows.Columns[i].Type {
	case value.Int, value.Int4:
		return reflect.TypeFor[int64]()
	case value.Float:
		return reflect.TypeFor[float64]()
	case value.Bool:
		return reflect.TypeFor[bool]()
	case value.String, value.Decimal:
		return reflect.TypeFor[string]()
	case value.Bytes:
		return reflect.TypeFor[[]byte]()
	case value.Time:
		return reflect.TypeFor[time.Time]()
	}
	return reflect.TypeFor[any]()
}
