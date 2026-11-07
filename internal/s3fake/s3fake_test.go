package s3fake_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"hegel.dev/go/hegel"
	"maragu.dev/is"

	"maragu.dev/maragudb/internal/s3"
	"maragu.dev/maragudb/internal/s3fake"
	"maragu.dev/maragudb/internal/verify"
)

func TestMain(m *testing.M) {
	verify.Main(m)
}

func TestClient_Put(t *testing.T) {
	t.Run("returns the same ETag that a later Get returns", func(t *testing.T) {
		c := s3fake.New()
		etag, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		o, err := c.Get(t.Context(), "k", s3.GetOptions{})
		is.NotError(t, err)
		is.Equal(t, etag, o.ETag)
		is.Equal(t, "v", string(o.Body))
	})

	t.Run("fails with ErrPreconditionFailed when IfNoneMatch and the object exists", func(t *testing.T) {
		c := s3fake.New()
		_, err := c.Put(t.Context(), "k", []byte("v1"), s3.PutOptions{IfNoneMatch: true})
		is.NotError(t, err)
		_, err = c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfNoneMatch: true})
		is.Error(t, s3.ErrPreconditionFailed, err)
		o, err := c.Get(t.Context(), "k", s3.GetOptions{})
		is.NotError(t, err)
		is.Equal(t, "v1", string(o.Body))
	})

	t.Run("fails with ErrPreconditionFailed when IfMatch does not match", func(t *testing.T) {
		verify.Covers(t, "the fake refuses a conditional write whose ETag does not match, as S3 does", "INV-CAS-COMMIT")
		c := s3fake.New()
		_, err := c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfMatch: `"nope"`})
		is.Error(t, s3.ErrNotFound, err, "missing object, as S3 answers")

		etag, err := c.Put(t.Context(), "k", []byte("v1"), s3.PutOptions{})
		is.NotError(t, err)
		_, err = c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfMatch: `"nope"`})
		is.Error(t, s3.ErrPreconditionFailed, err, "wrong etag")

		etag2, err := c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfMatch: etag})
		is.NotError(t, err)
		is.True(t, etag != etag2)
		_, err = c.Put(t.Context(), "k", []byte("v3"), s3.PutOptions{IfMatch: etag})
		is.Error(t, s3.ErrPreconditionFailed, err, "stale etag")
	})
}

func TestClient_Get(t *testing.T) {
	t.Run("returns ErrNotFound for a missing object", func(t *testing.T) {
		c := s3fake.New()
		_, err := c.Get(t.Context(), "k", s3.GetOptions{})
		is.Error(t, s3.ErrNotFound, err)
	})

	t.Run("returns ErrNotModified when IfNoneMatch matches and the body otherwise", func(t *testing.T) {
		c := s3fake.New()
		etag, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		_, err = c.Get(t.Context(), "k", s3.GetOptions{IfNoneMatch: etag})
		is.Error(t, s3.ErrNotModified, err)
		o, err := c.Get(t.Context(), "k", s3.GetOptions{IfNoneMatch: `"other"`})
		is.NotError(t, err)
		is.Equal(t, "v", string(o.Body))
	})

	t.Run("returns a byte range", func(t *testing.T) {
		c := s3fake.New()
		_, err := c.Put(t.Context(), "k", []byte("0123456789"), s3.PutOptions{})
		is.NotError(t, err)
		o, err := c.Get(t.Context(), "k", s3.GetOptions{Range: &s3.ByteRange{Offset: 2, Length: 3}})
		is.NotError(t, err)
		is.Equal(t, "234", string(o.Body))
		o, err = c.Get(t.Context(), "k", s3.GetOptions{Range: &s3.ByteRange{Offset: 8, Length: 10}})
		is.NotError(t, err)
		is.Equal(t, "89", string(o.Body))
		_, err = c.Get(t.Context(), "k", s3.GetOptions{Range: &s3.ByteRange{Offset: 10, Length: 1}})
		is.True(t, err != nil)
		o, err = c.Get(t.Context(), "k", s3.GetOptions{Range: &s3.ByteRange{Offset: 1, Length: math.MaxInt64}})
		is.NotError(t, err, "a length that overflows is clipped")
		is.Equal(t, "123456789", string(o.Body))
	})
}

