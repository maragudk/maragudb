// Package engine is the storage and transaction core: an ordered key space of immutable segment
// objects in S3, a manifest committed by compare-and-swap, snapshot transactions with optimistic
// multi-writer commits, optional caches, compaction, and garbage collection.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/cache"
	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
)

var (
	// ErrConflict is returned by a commit that lost to a concurrent commit. The transaction can be retried.
	ErrConflict = errors.New("engine: transaction conflict, retry the transaction")
	// ErrCommitUnknown is returned when the outcome of a commit could not be determined after the
	// store stopped answering. The commit may or may not have been applied.
	ErrCommitUnknown = errors.New("engine: commit outcome unknown")
	// ErrSnapshotExpired is returned when a transaction's snapshot is older than the GC grace period
	// and an object it references is gone.
	ErrSnapshotExpired = errors.New("engine: snapshot expired, the transaction is older than the GC grace period")
	// ErrTxDone is returned when using a transaction after commit or rollback.
	ErrTxDone = errors.New("engine: transaction already committed or rolled back")
	// ErrReadOnly is returned when writing in a read-only transaction.
	ErrReadOnly = errors.New("engine: read-only transaction")
	// ErrUnsupportedManifest is returned for a manifest in an encoding that this version does not
	// read, such as one written by an earlier version.
	ErrUnsupportedManifest = errors.New("engine: unsupported manifest version")
	// ErrCorrupt is returned when an object in the store has damaged bytes: a block of a segment,
	// the manifest, or a garbage batch that fails its checksum or does not decode, or a value
	// that a layer above decodes from them and finds invalid. Damaged bytes are never served.
	ErrCorrupt = errors.New("engine: corrupt data")
)

// Options for [Open].
type Options struct {
	// Client for the object store. Required.
	Client s3.Client
	// Prefix under which all objects live. A trailing slash is added when missing.
	Prefix string
	// Logger for debug and warning messages. Discarded when nil.
	Logger *slog.Logger
	// MemoryCacheBytes bounds the in-memory block cache. Defaults to 64 MiB. Zero disables it.
	MemoryCacheBytes *int64
	// DiskCache is an optional second cache tier. It must be dedicated to this one database,
	// because entries are keyed by object key under Prefix only.
	DiskCache cache.Cache
	// BlockSize of segment blocks in bytes. Defaults to 16 KiB and must be positive.
	BlockSize *int
	// GCGracePeriod is how long an unreferenced object is kept so that readers with older snapshots
	// can still read it. Defaults to one hour. Transactions older than this may fail with [ErrSnapshotExpired].
	// The period is measured on the clocks of the processes that start transactions and that run GC,
	// so it must exceed the clock skew between processes by a comfortable margin.
	GCGracePeriod *time.Duration
	// Now is the clock. Defaults to [time.Now].
	Now func() time.Time
	// Sleep waits for d or until ctx ends, between retries of a failed read. Defaults to a real
	// sleep. It must be consistent with Now.
	Sleep func(ctx context.Context, d time.Duration)
	// Rand is the source of commit and segment identifiers. Defaults to a cryptographically seeded source.
	Rand *mathrand.Rand
	// Mutations deliberately break the engine. For tests only.
	Mutations Mutations
}

