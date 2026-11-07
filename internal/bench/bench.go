// Package bench runs benchmark workloads against a database in an object store, through the
// database/sql driver, and measures what each operation costs: the requests to the store by kind,
// the bytes they move, the hit rates of the caches, the conflicts of commits, and latencies.
//
// A workload runs in an [Env]. With a [sched.Scheduler], the processes of a workload take turns
// in virtual time and each request to the store takes a fixed virtual latency, so that every run
// makes the same requests in the same order, also with several clients. Without one, the
// processes run in real time, as goroutines, against whatever store the Env has.
package bench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/sched"
	"maragu.dev/maragudb/internal/sqldriver"
)

// Env is where workloads run.
type Env struct {
	// Store holds the database. Required.
	Store s3.Client
	// Prefix of the database in Store. Required.
	Prefix string
	// Sched runs the processes of a workload one at a time in virtual time, with every request to
	// Store taking the virtual time that [VirtualLatency] gives, and seeds the identifiers that
	// the engine draws, so that a run is deterministic. Nil runs the processes in real time.
	Sched *sched.Scheduler
	// TempDir returns a new empty directory, for the disk caches of the workloads that have one.
	// Required for those workloads.
	TempDir func() string
	// Seed of the random choices of the workloads.
	Seed uint64
}

// VirtualLatency of a request of the given kind in an [Env] with a scheduler. The values are
// round numbers near what an S3-compatible store takes from a nearby machine. They only decide
// how the processes of a workload interleave, and so how often commits conflict.
func VirtualLatency(kind s3.Kind) time.Duration {
	switch kind {
	case s3.KindGetConditional:
		return 15 * time.Millisecond
	case s3.KindGet, s3.KindDelete:
		return 20 * time.Millisecond
	case s3.KindList:
		return 30 * time.Millisecond
	case s3.KindPut:
		return 40 * time.Millisecond
	case s3.KindPutConditional:
		return 50 * time.Millisecond
	}
	return 20 * time.Millisecond
}

// run f as the main task of the environment and wait for it and every task it starts. A panic
// of a task in virtual time stops the scheduler with the other tasks parked for good, and is
// returned as an error.
func (e *Env) run(f func(ctx context.Context) error) (err error) {
	if e.Sched == nil {
		return f(context.Background())
	}
	defer func() {
		if p := recover(); p != nil {
			tp, ok := p.(*sched.TaskPanic)
			if !ok {
				panic(p)
			}
			err = fmt.Errorf("bench: task %s panicked: %v\n%s", tp.Task, tp.Value, tp.Stack)
		}
	}()
	e.Sched.Go("main", func(ctx context.Context) {
		err = f(ctx)
	})
	e.Sched.Run()
	return err
}

// now on the clock of the environment.
func (e *Env) now() time.Time {
	if e.Sched == nil {
		return time.Now()
	}
	return e.Sched.Now()
}

// sleep for d on the clock of the environment, or until ctx ends.
func (e *Env) sleep(ctx context.Context, d time.Duration) {
	e.sleepUnless(ctx, d, nil)
}

// sleepUnless sleeps for d on the clock of the environment, or until ctx ends, and in real time
// also until stop closes. In virtual time, the sleep always lasts d, because a task wakes only
// when its time comes.
func (e *Env) sleepUnless(ctx context.Context, d time.Duration, stop <-chan struct{}) {
	if e.Sched != nil {
		e.Sched.Sleep(ctx, d)
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-stop:
	case <-t.C:
	}
}

// group of concurrent tasks.
type group struct {
	env  *Env
	wg   sync.WaitGroup
	left atomic.Int64
	mu   sync.Mutex
	err  error
}

// goTask runs f concurrently with the caller, as a task of the scheduler or as a goroutine.
func (g *group) goTask(ctx context.Context, name string, f func(ctx context.Context) error) {
	g.left.Add(1)
	done := func(err error) {
		if err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = fmt.Errorf("%s: %w", name, err)
			}
			g.mu.Unlock()
		}
		g.left.Add(-1)
	}
	if g.env.Sched == nil {
		g.wg.Go(func() { done(f(ctx)) })
		return
	}
	g.env.Sched.Go(name, func(ctx context.Context) { done(f(ctx)) })
}

