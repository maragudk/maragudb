package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
)

// Tiers of size-tiered compaction. A segment's tier follows from its size: tier 0 holds the
// segments smaller than BaseBytes, and tier i, for i of at least 1, the segments of at least
// BaseBytes·FanIn^(i-1) and less than BaseBytes·FanIn^i bytes.
//
// Only segments that are adjacent in age order are merged, and the merged segment takes their
// place, because a newer segment shadows the older ones. A compaction is due when FanIn adjacent
// segments are in the same tier, counted as the highest tier of the segment and of every newer
// one, so that a segment that deletes made smaller than a newer one goes along with that one.
// It merges as many adjacent segments of the tier as fit in the next tier.
// The whole database is merged into one segment when there are at least FanIn segments, and
// those after the oldest one are together FullRatio times as large as it, or hold tombstones for
// FullRatio times as many of its entries as remain. This keeps the newer segments, with their shadowed values and
// tombstones, below about FullRatio times the size of the oldest one, and reclaims the space of
// mass deletes, whose tombstones are small. Only a merge that includes the oldest segment drops
// tombstones.
//
// Each merge of FanIn segments of one tier makes a segment of the next tier, so a byte is
// rewritten about once per tier, and there are O(log n) tiers for a database of n bytes. Once no
// compaction is due, every tier holds fewer than FanIn segments, so there are O(log n) segments.
type Tiers struct {
	// FanIn is the number of adjacent segments of one tier that are merged into one. Defaults to
	// 4, and is at least 2.
	FanIn int
	// BaseBytes bounds the segments of tier 0. Defaults to 1 KiB.
	BaseBytes int64
	// FullRatio is how many times as large as the oldest segment the segments after it are
	// together when the whole database is merged, or how many times as many of its entries as
	// remain they delete. Defaults to 1.
	FullRatio int
}

func (t Tiers) withDefaults() Tiers {
	if t.FanIn <= 0 {
		t.FanIn = 4
	}
	t.FanIn = max(t.FanIn, 2)
	if t.BaseBytes <= 0 {
		t.BaseBytes = 1 << 10
	}
	if t.FullRatio <= 0 {
		t.FullRatio = 1
	}
	return t
}

// tier of a segment of the given size.
func (t Tiers) tier(size int64) int {
	tier := 0
	for limit := t.BaseBytes; size >= limit; tier++ {
		if limit > math.MaxInt64/int64(t.FanIn) {
			// The next limit does not fit, so no size reaches it.
			return tier + 1
		}
		limit *= int64(t.FanIn)
	}
	return tier
}

// limit of the sizes of the segments of a tier, BaseBytes·FanIn^tier, or [math.MaxInt64] when
// that does not fit.
func (t Tiers) limit(tier int) int64 {
	limit := t.BaseBytes
	for range tier {
		if limit > math.MaxInt64/int64(t.FanIn) {
			return math.MaxInt64
		}
		limit *= int64(t.FanIn)
	}
	return limit
}

// window of adjacent segments [start, end) in age order, which one merge replaces.
type window struct {
	start, end int
}