// Mutations are switches that break the engine on purpose, so that a test can prove it would
// notice each kind of bug. All of them are off in correct operation.
type Mutations struct {
	// CommitWithoutCAS writes the manifest unconditionally.
	CommitWithoutCAS bool
	// GCIgnoreGrace deletes unreferenced objects immediately.
	GCIgnoreGrace bool
	// SkipFreshnessCheck starts transactions from the cached manifest without asking the store.
	SkipFreshnessCheck bool
	// AckBeforeConfirm reports a commit as successful without waiting for the store's answer.
	AckBeforeConfirm bool
	// CompactedSegmentAhead puts the compacted segment ahead of the segments that were committed
	// during the compaction when it rebases, so that the merged values shadow the newer ones.
	CompactedSegmentAhead bool
	// GCBatchObjectsFirst deletes the batch objects of an expired chain of garbage batches before
	// its garbage, and before the manifest write that drops the chain.
	GCBatchObjectsFirst bool
	// GCSweepSegmentsOnly sweeps only segments for orphans, and not garbage batch objects.
	GCSweepSegmentsOnly bool
	// GCChainKeepsFirstStamp adds every GC pass's batch to the newest chain of garbage batches,
	// however old the chain is, and keeps the stamp of its oldest batch as the stamp of the chain.
	GCChainKeepsFirstStamp bool
	// GCDeleteReferenced deletes the garbage of an expired chain of garbage batches even when the
	// manifest still references it otherwise: as a live segment, as unstamped garbage, or in a
	// chain that has not expired.
	GCDeleteReferenced bool
	// CompactNonAdjacent leaves the second-oldest segment of every merge where it is, and merges
	// the segment after the merge's window instead, so that the skipped segment shadows newer
	// values in the merged one.
	CompactNonAdjacent bool
	// CompactDropTombstones drops tombstones in every merge, also in one that excludes the
	// oldest segment, whose older values then show again.
	CompactDropTombstones bool
	// CompactRebaseLosesSegment leaves out the oldest of the segments that commits added during
	// a compaction when the compaction rebases on them.
	CompactRebaseLosesSegment bool
	// CompactRebaseOnCompaction rebases a compaction on every newer manifest, also on one in which
	// another compaction replaced segments, instead of only on one that commits extended, and
	// takes the segments after as many as the compaction began with as the newer ones.
	CompactRebaseOnCompaction bool
	// CompactAll merges every segment whenever there are at least [Tiers.FanIn] segments, instead
	// of the segments that the tiers make due, as compaction did before tiers.
	CompactAll bool
	// SegmentUnchecked decodes the blocks of segments without checking their checksums, and
	// caches the blocks it reads from the store before decoding them.
	SegmentUnchecked bool
	// FilterOmitsKeys leaves every other key of a segment out of its key filter, so that a point
	// lookup skips a segment that holds its key and reads an older value, or none.
	FilterOmitsKeys bool
	// FilterUnchecked decodes the filter blocks of segments without checking their checksums, and
	// caches the filter blocks it reads from the store before decoding them.
	FilterUnchecked bool
	// MaintenancePanics makes one in three [Maintainer] steps that get to work panic with
	// [ErrInjectedPanic], at one of the first eight calls of the step's progress hook, which the
	// random source picks: before a block read or the segment write of a merge, before the
	// manifest write of a compaction, or between the reads, the writes, and the deletes of
	// garbage collection. It is a fault rather than a bug: the step recovers the panic.
	MaintenancePanics bool
	// MaintenanceUnrecovered lets a panic in a [Maintainer] step unwind out of the step instead of
	// recovering it.
	MaintenanceUnrecovered bool
}

// Stats about store requests and cache effectiveness.
type Stats struct {
	S3                s3.Counts
	MemoryCacheHits   int64
	MemoryCacheMisses int64
	DiskCacheHits     int64
	DiskCacheMisses   int64
	// MemoryCacheRefused and DiskCacheRefused count the blocks and manifests that a cache returned
	// and that failed their check, which a cache that keeps what it was given never returns. Such a
	// read goes to the store instead.
	MemoryCacheRefused, DiskCacheRefused int64
	// Seq is the latest known commit sequence number.
	Seq uint64
	// Segments currently referenced by the latest known manifest.
	Segments int
	// Garbage objects awaiting their grace period.
	Garbage int
	// CompactionsStarted counts the compactions that began merging, Compactions the ones that
	// landed, CompactionRebases the times a compaction reapplied its swap on top of commits that
	// landed meanwhile instead of failing, and Deleted the objects garbage collection deleted.
	CompactionsStarted, Compactions, CompactionRebases, Deleted int64
	// MaintenanceRuns counts the [Maintainer] steps that changed the database, and
	// LeaseAcquisitions and LeaseLosses the times a maintainer acquired the maintenance lease
	// and lost it to another maintainer.
	MaintenanceRuns, LeaseAcquisitions, LeaseLosses int64
	// CommittedBytes counts the bytes of the segments of the commits that succeeded,
	// CompactionBytesRead the bytes of the blocks that compactions merged, from the caches or the
	// store, and CompactionBytesWritten the bytes of the segments that they wrote, also of the
	// compactions that failed afterwards.
	CommittedBytes, CompactionBytesRead, CompactionBytesWritten int64
}

