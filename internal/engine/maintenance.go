package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"maragu.dev/maragudb/internal/reach"
	"maragu.dev/maragudb/internal/s3"
)

// ErrLeaseLost is returned by maintenance that stopped because it no longer holds the lease:
// another maintainer took it over after it expired without renewal.
var ErrLeaseLost = errors.New("engine: maintenance lease lost")

// MaintenanceOptions for [DB.NewMaintainer].
type MaintenanceOptions struct {
	// Holder identifies the maintainer in the lease. Defaults to a random ID.
	Holder string
	// SegmentThreshold is the segment count at which compaction is due. Defaults to 8, and is
	// at least 2, because one segment cannot be compacted.
	SegmentThreshold int
	// LeaseDuration is how long an acquired lease lasts without renewal. Defaults to one minute.
	// During long work the lease is renewed once half of it has passed.
	LeaseDuration time.Duration
	// DisableLease runs maintenance without the lease, so that every maintainer does the work.
	DisableLease bool
	// IgnoreLeaseExpiry makes the maintainer neither renew its lease nor stop when it expires,
	// as a stalled process would, so that other maintainers take the lease over while this one
	// still believes it holds it, and the leases overlap.
	IgnoreLeaseExpiry bool
}

// Maintainer compacts and garbage collects a database when there is work, coordinated with the
// maintainers of other processes through a lease object in the store. The lease only saves
// work: correctness never depends on it, because compaction is a conditional commit and a
// delete can safely happen twice. It is not safe for concurrent use.
type Maintainer struct {
	db   *DB
	opts MaintenanceOptions
	key  string

	held       bool
	etag       string
	acquiredAt time.Time
}

// lease is the body of the lease object.
type lease struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// NewMaintainer for the database.
func (db *DB) NewMaintainer(opts MaintenanceOptions) *Maintainer {
	if opts.Holder == "" {
		opts.Holder = db.newID()
	}
	if opts.SegmentThreshold <= 0 {
		opts.SegmentThreshold = 8
	}
	opts.SegmentThreshold = max(opts.SegmentThreshold, 2)
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = time.Minute
	}
	return &Maintainer{db: db, opts: opts, key: db.key("maintenance-lease")}
}

// StepResult of one maintenance step.
type StepResult struct {
	// Attempted is true when work was due and the maintainer got to do it, because it holds
	// the lease or runs without one.
	Attempted bool
	// Compacted is true when the step merged segments.
	Compacted bool
	// Deleted is how many objects garbage collection deleted.
	Deleted int
	// Ran is true when the step changed the database: it compacted, or garbage collection
	// stamped or deleted something.
	Ran bool
}

// Step runs maintenance once if there is work: compaction when the manifest has at least
// SegmentThreshold segments, and garbage collection when it has garbage that is unstamped or
// older than the grace period. It decides from the manifest the database already has, so a
// step without work makes no requests. With the lease, the step first acquires it, and gives
// it up again at the end, so that an idle process holds nothing.
func (m *Maintainer) Step(ctx context.Context) (StepResult, error) {
	var res StepResult
	man, _ := m.db.cachedManifest()
	if man == nil || !m.due(man) {
		return res, nil
	}
	if !m.opts.DisableLease && !m.held {
		ok, err := m.Acquire(ctx)
		if err != nil || !ok {
			return res, err
		}
	}
	res.Attempted = true
	err := m.maintain(ctx, &res)
	if res.Ran {
		m.db.maintenanceRuns.Add(1)
	}
	assert.Sometimes(res.Ran, "a maintenance step does work", nil)
	if !m.opts.DisableLease {
		if rerr := m.Release(ctx); rerr != nil && err == nil {
			err = rerr
		}
	}
	return res, err
}

// due reports whether the manifest has work.
func (m *Maintainer) due(man *manifest) bool {
	return len(man.Segments) >= m.opts.SegmentThreshold || m.garbageDue(man)
}

// garbageDue reports whether the manifest has garbage to stamp or to delete.
func (m *Maintainer) garbageDue(man *manifest) bool {
	now := m.db.now()
	for _, g := range man.Garbage {
		if g.UnreferencedAt.IsZero() || now.Sub(g.UnreferencedAt) >= m.db.grace {
			return true
		}
	}
	return false
}

