package bench

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/sched"
)

func TestMeter(t *testing.T) {
	t.Run("counts requests by kind, the bytes of GET and PUT bodies, and the failed conditional writes of the manifest", func(t *testing.T) {
		m := &meter{Client: s3fake.New(), manifest: "db/manifest"}
		ctx := t.Context()
		etag, err := m.Put(ctx, "db/manifest", []byte("12345"), s3.PutOptions{IfNoneMatch: true})
		is.NotError(t, err)
		_, err = m.Put(ctx, "db/manifest", []byte("123"), s3.PutOptions{IfMatch: `"other"`})
		is.True(t, errors.Is(err, s3.ErrPreconditionFailed))
		_, err = m.Put(ctx, "db/lease", []byte("1"), s3.PutOptions{IfNoneMatch: true})
		is.NotError(t, err)
		_, err = m.Put(ctx, "db/lease", []byte("1"), s3.PutOptions{IfNoneMatch: true})
		is.True(t, errors.Is(err, s3.ErrPreconditionFailed))
		_, err = m.Get(ctx, "db/manifest", s3.GetOptions{})
		is.NotError(t, err)
		_, err = m.Get(ctx, "db/manifest", s3.GetOptions{IfNoneMatch: etag})
		is.True(t, errors.Is(err, s3.ErrNotModified))
		_, err = m.List(ctx, "db/")
		is.NotError(t, err)
		is.NotError(t, m.Delete(ctx, "db/lease"))

		c := m.snapshot()
		is.Equal(t, s3.Counts{Get: 1, GetConditional: 1, PutConditional: 4, List: 1, Delete: 1}, c.requests)
		is.Equal(t, int64(5), c.bytesRead)
		is.Equal(t, int64(5+3+1+1), c.bytesWritten)
		is.Equal(t, int64(5+3), c.manifestBytesWritten)
		is.Equal(t, int64(1), c.preconditionFailed, "only the manifest's failed write is a conflict")
	})

	t.Run("makes each request take its virtual latency in a scheduler", func(t *testing.T) {
		s := sched.New(time.Unix(0, 0))
		m := &meter{Client: s3fake.New(), sched: s}
		s.Go("client", func(ctx context.Context) {
			_, _ = m.Put(ctx, "k", []byte("v"), s3.PutOptions{})
			_, _ = m.Get(ctx, "k", s3.GetOptions{})
		})
		s.Run()
		is.Equal(t, VirtualLatency(s3.KindPut)+VirtualLatency(s3.KindGet), s.Elapsed())
	})
}

func TestGroup(t *testing.T) {
	for _, mode := range []string{"virtual", "real"} {
		t.Run("waits for every task and returns the first error, in "+mode+" time", func(t *testing.T) {
			e := &Env{}
			if mode == "virtual" {
				e.Sched = sched.New(time.Unix(0, 0))
			}
			var finished atomic.Int64
			err := e.run(func(ctx context.Context) error {
				g := &group{env: e}
				g.goTask(ctx, "slow", func(ctx context.Context) error {
					e.sleep(ctx, 20*time.Millisecond)
					finished.Add(1)
					return errors.New("slow failed")
				})
				g.goTask(ctx, "fast", func(ctx context.Context) error {
					e.sleep(ctx, 10*time.Millisecond)
					finished.Add(1)
					return nil
				})
				return g.wait(ctx)
			})
			is.Equal(t, "slow: slow failed", err.Error())
			is.Equal(t, int64(2), finished.Load())
		})
	}

	t.Run("returns a panic of the main task as an error", func(t *testing.T) {
		e := &Env{Sched: sched.New(time.Unix(0, 0))}
		err := e.run(func(ctx context.Context) error { panic("boom") })
		is.True(t, strings.HasPrefix(err.Error(), "bench: task main panicked: boom"), err.Error())
	})

	t.Run("returns a panic of another task in virtual time as an error", func(t *testing.T) {
		e := &Env{Sched: sched.New(time.Unix(0, 0))}
		err := e.run(func(ctx context.Context) error {
			g := &group{env: e}
			g.goTask(ctx, "writer", func(ctx context.Context) error { panic("bang") })
			return g.wait(ctx)
		})
		is.True(t, strings.HasPrefix(err.Error(), "bench: task writer panicked: bang"), err.Error())
	})
}