// DB is an open database. It is safe for concurrent use.
type DB struct {
	client    *s3.CountingClient
	prefix    string
	log       *slog.Logger
	mem       *cache.Memory
	disk      cache.Cache
	blockSize int
	memBytes  int64
	grace     time.Duration
	now       func() time.Time
	sleep     func(ctx context.Context, d time.Duration)
	mutations Mutations

	randMu sync.Mutex
	rand   *mathrand.Rand

	manifestMu sync.Mutex
	manifest   *manifest
	etag       string

	// batches caches the garbage batch objects of the chains that the last GC pass read, which
	// are immutable, so that a pass reads only the batches that are new to it.
	batchMu sync.Mutex
	batches map[string]*garbageBatch

	memHits, memMisses, diskHits, diskMisses                    atomic.Int64
	memRefused, diskRefused                                     atomic.Int64
	compactionsStarted, compactions, compactionRebases, deleted atomic.Int64
	maintenanceRuns, leaseAcquisitions, leaseLosses             atomic.Int64
	committedBytes, compactionBytesRead, compactionBytesWritten atomic.Int64
}

// Open a database, creating it when no manifest exists.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.Client == nil {
		return nil, errors.New("engine: Client is required")
	}
	db := &DB{
		client:    s3.NewCountingClient(opts.Client),
		prefix:    opts.Prefix,
		log:       opts.Logger,
		disk:      opts.DiskCache,
		blockSize: 16 << 10,
		grace:     time.Hour,
		now:       opts.Now,
		sleep:     opts.Sleep,
		rand:      opts.Rand,
		mutations: opts.Mutations,
	}
	if db.prefix != "" && !strings.HasSuffix(db.prefix, "/") {
		db.prefix += "/"
	}
	if db.log == nil {
		db.log = slog.New(slog.DiscardHandler)
	}
	memBytes := int64(64 << 20)
	if opts.MemoryCacheBytes != nil {
		memBytes = *opts.MemoryCacheBytes
	}
	if memBytes > 0 {
		db.mem = cache.NewMemory(memBytes)
		db.memBytes = memBytes
	}
	if opts.BlockSize != nil {
		if *opts.BlockSize <= 0 {
			return nil, fmt.Errorf("engine: BlockSize must be positive, got %d", *opts.BlockSize)
		}
		db.blockSize = *opts.BlockSize
	}
	if opts.GCGracePeriod != nil {
		db.grace = *opts.GCGracePeriod
	}
	if db.now == nil {
		db.now = time.Now
	}
	if db.sleep == nil {
		db.sleep = func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}
	}
	if db.rand == nil {
		var seed [16]byte
		if _, err := rand.Read(seed[:]); err != nil {
			return nil, fmt.Errorf("engine: seeding: %w", err)
		}
		db.rand = mathrand.New(mathrand.NewPCG(binary.LittleEndian.Uint64(seed[:8]), binary.LittleEndian.Uint64(seed[8:])))
	}

	db.loadManifestFromDisk()
	if _, _, err := db.refreshManifest(ctx); err != nil {
		if !errors.Is(err, s3.ErrNotFound) {
			return nil, err
		}
		if err := db.createManifest(ctx); err != nil {
			return nil, err
		}
	}
	return db, nil
}

// Close the database. It does not affect the store.
func (db *DB) Close() error {
	return nil
}

// Stats so far.
func (db *DB) Stats() Stats {
	m, _ := db.cachedManifest()
	s := Stats{
		S3:                 db.client.Counter.Counts(),
		MemoryCacheHits:    db.memHits.Load(),
		MemoryCacheMisses:  db.memMisses.Load(),
		DiskCacheHits:      db.diskHits.Load(),
		DiskCacheMisses:    db.diskMisses.Load(),
		MemoryCacheRefused: db.memRefused.Load(),
		DiskCacheRefused:   db.diskRefused.Load(),
		CompactionsStarted: db.compactionsStarted.Load(),
		Compactions:        db.compactions.Load(),
		CompactionRebases:  db.compactionRebases.Load(),
		Deleted:            db.deleted.Load(),
		MaintenanceRuns:    db.maintenanceRuns.Load(),
		LeaseAcquisitions:  db.leaseAcquisitions.Load(),
		LeaseLosses:        db.leaseLosses.Load(),

		CommittedBytes:         db.committedBytes.Load(),
		CompactionBytesRead:    db.compactionBytesRead.Load(),
		CompactionBytesWritten: db.compactionBytesWritten.Load(),
	}
	if m != nil {
		s.Seq = m.Seq
		s.Segments = len(m.Segments)
		s.Garbage = len(m.Garbage)
		for _, b := range m.Batches {
			s.Garbage += b.Objects
		}
	}
	return s
}

func (db *DB) key(name string) string {
	return db.prefix + name
}

func (db *DB) manifestKey() string {
	return db.key("manifest")
}

func (db *DB) newID() string {
	db.randMu.Lock()
	defer db.randMu.Unlock()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], db.rand.Uint64())
	return hex.EncodeToString(b[:])
}