func TestClient_Delete(t *testing.T) {
	t.Run("removes the object and ignores missing objects", func(t *testing.T) {
		c := s3fake.New()
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		is.NotError(t, c.Delete(t.Context(), "k"))
		_, err = c.Get(t.Context(), "k", s3.GetOptions{})
		is.Error(t, s3.ErrNotFound, err)
		is.NotError(t, c.Delete(t.Context(), "k"))
	})
}

func TestClient_List(t *testing.T) {
	t.Run("lists objects under a prefix sorted by key", func(t *testing.T) {
		c := s3fake.New()
		for _, k := range []string{"a/2", "b/1", "a/1", "a/10"} {
			_, err := c.Put(t.Context(), k, []byte(k), s3.PutOptions{})
			is.NotError(t, err)
		}
		infos, err := c.List(t.Context(), "a/")
		is.NotError(t, err)
		var keys []string
		for _, i := range infos {
			keys = append(keys, i.Key)
			is.Equal(t, int64(len(i.Key)), i.Size)
		}
		is.EqualSlice(t, []string{"a/1", "a/10", "a/2"}, keys)
	})
}

func TestClient_Counts(t *testing.T) {
	t.Run("counts requests by kind including failed ones", func(t *testing.T) {
		c := s3fake.New()
		_, _ = c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		_, _ = c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{IfNoneMatch: true})
		_, _ = c.Get(t.Context(), "k", s3.GetOptions{})
		_, _ = c.Get(t.Context(), "k", s3.GetOptions{IfNoneMatch: `"x"`})
		_, _ = c.Get(t.Context(), "missing", s3.GetOptions{})
		_ = c.Delete(t.Context(), "k")
		_, _ = c.List(t.Context(), "")
		is.Equal(t, s3.Counts{Get: 2, GetConditional: 1, Put: 1, PutConditional: 1, Delete: 1, List: 1}, c.Counts())
	})
}

