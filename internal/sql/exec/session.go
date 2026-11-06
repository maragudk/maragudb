// Package exec runs SQL statements against the storage engine: it binds and type-checks
// expressions, plans primary key access, executes statements with statement-level atomicity, and
// manages transactions and autocommit retries for a session.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/sql/ast"
	"maragu.dev/maragudb/internal/sql/parser"
	"maragu.dev/maragudb/internal/sql/value"
)

// Options for a [Session].
type Options struct {
	// Logger for debug messages. Discarded when nil.
	Logger *slog.Logger
	// Mutations deliberately break the executor. For tests only.
	Mutations Mutations
}

// Mutations are switches that break the executor on purpose, so that a test can prove it would
// notice each kind of bug. All of them are off in correct operation.
type Mutations struct {
	// RetryAfterUnknownCommit retries an autocommit statement whose commit outcome is unknown,
	// which can apply it twice.
	RetryAfterUnknownCommit bool
	// FreshSnapshotPerStatement starts every statement of an explicit transaction that has not
	// written yet from the latest commit instead of the transaction's snapshot.
	FreshSnapshotPerStatement bool
	// UpdateSeesOwnWrites makes UPDATE restart its scan after every row it writes, so it can
	// visit a row it has already changed, which is the Halloween problem.
	UpdateSeesOwnWrites bool
	// IndexStaleDelete leaves the old index entries of a row that an UPDATE changes or a DELETE
	// removes.
	IndexStaleDelete bool
	// UniqueNoCheck skips the check that no other row has a row's entry in a unique index.
	UniqueNoCheck bool
	// UniqueOwnWrites makes the check of a unique entry read the transaction without the
	// statement's own writes, so that it misses a duplicate within one statement.
	UniqueOwnWrites bool
	// CheckSkipUpdate checks the CHECK constraints of a row only when it is inserted.
	CheckSkipUpdate bool
	// ForeignKeyNoChildCheck skips the check that an inserted or updated row has a parent row.
	ForeignKeyNoChildCheck bool
	// ForeignKeyNoParentCheck skips the checks and actions on the rows that reference a deleted or
	// updated row.
	ForeignKeyNoParentCheck bool
	// ForeignKeyOwnWrites makes the lookups of foreign keys read the transaction without the
	// statement's own writes.
	ForeignKeyOwnWrites bool
}

// Session runs statements for one client, serially. It is not safe for concurrent use.
type Session struct {
	db   *engine.DB
	opts Options
	log  *slog.Logger
	// tx is the current transaction, or nil between autocommit statements.
	tx       *engine.Tx
	explicit bool
	readOnly bool
	// wrote reports whether the transaction has applied any writes.
	wrote bool
	// aborted is set when a statement of the explicit transaction failed. Every later statement
	// fails with [ErrTxAborted] until the transaction ends, as in Postgres.
	aborted bool
	// tables caches definitions read in the transaction. A nil entry is a known-missing table.
	tables map[string]*table
	// checks caches the bound CHECK constraints of the tables in tables.
	checks map[*table][]boundCheck
	// events are the rows the statement wrote, for its foreign keys, and cascaded the tables in
	// which its actions deleted rows.
	events   []rowEvent
	cascaded map[string]bool
	kv       kvstore
}

// NewSession over db.
func NewSession(db *engine.DB, opts Options) *Session {
	s := &Session{db: db, opts: opts, log: opts.Logger}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s
}

// Statement is a parsed statement.
type Statement struct {
	ast     ast.Statement
	nparams int
}

// NumParams is the highest parameter number used.
func (st *Statement) NumParams() int {
	return st.nparams
}

// Prepare parses sql.
func Prepare(sql string) (*Statement, error) {
	s, err := parser.Parse(sql)
	if err != nil {
		return nil, err
	}
	st := &Statement{ast: s}
	for _, e := range exprs(s) {
		ast.Walk(e, func(x ast.Expr) bool {
			if p, ok := x.(*ast.Param); ok && p.Index > st.nparams {
				st.nparams = p.Index
			}
			return true
		})
	}
	return st, nil
}

// Prepare parses sql in the session. As in Postgres, a statement that does not parse, or that
// maragudb rejects as unsupported while parsing, aborts an explicit transaction.
func (s *Session) Prepare(sql string) (*Statement, error) {
	st, err := Prepare(sql)
	if err != nil {
		s.aborted = s.explicit
	}
	return st, err
}

