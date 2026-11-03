// Package s3fake is an in-process fake of an S3-compatible object store.
//
// It implements conditional writes faithfully, counts requests by kind, and can inject faults
// driven by a seeded random source: errors, latency in virtual time, lost acknowledgements
// (the write is applied but the caller sees an error), and client crashes at any step.
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
	// Decide scripts the outcome of each request instead of the rates when non-nil.
	// Latency still follows MaxLatency.
	Decide func(kind s3.Kind, key string) Outcome
	// ErrorRate is the probability that a request fails before it is applied.
	ErrorRate float64
	// LostAckRate is the probability that a write is applied but the caller sees an error.
	LostAckRate float64
	// CrashRate is the probability that the calling process crashes at a request,
	// before or after the request is applied with equal probability.
	// Crashes only affect requests whose context carries a [Process].
	CrashRate float64
	// MaxLatency bounds the uniformly drawn virtual latency added before and after a request.
	MaxLatency time.Duration
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
	store   *store
	counter s3.Counter
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
	if c.faults.Rand == nil && (c.faults.ErrorRate > 0 || c.faults.LostAckRate > 0 || c.faults.CrashRate > 0 || c.faults.MaxLatency > 0) {
		panic("s3fake: faults require a Rand")
	}
	return c
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
		c.clock.Sleep(ctx, time.Duration(f.Rand.Int64N(int64(f.MaxLatency))))
	}
	outcome := OutcomeNormal
	switch {
	case f.Decide != nil:
		outcome = f.Decide(kind, key)
	case f.Rand != nil:
		if f.ErrorRate > 0 && f.Rand.Float64() < f.ErrorRate {
			outcome = OutcomeError
		} else if p != nil && f.CrashRate > 0 && f.Rand.Float64() < f.CrashRate {
			outcome = OutcomeCrashBefore
			if f.Rand.IntN(2) == 0 {
				outcome = OutcomeCrashAfter
			}
		} else if write && f.LostAckRate > 0 && f.Rand.Float64() < f.LostAckRate {
			outcome = OutcomeLostAck
		}
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
		c.clock.Sleep(ctx, time.Duration(f.Rand.Int64N(int64(f.MaxLatency))))
	}
	if outcome == OutcomeLostAck && err == nil {
		c.log("%s: lost ack", step)
		return fmt.Errorf("%w after %s (lost ack)", ErrInjected, step)
	}
	if err != nil {
		c.log("%s: %v", step, err)
	} else {
		c.log("%s: ok", step)
	}
	return err
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
			end := min(r.Offset+r.Length, int64(len(body)))
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
		if opts.IfMatch != "" && (!exists || o.etag != opts.IfMatch) {
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