func TestClient_Faults(t *testing.T) {
	t.Run("errors are not applied", func(t *testing.T) {
		verify.Covers(t, "an injected error fails the write and leaves the store unchanged", "AC-1.1")
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), ErrorRate: 1}))
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrInjected, err)
		is.Equal(t, 0, len(c.Keys()))
	})

	t.Run("lost acks are applied but reported as errors", func(t *testing.T) {
		verify.Covers(t, "a lost acknowledgement applies the write and still reports an error", "AC-1.3")
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), LostAckRate: 1}))
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrInjected, err)
		body, ok := c.Body("k")
		is.True(t, ok)
		is.Equal(t, "v", string(body))
	})

	t.Run("a lost ack hides a precondition failure too", func(t *testing.T) {
		verify.Covers(t, "a lost acknowledgement also hides a precondition failure, so the client cannot tell what happened", "AC-1.3")
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Decide: func(kind s3.Kind, key string) s3fake.Outcome {
			return s3fake.OutcomeLostAck
		}}))
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrInjected, err)
		_, err = c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfNoneMatch: true})
		is.Error(t, s3fake.ErrInjected, err)
		is.True(t, !errors.Is(err, s3.ErrPreconditionFailed))
		body, _ := c.Body("k")
		is.Equal(t, "v", string(body))
	})

	t.Run("rates are safe to use from several goroutines", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), ErrorRate: 0.5, LostAckRate: 0.5, MaxLatency: time.Millisecond}))
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range 50 {
					_, _ = c.Put(t.Context(), fmt.Sprint(i, j), []byte("v"), s3.PutOptions{})
				}
			}()
		}
		wg.Wait()
		is.Equal(t, int64(400), c.Counts().Put)
	})

	t.Run("lost acks never affect reads", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), LostAckRate: 1}))
		_, _ = c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		o, err := c.Get(t.Context(), "k", s3.GetOptions{})
		is.NotError(t, err)
		is.Equal(t, "v", string(o.Body))
	})

	t.Run("latency advances the virtual clock", func(t *testing.T) {
		verify.Covers(t, "injected latency advances the virtual clock, by at most the maximum latency", "AC-1.2")
		clock := s3fake.NewVirtualClock(time.Unix(0, 0))
		c := s3fake.New(s3fake.WithClock(clock), s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), MaxLatency: time.Second}))
		before := clock.Now()
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		is.True(t, clock.Now().After(before))
		is.True(t, clock.Now().Sub(before) < 2*time.Second)
	})

	t.Run("a crashed process cannot make further requests", func(t *testing.T) {
		verify.Covers(t, "a crashed process fails every later request, and other processes are unaffected", "AC-1.4")
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), CrashRate: 1}))
		p := &s3fake.Process{Name: "p"}
		ctx := s3fake.WithProcess(t.Context(), p)
		_, err := c.Put(ctx, "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrCrashed, err)
		crashed, step := p.Crashed()
		is.True(t, crashed)
		is.True(t, step != "")
		_, err = c.Get(ctx, "k", s3.GetOptions{})
		is.Error(t, s3fake.ErrCrashed, err)
		_, err = c.Get(t.Context(), "k", s3.GetOptions{})
		is.True(t, !errors.Is(err, s3fake.ErrCrashed), "another process is unaffected")
	})

	t.Run("crashes happen both before and after apply", func(t *testing.T) {
		verify.Covers(t, "over 50 seeds, crashes happen both before and after the write is applied", "AC-1.4")
		var before, after int
		for seed := range uint64(50) {
			c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(seed, 0)), CrashRate: 1}))
			p := &s3fake.Process{Name: "p"}
			_, err := c.Put(s3fake.WithProcess(t.Context(), p), "k", []byte("v"), s3.PutOptions{})
			is.Error(t, s3fake.ErrCrashed, err)
			if _, ok := c.Body("k"); ok {
				after++
			} else {
				before++
			}
		}
		is.True(t, before > 0)
		is.True(t, after > 0)
	})

	t.Run("crashes do not affect requests without a process", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), CrashRate: 1}))
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
	})

	t.Run("the same seed produces the same outcomes", func(t *testing.T) {
		verify.Covers(t, "the same seed produces the same fault outcomes, final keys, and virtual time", "AC-1.9")
		run := func() string {
			c := s3fake.New(s3fake.WithFaults(s3fake.Faults{
				Rand: rand.New(rand.NewPCG(7, 7)), ErrorRate: 0.2, LostAckRate: 0.2, CrashRate: 0.01, MaxLatency: time.Second,
			}))
			var trace string
			for i := range 200 {
				p := &s3fake.Process{Name: "p"}
				ctx := s3fake.WithProcess(t.Context(), p)
				_, err := c.Put(ctx, fmt.Sprint("k", i%5), []byte(fmt.Sprint(i)), s3.PutOptions{})
				trace += fmt.Sprintf("%d:%v;", i, err)
			}
			return trace + fmt.Sprint(c.Keys(), c.Now())
		}
		first := run()
		is.Equal(t, first, run())
	})
}