// exprs of a statement, for walking.
func exprs(s ast.Statement) []ast.Expr {
	var out []ast.Expr
	add := func(e ast.Expr) {
		if e != nil {
			out = append(out, e)
		}
	}
	switch s := s.(type) {
	case *ast.Insert:
		for _, row := range s.Rows {
			for _, e := range row {
				add(e)
			}
		}
	case *ast.Update:
		for _, a := range s.Set {
			add(a.Value)
		}
		add(s.Where)
	case *ast.Delete:
		add(s.Where)
	case *ast.Select:
		for _, item := range s.Items {
			add(item.Expr)
		}
		add(s.Where)
		for _, o := range s.OrderBy {
			add(o.Expr)
		}
		add(s.Limit)
		add(s.Offset)
	}
	return out
}

// Result of a statement that returns no rows.
type Result struct {
	RowsAffected int64
}

// Column of a result.
type Column struct {
	Name     string
	Type     value.Type
	Nullable bool
}

// Rows of a query, fully materialized.
type Rows struct {
	Columns []Column
	Rows    [][]value.Value
}

// InTx reports whether an explicit transaction is open.
func (s *Session) InTx() bool {
	return s.explicit
}

// Begin an explicit transaction.
func (s *Session) Begin(ctx context.Context, readOnly bool) error {
	if s.explicit {
		return ErrTxInProgress
	}
	if err := s.begin(ctx, readOnly); err != nil {
		return err
	}
	s.explicit = true
	return nil
}

func (s *Session) begin(ctx context.Context, readOnly bool) error {
	var tx *engine.Tx
	var err error
	if readOnly {
		tx, err = s.db.BeginReadOnly(ctx)
	} else {
		tx, err = s.db.Begin(ctx)
	}
	if err != nil {
		return err
	}
	s.tx = tx
	s.readOnly = readOnly
	s.wrote = false
	s.aborted = false
	s.tables = map[string]*table{}
	s.checks = nil
	s.kv = kvstore{tx: tx}
	return nil
}

// Commit the explicit transaction. The session leaves the transaction whether or not the commit
// succeeds. It returns [engine.ErrConflict] when the transaction lost to a concurrent commit,
// and [ErrTxAborted] when a statement of the transaction failed, in which case the transaction
// is rolled back instead, as Postgres does on COMMIT in an aborted transaction.
func (s *Session) Commit(ctx context.Context) error {
	if !s.explicit {
		return ErrNoTx
	}
	if s.aborted {
		s.rollback()
		return fmt.Errorf("%w: the transaction was rolled back", ErrTxAborted)
	}
	return s.commit(ctx)
}

func (s *Session) commit(ctx context.Context) error {
	tx := s.tx
	s.end()
	return tx.Commit(ctx)
}

// Rollback the explicit transaction.
func (s *Session) Rollback() error {
	if !s.explicit {
		return ErrNoTx
	}
	s.rollback()
	return nil
}

func (s *Session) rollback() {
	if s.tx != nil {
		_ = s.tx.Rollback()
	}
	s.end()
}

func (s *Session) end() {
	s.tx = nil
	s.explicit = false
	s.readOnly = false
	s.aborted = false
	s.tables = nil
	s.checks = nil
	s.kv = kvstore{}
}

