package bench

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"maragu.dev/maragudb/internal/engine"
	"maragu.dev/maragudb/internal/s3"
)

// The dataset is one table in the style of YCSB's usertable, sized for a small application.
const (
	// Rows in usertable after [Load].
	Rows = 100_000
	// payloadBytes in the payload column of each row, which make a row about 130 bytes.
	payloadBytes = 100
	// loadBatch rows per INSERT of [Load].
	loadBatch = 5_000
	// smallCacheBytes is the memory cache of the read workloads, about a quarter of the
	// database, so that reads also miss it.
	smallCacheBytes int64 = 4 << 20
	// zipfSkew of the choice of keys. YCSB uses 0.99, but the generator needs more than 1.
	zipfSkew = 1.1
)

const (
	readQuery  = `SELECT name, score, payload FROM usertable WHERE id = $1`
	scanQuery  = `SELECT id, score FROM usertable WHERE id >= $1 ORDER BY id LIMIT 100`
	updateStmt = `UPDATE usertable SET score = score + 1 WHERE id = $1`
	insertStmt = `INSERT INTO usertable VALUES ($1, $2, 0, $3)`
)

// Result of a workload: what its measured operations cost together.
type Result struct {
	Workload string
	// Ops is the number of measured operations: statements, transactions, or opens.
	Ops int
	// Requests to the store by kind.
	Requests s3.Counts
	// BytesRead are the bytes of the bodies that GETs returned, and BytesWritten those of PUTs,
	// of which ManifestBytesWritten went to the manifest.
	BytesRead, BytesWritten, ManifestBytesWritten int64
	// Conflicts are the conditional PUTs of the manifest that failed their precondition: commits
	// that lost to a concurrent commit, including those of autocommit statements that the driver
	// retried, and compactions that lost to a commit.
	Conflicts int64
	// Retries are the operations that a client ran again after a conflict.
	Retries int64
	// Cache hits and misses of the memory and the disk cache.
	MemoryCacheHits, MemoryCacheMisses, DiskCacheHits, DiskCacheMisses int64
	// CommittedBytes are the bytes of the segments of the commits that landed, and
	// CompactionBytesWritten those of the segments that compactions wrote.
	CommittedBytes, CompactionBytesWritten int64
	// Segments of the database, as the first process of the workload last saw them.
	Segments int
	// Latencies of the operations, in the order they ended.
	Latencies []time.Duration
	// Elapsed time of the measured operations.
	Elapsed time.Duration
}

// Workload of the benchmark.
type Workload struct {
	// Name of the workload, which identifies it in the baseline.
	Name string
	// Summary of what the workload does.
	Summary string
	run     func(ctx context.Context, e *Env) (Result, error)
}

// Workloads of the benchmark.
func Workloads() []Workload {
	return []Workload{
		{
			Name:    "point-reads",
			Summary: "One client reads 500 rows by primary key, chosen by a Zipf distribution, after 500 such reads warmed its memory cache of 4 MiB.",
			run:     pointReads,
		},
		{
			Name:    "point-writes",
			Summary: "One client updates 200 rows by primary key, chosen by a Zipf distribution, with one autocommit UPDATE each, and no maintenance.",
			run:     pointWrites,
		},
		{
			Name:    "range-scans",
			Summary: "One client reads 100 rows in key order from a start key chosen by a Zipf distribution, 100 times, after 50 such scans warmed its memory cache of 4 MiB.",
			run:     rangeScans,
		},
		{
			Name:    "read-write-mix",
			Summary: "One client runs 1,000 statements by primary key, 95% reads and 5% autocommit updates, after 500 reads warmed its memory cache of 4 MiB.",
			run:     readWriteMix,
		},
		{
			Name:    "contending-writers",
			Summary: "Four clients commit 50 writes each, alternately an autocommit UPDATE and a transaction that reads a row and writes it back, which the client runs again after a conflict, with up to 20 ms between writes.",
			run:     contendingWriters,
		},
		{
			Name:    "cold-open",
			Summary: "A new client with empty caches opens the database and reads one row by primary key, 10 times; an operation is the open and the read.",
			run:     coldOpen,
		},
		{
			Name:    "disk-cache-restart",
			Summary: "A client with a disk cache and a memory cache of 4 MiB reads 500 rows, closes, and a new client on the same disk cache reads 200 rows, which are measured.",
			run:     diskCacheRestart,
		},
		{
			Name:    "writes-with-maintenance",
			Summary: "One client runs 400 autocommit writes, two updates for every insert, and a maintenance step every 10 seconds, with the lease, as the driver's background maintenance does; the requests of maintenance count too.",
			run:     writesWithMaintenance,
		},
	}
}

