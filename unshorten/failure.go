package unshorten

import "strconv"

// FailureKind is the structured kind of a hop outcome or a resolution
// failure. Each kind re-hydrates to the same sentinel error whether the
// outcome came from the network or from the Cache.
type FailureKind uint8

const (
	FailNone FailureKind = iota
	FailHopLimit
	FailLoop
	FailFallback
	FailNoRedirect
	FailNoMatch
	FailBadTarget
	FailStatus
	FailFetchDenied
	FailTransport // definitive transport errors only: DNS NXDOMAIN, connection refused, TLS verification, dial-guard denial
	FailBudget
	FailTransient // any timeout (detached deadline, dial, TLS handshake, response header, body read, DNS timeout),
	// the caller's cancellation or deadline, Limiter rejection, HTTP 429/502/503/504, a slot wait
	// (per host or global), MaxInFlightPerHost or MaxInFlight,
	// or any attempted strategy of the hop failing so; NEVER cached
)

var failureNames = [...]string{
	FailNone:        "none",
	FailHopLimit:    "hop_limit",
	FailLoop:        "loop",
	FailFallback:    "fallback",
	FailNoRedirect:  "no_redirect",
	FailNoMatch:     "no_match",
	FailBadTarget:   "bad_target",
	FailStatus:      "status",
	FailFetchDenied: "fetch_denied",
	FailTransport:   "transport",
	FailBudget:      "budget",
	FailTransient:   "transient",
}

// String returns "none", "hop_limit", "loop", "fallback", "no_redirect",
// "no_match", "bad_target", "status", "fetch_denied", "transport", "budget"
// or "transient", and "FailureKind(n)" for any other value.
func (k FailureKind) String() string {
	if int(k) < len(failureNames) {
		return failureNames[k]
	}
	return "FailureKind(" + strconv.Itoa(int(k)) + ")"
}

// CacheEntry is the outcome of ONE network hop: the strategy's validated
// next URL for a requested URL, or a definitive failure. It never depends on a
// Cleaner, so a Cache may be shared by every Resolver in a process that has the
// same UserAgent, Transport and Registry (WithCleaner views always do). Failure is
// never FailTransient (not cached), and never a chain-level kind (FailHopLimit,
// FailLoop, FailFallback), which are recomputed per call. A stored structured
// kind re-hydrates to the same sentinel, so errors.Is keeps working.
type CacheEntry struct {
	Next     string // validated target exactly as returned, before any clean; "" with FailNone records ErrNotShortLink (stop)
	Strategy string // the strategy that produced it
	Status   int    // HTTP status of the deciding response (0 for body/preview strategies); for FailStatus, the unexpected status
	Requests int    // requests written to the wire, which can exceed the Fetcher.Get calls: net/http resends a GET once when a server closes a reused connection
	Failure  FailureKind
}
