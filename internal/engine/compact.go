package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
)

// Compact merges all segments into one, dropping tombstones and overwritten values. The old
// segments become garbage for [DB.GC]. It is safe to run concurrently with transactions in any
// process: when commits land meanwhile, the merged segment is placed behind their segments, which
// keep shadowing it, so the merge is not redone. It returns [ErrConflict] only when a segment it
// merged is no longer live, because another compaction won, and [ErrSnapshotExpired] when it took
// longer than half the GC grace period, because its output is collected as an orphan after a
// whole one.
func (db *DB) Compact(ctx context.Context) error {
	_, err := db.compact(ctx, nil)
	return err
}

// compact with an optional progress hook, called now and then during long work. An error from it
// aborts the compaction. It reports whether segments were merged.
func (db *DB) compact(ctx context.Context, progress func() error) (bool, error) {
	startedAt := db.now()
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return false, fmt.Errorf("engine: compacting: %w", err)
	}
	if len(m.Segments) < 2 {
		return false, nil
	}
	db.compactionsStarted.Add(1)
	if progress == nil {
		progress = func() error { return nil }
	}

	// Merging every segment is what makes dropping tombstones safe: no older segment remains
	// whose values a tombstone would have to keep hiding.
	it := &Iterator{}
	for i := len(m.Segments) - 1; i >= 0; i-- {
		fetch := db.fetcher(ctx, startedAt, m.Segments[i])
		// Block reads are the slow part of the merge, so progress is reported before each.
		it.sources = append(it.sources, newSegmentIterator(m.Segments[i].Root, nil, func(ref blockRef) (*decodedBlock, error) {
			if err := progress(); err != nil {
				return nil, err
			}
			return fetch(ref)
		}))
	}
	b := newSegmentBuilder(db.blockSize)
	var count int
	for it.Next() {
		if err := b.add(it.Key(), it.Value(), false); err != nil {
			return false, err
		}
		count++
	}
	if err := it.Err(); err != nil {
		return false, fmt.Errorf("engine: compacting: %w", err)
	}

	id := db.newID()
	var compacted []segmentMeta
	if count > 0 {
		body, meta, err := b.finish()
		if err != nil {
			return false, err
		}
		meta.Key = db.key(segmentKey(m.Seq, id))
		if err := progress(); err != nil {
			return false, err
		}
		if err := db.putObject(ctx, meta.Key, body); err != nil {
			return false, err
		}
		// The whole segment is in memory, so the memory cache is primed with every block when the
		// segment leaves room for other blocks, and reads in this process stay warm across the
		// compaction. The disk cache is left to fill with the blocks that are read.
		if db.mem != nil && int64(len(body)) <= db.memBytes/2 {
			for _, ref := range b.blocks {
				// A copy per block, so that a cached block does not pin the whole segment.
				db.mem.Put(blockCacheKey(meta.Key, ref), bytes.Clone(body[ref.Offset:ref.Offset+ref.Length]))
			}
		}
		compacted = []segmentMeta{meta}
	}

	// The swap replaces the merged segments with the compacted one. When the write loses to
	// commits that only added segments, the swap is applied again on top of the new manifest,
	// with the newer segments ahead of the compacted one.
	base, baseEtag := m, etag
	for attempt := 0; ; attempt++ {
		if age := db.now().Sub(startedAt); age >= db.grace/2 {
			// GC may delete the compacted segment as an orphan once the compaction is a grace
			// period old, and a manifest write that lands after that would reference a missing
			// object for good, so the write is not even started past half of it.
			return false, fmt.Errorf("%w: compaction is %v old", ErrSnapshotExpired, age)
		}
		if err := progress(); err != nil {
			return false, err
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
			return false, err
		}
		cur, curEtag := db.cachedManifest()
		if attempt == 8 || cur.Seq <= base.Seq {
			assert.Sometimes(true, "a compaction gives up rebasing", nil)
			db.log.Debug("compaction failed", "error", err)
			return false, err
		}
		if !cur.extends(m) {
			assert.Sometimes(true, "a compaction loses to another compaction", nil)
			db.log.Debug("compaction failed", "error", err)
			return false, err
		}
		assert.Sometimes(true, "a compaction rebases on commits that only added segments", nil)
		reach.Hit(ctx, reach.CompactionRebase)
		db.compactionRebases.Add(1)
		base, baseEtag = cur, curEtag
	}
	assert.Reachable("compaction succeeded", nil)
	db.compactions.Add(1)
	db.log.Info("compacted", "segments", len(m.Segments), "entries", count, "seq", db.Stats().Seq)
	return true, nil
}

