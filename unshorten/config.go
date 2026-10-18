package unshorten

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
)

// Config: every zero value is a safe default. A negative count or duration
// is an error from New.
type Config struct {
	Cleaner   *linkclean.Cleaner // nil means linkclean.Default()
	Transport http.RoundTripper  // nil means NewTransport(TransportOptions{})
	// AllowUnguardedTransport accepts a Transport that New cannot verify as
	// SSRF-guarded (any non-*http.Transport, e.g. an otelhttp wrapper, or an
	// *http.Transport not produced by NewTransport/GuardTransport, or one whose
	// Proxy is http.ProxyFromEnvironment). Set it only when you have guarded the
	// dialer yourself (GuardDialer) or wrap a guarded transport.
	AllowUnguardedTransport bool
	// Logger: nil means slog.Default(). It is used for the unguarded-transport
	// warning and to log a recovered strategy panic at Error level (entry,
	// host, the panic value's type and the stack; never the value, which may
	// hold the URL).
	Logger   *slog.Logger
	Registry *Registry // nil means DefaultRegistry()
	// Cache: nil means NewMemoryCache(10_000); NoCache() disables. It is shared
	// by WithCleaner views, and may be shared with other Resolvers only if they
	// have the same UserAgent, Transport and Registry (see CacheEntry).
	Cache   Cache
	Limiter Limiter // nil means NewHostLimiter(2 /*rps*/, 4 /*burst*/); per-host budgets are per Limiter instance
	// MaxConcurrentRequests bounds outbound requests in progress at once, across
	// this Resolver and its WithCleaner views. 0 means 32.
	// MaxConcurrentRequestsPerHost bounds them per request host and is taken
	// first, so one stalling host holds at most that many global slots. 0 means
	// 4 (= NewTransport's MaxConnsPerHost).
	// Fetcher.Get takes the host slot, then the global slot, then Limiter.Wait,
	// so a slot wait that fails consumes no rate token. Both slots are released
	// on every path: a Wait error, a RoundTrip error, or after the response body
	// is closed. A slot wait past the deadline is a transient failure (never
	// cached).
	MaxConcurrentRequests        int
	MaxConcurrentRequestsPerHost int
	// MaxInFlight bounds distinct uncached hops being fetched at once (coalesced
	// lookups, including detached ones whose callers have gone). A miss beyond it
	// fails open immediately with ErrTransient (never cached) and makes no
	// request. 0 means 256. It bounds goroutines and memory under a flood of
	// unique short links, whatever the rate limit.
	MaxInFlight int
	// MaxInFlightPerHost bounds, per request host (the host of the hop URL
	// being resolved), the keys counted in MaxInFlight. It is checked first,
	// under the same lock, so a flood of unique short links for one host holds
	// at most this many global keys and cannot fail other hosts' hops. A miss
	// beyond it fails open immediately with ErrTransient and makes no request.
	// 0 means 16: what the default Limiter (2 rps, burst 4) can serve within the
	// default Timeout (6s); with another Limiter or Timeout set it to about
	// perSecond*Timeout + burst.
	MaxInFlightPerHost int
	Generic            GenericPolicy // zero: PreviewOnly
	MaxHops            int           // network hops; 0 means 4; more than 5 means 5
	Timeout            time.Duration // whole Resolve incl. all hops; 0 means 6s
	PositiveTTL        time.Duration // per successful hop; 0 means 24h
	NegativeTTL        time.Duration // per definitive failure; 0 means 10m; transient failures are never cached
	// UserAgent: "" means "linkclean/<module version>
	// (+https://github.com/Espresso-Aficionados/linkclean)". One containing CR,
	// LF or NUL is an error.
	UserAgent string
	// OnRequest is called after every outbound request: once per Fetcher.Get
	// that reached the transport (a dial failure counts), never for a Fetcher
	// refusal, a slot wait that ended or a Limiter rejection. Calls are made
	// after the hop's lookup is released, on the goroutine that fetched the
	// hop, so OnRequest must be safe for concurrent use, should not block, and
	// may run after Resolve has returned.
	OnRequest func(RequestInfo)
}

// RequestInfo never contains the path or query: short tokens are capability URLs.
// Failure is the structured kind (FailNone on success); a request whose
// response the hop then failed on (an HTTP status, no redirect, no match, a
// bad target) carries that failure. Err is SANITIZED:
// the Resolver never passes a *url.Error (whose Error() embeds the request URL);
// it reduces a transport error to its class (timeout of any kind, cancel, DNS,
// dial refused, TLS, dial-guard denial; FailTransient says which are transient)
// and wraps the matching sentinel: ErrTransient for a timeout, and
// ssrf.ErrProhibitedIP, ErrProhibitedPort or ErrProhibitedNetwork
// (code.dny.dev/ssrf) for a dial-guard denial. So Err.Error() never contains
// a URL, path or query. Errors returned by Resolve and Process are sanitized
// the same way. Neither wraps context.DeadlineExceeded for a transport or hop
// timeout: an error wraps a context error only when the caller's own ctx
// ended.
type RequestInfo struct {
	Entry, Strategy, Host string // Host: the request host
	Hop                   int    // index of the hop, from 0, for the caller that started it
	Status                int    // HTTP status; 0 if no response
	Duration              time.Duration
	Failure               FailureKind
	Err                   error
}

