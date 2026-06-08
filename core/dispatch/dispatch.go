// Package dispatch provides per-chat job serialization with a global
// concurrency cap. Each chat gets its own goroutine + buffered channel so jobs
// for one chat run serially in FIFO order; a shared weighted semaphore gates job
// *starts* so different chats run in parallel up to a configured cap. This
// replaces Stage 2's single per-process serial guard with real per-chat
// isolation and parallelism (plan §7.4).
package dispatch

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"
)

// job is one unit of work plus the per-chat cancelable context it runs under.
type job struct {
	ctx context.Context
	run func(ctx context.Context)
}

// chatQueue owns the goroutine + buffered channel for a single chat and the
// cancel hook for that chat's currently-running (or next-to-run) job.
//
// The channel is never closed: workers exit via the dispatcher's cancelled
// rootCtx (see worker), which keeps Submit's send safe against a concurrent
// Shutdown — there is no closed channel to send on. Once a Dispatcher is
// unreferenced, GC reclaims its queues and channels.
//
// TODO(stage-4+): per-chat worker goroutines are never reaped while the
// dispatcher lives; a long-running process accumulates one idle goroutine per
// distinct chat ID. Add an idle-reaper (close+delete a chat's queue after an
// idle timeout, recreating it on the next Submit) when chat churn warrants it.
// Out of Stage 3 scope.
type chatQueue struct {
	ch chan job

	mu     sync.Mutex
	cancel context.CancelFunc // cancels the in-flight job's ctx; nil when idle
}

// Dispatcher runs submitted jobs serially within a chat and in parallel across
// chats, capped globally by a weighted semaphore.
type Dispatcher struct {
	sem     *semaphore.Weighted
	bufSize int

	mu       sync.Mutex
	chats    map[int64]*chatQueue
	closed   bool
	wg       sync.WaitGroup // tracks per-chat worker goroutines for graceful drain
	rootCtx  context.Context
	rootStop context.CancelFunc
}

// queueBuffer is the per-chat channel buffer: how many jobs may be queued for a
// chat before Submit would block. Generous enough that Submit is effectively
// non-blocking under normal load while still bounding memory.
const queueBuffer = 64

// New creates a Dispatcher allowing at most maxConcurrent jobs to run at once
// across all chats. A non-positive maxConcurrent is treated as 1.
func New(maxConcurrent int) *Dispatcher {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	root, stop := context.WithCancel(context.Background())
	return &Dispatcher{
		sem:      semaphore.NewWeighted(int64(maxConcurrent)),
		bufSize:  queueBuffer,
		chats:    make(map[int64]*chatQueue),
		rootCtx:  root,
		rootStop: stop,
	}
}

// Submit enqueues run for chatID. Jobs for the same chat execute serially in the
// order submitted; jobs for different chats run in parallel up to the global
// cap. The ctx passed to run is cancelable via Cancel(chatID) and via Shutdown.
//
// Submit is effectively non-blocking until the per-chat buffer (queueBuffer)
// fills, at which point it applies back-pressure and blocks until a slot frees.
// If the dispatcher is (or becomes) closed the job is cleanly dropped — both the
// initial fast-path check and the enqueue itself bail out on rootCtx.Done(), so
// Submit can never send on a queue whose worker has gone away.
func (d *Dispatcher) Submit(chatID int64, run func(ctx context.Context)) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	q := d.chats[chatID]
	if q == nil {
		q = &chatQueue{ch: make(chan job, d.bufSize)}
		d.chats[chatID] = q
		d.wg.Add(1)
		go d.worker(q)
	}
	d.mu.Unlock()

	// Derive each job's ctx from the dispatcher root so Shutdown can cancel
	// everything; Cancel swaps in a fresh cancel just before the job runs.
	//
	// The send and the shutdown signal are selected together: if Shutdown cancels
	// rootCtx while we are blocked on a full buffer (or racing the closed-check
	// above), the job is dropped instead of risking a send on an abandoned queue.
	// The channel is never closed, so this select can only enqueue or drop.
	select {
	case q.ch <- job{ctx: d.rootCtx, run: run}:
	case <-d.rootCtx.Done():
	}
}