// plan the windows that a compaction of segs merges, oldest first. They do not overlap.
func (t Tiers) plan(segs []segmentMeta) []window {
	t = t.withDefaults()
	k := len(segs)
	if k < 2 {
		return nil
	}
	// The newer segments are measured in bytes, and in tombstones, which are small but can each
	// hide an entry of the oldest segment. With t tombstones for its c entries, at most c-t remain,
	// so t·(FullRatio+1) >= FullRatio·c holds when the deleted ones can be FullRatio times as many.
	var newer int64
	var tombstones int
	for _, seg := range segs[1:] {
		newer += seg.Size
		tombstones += seg.Tombstones
	}
	if k >= t.FanIn && (newer >= int64(t.FullRatio)*segs[0].Size || tombstones > 0 && tombstones*(t.FullRatio+1) >= t.FullRatio*segs[0].Count) {
		return []window{{0, k}}
	}
	// The tier of a segment counts as the highest tier of it and of every newer segment, so that
	// tiers never rise from the oldest segment to the newest.
	tiers := make([]int, k)
	for i := k - 1; i >= 0; i-- {
		tiers[i] = t.tier(segs[i].Size)
		if i < k-1 {
			tiers[i] = max(tiers[i], tiers[i+1])
		}
	}
	// A run of segments of one tier is merged in windows of at least FanIn, from the oldest, each
	// with as many segments as fit in the tier above, so that a merged segment is at most one tier
	// above the run, and no higher than the segment before it. Fan-in segments of the run always
	// fit, because each is smaller than the limit of the run's tier.
	var ws []window
	for start := 0; start < k; {
		end := start
		for end < k && tiers[end] == tiers[start] {
			end++
		}
		limit := t.limit(tiers[start] + 1)
		for s := start; s < end; {
			e, size := s, int64(0)
			for e < end && (e-s < t.FanIn || segs[e].Size < limit-size) {
				size += segs[e].Size
				e++
			}
			if e-s < t.FanIn {
				break
			}
			ws = append(ws, window{s, e})
			s = e
		}
		start = end
	}
	return ws
}

// merge of some segments of a manifest into one.
type merge struct {
	// members are the indexes of the merged segments in the manifest, oldest first. They are
	// adjacent, except with [Mutations.CompactNonAdjacent].
	members []int
	// out is the merged segment, which takes the place of the oldest member, or nil when nothing
	// is left of the members.
	out *segmentMeta
}

// members of the merges of the windows of a manifest with k segments.
func (db *DB) members(ws []window, k int) [][]int {
	var ms [][]int
	for _, w := range ws {
		var members []int
		for i := w.start; i < w.end; i++ {
			members = append(members, i)
		}
		if db.mutations.CompactNonAdjacent {
			// The second-oldest member stays where it is, and the segment after the window takes its
			// place in the merge.
			members = append([]int{w.start}, members[2:]...)
			if w.end < k {
				members = append(members, w.end)
			}
			if len(members) < 2 {
				continue
			}
		}
		ms = append(ms, members)
	}
	return ms
}

// replaceMerged segments of segs with the outputs of the merges.
func replaceMerged(segs []segmentMeta, merges []merge) []segmentMeta {
	outAt := map[int]*segmentMeta{}
	merged := map[int]bool{}
	for _, mg := range merges {
		outAt[mg.members[0]] = mg.out
		for _, i := range mg.members {
			merged[i] = true
		}
	}
	var out []segmentMeta
	for i, seg := range segs {
		if o := outAt[i]; o != nil {
			out = append(out, *o)
		}
		if !merged[i] {
			out = append(out, seg)
		}
	}
	return out
}

// Compact merges all segments into one, dropping tombstones and overwritten values. The old
// segments become garbage for [DB.GC]. It is safe to run concurrently with transactions in any
// process: when commits land meanwhile, the merged segment is placed behind their segments, which
// keep shadowing it, so the merge is not redone. It returns [ErrConflict] only when a segment it
// merged is no longer live, because another compaction won, and [ErrSnapshotExpired] when it took
// longer than half the GC grace period, because its output is collected as an orphan after a
// whole one.
func (db *DB) Compact(ctx context.Context) error {
	_, err := db.compact(ctx, nil, nil)
	return err
}

// CompactTiers merges the segments that the tiers make due, as [Tiers] describes, and reports
// whether it merged any. It is as safe to run concurrently as [DB.Compact], and fails in the same
// ways.
func (db *DB) CompactTiers(ctx context.Context, tiers Tiers) (bool, error) {
	tiers = tiers.withDefaults()
	return db.compact(ctx, &tiers, nil)
}

// plan the windows that a compaction of segs merges: every segment, or with tiers the windows
// that they make due.
func (db *DB) plan(tiers *Tiers, segs []segmentMeta) []window {
	switch {
	case len(segs) < 2:
		return nil
	case tiers == nil:
		return []window{{0, len(segs)}}
	case db.mutations.CompactAll && len(segs) >= tiers.FanIn:
		return []window{{0, len(segs)}}
	case db.mutations.CompactAll:
		return nil
	}
	return tiers.plan(segs)
}

