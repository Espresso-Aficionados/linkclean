// Package unshorten resolves an allowlisted set of link shorteners over the
// network, so a shortener sees one request from your server instead of a click
// from every reader. It is opt-in: nothing in package linkclean makes network
// requests.
//
// A Resolver does the work. New builds one from a Config whose zero value is
// a safe default: the guarded transport, DefaultRegistry, an in-memory cache
// and a per-host rate limit. Resolve returns the final URL with the network
// hops that led to it; Process returns the same URL as a linkclean.Result,
// so a Resolver can serve as a discordlink.Processor. Both fail open: the URL
// they return is always safe to use, and on any failure it is the offline
// clean of the input, with an error wrapping ErrUnresolved that says why.
// WithCleaner returns a view that cleans with another Cleaner (another
// affiliate policy, say) and shares all of the Resolver's network state.
// Use one Resolver per process and one view per policy:
//
//	r, err := unshorten.New(unshorten.Config{})
//	if err != nil {
//		// handle err
//	}
//	strip := r.WithCleaner(linkclean.MustNew(linkclean.Config{Affiliate: linkclean.AffiliateStrip}))
//	res, err := strip.Resolve(ctx, "https://amzn.to/abc")
//	// res.URL is usable whatever err is.
//
// A Registry lists the shorteners: each Entry names its hosts, the Strategy
// that finds a short link's destination and the shortener's own fallback
// pages. DefaultRegistry is the built-in list; Registry.With and
// Registry.Without derive others, and a custom Strategy can use a Fetcher,
// which enforces the rules below for every request it makes.
//
// # Network behaviour
//
// This section is normative:
//   - Every input is first cleaned offline (linkclean unwrap + strip), so tracking
//     parameters are never sent to a shortener. An input the offline clean
//     skips (linkclean.Result.Skipped) is returned unchanged and never
//     requested.
//   - Requests go ONLY to hosts named by Registry entries (Match hosts and
//     FetchHosts), and only when the offline-cleaned input matches a
//     non-intermediate entry. Matching ignores the scheme: http://t.co/x and
//     https://t.co/x both match. Only a requestable URL matches: one with
//     userinfo, an IP host, a port other than its scheme's default, a "."
//     or ".." path segment (raw or percent-encoded), or a path that would
//     not be sent exactly as written (raw non-ASCII bytes, an invalid
//     percent escape) never does. The first request is always made over
//     https unless the entry sets AllowHTTP: an http input is upgraded (an
//     explicit :80 is dropped), and plain http is used only for entries
//     with AllowHTTP. No DefaultRegistry entry sets it.
//   - A hop to any host that is not allowlisted ends resolution. That URL is
//     returned WITHOUT being requested.
//   - Method GET only, never HEAD. Headers: a fixed non-browser User-Agent and
//     Accept: */*. No cookies (no jar), no Referer, no Authorization, no
//     Accept-Language, no body. The URL fragment is never sent.
//   - Bodies are read only by strategies that extract from them, capped at 64 KiB.
//   - Ports 443/80 only, no userinfo, never IP-literal hosts.
//   - Preview ("no-click") endpoints are used before redirect-following wherever
//     the shortener offers one. Generic shorteners (Marketing entries) follow
//     Config.Generic: PreviewOnly (the default) uses only their preview
//     endpoints, Follow may also follow their redirect, and Skip never
//     contacts them.
//   - With DefaultRegistry the hosts contacted are: t.co, spotify.link, a.co
//     (/d/… only), amzn.to, amzn.eu, amzn.asia, pin.it, api.pinterest.com
//     (only as a later hop, never from input), reddit.com and
//     www.reddit.com (paths of the form /r/…/s/… only), share.google,
//     www.google.com and google.com (/share.google… only, as a later hop,
//     never from input), vm.tiktok.com, vt.tiktok.com, www.tiktok.com (/t/…
//     and the oEmbed endpoint), lnkd.in and fb.me. DefaultRegistry
//     registers no generic shortener: bit.ly and tinyurl.com are contacted
//     only if you register them (see Not registered by default).
//   - Each network hop is cached (in memory by default, keyed by the requested
//     URL without its fragment; 24h for a hop, 10m for a definitive failure; timeouts of every
//     kind, rate-limit rejections and overload (HTTP 429, 502, 503, 504) are
//     never cached). A cached hop does not depend
//     on the Cleaner, so one cache serves every policy. Concurrent lookups of a
//     hop are coalesced. Requests are rate-limited per host, and the number of
//     requests (overall and per host) and of lookups in progress is bounded.
//   - The default transport ignores HTTP(S)_PROXY and refuses non-public
//     destinations at dial time (code.dny.dev/ssrf). New refuses transports it
//     cannot verify as guarded unless Config.AllowUnguardedTransport is set.
//
// # Privacy
//
// Unshortening tells the shortener that someone posted the link. Each request
// discloses to the shortener's host your server's IP address, the
// User-Agent (by default linkclean's name, version and repository URL), the
// time, and the short link itself. A short link's token is often created per share, so
// the shortener can tie the request to the account that shared it. Nothing
// about the reader, the message or the chat the link came from is sent, and
// the offline clean has already removed the input's tracking parameters.
//
// A shortener that counts clicks can count your server's request as one
// click on the link, in the sharer's analytics; an amzn.to request may pass
// through Amazon Associates' click tracking. The destination (amazon.com,
// facebook.com, a news site) is never contacted: the request stops at the
// first host the Registry does not list. A hop to a host the Registry lists
// as an intermediate (api.pinterest.com, www.google.com/share.google…) is
// part of the shortener's own redirect chain. It is requested as the
// shortener served it, without its fragment and with no clean first, so it
// carries whatever parameters the shortener put there.
//
// A resolved hop is cached for PositiveTTL (24h by default), so repeat posts
// of a link within that time make no request unless the cache has evicted
// it, and concurrent lookups of one link make one request. Go's HTTP client
// may send a GET a second time when the server closes a reused connection
// without answering, so a hop can occasionally reach the shortener twice;
// Hop.Requests counts the requests written.
//
// Short links are capability URLs, so the package keeps their paths and
// queries out of everything it reports: RequestInfo (OnRequest) carries
// the entry, strategy and host but no URL, and neither the errors Resolve
// and Process return nor any line the package logs contains one. Result
// and the Cache do hold the URLs, since they are the answer; treat a shared
// Cache as holding the links posted.
//
// # Transport
//
// NewTransport builds the guarded default transport; GuardTransport guards a
// transport of your own, and GuardControl and GuardDialer guard a dialer.
//
// The guard (code.dny.dev/ssrf, plus 168.63.129.16) runs as the dialer's
// Control, so it checks the address actually being connected, just before
// connect(2): per attempt and per Happy Eyeballs racer, after DNS. DNS
// rebinding and names with several A records are therefore covered. It
// allows only public unicast addresses on tcp ports 80 and 443. It is not
// installed on the DNS resolver's own dials, which would deny the local
// stub resolver. NAT64 (64:ff9b::/96) is denied, so on an IPv6-only host
// behind DNS64 every dial fails (definitively, cached for NegativeTTL);
// allowing that prefix would admit 64:ff9b::7f00:1, which is 127.0.0.1.
//
// The guard decides by address alone. An attacker who controls DNS for an
// allowlisted host (a poisoned resolver, a compromised zone) can still
// reach an address it treats as public: your server's own public IP (a
// hairpin to anything listening on it on port 80 or 443), a
// network-specific NAT64 prefix (RFC 6052) inside 2000::/3 that a
// translator maps to a private or link-local IPv4, an ISATAP-style address
// embedding one, or public ranges your network uses privately. Where that
// matters, enforce egress with a firewall or an egress proxy.
//
// New accepts a *http.Transport only if NewTransport or GuardTransport
// built it, and checks again at New that its DialContext and Proxy are
// still the ones installed (by code pointer) and that Dial, DialTLS and
// DialTLSContext are nil. It then uses a private clone built from the
// recorded guard and proxy, never the caller's current fields, so changing
// the transport after New has no effect. The clone uses net/http's own
// protocol negotiation (the caller's TLSNextProto and RegisterProtocol
// entries are not used), disables compression, and caps response headers
// at 32 KiB. GuardTransport installs the default 2s dial timeout.
//
// With TransportOptions.Proxy every request goes through that proxy, and
// the guard exempts exactly the proxy's address: https targets are
// tunnelled with CONNECT (the proxy resolves and dials them), and plain
// http targets are sent to the proxy as absolute URIs, sharing one
// connection pool. The guard therefore protects nothing behind a proxy:
// egress policy must be enforced there. The Fetcher's host allowlist and
// its IP-literal check still apply, and HTTP(S)_PROXY is never read. No
// error or log line contains the proxy URL, whose userinfo may hold
// credentials.
//
// The fixed header list (Network behaviour) holds for transports built by
// NewTransport and GuardTransport. With AllowUnguardedTransport and a
// wrapping RoundTripper, the wrapper may add headers: tracing middleware
// such as otelhttp adds traceparent and baggage, and baggage can carry
// user or tenant identifiers. Turn propagation off for this client.
//
// # Caching
//
// The Cache holds one record per network hop, never a cleaned result. Its
// key is the entry's name, the names of the strategies eligible under the
// GenericPolicy and the URL as requested (request form: fragment removed,
// scheme and host lowercased, http upgraded to https unless the entry has
// AllowHTTP), so https://t.co/x#a and HTTP://T.CO/x share one record and no
// fragment is ever stored. Its value is the shortener's destination exactly
// as served, which can carry tracking and affiliate parameters that a
// Cleaner removes later. Every clean is recomputed on each call with the
// view's own Cleaner, so a result is the same whether its hops came from
// the network or the Cache, and WithCleaner views share one Cache safely.
//
// A next URL is kept for PositiveTTL (24h) and a definitive failure for
// NegativeTTL (10m). Transient failures (every timeout, the caller's
// cancellation, Limiter rejections, the request and in-flight bounds, HTTP
// 429, 502, 503 and 504) are never stored. ErrHopLimit, ErrLoop and
// ErrFallback are decided from the hop records on every call, so a cached
// fallback page costs no request. NewMemoryCache keeps definitive
// failures, stops and the hops an entry's IsFallback flags in its smaller
// bound, so a flood of invalid codes cannot evict real successes, and does
// not store a record whose key plus destination exceeds 2 KiB.
//
// A Cache may be shared only by Resolvers with the same UserAgent,
// Transport and Registry, since those decide a hop's outcome; a Limiter
// may be shared by any. The simplest form is one Resolver with a
// WithCleaner view per policy, which also shares the request and in-flight
// bounds and the coalescing.
//
// Concurrent lookups of one uncached hop are coalesced into one request,
// made detached from the callers under Config.Timeout: a caller that gives
// up fails open without stopping it, and a definitive outcome is still
// cached. MaxInFlightPerHost counts the hops in flight per host of the URL
// being resolved. Entries whose short-link hosts share one fetch host therefore
// share its request slots but not this bound: the tiktok entry's
// vm.tiktok.com, vt.tiktok.com and www.tiktok.com/t/ links all fetch
// TikTok's oEmbed endpoint on www.tiktok.com, so when it stalls up to
// 3 × 16 = 48 of the default 256 hops can wait on its 4 request slots;
// other hosts are not affected.
//
// # Not registered by default
//
// The package exports strategies for two generic shorteners that
// DefaultRegistry does not register: BitlyInfo (bit.ly's "+" info page) and
// TinyURLPreview (tinyurl.com's preview page). Both are Marketing entries:
// their links are often per-recipient campaign links, and whether a preview
// request registers a click, or works without logging in, is not
// established. Add them with Registry.With if you accept that:
//
//	reg, err := unshorten.DefaultRegistry().With(
//		unshorten.Entry{
//			Name:     "bit.ly",
//			Match:    []string{"*://bit.ly/*", "*://bitly.com/*"},
//			Strategy: unshorten.BitlyInfo(),
//			Fallback: unshorten.Redirect(), // used only under Follow
//			Kind:     unshorten.Marketing,
//			// bit.ly's own homepage is its fallback, never a destination.
//			IsFallback: func(_, t *url.URL) bool {
//				h := strings.ToLower(t.Hostname())
//				return (h == "bitly.com" || h == "www.bitly.com" || h == "bit.ly") &&
//					(t.Path == "" || t.Path == "/") && t.RawQuery == ""
//			},
//		},
//		unshorten.Entry{
//			Name:     "tinyurl",
//			Match:    []string{"*://tinyurl.com/*"},
//			Strategy: unshorten.TinyURLPreview(),
//			Fallback: unshorten.Redirect(), // used only under Follow
//			Kind:     unshorten.Marketing,
//		},
//	)
//	if err != nil {
//		// handle err
//	}
//	r, err := unshorten.New(unshorten.Config{Registry: reg})
//
// Under GenericPolicy PreviewOnly (the default) these entries request only
// bit.ly/<id>+ and tinyurl.com/preview/<alias>; under Follow they may also
// follow the short link's redirect, which registers a click from your
// server; under Skip they are never requested.
package unshorten