// Execute a statement. SELECT returns rows; other statements return a result. The transaction
// statements behave as in Postgres: BEGIN inside a transaction and COMMIT or ROLLBACK outside
// one do nothing but warn, and COMMIT of a transaction in which a statement failed rolls it
// back.
func (s *Session) Execute(ctx context.Context, st *Statement, params []value.Value) (*Rows, Result, error) {
	switch stmt := st.ast.(type) {
	case *ast.Begin:
		if s.explicit {
			s.log.Warn("there is already a transaction in progress")
			return nil, Result{}, nil
		}
		return nil, Result{}, s.Begin(ctx, stmt.ReadOnly)
	case *ast.Commit:
		if !s.explicit {
			s.log.Warn("there is no transaction in progress")
			return nil, Result{}, nil
		}
		if s.aborted {
			s.rollback()
			return nil, Result{}, nil
		}
		return nil, Result{}, s.Commit(ctx)
	case *ast.Rollback:
		if !s.explicit {
			s.log.Warn("there is no transaction in progress")
			return nil, Result{}, nil
		}
		return nil, Result{}, s.Rollback()
	}
	if s.explicit {
		if s.aborted {
			return nil, Result{}, ErrTxAborted
		}
		if s.opts.Mutations.FreshSnapshotPerStatement && !s.wrote {
			if err := s.begin(ctx, s.readOnly); err != nil {
				return nil, Result{}, err
			}
			s.explicit = true
		}
		return s.run(ctx, st, params)
	}
	// Autocommit: a statement outside a transaction retries on conflict.
	_, readOnly := st.ast.(*ast.Select)
	const maxAttempts = 32
	// conflicted is set once an attempt ran and lost its commit to a concurrent one.
	conflicted := false
	for attempt := 1; ; attempt++ {
		if err := s.begin(ctx, readOnly); err != nil {
			return nil, Result{}, err
		}
		rows, res, err := s.run(ctx, st, params)
		if err != nil {
			unique := conflicted && errors.Is(err, ErrDuplicateKey)
			assert.Sometimes(unique, "a unique violation occurs only because of a concurrent commit", nil)
			if unique {
				reach.Hit(ctx, reach.UniqueAfterConflict)
			}
			fk := conflicted && errors.Is(err, ErrForeignKey)
			assert.Sometimes(fk, "a foreign key violation occurs only because of a concurrent commit", nil)
			if fk {
				reach.Hit(ctx, reach.ForeignKeyAfterConflict)
			}
			s.rollback()
			return nil, Result{}, err
		}
		err = s.commit(ctx)
		if err == nil {
			return rows, res, nil
		}
		retry := errors.Is(err, engine.ErrConflict)
		assert.Sometimes(retry, "an autocommit statement retries after a conflict", nil)
		assert.Sometimes(errors.Is(err, engine.ErrCommitUnknown), "an autocommit statement reports an unknown commit outcome instead of retrying", nil)
		if s.opts.Mutations.RetryAfterUnknownCommit && errors.Is(err, engine.ErrCommitUnknown) {
			retry = true
		}
		if !retry || attempt == maxAttempts {
			return nil, Result{}, err
		}
		if errors.Is(err, engine.ErrConflict) {
			reach.Hit(ctx, reach.AutocommitRetry)
			conflicted = true
		}
		s.log.Debug("retrying statement after conflict", "attempt", attempt, "error", err)
	}
}

// run one statement in the current transaction, applying its writes only when it succeeds. A
// failure in an explicit transaction aborts it.
func (s *Session) run(ctx context.Context, st *Statement, params []value.Value) (*Rows, Result, error) {
	s.events = nil
	rows, res, err := s.execute(ctx, st.ast, params)
	s.events = nil
	ddl := false
	switch st.ast.(type) {
	case *ast.CreateTable, *ast.DropTable, *ast.CreateIndex, *ast.DropIndex:
		ddl = true
	}
	if err != nil {
		s.kv.discard()
		if ddl {
			s.tables, s.checks = map[string]*table{}, nil
		}
		s.aborted = s.explicit
		return nil, Result{}, err
	}
	wrote := len(s.kv.pending) > 0
	if err := s.kv.apply(); err != nil {
		// Some of the statement's writes may have reached the transaction, which therefore
		// cannot commit.
		s.aborted = s.explicit
		return nil, Result{}, err
	}
	s.wrote = s.wrote || wrote
	if ddl {
		s.tables, s.checks = map[string]*table{}, nil
	}
	return rows, res, nil
}

func (s *Session) execute(ctx context.Context, st ast.Statement, params []value.Value) (*Rows, Result, error) {
	switch st := st.(type) {
	case *ast.Select:
		rows, err := s.selectStmt(ctx, st, params)
		return rows, Result{}, err
	case *ast.CreateTable, *ast.DropTable, *ast.CreateIndex, *ast.DropIndex:
		// Postgres refuses DDL in a read-only transaction before it looks at the statement, and
		// INSERT, UPDATE, and DELETE only once they are analyzed and planned, which
		// [Session.writable] checks.
		if err := s.writable(st); err != nil {
			return nil, Result{}, err
		}
	}
	var res Result
	var err error
	switch st := st.(type) {
	case *ast.CreateTable:
		err = s.createTable(ctx, st)
	case *ast.DropTable:
		err = s.dropTable(ctx, st)
	case *ast.CreateIndex:
		err = s.createIndex(ctx, st)
	case *ast.DropIndex:
		err = s.dropIndex(ctx, st)
	case *ast.Insert:
		res, err = s.insert(ctx, st, params)
	case *ast.Update:
		res, err = s.update(ctx, st, params)
	case *ast.Delete:
		res, err = s.delete(ctx, st, params)
	default:
		err = fmt.Errorf("exec: unsupported statement %T", st)
	}
	return nil, res, err
}

