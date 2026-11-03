package maragudb

import (
	"context"
	"fmt"
	"sort"

	"maragu.dev/maragudb/internal/s3"
)

// Errors an object store set with [Options.Store] returns, possibly wrapped, so that the
// database can tell them apart from failures.
var (
	// ErrObjectNotFound is returned by a read of a missing object, and by a write with
	// [PutOptions.IfMatch] when the object is missing.
	ErrObjectNotFound = s3.ErrNotFound
	// ErrPreconditionFailed is returned by a write whose condition does not hold (HTTP 412).
	ErrPreconditionFailed = s3.ErrPreconditionFailed
	// ErrNotModified is returned by a read with [GetOptions.IfNoneMatch] when the object's ETag
	// equals it (HTTP 304).
	ErrNotModified = s3.ErrNotModified
)

// objectStore is an S3-compatible object store with conditional writes. [Options.Store]
// documents its contract.
type objectStore interface {
	// Get an object, or a byte range of it, and its ETag.
	Get(ctx context.Context, key string, opts GetOptions) (body []byte, etag string, err error)
	// Put an object and return its new ETag.
	Put(ctx context.Context, key string, body []byte, opts PutOptions) (etag string, err error)
	// Delete an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List the keys of the objects whose keys start with prefix, in any order.
	List(ctx context.Context, prefix string) ([]string, error)
}

// GetOptions for a read from an object store.
type GetOptions struct {
	// IfNoneMatch, when not empty, makes the read return [ErrNotModified] when the object's ETag
	// equals it.
	IfNoneMatch string
	// Offset and Length select the byte range [Offset, Offset+Length) when Length is positive.
	// A range that ends past the end of the object stops at the end. Length zero reads the
	// whole object.
	Offset, Length int64
	// Consistent asks for a strongly consistent read, for stores that offer both kinds.
	Consistent bool
}

// PutOptions for a write to an object store.
type PutOptions struct {
	// IfNoneMatch writes only when no object exists at the key (If-None-Match: *), and returns
	// [ErrPreconditionFailed] otherwise.
	IfNoneMatch bool
	// IfMatch, when not empty, writes only when the object's ETag equals it (If-Match), and
	// returns [ErrPreconditionFailed] otherwise.
	IfMatch string
}

// storeClient adapts an [objectStore] to the engine's client.
type storeClient struct {
	store objectStore
}

func (c storeClient) Get(ctx context.Context, key string, opts s3.GetOptions) (s3.Object, error) {
	o := GetOptions{IfNoneMatch: opts.IfNoneMatch, Consistent: opts.Consistent}
	if r := opts.Range; r != nil {
		if r.Offset < 0 || r.Length <= 0 {
			return s3.Object{}, fmt.Errorf("maragudb: invalid byte range %d+%d of %q", r.Offset, r.Length, key)
		}
		o.Offset, o.Length = r.Offset, r.Length
	}
	body, etag, err := c.store.Get(ctx, key, o)
	if err != nil {
		return s3.Object{}, err
	}
	return s3.Object{Body: body, ETag: etag}, nil
}

func (c storeClient) Put(ctx context.Context, key string, body []byte, opts s3.PutOptions) (string, error) {
	return c.store.Put(ctx, key, body, PutOptions{IfNoneMatch: opts.IfNoneMatch, IfMatch: opts.IfMatch})
}

func (c storeClient) Delete(ctx context.Context, key string) error {
	return c.store.Delete(ctx, key)
}

func (c storeClient) List(ctx context.Context, prefix string) ([]s3.ObjectInfo, error) {
	keys, err := c.store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	infos := make([]s3.ObjectInfo, len(keys))
	for i, k := range keys {
		infos[i] = s3.ObjectInfo{Key: k}
	}
	return infos, nil
}