// compact the windows that [DB.plan] picks in the current manifest, with an optional progress
// hook, called now and then during long work, whose error aborts the compaction. It reports
// whether segments were merged.
func (db *DB) compact(ctx context.Context, tiers *Tiers, progress func() error) (bool, error) {
	startedAt := db.now()
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return false, fmt.Errorf("engine: compacting: %w", err)
	}
	ws := db.plan(tiers, m.Segments)
	if len(ws) == 0 {
		return false, nil
	}
	db.compactionsStarted.Add(1)
	if progress == nil {
		progress = func() error { return nil }
	}

	var merges []merge
	for _, members := range db.members(ws, len(m.Segments)) {
		out, err := db.mergeSegments(ctx, startedAt, m, members, progress)
		if err != nil {
			return false, err
		}
		merges = append(merges, merge{members: members, out: out})
		if tiers != nil && tiers.tier(m.Segments[members[len(members)-1]].Size) >= 2 {
			assert.Sometimes(true, "a compaction merges segments of tier 2 or higher", nil)
			reach.Hit(ctx, reach.CompactionUpperTier)
		}
	}
	if len(merges) == 0 {
		return false, nil
	}
	if len(merges) > 1 {
		assert.Sometimes(true, "a compaction merges several windows of segments at once", nil)
		reach.Hit(ctx, reach.CompactionWindows)
	}

	// The swap replaces the merged segments with their outputs. When the write loses to commits
	// that only added segments, the swap is applied again on top of the new manifest, with the
	// newer segments after the merged ones.
	id := db.newID()
	base, baseEtag := m, etag
	for attempt := 0; ; attempt++ {
		if age := db.now().Sub(startedAt); age >= db.grace/2 {
			// GC may delete the merged segments as orphans once the compaction is a grace period
			// old, and a manifest write that lands after that would reference a missing object for
			// good, so the write is not even started past half of it.
			return false, fmt.Errorf("%w: compaction is %v old", ErrSnapshotExpired, age)
		}
		if err := progress(); err != nil {
			return false, err
		}
		newer := base.Segments[min(len(m.Segments), len(base.Segments)):]
		if db.mutations.CompactRebaseLosesSegment && len(newer) > 0 {
			newer = newer[1:]
		}
		next := base.next(id, db.now())
		next.Segments = append(replaceMerged(m.Segments, merges), newer...)
		if db.mutations.CompactedSegmentAhead {
			// The merged segments go after the newer ones, which they then shadow.
			var outs []segmentMeta
			for _, mg := range merges {
				if mg.out != nil {
					outs = append(outs, *mg.out)
				}
			}
			next.Segments = slices.DeleteFunc(next.Segments, func(seg segmentMeta) bool {
				return slices.ContainsFunc(outs, func(o segmentMeta) bool { return o.Key == seg.Key })
			})
			next.Segments = append(next.Segments, outs...)
		}
		// A GC pass may have stamped a merged segment as an orphan meanwhile, in a garbage batch
		// that keeps the stamp. GC never deletes garbage that the manifest it read has as a live
		// segment, and the stamp is later than the start of the compaction, which writes no
		// manifest once it is half a grace period old.
		for _, mg := range merges {
			for _, i := range mg.members {
				next.Garbage = append(next.Garbage, m.Segments[i].Key)
			}
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
		if !cur.extends(m) && !db.mutations.CompactRebaseOnCompaction {
			assert.Sometimes(true, "a compaction loses to another compaction", nil)
			db.log.Debug("compaction failed", "error", err)
			return false, err
		}
		assert.Sometimes(true, "a compaction rebases on commits that only added segments", nil)
		reach.Hit(ctx, reach.CompactionRebase)
		if last := merges[len(merges)-1].members; merges[0].members[0] > 0 || last[len(last)-1] < len(m.Segments)-1 {
			assert.Sometimes(true, "a compaction that left some segments alone rebases on newer commits", nil)
			reach.Hit(ctx, reach.CompactionPartialRebase)
		}
		db.compactionRebases.Add(1)
		base, baseEtag = cur, curEtag
	}
	assert.Reachable("compaction succeeded", nil)
	db.compactions.Add(1)
	db.log.Info("compacted", "windows", len(merges), "segments", len(m.Segments), "seq", db.Stats().Seq)
	return true, nil
}

