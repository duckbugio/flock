package dispatch

import (
	"context"
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

	for _, id := range []int64{1, 2} {
		d.Submit(id, func(ctx context.Context) {
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

	d.Submit(1, func(_ context.Context) {
		<-first // hold until the test lets the first finish
		mu.Lock()
		order = append(order, 1)
		mu.Unlock()
	})
	d.Submit(1, func(_ context.Context) {
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
		d.Submit(int64(i), func(_ context.Context) {
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

	d.Submit(42, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
	})

	recv(t, started, "job did not start")
	d.Cancel(42)
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
	d.Submit(7, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	})
	// Jobs 2 and 3 queue behind it on the SAME chat (serial within a chat).
	for i := 0; i < 2; i++ {
		d.Submit(7, func(_ context.Context) { queuedRan.Store(true) })
	}

	recv(t, started, "first job did not start")
	d.Cancel(7) // cancels job 1 AND drains the two queued jobs

	// Give a drained job ample time to (wrongly) run after the lane frees.
	time.Sleep(100 * time.Millisecond)
	if queuedRan.Load() {
		t.Fatal("a queued job ran after Cancel — the lane was not drained")
	}
}

// TestCancelUnknownChatNoop ensures Cancel on an idle/unknown chat is harmless.
func TestCancelUnknownChatNoop(t *testing.T) {
	d := New(2)
	defer d.Close()
	d.Cancel(999) // must not panic
}

// TestShutdownDrains cancels the in-flight job and waits for workers to drain
// within the deadline.
func TestShutdownDrains(t *testing.T) {
	d := New(2)

	started := make(chan struct{})
	d.Submit(1, func(ctx context.Context) {
		close(started)
		<-ctx.Done() // exits only because Shutdown cancels the job
	})
	recv(t, started, "job did not start")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown did not drain in time: %v", err)
	}

	// Submitting after shutdown is a dropped no-op (must not panic or run).
	d.Submit(2, func(context.Context) { t.Fatal("job ran after shutdown") })
	time.Sleep(20 * time.Millisecond)
}

// TestSubmitRacesShutdown hammers the Submit||Shutdown window: many goroutines
// fire Submits (across several chats, with jobs that may fill the buffer) while
// another goroutine shuts the dispatcher down. The fix (never closing the queue
// channels; selecting the send against rootCtx.Done()) means no Submit can ever
// send on a closed/abandoned channel, so the only acceptable outcome is "no
// panic, every call returns". Run under -race -count=N to make the race likely.
func TestSubmitRacesShutdown(t *testing.T) {
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
					chatID := int64((base + j) % 4)
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