// writable checks that the transaction can run a statement that writes.
func (s *Session) writable(st ast.Statement) error {
	if s.readOnly {
		return fmt.Errorf("%w: cannot run %s", ErrReadOnlyTx, verb(st))
	}
	return nil
}

func verb(st ast.Statement) string {
	switch st.(type) {
	case *ast.CreateTable:
		return "CREATE TABLE"
	case *ast.DropTable:
		return "DROP TABLE"
	case *ast.CreateIndex:
		return "CREATE INDEX"
	case *ast.DropIndex:
		return "DROP INDEX"
	case *ast.Insert:
		return "INSERT"
	case *ast.Update:
		return "UPDATE"
	case *ast.Delete:
		return "DELETE"
	}
	return "statement"
}

// kvstore is the transaction's key space as a statement sees it: reads see the transaction's
// earlier writes and the statement's own pending writes by key, scans see only the transaction's
// earlier writes, and pending writes are applied to the transaction when the statement succeeds.
type kvstore struct {
	tx      *engine.Tx
	pending map[string]pendingEntry
	// keys in write order, so that writes apply deterministically.
	keys []string
}

type pendingEntry struct {
	value     []byte
	tombstone bool
}

func (k *kvstore) get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if e, ok := k.pending[string(key)]; ok {
		if e.tombstone {
			return nil, false, nil
		}
		return e.value, true, nil
	}
	return k.tx.Get(ctx, key)
}

func (k *kvstore) scan(ctx context.Context, start, end []byte) *engine.Iterator {
	return k.tx.Scan(ctx, start, end)
}

func (k *kvstore) put(key, val []byte) {
	k.set(key, pendingEntry{value: val})
}

func (k *kvstore) del(key []byte) {
	k.set(key, pendingEntry{tombstone: true})
}

func (k *kvstore) set(key []byte, e pendingEntry) {
	if k.pending == nil {
		k.pending = map[string]pendingEntry{}
	}
	if _, ok := k.pending[string(key)]; !ok {
		k.keys = append(k.keys, string(key))
	}
	k.pending[string(key)] = e
}

// apply the pending writes to the transaction. The pending writes are dropped either way, so a
// failure, which only happens when the transaction is done or read-only, cannot leak them into
// a later statement.
func (k *kvstore) apply() error {
	defer k.discard()
	for _, key := range k.keys {
		e := k.pending[key]
		var err error
		if e.tombstone {
			err = k.tx.Delete([]byte(key))
		} else {
			err = k.tx.Put([]byte(key), e.value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// wrote reports whether the transaction or the statement has written the key.
func (k *kvstore) wrote(key []byte) bool {
	_, ok := k.pending[string(key)]
	return ok || k.tx.Wrote(key)
}

// kvPair is a key and its value.
type kvPair struct {
	key, value []byte
}

// scanLatest returns the keys in [start, end) and their values in order, as the statement sees
// them: with its own pending writes, unlike [kvstore.scan]. With txOnly, it leaves out the
// pending writes.
func (k *kvstore) scanLatest(ctx context.Context, start, end []byte, txOnly bool) ([]kvPair, error) {
	var out []kvPair
	it := k.tx.Scan(ctx, start, end)
	for it.Next() {
		key := string(it.Key())
		if _, ok := k.pending[key]; ok && !txOnly {
			continue
		}
		out = append(out, kvPair{key: []byte(key), value: append([]byte{}, it.Value()...)})
	}
	if err := it.Err(); err != nil {
		return nil, err
	}
	if txOnly {
		return out, nil
	}
	for key, e := range k.pending {
		if e.tombstone || key < string(start) || key >= string(end) {
			continue
		}
		out = append(out, kvPair{key: []byte(key), value: e.value})
	}
	slices.SortFunc(out, func(a, b kvPair) int { return bytes.Compare(a.key, b.key) })
	return out, nil
}

func (k *kvstore) discard() {
	k.pending = nil
	k.keys = nil
}
