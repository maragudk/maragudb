// Package s3fake is an in-process fake of an S3-compatible object store.
//
// It implements conditional writes faithfully, counts requests by kind, and can inject faults
// driven by a seeded random source: errors, latency in virtual time, lost acknowledgements
// (the write is applied but the response is an error), client crashes at any step, and outages
// and partitions that make the store unreachable for a stretch of virtual time while the clients
// keep running.
package s3fake

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"maragu.dev/maragudb/internal/s3"
)

var (
	// ErrInjected is an injected request failure. The request may or may not have been applied.
	ErrInjected = errors.New("s3fake: injected error")
	// ErrCrashed is returned to a process that has crashed. Nothing is applied after a crash.
	ErrCrashed = errors.New("s3fake: process crashed")
	// ErrUnavailable is returned during an outage or a partition. A write that timed out may or
	// may not have been applied.
	ErrUnavailable = errors.New("s3fake: store unavailable")
)

// Clock in which latency is simulated.
type Clock interface {
	Now() time.Time
	// Sleep for d. It may yield to other tasks.
	Sleep(ctx context.Context, d time.Duration)
}

// VirtualClock advances only when slept. It is the default clock.
type VirtualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewVirtualClock starting at t.
func NewVirtualClock(t time.Time) *VirtualClock {
	return &VirtualClock{now: t}
}

func (c *VirtualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *VirtualClock) Sleep(_ context.Context, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Outcome of a request chosen by a [Faults.Decide] script.
type Outcome int

// Outcomes of a request.
const (
	// OutcomeNormal applies the request and returns its result.
	OutcomeNormal Outcome = iota
	// OutcomeError fails the request before it is applied.
	OutcomeError
	// OutcomeLostAck applies the request but fails the response.
	OutcomeLostAck
	// OutcomeCrashBefore crashes the process before the request is applied.
	OutcomeCrashBefore
	// OutcomeCrashAfter crashes the process after the request is applied.
	OutcomeCrashAfter
)

// Faults to inject. Rates are probabilities in [0, 1] per request.
type Faults struct {
	// Rand drives all random fault decisions. Required when any rate is non-zero.
	Rand *rand.Rand
	// Decide scripts the outcome of each request instead of the error, lost acknowledgement, and
	// crash rates when non-nil. Latency still follows MaxLatency, and outages OutageRate.
	Decide func(kind s3.Kind, key string) Outcome
	// ErrorRate is the probability that a request fails before it is applied.
	ErrorRate float64
	// LostAckRate is the probability that a write is applied but the response is an error.
	LostAckRate float64
	// CrashRate is the probability that the calling process crashes at a request,
	// before or after the request is applied with equal probability.
	// Crashes only affect requests whose context carries a [Process].
	CrashRate float64
	// MaxLatency bounds the uniformly drawn virtual latency added before and after a request.
	MaxLatency time.Duration
	// OutageRate is the probability that a request starts an outage at the moment it is made,
	// which fails that request and the ones after it for up to MaxOutage of virtual time. Two in
	// three are partitions of the calling [Process], and the rest, as well as those started by a
	// request without a process, make the store unreachable for everyone. See [Client.Unavailable].
	OutageRate float64
	// MaxOutage bounds the uniformly drawn length of an outage started by OutageRate. Required
	// when OutageRate is non-zero.
	MaxOutage time.Duration
	// OutageTimeout is how long a request that times out during an outage takes, in virtual
	// time. Zero makes every request in an outage fail at once.
	OutageTimeout time.Duration
}

// Process is a simulated client process that can crash.
type Process struct {
	Name    string
	mu      sync.Mutex
	crashed bool
	step    string
}

// Crashed reports whether the process has crashed, and at which request.
func (p *Process) Crashed() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.crashed, p.step
}

// Crash the process. Every later request from it fails with [ErrCrashed].
func (p *Process) Crash(step string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.crashed = true
	p.step = step
}

type processKey struct{}

// WithProcess returns a context whose requests belong to p.
func WithProcess(ctx context.Context, p *Process) context.Context {
	return context.WithValue(ctx, processKey{}, p)
}

