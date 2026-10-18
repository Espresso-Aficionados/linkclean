package unshorten

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/flight"
)

// state is the network state a Resolver and all its WithCleaner views share.
type state struct {
	cfg    resolvedConfig
	reg    *Registry
	cache  Cache
	lim    Limiter
	client *http.Client
	// hostSlots holds one MaxConcurrentRequestsPerHost slot channel per
	// allowlisted host (every entry's Match hosts and FetchHosts). It is
	// built by newState and never changes, so it needs no lock.
	hostSlots map[string]chan struct{}
	global    chan struct{} // the MaxConcurrentRequests slots
	group     *flight.Group[hopOutcome]
	log       *slog.Logger
}

// hopOutcome is what one hop's flight call delivers.
type hopOutcome struct {
	e         CacheEntry
	fromCache bool          // the re-check inside the call hit the Cache: no request was made
	class     errClass      // fresh-path detail for the error text
	infos     []RequestInfo // emitted by the call's after func, once the key is released
}

// newState builds the shared state over a transport New has already
// checked (newState does no transport check, so tests can pass an opaque
// RoundTripper): a client that never follows redirects and has no jar and no
// timeout, a slot channel per allowlisted host, the global slot channel and
// the flight group. A call that panics delivers a transient outcome and is
// logged without its value.
func newState(rc resolvedConfig, reg *Registry, cache Cache, lim Limiter, rt http.RoundTripper, log *slog.Logger) *state {
	st := &state{
		cfg:   rc,
		reg:   reg,
		cache: cache,
		lim:   lim,
		client: &http.Client{
			Transport:     rt,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		hostSlots: make(map[string]chan struct{}),
		global:    make(chan struct{}, rc.maxReq),
		log:       log,
	}
	for _, e := range reg.entries {
		for _, h := range e.hosts {
			if st.hostSlots[h] == nil {
				st.hostSlots[h] = make(chan struct{}, rc.maxReqHost)
			}
		}
	}
	abnormal := hopOutcome{e: CacheEntry{Failure: FailTransient}, class: classOther}
	st.group = flight.New(rc.maxFlight, rc.maxFlightHost, abnormal, st.logPanic)
	return st
}

// logPanic logs a recovered strategy panic: the entry (the hop key up to its
// first NUL, see hopKey), the host, the panic value's type and the stack.
// Never the value or the rest of the key, which hold the URL.
func (st *state) logPanic(key, host string, r any, stack []byte) {
	entry, _, _ := strings.Cut(key, "\x00")
	st.log.Error("unshorten: strategy panicked; hop failed as transient",
		slog.String("entry", entry),
		slog.String("host", host),
		slog.String("panic_type", fmt.Sprintf("%T", r)),
		slog.String("stack", string(stack)))
}
