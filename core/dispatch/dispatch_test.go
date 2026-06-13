//nolint:testpackage // intentionally whitebox to test unexported dispatch routing internals
package dispatch

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recv waits for a value on ch up to a deadline, failing the test on timeout.
func recv(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout: %s", msg)
	}
}

// TestParallelAcrossChats asserts two different chats run concurrently: both
// reach a barrier before either is released, proving overlap (AC2a).
func TestParallelAcrossChats(t *testing.T) {
	d := New(4)
	defer d.Close()

	entered := make(chan struct{}, 2)
	release := make(chan struct{})

	for _, id := range []string{"1", "2"} {
		d.Submit(id, func(_ context.Context) {
			entered <- struct{}{}
			<-release
		})
	}

	// Both jobs must enter before either exits → genuine overlap.
	recv(t, entered, "first job did not start")
	recv(t, entered, "second chat did not run concurrently")
	close(release)
}

// TestSerialWithinChat asserts two jobs for the SAME chat run one after another:
// the second starts only after the first finishes (AC2b).
func TestSerialWithinChat(t *testing.T) {
	d := New(4)
	defer d.Close()

	var order []int
	var mu sync.Mutex
	first := make(chan struct{})
	secondStarted := make(chan struct{})

	d.Submit("1", func(_ context.Context) {
		<-first // hold until the test lets the first finish
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	d.Submit("1", func(_ context.Context) {
		mu.Lock()
		order = append(order, 2)
		mu.Unlock()
		close(secondStarted)
	})

	// Give the second job a chance to (wrongly) start while the first is blocked.
	select {
	case <-secondStarted:
		t.Fatal("second job started before the first finished — not serial")
	case <-time.After(50 * time.Millisecond):
	}

	close(first)
	recv(t, secondStarted, "second job never ran")

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Fatalf("jobs ran out of order: %v", order)
	}
}

// TestGlobalCap asserts the global cap truly limits concurrently-running jobs to
// at most N, using an atomic counter and a max-observed assertion (AC2c).
func TestGlobalCap(t *testing.T) {
	const maxN = 2
	const chats = 6
	d := New(maxN)
	defer d.Close()

	var running, maxSeen atomic.Int64
	var wg sync.WaitGroup
	release := make(chan struct{})

	wg.Add(chats)
	for i := 0; i < chats; i++ {
		d.Submit(strconv.Itoa(i), func(_ context.Context) {
			defer wg.Done()
			cur := running.Add(1)
			// Track the high-water mark of concurrent jobs.
			for {
				m := maxSeen.Load()
				if cur <= m || maxSeen.CompareAndSwap(m, cur) {
					break
				}
			}
			<-release
			running.Add(-1)
		})
	}

	// Let the capped set sit at the barrier, then release and drain.
	time.Sleep(100 * time.Millisecond)
	if got := maxSeen.Load(); got > maxN {
		t.Fatalf("observed %d concurrent jobs, cap is %d", got, maxN)
	}
	close(release)
	wg.Wait()

	if got := maxSeen.Load(); got == 0 {
		t.Fatal("no jobs ran")
	}
}

// TestCancelStopsInFlight asserts Cancel(chatID) cancels the running job's ctx,
// which the job observes via ctx.Done() (AC2d).
func TestCancelStopsInFlight(t *testing.T) {
	d := New(4)
	defer d.Close()

	started := make(chan struct{})
	cancelled := make(chan struct{})

	d.Submit("42", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})

	recv(t, started, "job did not start")
	d.Cancel("42")
	recv(t, cancelled, "Cancel did not cancel the in-flight job")
}

// TestCancelDrainsQueued asserts Cancel discards the chat's queued-but-not-started
// jobs, not just the in-flight one: a job waiting behind the running one never runs
// after Cancel. This is what lets Stop purge the whole lane and an edit-supersede
// drop the stale queued job instead of duplicating it.
func TestCancelDrainsQueued(t *testing.T) {
	d := New(4)
	defer d.Close()

	started := make(chan struct{})
	var queuedRan atomic.Bool

	// Job 1 holds the (serial) lane open until its ctx is cancelled.
	d.Submit("7", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	})
	// Jobs 2 and 3 queue behind it on the SAME chat (serial within a chat).
	for i := 0; i < 2; i++ {
		d.Submit("7", func(_ context.Context) { queuedRan.Store(true) })
	}

	recv(t, started, "first job did not start")
	d.Cancel("7") // cancels job 1 AND drains the two queued jobs

	// Give a drained job ample time to (wrongly) run after the lane frees.
	time.Sleep(100 * time.Millisecond)
	if queuedRan.Load() {
		t.Fatal("a queued job ran after Cancel — the lane was not drained")
	}
}

// TestCancelUnknownChatNoop ensures Cancel on an idle/unknown chat is harmless.
func TestCancelUnknownChatNoop(_ *testing.T) {
	d := New(2)
	defer d.Close()
	d.Cancel("999") // must not panic
}

