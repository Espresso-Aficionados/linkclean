package unshorten

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Fetcher is concrete, so methods can be added later.
type Fetcher struct {
	st       *state
	e        *compiledEntry
	hop      int             // hop index for RequestInfo.Hop
	hopCtx   context.Context // the detached hop context; Get never outlives it
	deadline time.Time       // hopCtx's deadline, applied to every Get's ctx

	mu   sync.Mutex
	used int  // Gets that passed the checks: at most 2 per hop
	done bool // set once the hop's last strategy returned: every later Get is ErrBudget
	recs []requestRecord
}

// requestRecord is one Fetcher.Get that reached the transport.
type requestRecord struct {
	host     string
	status   int // 0 if no response
	dur      time.Duration
	kind     FailureKind // FailNone when a response arrived
	class    errClass
	wire     int   // request writes seen by httptrace's WroteRequest: ≥ 1 on a net/http retry, 0 if never written
	readBody bool  // decides Hop.Status: only a Get without the body reports its status
	err      error // sanitized (requestError); nil on success
}

// Get performs exactly one GET and never follows redirects. Before sending it
// enforces: host ∈ entry Match hosts ∪ FetchHosts (else ErrFetchDenied), https
// (or http with AllowHTTP), port ""/443 (80 for http), no userinfo, no IP literal
// (else ErrBadTarget), the per-hop budget of 2 requests (strategy, then
// fallback; so at most 2*MaxHops per Resolve; else ErrBudget), a
// MaxConcurrentRequestsPerHost slot, a MaxConcurrentRequests slot,
// Limiter.Wait (in that order; both slots released on every path, after
// Body.Close on success), and the fixed headers. ctx is bounded by the hop
// deadline (Timeout) whatever the strategy passes. Location is RESOLVED
// against the request URL (relative and protocol-relative handled); a
// Location that does not parse is ErrBadTarget on a 3xx and ignored
// otherwise. The body is read via io.LimitReader(64 KiB) only if readBody;
// otherwise it is closed unread.
func (f *Fetcher) Get(ctx context.Context, u *url.URL, readBody bool) (*Response, error) {
	panic("unshorten: not implemented")
}

// Response is the part of an HTTP response a Strategy sees.
type Response struct {
	Status      int
	Location    *url.URL // absolute, or nil
	ContentType string
	Body        []byte // nil unless readBody
}

// buildRequest applies Get's checks to u for entry e and builds the GET for
// its request form (no fragment), with no headers yet. It is pure. An input
// rawurl.Parse skips, an opaque URL, userinfo, an IP host, or a scheme or
// port outside https 443 / http 80 (http only with AllowHTTP) is
// ErrBadTarget; a host outside e's hosts is ErrFetchDenied. The request's
// RequestURI must equal the request form's path and query byte for byte,
// else ErrBadTarget, so the bytes on the wire are the cache key's. host is
// the host view.
func buildRequest(ctx context.Context, u *url.URL, e *compiledEntry) (req *http.Request, host string, err error) {
	panic("unshorten: not implemented")
}
