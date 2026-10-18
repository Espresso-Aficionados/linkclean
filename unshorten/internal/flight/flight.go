// Package flight coalesces concurrent calls by key under two bounds: the
// number of keys in flight overall and per host. One mutex guards the key map
// and the per-host counts, so the bound test and the registration of a new
// key are atomic and concurrent starters cannot exceed either bound.
package flight

import (
	"fmt"
	"runtime/debug"
	"sync"
)

// Group coalesces calls by key. The zero value is not usable; use New.
type Group[V any] struct {
	maxKeys    int
	maxPerHost int
	abnormal   V
	onPanic    func(key, host string, r any, stack []byte)

	mu      sync.Mutex
	idle    sync.Cond // on mu; broadcast when active drops to 0
	calls   map[string]*call[V]
	perHost map[string]int // hosts with no key in flight have no entry
	active  int            // started calls whose goroutine has not finished (after and onPanic included)
}

// call is one key in flight.
type call[V any] struct {
	host  string
	chans []chan V // the starter's first; appended to under Group.mu until the key is removed
}

// New returns a Group that holds at most maxKeys keys in flight, and at most
// maxPerHost per host. It panics if either is below 1.
//
// abnormal is the value delivered when fn panics or calls runtime.Goexit: it
// is pre-set before fn runs. onPanic, if non-nil, is called after a
// recovered panic, once the key is released and abnormal delivered, inside
// its own recover, with the key and host the call was started with, the
// recovered value and the stack. A blocking or panicking onPanic therefore
// neither holds waiters nor crashes the process.
func New[V any](maxKeys, maxPerHost int, abnormal V, onPanic func(key, host string, r any, stack []byte)) *Group[V] {
	if maxKeys < 1 || maxPerHost < 1 {
		panic(fmt.Sprintf("flight: New(%d, %d): both bounds must be at least 1", maxKeys, maxPerHost))
	}
	g := &Group[V]{
		maxKeys:    maxKeys,
		maxPerHost: maxPerHost,
		abnormal:   abnormal,
		onPanic:    onPanic,
		calls:      map[string]*call[V]{},
		perHost:    map[string]int{},
	}
	g.idle.L = &g.mu
	return g
}

// DoChan joins the call in flight for key, or starts fn for key on a new
// goroutine, and returns a channel (buffered: a caller that stops receiving
// blocks nothing) that receives fn's value once. Joining is always allowed.
// Starting is refused (nil, false) when host already has maxPerHost keys in
// flight or the group has maxKeys; the per-host test is made first, under
// the same lock.
//
// fn returns its value and an optional after func. The key is removed and
// both counts released when fn returns, then the value is delivered to every
// joined channel, then after (if non-nil) runs on fn's goroutine inside its
// own recover. after does not run when fn panics or exits. A panic in after
// is recovered and dropped: the value has already been delivered.
func (g *Group[V]) DoChan(key, host string, fn func() (V, func())) (<-chan V, bool) {
	ch := make(chan V, 1)
	g.mu.Lock()
	if c, ok := g.calls[key]; ok {
		c.chans = append(c.chans, ch)
		g.mu.Unlock()
		return ch, true
	}
	if g.perHost[host] >= g.maxPerHost || len(g.calls) >= g.maxKeys {
		g.mu.Unlock()
		return nil, false
	}
	c := &call[V]{host: host, chans: []chan V{ch}}
	g.calls[key] = c
	g.perHost[host]++
	g.active++
	g.mu.Unlock()
	go g.run(key, c, fn)
	return ch, true
}

// run calls fn and settles the call. The first deferred function, which
// runs last, ends the call's activity for Wait, even when after or onPanic
// calls runtime.Goexit. The second releases the key and delivers on every
// path: normal return, panic and Goexit.
func (g *Group[V]) run(key string, c *call[V], fn func() (V, func())) {
	defer g.finish()
	v := g.abnormal // what a panic or Goexit in fn delivers: fn never assigned v
	var after func()
	normal := false
	defer func() {
		var r any
		var stack []byte
		if !normal {
			// recover is nil for runtime.Goexit; a panic(nil) recovers a
			// *runtime.PanicNilError.
			if r = recover(); r != nil {
				stack = debug.Stack()
			}
		}
		g.mu.Lock()
		delete(g.calls, key)
		if g.perHost[c.host]--; g.perHost[c.host] <= 0 {
			delete(g.perHost, c.host)
		}
		chans := c.chans // no further joins: the key is gone
		g.mu.Unlock()
		for _, ch := range chans {
			ch <- v // buffered, one value per channel: never blocks
		}
		switch {
		case normal && after != nil:
			protect(after)
		case r != nil && g.onPanic != nil:
			protect(func() { g.onPanic(key, c.host, r, stack) })
		}
	}()
	v, after = fn()
	normal = true
}

// finish marks one started call as fully done and wakes Wait when none is
// left.
func (g *Group[V]) finish() {
	g.mu.Lock()
	g.active--
	if g.active == 0 {
		g.idle.Broadcast()
	}
	g.mu.Unlock()
}

// protect calls f and drops any panic it raises.
func protect(f func()) {
	defer func() { _ = recover() }()
	f()
}

// InFlight returns the number of keys in flight overall and for host.
func (g *Group[V]) InFlight(host string) (total, forHost int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls), g.perHost[host]
}

// Joiners returns the number of channels waiting on key's call, the starter's
// included; 0 when key is not in flight. Tests use it as a barrier.
func (g *Group[V]) Joiners(key string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.calls[key]; ok {
		return len(c.chans)
	}
	return 0
}

// Wait blocks until no key is in flight and no after func or onPanic call is
// running. Tests use it to drain detached calls before they end. A call that
// never returns, or an onPanic that blocks, blocks Wait too.
func (g *Group[V]) Wait() {
	g.mu.Lock()
	for g.active > 0 {
		g.idle.Wait()
	}
	g.mu.Unlock()
}