func (m *Maintainer) maintain(ctx context.Context, res *StepResult) error {
	man, _ := m.db.cachedManifest()
	progress := func() error { return m.progress(ctx) }
	if len(man.Segments) >= m.opts.SegmentThreshold {
		merged, err := m.db.compact(ctx, progress)
		if err != nil {
			return err
		}
		res.Compacted = merged
		res.Ran = res.Ran || merged
	}
	if err := progress(); err != nil {
		return err
	}
	// Compaction refreshed the manifest, which may show that another maintainer did the work.
	if man, _ = m.db.cachedManifest(); !res.Compacted && !m.garbageDue(man) {
		return nil
	}
	deleted, changed, err := m.db.gc(ctx, progress)
	res.Deleted += deleted
	res.Ran = res.Ran || changed || deleted > 0
	return err
}

// progress renews the lease during long work once half of its duration has passed. A renewal
// that fails for a passing reason is tried again at the next call, as long as the lease has
// not expired; work stops when ctx ends or the lease is lost.
func (m *Maintainer) progress(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.held || m.opts.IgnoreLeaseExpiry {
		return nil
	}
	age := m.db.now().Sub(m.acquiredAt)
	if age < m.opts.LeaseDuration/2 {
		return nil
	}
	err := m.Renew(ctx)
	if err != nil && !errors.Is(err, ErrLeaseLost) && age < m.opts.LeaseDuration {
		m.db.log.Debug("renewing maintenance lease failed, trying again later", "holder", m.opts.Holder, "error", err)
		return nil
	}
	if err != nil && !errors.Is(err, ErrLeaseLost) {
		m.lost(err)
		return fmt.Errorf("%w: expired after a renewal failed: %w", ErrLeaseLost, err)
	}
	return err
}

// Acquire the lease, or take over an expired one, and report whether it is now held. A lease
// held by another maintainer is left alone. It is a no-op when the lease is already held.
func (m *Maintainer) Acquire(ctx context.Context) (bool, error) {
	if m.held {
		return true, nil
	}
	now := m.db.now()
	body, err := json.Marshal(lease{Holder: m.opts.Holder, ExpiresAt: now.Add(m.opts.LeaseDuration)})
	if err != nil {
		return false, fmt.Errorf("engine: encoding lease: %w", err)
	}
	cur, err := m.db.client.Get(ctx, m.key, s3.GetOptions{Consistent: true})
	var etag string
	// takeover is true when the lease belongs to another holder that let it expire without
	// releasing it.
	takeover := false
	switch {
	case errors.Is(err, s3.ErrNotFound):
		etag, err = m.db.client.Put(ctx, m.key, body, s3.PutOptions{IfNoneMatch: true})
	case err != nil:
		return false, fmt.Errorf("engine: reading lease: %w", err)
	default:
		var l lease
		parsed := json.Unmarshal(cur.Body, &l) == nil
		if parsed && l.Holder != m.opts.Holder && now.Before(l.ExpiresAt) {
			assert.Sometimes(true, "a maintainer finds the lease held by another", nil)
			return false, nil
		}
		takeover = parsed && l.Holder != m.opts.Holder && !l.ExpiresAt.IsZero()
		etag, err = m.db.client.Put(ctx, m.key, body, s3.PutOptions{IfMatch: cur.ETag})
	}
	if err != nil {
		if errors.Is(err, s3.ErrPreconditionFailed) || errors.Is(err, s3.ErrNotFound) {
			// Another maintainer got there first.
			return false, nil
		}
		// The write may have landed and only its answer been lost.
		if l, etag, ok := m.reclaim(ctx); ok {
			m.hold(etag, l)
			return true, nil
		}
		return false, fmt.Errorf("engine: acquiring lease: %w", err)
	}
	if takeover {
		assert.Sometimes(true, "a maintainer takes over an expired lease", nil)
		reach.Hit(ctx, reach.LeaseTakeover)
	}
	m.hold(etag, lease{ExpiresAt: now.Add(m.opts.LeaseDuration)})
	return true, nil
}

// hold records the lease as held under etag, with its expiry.
func (m *Maintainer) hold(etag string, l lease) {
	assert.Sometimes(true, "a maintainer acquires the lease", nil)
	m.held, m.etag, m.acquiredAt = true, etag, l.ExpiresAt.Add(-m.opts.LeaseDuration)
	m.db.leaseAcquisitions.Add(1)
	m.db.log.Debug("acquired maintenance lease", "holder", m.opts.Holder)
}

