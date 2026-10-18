package unshorten

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

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
	if !(perSecond > 0) || math.IsInf(perSecond, 1) || burst < 1 {
		panic(fmt.Sprintf("unshorten: NewHostLimiter(%v, %d): perSecond must be positive and finite, burst at least 1", perSecond, burst))
	}
	// An out-of-range float-to-int conversion is not portable, so clamp
	// first: below about 1e-10 per second the interval is the longest
	// Duration, above 1e9 it is one nanosecond.
	interval := time.Duration(math.MaxInt64)
	if f := float64(time.Second) / perSecond; f < math.MaxInt64 {
		interval = max(time.Duration(f), 1)
	}
	window := time.Duration(math.MaxInt64)
	if int64(burst) <= math.MaxInt64/int64(interval) {
		window = time.Duration(burst) * interval
	}
	return &hostLimiter{
		interval: interval,
		window:   window,
		tat:      map[string]time.Time{},
		pruneAt:  limiterPruneAt,
	}
}

// limiterPruneAt is the host count past which NewHostLimiter's map is pruned.
const limiterPruneAt = 256

// errLimitDeadline is Wait's error when the host's next slot is past ctx's
// deadline. It does not wrap context.DeadlineExceeded: ctx has not ended.
var errLimitDeadline = errors.New("unshorten: rate limit: next request slot is past the deadline")

// hostLimiter is a GCRA (the virtual-scheduling form of a token bucket) per
// host. tat is each host's theoretical arrival time: the end of its
// committed schedule. A request commits tat' = max(tat, now) + interval and
// may start at tat' − window, where window = burst × interval.
type hostLimiter struct {
	interval time.Duration
	window   time.Duration

	mu      sync.Mutex
	tat     map[string]time.Time
	pruneAt int // prune when len(tat) exceeds it; grows with the live set, so pruning stays amortized O(1)
}

func (l *hostLimiter) Wait(ctx context.Context, host string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now()
	l.mu.Lock()
	tat, ok := l.tat[host]
	if !ok || tat.Before(now) {
		tat = now
	}
	next := tat.Add(l.interval)
	// delay = next − window − now, the time the request must wait when
	// positive. It is written so that neither term can overflow: tat.Sub
	// saturates and window ≥ interval.
	delay := tat.Sub(now) - (l.window - l.interval)
	if delay <= 0 {
		l.commit(host, next, now, ok)
		l.mu.Unlock()
		return nil
	}
	allowAt := now.Add(delay)
	if dl, has := ctx.Deadline(); has && dl.Before(allowAt) {
		l.mu.Unlock()
		return errLimitDeadline // nothing committed
	}
	l.commit(host, next, now, ok)
	l.mu.Unlock()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		// Give the slot back if it is still the last one committed, as
		// x/time/rate restores a cancelled reservation's tokens less those
		// reserved after it: a later waiter keeps its start time, so
		// returning an earlier slot would let one request too many start
		// within the burst window. A later waiter that is itself cancelled
		// first gives its slot back, and then this one can too. The
		// schedule never moves into the past.
		now := time.Now()
		l.mu.Lock()
		if t, ok := l.tat[host]; ok && t.Equal(next) {
			l.tat[host] = maxTime(t.Add(-l.interval), now)
		}
		l.mu.Unlock()
		return ctx.Err()
	}
}

// commit records host's new schedule end, pruning first when a new host
// takes the map past pruneAt. l.mu is held.
func (l *hostLimiter) commit(host string, tat, now time.Time, existed bool) {
	if !existed && len(l.tat) >= l.pruneAt {
		for h, t := range l.tat {
			if !t.After(now) {
				delete(l.tat, h)
			}
		}
		l.pruneAt = max(limiterPruneAt, 2*len(l.tat))
	}
	l.tat[host] = tat
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// NoLimit returns a Limiter that never waits; Wait returns ctx.Err().
func NoLimit() Limiter { return noLimit{} }

type noLimit struct{}

func (noLimit) Wait(ctx context.Context, _ string) error { return ctx.Err() }
