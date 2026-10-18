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
// # Caching
//
// See Cache, CacheEntry and NewMemoryCache.
//
// # Not registered by default
//
// Shorteners whose strategies the package exports but DefaultRegistry does
// not register can be added with Registry.With.
package unshorten
