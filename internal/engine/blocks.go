package engine

import (
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
	if db.mem != nil {
		// The decoded block is kept with the cached bytes, so that a hit does not decode them
		// again. Readers share it and never change it.
		if b, parsed, ok := db.mem.GetParsed(ck); ok {
			if d, ok := parsed.(*decodedBlock); ok {
				db.memHits.Add(1)
				return d, nil
			}
			d, err := db.decodeBlock(objectKey, ref, b)
			if err == nil {
				db.memHits.Add(1)
				db.mem.SetParsed(ck, b, d)
				return d, nil
			}
			db.cacheRefused(&db.memRefused, "memory", ck, err)
		}
		db.memMisses.Add(1)
	}
	if db.disk != nil {
		if b, ok := db.disk.Get(ck); ok {
			d, err := db.decodeBlock(objectKey, ref, b)
			if err == nil {
				db.diskHits.Add(1)
				if db.mem != nil {
					db.mem.Put(ck, b)
					db.mem.SetParsed(ck, b, d)
				}
				return d, nil
			}
			db.cacheRefused(&db.diskRefused, "disk", ck, err)
		}
		db.diskMisses.Add(1)
	}
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
	if db.mutations.SegmentUnchecked {
		db.cacheBlock(ck, res.Body, nil)
	}
	d, err := db.decodeBlock(objectKey, ref, res.Body)
	if err != nil {
		db.corruptRead(ctx, err)
		return nil, err
	}
	if !db.mutations.SegmentUnchecked {
		db.cacheBlock(ck, res.Body, d)
	}
	return d, nil
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

// cacheBlock read from the store in the caches, with its decoded form d in the memory cache
// unless it is nil.
func (db *DB) cacheBlock(ck string, b []byte, d *decodedBlock) {
	if db.mem != nil {
		db.mem.Put(ck, b)
		if d != nil {
			db.mem.SetParsed(ck, b, d)
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
		db.randMu.Lock()
		wait += time.Duration(db.rand.Int64N(int64(wait)))
		db.randMu.Unlock()
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
