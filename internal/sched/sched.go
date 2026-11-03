// Package sched is a deterministic cooperative scheduler with virtual time.
//
// Tasks run as goroutines, but only one runs at a time: a task holds the baton until it calls
// [Scheduler.Sleep], which parks it until its virtual wake-up time. The scheduler then resumes
// the task with the earliest wake-up time (ties broken in creation order). Given the same
// sequence of calls, the interleaving is identical on every run, which makes a simulation
// reproducible from its seed alone.
package sched

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

type ctxKey struct{}

// Scheduler runs tasks one at a time in virtual time.
type Scheduler struct {
	epoch  time.Time
	mu     sync.Mutex
	now    time.Duration
	seq    uint64
	queue  waiters
	parked chan struct{}
	live   int
}

type task struct {
	name string
	wake chan struct{}
}

type waiter struct {
	at   time.Duration
	seq  uint64
	task *task
}

type waiters []waiter

func (w waiters) Len() int { return len(w) }
func (w waiters) Less(i, j int) bool {
	if w[i].at != w[j].at {
		return w[i].at < w[j].at
	}
	return w[i].seq < w[j].seq
}
func (w waiters) Swap(i, j int) { w[i], w[j] = w[j], w[i] }
func (w *waiters) Push(x any)   { *w = append(*w, x.(waiter)) }
func (w *waiters) Pop() any {
	old := *w
	x := old[len(old)-1]
	*w = old[:len(old)-1]
	return x
}

// New scheduler whose virtual clock starts at epoch.
func New(epoch time.Time) *Scheduler {
	return &Scheduler{epoch: epoch, parked: make(chan struct{})}
}

// Go starts a task. It can be called before [Scheduler.Run] or from a running task.
// The task's context carries its identity, so [Scheduler.Sleep] must be called with it or a context derived from it.
func (s *Scheduler) Go(name string, f func(ctx context.Context)) {
	t := &task{name: name, wake: make(chan struct{})}
	s.mu.Lock()
	s.live++
	s.enqueueLocked(t, s.now)
	s.mu.Unlock()

	ctx := context.WithValue(context.Background(), ctxKey{}, t)
	go func() {
		<-t.wake
		f(ctx)
		s.mu.Lock()
		s.live--
		s.mu.Unlock()
		s.parked <- struct{}{}
	}()
}

func (s *Scheduler) enqueueLocked(t *task, at time.Duration) {
	s.seq++
	heap.Push(&s.queue, waiter{at: at, seq: s.seq, task: t})
}

// Run tasks until all of them have finished.
func (s *Scheduler) Run() {
	for {
		s.mu.Lock()
		if s.queue.Len() == 0 {
			s.mu.Unlock()
			return
		}
		w := heap.Pop(&s.queue).(waiter)
		if w.at > s.now {
			s.now = w.at
		}
		s.mu.Unlock()

		w.task.wake <- struct{}{}
		<-s.parked
	}
}

// Now in virtual time.
func (s *Scheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch.Add(s.now)
}

// Elapsed virtual time since the epoch.
func (s *Scheduler) Elapsed() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// Sleep parks the calling task for d of virtual time and yields to other tasks.
// A zero d yields without advancing time. It panics when ctx does not belong to a task.
func (s *Scheduler) Sleep(ctx context.Context, d time.Duration) {
	t, ok := ctx.Value(ctxKey{}).(*task)
	if !ok {
		panic("sched: Sleep called outside a scheduled task")
	}
	if d < 0 {
		d = 0
	}
	s.mu.Lock()
	s.enqueueLocked(t, s.now+d)
	s.mu.Unlock()
	s.parked <- struct{}{}
	<-t.wake
}
