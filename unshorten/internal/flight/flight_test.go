package flight

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

const abnormal = -1

func newGroup(maxKeys, maxPerHost int) *Group[int] {
	return New(maxKeys, maxPerHost, abnormal, nil)
}

// blocked returns an fn that counts its calls, blocks until release is
// closed and returns v.
func blocked(release <-chan struct{}, calls *atomic.Int32, v int) func() (int, func()) {
	return func() (int, func()) {
		calls.Add(1)
		<-release
		return v, nil
	}
}

func mustStart(t *testing.T, g *Group[int], key, host string, fn func() (int, func())) <-chan int {
	t.Helper()
	ch, ok := g.DoChan(key, host, fn)
	if !ok || ch == nil {
		t.Fatalf("DoChan(%q, %q) = %v, %v; want a channel", key, host, ch, ok)
	}
	return ch
}

func recv(t *testing.T, ch <-chan int) int {
	t.Helper()
	select {
	case v := <-ch:
		return v
	default:
		t.Fatal("no value delivered")
		return 0
	}
}

func TestFlightJoinCoalesces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(4, 4)
		release := make(chan struct{})
		var calls atomic.Int32
		const n = 10
		chans := []<-chan int{mustStart(t, g, "k", "h", blocked(release, &calls, 7))}
		for range n - 1 {
			chans = append(chans, mustStart(t, g, "k", "h", blocked(release, &calls, 99)))
		}
		synctest.Wait()
		if got := g.Joiners("k"); got != n {
			t.Errorf("Joiners = %d, want %d", got, n)
		}
		if total, forHost := g.InFlight("h"); total != 1 || forHost != 1 {
			t.Errorf("InFlight = %d, %d; want 1, 1 (joins add no key)", total, forHost)
		}
		close(release)
		synctest.Wait()
		for i, ch := range chans {
			if v := recv(t, ch); v != 7 {
				t.Errorf("chan %d got %d, want 7", i, v)
			}
		}
		if c := calls.Load(); c != 1 {
			t.Errorf("fn ran %d times, want 1", c)
		}
		if got := g.Joiners("k"); got != 0 {
			t.Errorf("Joiners after completion = %d, want 0", got)
		}
		if total, forHost := g.InFlight("h"); total != 0 || forHost != 0 {
			t.Errorf("InFlight after completion = %d, %d; want 0, 0", total, forHost)
		}
		if g.Joiners("unknown") != 0 {
			t.Error("Joiners of a key never started is not 0")
		}
	})
}

// TestFlightRestart: a key started again after its call completed is a new
// call, never a join of the finished one.
func TestFlightRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(1, 1)
		var calls atomic.Int32
		for i := range 3 {
			ch := mustStart(t, g, "k", "h", func() (int, func()) { return int(calls.Add(1)), nil })
			synctest.Wait()
			if v := recv(t, ch); v != i+1 {
				t.Fatalf("round %d got %d, want %d", i, v, i+1)
			}
		}
	})
}

// TestFlightBoundAtomic starts 64 distinct keys at once and checks that
// exactly the bound starts: once over distinct hosts (the global bound) and
// once on one host (the per-host bound). Run it under -race.
func TestFlightBoundAtomic(t *testing.T) {
	for _, tc := range []struct {
		name                string
		maxKeys, maxPerHost int
		host                func(i int) string
	}{
		{"distinct hosts", 4, 64, func(i int) string { return fmt.Sprintf("h%d", i) }},
		{"same host", 64, 4, func(int) string { return "h" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				g := newGroup(tc.maxKeys, tc.maxPerHost)
				release := make(chan struct{})
				var calls, started, refused atomic.Int32
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := range 64 {
					wg.Go(func() {
						<-start
						ch, ok := g.DoChan(fmt.Sprintf("k%d", i), tc.host(i), blocked(release, &calls, i))
						if ok != (ch != nil) {
							t.Errorf("DoChan returned ch=%v ok=%v", ch, ok)
						}
						if ok {
							started.Add(1)
						} else {
							refused.Add(1)
						}
					})
				}
				close(start)
				wg.Wait()
				synctest.Wait()
				if s, r, c := started.Load(), refused.Load(), calls.Load(); s != 4 || r != 60 || c != 4 {
					t.Errorf("started %d, refused %d, fn calls %d; want 4, 60, 4", s, r, c)
				}
				if total, _ := g.InFlight("h"); total != 4 {
					t.Errorf("InFlight total = %d, want 4", total)
				}
				close(release)
				g.Wait()
				if total, forHost := g.InFlight("h"); total != 0 || forHost != 0 {
					t.Errorf("InFlight after drain = %d, %d; want 0, 0", total, forHost)
				}
			})
		})
	}
}

