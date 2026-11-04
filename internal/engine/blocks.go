package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
)

func blockCacheKey(objectKey string, ref blockRef) string {
	return fmt.Sprintf("%s@%d+%d", objectKey, ref.Offset, ref.Length)
}

// readBlock of a segment through the caches. Blocks are immutable, so cached copies are never stale.
// A missing object means the snapshot's objects were garbage collected.
func (db *DB) readBlock(ctx context.Context, snapshotAt time.Time, objectKey string, ref blockRef) (*decodedBlock, error) {
	ck := blockCacheKey(objectKey, ref)
	// A corrupt cache entry is ignored and replaced from the store.
	if db.mem != nil {
		if b, ok := db.mem.Get(ck); ok {
			if d, err := decodeBlock(b); err == nil {
				db.memHits.Add(1)
				return d, nil
			}
			db.log.Warn("corrupt block in memory cache", "key", ck)
		}
		db.memMisses.Add(1)
	}
	if db.disk != nil {
		if b, ok := db.disk.Get(ck); ok {
			if d, err := decodeBlock(b); err == nil {
				db.diskHits.Add(1)
				if db.mem != nil {
					db.mem.Put(ck, b)
				}
				return d, nil
			}
			db.log.Warn("corrupt block in disk cache", "key", ck)
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
	if db.mem != nil {
		db.mem.Put(ck, res.Body)
	}
	if db.disk != nil {
		db.disk.Put(ck, res.Body)
	}
	return decodeBlock(res.Body)
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