func processFrom(ctx context.Context) *Process {
	p, _ := ctx.Value(processKey{}).(*Process)
	return p
}

type object struct {
	body     []byte
	etag     string
	modified time.Time
}

// store holds the objects, shared between a client and its fault-free views.
type store struct {
	mu      sync.Mutex
	objects map[string]object
}

// Client is a fake object store. It implements [s3.Client].
type Client struct {
	clock   Clock
	faults  Faults
	randMu  sync.Mutex
	store   *store
	counter s3.Counter

	outageMu sync.Mutex
	// outages by process name, where the empty name is every request, with their end times.
	outages map[string]time.Time
	// started counts the outages started by [Faults.OutageRate].
	started int
	// Log receives one line per request when non-nil.
	Log func(string)
}

// Option for [New].
type Option func(*Client)

// WithClock sets the clock. The default is a [VirtualClock] starting at the Unix epoch.
func WithClock(c Clock) Option {
	return func(f *Client) {
		f.clock = c
	}
}

// WithFaults enables fault injection.
func WithFaults(faults Faults) Option {
	return func(f *Client) {
		f.faults = faults
	}
}

// New fake client.
func New(opts ...Option) *Client {
	c := &Client{
		clock: NewVirtualClock(time.Unix(0, 0).UTC()),
		store: &store{objects: map[string]object{}},
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.faults.Rand == nil && (c.faults.ErrorRate > 0 || c.faults.LostAckRate > 0 || c.faults.CrashRate > 0 || c.faults.MaxLatency > 0 || c.faults.OutageTimeout > 0 || c.faults.OutageRate > 0) {
		panic("s3fake: faults require a Rand")
	}
	if c.faults.OutageRate > 0 && c.faults.MaxOutage <= 0 {
		panic("s3fake: an outage rate requires MaxOutage")
	}
	return c
}

// Unavailable makes the store unreachable until the given time on the fake's clock: for the
// requests of every [Process] with the given name, which is a partition, or for every request when
// the name is empty, which is an outage. A process restarted under the same name stays
// partitioned. A later call for the same name replaces the end time.
//
// A request during an outage fails with [ErrUnavailable] and the process keeps running. Without
// [Faults.OutageTimeout] it fails at once and is not applied, like a refused connection. With
// it, half of the requests instead hang for the timeout first, like a client timeout, and a
// write among those reaches the store with probability one half before its answer is lost.
func (c *Client) Unavailable(process string, until time.Time) {
	c.outageMu.Lock()
	defer c.outageMu.Unlock()
	if c.outages == nil {
		c.outages = map[string]time.Time{}
	}
	c.outages[process] = until
}

// Outages started by [Faults.OutageRate] so far.
func (c *Client) Outages() int {
	c.outageMu.Lock()
	defer c.outageMu.Unlock()
	return c.started
}

// startOutage draws whether a request of p starts an outage, and starts it.
func (c *Client) startOutage(p *Process, step string) bool {
	f := c.faults
	if f.Rand == nil || f.OutageRate <= 0 || f.MaxOutage <= 0 {
		return false
	}
	c.randMu.Lock()
	start := f.Rand.Float64() < f.OutageRate
	partition := p != nil && f.Rand.IntN(3) != 0 && p.Name != ""
	d := 1 + time.Duration(f.Rand.Int64N(int64(f.MaxOutage)))
	c.randMu.Unlock()
	if !start {
		return false
	}
	name, who := "", "everyone"
	if partition {
		name, who = p.Name, p.Name
	}
	c.Unavailable(name, c.clock.Now().Add(d))
	c.outageMu.Lock()
	c.started++
	c.outageMu.Unlock()
	c.log("%s: store unreachable for %s for %v", step, who, d)
	return true
}

// unavailable reports whether the store is unreachable for a request of p now.
func (c *Client) unavailable(p *Process) bool {
	c.outageMu.Lock()
	defer c.outageMu.Unlock()
	if len(c.outages) == 0 {
		return false
	}
	now := c.clock.Now()
	if until, ok := c.outages[""]; ok && now.Before(until) {
		return true
	}
	if p == nil {
		return false
	}
	until, ok := c.outages[p.Name]
	return ok && now.Before(until)
}

// WithoutFaults returns a client over the same objects and clock that injects no faults.
func (c *Client) WithoutFaults() *Client {
	return &Client{clock: c.clock, store: c.store}
}

// Counts of requests by kind, including requests that failed or whose acknowledgement was lost.
func (c *Client) Counts() s3.Counts {
	return c.counter.Counts()
}

// Keys of all objects, sorted.
func (c *Client) Keys() []string {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	keys := make([]string, 0, len(c.store.objects))
	for k := range c.store.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Body of the object at key, if present.
func (c *Client) Body(key string) ([]byte, bool) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	o, ok := c.store.objects[key]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), o.body...), true
}

