package unshorten

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// waitFor runs l.Wait on a fresh background ctx and returns how long it took
// on the bubble's clock.
func waitFor(t *testing.T, l Limiter, host string) time.Duration {
	t.Helper()
	start := time.Now()
	if err := l.Wait(context.Background(), host); err != nil {
		t.Fatalf("Wait(%q) = %v", host, err)
	}
	return time.Since(start)
}

func TestHostLimiterBurstRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewHostLimiter(2, 4)
		for i := range 4 {
			if d := waitFor(t, l, "t.co"); d != 0 {
				t.Fatalf("burst request %d waited %v", i, d)
			}
		}
		for i := range 3 {
			if d := waitFor(t, l, "t.co"); d != 500*time.Millisecond {
				t.Fatalf("request %d past the burst waited %v, want 500ms", 5+i, d)
			}
		}
		// Another host has its own bucket.
		if d := waitFor(t, l, "amzn.to"); d != 0 {
			t.Errorf("another host waited %v", d)
		}
		// Idle time refills the burst, never beyond it.
		time.Sleep(time.Hour)
		for i := range 4 {
			if d := waitFor(t, l, "t.co"); d != 0 {
				t.Fatalf("refilled burst request %d waited %v", i, d)
			}
		}
		if d := waitFor(t, l, "t.co"); d != 500*time.Millisecond {
			t.Errorf("request past the refilled burst waited %v, want 500ms", d)
		}
	})
}

// TestHostLimiterDeadlineReject: a slot past ctx's deadline is refused at
// once and commits nothing, so 1,000 rejections leave the schedule as it
// was: the next request waits at most 1/perSecond.
func TestHostLimiterDeadlineReject(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewHostLimiter(2, 1)
		waitFor(t, l, "t.co")
		start := time.Now()
		for range 1_000 {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err := l.Wait(ctx, "t.co")
			cancel()
			if !errors.Is(err, errLimitDeadline) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Wait = %v, want errLimitDeadline only", err)
			}
		}
		if d := time.Since(start); d != 0 {
			t.Fatalf("rejections took %v, want immediate", d)
		}
		if d := waitFor(t, l, "t.co"); d != 500*time.Millisecond {
			t.Errorf("after the flood the next request waited %v, want 500ms", d)
		}
		// A deadline at or after the slot is accepted and waits for it.
		// (Exactly at the slot is accepted too, but the timer and the
		// deadline would then fire together.)
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		start = time.Now()
		if err := l.Wait(ctx, "t.co"); err != nil {
			t.Errorf("Wait with a deadline after the slot = %v", err)
		}
		if d := time.Since(start); d != 500*time.Millisecond {
			t.Errorf("waited %v, want 500ms", d)
		}
	})
}

// TestHostLimiterCancelReturnsToken: a Wait cancelled while it sleeps gives
// its slot back.
func TestHostLimiterCancelReturnsToken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewHostLimiter(1, 1)
		waitFor(t, l, "t.co")
		ctx, cancel := context.WithCancel(context.Background())
		var err error
		done := make(chan struct{})
		go func() { err = l.Wait(ctx, "t.co"); close(done) }()
		time.Sleep(100 * time.Millisecond)
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Wait = %v, want context.Canceled", err)
		}
		if d := waitFor(t, l, "t.co"); d != 900*time.Millisecond {
			t.Errorf("after the cancel the next request waited %v, want 900ms (the slot at 1s)", d)
		}

		// Two waiters: cancelling the later one first, then the earlier,
		// returns both slots. Cancelling the earlier one while the later
		// still waits returns nothing: the later one keeps its start time.
		time.Sleep(time.Hour)
		waitFor(t, l, "t.co") // the schedule now ends 1s ahead
		type waiter struct {
			cancel context.CancelFunc
			done   chan error
		}
		start := func() waiter {
			ctx, cancel := context.WithCancel(context.Background())
			w := waiter{cancel, make(chan error, 1)}
			go func() { w.done <- l.Wait(ctx, "t.co") }()
			synctest.Wait()
			return w
		}
		first, second := start(), start() // slots at +1s and +2s
		for _, w := range []waiter{second, first} {
			w.cancel()
			if err := <-w.done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled Wait = %v", err)
			}
		}
		if d := waitFor(t, l, "t.co"); d != time.Second {
			t.Errorf("after LIFO cancels the next request waited %v, want 1s (both slots back)", d)
		}
		first, second = start(), start() // slots at +1s and +2s
		first.cancel()
		if err := <-first.done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Wait = %v", err)
		}
		next := time.Now()
		ok := make(chan error, 1)
		go func() { ok <- l.Wait(context.Background(), "t.co") }()
		if err := <-second.done; err != nil {
			t.Fatalf("the later waiter failed: %v", err)
		}
		if d := time.Since(next); d != 2*time.Second {
			t.Errorf("the later waiter started after %v, want its 2s slot", d)
		}
		<-ok
		if d := time.Since(next); d != 3*time.Second {
			t.Errorf("a new request started after %v, want 3s: the earlier cancel must not let it share the 2s slot", d)
		}

		// A ctx already done consumes nothing.
		time.Sleep(time.Hour)
		dead, cancel2 := context.WithCancel(context.Background())
		cancel2()
		for range 10 {
			if err := l.Wait(dead, "t.co"); !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait on a done ctx = %v", err)
			}
		}
		if d := waitFor(t, l, "t.co"); d != 0 {
			t.Errorf("done-ctx Waits consumed tokens: next waited %v", d)
		}
	})
}