// extends reports whether base's segments are all still live in m, unchanged and in the same
// order, so that m differs from base only by segments added after it.
func (m *manifest) extends(base *manifest) bool {
	if len(m.Segments) < len(base.Segments) {
		return false
	}
	for i, seg := range base.Segments {
		if m.Segments[i].Key != seg.Key {
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
// than the grace period. When the manifest write loses to a commit, the pass reapplies its
// changes on top of the new manifest, so steady writers do not starve it.
// It returns [ErrConflict] when it kept losing; a retry is safe.
func (db *DB) GC(ctx context.Context) (int, error) {
	deleted, _, err := db.gc(ctx, nil)
	return deleted, err
}

// gc is [DB.GC] with an optional progress hook, called now and then during long work, whose
// error aborts the pass. It also reports whether the pass changed the manifest or deleted anything.
func (db *DB) gc(ctx context.Context, progress func() error) (int, bool, error) {
	if progress == nil {
		progress = func() error { return nil }
	}
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("engine: garbage collecting: %w", err)
	}
	now := db.now()

	referenced := map[string]bool{}
	for _, seg := range m.Segments {
		referenced[seg.Key] = true
	}
	for _, g := range m.Garbage {
		referenced[g.Key] = true
	}

	// stamp holds the garbage that left the manifest before this pass read it, which the stamp
	// taken then is safe for. expired holds the garbage whose stamp is older than the grace period.
	stamp := map[string]bool{}
	expired := map[string]bool{}
	for _, g := range m.Garbage {
		switch {
		case db.mutations.GCIgnoreGrace:
			expired[g.Key] = true
		case g.UnreferencedAt.IsZero():
			stamp[g.Key] = true
		case now.Sub(g.UnreferencedAt) >= db.grace:
			expired[g.Key] = true
		}
	}

	infos, err := db.client.List(ctx, db.key(segmentKeyPrefix))
	if err != nil {
		return 0, false, fmt.Errorf("engine: garbage collecting: listing: %w", err)
	}
	orphans := map[string]bool{}
	for _, info := range infos {
		if referenced[info.Key] {
			continue
		}
		baseSeq, ok := segmentBaseSeq(strings.TrimPrefix(info.Key, db.prefix))
		if !ok {
			continue
		}
		if baseSeq < m.Seq {
			orphans[info.Key] = true
		}
	}
	assert.Sometimes(len(orphans) > 0, "GC finds an orphaned object", nil)
	assert.Sometimes(len(expired) > 0, "GC finds an expired object", nil)
	if db.mutations.GCIgnoreGrace {
		for key := range orphans {
			expired[key] = true
		}
		orphans = map[string]bool{}
	}

	// Deletes follow the manifest's order, so that a run is reproducible.
	var toDelete []string
	for _, g := range m.Garbage {
		if expired[g.Key] {
			toDelete = append(toDelete, g.Key)
		}
	}
	for _, info := range infos {
		if expired[info.Key] && !referenced[info.Key] {
			toDelete = append(toDelete, info.Key)
		}
	}
	deleted := 0
	gone := map[string]bool{}
	failed := map[string]bool{}
	for i, key := range toDelete {
		if i%16 == 15 {
			if err := progress(); err != nil {
				return deleted, deleted > 0, err
			}
		}
		if err := db.client.Delete(ctx, key); err != nil {
			db.log.Warn("deleting expired object", "key", key, "error", err)
			failed[key] = true
			continue
		}
		gone[key] = true
		deleted++
		db.deleted.Add(1)
	}

	// The pass is reapplied on top of whatever manifest is current: deleted objects leave the
	// garbage list, failed deletes stay expired, the garbage this pass found unstamped gets this
	// pass's stamp, and orphans that nothing references yet enter the list. Garbage that entered
	// the list after this pass began is left for the next pass to stamp.
	base, baseEtag := m, etag
	for attempt := 0; ; attempt++ {
		if err := progress(); err != nil {
			return deleted, deleted > 0, err
		}
		next := base.next(db.newID(), now)
		next.Garbage = nil
		changed := false
		live := map[string]bool{}
		for _, seg := range base.Segments {
			live[seg.Key] = true
			// A deleted object can only be live when a compaction rebased on it after the grace
			// period, which it must never do. The manifest is not written then.
			assert.Always(!gone[seg.Key], "GC never deletes an object a compaction then references", map[string]any{"key": seg.Key})
			if gone[seg.Key] {
				return deleted, true, fmt.Errorf("engine: garbage collecting: deleted %q is referenced by the manifest at %d", seg.Key, base.Seq)
			}
		}
		for _, g := range base.Garbage {
			switch {
			case gone[g.Key]:
				changed = true
				continue
			case failed[g.Key] && now.Sub(g.UnreferencedAt) < db.grace:
				g.UnreferencedAt = now.Add(-db.grace)
				changed = true
			case stamp[g.Key] && g.UnreferencedAt.IsZero():
				g.UnreferencedAt = now
				changed = true
			}
			live[g.Key] = true
			next.Garbage = append(next.Garbage, g)
		}
		for _, info := range infos {
			if orphans[info.Key] && !live[info.Key] {
				next.Garbage = append(next.Garbage, garbageEntry{Key: info.Key, UnreferencedAt: now})
				changed = true
			}
		}
		if !changed {
			return deleted, deleted > 0, nil
		}
		_, err := db.casManifest(ctx, baseEtag, next)
		if err == nil {
			db.log.Info("garbage collected", "deleted", deleted, "remaining", len(next.Garbage), "seq", next.Seq)
			return deleted, true, nil
		}
		if !errors.Is(err, ErrConflict) {
			return deleted, deleted > 0, fmt.Errorf("engine: garbage collecting: %w", err)
		}
		cur, curEtag := db.cachedManifest()
		if attempt == 8 || cur.Seq <= base.Seq {
			return deleted, deleted > 0, err
		}
		assert.Sometimes(true, "a GC pass rebases on commits that landed meanwhile", nil)
		reach.Hit(ctx, reach.GCRebase)
		base, baseEtag = cur, curEtag
	}
}
