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
	LostAckLanded           State = "STATE-LOST-ACK-LANDED"
	LostAckConflict         State = "STATE-LOST-ACK-CONFLICT"
	LiveUnknown             State = "STATE-LIVE-UNKNOWN"
	AutocommitRetry         State = "STATE-AUTOCOMMIT-RETRY"
	CompactionRebase        State = "STATE-COMPACTION-REBASE"
	GCRebase                State = "STATE-GC-REBASE"
	LeaseTakeover           State = "STATE-LEASE-TAKEOVER"
	LeaseOverlap            State = "STATE-LEASE-OVERLAP"
	SnapshotExpiredRefused  State = "STATE-SNAPSHOT-EXPIRED-REFUSED"
	BlockReadRetrySucceeded State = "STATE-BLOCK-READ-RETRY-SUCCEEDED"
	CrashMidCommitRecovered State = "STATE-CRASH-MID-COMMIT-RECOVERED"
	CrashMidCompaction      State = "STATE-CRASH-MID-COMPACTION"
	CrashMidGC              State = "STATE-CRASH-MID-GC"
	UniqueAfterConflict     State = "STATE-UNIQUE-AFTER-CONFLICT"
	UniqueValueReused       State = "STATE-UNIQUE-VALUE-REUSED"
	ForeignKeyAfterConflict State = "STATE-FK-AFTER-CONFLICT"
	ForeignKeyCascadeTables State = "STATE-FK-CASCADE-TABLES"
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
		{GCRebase, "a GC pass reapplies its manifest change on top of commits that landed meanwhile", "a GC pass rebases on commits that landed meanwhile"},
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
