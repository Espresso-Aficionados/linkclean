// Package dialhook holds the two test seams of unshorten's guarded dialer.
// Both are nil in production, where each dial pays one atomic load and a nil
// check. Only unshorten's tests set them; the package is internal, so no
// consumer can.
//
//   - Resolve maps a dial address (host, port) to the address actually dialed.
//     When it is set, the guarded dialer calls it before any DNS lookup, and
//     an unmapped address (ok false) fails the dial with ErrUnmapped.
//   - AllowAddr exempts exactly the addresses it reports true for from the
//     dial guard's IP and port checks (the test listeners on loopback). Every
//     other address still goes through the guard.
//
// The setters are atomic, so a detached goroutine that outlives a test reads
// either the old or the new seam, never a torn value.
package dialhook

import (
	"errors"
	"net/netip"
	"sync/atomic"
)

// ErrUnmapped is the dial error for an address Resolve does not map.
var ErrUnmapped = errors.New("unshorten: dial to a host the test network does not map")

// ResolveFunc maps host and port as dialed (the port always present) to the
// address to dial, "ip:port" or "name:port".
type ResolveFunc = func(host, port string) (addr string, ok bool)

// AllowAddrFunc reports whether the dial guard must allow a.
type AllowAddrFunc = func(a netip.AddrPort) bool

var (
	resolve   atomic.Pointer[ResolveFunc]
	allowAddr atomic.Pointer[AllowAddrFunc]
)

// SetResolve installs f (nil clears the seam) and returns a func that
// restores the previous seam.
func SetResolve(f ResolveFunc) (restore func()) {
	var p *ResolveFunc
	if f != nil {
		p = &f
	}
	old := resolve.Swap(p)
	return func() { resolve.Store(old) }
}

// SetAllowAddr installs f (nil clears the seam) and returns a func that
// restores the previous seam.
func SetAllowAddr(f AllowAddrFunc) (restore func()) {
	var p *AllowAddrFunc
	if f != nil {
		p = &f
	}
	old := allowAddr.Swap(p)
	return func() { allowAddr.Store(old) }
}

// Resolve returns the installed Resolve seam, or nil.
func Resolve() ResolveFunc {
	if p := resolve.Load(); p != nil {
		return *p
	}
	return nil
}

// AllowAddr returns the installed AllowAddr seam, or nil.
func AllowAddr() AllowAddrFunc {
	if p := allowAddr.Load(); p != nil {
		return *p
	}
	return nil
}
