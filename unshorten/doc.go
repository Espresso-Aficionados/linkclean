// Package unshorten resolves an allowlisted set of link shorteners over the
// network, so a shortener sees one request from your server instead of a click
// from every reader. It is opt-in: nothing in package linkclean makes network
// requests.
//
// # Network behaviour
//
// This section is normative:
//   - Every input is first cleaned offline (linkclean unwrap + strip), so tracking
//     parameters are never sent to a shortener.
//   - Requests go ONLY to hosts named by Registry entries (Match hosts and
//     FetchHosts), and only when the offline-cleaned input matches a
//     non-intermediate entry. Matching ignores the scheme: http://t.co/x and
//     https://t.co/x both match. The first request is always made over https;
//     plain http is used only for entries with AllowHTTP.
//   - A hop to any host that is not allowlisted ends resolution. That URL is
//     returned WITHOUT being requested.
//   - Method GET only, never HEAD. Headers: a fixed non-browser User-Agent and
//     Accept: */*. No cookies (no jar), no Referer, no Authorization, no
//     Accept-Language, no body. The URL fragment is never sent.
//   - Bodies are read only by strategies that extract from them, capped at 64 KiB.
//   - Ports 443/80 only, no userinfo, never IP-literal hosts.
//   - Preview ("no-click") endpoints are used before redirect-following wherever
//     the shortener offers one.
//   - With DefaultRegistry the hosts contacted are: t.co, spotify.link, a.co,
//     amzn.to, amzn.eu, amzn.asia, pin.it, api.pinterest.com (as hop 2 only),
//     reddit.com / www.reddit.com (/r/<sub>/s/<token> only), share.google,
//     www.google.com (/share.google… as hop 2 only), vm.tiktok.com,
//     vt.tiktok.com, www.tiktok.com (/t/… and the oEmbed endpoint), lnkd.in,
//     fb.me, and, under GenericPolicy PreviewOnly, tinyurl.com (/preview/…) and
//     bit.ly (/<id>+).
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
// See Network behaviour.
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
// See Cache, CacheEntry and NewMemoryCache.
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