// TestFlightPerHostBoundNoStarvation: a host at its bound is refused new
// keys but can still be joined, other hosts still start and finish while it
// is stalled, and its slots come back when its calls finish. The global
// bound refuses a new host only once every key is taken.
func TestFlightPerHostBoundNoStarvation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(5, 2)
		stall := make(chan struct{})
		var stalled atomic.Int32
		a1 := mustStart(t, g, "a1", "a", blocked(stall, &stalled, 1))
		a2 := mustStart(t, g, "a2", "a", blocked(stall, &stalled, 2))
		if ch, ok := g.DoChan("a3", "a", blocked(stall, &stalled, 3)); ok || ch != nil {
			t.Fatal("a third key for host a started past maxPerHost 2")
		}
		join := mustStart(t, g, "a1", "a", blocked(stall, &stalled, 0)) // joining is always allowed

		// Host b is not starved by a: it starts, finishes and starts again.
		for i := range 3 {
			ch := mustStart(t, g, fmt.Sprintf("b%d", i), "b", func() (int, func()) { return 10 + i, nil })
			synctest.Wait()
			if v := recv(t, ch); v != 10+i {
				t.Fatalf("b%d got %d", i, v)
			}
		}
		if total, forHost := g.InFlight("a"); total != 2 || forHost != 2 {
			t.Fatalf("InFlight(a) = %d, %d; want 2, 2", total, forHost)
		}

		// Fill the global bound: b and c take the other three keys.
		hold := make(chan struct{})
		var held atomic.Int32
		mustStart(t, g, "b-hold", "b", blocked(hold, &held, 0))
		mustStart(t, g, "c1", "c", blocked(hold, &held, 0))
		mustStart(t, g, "c2", "c", blocked(hold, &held, 0))
		if _, ok := g.DoChan("d1", "d", blocked(hold, &held, 0)); ok {
			t.Fatal("a sixth key started past maxKeys 5")
		}
		mustStart(t, g, "c1", "c", blocked(hold, &held, 0)) // a join at the global bound is allowed
		close(hold)
		synctest.Wait()
		mustStart(t, g, "d1", "d", func() (int, func()) { return 0, nil })

		close(stall)
		synctest.Wait()
		for _, ch := range []<-chan int{a1, join} {
			if v := recv(t, ch); v != 1 {
				t.Errorf("a1 waiter got %d, want 1", v)
			}
		}
		if v := recv(t, a2); v != 2 {
			t.Errorf("a2 got %d, want 2", v)
		}
		if n := stalled.Load(); n != 2 {
			t.Errorf("host a fns ran %d times, want 2", n)
		}
		mustStart(t, g, "a3", "a", func() (int, func()) { return 3, nil }) // a's slots came back
		g.Wait()
	})
}

