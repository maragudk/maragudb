// Package reach counts how often a run reaches the interesting states that a test corpus must
// reach for its passing checkers to mean anything, in the style of Antithesis's Sometimes
// assertions. Every state is also marked by an Antithesis SDK assertion with the declared message,
// next to the [Hit] that counts it.
//
// Counting is scoped to a context: a run that wants counts puts a [Counter] in its context with
// [WithCounter], and a [Hit] on a context without one costs a context lookup and nothing else.
package reach

import (
	"context"
	"sync"
)

// State is the stable ID of an interesting state.
type State string

// States.
const (
	LostAckLanded            State = "STATE-LOST-ACK-LANDED"
	LostAckConflict          State = "STATE-LOST-ACK-CONFLICT"
	LiveUnknown              State = "STATE-LIVE-UNKNOWN"
	AutocommitRetry          State = "STATE-AUTOCOMMIT-RETRY"
	CompactionRebase         State = "STATE-COMPACTION-REBASE"
	CompactionPartialRebase  State = "STATE-COMPACTION-PARTIAL-REBASE"
	CompactionKeepsTombstone State = "STATE-COMPACTION-KEEPS-TOMBSTONE"
	CompactionUpperTier      State = "STATE-COMPACTION-UPPER-TIER"
	CompactionWindows        State = "STATE-COMPACTION-WINDOWS"
	GCRebase                 State = "STATE-GC-REBASE"
	GCKeepsLiveGarbage       State = "STATE-GC-KEEPS-LIVE-GARBAGE"
	GCBatchRewritten         State = "STATE-GC-BATCH-REWRITTEN"
	GCChainDropped           State = "STATE-GC-CHAIN-DROPPED"
	LeaseTakeover            State = "STATE-LEASE-TAKEOVER"
	LeaseOverlap             State = "STATE-LEASE-OVERLAP"
	SnapshotExpiredRefused   State = "STATE-SNAPSHOT-EXPIRED-REFUSED"
	BlockReadRetrySucceeded  State = "STATE-BLOCK-READ-RETRY-SUCCEEDED"
	CrashMidCommitRecovered  State = "STATE-CRASH-MID-COMMIT-RECOVERED"
	CrashMidCompaction       State = "STATE-CRASH-MID-COMPACTION"
	CrashMidGC               State = "STATE-CRASH-MID-GC"
	UniqueAfterConflict      State = "STATE-UNIQUE-AFTER-CONFLICT"
	UniqueValueReused        State = "STATE-UNIQUE-VALUE-REUSED"
	ForeignKeyAfterConflict  State = "STATE-FK-AFTER-CONFLICT"
	ForeignKeyCascadeTables  State = "STATE-FK-CASCADE-TABLES"
	DiskCacheEviction        State = "STATE-DISK-CACHE-EVICTION"
	DiskCacheRefused         State = "STATE-DISK-CACHE-REFUSED"
	DamagedReadRefused       State = "STATE-DAMAGED-READ-REFUSED"
	FilterSkip               State = "STATE-FILTER-SKIP"
	FilterBlockRead          State = "STATE-FILTER-BLOCK-READ"
)

// Declared state, with what it means and the message of the Antithesis assertion that marks it.
type Declared struct {
	State       State
	Description string
	Assertion   string
}

