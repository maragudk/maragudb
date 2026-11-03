package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antithesishq/antithesis-sdk-go/assert"
)

// Compact merges all segments into one, dropping tombstones and overwritten values. The old
// segments become garbage for [DB.GC]. It returns [ErrConflict] when a commit happened meanwhile.
// It is safe to run concurrently with transactions in any process.
func (db *DB) Compact(ctx context.Context) error {
	startedAt := db.now()
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return fmt.Errorf("engine: compacting: %w", err)
	}
	if len(m.Segments) < 2 {
		return nil
	}

	// Merging every segment is what makes dropping tombstones safe: no older segment remains
	// whose values a tombstone would have to keep hiding.
	it := &Iterator{}
	for i := len(m.Segments) - 1; i >= 0; i-- {
		seg := m.Segments[i]
		it.sources = append(it.sources, newSegmentIterator(seg.Root, nil, db.fetcher(ctx, startedAt, seg)))
	}
	b := newSegmentBuilder(db.blockSize)
	var count int
	for it.Next() {
		if err := b.add(it.Key(), it.Value(), false); err != nil {
			return err
		}
		count++
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("engine: compacting: %w", err)
	}

	id := db.newID()
	next := m.next(id, db.now())
	next.Segments = nil
	if count > 0 {
		body, meta, err := b.finish()
		if err != nil {
			return err
		}
		meta.Key = db.key(segmentKey(m.Seq, id))
		if err := db.putObject(ctx, meta.Key, body); err != nil {
			return err
		}
		next.Segments = []segmentMeta{meta}
	}
	for _, seg := range m.Segments {
		next.Garbage = append(next.Garbage, garbageEntry{Key: seg.Key})
	}
	if _, err := db.casManifest(ctx, etag, next); err != nil {
		db.log.Debug("compaction failed", "error", err)
		return err
	}
	assert.Reachable("compaction succeeded", nil)
	db.log.Info("compacted", "segments", len(m.Segments), "entries", count, "seq", next.Seq)
	return nil
}

// GC deletes objects that no snapshot within the grace period can reference, and returns how
// many it deleted. Objects that left the manifest are stamped on the first GC pass that sees
// them, and deleted once the stamp is older than the grace period. Objects that never entered
// the manifest, because the commit that wrote them failed or crashed, are deleted as soon as
// the manifest has moved past the sequence number they were written against.
// It returns [ErrConflict] when a commit happened meanwhile; a retry is safe.
func (db *DB) GC(ctx context.Context) (int, error) {
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: garbage collecting: %w", err)
	}
	now := db.now()

	referenced := map[string]bool{}
	for _, seg := range m.Segments {
		referenced[seg.Key] = true
	}
	for _, g := range m.Garbage {
		referenced[g.Key] = true
	}

	var toDelete []string
	var keep []garbageEntry
	changed := false
	for _, g := range m.Garbage {
		switch {
		case db.mutations.GCIgnoreGrace:
			toDelete = append(toDelete, g.Key)
		case g.UnreferencedAt.IsZero():
			g.UnreferencedAt = now
			keep = append(keep, g)
			changed = true
		case now.Sub(g.UnreferencedAt) >= db.grace:
			toDelete = append(toDelete, g.Key)
		default:
			keep = append(keep, g)
		}
	}

	infos, err := db.client.List(ctx, db.key(segmentKeyPrefix))
	if err != nil {
		return 0, fmt.Errorf("engine: garbage collecting: listing: %w", err)
	}
	var orphans []string
	for _, info := range infos {
		if referenced[info.Key] {
			continue
		}
		baseSeq, ok := segmentBaseSeq(strings.TrimPrefix(info.Key, db.prefix))
		if !ok {
			continue
		}
		if baseSeq < m.Seq {
			orphans = append(orphans, info.Key)
		}
	}
	assert.Sometimes(len(orphans) > 0, "GC finds an orphaned object", nil)
	assert.Sometimes(len(toDelete) > 0, "GC finds an expired object", nil)

	deleted := 0
	var failed []garbageEntry
	for _, key := range toDelete {
		if err := db.client.Delete(ctx, key); err != nil {
			db.log.Warn("deleting expired object", "key", key, "error", err)
			failed = append(failed, garbageEntry{Key: key, UnreferencedAt: now.Add(-db.grace)})
			continue
		}
		deleted++
	}
	for _, key := range orphans {
		if err := db.client.Delete(ctx, key); err != nil {
			db.log.Warn("deleting orphaned object", "key", key, "error", err)
			continue
		}
		deleted++
	}

	if !changed && len(toDelete) == 0 {
		return deleted, nil
	}
	next := m.next(db.newID(), now)
	next.Garbage = append(keep, failed...)
	if _, err := db.casManifest(ctx, etag, next); err != nil {
		if errors.Is(err, ErrConflict) {
			return deleted, err
		}
		return deleted, fmt.Errorf("engine: garbage collecting: %w", err)
	}
	db.log.Info("garbage collected", "deleted", deleted, "remaining", len(next.Garbage), "seq", next.Seq)
	return deleted, nil
}
