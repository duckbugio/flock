// Package dispatch provides per-chat job serialization with a global
// concurrency cap. Each chat gets its own goroutine + buffered channel so jobs
// for one chat run serially in FIFO order; a shared weighted semaphore gates job
// *starts* so different chats run in parallel up to a configured cap. This
// replaces Stage 2's single per-process serial guard with real per-chat
// isolation and parallelism (plan §7.4).
package dispatch

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// ErrShutdown is the cancellation cause set on a job's context when the
// dispatcher is shutting down the whole process (Shutdown's post-drain cancel or
// Close), as opposed to a per-chat Cancel (Stop / edit-supersede). A run can
// distinguish the two via context.Cause(ctx): a shutdown-cancelled run was killed
// by a deploy/restart and should keep its interrupted-run marker for auto-resume,
// while a user-Stopped run clears it.
var ErrShutdown = errors.New("dispatch: shutting down")

// job is one unit of work plus the per-chat cancelable context it runs under.
type job struct {
	// ctx is the job's payload (carried through the buffered channel), not a
	// per-call parameter — each queued job runs under its own cancelable context.
	//nolint:containedctx // ctx is the job's queued payload, not a per-call parameter.
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
// Known limitation: per-chat worker goroutines are never reaped while the
// dispatcher lives; a long-running process accumulates one idle goroutine per
// distinct chat ID. A future idle-reaper (close+delete a chat's queue after an
// idle timeout, recreating it on the next Submit) could bound this when chat
// churn warrants it.
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

	mu     sync.Mutex
	chats  map[string]*chatQueue
	closed bool
	// wg tracks per-chat worker goroutines. A worker stays alive while it is running
	// a job and exits once it is idle and the drain/rootCtx signal fires, so
	// wg.Wait() after closing drain blocks exactly until every in-flight job has
	// finished on its own — the graceful-drain wait.
	wg sync.WaitGroup
	// drain is closed by Shutdown to start the graceful-drain phase: workers stop
	// dequeuing NEW jobs but a job already running is left to finish on its own
	// (its context is NOT cancelled) until the drain deadline elapses.
	drain     chan struct{}
	drainOnce sync.Once
	//nolint:containedctx // root context held for the dispatcher's lifecycle; cancelled in Shutdown via rootStop.
	rootCtx  context.Context
	rootStop context.CancelCauseFunc
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
	root, stop := context.WithCancelCause(context.Background())
	return &Dispatcher{
		sem:      semaphore.NewWeighted(int64(maxConcurrent)),
		bufSize:  queueBuffer,
		chats:    make(map[string]*chatQueue),
		drain:    make(chan struct{}),
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
func (d *Dispatcher) Submit(chatID string, run func(ctx context.Context)) {
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
// The queue channel is never closed. The worker exits when Shutdown cancels
// rootCtx (the immediate Close path) OR when the graceful drain begins (d.drain
// closed): on drain it stops dequeuing NEW jobs and returns, leaving any job it
// is currently running to finish on its own (that job runs inside runOne, which
// has already returned to the loop only after the job completed). Pending
// buffered jobs are abandoned in both cases.
func (d *Dispatcher) worker(q *chatQueue) {
	defer d.wg.Done()
	for {
		// Prioritize the shutdown signals: once draining (or rootCtx cancelled), stop
		// dequeuing new jobs even if the buffer still holds some, so a graceful drain
		// does not start fresh work past the SIGTERM.
		select {
		case <-d.drain:
			return
		case <-d.rootCtx.Done():
			return
		default:
		}
		select {
		case j := <-q.ch:
			d.runOne(q, j)
		case <-d.drain:
			return
		case <-d.rootCtx.Done():
			return
		}
	}
}

// runOne acquires a global slot, installs the per-chat cancel hook, runs the
// job, and releases the slot. The semaphore is always released via defer so a
// panicking job cannot leak a slot. The worker stays alive (tracked on d.wg) for
// the whole call, so a graceful Shutdown's wg.Wait() blocks until the running job
// finishes on its own.
func (d *Dispatcher) runOne(q *chatQueue, j job) {
	// Block until a global slot is free (parallel across chats, capped). If the
	// dispatcher is shutting down (rootCtx cancelled), abandon the job.
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
func (d *Dispatcher) Cancel(chatID string) {
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

// Shutdown performs a graceful drain: it stops accepting and starting NEW jobs,
// then FIRST WAITS for already-running (in-flight) jobs to finish on their own —
// without cancelling them — until ctx is done. A job that completes within that
// window delivers its real result. Only jobs still running when ctx's deadline
// elapses are cancelled (the backstop), after which Shutdown waits a little more
// for them to unwind. It returns nil when every in-flight job finished on its own
// within the window, or ctx.Err() when the deadline forced a cancel.
//
// It is safe to call more than once (the closed flag guards a second pass); later
// Submits are dropped. Pending (queued-but-not-started) jobs are abandoned.
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

	// Signal the graceful drain: workers stop dequeuing NEW jobs (and abandon
	// queued ones) but leave any job they are currently running to finish on its
	// own. We deliberately do NOT cancel rootCtx or the per-chat cancel hooks here
	// — that is what lets an in-flight run complete and deliver its real answer.
	d.drainOnce.Do(func() { close(d.drain) })

	// Phase 1 — wait for in-flight jobs to finish on their own, up to the deadline.
	// A worker exits once it is idle and sees drain, so wg.Wait() unblocks exactly
	// when every currently-running job has completed (and the idle workers have
	// returned).
	inflightDone := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(inflightDone)
	}()
	select {
	case <-inflightDone:
		// Every in-flight job finished within the window; tear down the workers and
		// return cleanly.
		d.rootStop(ErrShutdown)
		return nil
	case <-ctx.Done():
		// Deadline elapsed with survivors still running.
	}

	// Phase 2 — cancel the survivors (rootCtx + per-chat hooks) so they unwind, then
	// give them a brief, bounded grace to deliver a terminal/persist a marker before
	// the caller exits the process. The grace is independent of the (now-expired)
	// drain ctx so a survivor's "Stopped"/marker write isn't itself cut off.
	//
	// Order matters: rootStop sets the shutdown cause (ErrShutdown) BEFORE the
	// per-chat cancels below, so every in-flight survivor observes ErrShutdown and
	// keeps its interrupted-run marker to drive auto-resume on restart. A user Stop
	// whose own per-chat cancel happens to land in the narrow window just before
	// this rootStop is instead seen as a clean context.Canceled (marker cleared, no
	// resume). That race is intentionally left unguarded: it is vanishingly rare,
	// user-initiated, and resolves in the safe direction — a run the user just
	// stopped should not auto-resume — so no locking or reordering is warranted.
	d.rootStop(ErrShutdown)
	for _, q := range chats {
		q.mu.Lock()
		if q.cancel != nil {
			q.cancel()
			q.cancel = nil
		}
		q.mu.Unlock()
	}

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), postCancelGrace)
	defer cancelGrace()
	select {
	case <-inflightDone:
	case <-graceCtx.Done():
	}
	return ctx.Err()
}

// postCancelGrace bounds how long Shutdown waits, AFTER cancelling survivors at
// the drain deadline, for those cancelled jobs to unwind and deliver a terminal
// message / persist their interrupted-run marker. It keeps total shutdown well
// under the Docker stop_grace_period so the process self-exits before SIGKILL.
const postCancelGrace = 10 * time.Second

// Close stops the dispatcher immediately, cancelling in-flight jobs without
// waiting for the graceful drain. It is the no-drain path for callers that don't
// need to block.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	chats := make([]*chatQueue, 0, len(d.chats))
	for _, q := range d.chats {
		chats = append(chats, q)
	}
	d.mu.Unlock()

	// Cancel everything at once: rootCtx unblocks the workers and cancels every
	// in-flight job's ctx; the per-chat hooks unblock a job parked on ctx.Done().
	// Also close drain so a later Shutdown is a no-op and workers always have an
	// exit signal. Channels are never closed, keeping a concurrent Submit safe.
	d.drainOnce.Do(func() { close(d.drain) })
	d.rootStop(ErrShutdown)
	for _, q := range chats {
		q.mu.Lock()
		if q.cancel != nil {
			q.cancel()
			q.cancel = nil
		}
		q.mu.Unlock()
	}
}