// TestShutdownDrains drives a job that only exits on cancellation through the
// graceful drain: it survives the (short) drain window, gets cancelled at the
// deadline, and Shutdown reports the deadline error after the survivor unwinds.
// Submits after shutdown are dropped no-ops.
func TestShutdownDrains(t *testing.T) {
	d := New(2)

	started := make(chan struct{})
	d.Submit("1", func(ctx context.Context) {
		close(started)
		<-ctx.Done() // exits only because Shutdown cancels the job at the deadline
	})
	recv(t, started, "job did not start")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown returned nil; the survivor outlived the drain window and should report the deadline")
	}

	// Submitting after shutdown is a dropped no-op (must not panic or run).
	d.Submit("2", func(context.Context) { t.Fatal("job ran after shutdown") })
	time.Sleep(20 * time.Millisecond)
}

// TestShutdownWaitsForInFlightThenCancels asserts the graceful-drain contract:
// Shutdown first WAITS for in-flight jobs to finish on their own (without
// cancelling them) up to the drain deadline. A job that finishes within the
// window runs to completion uncancelled, and Shutdown blocks until it returns.
func TestShutdownWaitsForInFlightThenCancels(t *testing.T) {
	d := New(2)

	started := make(chan struct{})
	var cancelled atomic.Bool
	var completed atomic.Bool

	// A short job (well under the drain window): it must run to completion WITHOUT
	// observing a cancelled context.
	d.Submit("1", func(ctx context.Context) {
		close(started)
		select {
		case <-time.After(100 * time.Millisecond):
			completed.Store(true)
		case <-ctx.Done():
			cancelled.Store(true)
		}
	})
	recv(t, started, "job did not start")

	// Generous drain window so the 100ms job finishes inside it. Shutdown must
	// block until the job returns on its own.
	drainStart := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown returned error: %v", err)
	}

	if cancelled.Load() {
		t.Fatal("in-flight job was cancelled during the drain window — it should have been left to finish")
	}
	if !completed.Load() {
		t.Fatal("in-flight job did not complete on its own before Shutdown returned")
	}
	// Shutdown must have blocked for roughly the job's duration (not returned
	// immediately by cancelling).
	if elapsed := time.Since(drainStart); elapsed < 80*time.Millisecond {
		t.Fatalf("Shutdown returned in %v, expected it to block until the job finished", elapsed)
	}
}

// TestShutdownCancelsSurvivorsAfterDrain asserts that a job still running when the
// drain deadline elapses gets its context cancelled (the backstop), and Shutdown
// reports the deadline error rather than hanging forever.
func TestShutdownCancelsSurvivorsAfterDrain(t *testing.T) {
	d := New(2)

	started := make(chan struct{})
	cancelled := make(chan struct{})

	// A long job that only exits when its context is cancelled. The drain window is
	// short, so it must be cancelled at the deadline.
	d.Submit("1", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})
	recv(t, started, "job did not start")

	// Short drain window: the job outlives it and must be cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := d.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown returned nil, want a deadline error when a survivor had to be cancelled")
	}
	// The survivor's context must have been cancelled at the deadline so it unwinds.
	recv(t, cancelled, "survivor job was not cancelled after the drain deadline")
}

// TestCloseIsImmediate asserts Close cancels in-flight jobs and returns without
// waiting for the drain window (the no-drain path used by callers that don't
// block).
func TestCloseIsImmediate(t *testing.T) {
	d := New(2)

	started := make(chan struct{})
	cancelled := make(chan struct{})
	d.Submit("1", func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})
	recv(t, started, "job did not start")

	done := make(chan struct{})
	go func() {
		d.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not return promptly — it should not wait for a drain")
	}
	recv(t, cancelled, "Close did not cancel the in-flight job")
}

// TestSubmitRacesShutdown hammers the Submit||Shutdown window: many goroutines
// fire Submits (across several chats, with jobs that may fill the buffer) while
// another goroutine shuts the dispatcher down. The fix (never closing the queue
// channels; selecting the send against rootCtx.Done()) means no Submit can ever
// send on a closed/abandoned channel, so the only acceptable outcome is "no
// panic, every call returns". Run under -race -count=N to make the race likely.
func TestSubmitRacesShutdown(_ *testing.T) {
	// Repeat internally so a single `go test -race` run still hits the window
	// many times even without -count.
	for iter := 0; iter < 50; iter++ {
		d := New(4)

		const submitters = 16
		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(submitters)
		for i := 0; i < submitters; i++ {
			go func(base int) {
				defer wg.Done()
				<-start // line everyone up so Submits and Shutdown truly overlap
				for j := 0; j < 50; j++ {
					// A few chat IDs so buffers fill (back-pressure path) and
					// new workers spin up concurrently with shutdown.
					chatID := strconv.Itoa((base + j) % 4)
					d.Submit(chatID, func(ctx context.Context) {
						<-ctx.Done() // block until cancelled; exercises the full buffer
					})
				}
			}(i)
		}

		// Shut down concurrently with the Submit storm.
		var shutWG sync.WaitGroup
		shutWG.Add(1)
		go func() {
			defer shutWG.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = d.Shutdown(ctx)
		}()

		close(start) // fire everything at once
		wg.Wait()
		shutWG.Wait()

		// Double-Close / Shutdown after Shutdown must stay safe.
		d.Close()
	}
}