// TestHostLimiterConcurrent: many goroutines on one host; exactly burst of
// them pass without waiting, and the rest are spaced by the interval. Run it
// under -race.
func TestHostLimiterConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewHostLimiter(10, 3)
		start := time.Now()
		var immediate atomic.Int32
		var mu sync.Mutex
		var last time.Duration
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				if err := l.Wait(context.Background(), "t.co"); err != nil {
					t.Error(err)
				}
				d := time.Since(start)
				if d == 0 {
					immediate.Add(1)
				}
				mu.Lock()
				last = max(last, d)
				mu.Unlock()
			})
		}
		wg.Wait()
		if n := immediate.Load(); n != 3 {
			t.Errorf("%d requests passed at once, want the burst of 3", n)
		}
		if last != 1700*time.Millisecond {
			t.Errorf("last request started at %v, want 1.7s (17 × 100ms)", last)
		}
	})
}

func TestHostLimiterPrune(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := NewHostLimiter(1, 1).(*hostLimiter)
		size := func() int {
			l.mu.Lock()
			defer l.mu.Unlock()
			return len(l.tat)
		}
		for i := range limiterPruneAt {
			waitFor(t, l, fmt.Sprintf("h%d", i))
		}
		if n := size(); n != limiterPruneAt {
			t.Fatalf("%d hosts tracked, want %d", n, limiterPruneAt)
		}
		// Every host's schedule ends within a second; one keeps a future one.
		time.Sleep(2 * time.Second)
		waitFor(t, l, "h0")
		waitFor(t, l, "new")
		if n := size(); n != 2 {
			t.Errorf("after pruning %d hosts tracked, want 2 (h0 and new)", n)
		}
		if d := waitFor(t, l, "h0"); d != time.Second {
			t.Errorf("pruning dropped h0's live schedule: waited %v, want 1s", d)
		}

		// Now "new"'s schedule has ended and h0's has not. When nothing else
		// can be pruned the map still grows, and the next prune waits until
		// it has doubled: the live hosts are never dropped.
		for i := range 600 {
			waitFor(t, l, fmt.Sprintf("live%d", i))
		}
		l.mu.Lock()
		n, at := len(l.tat), l.pruneAt
		_, hasNew := l.tat["new"]
		l.mu.Unlock()
		if n != 601 || hasNew || at < n {
			t.Errorf("tracked %d hosts (new kept: %v) with pruneAt %d; want 601 (h0 and the live ones), false, ≥ 601", n, hasNew, at)
		}
	})
}

func TestNewHostLimiterPanics(t *testing.T) {
	for _, tc := range []struct {
		rate  float64
		burst int
	}{
		{math.NaN(), 1}, {math.Inf(1), 1}, {math.Inf(-1), 1}, {0, 1}, {-1, 1}, {1, 0}, {1, -1},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("NewHostLimiter(%v, %d) did not panic", tc.rate, tc.burst)
				}
			}()
			NewHostLimiter(tc.rate, tc.burst)
		}()
	}
}

// TestHostLimiterExtremes: rates and bursts at the edges of Duration's range
// neither panic nor overflow.
func TestHostLimiterExtremes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			rate  float64
			burst int
		}{
			{1e300, 1}, {math.SmallestNonzeroFloat64, 1}, {1e-300, math.MaxInt}, {1, math.MaxInt}, {1e300, math.MaxInt},
		} {
			l := NewHostLimiter(tc.rate, tc.burst)
			if d := waitFor(t, l, "h"); d != 0 {
				t.Errorf("NewHostLimiter(%v, %d): the first request waited %v", tc.rate, tc.burst, d)
			}
		}
		slow := NewHostLimiter(1e-300, 1)
		waitFor(t, slow, "h")
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		if err := slow.Wait(ctx, "h"); !errors.Is(err, errLimitDeadline) {
			t.Errorf("second request at a near-zero rate = %v, want errLimitDeadline", err)
		}
	})
}

func TestNoLimit(t *testing.T) {
	l := NoLimit()
	for range 1_000 {
		if err := l.Wait(context.Background(), "t.co"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := l.Wait(ctx, "t.co"); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait on a done ctx = %v, want context.Canceled", err)
	}
}