// wait for every task of the group, and return the first error of one.
func (g *group) wait(ctx context.Context) error {
	if g.env.Sched == nil {
		g.wg.Wait()
	} else {
		// A task cannot block without sleeping, so it polls in virtual time. The polls make no
		// requests, so they do not change the order of the other tasks' requests.
		for g.left.Load() > 0 {
			g.env.Sched.Sleep(ctx, 10*time.Millisecond)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}

// meter is a [s3.Client] that counts the requests of a process and the bytes they move, and
// that makes each request take virtual time in an environment with a scheduler.
type meter struct {
	s3.Client
	sched *sched.Scheduler
	// manifest is the key of the manifest, whose conditional writes are commits.
	manifest string

	mu     sync.Mutex
	counts counts
}

// counts of the requests that went through a [meter].
type counts struct {
	requests s3.Counts
	// bytesRead are the bytes of the bodies of GETs, and bytesWritten those of PUTs.
	bytesRead, bytesWritten int64
	// manifestBytesWritten are the bytes of the PUTs of the manifest.
	manifestBytesWritten int64
	// preconditionFailed counts the conditional PUTs of the manifest that failed their
	// precondition.
	preconditionFailed int64
}

func (c counts) sub(o counts) counts {
	return counts{
		requests:             c.requests.Sub(o.requests),
		bytesRead:            c.bytesRead - o.bytesRead,
		bytesWritten:         c.bytesWritten - o.bytesWritten,
		manifestBytesWritten: c.manifestBytesWritten - o.manifestBytesWritten,
		preconditionFailed:   c.preconditionFailed - o.preconditionFailed,
	}
}

func (c counts) add(o counts) counts {
	return counts{
		requests:             c.requests.Add(o.requests),
		bytesRead:            c.bytesRead + o.bytesRead,
		bytesWritten:         c.bytesWritten + o.bytesWritten,
		manifestBytesWritten: c.manifestBytesWritten + o.manifestBytesWritten,
		preconditionFailed:   c.preconditionFailed + o.preconditionFailed,
	}
}

func (m *meter) record(kind s3.Kind, c counts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var one s3.Counter
	one.Record(kind)
	c.requests = one.Counts()
	m.counts = m.counts.add(c)
}

func (m *meter) snapshot() counts {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts
}

func (m *meter) wait(ctx context.Context, kind s3.Kind) {
	if m.sched != nil {
		m.sched.Sleep(ctx, VirtualLatency(kind))
	}
}

func (m *meter) Get(ctx context.Context, key string, opts s3.GetOptions) (s3.Object, error) {
	kind := s3.GetKind(opts)
	m.wait(ctx, kind)
	obj, err := m.Client.Get(ctx, key, opts)
	m.record(kind, counts{bytesRead: int64(len(obj.Body))})
	return obj, err
}

func (m *meter) Put(ctx context.Context, key string, body []byte, opts s3.PutOptions) (string, error) {
	kind := s3.PutKind(opts)
	m.wait(ctx, kind)
	etag, err := m.Client.Put(ctx, key, body, opts)
	c := counts{bytesWritten: int64(len(body))}
	if key == m.manifest {
		c.manifestBytesWritten = c.bytesWritten
		if errors.Is(err, s3.ErrPreconditionFailed) {
			c.preconditionFailed = 1
		}
	}
	m.record(kind, c)
	return etag, err
}

func (m *meter) Delete(ctx context.Context, key string) error {
	m.wait(ctx, s3.KindDelete)
	err := m.Client.Delete(ctx, key)
	m.record(s3.KindDelete, counts{})
	return err
}

func (m *meter) List(ctx context.Context, prefix string) ([]s3.ObjectInfo, error) {
	m.wait(ctx, s3.KindList)
	infos, err := m.Client.List(ctx, prefix)
	m.record(s3.KindList, counts{})
	return infos, err
}

// process is a client of the database with its own driver, engine, and caches, as a process of
// an application is.
type process struct {
	name  string
	meter *meter
	db    *sql.DB
	edb   *engine.DB
	// marked are the counts and the statistics when measuring began.
	markedCounts counts
	markedStats  engine.Stats
}

// processOptions for [Env.open].
type processOptions struct {
	// memoryCacheBytes bounds the memory cache, which has its default size when nil.
	memoryCacheBytes *int64
	// diskCacheDir holds a disk cache when set.
	diskCacheDir string
}

// open a process named name, which must be unique within the workload. Its driver runs no
// background maintenance; the workloads that need it run a maintainer of their own.
func (e *Env) open(ctx context.Context, name string, opts processOptions) (*process, error) {
	m := &meter{Client: e.Store, sched: e.Sched, manifest: strings.TrimSuffix(e.Prefix, "/") + "/manifest"}
	eopts := engine.Options{Client: m, MemoryCacheBytes: opts.memoryCacheBytes}
	if e.Sched != nil {
		h := fnv.New64a()
		_, _ = h.Write([]byte(name))
		eopts.Now = e.Sched.Now
		eopts.Sleep = e.Sched.Sleep
		eopts.Rand = rand.New(rand.NewPCG(e.Seed, h.Sum64()))
	}
	dopts := sqldriver.Options{Engine: eopts, Maintenance: sqldriver.MaintenanceOptions{Disabled: true}}
	if opts.diskCacheDir != "" {
		dopts.DiskCacheDir = opts.diskCacheDir
		dopts.DiskCacheBytes = 1 << 30
	}
	c, err := sqldriver.NewConnector(sqldriver.New(dopts), "s3://bench/"+e.Prefix)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(c)
	// One connection, so that the process's statements run in the order it makes them.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// The engine opens here, as it would with the first statement, which reads the manifest.
	edb, err := c.DB(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &process{name: name, meter: m, db: db, edb: edb}, nil
}

// close the process.
func (p *process) close() error {
	return p.db.Close()
}

// mark the start of measuring.
func (p *process) mark() {
	p.markedCounts = p.meter.snapshot()
	p.markedStats = p.edb.Stats()
}

// maintain the database as background maintenance does, without the lease, until no compaction
// is due.
func (p *process) maintain(ctx context.Context) error {
	m := p.edb.NewMaintainer(engine.MaintenanceOptions{DisableLease: true})
	for {
		res, err := m.Step(ctx)
		if err != nil || !res.Compacted {
			return err
		}
	}
}
