// Package s3 abstracts an S3-compatible object store with conditional writes.
package s3

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

var (
	// ErrNotFound is returned when an object does not exist.
	ErrNotFound = errors.New("s3: object not found")
	// ErrPreconditionFailed is returned when a conditional write fails its condition (HTTP 412).
	ErrPreconditionFailed = errors.New("s3: precondition failed")
	// ErrNotModified is returned by a conditional read whose ETag still matches (HTTP 304).
	ErrNotModified = errors.New("s3: not modified")
)

// Object is an object body, or a byte range of it, with its ETag.
type Object struct {
	Body []byte
	ETag string
}

// ObjectInfo describes an object in a listing.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// ByteRange of an object, [Offset, Offset+Length).
type ByteRange struct {
	Offset int64
	Length int64
}

// GetOptions for a read.
type GetOptions struct {
	// IfNoneMatch returns [ErrNotModified] when the object's ETag equals this value. Empty means unconditional.
	IfNoneMatch string
	// Range limits the read to a byte range. Nil reads the whole object.
	Range *ByteRange
	// Consistent requests a strongly consistent read where the store distinguishes.
	Consistent bool
}

// PutOptions for a write.
type PutOptions struct {
	// IfNoneMatch writes only when no object exists at the key (If-None-Match: *).
	IfNoneMatch bool
	// IfMatch writes only when the current ETag equals this value. Empty means unconditional.
	IfMatch string
}

// Conditional reports whether the put has a condition.
func (o PutOptions) Conditional() bool {
	return o.IfNoneMatch || o.IfMatch != ""
}

// Client for an object store. Implementations must be safe for concurrent use.
type Client interface {
	// Get an object or a byte range of it.
	Get(ctx context.Context, key string, opts GetOptions) (Object, error)
	// Put an object and return its ETag.
	Put(ctx context.Context, key string, body []byte, opts PutOptions) (string, error)
	// Delete an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List objects under a prefix, ordered by key.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
}

// Counts of requests by kind.
type Counts struct {
	Get            int64
	GetConditional int64
	Put            int64
	PutConditional int64
	Delete         int64
	List           int64
}

// Total requests.
func (c Counts) Total() int64 {
	return c.Get + c.GetConditional + c.Put + c.PutConditional + c.Delete + c.List
}

// Sub returns c minus o.
func (c Counts) Sub(o Counts) Counts {
	return Counts{
		Get:            c.Get - o.Get,
		GetConditional: c.GetConditional - o.GetConditional,
		Put:            c.Put - o.Put,
		PutConditional: c.PutConditional - o.PutConditional,
		Delete:         c.Delete - o.Delete,
		List:           c.List - o.List,
	}
}

// Counter counts requests by kind. The zero value is ready to use.
type Counter struct {
	get, getConditional, put, putConditional, del, list atomic.Int64
}

// Record a request of the given kind.
func (c *Counter) Record(kind Kind) {
	switch kind {
	case KindGet:
		c.get.Add(1)
	case KindGetConditional:
		c.getConditional.Add(1)
	case KindPut:
		c.put.Add(1)
	case KindPutConditional:
		c.putConditional.Add(1)
	case KindDelete:
		c.del.Add(1)
	case KindList:
		c.list.Add(1)
	}
}

// Counts so far.
func (c *Counter) Counts() Counts {
	return Counts{
		Get:            c.get.Load(),
		GetConditional: c.getConditional.Load(),
		Put:            c.put.Load(),
		PutConditional: c.putConditional.Load(),
		Delete:         c.del.Load(),
		List:           c.list.Load(),
	}
}

// Kind of request.
type Kind int

// Request kinds.
const (
	KindGet Kind = iota
	KindGetConditional
	KindPut
	KindPutConditional
	KindDelete
	KindList
)

func (k Kind) String() string {
	switch k {
	case KindGet:
		return "GET"
	case KindGetConditional:
		return "GET-conditional"
	case KindPut:
		return "PUT"
	case KindPutConditional:
		return "PUT-conditional"
	case KindDelete:
		return "DELETE"
	case KindList:
		return "LIST"
	}
	return "unknown"
}

// GetKind of a read with the given options.
func GetKind(opts GetOptions) Kind {
	if opts.IfNoneMatch != "" {
		return KindGetConditional
	}
	return KindGet
}

// PutKind of a write with the given options.
func PutKind(opts PutOptions) Kind {
	if opts.Conditional() {
		return KindPutConditional
	}
	return KindPut
}

// CountingClient wraps a [Client] and counts requests by kind.
type CountingClient struct {
	Client
	Counter Counter
}

// NewCountingClient wrapping c.
func NewCountingClient(c Client) *CountingClient {
	return &CountingClient{Client: c}
}

func (c *CountingClient) Get(ctx context.Context, key string, opts GetOptions) (Object, error) {
	c.Counter.Record(GetKind(opts))
	return c.Client.Get(ctx, key, opts)
}

func (c *CountingClient) Put(ctx context.Context, key string, body []byte, opts PutOptions) (string, error) {
	c.Counter.Record(PutKind(opts))
	return c.Client.Put(ctx, key, body, opts)
}

func (c *CountingClient) Delete(ctx context.Context, key string) error {
	c.Counter.Record(KindDelete)
	return c.Client.Delete(ctx, key)
}

func (c *CountingClient) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	c.Counter.Record(KindList)
	return c.Client.List(ctx, prefix)
}