// reclaim reads the lease back and reports whether it is under this holder's name, which
// means a write whose answer was lost did land, with the lease and its current ETag.
func (m *Maintainer) reclaim(ctx context.Context) (lease, string, bool) {
	cur, err := m.db.client.Get(ctx, m.key, s3.GetOptions{Consistent: true})
	if err != nil {
		return lease{}, "", false
	}
	var l lease
	if err := json.Unmarshal(cur.Body, &l); err != nil || l.Holder != m.opts.Holder {
		return lease{}, "", false
	}
	assert.Sometimes(true, "a maintainer reclaims a lease write whose answer was lost", nil)
	return l, cur.ETag, true
}

// Renew the held lease for another LeaseDuration. It returns [ErrLeaseLost] when another
// maintainer took the lease over, after which it is no longer held.
func (m *Maintainer) Renew(ctx context.Context) error {
	if !m.held {
		return ErrLeaseLost
	}
	now := m.db.now()
	body, err := json.Marshal(lease{Holder: m.opts.Holder, ExpiresAt: now.Add(m.opts.LeaseDuration)})
	if err != nil {
		return fmt.Errorf("engine: encoding lease: %w", err)
	}
	etag, err := m.db.client.Put(ctx, m.key, body, s3.PutOptions{IfMatch: m.etag})
	if err != nil {
		// The precondition fails after another maintainer took over, and also when an earlier
		// renewal of this maintainer's own landed with its answer lost. Reading the lease back
		// tells the two apart.
		if l, curEtag, ok := m.reclaim(ctx); ok && l.ExpiresAt.After(now) {
			m.etag, m.acquiredAt = curEtag, l.ExpiresAt.Add(-m.opts.LeaseDuration)
			return nil
		}
		if m.lost(err) {
			return fmt.Errorf("%w: renewal failed: %w", ErrLeaseLost, err)
		}
		return fmt.Errorf("engine: renewing lease: %w", err)
	}
	m.etag, m.acquiredAt = etag, now
	return nil
}

// Release the lease by marking it expired, so that any maintainer can take it over at once.
// It is a no-op when the lease is not held. After it, the lease is not held whatever happened,
// because an unreleased lease expires on its own.
func (m *Maintainer) Release(ctx context.Context) error {
	if !m.held {
		return nil
	}
	body, err := json.Marshal(lease{Holder: m.opts.Holder})
	if err != nil {
		return fmt.Errorf("engine: encoding lease: %w", err)
	}
	_, err = m.db.client.Put(ctx, m.key, body, s3.PutOptions{IfMatch: m.etag})
	if err != nil {
		// A write of this maintainer's own may have landed with its answer lost: a renewal,
		// after which the release is tried once more, or this release itself.
		l, curEtag, ok := m.reclaim(ctx)
		switch {
		case ok && l.ExpiresAt.After(m.db.now()):
			_, err = m.db.client.Put(ctx, m.key, body, s3.PutOptions{IfMatch: curEtag})
		case ok:
			err = nil
		case errors.Is(err, s3.ErrPreconditionFailed), errors.Is(err, s3.ErrNotFound):
			m.lost(err)
			return nil
		}
	}
	m.held, m.etag = false, ""
	if err != nil {
		return fmt.Errorf("engine: releasing lease: %w", err)
	}
	m.db.log.Debug("released maintenance lease", "holder", m.opts.Holder)
	return nil
}

// lost records that the lease is gone when err says another maintainer replaced it, or when
// the lease expired while a renewal kept failing.
func (m *Maintainer) lost(err error) bool {
	if !errors.Is(err, s3.ErrPreconditionFailed) && !errors.Is(err, s3.ErrNotFound) && m.db.now().Sub(m.acquiredAt) < m.opts.LeaseDuration {
		return false
	}
	assert.Sometimes(true, "a maintainer loses the lease to another", nil)
	m.held, m.etag = false, ""
	m.db.leaseLosses.Add(1)
	m.db.log.Debug("lost maintenance lease", "holder", m.opts.Holder)
	return true
}

// DB the maintainer maintains.
func (m *Maintainer) DB() *DB {
	return m.db
}

// Holds reports whether the maintainer believes it holds the lease.
func (m *Maintainer) Holds() bool {
	return m.held
}

// Close releases the lease if it is held.
func (m *Maintainer) Close(ctx context.Context) error {
	return m.Release(ctx)
}