func TestFlightPanic(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		check func(r any) bool
	}{
		{"value", "secret https://t.co/x", func(r any) bool { return r == "secret https://t.co/x" }},
		{"nil", nil, func(r any) bool {
			var pe *runtime.PanicNilError
			e, ok := r.(error)
			return ok && errors.As(e, &pe)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				type report struct {
					key, host string
					r         any
					stack     string
				}
				var reports []report
				g := New(4, 4, abnormal, func(key, host string, r any, stack []byte) {
					reports = append(reports, report{key, host, r, string(stack)})
				})
				release := make(chan struct{})
				ch := mustStart(t, g, "k", "h", func() (int, func()) {
					<-release
					panic(tc.value)
				})
				join := mustStart(t, g, "k", "h", nil)
				close(release)
				g.Wait()
				for _, c := range []<-chan int{ch, join} {
					if v := recv(t, c); v != abnormal {
						t.Errorf("got %d, want abnormal %d", v, abnormal)
					}
				}
				if total, forHost := g.InFlight("h"); total != 0 || forHost != 0 {
					t.Errorf("InFlight = %d, %d; want 0, 0", total, forHost)
				}
				if len(reports) != 1 {
					t.Fatalf("onPanic called %d times, want 1", len(reports))
				}
				rep := reports[0]
				if rep.key != "k" || rep.host != "h" || !tc.check(rep.r) {
					t.Errorf("onPanic(%q, %q, %#v); want k, h and the recovered value", rep.key, rep.host, rep.r)
				}
				if !strings.Contains(rep.stack, "flight_test.go") {
					t.Errorf("stack does not reach the panicking fn:\n%s", rep.stack)
				}
				mustStart(t, g, "k", "h", func() (int, func()) { return 1, nil }) // the key was released
				g.Wait()
			})
		})
	}
}

func TestFlightGoexit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		panics := 0
		g := New(1, 1, abnormal, func(string, string, any, []byte) { panics++ })
		afterRan := false
		ch := mustStart(t, g, "k", "h", func() (int, func()) {
			runtime.Goexit()
			return 1, func() { afterRan = true }
		})
		g.Wait()
		if v := recv(t, ch); v != abnormal {
			t.Errorf("got %d, want abnormal", v)
		}
		if panics != 0 || afterRan {
			t.Errorf("onPanic calls %d, after ran %v; want 0, false", panics, afterRan)
		}
		if total, _ := g.InFlight("h"); total != 0 {
			t.Errorf("InFlight = %d, want 0", total)
		}
		mustStart(t, g, "k2", "h", func() (int, func()) { return 1, nil })
		g.Wait()
	})
}

// TestFlightPanicOrder: onPanic runs only after the key is released and
// abnormal delivered, so one that blocks or panics neither holds waiters nor
// crashes the process.
func TestFlightPanicOrder(t *testing.T) {
	t.Run("blocking onPanic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			unblock := make(chan struct{})
			entered := false
			g := New(1, 1, abnormal, func(string, string, any, []byte) {
				entered = true
				<-unblock
			})
			ch := mustStart(t, g, "k", "h", func() (int, func()) { panic("boom") })
			join := mustStart(t, g, "k", "h", nil)
			synctest.Wait() // run's goroutine is now blocked in onPanic
			if !entered {
				t.Fatal("onPanic not called")
			}
			for _, c := range []<-chan int{ch, join} {
				if v := recv(t, c); v != abnormal {
					t.Errorf("got %d, want abnormal", v)
				}
			}
			if total, _ := g.InFlight("h"); total != 0 {
				t.Errorf("InFlight = %d while onPanic blocks; want 0", total)
			}
			mustStart(t, g, "k2", "h", func() (int, func()) { return 1, nil }) // the bounds came back

			waited := false
			go func() { g.Wait(); waited = true }()
			synctest.Wait()
			if waited {
				t.Error("Wait returned while onPanic was running")
			}
			close(unblock)
			synctest.Wait()
			if !waited {
				t.Error("Wait did not return after onPanic finished")
			}
		})
	})
	t.Run("panicking onPanic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := New(1, 1, abnormal, func(string, string, any, []byte) { panic("handler") })
			ch := mustStart(t, g, "k", "h", func() (int, func()) { panic("boom") })
			g.Wait()
			if v := recv(t, ch); v != abnormal {
				t.Errorf("got %d, want abnormal", v)
			}
		})
	})
	t.Run("Goexit in onPanic", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			g := New(1, 1, abnormal, func(string, string, any, []byte) { runtime.Goexit() })
			ch := mustStart(t, g, "k", "h", func() (int, func()) { panic("boom") })
			g.Wait() // returns: Goexit still ends the call's activity
			if v := recv(t, ch); v != abnormal {
				t.Errorf("got %d, want abnormal", v)
			}
		})
	})
}