func TestClient_Unavailable(t *testing.T) {
	newClient := func(timeout time.Duration) (*s3fake.Client, *s3fake.VirtualClock) {
		clock := s3fake.NewVirtualClock(time.Unix(0, 0))
		return s3fake.New(s3fake.WithClock(clock), s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), OutageTimeout: timeout})), clock
	}

	t.Run("an outage refuses every request until it ends, without crashing the process", func(t *testing.T) {
		verify.Covers(t, "an outage fails every request until it ends, while the process keeps running", "AC-1.1")
		c, clock := newClient(0)
		p := &s3fake.Process{Name: "p"}
		ctx := s3fake.WithProcess(t.Context(), p)
		c.Unavailable("", clock.Now().Add(time.Second))
		_, err := c.Put(ctx, "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err)
		_, err = c.Get(ctx, "k", s3.GetOptions{})
		is.Error(t, s3fake.ErrUnavailable, err)
		_, err = c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err, "requests without a process too")
		is.Error(t, s3fake.ErrUnavailable, c.Delete(ctx, "k"))
		_, err = c.List(ctx, "")
		is.Error(t, s3fake.ErrUnavailable, err)
		is.Equal(t, 0, len(c.Keys()), "refused writes are not applied")
		crashed, _ := p.Crashed()
		is.True(t, !crashed, "the process keeps running")

		clock.Sleep(t.Context(), time.Second)
		_, err = c.Put(ctx, "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err, "the outage is over")
	})

	t.Run("a partition only affects the named process", func(t *testing.T) {
		c, clock := newClient(0)
		pctx := s3fake.WithProcess(t.Context(), &s3fake.Process{Name: "p"})
		qctx := s3fake.WithProcess(t.Context(), &s3fake.Process{Name: "q"})
		c.Unavailable("p", clock.Now().Add(time.Second))
		_, err := c.Put(pctx, "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err)
		_, err = c.Put(qctx, "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		_, err = c.Get(t.Context(), "k", s3.GetOptions{})
		is.NotError(t, err, "requests without a process are not partitioned")
		_, err = c.Get(s3fake.WithProcess(t.Context(), &s3fake.Process{Name: "p"}), "k", s3.GetOptions{})
		is.Error(t, s3fake.ErrUnavailable, err, "a restarted process with the same name is still partitioned")
	})

	t.Run("extending a partition never ends it earlier", func(t *testing.T) {
		c, clock := newClient(0)
		pctx := s3fake.WithProcess(t.Context(), &s3fake.Process{Name: "p"})
		c.Unavailable("p", clock.Now().Add(time.Second))
		c.ExtendUnavailable("p", clock.Now().Add(100*time.Millisecond))
		clock.Sleep(t.Context(), 500*time.Millisecond)
		_, err := c.Put(pctx, "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err, "the longer partition still holds")
		c.ExtendUnavailable("p", clock.Now().Add(time.Second))
		clock.Sleep(t.Context(), 700*time.Millisecond)
		_, err = c.Put(pctx, "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err, "the extended partition holds")
		clock.Sleep(t.Context(), 300*time.Millisecond)
		_, err = c.Put(pctx, "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
	})

	t.Run("a fault-free view ignores outages", func(t *testing.T) {
		c, clock := newClient(0)
		c.Unavailable("", clock.Now().Add(time.Second))
		_, err := c.WithoutFaults().Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
	})

	t.Run("with a timeout, requests are refused or hang, and some timed-out writes are applied", func(t *testing.T) {
		c, clock := newClient(100 * time.Millisecond)
		c.Unavailable("", clock.Now().Add(time.Hour))
		var refused, timedOut, applied int
		for i := range 200 {
			key := fmt.Sprint("k", i)
			before := clock.Now()
			_, err := c.Put(t.Context(), key, []byte("v"), s3.PutOptions{})
			is.Error(t, s3fake.ErrUnavailable, err)
			switch clock.Now().Sub(before) {
			case 0:
				refused++
				_, ok := c.Body(key)
				is.True(t, !ok, "a refused write is not applied")
			case 100 * time.Millisecond:
				timedOut++
				if _, ok := c.Body(key); ok {
					applied++
				}
			default:
				t.Fatalf("request took %v", clock.Now().Sub(before))
			}
		}
		is.True(t, refused > 50, fmt.Sprint("refused ", refused))
		is.True(t, timedOut > 50, fmt.Sprint("timed out ", timedOut))
		is.True(t, applied > 10 && applied < timedOut-10, fmt.Sprint("applied ", applied, " of ", timedOut))
	})

	t.Run("a timed-out write checks its condition when it lands", func(t *testing.T) {
		c, clock := newClient(100 * time.Millisecond)
		_, err := c.Put(t.Context(), "k", []byte("v1"), s3.PutOptions{})
		is.NotError(t, err)
		c.Unavailable("", clock.Now().Add(time.Hour))
		for range 50 {
			_, err := c.Put(t.Context(), "k", []byte("v2"), s3.PutOptions{IfNoneMatch: true})
			is.Error(t, s3fake.ErrUnavailable, err)
		}
		body, _ := c.Body("k")
		is.Equal(t, "v1", string(body))
	})
}

// hookClock runs a hook on every sleep, standing in for another request of the same process
// that runs meanwhile.
type hookClock struct {
	*s3fake.VirtualClock
	hook func()
}

func (c hookClock) Sleep(ctx context.Context, d time.Duration) {
	c.VirtualClock.Sleep(ctx, d)
	c.hook()
}

func TestClient_crashDuringRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		faults s3fake.Faults
		before func(c *s3fake.Client)
	}{
		{name: "a process that crashes while a request waits out its latency does not apply it", faults: s3fake.Faults{MaxLatency: time.Second}},
		{name: "a process that crashes while a request times out in an outage does not apply it", faults: s3fake.Faults{OutageTimeout: time.Second}, before: func(c *s3fake.Client) {
			c.Unavailable("", c.Now().Add(time.Hour))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for seed := range uint64(20) {
				p := &s3fake.Process{Name: "p"}
				clock := hookClock{VirtualClock: s3fake.NewVirtualClock(time.Unix(0, 0)), hook: func() { p.Crash("another request") }}
				faults := test.faults
				faults.Rand = rand.New(rand.NewPCG(seed, 1))
				c := s3fake.New(s3fake.WithClock(clock), s3fake.WithFaults(faults))
				if test.before != nil {
					test.before(c)
				}
				_, err := c.Put(s3fake.WithProcess(t.Context(), p), "k", []byte("v"), s3.PutOptions{})
				is.True(t, err != nil)
				_, ok := c.Body("k")
				is.True(t, !ok, "nothing is applied after a crash")
			}
		})
	}
}

func TestNew(t *testing.T) {
	t.Run("panics on an outage rate without a maximum outage length", func(t *testing.T) {
		defer func() {
			is.True(t, recover() != nil)
		}()
		s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 1)), OutageRate: 0.5}))
	})
}

