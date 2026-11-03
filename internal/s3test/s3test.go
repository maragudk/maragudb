// Package s3test adapts an internal object store client to the object store that
// [maragudb.Options.Store] accepts, so that tests can run the public package over the fake.
package s3test

import (
	"context"

	"maragu.dev/maragudb"
	"maragu.dev/maragudb/internal/s3"
)

// Store is an [s3.Client] with the method set of a [maragudb.Options.Store].
type Store struct {
	Client s3.Client
}

// NewStore over c.
func NewStore(c s3.Client) *Store {
	return &Store{Client: c}
}

// Get an object or a byte range of it.
func (s *Store) Get(ctx context.Context, key string, opts maragudb.GetOptions) ([]byte, string, error) {
	o := s3.GetOptions{IfNoneMatch: opts.IfNoneMatch, Consistent: opts.Consistent}
	if opts.Length > 0 {
		o.Range = &s3.ByteRange{Offset: opts.Offset, Length: opts.Length}
	}
	res, err := s.Client.Get(ctx, key, o)
	if err != nil {
		return nil, "", err
	}
	return res.Body, res.ETag, nil
}

// Put an object.
func (s *Store) Put(ctx context.Context, key string, body []byte, opts maragudb.PutOptions) (string, error) {
	return s.Client.Put(ctx, key, body, s3.PutOptions{IfNoneMatch: opts.IfNoneMatch, IfMatch: opts.IfMatch})
}

// Delete an object.
func (s *Store) Delete(ctx context.Context, key string) error {
	return s.Client.Delete(ctx, key)
}

// List the keys under a prefix.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	infos, err := s.Client.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, len(infos))
	for i, info := range infos {
		keys[i] = info.Key
	}
	return keys, nil
}
