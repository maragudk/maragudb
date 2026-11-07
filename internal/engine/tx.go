package engine

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
)

// Tx is a transaction over a snapshot of the database. It is not safe for concurrent use.
type Tx struct {
	db        *DB
	m         *manifest
	etag      string
	readOnly  bool
	startedAt time.Time
	writes    map[string]entry
	done      bool
}

// Begin a read-write transaction. It sees every commit acknowledged before this call.
func (db *DB) Begin(ctx context.Context) (*Tx, error) {
	return db.begin(ctx, false)
}

// BeginReadOnly begins a transaction that cannot write and never conflicts.
func (db *DB) BeginReadOnly(ctx context.Context) (*Tx, error) {
	return db.begin(ctx, true)
}

func (db *DB) begin(ctx context.Context, readOnly bool) (*Tx, error) {
	startedAt := db.now()
	m, etag, err := db.currentManifest(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: beginning transaction: %w", err)
	}
	return &Tx{db: db, m: m, etag: etag, readOnly: readOnly, startedAt: startedAt, writes: map[string]entry{}}, nil
}

// Seq of the snapshot: the sequence number of the latest commit it includes.
func (tx *Tx) Seq() uint64 {
	return tx.m.Seq
}

// StartedAt is the time the transaction began.
func (tx *Tx) StartedAt() time.Time {
	return tx.startedAt
}

func (tx *Tx) check() error {
	if tx.done {
		return ErrTxDone
	}
	if age := tx.db.now().Sub(tx.startedAt); age >= tx.db.grace {
		assert.Sometimes(true, "a transaction outlives the GC grace period", nil)
		return fmt.Errorf("%w: transaction is %v old", ErrSnapshotExpired, age)
	}
	return nil
}

// Get the value at key. The second result is false when the key is absent.
func (tx *Tx) Get(ctx context.Context, key []byte) ([]byte, bool, error) {
	if err := tx.check(); err != nil {
		return nil, false, err
	}
	if e, ok := tx.writes[string(key)]; ok {
		if e.tombstone {
			return nil, false, nil
		}
		return e.value, true, nil
	}
	h := keyHash(key)
	for i := len(tx.m.Segments) - 1; i >= 0; i-- {
		seg := tx.m.Segments[i]
		if bytes.Compare(key, seg.MinKey) < 0 || bytes.Compare(key, seg.MaxKey) > 0 {
			continue
		}
		fetch, err := tx.db.lookupFetcher(ctx, tx.startedAt, seg, h)
		if err != nil {
			return nil, false, err
		}
		if fetch == nil {
			assert.Sometimes(true, "a point lookup skips a segment whose filter does not hold the key", nil)
			reach.Hit(ctx, reach.FilterSkip)
			continue
		}
		e, ok, err := segmentLookup(seg.Root, key, fetch)
		if err != nil {
			return nil, false, err
		}
		if ok {
			if e.tombstone {
				return nil, false, nil
			}
			return e.value, true, nil
		}
	}
	return nil, false, nil
}

// Put a value at key. The key and value are copied.
func (tx *Tx) Put(key, value []byte) error {
	if tx.done {
		return ErrTxDone
	}
	if tx.readOnly {
		return ErrReadOnly
	}
	tx.writes[string(key)] = entry{key: append([]byte{}, key...), value: append([]byte{}, value...)}
	return nil
}

// Delete the value at key.
func (tx *Tx) Delete(key []byte) error {
	if tx.done {
		return ErrTxDone
	}
	if tx.readOnly {
		return ErrReadOnly
	}
	tx.writes[string(key)] = entry{key: append([]byte{}, key...), tombstone: true}
	return nil
}

// Wrote reports whether the transaction has put or deleted the key.
func (tx *Tx) Wrote(key []byte) bool {
	_, ok := tx.writes[string(key)]
	return ok
}

// Scan keys in [start, end) in order. A nil start begins at the first key, and a nil end is unbounded.
func (tx *Tx) Scan(ctx context.Context, start, end []byte) *Iterator {
	it := &Iterator{end: end}
	if err := tx.check(); err != nil {
		it.err = err
		return it
	}
	if len(tx.writes) > 0 {
		var pending []entry
		for _, e := range tx.writes {
			if (start == nil || bytes.Compare(e.key, start) >= 0) && (end == nil || bytes.Compare(e.key, end) < 0) {
				pending = append(pending, e)
			}
		}
		sort.Slice(pending, func(i, j int) bool { return bytes.Compare(pending[i].key, pending[j].key) < 0 })
		it.sources = append(it.sources, &sliceSource{entries: pending})
	}
	for i := len(tx.m.Segments) - 1; i >= 0; i-- {
		seg := tx.m.Segments[i]
		if end != nil && bytes.Compare(seg.MinKey, end) >= 0 {
			continue
		}
		if start != nil && bytes.Compare(seg.MaxKey, start) < 0 {
			continue
		}
		it.sources = append(it.sources, newSegmentIterator(seg.Root, start, tx.db.fetcher(ctx, tx.startedAt, seg)))
	}
	return it
}

