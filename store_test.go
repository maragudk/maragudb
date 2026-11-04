package maragudb_test

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/s3test"
)

// memStore is an object store in memory, written as code outside this module would write it:
// it only uses types from maragudb and the standard library.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	// ranged counts the reads of a byte range.
	ranged int
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]byte{}}
}

func etagOf(body []byte) string {
	sum := md5.Sum(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (s *memStore) Get(_ context.Context, key string, opts maragudb.GetOptions) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	if !ok {
		return nil, "", fmt.Errorf("get %q: %w", key, maragudb.ErrObjectNotFound)
	}
	etag := etagOf(body)
	if opts.IfNoneMatch == etag {
		return nil, "", maragudb.ErrNotModified
	}
	if opts.Length > 0 {
		s.ranged++
		if opts.Offset >= int64(len(body)) {
			return nil, "", fmt.Errorf("get %q: range starts past the end", key)
		}
		body = body[opts.Offset:min(int64(len(body)), opts.Offset+opts.Length)]
	}
	return append([]byte(nil), body...), etag, nil
}

func (s *memStore) Put(_ context.Context, key string, body []byte, opts maragudb.PutOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.objects[key]
	switch {
	case opts.IfNoneMatch && exists:
		return "", maragudb.ErrPreconditionFailed
	case opts.IfMatch != "" && !exists:
		return "", maragudb.ErrObjectNotFound
	case opts.IfMatch != "" && opts.IfMatch != etagOf(old):
		return "", maragudb.ErrPreconditionFailed
	}
	s.objects[key] = append([]byte(nil), body...)
	return etagOf(body), nil
}

func (s *memStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, key)
	return nil
}

func (s *memStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// ExampleOptions plugs in an object store of its own, here one that keeps objects in memory.
func ExampleOptions() {
	c, err := maragudb.NewConnector(maragudb.Options{Store: newMemStore()}, "s3://ignored/app")
	if err != nil {
		panic(err)
	}
	db := sql.OpenDB(c)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, "CREATE TABLE greetings (id bigint PRIMARY KEY, text text NOT NULL)"); err != nil {
		panic(err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO greetings VALUES ($1, $2)", 1, "hello"); err != nil {
		panic(err)
	}
	var text string
	if err := db.QueryRowContext(ctx, "SELECT text FROM greetings WHERE id = $1", 1).Scan(&text); err != nil {
		panic(err)
	}
	fmt.Println(text)
	// Output: hello
}

// failingStore fails every request with its error, wrapped.
type failingStore struct {
	err error
}

func (s failingStore) Get(context.Context, string, maragudb.GetOptions) ([]byte, string, error) {
	return nil, "", fmt.Errorf("get: %w", s.err)
}

func (s failingStore) Put(context.Context, string, []byte, maragudb.PutOptions) (string, error) {
	return "", fmt.Errorf("put: %w", s.err)
}

func (s failingStore) Delete(context.Context, string) error {
	return fmt.Errorf("delete: %w", s.err)
}

func (s failingStore) List(context.Context, string) ([]string, error) {
	return nil, fmt.Errorf("list: %w", s.err)
}

func TestOptions_Store(t *testing.T) {
	t.Run("runs a database over a store written outside the module, shared by two connectors", func(t *testing.T) {
		store := newMemStore()
		open := func() *sql.DB {
			c, err := maragudb.NewConnector(maragudb.Options{Store: store, MemoryCacheBytes: new(int64), Maintenance: new(false)}, "s3://bucket/app")
			is.NotError(t, err)
			db := sql.OpenDB(c)
			t.Cleanup(func() { _ = db.Close() })
			return db
		}
		a, b := open(), open()
		mustExec(t, a, "CREATE TABLE t (id bigint PRIMARY KEY, v text)")
		for i := range 300 {
			mustExec(t, a, "INSERT INTO t VALUES ($1, $2)", i, strings.Repeat("x", 100))
		}
		is.NotError(t, maragudb.Compact(t.Context(), a))
		is.Equal(t, "300", queryRows(t, b, "SELECT id + 1 FROM t WHERE id = 299"))

		mustExec(t, b, "UPDATE t SET v = 'y' WHERE id = 0")
		is.Equal(t, `"y"`, queryRows(t, a, "SELECT v FROM t WHERE id = 0"))

		// A transaction that loses to a commit through the other connector conflicts, which
		// needs the store's conditional writes.
		tx, err := a.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		_, err = tx.ExecContext(t.Context(), "UPDATE t SET v = 'a' WHERE id = 1")
		is.NotError(t, err)
		mustExec(t, b, "UPDATE t SET v = 'b' WHERE id = 1")
		is.Error(t, maragudb.ErrConflict, tx.Commit())
		is.Equal(t, `"b"`, queryRows(t, a, "SELECT v FROM t WHERE id = 1"))

		store.mu.Lock()
		keys := len(store.objects)
		store.mu.Unlock()
		is.True(t, keys > 1, "objects are in the store")
	})

	t.Run("reads a byte range of a large segment instead of the whole object", func(t *testing.T) {
		store := newMemStore()
		c, err := maragudb.NewConnector(maragudb.Options{Store: store, MemoryCacheBytes: new(int64), Maintenance: new(false)}, "s3://bucket/app")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY, v text)")
		tx, err := db.BeginTx(t.Context(), nil)
		is.NotError(t, err)
		for i := range 2000 {
			_, err := tx.ExecContext(t.Context(), "INSERT INTO t VALUES ($1, $2)", i, strings.Repeat("x", 100))
			is.NotError(t, err)
		}
		is.NotError(t, tx.Commit())
		before, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		is.Equal(t, "1234", queryRows(t, db, "SELECT id FROM t WHERE id = 1234"))
		after, err := maragudb.Stats(t.Context(), db)
		is.NotError(t, err)
		gets := after.Requests.Sub(before.Requests).Get
		is.True(t, gets >= 2 && gets <= 6, fmt.Sprint("a cold point lookup reads a few blocks, got ", gets))
		store.mu.Lock()
		ranged := store.ranged
		store.mu.Unlock()
		is.True(t, ranged >= 2, "blocks are read as byte ranges")
	})

	t.Run("passes the store's errors through, so callers can tell what failed", func(t *testing.T) {
		outage := errors.New("store is down")
		c, err := maragudb.NewConnector(maragudb.Options{Store: failingStore{err: outage}, Maintenance: new(false)}, "s3://bucket/app")
		is.NotError(t, err)
		db := sql.OpenDB(c)
		defer func() { _ = db.Close() }()
		is.Error(t, outage, db.PingContext(t.Context()))
	})

	t.Run("is satisfied by the internal fake through the test adapter", func(t *testing.T) {
		fake := s3fake.New()
		db := sql.OpenDB(must(maragudb.NewConnector(maragudb.Options{Store: s3test.NewStore(fake), Maintenance: new(false)}, "s3://bucket/app")))
		defer func() { _ = db.Close() }()
		mustExec(t, db, "CREATE TABLE t (id bigint PRIMARY KEY)")
		_, ok := fake.Body("app/manifest")
		is.True(t, ok, "the manifest is under the DSN's prefix")
		is.True(t, fake.Counts().Get+fake.Counts().GetConditional > 0, "the fake was used")
	})
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
