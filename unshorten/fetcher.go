package unshorten

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
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

// maxBody is the most of a body Get reads.
const maxBody = 64 << 10

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
	f.mu.Lock()
	done := f.done
	f.mu.Unlock()
	if done {
		return nil, ErrBudget
	}
	if u == nil {
		return nil, ErrBadTarget
	}

	// The strategy's ctx is never trusted alone: a strategy that passes
	// context.Background() still cannot hold a slot, the Limiter or a body
	// read past the hop deadline.
	if ctx == nil {
		ctx = context.Background()
	}
	var cancel context.CancelFunc
	if !f.deadline.IsZero() {
		ctx, cancel = context.WithDeadline(ctx, f.deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if f.hopCtx != nil {
		defer context.AfterFunc(f.hopCtx, cancel)()
	}
	var wire atomic.Int64
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wire.Add(1) },
	})

	req, host, err := buildRequest(ctx, u, f.e)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	if f.done || f.used >= 2 {
		f.mu.Unlock()
		return nil, ErrBudget
	}
	f.used++
	f.mu.Unlock()

	// Host slot, then global slot, then the Limiter: a slot wait that ends
	// consumes no rate token. Both slots are released on every path, after
	// the body is closed.
	hs := f.st.hostSlots[host]
	if hs == nil {
		// Unreachable when f.e belongs to f.st's registry; refuse rather
		// than send a request no bound covers.
		return nil, ErrFetchDenied
	}
	select {
	case hs <- struct{}{}:
		defer func() { <-hs }()
	case <-ctx.Done():
		return nil, &fetchError{kind: FailTransient, class: classSlot}
	}
	select {
	case f.st.global <- struct{}{}:
		defer func() { <-f.st.global }()
	case <-ctx.Done():
		return nil, &fetchError{kind: FailTransient, class: classSlot}
	}
	if err := f.st.lim.Wait(ctx, host); err != nil {
		return nil, &fetchError{kind: FailTransient, class: classLimiter}
	}

	req.Header = http.Header{
		"User-Agent": {f.st.cfg.ua},
		"Accept":     {"*/*"},
	}
	// http.Client parses a redirect's Location before CheckRedirect stops
	// it, and fails Do (with the URL in the text) when it does not parse.
	// Seeing the response go by is how Get tells that apart from a
	// transport error, by type rather than by message.
	saw := &sawStatus{rt: f.st.client.Transport}
	client := *f.st.client
	client.Transport = saw
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		if saw.status != 0 {
			f.record(requestRecord{
				host: host, status: saw.status, dur: time.Since(start),
				wire: int(wire.Load()), readBody: readBody,
			})
			return nil, ErrBadTarget
		}
		return nil, f.failed(host, start, &wire, readBody, err)
	}
	defer resp.Body.Close()
	var body []byte
	if readBody {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			resp.Body.Close()
			return nil, f.failed(host, start, &wire, readBody, err)
		}
	}
	resp.Body.Close()

	out := &Response{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: body}
	loc, lerr := resp.Location()
	switch {
	case lerr == nil:
		out.Location = loc
	case errors.Is(lerr, http.ErrNoLocation):
	case 300 <= resp.StatusCode && resp.StatusCode < 400:
		// A redirect whose target does not parse cannot be followed or
		// validated.
		err = ErrBadTarget
	}
	f.record(requestRecord{
		host: host, status: resp.StatusCode, dur: time.Since(start),
		wire: int(wire.Load()), readBody: readBody,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// sawStatus is the RoundTripper of one Get: it records the status of the
// response its transport returned, if any.
type sawStatus struct {
	rt     http.RoundTripper
	status int
}

func (s *sawStatus) RoundTrip(req *http.Request) (*http.Response, error) {
	rt := s.rt
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err == nil && resp != nil {
		s.status = resp.StatusCode
	}
	return resp, err
}

// failed records a Get whose request or body read failed and returns the
// classified error.
func (f *Fetcher) failed(host string, start time.Time, wire *atomic.Int64, readBody bool, err error) error {
	kind, class := classifyTransport(err, f.hopCtx)
	if kind == FailTransient && (class == classCanceled || class == classTimeout) {
		// When the hop ends, Get's ctx is ended by whichever fires first:
		// its own deadline timer (DeadlineExceeded, possibly before
		// hopCtx's timer has fired) or the AfterFunc on hopCtx (Canceled).
		// Either way the cause is the hop's end.
		switch herr := f.hopEnded(); {
		case errors.Is(herr, context.DeadlineExceeded):
			class = classDeadline
		case herr != nil:
			class = classCanceled
		}
	}
	f.record(requestRecord{
		host: host, dur: time.Since(start), kind: kind, class: class,
		wire: int(wire.Load()), readBody: readBody,
		err: requestError(f.e.Name, host, kind, class, 0, err),
	})
	return &fetchError{kind: kind, class: class}
}

// hopEnded returns hopCtx's error, or DeadlineExceeded once the hop
// deadline has passed though hopCtx's timer has not fired yet; nil while the
// hop runs.
func (f *Fetcher) hopEnded() error {
	if f.hopCtx != nil {
		if err := f.hopCtx.Err(); err != nil {
			return err
		}
	}
	if !f.deadline.IsZero() && !time.Now().Before(f.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *Fetcher) record(r requestRecord) {
	f.mu.Lock()
	f.recs = append(f.recs, r)
	f.mu.Unlock()
}

// finish marks f done, so every later Get is ErrBudget, and returns its
// records. The hop calls it once its last strategy has returned.
func (f *Fetcher) finish() []requestRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.done = true
	return slices.Clone(f.recs)
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
//
// The comparison uses the request form's PathForRequest: an empty path is
// sent as "/", as net/http always does.
func buildRequest(ctx context.Context, u *url.URL, e *compiledEntry) (req *http.Request, host string, err error) {
	if u.Opaque != "" || u.User != nil {
		return nil, "", ErrBadTarget
	}
	rv, skip := rawurl.Parse(u.String())
	if skip != rawurl.OK || rv.IsIP || strings.IndexByte(rv.Authority, '@') >= 0 {
		return nil, "", ErrBadTarget
	}
	switch {
	case rv.Scheme == "https" && (rv.Port == "" || rv.Port == "443"):
	case rv.Scheme == "http" && e.AllowHTTP && (rv.Port == "" || rv.Port == "80"):
	default:
		return nil, "", ErrBadTarget
	}
	if _, ok := slices.BinarySearch(e.hosts, rv.Host); !ok {
		return nil, "", ErrFetchDenied
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, rv.RequestForm(), nil)
	if err != nil || req.URL.Hostname() != rv.Host || req.URL.RequestURI() != rv.PathForRequest() {
		return nil, "", ErrBadTarget
	}
	return req, rv.Host, nil
}