// Now in the fake's clock.
func (c *Client) Now() time.Time {
	return c.clock.Now()
}

func etagOf(body []byte) string {
	sum := md5.Sum(body)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// request runs apply with faults around it. write says whether apply mutates state, which
// enables lost acknowledgements.
func (c *Client) request(ctx context.Context, kind s3.Kind, key string, write bool, apply func() error) error {
	c.counter.Record(kind)
	p := processFrom(ctx)
	step := fmt.Sprintf("%v %s", kind, key)
	if p != nil {
		if crashed, _ := p.Crashed(); crashed {
			return ErrCrashed
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	f := c.faults
	if f.Rand != nil && f.MaxLatency > 0 {
		c.clock.Sleep(ctx, c.latency())
		// Another request of the process may have crashed it meanwhile.
		if crashed(p) {
			return ErrCrashed
		}
	}
	if c.unavailable(p) || c.startOutage(p, step) {
		return c.outage(ctx, p, step, write, apply)
	}
	outcome := OutcomeNormal
	switch {
	case f.Decide != nil:
		outcome = f.Decide(kind, key)
	case f.Rand != nil:
		outcome = c.decide(p != nil, write)
	}
	if p == nil && (outcome == OutcomeCrashBefore || outcome == OutcomeCrashAfter) {
		outcome = OutcomeNormal
	}
	if !write && outcome == OutcomeLostAck {
		outcome = OutcomeNormal
	}

	switch outcome {
	case OutcomeError:
		c.log("%s: injected error before apply", step)
		return fmt.Errorf("%w before %s", ErrInjected, step)
	case OutcomeCrashBefore:
		c.log("%s: crash before apply", step)
		p.Crash("before " + step)
		return ErrCrashed
	case OutcomeCrashAfter:
		if err := apply(); err != nil {
			c.log("%s: crash after failed apply: %v", step, err)
			p.Crash("after failed " + step)
			return ErrCrashed
		}
		c.log("%s: crash after apply", step)
		p.Crash("after " + step)
		return ErrCrashed
	}

	err := apply()

	if f.Rand != nil && f.MaxLatency > 0 {
		c.clock.Sleep(ctx, c.latency())
	}
	if outcome == OutcomeLostAck {
		// The answer is lost whether the request was applied or failed its precondition.
		c.log("%s: lost ack (%v)", step, err)
		return fmt.Errorf("%w after %s (lost ack)", ErrInjected, step)
	}
	if err != nil {
		c.log("%s: %v", step, err)
	} else {
		c.log("%s: ok", step)
	}
	return err
}

// crashed reports whether p is a process that has crashed.
func crashed(p *Process) bool {
	if p == nil {
		return false
	}
	crashed, _ := p.Crashed()
	return crashed
}

// outage fails a request during an outage or partition, as described at [Client.Unavailable].
func (c *Client) outage(ctx context.Context, p *Process, step string, write bool, apply func() error) error {
	timeout, applied := false, false
	if c.faults.Rand != nil && c.faults.OutageTimeout > 0 {
		c.randMu.Lock()
		timeout = c.faults.Rand.IntN(2) == 0
		applied = write && timeout && c.faults.Rand.IntN(2) == 0
		c.randMu.Unlock()
	}
	if !timeout {
		c.log("%s: unavailable, refused", step)
		return fmt.Errorf("%w: %s refused", ErrUnavailable, step)
	}
	c.clock.Sleep(ctx, c.faults.OutageTimeout)
	if crashed(p) {
		return ErrCrashed
	}
	if applied {
		err := apply()
		c.log("%s: unavailable, timed out after apply (%v)", step, err)
	} else {
		c.log("%s: unavailable, timed out", step)
	}
	return fmt.Errorf("%w: %s timed out", ErrUnavailable, step)
}

// latency draws a virtual latency. The random source is guarded because clients may be used
// from several goroutines.
func (c *Client) latency() time.Duration {
	c.randMu.Lock()
	defer c.randMu.Unlock()
	return time.Duration(c.faults.Rand.Int64N(int64(c.faults.MaxLatency)))
}

// decide an outcome from the fault rates.
func (c *Client) decide(hasProcess, write bool) Outcome {
	c.randMu.Lock()
	defer c.randMu.Unlock()
	f := c.faults
	switch {
	case f.ErrorRate > 0 && f.Rand.Float64() < f.ErrorRate:
		return OutcomeError
	case hasProcess && f.CrashRate > 0 && f.Rand.Float64() < f.CrashRate:
		if f.Rand.IntN(2) == 0 {
			return OutcomeCrashAfter
		}
		return OutcomeCrashBefore
	case write && f.LostAckRate > 0 && f.Rand.Float64() < f.LostAckRate:
		return OutcomeLostAck
	}
	return OutcomeNormal
}

func (c *Client) log(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

func (c *Client) Get(ctx context.Context, key string, opts s3.GetOptions) (s3.Object, error) {
	var res s3.Object
	err := c.request(ctx, s3.GetKind(opts), key, false, func() error {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		o, ok := c.store.objects[key]
		if !ok {
			return s3.ErrNotFound
		}
		if opts.IfNoneMatch != "" && opts.IfNoneMatch == o.etag {
			return s3.ErrNotModified
		}
		body := o.body
		if opts.Range != nil {
			r := *opts.Range
			if r.Offset < 0 || r.Length <= 0 || r.Offset >= int64(len(body)) {
				return fmt.Errorf("s3fake: invalid range %d+%d for object of size %d", r.Offset, r.Length, len(body))
			}
			end := int64(len(body))
			if r.Length < end-r.Offset {
				end = r.Offset + r.Length
			}
			body = body[r.Offset:end]
		}
		res = s3.Object{Body: append([]byte(nil), body...), ETag: o.etag}
		return nil
	})
	if err != nil {
		return s3.Object{}, err
	}
	return res, nil
}

func (c *Client) Put(ctx context.Context, key string, body []byte, opts s3.PutOptions) (string, error) {
	etag := etagOf(body)
	err := c.request(ctx, s3.PutKind(opts), key, true, func() error {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		o, exists := c.store.objects[key]
		if opts.IfNoneMatch && exists {
			return s3.ErrPreconditionFailed
		}
		if opts.IfMatch != "" && !exists {
			return s3.ErrNotFound
		}
		if opts.IfMatch != "" && o.etag != opts.IfMatch {
			return s3.ErrPreconditionFailed
		}
		c.store.objects[key] = object{body: append([]byte(nil), body...), etag: etag, modified: c.clock.Now()}
		return nil
	})
	if err != nil {
		return "", err
	}
	return etag, nil
}

func (c *Client) Delete(ctx context.Context, key string) error {
	return c.request(ctx, s3.KindDelete, key, true, func() error {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		delete(c.store.objects, key)
		return nil
	})
}

func (c *Client) List(ctx context.Context, prefix string) ([]s3.ObjectInfo, error) {
	var infos []s3.ObjectInfo
	err := c.request(ctx, s3.KindList, prefix, false, func() error {
		c.store.mu.Lock()
		defer c.store.mu.Unlock()
		for k, o := range c.store.objects {
			if strings.HasPrefix(k, prefix) {
				infos = append(infos, s3.ObjectInfo{Key: k, Size: int64(len(o.body)), LastModified: o.modified})
			}
		}
		sort.Slice(infos, func(i, j int) bool { return infos[i].Key < infos[j].Key })
		return nil
	})
	if err != nil {
		return nil, err
	}
	return infos, nil
}
