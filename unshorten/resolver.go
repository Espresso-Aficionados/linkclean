package unshorten

import (
	"context"
	"log/slog"

	"github.com/Espresso-Aficionados/linkclean"
)

// New validates cfg (registry patterns, hosts lowercase ASCII without port,
// non-nil strategies, and no negative count or duration) and the transport:
//   - nil → NewTransport(TransportOptions{}).
//   - *http.Transport created by NewTransport or GuardTransport → attested, and
//     RE-VERIFIED field by field at New, because the attestation is recorded by
//     pointer and the caller may have mutated the transport since:
//     Proxy is nil or the exact fixed-proxy func GuardTransport/NewTransport
//     installed (compared by reflect code pointer, so ProxyFromEnvironment or
//     any other func fails); DialContext is the guard-installed func (same
//     comparison); Dial, DialTLS and DialTLSContext are nil.
//     Any mismatch → error wrapping ErrUnguardedTransport (unless
//     AllowUnguardedTransport, which then logs one warning). Clone() drops the
//     attestation, so wrap clones with GuardTransport.
//   - *http.Transport whose Proxy is http.ProxyFromEnvironment (so
//     http.DefaultTransport is caught), or any other *http.Transport or
//     RoundTripper → error wrapping ErrUnguardedTransport, unless
//     AllowUnguardedTransport. When allowed and the transport is detectably
//     unguarded (an *http.Transport), New logs one warning via cfg.Logger.
//
// After verification New uses a private Clone() of an attested transport
// (re-attested internally), so mutating the caller's transport after New has
// no effect on the Resolver. The private transport uses Go's default protocol
// negotiation (the caller's TLSNextProto and RegisterProtocol entries are not
// used), disables compression, and caps response headers at 32 KiB.
func New(cfg Config) (*Resolver, error) {
	rc, err := resolveConfig(cfg)
	if err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	reg := cfg.Registry
	if reg == nil {
		reg = DefaultRegistry()
	}
	cache := cfg.Cache
	if cache == nil {
		cache = NewMemoryCache(defaultCacheEntries)
	}
	lim := cfg.Limiter
	if lim == nil {
		lim = NewHostLimiter(2, 4)
	}
	rt, err := checkTransport(cfg.Transport, cfg.AllowUnguardedTransport, log)
	if err != nil {
		return nil, err
	}
	c := cfg.Cleaner
	if c == nil {
		c = linkclean.Default()
	}
	return &Resolver{st: newState(rc, reg, cache, lim, rt, log), c: c}, nil
}

// Resolver is immutable and safe for concurrent use. It owns the network state:
// client and transport, Registry, Cache, Limiter, the request and in-flight
// bounds, and the coalescing of concurrent lookups.
type Resolver struct {
	st *state             // shared by r and every WithCleaner view
	c  *linkclean.Cleaner // this view's Cleaner
}

// WithCleaner returns a view of r that cleans with c (nil means
// linkclean.Default()) and shares ALL of r's network state. The cache records
// network hops, never cleaned results, so views with different Cleaners
// get policy-correct results from one cache, and every per-host budget, request
// bound and coalesced lookup is shared. Use one Resolver per process and one
// view per policy. It performs no I/O.
func (r *Resolver) WithCleaner(c *linkclean.Cleaner) *Resolver {
	if c == nil {
		c = linkclean.Default()
	}
	return &Resolver{st: r.st, c: c}
}

// Resolve offline-cleans rawURL. If the result matches a non-intermediate registry
// entry, it follows short-link hops over the network and offline-cleans the final URL.
// An input the offline clean skips (linkclean.Result.Skipped) is returned
// unchanged, without a request. A strategy that reports ErrNotShortLink for
// the first hop leaves the offline result, with a nil error.
//
// Result.URL is ALWAYS safe to use. On any failure it is Cleaner.Clean(rawURL):
// never a homepage or fallback page, never an intermediate redirector. err is
// non-nil iff network resolution was attempted and did not complete; then
// errors.Is(err, ErrUnresolved) holds, the specific sentinel is also wrapped, and
// errors.Is keeps working after a cache hit.
func (r *Resolver) Resolve(ctx context.Context, rawURL string) (Result, error) {
	ch := r.resolve(ctx, rawURL)
	return Result{
		Input:    rawURL,
		URL:      ch.url,
		Resolved: len(ch.hops) > 0 && ch.url != ch.off.URL,
		Cached:   ch.cached && len(ch.hops) > 0,
		Hops:     ch.hops,
		Clean:    ch.lastClean(),
	}, ch.err
}

// Process implements discordlink.Processor. The returned linkclean.Result has
// Input = rawURL and URL = the final URL. Its Hops are, in order, the offline
// hops of the first clean, then for each network hop: that hop (Category
// CatUnshortened, Rule "unshorten:<entry>", FromHost = the short-link host that
// was requested) followed by the offline hops of the clean of its destination,
// if the destination left the registry. Its Removed/Kept are the first clean's,
// then each destination clean's in hop order (the first clean removes tracking
// before the short link is requested; the destination's tracking and affiliate
// removals come from Explain of the destination as served). Changed is
// URL != Input, Skipped is the first clean's, Signed is the last clean's, and
// UnwrapAbandoned is true if any clean abandoned an unwrap.
func (r *Resolver) Process(ctx context.Context, rawURL string) (linkclean.Result, error) {
	ch := r.resolve(ctx, rawURL)
	return ch.assemble(rawURL), ch.err
}

// Match reports, offline and without I/O, whether rawURL would trigger network
// resolution under r's GenericPolicy, and which entry. Only a requestable
// URL matches: one with userinfo, an IP host, or a port other than its
// scheme's default never does.
func (r *Resolver) Match(rawURL string) (entry string, ok bool) {
	off := r.c.Explain(rawURL)
	if off.Skipped != linkclean.NotSkipped {
		return "", false
	}
	e, _, ok := r.st.lookup(off.URL, false)
	if !ok {
		return "", false
	}
	return e.Name, true
}

// Result is the outcome of Resolve.
type Result struct {
	Input    string
	URL      string // always usable
	Resolved bool   // network resolution succeeded and changed the URL
	Cached   bool   // every network hop was served from the Cache
	Hops     []Hop
	Clean    linkclean.Result // the final offline clean
}

// Hop is one network hop.
type Hop struct {
	From, To string // From: the URL as requested (fragment removed, scheme and host lowercased, http upgraded to https unless the entry has AllowHTTP); To: the target exactly as returned by the strategy and validated, before any clean
	Entry    string
	Strategy string
	Status   int // 0 for body/preview strategies
	Requests int // requests written to the wire for this hop (see CacheEntry.Requests)
}
