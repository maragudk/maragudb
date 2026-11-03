package maragudb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"hegel.dev/go/hegel"

	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
)

// fakeStore has the method set of an [objectStore] over an [s3.Client]. It is the test adapter
// in internal/s3test, which this package's internal tests cannot import.
type fakeStore struct {
	c s3.Client
}

func (f fakeStore) Get(ctx context.Context, key string, opts GetOptions) ([]byte, string, error) {
	o := s3.GetOptions{IfNoneMatch: opts.IfNoneMatch, Consistent: opts.Consistent}
	if opts.Length > 0 {
		o.Range = &s3.ByteRange{Offset: opts.Offset, Length: opts.Length}
	}
	res, err := f.c.Get(ctx, key, o)
	return res.Body, res.ETag, err
}

func (f fakeStore) Put(ctx context.Context, key string, body []byte, opts PutOptions) (string, error) {
	return f.c.Put(ctx, key, body, s3.PutOptions{IfNoneMatch: opts.IfNoneMatch, IfMatch: opts.IfMatch})
}

func (f fakeStore) Delete(ctx context.Context, key string) error {
	return f.c.Delete(ctx, key)
}

func (f fakeStore) List(ctx context.Context, prefix string) ([]string, error) {
	infos, err := f.c.List(ctx, prefix)
	keys := make([]string, 0, len(infos))
	// Reversed, because a store may list in any order.
	for i := len(infos) - 1; i >= 0; i-- {
		keys = append(keys, infos[i].Key)
	}
	return keys, err
}

// errClass of an error for comparison.
func errClass(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrObjectNotFound):
		return "not found"
	case errors.Is(err, ErrPreconditionFailed):
		return "precondition failed"
	case errors.Is(err, ErrNotModified):
		return "not modified"
	}
	return "error"
}

func TestStoreClient(t *testing.T) {
	t.Run("behaves exactly like the client it adapts, for every sequence of requests", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			direct := s3fake.New()
			adapted := storeClient{store: fakeStore{c: s3fake.New()}}
			keys := []string{"a", "a/b", "b"}
			var etags []string
			n := hegel.Draw(ht, hegel.Integers(1, 30))
			for range n {
				key := hegel.Draw(ht, hegel.SampledFrom(keys))
				etag := ""
				if len(etags) > 0 && hegel.Draw(ht, hegel.Booleans()) {
					etag = hegel.Draw(ht, hegel.SampledFrom(etags))
				}
				var want, got string
				switch hegel.Draw(ht, hegel.Integers(0, 3)) {
				case 0:
					body := hegel.Draw(ht, hegel.Binary(0, 8))
					opts := s3.PutOptions{IfMatch: etag, IfNoneMatch: etag == "" && hegel.Draw(ht, hegel.Booleans())}
					e1, err1 := direct.Put(t.Context(), key, body, opts)
					e2, err2 := adapted.Put(t.Context(), key, body, opts)
					want, got = fmt.Sprint(e1, errClass(err1)), fmt.Sprint(e2, errClass(err2))
					if err1 == nil {
						etags = append(etags, e1)
					}
				case 1:
					opts := s3.GetOptions{IfNoneMatch: etag, Consistent: hegel.Draw(ht, hegel.Booleans())}
					if hegel.Draw(ht, hegel.Booleans()) {
						opts.Range = &s3.ByteRange{Offset: hegel.Draw(ht, hegel.Integers[int64](-1, 9)), Length: hegel.Draw(ht, hegel.Integers[int64](-1, 9))}
					}
					o1, err1 := direct.Get(t.Context(), key, opts)
					o2, err2 := adapted.Get(t.Context(), key, opts)
					want, got = fmt.Sprintf("%q %s %s", o1.Body, o1.ETag, errClass(err1)), fmt.Sprintf("%q %s %s", o2.Body, o2.ETag, errClass(err2))
					if r := opts.Range; r != nil && (r.Offset < 0 || r.Length <= 0) {
						// The engine never asks for an invalid range. Both refuse it, though the fake
						// may first report a missing object.
						want, got = fmt.Sprint(err1 != nil), fmt.Sprint(err2 != nil)
					}
				case 2:
					want, got = errClass(direct.Delete(t.Context(), key)), errClass(adapted.Delete(t.Context(), key))
				case 3:
					prefix := hegel.Draw(ht, hegel.SampledFrom([]string{"", "a", "a/", "c"}))
					l1, err1 := direct.List(t.Context(), prefix)
					l2, err2 := adapted.List(t.Context(), prefix)
					k1, k2 := make([]string, len(l1)), make([]string, len(l2))
					for i := range l1 {
						k1[i] = l1[i].Key
					}
					for i := range l2 {
						k2[i] = l2[i].Key
					}
					want, got = fmt.Sprint(k1, errClass(err1)), fmt.Sprint(k2, errClass(err2))
				}
				if want != got {
					ht.Fatalf("direct %s, adapted %s", want, got)
				}
			}
		}, hegel.WithTestCases(300))
	})
}
