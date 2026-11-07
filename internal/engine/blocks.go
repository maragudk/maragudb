package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
)

func blockCacheKey(objectKey string, ref blockRef) string {
	return fmt.Sprintf("%s@%d+%d", objectKey, ref.Offset, ref.Length)
}

// readBlock of a segment through the caches. Blocks are immutable, so cached copies are never stale.
// A missing object means the snapshot's objects were garbage collected. A block is checked before
// it is served or cached: one from a cache that fails its check is a miss, and one from the store
// gives an error that wraps [ErrCorrupt].
func (db *DB) readBlock(ctx context.Context, snapshotAt time.Time, objectKey string, ref blockRef) (*decodedBlock, error) {
	ck := blockCacheKey(objectKey, ref)
	if d, ok := db.cachedBlock(ck, objectKey, ref); ok {
		return d, nil
	}
	body, err := db.readRange(ctx, snapshotAt, objectKey, ref)
	if err != nil {
		return nil, err
	}
	return db.storedBlock(ctx, ck, objectKey, ref, body)
}

// cachedBlock of a segment at the cache key ck, from the memory cache or the disk cache, checked.
// It is false when neither cache has the block whole.
func (db *DB) cachedBlock(ck, objectKey string, ref blockRef) (*decodedBlock, bool) {
	return cachedEntry(db, ck, func(b []byte) (*decodedBlock, error) { return db.decodeBlock(objectKey, ref, b) })
}

// storedBlock checks and decodes the bytes of a block that the store returned, and caches them
// at ck when they pass.
func (db *DB) storedBlock(ctx context.Context, ck, objectKey string, ref blockRef, body []byte) (*decodedBlock, error) {
	return storedEntry(ctx, db, ck, body, db.mutations.SegmentUnchecked, func(b []byte) (*decodedBlock, error) { return db.decodeBlock(objectKey, ref, b) })
}

// cachedEntry at the cache key ck, from the memory cache or the disk cache, which decode checks
// and decodes. It is false when neither cache has the entry whole. The decoded entry is kept with
// the bytes in the memory cache, so that a hit does not decode them again; readers share it and
// never change it.
func cachedEntry[T any](db *DB, ck string, decode func([]byte) (T, error)) (T, bool) {
	var none T
	if db.mem != nil {
		if b, parsed, ok := db.mem.GetParsed(ck); ok {
			if v, ok := parsed.(T); ok {
				db.memHits.Add(1)
				return v, true
			}
			v, err := decode(b)
			if err == nil {
				db.memHits.Add(1)
				db.mem.SetParsed(ck, b, v)
				return v, true
			}
			db.cacheRefused(&db.memRefused, "memory", ck, err)
		}
		db.memMisses.Add(1)
	}
	if db.disk != nil {
		if b, ok := db.disk.Get(ck); ok {
			v, err := decode(b)
			if err == nil {
				db.diskHits.Add(1)
				if db.mem != nil {
					db.mem.Put(ck, b)
					db.mem.SetParsed(ck, b, v)
				}
				return v, true
			}
			db.cacheRefused(&db.diskRefused, "disk", ck, err)
		}
		db.diskMisses.Add(1)
	}
	return none, false
}

// storedEntry checks and decodes with decode the bytes that the store returned, and caches them at
// ck when they pass. With unchecked, for the mutations that skip a check, it caches them before it
// decodes them.
func storedEntry[T any](ctx context.Context, db *DB, ck string, body []byte, unchecked bool, decode func([]byte) (T, error)) (T, error) {
	if unchecked {
		db.cacheBytes(ck, body, nil)
	}
	v, err := decode(body)
	if err != nil {
		db.corruptRead(ctx, err)
		return v, err
	}
	if !unchecked {
		db.cacheBytes(ck, body, v)
	}
	return v, nil
}

