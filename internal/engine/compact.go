package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antithesishq/antithesis-sdk-go/assert"
)

// Compact merges all segments into one, dropping tombstones and overwritten values. The old
// segments become garbage for [DB.GC]. It is safe to run concurrently with transactions in any
// process: when commits land meanwhile, the merged segment is placed behind their segments, which
// keep shadowing it, so the merge is not redone. It returns [ErrConflict] only when a segment it
// merged is no longer live, because another compaction won, and [ErrSnapshotExpired] when it took
// longer than the GC grace period, after which its output may have been collected as an orphan.
func (db *DB) Compact(ctx context.Context) error {
	return db.compact(ctx, nil)
}

// compact with an optional progress hook, called now and then during long work. An error from it
// aborts the compaction.
func (db *DB) compact(ctx context.Context, progress func() error) error {
	startedAt := db.now()
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return fmt.Errorf("engine: compacting: %w", err)
	}
	if len(m.Segments) < 2 {
		return nil
	}
	if progress == nil {
		progress = func() error { return nil }
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
		if count%1024 == 0 {
			if err := progress(); err != nil {
				return err
			}
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("engine: compacting: %w", err)
	}

	id := db.newID()
	var compacted []segmentMeta
	if count > 0 {
		body, meta, err := b.finish()
		if err != nil {
			return err
		}
		meta.Key = db.key(segmentKey(m.Seq, id))
		if err := progress(); err != nil {
			return err
		}
		if err := db.putObject(ctx, meta.Key, body); err != nil {
			return err
		}
		compacted = []segmentMeta{meta}
	}

	// The swap replaces the merged segments with the compacted one. When the write loses to
	// commits that only added segments, the swap is applied again on top of the new manifest,
	// with the newer segments ahead of the compacted one.
	base, baseEtag := m, etag
	for attempt := 0; ; attempt++ {
		if age := db.now().Sub(startedAt); age >= db.grace {
			// GC may have collected the compacted segment as an orphan by now.
			return fmt.Errorf("%w: compaction is %v old", ErrSnapshotExpired, age)
		}
		if err := progress(); err != nil {
			return err
		}
		newer := base.Segments[len(m.Segments):]
		next := base.next(id, db.now())
		next.Segments = append(append([]segmentMeta{}, compacted...), newer...)
		if db.mutations.CompactedSegmentAhead {
			next.Segments = append(append([]segmentMeta{}, newer...), compacted...)
		}
		next.Garbage = nil
		for _, g := range base.Garbage {
			// A GC pass may have stamped the compacted segment as an orphan meanwhile.
			if len(compacted) > 0 && g.Key == compacted[0].Key {
				continue
			}
			next.Garbage = append(next.Garbage, g)
		}
		for _, seg := range m.Segments {
			next.Garbage = append(next.Garbage, garbageEntry{Key: seg.Key})
		}
		_, err := db.casManifest(ctx, baseEtag, next)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrConflict) {
			db.log.Debug("compaction failed", "error", err)
			return err
		}
		cur, curEtag := db.cachedManifest()
		if attempt == 8 || cur.Seq <= base.Seq || !cur.extends(m) {
			assert.Sometimes(true, "a compaction loses to another compaction", nil)
			db.log.Debug("compaction failed", "error", err)
			return err
		}
		assert.Sometimes(true, "a compaction rebases on commits that only added segments", nil)
		db.compactionRebases.Add(1)
		base, baseEtag = cur, curEtag
	}
	assert.Reachable("compaction succeeded", nil)
	db.log.Info("compacted", "segments", len(m.Segments), "entries", count, "seq", db.Stats().Seq)
	return nil
}

// extends reports whether m's segments are all still live in cur, unchanged and in the same
// order, so that cur differs from m only by segments added after it.
func (cur *manifest) extends(m *manifest) bool {
	if len(cur.Segments) < len(m.Segments) {
		return false
	}
	for i, seg := range m.Segments {
		if cur.Segments[i].Key != seg.Key {
			return false
		}
	}
	return true
}

// GC deletes objects that no snapshot within the grace period can reference, and returns how
// many it deleted. Objects that left the manifest are stamped on the first GC pass that sees
// them, and deleted once the stamp is older than the grace period. Objects that never entered
// the manifest, because the commit or compaction that wrote them failed or crashed, enter the
// garbage list stamped once the manifest has moved past the sequence number they were written
// against, because a compaction that rebases may still reference its output until it is older
// than the grace period.
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
	if db.mutations.GCIgnoreGrace {
		toDelete = append(toDelete, orphans...)
	} else {
		for _, key := range orphans {
			keep = append(keep, garbageEntry{Key: key, UnreferencedAt: now})
			changed = true
		}
	}

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