// worker drains a single chat's queue, running one job at a time. Serial
// execution within the chat falls out of the single consumer; the global cap is
// enforced by acquiring a semaphore slot around each job's start.
//
// The queue channel is never closed; the worker exits when Shutdown cancels
// rootCtx. Pending buffered jobs are abandoned (their ctx, derived from rootCtx,
// is already cancelled). Draining queued jobs first would not help: rootCtx is
// cancelled, so each would observe cancellation immediately.
func (d *Dispatcher) worker(q *chatQueue) {
	defer d.wg.Done()
	for {
		select {
		case j := <-q.ch:
			d.runOne(q, j)
		case <-d.rootCtx.Done():
			return
		}
	}
}

// runOne acquires a global slot, installs the per-chat cancel hook, runs the
// job, and releases the slot. The semaphore is always released via defer so a
// panicking job cannot leak a slot.
func (d *Dispatcher) runOne(q *chatQueue, j job) {
	// Block until a global slot is free (parallel across chats, capped). If the
	// dispatcher is shutting down, abandon the job.
	if err := d.sem.Acquire(d.rootCtx, 1); err != nil {
		return
	}
	defer d.sem.Release(1)

	jobCtx, cancel := context.WithCancel(j.ctx)
	q.mu.Lock()
	q.cancel = cancel
	q.mu.Unlock()

	// The chat is serial (single worker), so q.cancel is only ever cleared by a
	// concurrent Cancel for this chat — never replaced by another job. Always
	// release this job's context and clear the hook on return.
	defer func() {
		cancel()
		q.mu.Lock()
		q.cancel = nil
		q.mu.Unlock()
	}()

	j.run(jobCtx)
}

// Cancel clears a chat's whole lane: it discards every queued-but-not-started job
// for chatID AND cancels the job currently running for it (if any) by cancelling
// its context. After Cancel, nothing the chat had pending will run — this is what
// makes Stop purge the queue and an edit-supersede drop the stale queued job
// instead of running it as a duplicate. It is a no-op for an unknown or idle chat.
// Best-effort at the queued→running boundary: a job the worker has just dequeued
// but not yet registered for cancellation may still run.
func (d *Dispatcher) Cancel(chatID int64) {
	d.mu.Lock()
	q := d.chats[chatID]
	d.mu.Unlock()
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	// Discard all queued-but-not-started jobs (non-blocking) so the lane is cleared,
	// not just the in-flight job. The worker receives from the same channel without
	// holding q.mu; a channel value goes to exactly one receiver, so draining here
	// can neither double-run a job nor steal one the worker is already executing.
drain:
	for {
		select {
		case <-q.ch:
		default:
			break drain
		}
	}
	if q.cancel != nil {
		q.cancel()
		q.cancel = nil
	}
}

// Shutdown stops accepting new jobs and cancels every in-flight job, then waits
// for the per-chat workers to drain until ctx is done. It is safe to call more
// than once (the closed flag guards a second pass); later Submits are dropped.
// A simple version sufficient for Stage 9's graceful shutdown.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	chats := make([]*chatQueue, 0, len(d.chats))
	for _, q := range d.chats {
		chats = append(chats, q)
	}
	d.mu.Unlock()

	// Cancel rootCtx so every worker exits via its select and every in-flight
	// job's ctx (derived from rootCtx) is cancelled. Channels are never closed,
	// which keeps a concurrent Submit's send safe. Also fire the per-chat cancel
	// hooks so a job blocked on ctx.Done() unblocks promptly.
	d.rootStop()
	for _, q := range chats {
		q.mu.Lock()
		if q.cancel != nil {
			q.cancel()
			q.cancel = nil
		}
		q.mu.Unlock()
	}

	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the dispatcher without waiting for the drain, cancelling in-flight
// jobs. It is a convenience over Shutdown for callers that don't need to block.
func (d *Dispatcher) Close() {
	// An already-cancelled context makes Shutdown return immediately after it has
	// signalled cancellation, without blocking on the worker drain.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d.Shutdown(cancelled)
}