// readRange of the object at objectKey from the store. A missing object means that GC deleted the
// objects of the snapshot taken at snapshotAt, which gives [ErrSnapshotExpired].
func (db *DB) readRange(ctx context.Context, snapshotAt time.Time, objectKey string, ref blockRef) ([]byte, error) {
	res, err := db.getBlock(ctx, objectKey, ref)
	if err != nil {
		if errors.Is(err, s3.ErrNotFound) {
			age := db.now().Sub(snapshotAt)
			if !db.mutations.GCIgnoreGrace {
				assert.Always(age >= db.grace, "a reader younger than the GC grace period never loses its objects", map[string]any{"age": age.String(), "grace": db.grace.String()})
			}
			return nil, fmt.Errorf("%w: object %q is gone after %v (grace period %v)", ErrSnapshotExpired, objectKey, age, db.grace)
		}
		return nil, fmt.Errorf("engine: reading block of %q: %w", objectKey, err)
	}
	return res.Body, nil
}

// lookupFetcher for a point lookup in seg of the key whose hash is h, or nil when the segment's
// key filter shows that the segment does not hold the key. A filter block that no cache has is
// read together with the root block when no cache has that either, and the fetcher then serves
// that root block without reading it again.
func (db *DB) lookupFetcher(ctx context.Context, snapshotAt time.Time, seg segmentMeta, h uint64) (blockFetcher, error) {
	if seg.FilterBlock == nil {
		if !seg.Filter.mayContainHash(h) {
			return nil, nil
		}
		return db.fetcher(ctx, snapshotAt, seg), nil
	}
	f, root, err := db.readFilter(ctx, snapshotAt, seg)
	switch {
	case err != nil:
		return nil, err
	case !f.mayContainHash(h):
		return nil, nil
	}
	fetch := db.fetcher(ctx, snapshotAt, seg)
	if root == nil {
		return fetch, nil
	}
	return func(ref blockRef) (*decodedBlock, error) {
		if ref == seg.Root {
			return root, nil
		}
		return fetch(ref)
	}, nil
}

// readFilter reads the filter block of seg from the caches, or else from the store: alone when a
// cache has the root block, and otherwise in one range read with the root block. It returns the
// root block when it has it. A filter block is read alone even though a lookup of a key that the
// segment holds then makes one GET more than without the filter, because the filter stays in the
// cache, and saves every later lookup of a key that the segment does not hold its descent.
func (db *DB) readFilter(ctx context.Context, snapshotAt time.Time, seg segmentMeta) (keyFilter, *decodedBlock, error) {
	ref := *seg.FilterBlock
	fk := blockCacheKey(seg.Key, ref)
	if f, ok := db.cachedFilter(fk, seg.Key, ref); ok {
		return f, nil, nil
	}
	rk := blockCacheKey(seg.Key, seg.Root)
	if root, ok := db.cachedBlock(rk, seg.Key, seg.Root); ok {
		body, err := db.readRange(ctx, snapshotAt, seg.Key, ref)
		if err != nil {
			return nil, nil, err
		}
		f, err := db.storedFilter(ctx, fk, seg.Key, ref, body)
		if err != nil {
			return nil, nil, err
		}
		return f, root, nil
	}
	both := blockRef{Offset: seg.Root.Offset, Length: seg.Root.Length + ref.Length}
	body, err := db.readRange(ctx, snapshotAt, seg.Key, both)
	if err != nil {
		return nil, nil, err
	}
	// Each block gets its own copy, so that a cached block does not pin the other.
	n := min(len(body), int(seg.Root.Length))
	f, err := db.storedFilter(ctx, fk, seg.Key, ref, bytes.Clone(body[n:]))
	if err != nil {
		return nil, nil, err
	}
	root, err := db.storedBlock(ctx, rk, seg.Key, seg.Root, bytes.Clone(body[:n]))
	if err != nil {
		return nil, nil, err
	}
	assert.Sometimes(true, "a point lookup reads a filter block with its root block", nil)
	reach.Hit(ctx, reach.FilterBlockRead)
	return f, root, nil
}

// cachedFilter of a segment at the cache key ck, from the memory cache or the disk cache, checked.
// It is false when neither cache has the filter whole.
func (db *DB) cachedFilter(ck, objectKey string, ref blockRef) (keyFilter, bool) {
	return cachedEntry(db, ck, func(b []byte) (keyFilter, error) { return db.decodeFilter(objectKey, ref, b) })
}

// storedFilter checks and decodes the bytes of a filter block that the store returned, and caches
// them at ck when they pass.
func (db *DB) storedFilter(ctx context.Context, ck, objectKey string, ref blockRef, body []byte) (keyFilter, error) {
	return storedEntry(ctx, db, ck, body, db.mutations.FilterUnchecked, func(b []byte) (keyFilter, error) { return db.decodeFilter(objectKey, ref, b) })
}

