package sched_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/maragudb/internal/sched"
)

func TestScheduler(t *testing.T) {
	t.Run("runs tasks in virtual time order and ties in creation order", func(t *testing.T) {
		s := sched.New(time.Unix(0, 0))
		var trace []string
		s.Go("a", func(ctx context.Context) {
			trace = append(trace, "a0")
			s.Sleep(ctx, 10*time.Millisecond)
			trace = append(trace, "a1")
			s.Sleep(ctx, 10*time.Millisecond)
			trace = append(trace, "a2")
		})
		s.Go("b", func(ctx context.Context) {
			trace = append(trace, "b0")
			s.Sleep(ctx, 15*time.Millisecond)
			trace = append(trace, "b1")
		})
		s.Run()
		is.EqualSlice(t, []string{"a0", "b0", "a1", "b1", "a2"}, trace)
		is.Equal(t, 20*time.Millisecond, s.Elapsed())
	})

	t.Run("produces the same trace on every run", func(t *testing.T) {
		run := func() string {
			s := sched.New(time.Unix(0, 0))
			var trace string
			for i := range 20 {
				s.Go(fmt.Sprint(i), func(ctx context.Context) {
					for j := range 5 {
						trace += fmt.Sprintf("%d:%d@%d ", i, j, s.Elapsed()/time.Millisecond)
						s.Sleep(ctx, time.Duration((i*7+j*3)%11)*time.Millisecond)
					}
				})
			}
			s.Run()
			return trace
		}
		first := run()
		for range 10 {
			is.Equal(t, first, run())
		}
	})

	t.Run("tasks started from tasks run too", func(t *testing.T) {
		s := sched.New(time.Unix(0, 0))
		var n int
		s.Go("parent", func(ctx context.Context) {
			s.Go("child", func(ctx context.Context) {
				s.Sleep(ctx, time.Second)
				n++
			})
			n++
		})
		s.Run()
		is.Equal(t, 2, n)
		is.Equal(t, time.Second, s.Elapsed())
	})

	t.Run("panics when sleeping outside a task", func(t *testing.T) {
		s := sched.New(time.Unix(0, 0))
		defer func() {
			r := recover()
			is.True(t, r != nil)
		}()
		s.Sleep(t.Context(), time.Second)
	})

	t.Run("reports task identity from context", func(t *testing.T) {
		s := sched.New(time.Unix(0, 0))
		is.True(t, !sched.InTask(t.Context()))
		s.Go("worker", func(ctx context.Context) {
			is.True(t, sched.InTask(ctx))
			is.Equal(t, "worker", sched.TaskName(ctx))
		})
		s.Run()
	})
}