func TestClient_OutageRate(t *testing.T) {
	t.Run("a process without a name is never partitioned on its own, so its outages reach everyone", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 1)), OutageRate: 1, MaxOutage: time.Hour}))
		_, err := c.Put(s3fake.WithProcess(t.Context(), &s3fake.Process{}), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err)
		is.Equal(t, 1, c.Outages())
		_, err = c.Put(s3fake.WithProcess(t.Context(), &s3fake.Process{Name: "q"}), "k", []byte("v"), s3.PutOptions{})
		is.Error(t, s3fake.ErrUnavailable, err)
	})

	t.Run("starts partitions and outages that fail the request that started them", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 1)), OutageRate: 0.1, MaxOutage: 50 * time.Millisecond, MaxLatency: 100 * time.Millisecond}))
		for i := range 200 {
			_, err := c.Put(s3fake.WithProcess(t.Context(), &s3fake.Process{Name: fmt.Sprint("p", i%4)}), fmt.Sprint("k", i), []byte("v"), s3.PutOptions{})
			if err != nil {
				is.Error(t, s3fake.ErrUnavailable, err)
			}
		}
		is.True(t, c.Outages() > 5, fmt.Sprint(c.Outages(), " outages"))
	})
}

// modelObject in the reference model.
type modelObject struct {
	body []byte
	etag string
}

func TestClient_MatchesModel(t *testing.T) {
	t.Run("behaves like a map with content ETags under random operations", func(t *testing.T) {
		hegel.Test(t, func(ht *hegel.T) {
			c := s3fake.New()
			model := map[string]modelObject{}
			etagByKey := map[string]string{}
			keys := hegel.Draw(ht, hegel.Lists(hegel.Text().MinSize(1).MaxSize(3)).MinSize(1).MaxSize(4))
			n := hegel.Draw(ht, hegel.Integers(1, 40))
			for range n {
				key := hegel.Draw(ht, hegel.SampledFrom(keys))
				switch hegel.Draw(ht, hegel.Integers(0, 4)) {
				case 0, 1:
					body := hegel.Draw(ht, hegel.Binary(0, 4))
					var opts s3.PutOptions
					switch hegel.Draw(ht, hegel.Integers(0, 2)) {
					case 1:
						opts.IfNoneMatch = true
					case 2:
						if hegel.Draw(ht, hegel.Booleans()) {
							opts.IfMatch = etagByKey[key]
							if opts.IfMatch == "" {
								opts.IfMatch = `"missing"`
							}
						} else {
							opts.IfMatch = `"stale"`
						}
					}
					etag, err := c.Put(t.Context(), key, body, opts)
					cur, exists := model[key]
					if opts.IfMatch != "" && !exists {
						if !errors.Is(err, s3.ErrNotFound) {
							ht.Fatalf("put %q %+v: want not found, got %v", key, opts, err)
						}
						continue
					}
					wantFail := (opts.IfNoneMatch && exists) || (opts.IfMatch != "" && cur.etag != opts.IfMatch)
					if wantFail {
						if !errors.Is(err, s3.ErrPreconditionFailed) {
							ht.Fatalf("put %q %+v: want precondition failure, got %v", key, opts, err)
						}
						continue
					}
					if err != nil {
						ht.Fatalf("put %q: %v", key, err)
					}
					model[key] = modelObject{body: body, etag: etag}
					etagByKey[key] = etag
				case 2:
					o, err := c.Get(t.Context(), key, s3.GetOptions{})
					cur, exists := model[key]
					if !exists {
						if !errors.Is(err, s3.ErrNotFound) {
							ht.Fatalf("get missing %q: want ErrNotFound, got %v", key, err)
						}
						continue
					}
					if err != nil || string(o.Body) != string(cur.body) || o.ETag != cur.etag {
						ht.Fatalf("get %q: got %q/%q %v, want %q/%q", key, o.Body, o.ETag, err, cur.body, cur.etag)
					}
				case 3:
					if err := c.Delete(t.Context(), key); err != nil {
						ht.Fatalf("delete %q: %v", key, err)
					}
					delete(model, key)
				case 4:
					infos, err := c.List(t.Context(), "")
					if err != nil {
						ht.Fatalf("list: %v", err)
					}
					if len(infos) != len(model) {
						ht.Fatalf("list: %d objects, want %d", len(infos), len(model))
					}
					for i, info := range infos {
						if _, ok := model[info.Key]; !ok {
							ht.Fatalf("list: unexpected key %q", info.Key)
						}
						if i > 0 && infos[i-1].Key >= info.Key {
							ht.Fatalf("list: not sorted: %q >= %q", infos[i-1].Key, info.Key)
						}
					}
				}
			}
		}, hegel.WithTestCases(500))
	})
}