// decodeFilter block of a segment, checking it unless [Mutations.FilterUnchecked] is set.
func (db *DB) decodeFilter(objectKey string, ref blockRef, b []byte) (keyFilter, error) {
	if !db.mutations.FilterUnchecked {
		return decodeFilterBlock(objectKey, ref, b)
	}
	body, err := blockBody(objectKey, ref, b)
	if err != nil {
		return nil, err
	}
	return parseFilterBlock(objectKey, ref, body)
}

// decodeBlock of a segment, checking it unless [Mutations.SegmentUnchecked] is set.
func (db *DB) decodeBlock(objectKey string, ref blockRef, b []byte) (*decodedBlock, error) {
	if !db.mutations.SegmentUnchecked {
		return decodeBlock(objectKey, ref, b)
	}
	body, err := blockBody(objectKey, ref, b)
	if err != nil {
		return nil, err
	}
	return parseBlock(objectKey, ref, body)
}

// cacheBytes read from the store in the caches, with their decoded form parsed in the memory
// cache unless it is nil.
func (db *DB) cacheBytes(ck string, b []byte, parsed any) {
	if db.mem != nil {
		db.mem.Put(ck, b)
		if parsed != nil {
			db.mem.SetParsed(ck, b, parsed)
		}
	}
	if db.disk != nil {
		db.disk.Put(ck, b)
	}
}

// cacheRefused counts and logs bytes that a cache returned and that failed their check. A cache
// returns what it was given, and only checked bytes are given to it, so this never happens
// without a bug.
func (db *DB) cacheRefused(counter *atomic.Int64, tier, key string, err error) {
	counter.Add(1)
	db.log.Warn("refused damaged bytes from the "+tier+" cache", "key", key, "error", err)
	if db.mutations == (Mutations{}) {
		assert.Unreachable("a cache returns bytes that fail their check", map[string]any{"tier": tier, "key": key, "error": err.Error()})
	}
}

// corruptRead marks a read from the store that gave damaged bytes.
func (db *DB) corruptRead(ctx context.Context, err error) {
	db.log.Warn("refused damaged bytes from the store", "error", err)
	assert.Sometimes(true, "a read of damaged bytes from the store fails with ErrCorrupt", nil)
	reach.Hit(ctx, reach.DamagedReadRefused)
}

// getBlock from the store. Reads are idempotent, so a transient failure is retried a bounded
// number of times with exponential backoff and jitter on the clock. A missing object is asked
// for once more with a consistent read, because a store with eventually consistent reads may
// not show a fresh object yet, and is then reported as [s3.ErrNotFound].
func (db *DB) getBlock(ctx context.Context, objectKey string, ref blockRef) (s3.Object, error) {
	byteRange := &s3.ByteRange{Offset: ref.Offset, Length: ref.Length}
	for attempt := 0; ; attempt++ {
		res, err := db.client.Get(ctx, objectKey, s3.GetOptions{Range: byteRange})
		if errors.Is(err, s3.ErrNotFound) {
			res, err = db.client.Get(ctx, objectKey, s3.GetOptions{Range: byteRange, Consistent: true})
		}
		if err == nil && attempt > 0 {
			assert.Sometimes(true, "a retried block read succeeds", nil)
			reach.Hit(ctx, reach.BlockReadRetrySucceeded)
		}
		if err == nil || s3.Definite(err) || attempt == 3 || ctx.Err() != nil {
			return res, err
		}
		assert.Sometimes(true, "a failed block read is retried", nil)
		wait := 10 * time.Millisecond << attempt
		wait += db.jitter(wait)
		db.log.Debug("retrying block read", "key", objectKey, "attempt", attempt+1, "wait", wait, "error", err)
		db.sleep(ctx, wait)
		if ctx.Err() != nil {
			return s3.Object{}, err
		}
	}
}

// fetcher for a segment within a snapshot.
func (db *DB) fetcher(ctx context.Context, snapshotAt time.Time, seg segmentMeta) blockFetcher {
	return func(ref blockRef) (*decodedBlock, error) {
		return db.readBlock(ctx, snapshotAt, seg.Key, ref)
	}
}
