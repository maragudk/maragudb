package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/s3"
)

func blockCacheKey(objectKey string, ref blockRef) string {
	return fmt.Sprintf("%s@%d+%d", objectKey, ref.Offset, ref.Length)
}

// readBlock of a segment through the caches. Blocks are immutable, so cached copies are never stale.
// A missing object means the snapshot's objects were garbage collected.
func (db *DB) readBlock(ctx context.Context, snapshotAt time.Time, objectKey string, ref blockRef) (*decodedBlock, error) {
	ck := blockCacheKey(objectKey, ref)
	if db.mem != nil {
		if b, ok := db.mem.Get(ck); ok {
			db.memHits.Add(1)
			return decodeBlock(b)
		}
		db.memMisses.Add(1)
	}
	if db.disk != nil {
		if b, ok := db.disk.Get(ck); ok {
			db.diskHits.Add(1)
			if db.mem != nil {
				db.mem.Put(ck, b)
			}
			return decodeBlock(b)
		}
		db.diskMisses.Add(1)
	}
	res, err := db.client.Get(ctx, objectKey, s3.GetOptions{Range: &s3.ByteRange{Offset: ref.Offset, Length: ref.Length}})
	if err != nil {
		if errors.Is(err, s3.ErrNotFound) {
			age := db.now().Sub(snapshotAt)
			assert.Always(age >= db.grace, "a reader younger than the GC grace period never loses its objects", map[string]any{"age": age.String(), "grace": db.grace.String()})
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

// fetcher for a segment within a snapshot.
func (db *DB) fetcher(ctx context.Context, snapshotAt time.Time, seg segmentMeta) blockFetcher {
	return func(ref blockRef) (*decodedBlock, error) {
		return db.readBlock(ctx, snapshotAt, seg.Key, ref)
	}
}