// Rollback discards the transaction's writes.
func (tx *Tx) Rollback() error {
	if tx.done {
		return ErrTxDone
	}
	tx.done = true
	return nil
}

// Commit the transaction. It returns nil only after the store has confirmed the write.
// It returns [ErrConflict] when another commit happened since the transaction began, and
// [ErrCommitUnknown] when the outcome could not be determined. Any other error means the
// transaction was not applied.
func (tx *Tx) Commit(ctx context.Context) error {
	if err := tx.check(); err != nil {
		return err
	}
	tx.done = true
	if tx.readOnly || len(tx.writes) == 0 {
		return nil
	}

	keys := make([]string, 0, len(tx.writes))
	for k := range tx.writes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := newSegmentBuilder(tx.db.blockSize)
	b.omitFilterKeys = tx.db.mutations.FilterOmitsKeys
	for _, k := range keys {
		e := tx.writes[k]
		if err := b.add(e.key, e.value, e.tombstone); err != nil {
			return err
		}
	}
	db := tx.db
	id := db.newID()
	body, meta, err := b.finish(db.key(segmentKey(tx.m.Seq, id)))
	if err != nil {
		return err
	}
	if err := db.putObject(ctx, meta.Key, body); err != nil {
		return err
	}
	// The root block, and the filter block that a lookup reads with it, go to the caches.
	prime := []blockRef{meta.Root}
	if meta.FilterBlock != nil {
		prime = append(prime, *meta.FilterBlock)
	}
	for _, ref := range prime {
		block := bytes.Clone(body[ref.Offset : ref.Offset+ref.Length])
		if db.mem != nil {
			db.mem.Put(blockCacheKey(meta.Key, ref), block)
		}
		if db.disk != nil {
			db.disk.Put(blockCacheKey(meta.Key, ref), block)
		}
	}

	next := tx.m.next(id, db.now())
	next.Segments = append(next.Segments, meta)
	_, err = db.casManifest(ctx, tx.etag, next)
	assert.Sometimes(err == nil, "a commit succeeds", nil)
	if err != nil {
		db.log.Debug("commit failed", "seq", next.Seq, "id", id, "error", err)
		return err
	}
	db.committedBytes.Add(meta.Size)
	db.log.Debug("committed", "seq", next.Seq, "id", id, "writes", len(keys))
	return nil
}

// source of entries for a merge.
type source interface {
	Valid() bool
	Entry() entry
	Next()
	Err() error
}

type sliceSource struct {
	entries []entry
	pos     int
}

func (s *sliceSource) Valid() bool  { return s.pos < len(s.entries) }
func (s *sliceSource) Entry() entry { return s.entries[s.pos] }
func (s *sliceSource) Next()        { s.pos++ }
func (s *sliceSource) Err() error   { return nil }

// Iterator over a key range. Sources are ordered newest first, and the newest entry for a key wins.
type Iterator struct {
	sources []source
	end     []byte
	// keepTombstones makes the iterator stop at a tombstone that wins, with tombstone set,
	// instead of skipping it.
	keepTombstones bool
	key            []byte
	value          []byte
	tombstone      bool
	valid          bool
	err            error
}

func (it *Iterator) advance() {
	it.valid = false
	for {
		var minKey []byte
		winner := -1
		for i, s := range it.sources {
			if err := s.Err(); err != nil {
				it.err = err
				return
			}
			if !s.Valid() {
				continue
			}
			k := s.Entry().key
			if winner == -1 || bytes.Compare(k, minKey) < 0 {
				minKey = k
				winner = i
			}
		}
		if winner == -1 {
			return
		}
		if it.end != nil && bytes.Compare(minKey, it.end) >= 0 {
			return
		}
		e := it.sources[winner].Entry()
		for _, s := range it.sources {
			if s.Valid() && bytes.Equal(s.Entry().key, minKey) {
				s.Next()
				if err := s.Err(); err != nil {
					it.err = err
					return
				}
			}
		}
		if e.tombstone && !it.keepTombstones {
			continue
		}
		it.key = e.key
		it.value = e.value
		it.tombstone = e.tombstone
		it.valid = true
		return
	}
}

// Next advances to the next key and reports whether one exists.
func (it *Iterator) Next() bool {
	if it.err != nil {
		return false
	}
	it.advance()
	return it.valid
}

// Key at the current position. Valid until the next call to Next.
func (it *Iterator) Key() []byte {
	return it.key
}

// Value at the current position. Valid until the next call to Next.
func (it *Iterator) Value() []byte {
	return it.value
}

// Err from reading. Check it when Next returns false.
func (it *Iterator) Err() error {
	return it.err
}