// mergeSegments of m with the given indexes into one segment, which it writes, or returns nil
// when nothing is left of them. Tombstones are dropped only when the oldest segment is among the
// members, because no older segment then remains whose values a tombstone would have to keep
// hiding.
func (db *DB) mergeSegments(ctx context.Context, startedAt time.Time, m *manifest, members []int, progress func() error) (*segmentMeta, error) {
	dropTombstones := members[0] == 0 || db.mutations.CompactDropTombstones
	it := &Iterator{keepTombstones: !dropTombstones}
	for _, i := range slices.Backward(members) {
		seg := m.Segments[i]
		fetch := db.fetcher(ctx, startedAt, seg)
		// Block reads are the slow part of the merge, so progress is reported before each.
		it.sources = append(it.sources, newSegmentIterator(seg.Root, nil, func(ref blockRef) (*decodedBlock, error) {
			if err := progress(); err != nil {
				return nil, err
			}
			db.compactionBytesRead.Add(ref.Length)
			return fetch(ref)
		}))
	}
	b := newSegmentBuilder(db.blockSize)
	var count, tombstones int
	for it.Next() {
		if err := b.add(it.Key(), it.Value(), it.tombstone); err != nil {
			return nil, err
		}
		count++
		if it.tombstone {
			tombstones++
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("engine: compacting: %w", err)
	}
	if tombstones > 0 {
		assert.Sometimes(true, "a compaction that excludes the oldest segment keeps a tombstone", nil)
		reach.Hit(ctx, reach.CompactionKeepsTombstone)
	}
	if count == 0 {
		return nil, nil
	}
	body, meta, err := b.finish(db.key(segmentKey(m.Seq, db.newID())))
	if err != nil {
		return nil, err
	}
	if err := progress(); err != nil {
		return nil, err
	}
	if err := db.putObject(ctx, meta.Key, body); err != nil {
		return nil, err
	}
	db.compactionBytesWritten.Add(int64(len(body)))
	// The whole segment is in memory, so the memory cache is primed with every block when the
	// segment leaves room for other blocks, and reads in this process stay warm across the
	// compaction. The disk cache is left to fill with the blocks that are read.
	if db.mem != nil && int64(len(body)) <= db.memBytes/2 {
		for _, ref := range b.blocks {
			// A copy per block, so that a cached block does not pin the whole segment.
			db.mem.Put(blockCacheKey(meta.Key, ref), bytes.Clone(body[ref.Offset:ref.Offset+ref.Length]))
		}
	}
	return &meta, nil
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
// many it deleted. A pass stamps the garbage that left the segment list before it read the
// manifest with its own clock, by listing the keys in a garbage batch object that the manifest
// references, and deletes the garbage of the batches whose stamp is older than the grace period,
// and then the batch objects themselves. Objects that never entered the manifest, because the
// commit, compaction, or GC pass that wrote them failed or crashed, are stamped the same way once
// the manifest has moved past the sequence number they were written against, because a
// compaction or GC pass that rebases may still reference its output until it is half a grace
// period old. When the manifest write loses to a commit, the pass reapplies its changes on top of
// the new manifest, so steady writers do not starve it.
// It returns [ErrConflict] when it kept losing, or when another pass deleted a garbage batch that
// it was reading, and [ErrSnapshotExpired] when it is older than half the grace period before its
// manifest write; a retry is safe after either.
func (db *DB) GC(ctx context.Context) (int, error) {
	deleted, _, err := db.gc(ctx, nil)
	return deleted, err
}

// chain of garbage batches, as read from the store.
type chain struct {
	// objects are the keys of the batch objects of the chain, newest first.
	objects []string
	// garbage are the keys that the batches of the chain list.
	garbage []string
}

// gc is [DB.GC] with an optional progress hook, called now and then during long work, whose
// error aborts the pass. It also reports whether the pass changed the manifest or deleted anything.
func (db *DB) gc(ctx context.Context, progress func() error) (int, bool, error) {
	if progress == nil {
		progress = func() error { return nil }
	}
	startedAt := db.now()
	m, etag, err := db.refreshManifest(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("engine: garbage collecting: %w", err)
	}
	// The stamp is taken after the manifest was read, so that every snapshot that still has the
	// garbage this pass stamps began before the stamp.
	now := db.now()
	chains, err := db.readChains(ctx, m, progress)
	if err != nil {
		return 0, false, fmt.Errorf("engine: garbage collecting: %w", err)
	}

	expired := map[string]bool{}
	for _, b := range m.Batches {
		if db.mutations.GCIgnoreGrace || now.Sub(b.StampedAt) >= db.grace {
			expired[b.Key] = true
		}
	}
	// referenced holds every object that the manifest references. protected holds the ones that
	// must outlive this pass even when an expired chain lists them: the live segments, the
	// unstamped garbage, the garbage of the chains that have not expired, and the batch objects
	// of every chain, which are deleted only after the manifest no longer references them.
	referenced := map[string]bool{}
	protected := map[string]bool{}
	for _, seg := range m.Segments {
		referenced[seg.Key], protected[seg.Key] = true, !db.mutations.GCDeleteReferenced
	}
	for _, key := range m.Garbage {
		referenced[key], protected[key] = true, !db.mutations.GCDeleteReferenced
	}
	for _, b := range m.Batches {
		for _, key := range chains[b.Key].objects {
			referenced[key], protected[key] = true, true
		}
		for _, key := range chains[b.Key].garbage {
			referenced[key] = true
			if !expired[b.Key] && !db.mutations.GCDeleteReferenced {
				protected[key] = true
			}
		}
	}

	prefixes := []string{segmentKeyPrefix, garbageBatchKeyPrefix}
	if db.mutations.GCSweepSegmentsOnly {
		prefixes = prefixes[:1]
	}
	var orphans []string
	// listed holds the objects that the listing shows. Expired garbage is deleted whether listed or
	// not, because a listing may miss an object, but only listed objects count as deleted.
	listed := map[string]bool{}
	for _, prefix := range prefixes {
		infos, err := db.client.List(ctx, db.key(prefix))
		if err != nil {
			return 0, false, fmt.Errorf("engine: garbage collecting: listing: %w", err)
		}
		for _, info := range infos {
			listed[info.Key] = true
			if referenced[info.Key] {
				continue
			}
			if baseSeq, ok := objectBaseSeq(strings.TrimPrefix(info.Key, db.prefix)); ok && baseSeq < m.Seq {
				orphans = append(orphans, info.Key)
			}
		}
	}
	assert.Sometimes(len(orphans) > 0, "GC finds an orphaned object", nil)
	assert.Sometimes(len(expired) > 0, "GC finds an expired object", nil)

	// stamp holds the garbage that this pass stamps: what left the segment list before the pass
	// read the manifest, which the stamp taken then is safe for, and the orphans.
	stamp := append(slices.Clone(m.Garbage), orphans...)
	// Deletes follow the manifest's order, so that a run is reproducible.
	var toDelete []string
	queued := map[string]bool{}
	live := map[string]bool{}
	for _, seg := range m.Segments {
		live[seg.Key] = true
	}
	for _, b := range m.Batches {
		if !expired[b.Key] {
			continue
		}
		if slices.ContainsFunc(chains[b.Key].garbage, func(key string) bool { return live[key] }) {
			assert.Sometimes(true, "GC keeps a live segment that an expired garbage batch lists", nil)
			reach.Hit(ctx, reach.GCKeepsLiveGarbage)
		}
		if db.mutations.GCBatchObjectsFirst {
			toDelete = append(toDelete, chains[b.Key].objects...)
		}
		for _, key := range chains[b.Key].garbage {
			if !protected[key] && !queued[key] {
				queued[key] = true
				toDelete = append(toDelete, key)
			}
		}
	}
	if db.mutations.GCIgnoreGrace {
		toDelete = append(toDelete, stamp...)
		stamp = nil
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
		if listed[key] {
			deleted++
			db.deleted.Add(1)
		}
	}
	// An expired chain leaves the manifest once all of its garbage is gone, and the unstamped
	// garbage leaves the list once it is in this pass's batch.
	drop := map[string]bool{}
	for head := range expired {
		drop[head] = !slices.ContainsFunc(chains[head].garbage, func(key string) bool { return failed[key] })
	}
	removed := map[string]bool{}
	for _, key := range m.Garbage {
		removed[key] = !failed[key]
	}

	// The pass is reapplied on top of whatever manifest is current: the expired chains whose
	// garbage is gone leave it, the garbage this pass found unstamped leaves the list for this
	// pass's batch, and the batch joins the newest chain, or starts a new one. Garbage that
	// entered the list after this pass began is left for the next pass to stamp.
	var batch *garbageBatch
	var batchKey string
	base, baseEtag := m, etag
	for attempt := 0; ; attempt++ {
		if err := progress(); err != nil {
			return deleted, deleted > 0, err
		}
		next := base.next(db.newID(), now)
		changed := false
		for _, seg := range base.Segments {
			// A deleted object can only be live when a compaction rebased on it after the grace
			// period, which it must never do. The manifest is not written then.
			assert.Always(!gone[seg.Key], "GC never deletes an object a compaction then references", map[string]any{"key": seg.Key})
			if gone[seg.Key] {
				return deleted, true, fmt.Errorf("engine: garbage collecting: deleted %q is referenced by the manifest at %d", seg.Key, base.Seq)
			}
		}
		next.Garbage = nil
		for _, key := range base.Garbage {
			if removed[key] {
				changed = true
				continue
			}
			next.Garbage = append(next.Garbage, key)
		}
		next.Batches = nil
		var dropped []string
		for _, b := range base.Batches {
			if drop[b.Key] {
				changed = true
				dropped = append(dropped, b.Key)
				continue
			}
			next.Batches = append(next.Batches, b)
		}
		if len(stamp) > 0 {
			changed = true
			if age := db.now().Sub(startedAt); age >= db.grace/2 {
				// Once the manifest has moved past the batch object, another pass stamps it as an
				// orphan and deletes it a grace period later, and a manifest write that lands after
				// that would reference a missing batch for good, so the write is not even started
				// past half of it.
				return deleted, deleted > 0, fmt.Errorf("%w: garbage collection is %v old", ErrSnapshotExpired, age)
			}
			ref := batchRef{Since: now, StampedAt: now, Objects: len(stamp) + 1}
			prev := ""
			if n := len(next.Batches); n > 0 && (db.mutations.GCChainKeepsFirstStamp || now.Sub(next.Batches[n-1].Since) < db.grace/8) {
				last := next.Batches[n-1]
				prev = last.Key
				ref.Since = last.Since
				ref.StampedAt = last.StampedAt
				if now.After(ref.StampedAt) && !db.mutations.GCChainKeepsFirstStamp {
					ref.StampedAt = now
				}
				ref.Objects += last.Objects
				next.Batches = next.Batches[:n-1]
			}
			if batch != nil && batch.Prev != prev {
				assert.Sometimes(true, "a GC pass writes its garbage batch again for another chain", nil)
				reach.Hit(ctx, reach.GCBatchRewritten)
			}
			if batch == nil || batch.Prev != prev {
				// A batch written for an earlier attempt that joined another chain is an orphan now.
				batch = &garbageBatch{Version: garbageBatchVersion, StampedAt: now, Keys: stamp, Prev: prev}
				batchKey = db.key(garbageBatchKey(base.Seq, db.newID()))
				body, err := encodeGarbageBatch(batchKey, batch)
				if err != nil {
					return deleted, deleted > 0, err
				}
				if err := db.putObject(ctx, batchKey, body); err != nil {
					return deleted, deleted > 0, fmt.Errorf("engine: garbage collecting: %w", err)
				}
				db.cacheBatch(batchKey, batch)
			}
			ref.Key = batchKey
			next.Batches = append(next.Batches, ref)
		}
		if !changed {
			return deleted, deleted > 0, nil
		}
		_, err := db.casManifest(ctx, baseEtag, next)
		if err == nil {
			// The batch objects of the dropped chains are unreferenced now. A pass that read an
			// older manifest and still reads them fails and can be retried, and one that this
			// process does not get to delete is swept as an orphan.
			for _, head := range dropped {
				assert.Sometimes(true, "a GC pass drops an expired chain and deletes its batches", nil)
				reach.Hit(ctx, reach.GCChainDropped)
				for _, key := range chains[head].objects {
					if err := db.client.Delete(ctx, key); err != nil {
						db.log.Warn("deleting garbage batch", "key", key, "error", err)
						continue
					}
					deleted++
					db.deleted.Add(1)
				}
			}
			db.log.Info("garbage collected", "deleted", deleted, "unstamped", len(next.Garbage), "chains", len(next.Batches), "seq", next.Seq)
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

// readChains reads the garbage batch objects of every chain that m references, from the cache
// when it has them, and keeps exactly those in the cache. The progress hook is called before each
// read from the store.
func (db *DB) readChains(ctx context.Context, m *manifest, progress func() error) (map[string]chain, error) {
	keep := map[string]*garbageBatch{}
	chains := map[string]chain{}
	for _, ref := range m.Batches {
		var c chain
		for key := ref.Key; key != ""; {
			if _, ok := keep[key]; ok {
				return nil, fmt.Errorf("%w: garbage batch %q is in two chains or twice in one", ErrCorrupt, key)
			}
			db.batchMu.Lock()
			b, ok := db.batches[key]
			db.batchMu.Unlock()
			if !ok {
				// A cold cache reads every batch, which is long work.
				if err := progress(); err != nil {
					return nil, err
				}
				var err error
				if b, err = db.getGarbageBatch(ctx, m, key); err != nil {
					return nil, err
				}
			}
			keep[key] = b
			c.objects = append(c.objects, key)
			c.garbage = append(c.garbage, b.Keys...)
			key = b.Prev
		}
		chains[ref.Key] = c
	}
	db.batchMu.Lock()
	db.batches = keep
	db.batchMu.Unlock()
	return chains, nil
}

// getGarbageBatch from the store, which m references.
func (db *DB) getGarbageBatch(ctx context.Context, m *manifest, key string) (*garbageBatch, error) {
	res, err := db.client.Get(ctx, key, s3.GetOptions{Consistent: true})
	if errors.Is(err, s3.ErrNotFound) {
		// Another pass may have dropped the chain and deleted its objects after m was read. When
		// the manifest is still m, it references a missing batch, which must never happen.
		cur, _, rerr := db.refreshManifest(ctx)
		if rerr != nil || cur.Seq != m.Seq {
			return nil, fmt.Errorf("%w: garbage batch %q is gone, and the manifest moved on", ErrConflict, key)
		}
		if db.mutations == (Mutations{}) {
			assert.Unreachable("the manifest references a missing garbage batch", map[string]any{"key": key})
		}
		return nil, fmt.Errorf("engine: the manifest at %d references the missing garbage batch %q", m.Seq, key)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: reading garbage batch %q: %w", key, err)
	}
	b, err := decodeGarbageBatch(key, res.Body)
	if err != nil {
		db.corruptRead(ctx, err)
		return nil, err
	}
	return b, nil
}

// cacheBatch that this process wrote.
func (db *DB) cacheBatch(key string, b *garbageBatch) {
	db.batchMu.Lock()
	defer db.batchMu.Unlock()
	if db.batches == nil {
		db.batches = map[string]*garbageBatch{}
	}
	db.batches[key] = b
}
