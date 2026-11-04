package s3_test

import (
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/tigristest"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestAWSClient_Tigris(t *testing.T) {
	verify.Covers(t, "the S3 client's Tigris suite can run: the credentials are there and Tigris answers", "AC-7.1")
	client, cfg := tigristest.NewClient(t)
	is.True(t, client.Tigris(), "Tigris endpoint detected")
	key := cfg.Prefix + "/object"

	t.Run("conditional writes and reads behave like the fake", func(t *testing.T) {
		verify.Covers(t, "on Tigris, If-None-Match, If-Match, conditional and ranged reads, list, and delete behave like the fake, and the PUT ETag equals the GET ETag; latencies are recorded", "AC-7.1", "INV-CAS-COMMIT", "AC-5.4")
		start := time.Now()
		etag, err := client.Put(t.Context(), key, []byte("v1"), s3.PutOptions{IfNoneMatch: true})
		is.NotError(t, err)
		verify.Record(t, verify.Fact{Latency: &verify.Latency{Name: "conditional PUT", Duration: time.Since(start)}})
		t.Logf("conditional PUT: %v", time.Since(start).Round(time.Millisecond))
		is.True(t, etag != "")

		_, err = client.Put(t.Context(), key, []byte("v2"), s3.PutOptions{IfNoneMatch: true})
		is.Error(t, s3.ErrPreconditionFailed, err, "If-None-Match on an existing object")

		_, err = client.Put(t.Context(), key, []byte("v2"), s3.PutOptions{IfMatch: `"stale"`})
		is.Error(t, s3.ErrPreconditionFailed, err, "If-Match with the wrong ETag")

		start = time.Now()
		o, err := client.Get(t.Context(), key, s3.GetOptions{Consistent: true})
		is.NotError(t, err)
		verify.Record(t, verify.Fact{Latency: &verify.Latency{Name: "consistent GET", Duration: time.Since(start)}})
		t.Logf("consistent GET: %v", time.Since(start).Round(time.Millisecond))
		is.Equal(t, "v1", string(o.Body))
		is.Equal(t, etag, o.ETag, "the PUT's ETag equals the GET's ETag")

		_, err = client.Get(t.Context(), key, s3.GetOptions{IfNoneMatch: etag, Consistent: true})
		is.Error(t, s3.ErrNotModified, err)

		etag2, err := client.Put(t.Context(), key, []byte("v2"), s3.PutOptions{IfMatch: etag})
		is.NotError(t, err)
		is.True(t, etag2 != etag)
		_, err = client.Put(t.Context(), key, []byte("v3"), s3.PutOptions{IfMatch: etag})
		is.Error(t, s3.ErrPreconditionFailed, err, "the old ETag no longer matches")

		o, err = client.Get(t.Context(), key, s3.GetOptions{Range: &s3.ByteRange{Offset: 1, Length: 1}})
		is.NotError(t, err)
		is.Equal(t, "2", string(o.Body))

		infos, err := client.List(t.Context(), cfg.Prefix)
		is.NotError(t, err)
		is.Equal(t, 1, len(infos))
		is.Equal(t, key, infos[0].Key)
		is.Equal(t, int64(2), infos[0].Size)

		is.NotError(t, client.Delete(t.Context(), key))
		_, err = client.Get(t.Context(), key, s3.GetOptions{})
		is.Error(t, s3.ErrNotFound, err)
		is.NotError(t, client.Delete(t.Context(), key), "deleting a missing object")
	})
}