// intN drawn uniformly from [0, n), for n > 0.
func (db *DB) intN(n int) int {
	db.randMu.Lock()
	defer db.randMu.Unlock()
	return db.rand.IntN(n)
}

// jitter drawn uniformly from [0, d), for d > 0.
func (db *DB) jitter(d time.Duration) time.Duration {
	db.randMu.Lock()
	defer db.randMu.Unlock()
	return time.Duration(db.rand.Int64N(int64(d)))
}

// cachedManifest and its ETag, if any.
func (db *DB) cachedManifest() (*manifest, string) {
	db.manifestMu.Lock()
	defer db.manifestMu.Unlock()
	return db.manifest, db.etag
}

// setManifest as the cached one. A manifest the store answered with always wins, because the
// store is the truth and a cached one may belong to a database that no longer exists. One that
// a local write produced only wins when it is newer than the cached one.
func (db *DB) setManifest(m *manifest, etag string, fromStore bool) {
	if !db.replaceManifest(m, etag, fromStore) {
		return
	}
	if db.disk != nil {
		if b, err := encodeManifest(db.manifestKey(), m); err == nil {
			cached, _ := json.Marshal(diskManifest{ETag: etag, Body: b})
			db.disk.Put(db.diskManifestKey(), cached)
		}
	}
}

// replaceManifest sets the cached manifest under its lock, unless a local write produced m and
// the cached one is newer, and reports whether it did.
func (db *DB) replaceManifest(m *manifest, etag string, fromStore bool) bool {
	db.manifestMu.Lock()
	defer db.manifestMu.Unlock()
	if !fromStore && db.manifest != nil && db.manifest.Seq > m.Seq {
		return false
	}
	db.manifest = m
	db.etag = etag
	return true
}

// diskManifestKey of the disk cache's record of the last seen manifest. It names the version of
// the manifest, so that the record of an earlier version is never read.
func (db *DB) diskManifestKey() string {
	return fmt.Sprintf("%s@v%d", db.manifestKey(), manifestVersion)
}

// diskManifest is the disk cache's record of the last seen manifest, with the bytes of its object.
type diskManifest struct {
	ETag string `json:"etag"`
	Body []byte `json:"body"`
}

func (db *DB) loadManifestFromDisk() {
	if db.disk == nil {
		return
	}
	b, ok := db.disk.Get(db.diskManifestKey())
	if !ok {
		return
	}
	var dm diskManifest
	err := json.Unmarshal(b, &dm)
	var m *manifest
	if err == nil {
		m, err = decodeManifest(db.manifestKey(), dm.Body)
	}
	if err != nil {
		db.cacheRefused(&db.diskRefused, "disk", db.diskManifestKey(), err)
		return
	}
	db.replaceManifest(m, dm.ETag, true)
}

// refreshManifest asks the store whether the cached manifest is still current, with a conditional
// read when one is cached, and returns the current manifest.
func (db *DB) refreshManifest(ctx context.Context) (*manifest, string, error) {
	cached, etag := db.cachedManifest()
	res, err := db.client.Get(ctx, db.manifestKey(), s3.GetOptions{IfNoneMatch: etag, Consistent: true})
	if err != nil {
		if errors.Is(err, s3.ErrNotModified) && cached != nil {
			return cached, etag, nil
		}
		return nil, "", err
	}
	m, err := decodeManifest(db.manifestKey(), res.Body)
	if err != nil {
		if errors.Is(err, ErrCorrupt) {
			db.corruptRead(ctx, err)
		}
		return nil, "", err
	}
	db.setManifest(m, res.ETag, true)
	return m, res.ETag, nil
}

// currentManifest for a new transaction: the cached one when fresh, or the store's.
func (db *DB) currentManifest(ctx context.Context) (*manifest, string, error) {
	if db.mutations.SkipFreshnessCheck {
		if m, etag := db.cachedManifest(); m != nil {
			return m, etag, nil
		}
	}
	return db.refreshManifest(ctx)
}

func (db *DB) createManifest(ctx context.Context) error {
	m := &manifest{Version: manifestVersion, UpdatedAt: db.now()}
	b, err := encodeManifest(db.manifestKey(), m)
	if err != nil {
		return err
	}
	etag, err := db.client.Put(ctx, db.manifestKey(), b, s3.PutOptions{IfNoneMatch: true})
	if err != nil {
		if errors.Is(err, s3.ErrPreconditionFailed) {
			_, _, err = db.refreshManifest(ctx)
			return err
		}
		return fmt.Errorf("engine: creating manifest: %w", err)
	}
	assert.Reachable("a database is created", nil)
	db.log.Info("created database", "prefix", db.prefix)
	db.setManifest(m, etag, true)
	return nil
}