// GenericPolicy decides how Marketing entries (generic shorteners) are used.
type GenericPolicy uint8

const (
	PreviewOnly GenericPolicy = iota // Marketing entries: only no-click preview endpoints
	Follow                           // Marketing entries: may also follow 3xx (registers a click from your IP)
	Skip                             // never touch Marketing entries
)

// resolvedConfig is cfg's scalar settings with every default filled and
// validated, built by resolveConfig.
type resolvedConfig struct {
	maxReq, maxReqHost, maxFlight, maxFlightHost, maxHops int
	timeout, posTTL, negTTL                               time.Duration
	policy                                                GenericPolicy
	ua                                                    string
	onRequest                                             func(RequestInfo)
}

// Defaults, and the MaxHops clamp.
const (
	defaultMaxReq        = 32
	defaultMaxReqHost    = 4
	defaultMaxFlight     = 256
	defaultMaxFlightHost = 16
	defaultMaxHops       = 4
	maxMaxHops           = 5
	defaultTimeout       = 6 * time.Second
	defaultPositiveTTL   = 24 * time.Hour
	defaultNegativeTTL   = 10 * time.Minute
)

// resolveConfig fills cfg's scalar defaults and validates them: a negative
// count or duration, an unknown GenericPolicy, or a UserAgent with CR, LF or
// NUL is an error; MaxHops above 5 becomes 5. The Cleaner, Transport,
// Logger, Registry, Cache and Limiter are New's.
func resolveConfig(cfg Config) (resolvedConfig, error) {
	var errs []error
	count := func(name string, v, def int) int {
		switch {
		case v < 0:
			errs = append(errs, fmt.Errorf("unshorten: Config.%s is negative (%d)", name, v))
		case v == 0:
			return def
		}
		return v
	}
	dur := func(name string, v, def time.Duration) time.Duration {
		switch {
		case v < 0:
			errs = append(errs, fmt.Errorf("unshorten: Config.%s is negative (%v)", name, v))
		case v == 0:
			return def
		}
		return v
	}
	rc := resolvedConfig{
		maxReq:        count("MaxConcurrentRequests", cfg.MaxConcurrentRequests, defaultMaxReq),
		maxReqHost:    count("MaxConcurrentRequestsPerHost", cfg.MaxConcurrentRequestsPerHost, defaultMaxReqHost),
		maxFlight:     count("MaxInFlight", cfg.MaxInFlight, defaultMaxFlight),
		maxFlightHost: count("MaxInFlightPerHost", cfg.MaxInFlightPerHost, defaultMaxFlightHost),
		maxHops:       min(count("MaxHops", cfg.MaxHops, defaultMaxHops), maxMaxHops),
		timeout:       dur("Timeout", cfg.Timeout, defaultTimeout),
		posTTL:        dur("PositiveTTL", cfg.PositiveTTL, defaultPositiveTTL),
		negTTL:        dur("NegativeTTL", cfg.NegativeTTL, defaultNegativeTTL),
		policy:        cfg.Generic,
		ua:            cfg.UserAgent,
		onRequest:     cfg.OnRequest,
	}
	if cfg.Generic > Skip {
		errs = append(errs, fmt.Errorf("unshorten: Config.Generic is not a GenericPolicy (%d)", cfg.Generic))
	}
	if strings.ContainsAny(cfg.UserAgent, "\r\n\x00") {
		errs = append(errs, errors.New("unshorten: Config.UserAgent contains CR, LF or NUL"))
	}
	if rc.ua == "" {
		rc.ua = defaultUA()
	}
	if err := errors.Join(errs...); err != nil {
		return resolvedConfig{}, err
	}
	return rc, nil
}

// modulePath is linkclean's module path, looked up in the build info.
const modulePath = "github.com/Espresso-Aficionados/linkclean"

// defaultUA is the default User-Agent, from the running binary's build info.
var defaultUA = sync.OnceValue(func() string {
	bi, _ := debug.ReadBuildInfo()
	return uaFrom(bi)
})

// uaFrom returns "linkclean/<v> (+https://github.com/Espresso-Aficionados/linkclean)".
// <v> is linkclean's module version in bi: Main's when Main is linkclean (its
// own tests), else the Deps entry's, its Replace's version when the
// replacement has one. A directory replacement, "(devel)", an empty version
// or a nil bi give "devel".
func uaFrom(bi *debug.BuildInfo) string {
	v := ""
	if bi != nil {
		if bi.Main.Path == modulePath {
			v = bi.Main.Version
		} else {
			for _, d := range bi.Deps {
				if d.Path != modulePath {
					continue
				}
				v = d.Version
				if d.Replace != nil {
					v = d.Replace.Version
				}
				break
			}
		}
	}
	if v == "" || v == "(devel)" {
		v = "devel"
	}
	return "linkclean/" + v + " (+https://github.com/Espresso-Aficionados/linkclean)"
}
