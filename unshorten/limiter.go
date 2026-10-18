package unshorten

import "context"

// Limiter is a pure rate limiter. It has no release, so it cannot bound
// concurrency; Config.MaxConcurrentRequestsPerHost, MaxConcurrentRequests,
// MaxInFlightPerHost and MaxInFlight do that.
type Limiter interface {
	// Wait returns nil when a request to host may start, possibly after waiting.
	// It returns an error immediately when the next slot is past ctx's deadline,
	// and when ctx is done. A non-nil error fails the hop open as a transient
	// failure (ErrTransient; never cached). A rejected or cancelled Wait
	// consumes no token (as x/time/rate.Limiter.Wait cancels its reservation),
	// so a flood of rejected requests never pushes a host's budget into debt.
	Wait(ctx context.Context, host string) error
}

// NewHostLimiter returns a per-host token bucket (perSecond, burst); in-house,
// no x/time dependency. It panics unless perSecond is positive and finite and
// burst is at least 1.
func NewHostLimiter(perSecond float64, burst int) Limiter {
	panic("unshorten: not implemented")
}

// NoLimit returns a Limiter that never waits; Wait returns ctx.Err().
func NoLimit() Limiter {
	panic("unshorten: not implemented")
}