func TestClient_DamageRate(t *testing.T) {
	t.Run("damages the bodies of reads with flipped bits, cuts, and the bytes of other objects at the same range, leaves the objects whole, and counts the damage by process", func(t *testing.T) {
		verify.Covers(t, "the fake store damages the bodies of reads at a rate, with a flipped bit, a cut, or another object's bytes at the same range, while the objects stay whole", "INV-DAMAGE-DETECTED")
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Rand: rand.New(rand.NewPCG(1, 2)), DamageRate: 0.5}))
		clean := c.WithoutFaults()
		a := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		b := []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		_, err := clean.Put(t.Context(), "a", a, s3.PutOptions{})
		is.NotError(t, err)
		_, err = clean.Put(t.Context(), "b", b, s3.PutOptions{})
		is.NotError(t, err)
		p := &s3fake.Process{Name: "p"}
		ctx := s3fake.WithProcess(t.Context(), p)
		var whole, flipped, cut, other int
		for range 300 {
			o, err := c.Get(ctx, "a", s3.GetOptions{Range: &s3.ByteRange{Offset: 10, Length: 20}})
			is.NotError(t, err)
			want := a[10:30]
			switch {
			case bytes.Equal(o.Body, want):
				whole++
			case bytes.Equal(o.Body, b[10:30]):
				other++
			case len(o.Body) < len(want) && bytes.Equal(o.Body, want[:len(o.Body)]):
				cut++
			case len(o.Body) == len(want):
				flipped++
			default:
				t.Fatalf("unexpected body %q", o.Body)
			}
		}
		is.True(t, whole > 100 && flipped > 20 && cut > 20 && other > 20, fmt.Sprint(whole, flipped, cut, other))
		is.Equal(t, 300-whole, c.Damaged("p"))
		is.Equal(t, 0, c.Damaged(""))
		body, ok := c.Body("a")
		is.True(t, ok)
		is.True(t, bytes.Equal(a, body))
		o, err := clean.Get(t.Context(), "a", s3.GetOptions{})
		is.NotError(t, err)
		is.True(t, bytes.Equal(a, o.Body), "a fault-free view never damages")
	})

	t.Run("a damage script replaces the body of every read that returns one", func(t *testing.T) {
		c := s3fake.New(s3fake.WithFaults(s3fake.Faults{Damage: func(key string, body []byte) []byte {
			return append(body, '!')
		}}))
		_, err := c.Put(t.Context(), "k", []byte("v"), s3.PutOptions{})
		is.NotError(t, err)
		o, err := c.Get(t.Context(), "k", s3.GetOptions{})
		is.NotError(t, err)
		is.Equal(t, "v!", string(o.Body))
		_, err = c.Get(t.Context(), "k", s3.GetOptions{IfNoneMatch: o.ETag})
		is.Error(t, s3.ErrNotModified, err)
		is.Equal(t, 1, c.Damaged(""))
	})
}