// TestFlightAfter: after runs once the key is released and the value is
// delivered, on fn's goroutine; Wait waits for it; a panic or Goexit in it is
// contained.
func TestFlightAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(1, 1)
		unblock := make(chan struct{})
		var ch, join <-chan int
		var sawTotal, sawJoiners, sawDelivered = -1, -1, false
		fn := func() (int, func()) {
			return 5, func() {
				sawTotal, _ = g.InFlight("h")
				sawJoiners = g.Joiners("k")
				sawDelivered = len(ch) == 1 && len(join) == 1
				<-unblock
			}
		}
		ch = mustStart(t, g, "k", "h", func() (int, func()) {
			<-unblock // hold the key until join is registered
			return fn()
		})
		join = mustStart(t, g, "k", "h", nil)
		unblock <- struct{}{}
		synctest.Wait() // after is now blocked
		if sawTotal != 0 || sawJoiners != 0 || !sawDelivered {
			t.Errorf("in after: InFlight %d, Joiners %d, delivered %v; want 0, 0, true", sawTotal, sawJoiners, sawDelivered)
		}
		// A blocked after holds no key: the bound admits a new one.
		mustStart(t, g, "k2", "h", func() (int, func()) { return 0, nil })

		waited := false
		go func() { g.Wait(); waited = true }()
		synctest.Wait()
		if waited {
			t.Error("Wait returned while after was running")
		}
		close(unblock)
		synctest.Wait()
		if !waited {
			t.Error("Wait did not return after the after func finished")
		}
		for _, c := range []<-chan int{ch, join} {
			if v := recv(t, c); v != 5 {
				t.Errorf("got %d, want 5", v)
			}
		}

		for _, bad := range []func(){func() { panic("after") }, runtime.Goexit} {
			c := mustStart(t, g, "k3", "h", func() (int, func()) { return 6, bad })
			g.Wait()
			if v := recv(t, c); v != 6 {
				t.Errorf("got %d, want 6", v)
			}
			mustStart(t, g, "k4", "h", func() (int, func()) { return 0, nil })
			g.Wait()
		}
	})
}

func TestFlightWaitIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(1, 1)
		g.Wait() // nothing in flight: returns at once
		release := make(chan struct{})
		var calls atomic.Int32
		mustStart(t, g, "k", "h", blocked(release, &calls, 0))
		waited := false
		go func() { g.Wait(); waited = true }()
		synctest.Wait()
		if waited {
			t.Fatal("Wait returned while a key was in flight")
		}
		close(release)
		synctest.Wait()
		if !waited {
			t.Fatal("Wait did not return once the key finished")
		}
	})
}

func TestFlightRefusedRunsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGroup(1, 1)
		release := make(chan struct{})
		var calls atomic.Int32
		mustStart(t, g, "k", "h", blocked(release, &calls, 0))
		if ch, ok := g.DoChan("other", "h2", blocked(release, &calls, 0)); ok || ch != nil {
			t.Fatal("started past maxKeys 1")
		}
		synctest.Wait()
		if c := calls.Load(); c != 1 {
			t.Errorf("fn ran %d times, want 1 (a refused fn must not run)", c)
		}
		close(release)
		g.Wait()
	})
}

func TestFlightNewPanics(t *testing.T) {
	for _, b := range [][2]int{{0, 1}, {1, 0}, {-1, 4}, {4, -1}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%d, %d) did not panic", b[0], b[1])
				}
			}()
			New(b[0], b[1], 0, nil)
		}()
	}
}