// casManifest replaces the manifest at base with next by a conditional write and returns the new ETag.
//
// A write whose answer is lost is resolved by reading the manifest back: the write landed when
// the record at next.Seq carries next.CommitID, it lost when another commit took that sequence
// number, and it is retried with the same body while the manifest is unchanged. The same
// commit ID is reused on every attempt, so a commit can never be applied twice.
func (db *DB) casManifest(ctx context.Context, baseEtag string, next *manifest) (string, error) {
	body, err := encodeManifest(db.manifestKey(), next)
	if err != nil {
		return "", err
	}
	opts := s3.PutOptions{IfMatch: baseEtag}
	if db.mutations.CommitWithoutCAS {
		opts = s3.PutOptions{}
	}

	const maxAttempts = 8
	var lastErr error
	// ambiguous is true once an attempt's answer was lost, so that how it is resolved is known.
	ambiguous := false
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("%w: %w", ErrCommitUnknown, err)
		}
		etag, err := db.client.Put(ctx, db.manifestKey(), body, opts)
		if db.mutations.AckBeforeConfirm {
			if err != nil {
				etag = `"unconfirmed"`
			}
			db.setManifest(next, etag, false)
			return etag, nil
		}
		if err == nil {
			db.setManifest(next, etag, false)
			return etag, nil
		}
		lastErr = err
		definite := errors.Is(err, s3.ErrPreconditionFailed)
		assert.Sometimes(definite, "a manifest write fails its precondition", nil)
		assert.Sometimes(!definite, "a manifest write has an unknown outcome", nil)
		ambiguous = ambiguous || !definite

		cur, curEtag, rerr := db.readManifest(ctx)
		if rerr != nil {
			lastErr = rerr
			continue
		}
		switch {
		case cur.Seq == next.Seq && cur.CommitID == next.CommitID:
			assert.Sometimes(true, "a lost manifest write is found to have landed", nil)
			reach.Hit(ctx, reach.LostAckLanded)
			db.setManifest(cur, curEtag, true)
			return curEtag, nil
		case curEtag == baseEtag && !definite:
			continue
		case cur.Seq >= next.Seq:
			rec, covered := cur.commitAt(next.Seq)
			if !covered {
				return "", fmt.Errorf("%w: %d commits happened meanwhile", ErrCommitUnknown, cur.Seq-next.Seq)
			}
			if rec.ID == next.CommitID {
				assert.Sometimes(true, "a lost manifest write is found to have landed", nil)
				reach.Hit(ctx, reach.LostAckLanded)
				db.setManifest(cur, curEtag, true)
				return curEtag, nil
			}
			db.setManifest(cur, curEtag, true)
			assert.Sometimes(true, "a commit loses to a concurrent commit", nil)
			if ambiguous {
				assert.Sometimes(true, "a lost manifest write is found to have lost to a concurrent commit", nil)
				reach.Hit(ctx, reach.LostAckConflict)
			}
			return "", fmt.Errorf("%w: commit %d was taken by %s", ErrConflict, next.Seq, rec.ID)
		default:
			return "", fmt.Errorf("%w: manifest at %d after writing %d", ErrCommitUnknown, cur.Seq, next.Seq)
		}
	}
	return "", fmt.Errorf("%w: %w", ErrCommitUnknown, lastErr)
}

// readManifest unconditionally and consistently.
func (db *DB) readManifest(ctx context.Context) (*manifest, string, error) {
	res, err := db.client.Get(ctx, db.manifestKey(), s3.GetOptions{Consistent: true})
	if err != nil {
		return nil, "", err
	}
	m, err := decodeManifest(db.manifestKey(), res.Body)
	if err != nil {
		if errors.Is(err, ErrCorrupt) {
			db.corruptRead(ctx, err)
		}
		return nil, "", err
	}
	return m, res.ETag, nil
}

// putObject writes an immutable object, retrying on errors. The body is identical on every
// attempt, so a lost acknowledgement is harmless.
func (db *DB) putObject(ctx context.Context, key string, body []byte) error {
	var err error
	for range 4 {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("engine: writing %q: %w", key, err)
		}
		_, err = db.client.Put(ctx, key, body, s3.PutOptions{})
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("engine: writing %q: %w", key, err)
}
