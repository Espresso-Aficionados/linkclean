// Package flight coalesces concurrent calls by key under two bounds: the
// number of keys in flight overall and per host. One mutex guards the key map
// and the per-host counts, so the bound test and the registration of a new
// key are atomic and concurrent starters cannot exceed either bound.
package flight

// Group coalesces calls by key. The zero value is not usable; use New.
type Group[V any] struct{}

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
	panic("flight: not implemented")
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
// own recover. after does not run when fn panics or exits.
func (g *Group[V]) DoChan(key, host string, fn func() (V, func())) (<-chan V, bool) {
	panic("flight: not implemented")
}

// InFlight returns the number of keys in flight overall and for host.
func (g *Group[V]) InFlight(host string) (total, forHost int) {
	panic("flight: not implemented")
}

// Joiners returns the number of channels waiting on key's call, the starter's
// included; 0 when key is not in flight. Tests use it as a barrier.
func (g *Group[V]) Joiners(key string) int {
	panic("flight: not implemented")
}

// Wait blocks until no key is in flight and no after func is running. Tests
// use it to drain detached calls before they end.
func (g *Group[V]) Wait() {
	panic("flight: not implemented")
}