// Load the dataset into the database of e: usertable with [Rows] rows, inserted in batches with
// maintenance after each, as it would run in the background, until no compaction is due.
func Load(e *Env) error {
	return e.run(func(ctx context.Context) error {
		p, err := e.open(ctx, "load", processOptions{})
		if err != nil {
			return err
		}
		defer func() { _ = p.close() }()
		if _, err := p.db.ExecContext(ctx, `CREATE TABLE usertable (id bigint PRIMARY KEY, name text NOT NULL, score bigint NOT NULL, payload text NOT NULL)`); err != nil {
			return err
		}
		for i := 0; i < Rows; i += loadBatch {
			var b strings.Builder
			b.WriteString("INSERT INTO usertable VALUES ")
			for id := i; id < min(i+loadBatch, Rows); id++ {
				if id > i {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "(%d, '%s', %d, '%s')", id, name(int64(id)), id%1000, payload(int64(id)))
			}
			if _, err := p.db.ExecContext(ctx, b.String()); err != nil {
				return err
			}
			if err := p.maintain(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// Run the workload in e, whose database [Load] loaded. It first maintains the database until no
// compaction is due, without measuring, so that the workloads that run one after another on one
// database each start from a maintained one.
func Run(e *Env, w Workload) (Result, error) {
	var res Result
	err := e.run(func(ctx context.Context) error {
		p, err := e.open(ctx, "settle", processOptions{memoryCacheBytes: new(int64)})
		if err != nil {
			return err
		}
		err = p.maintain(ctx)
		_ = p.close()
		if err != nil {
			return err
		}
		res, err = w.run(ctx, e)
		res.Workload = w.Name
		return err
	})
	return res, err
}

// name of the row with the given id.
func name(id int64) string {
	return fmt.Sprint("user", id)
}

// payload of the row with the given id: letters and digits that depend on it.
func payload(id int64) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewPCG(uint64(id), 0x9a7))
	b := make([]byte, payloadBytes)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

// keys draws row ids by a Zipf distribution whose ranks are scattered over the ids, as YCSB
// scrambles them, so that the hot rows are not neighbors.
type keys struct {
	zipf *rand.Zipf
	rng  *rand.Rand
}

func (e *Env) keys(stream uint64) *keys {
	rng := rand.New(rand.NewPCG(e.Seed, stream))
	return &keys{zipf: rand.NewZipf(rng, zipfSkew, 1, Rows-1), rng: rng}
}

// next row id.
func (k *keys) next() int64 {
	// The multiplier has no factor in common with Rows, so the ranks map to distinct ids.
	return int64(k.zipf.Uint64() * 2654435761 % Rows)
}

// measurement of the operations of a workload.
type measurement struct {
	env   *Env
	procs []*process
	start time.Time

	mu        sync.Mutex
	latencies []time.Duration
	retries   int64
}

// measure the operations that follow, and the requests of procs from now on.
func (e *Env) measure(procs ...*process) *measurement {
	for _, p := range procs {
		p.mark()
	}
	return &measurement{env: e, procs: procs, start: e.now()}
}

// add a process that opened since measuring began, all of whose requests count.
func (m *measurement) add(p *process) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.procs = append(m.procs, p)
}

// op runs one measured operation and records its latency.
func (m *measurement) op(f func() error) error {
	start := m.env.now()
	if err := f(); err != nil {
		return err
	}
	d := m.env.now().Sub(start)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.latencies = append(m.latencies, d)
	return nil
}

// retried records an operation that runs again after a conflict.
func (m *measurement) retried() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries++
}

// stop measuring, and return the result of the measured operations.
func (m *measurement) stop() Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := Result{Ops: len(m.latencies), Retries: m.retries, Latencies: m.latencies, Elapsed: m.env.now().Sub(m.start)}
	var c counts
	for i, p := range m.procs {
		c = c.add(p.meter.snapshot().sub(p.markedCounts))
		s := p.edb.Stats()
		res.MemoryCacheHits += s.MemoryCacheHits - p.markedStats.MemoryCacheHits
		res.MemoryCacheMisses += s.MemoryCacheMisses - p.markedStats.MemoryCacheMisses
		res.DiskCacheHits += s.DiskCacheHits - p.markedStats.DiskCacheHits
		res.DiskCacheMisses += s.DiskCacheMisses - p.markedStats.DiskCacheMisses
		res.CommittedBytes += s.CommittedBytes - p.markedStats.CommittedBytes
		res.CompactionBytesWritten += s.CompactionBytesWritten - p.markedStats.CompactionBytesWritten
		if i == 0 {
			res.Segments = s.Segments
		}
	}
	res.Requests = c.requests
	res.BytesRead, res.BytesWritten, res.ManifestBytesWritten = c.bytesRead, c.bytesWritten, c.manifestBytesWritten
	res.Conflicts = c.preconditionFailed
	return res
}

// read the row with the given id.
func read(ctx context.Context, db *sql.DB, id int64) error {
	var n string
	var score int64
	var p string
	if err := db.QueryRowContext(ctx, readQuery, id).Scan(&n, &score, &p); err != nil {
		return fmt.Errorf("reading row %d: %w", id, err)
	}
	return nil
}

// scan 100 rows from the given id on.
func scan(ctx context.Context, db *sql.DB, from int64) error {
	rows, err := db.QueryContext(ctx, scanQuery, from)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id, score int64
		if err := rows.Scan(&id, &score); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 100 {
		return fmt.Errorf("scan from %d: %d rows, not 100", from, n)
	}
	return nil
}

// update the row with the given id, which must exist, in an autocommit statement.
func update(ctx context.Context, db *sql.DB, id int64) error {
	res, err := db.ExecContext(ctx, updateStmt, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("updating row %d: %d rows affected, %v", id, n, err)
	}
	return nil
}

func pointReads(ctx context.Context, e *Env) (Result, error) {
	p, err := e.open(ctx, "reader", processOptions{memoryCacheBytes: new(smallCacheBytes)})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = p.close() }()
	k := e.keys(1)
	for range 500 {
		if err := read(ctx, p.db, k.next()); err != nil {
			return Result{}, err
		}
	}
	m := e.measure(p)
	for range 500 {
		if err := m.op(func() error { return read(ctx, p.db, k.next()) }); err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

func pointWrites(ctx context.Context, e *Env) (Result, error) {
	p, err := e.open(ctx, "writer", processOptions{})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = p.close() }()
	k := e.keys(2)
	m := e.measure(p)
	for range 200 {
		if err := m.op(func() error { return update(ctx, p.db, k.next()) }); err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

func rangeScans(ctx context.Context, e *Env) (Result, error) {
	p, err := e.open(ctx, "scanner", processOptions{memoryCacheBytes: new(smallCacheBytes)})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = p.close() }()
	k := e.keys(3)
	from := func() int64 { return k.next() % (Rows - 100) }
	for range 50 {
		if err := scan(ctx, p.db, from()); err != nil {
			return Result{}, err
		}
	}
	m := e.measure(p)
	for range 100 {
		if err := m.op(func() error { return scan(ctx, p.db, from()) }); err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

func readWriteMix(ctx context.Context, e *Env) (Result, error) {
	p, err := e.open(ctx, "client", processOptions{memoryCacheBytes: new(smallCacheBytes)})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = p.close() }()
	k := e.keys(4)
	for range 500 {
		if err := read(ctx, p.db, k.next()); err != nil {
			return Result{}, err
		}
	}
	m := e.measure(p)
	for range 1000 {
		id := k.next()
		write := k.rng.IntN(100) < 5
		if err := m.op(func() error {
			if write {
				return update(ctx, p.db, id)
			}
			return read(ctx, p.db, id)
		}); err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

// readModifyWrite reads the score of a row and writes it back plus one, in a transaction.
func readModifyWrite(ctx context.Context, db *sql.DB, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var score int64
	if err := tx.QueryRowContext(ctx, `SELECT score FROM usertable WHERE id = $1`, id).Scan(&score); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usertable SET score = $1 WHERE id = $2`, score+1, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func contendingWriters(ctx context.Context, e *Env) (Result, error) {
	const writers, writes = 4, 50
	procs := make([]*process, writers)
	for i := range procs {
		p, err := e.open(ctx, fmt.Sprint("writer-", i), processOptions{})
		if err != nil {
			return Result{}, err
		}
		defer func() { _ = p.close() }()
		procs[i] = p
	}
	m := e.measure(procs...)
	g := &group{env: e}
	for i, p := range procs {
		k := e.keys(uint64(10 + i))
		g.goTask(ctx, p.name, func(ctx context.Context) error {
			for j := range writes {
				e.sleep(ctx, time.Duration(k.rng.Int64N(int64(20*time.Millisecond))))
				id := k.next()
				err := m.op(func() error {
					for attempt := 1; ; attempt++ {
						var err error
						if j%2 == 0 {
							err = update(ctx, p.db, id)
						} else {
							err = readModifyWrite(ctx, p.db, id)
						}
						if !errors.Is(err, engine.ErrConflict) {
							return err
						}
						// A write that loses to the others this often is starved, which is a bug.
						if attempt == 100 {
							return fmt.Errorf("writing row %d: %d conflicts in a row: %w", id, attempt, err)
						}
						m.retried()
					}
				})
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := g.wait(ctx); err != nil {
		return Result{}, err
	}
	return m.stop(), nil
}

func coldOpen(ctx context.Context, e *Env) (Result, error) {
	m := e.measure()
	k := e.keys(5)
	for i := range 10 {
		var p *process
		err := m.op(func() error {
			var err error
			if p, err = e.open(ctx, fmt.Sprint("cold-", i), processOptions{}); err != nil {
				return err
			}
			return read(ctx, p.db, k.next())
		})
		if p != nil {
			m.add(p)
			_ = p.close()
		}
		if err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

func diskCacheRestart(ctx context.Context, e *Env) (Result, error) {
	dir := e.TempDir()
	opts := processOptions{memoryCacheBytes: new(smallCacheBytes), diskCacheDir: dir}
	k := e.keys(6)
	first, err := e.open(ctx, "before-restart", opts)
	if err != nil {
		return Result{}, err
	}
	for range 500 {
		if err := read(ctx, first.db, k.next()); err != nil {
			_ = first.close()
			return Result{}, err
		}
	}
	if err := first.close(); err != nil {
		return Result{}, err
	}
	p, err := e.open(ctx, "after-restart", opts)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = p.close() }()
	m := e.measure(p)
	for range 200 {
		if err := m.op(func() error { return read(ctx, p.db, k.next()) }); err != nil {
			return Result{}, err
		}
	}
	return m.stop(), nil
}

func writesWithMaintenance(ctx context.Context, e *Env) (Result, error) {
	writer, err := e.open(ctx, "writer", processOptions{})
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = writer.close() }()
	m := e.measure(writer)
	k := e.keys(7)
	// done closes when the writer is done, which ends the maintainer's wait in real time.
	done := make(chan struct{})
	var mt *engine.Maintainer
	g := &group{env: e}
	g.goTask(ctx, "writer", func(ctx context.Context) error {
		defer close(done)
		for i := range 400 {
			id := k.next()
			err := m.op(func() error {
				if i%3 == 2 {
					id := int64(Rows + i)
					_, err := writer.db.ExecContext(ctx, insertStmt, id, name(id), payload(id))
					return err
				}
				return update(ctx, writer.db, id)
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	// The maintainer works on the manifest that the writer's commits keep fresh, as the driver's
	// background maintenance does in every process.
	g.goTask(ctx, "maintainer", func(ctx context.Context) error {
		mt = writer.edb.NewMaintainer(engine.MaintenanceOptions{Holder: "bench-maintainer"})
		for {
			e.sleepUnless(ctx, 10*time.Second, done)
			select {
			case <-done:
				return nil
			default:
			}
			// As in the driver's loop, a step that yields to other work is no failure.
			if _, err := mt.Step(ctx); err != nil && !errors.Is(err, engine.ErrConflict) && !errors.Is(err, engine.ErrLeaseLost) {
				return err
			}
		}
	})
	if err := g.wait(ctx); err != nil {
		return Result{}, err
	}
	res := m.stop()
	// The lease is released after measuring, as closing a database releases it, so that later
	// workloads on the same database do not wait for it.
	if mt != nil {
		if err := mt.Release(ctx); err != nil {
			return Result{}, err
		}
	}
	return res, nil
}