// Declarations of every state, in the order they are reported.
func Declarations() []Declared {
	return []Declared{
		{LostAckLanded, "a manifest write whose answer was lost is resolved as landed", "a lost manifest write is found to have landed"},
		{LostAckConflict, "a manifest write without a definite answer, from a lost acknowledgement or a failed request, is resolved as a conflict, because another commit took its sequence number", "a lost manifest write is found to have lost to a concurrent commit"},
		{LiveUnknown, "a commit outcome is unknown in a client process that did not crash", "a commit outcome is unknown in a process that keeps running"},
		{AutocommitRetry, "an autocommit statement retries after a conflict", "an autocommit statement retries after a conflict"},
		{CompactionRebase, "a compaction reapplies its swap on top of commits that landed meanwhile", "a compaction rebases on commits that only added segments"},
		{CompactionPartialRebase, "a compaction that left the oldest or the newest segments alone reapplies its swap on top of commits that landed meanwhile, which go after the segments it left alone", "a compaction that left some segments alone rebases on newer commits"},
		{CompactionKeepsTombstone, "a merge of segments that excludes the oldest one keeps a tombstone, because an older segment may still hold the key", "a compaction that excludes the oldest segment keeps a tombstone"},
		{CompactionUpperTier, "a tiered compaction merges segments of tier 2 or higher, which earlier merges made", "a compaction merges segments of tier 2 or higher"},
		{CompactionWindows, "one compaction merges several separate windows of adjacent segments, and swaps them all in one manifest write", "a compaction merges several windows of segments at once"},
		{GCRebase, "a GC pass reapplies its manifest change on top of commits that landed meanwhile", "a GC pass rebases on commits that landed meanwhile"},
		{GCKeepsLiveGarbage, "an expired garbage batch lists a live segment, the output of a compaction that a GC pass stamped as an orphan before the compaction's rebase landed, and GC keeps it", "GC keeps a live segment that an expired garbage batch lists"},
		{GCBatchRewritten, "a GC pass that rebases writes its garbage batch again, because the chain it was to join changed meanwhile, and the first one becomes an orphan", "a GC pass writes its garbage batch again for another chain"},
		{GCChainDropped, "a GC pass deletes the garbage of an expired chain of garbage batches, drops the chain from the manifest, and then deletes its batch objects", "a GC pass drops an expired chain and deletes its batches"},
		{LeaseTakeover, "a maintainer takes over a lease that another holder let expire", "a maintainer takes over an expired lease"},
		{LeaseOverlap, "two maintainers believe they hold the lease at the same time", "two maintainers believe they hold the lease at once"},
		{SnapshotExpiredRefused, "a reader older than the grace period is refused with ErrSnapshotExpired", "a reader older than the grace period is told so"},
		{BlockReadRetrySucceeded, "a block read that failed transiently is retried and then succeeds", "a retried block read succeeds"},
		{CrashMidCommitRecovered, "a client crashes in the middle of a commit, restarts, and commits again", "a client that crashed during a commit recovers and commits"},
		{CrashMidCompaction, "a process crashes in the middle of a compaction", "a process crashes during a compaction"},
		{CrashMidGC, "a process crashes in the middle of a GC pass", "a process crashes during a GC pass"},
		{UniqueAfterConflict, "an autocommit statement that passed its unique checks loses its commit to a concurrent one, and its retry fails with a unique violation that the concurrent commit causes", "a unique violation occurs only because of a concurrent commit"},
		{UniqueValueReused, "a client inserts a row with a unique value that its own committed UPDATE or DELETE freed", "a unique value that a committed write freed is taken again"},
		{ForeignKeyAfterConflict, "an autocommit statement that passed its foreign key checks loses its commit to a concurrent one, and its retry fails with a foreign key violation that the concurrent commit causes", "a foreign key violation occurs only because of a concurrent commit"},
		{ForeignKeyCascadeTables, "one statement's ON DELETE CASCADE actions delete rows in more than one table", "a cascade deletes rows in more than one table"},
		{DiskCacheEviction, "a client's disk cache evicts its least recently used entries to stay within its bound", "a disk cache evicts to stay within its bound"},
		{DiskCacheRefused, "a client's disk cache refuses a file that a crash damaged or removed, and the read goes to the store", "a disk cache refuses a damaged file"},
		{DamagedReadRefused, "a read from the store gets damaged bytes of a block, the manifest, or a garbage batch, and fails with ErrCorrupt instead of serving or caching them", "a read of damaged bytes from the store fails with ErrCorrupt"},
		{FilterSkip, "a point lookup skips a segment whose key range holds the key, because the segment's key filter does not hold it", "a point lookup skips a segment whose filter does not hold the key"},
		{FilterBlockRead, "a point lookup with neither the root block nor the filter block of a segment in a cache reads both in one range read, and checks each", "a point lookup reads a filter block with its root block"},
	}
}

// Counter of reached states. It is safe for concurrent use.
type Counter struct {
	mu     sync.Mutex
	counts map[State]int
}

// Counts so far, by state.
func (c *Counter) Counts() map[State]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	counts := make(map[State]int, len(c.counts))
	for s, n := range c.counts {
		counts[s] = n
	}
	return counts
}

type contextKey struct{}

// WithCounter returns a context whose [Hit] calls count in c.
func WithCounter(ctx context.Context, c *Counter) context.Context {
	return context.WithValue(ctx, contextKey{}, c)
}

// Hit records that the state was reached, in the [Counter] of ctx if it has one.
func Hit(ctx context.Context, s State) {
	c, ok := ctx.Value(contextKey{}).(*Counter)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[State]int{}
	}
	c.counts[s]++
}
