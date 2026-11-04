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
	"strings"
	"sync"
	"time"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sql/exec"
	"maragu.dev/maragudb/internal/sql/value"
)

// ErrDSN is an invalid data source name.
var ErrDSN = errors.New("sqldriver: invalid DSN, want s3://bucket/prefix")

// Options for a [Driver].
type Options struct {
	// Engine options. Client is used for every DSN when set, otherwise NewClient opens one for the
	// DSN's bucket. Prefix comes from the DSN.
	Engine engine.Options
	// Exec options for sessions.
	Exec exec.Options
	// NewClient opens an object store client for a bucket. Required when Engine.Client is nil.
	NewClient func(ctx context.Context, bucket string) (s3.Client, error)
	// DiskCacheDir holds a disk cache per DSN when set.
	DiskCacheDir string
	// Maintenance in the background for every database.
	Maintenance MaintenanceOptions
	Logger      *slog.Logger
}

// MaintenanceOptions for the background loop that compacts and garbage collects a database.
type MaintenanceOptions struct {
	// Disabled turns the loop off.
	Disabled bool
	// Interval between checks for work. Each wait is drawn uniformly from the half-open interval
	// [Interval/2, 3*Interval/2), so that the clients of one database do not check in lockstep.
	// Defaults to 10 seconds.
	Interval time.Duration
	// Engine options of the maintainer: the thresholds and the lease.
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

// Open a connection, for [driver.Driver].
func (d *Driver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
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
	if opts.DiskCache == nil && c.d.opts.DiskCacheDir != "" {
		sum := sha256.Sum256([]byte(c.dsn))
		disk, err := cache.NewDisk(filepath.Join(c.d.opts.DiskCacheDir, hex.EncodeToString(sum[:8])))
		if err != nil {
			return nil, fmt.Errorf("sqldriver: opening disk cache: %w", err)
		}
		opts.DiskCache = disk
	}
	db, err := engine.Open(ctx, opts)
	if err != nil {
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
		if res, err := c.maintainer.Step(ctx); err != nil && ctx.Err() == nil {
			c.d.opts.Logger.Warn("background maintenance failed", "dsn", c.dsn, "error", err)
		} else if res.Ran {
			c.d.opts.Logger.Debug("background maintenance ran", "dsn", c.dsn, "compacted", res.Compacted, "deleted", res.Deleted)
		}
	}
}

// Connect opens a connection.
func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	db, err := c.DB(ctx)
	if err != nil {
		return nil, err
	}
	return &Conn{db: db, session: exec.NewSession(db, c.d.opts.Exec)}, nil
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
	return err
}

var _ io.Closer = (*Connector)(nil)

// Conn is one connection with its own session and transaction state.
type Conn struct {
	db      *engine.DB
	session *exec.Session
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
func (c *Conn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	st, err := exec.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &Stmt{conn: c, st: st}, nil
}

// Close the connection.
func (c *Conn) Close() error {
	if c.session.InTx() {
		return c.session.Rollback()
	}
	return nil
}

// Begin a transaction, for [driver.Conn].
func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx begins a transaction. Read-only transactions never conflict. The isolation level must
// be the default or serializable, which is the only level there is.
func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
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
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	st, err := exec.Prepare(query)
	if err != nil {
		return nil, err
	}
	return c.exec(ctx, st, args)
}

// QueryContext runs a query without preparing it separately.
func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	st, err := exec.Prepare(query)
	if err != nil {
		return nil, err
	}
	return c.query(ctx, st, args)
}

func (c *Conn) exec(ctx context.Context, st *exec.Statement, args []driver.NamedValue) (driver.Result, error) {
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

func (c *Conn) query(ctx context.Context, st *exec.Statement, args []driver.NamedValue) (driver.Rows, error) {
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
	return &Rows{rows: rows}, nil
}

// CheckNamedValue accepts the supported Go parameter types without converting them.
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

// Ping checks that the database is reachable.
func (c *Conn) Ping(ctx context.Context) error {
	tx, err := c.db.BeginReadOnly(ctx)
	if err != nil {
		return err
	}
	return tx.Rollback()
}

// ResetSession rolls back a transaction left open with a SQL BEGIN and discards the connection.
func (c *Conn) ResetSession(context.Context) error {
	if c.session.InTx() {
		_ = c.session.Rollback()
		return driver.ErrBadConn
	}
	return nil
}

// IsValid reports whether the connection can be reused.
func (c *Conn) IsValid() bool {
	return true
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
			params[i] = value.NewString(v)
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
// concurrent commit.
func (t *Tx) Commit() error {
	return t.conn.session.Commit(t.ctx)
}

// Rollback the transaction.
func (t *Tx) Rollback() error {
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
func (r *Rows) Next(dest []driver.Value) error {
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
	case value.Int:
		return v.I
	case value.Float:
		return v.F
	case value.Bool:
		return v.B
	case value.String:
		return v.S
	case value.Bytes:
		return v.Raw
	case value.Time:
		return v.T
	}
	return nil
}

// ColumnTypeDatabaseTypeName is the SQL type name in upper case, or empty for a NULL of unknown type.
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string {
	if r.rows.Columns[i].Type == value.Unknown {
		return ""
	}
	return strings.ToUpper(r.rows.Columns[i].Type.String())
}

// ColumnTypeNullable reports whether the column can be NULL.
func (r *Rows) ColumnTypeNullable(i int) (bool, bool) {
	return r.rows.Columns[i].Nullable, true
}

// ColumnTypeScanType is the Go type the column scans into.
func (r *Rows) ColumnTypeScanType(i int) reflect.Type {
	switch r.rows.Columns[i].Type {
	case value.Int:
		return reflect.TypeFor[int64]()
	case value.Float:
		return reflect.TypeFor[float64]()
	case value.Bool:
		return reflect.TypeFor[bool]()
	case value.String:
		return reflect.TypeFor[string]()
	case value.Bytes:
		return reflect.TypeFor[[]byte]()
	case value.Time:
		return reflect.TypeFor[time.Time]()
	}
	return reflect.TypeFor[any]()
}
