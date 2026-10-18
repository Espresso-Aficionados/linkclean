package unshorten

// Resolver rows of the §12.8 table: hops, scheme, validation, loop, hop
// limit, budget, requestable lookup, skipped inputs, stops, fallbacks,
// policy, request form, shared cache across views, affiliate interplay and
// categories. Every row runs over the test network (nettest_test.go) with
// the real dial guard in the path, unless it says otherwise. Helpers shared
// by the Resolver test files are prefixed "rv".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// rvToken is embedded in paths whose text must never reach an error, a
// RequestInfo or a log line.
const rvToken = "Zq7TOKENx9"

// rvResolver builds a Resolver over n. A nil cfg.Transport becomes
// NewTransport with n's CA (guarded, attested, never assigned a DialContext),
// and a nil cfg.Limiter becomes NoLimit(), so rows do not wait on the default
// 2 rps. Call it after installNet (see newTestResolver).
func rvResolver(t *testing.T, n *testNet, cfg Config) *Resolver {
	t.Helper()
	if cfg.Transport == nil {
		cfg.Transport = NewTransport(TransportOptions{TLSConfig: n.ClientTLS()})
	}
	if cfg.Limiter == nil {
		cfg.Limiter = NoLimit()
	}
	return newTestResolver(t, cfg)
}

// rvTransport is NewTransport over n with opts' timeouts.
func rvTransport(n *testNet, opts TransportOptions) *http.Transport {
	opts.TLSConfig = n.ClientTLS()
	return NewTransport(opts)
}

// rvRedirect answers status with Location loc (no Location when loc is "").
func rvRedirect(status int, loc string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(status)
	})
}

// rvBody answers 200 with body as text/html.
func rvBody(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

// rvRoutes serves a request by its RequestURI as received (path and query);
// anything else is 404.
type rvRoutes map[string]http.Handler

func (m rvRoutes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := m[r.RequestURI]; h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// rvGate holds requests until it is opened. Create it after the Resolver:
// cleanups run LIFO, so the gate then opens before the Resolver drains and
// the test network closes.
type rvGate struct {
	ch      chan struct{}
	once    sync.Once
	arrived atomic.Int64
}

func rvNewGate(t *testing.T) *rvGate {
	g := &rvGate{ch: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *rvGate) open() { g.once.Do(func() { close(g.ch) }) }

// hold counts each request's arrival, waits for the gate (or the request's
// end) and then serves h.
func (g *rvGate) hold(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.arrived.Add(1)
		select {
		case <-g.ch:
		case <-r.Context().Done():
			return
		}
		h.ServeHTTP(w, r)
	})
}

// rvWaitFor polls cond until it holds, failing the test after 10s.
func rvWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// rvPathHits counts the requests s received for path.
func rvPathHits(s *testServer, path string) int {
	n := 0
	for _, r := range s.Requests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// rvStrategy is a Strategy with a test-provided Next.
type rvStrategy struct {
	name string
	next func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error)
}

func (s rvStrategy) Name() string { return s.name }
func (s rvStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	return s.next(ctx, f, u)
}

// rvStopper fetches u once, then reports ErrNotShortLink.
func rvStopper() Strategy {
	return rvStrategy{name: "Stopper", next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		if _, err := f.Get(ctx, u, false); err != nil {
			return nil, err
		}
		return nil, ErrNotShortLink
	}}
}

// rvEntry is a FirstParty Redirect entry named host that matches host.
func rvEntry(host string) Entry {
	return Entry{Name: host, Match: []string{"*://" + host + "/*"}, Strategy: Redirect(), Kind: FirstParty}
}

// rvRegistry is NewRegistry(entries...), failing the test on an error.
func rvRegistry(t *testing.T, entries ...Entry) *Registry {
	t.Helper()
	reg, err := NewRegistry(entries...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// rvDefaultWith is DefaultRegistry().With(entries...).
func rvDefaultWith(t *testing.T, entries ...Entry) *Registry {
	t.Helper()
	reg, err := DefaultRegistry().With(entries...)
	if err != nil {
		t.Fatalf("DefaultRegistry().With: %v", err)
	}
	return reg
}

// rvRequireEntry skips the test when DefaultRegistry has no entry named
// name: "verify" entries and at-risk entries are removed from the default
// registry when the maintainer's livenet run does not confirm them.
func rvRequireEntry(t *testing.T, names ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, e := range DefaultRegistry().Entries() {
		have[e.Name] = true
	}
	for _, name := range names {
		if !have[name] {
			t.Skipf("DefaultRegistry has no %q entry (removed after the livenet run)", name)
		}
	}
}

// rvBitlyEntry is the bit.ly row of the default-registry table, which ships
// unregistered (Q18): BitlyInfo, Redirect only under Follow, Marketing,
// fallback = a bitly.com or bit.ly root.
func rvBitlyEntry() Entry {
	return Entry{
		Name:     "bit.ly",
		Match:    []string{"*://bit.ly/*", "*://bitly.com/*"},
		Strategy: BitlyInfo(),
		Fallback: Redirect(),
		Kind:     Marketing,
		IsFallback: func(_, target *url.URL) bool {
			h := strings.ToLower(target.Hostname())
			root := (target.Path == "" || target.Path == "/") && target.RawQuery == ""
			return root && (h == "bit.ly" || h == "bitly.com" || h == "www.bitly.com")
		},
	}
}

// rvKey returns the hop key r uses for the request form reqForm.
func rvKey(t *testing.T, r *Resolver, reqForm string, intermediate bool) string {
	t.Helper()
	u, skip := rawurl.Parse(reqForm)
	if skip != rawurl.OK {
		t.Fatalf("rawurl.Parse(%q): %v", reqForm, skip)
	}
	e, ok := r.st.reg.lookup(&u, intermediate, r.st.cfg.policy)
	if !ok {
		t.Fatalf("no entry for %q", reqForm)
	}
	return hopKey(e, r.st.cfg.policy, reqForm)
}

// rvLimiter is a spy Limiter: it counts Waits per host and returns err(host)
// when err is set, else ctx.Err().
type rvLimiter struct {
	mu    sync.Mutex
	waits map[string]int
	err   func(host string) error
}

func (l *rvLimiter) Wait(ctx context.Context, host string) error {
	l.mu.Lock()
	if l.waits == nil {
		l.waits = map[string]int{}
	}
	l.waits[host]++
	f := l.err
	l.mu.Unlock()
	if f != nil {
		if err := f(host); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (l *rvLimiter) count(host string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.waits[host]
}

// rvCache is a spy Cache over inner: it records every key and entry.
type rvCache struct {
	inner Cache
	mu    sync.Mutex
	gets  []string
	sets  []string
	vals  []CacheEntry
	get   func(ctx context.Context, key string) (CacheEntry, bool, bool) // optional override: (e, hit, handled)
}

func (c *rvCache) Get(ctx context.Context, key string) (CacheEntry, bool) {
	c.mu.Lock()
	c.gets = append(c.gets, key)
	f := c.get
	c.mu.Unlock()
	if f != nil {
		if e, hit, handled := f(ctx, key); handled {
			return e, hit
		}
	}
	return c.inner.Get(ctx, key)
}

func (c *rvCache) Set(ctx context.Context, key string, e CacheEntry, ttl time.Duration) {
	c.mu.Lock()
	c.sets = append(c.sets, key)
	c.vals = append(c.vals, e)
	c.mu.Unlock()
	c.inner.Set(ctx, key, e, ttl)
}

func (c *rvCache) keys() (gets, sets []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.gets...), append([]string(nil), c.sets...)
}

// rvInfos collects RequestInfos. Read it after r.st.group.Wait(): calls are
// made after the hop's key is released, possibly after Resolve returned.
type rvInfos struct {
	mu   sync.Mutex
	list []RequestInfo
}

func (x *rvInfos) record(i RequestInfo) {
	x.mu.Lock()
	x.list = append(x.list, i)
	x.mu.Unlock()
}

func (x *rvInfos) all() []RequestInfo {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]RequestInfo(nil), x.list...)
}

// rvFailOpen asserts the failure contract: err wraps ErrUnresolved and each
// of want; URL is Clean(raw); no hop; not resolved.
func rvFailOpen(t *testing.T, r *Resolver, raw string, res Result, err error, want ...error) {
	t.Helper()
	if err == nil {
		t.Fatalf("Resolve(%q) = %q, nil error; want a failure", raw, res.URL)
	}
	for _, w := range append([]error{ErrUnresolved}, want...) {
		if !errors.Is(err, w) {
			t.Errorf("Resolve(%q) error %q does not wrap %q", raw, err, w)
		}
	}
	if c := r.c.Clean(raw); res.URL != c {
		t.Errorf("failed Resolve(%q).URL = %q, want Clean(raw) %q", raw, res.URL, c)
	}
	if len(res.Hops) != 0 || res.Resolved || res.Cached {
		t.Errorf("failed Resolve(%q): Hops %v, Resolved %v, Cached %v; want none, false, false", raw, res.Hops, res.Resolved, res.Cached)
	}
	if res.Input != raw {
		t.Errorf("Input = %q, want %q", res.Input, raw)
	}
	rvInvariant(t, r, res)
}

// rvInvariant asserts Clean(URL) == URL.
func rvInvariant(t *testing.T, r *Resolver, res Result) {
	t.Helper()
	if c := r.c.Clean(res.URL); c != res.URL {
		t.Errorf("Clean(URL) = %q, want URL %q", c, res.URL)
	}
}

// rvMust resolves raw and fails the test on an error.
func rvMust(t *testing.T, r *Resolver, raw string) Result {
	t.Helper()
	res, err := r.Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", raw, err)
	}
	if res.Input != raw {
		t.Errorf("Input = %q, want %q", res.Input, raw)
	}
	rvInvariant(t, r, res)
	return res
}

// rvStrip is an AffiliateStrip Cleaner.
func rvStrip(t *testing.T) *linkclean.Cleaner {
	t.Helper()
	c, err := linkclean.New(linkclean.Config{Affiliate: linkclean.AffiliateStrip})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// --- Hops ---

// TestHopsChain resolves 1- to 4-hop chains through four registered
// shorteners. Every intermediate hop is requested exactly as served,
// uncleaned (the server sees its utm_source), and the destination is
// cleaned once. A second call is served from the cache.
func TestHopsChain(t *testing.T) {
	rvRequireEntry(t, "t.co", "spotify.link", "amazon", "pinterest")
	chain := []struct{ host, entry, path string }{
		{"t.co", "t.co", "/c1"},
		{"spotify.link", "spotify.link", "/c2"},
		{"amzn.to", "amazon", "/c3"},
		{"pin.it", "pinterest", "/c4"},
	}
	for hops := 1; hops <= 4; hops++ {
		t.Run(strconv.Itoa(hops), func(t *testing.T) {
			n := installNet(t)
			servers := make([]*testServer, hops)
			var from, to []string
			for i := 0; i < hops; i++ {
				reqURI := chain[i].path
				if i > 0 {
					reqURI += "?utm_source=hop"
				}
				loc := "https://example.net/end?utm_source=x&id=1"
				if i+1 < hops {
					loc = "https://" + chain[i+1].host + chain[i+1].path + "?utm_source=hop"
				}
				servers[i] = n.HandleTLS(chain[i].host, rvRoutes{reqURI: rvRedirect(http.StatusMovedPermanently, loc)})
				from = append(from, "https://"+chain[i].host+reqURI)
				to = append(to, loc)
			}
			dest := n.HandleTLS("example.net", rvRedirect(http.StatusInternalServerError, ""))
			r := rvResolver(t, n, Config{})
			raw := "https://t.co/c1"
			res := rvMust(t, r, raw)
			if res.URL != "https://example.net/end?id=1" || !res.Resolved || res.Cached {
				t.Errorf("URL %q Resolved %v Cached %v; want https://example.net/end?id=1, true, false", res.URL, res.Resolved, res.Cached)
			}
			if len(res.Hops) != hops {
				t.Fatalf("got %d hops %+v, want %d", len(res.Hops), res.Hops, hops)
			}
			for i, h := range res.Hops {
				want := Hop{From: from[i], To: to[i], Entry: chain[i].entry, Strategy: "Redirect", Status: 301, Requests: 1}
				if h != want {
					t.Errorf("hop %d = %+v, want %+v", i, h, want)
				}
			}
			for i, s := range servers {
				reqs := s.Requests()
				if len(reqs) != 1 {
					t.Fatalf("%s got %d requests, want 1", chain[i].host, len(reqs))
				}
				if i > 0 && reqs[0].RawQuery != "utm_source=hop" {
					t.Errorf("%s was requested with query %q, want the hop as served (utm_source=hop)", chain[i].host, reqs[0].RawQuery)
				}
			}
			if dest.Hits() != 0 {
				t.Errorf("the destination was requested %d times", dest.Hits())
			}
			again := rvMust(t, r, raw)
			if !again.Cached || again.URL != res.URL || !reflect.DeepEqual(again.Hops, res.Hops) {
				t.Errorf("second call = %+v, want the first's from the cache", again)
			}
			for i, s := range servers {
				if s.Hits() != 1 {
					t.Errorf("%s hit %d times after a cached call, want 1", chain[i].host, s.Hits())
				}
			}
		})
	}
}

// TestHopsPinterest: pin.it → api.pinterest.com (Intermediate, requested
// uncleaned) → www.pinterest.com (never requested).
func TestHopsPinterest(t *testing.T) {
	rvRequireEntry(t, "pinterest", "pinterest-api")
	n := installNet(t)
	api := "https://api.pinterest.com/url_shortener/abc/redirect/?utm_source=x"
	pin := n.HandleTLS("pin.it", rvRoutes{"/abc": rvRedirect(301, api)})
	apiS := n.HandleTLS("api.pinterest.com", rvRoutes{"/url_shortener/abc/redirect/?utm_source=x": rvRedirect(302, "https://www.pinterest.com/pin/123/?utm_source=y")})
	www := n.HandleTLS("www.pinterest.com", rvRedirect(500, ""))
	r := rvResolver(t, n, Config{})
	res := rvMust(t, r, "https://pin.it/abc")
	if res.URL != "https://www.pinterest.com/pin/123/" {
		t.Errorf("URL = %q", res.URL)
	}
	if len(res.Hops) != 2 || res.Hops[0].Entry != "pinterest" || res.Hops[1].Entry != "pinterest-api" || res.Hops[1].From != api {
		t.Errorf("hops = %+v", res.Hops)
	}
	if pin.Hits() != 1 || apiS.Hits() != 1 || www.Hits() != 0 {
		t.Errorf("hits pin.it %d, api %d, www %d; want 1, 1, 0", pin.Hits(), apiS.Hits(), www.Hits())
	}
	if reqs := apiS.Requests(); len(reqs) == 1 && reqs[0].RawQuery != "utm_source=x" {
		t.Errorf("api.pinterest.com saw query %q, want the hop uncleaned", reqs[0].RawQuery)
	}
}

// TestHopsShareGoogle: share.google → www.google.com/share.google…
// (Intermediate, requested uncleaned) → the destination.
func TestHopsShareGoogle(t *testing.T) {
	rvRequireEntry(t, "share.google", "share.google-hop")
	n := installNet(t)
	hop := "https://www.google.com/share.google?q=abc&utm_source=x"
	sg := n.HandleTLS("share.google", rvRoutes{"/abc": rvRedirect(302, hop)})
	g := n.HandleTLS("www.google.com", rvRoutes{"/share.google?q=abc&utm_source=x": rvRedirect(302, "https://example.net/a?utm_source=z&id=2")})
	dest := n.HandleTLS("example.net", rvRedirect(500, ""))
	r := rvResolver(t, n, Config{})
	res := rvMust(t, r, "https://share.google/abc")
	if res.URL != "https://example.net/a?id=2" {
		t.Errorf("URL = %q", res.URL)
	}
	if len(res.Hops) != 2 || res.Hops[1].Entry != "share.google-hop" || res.Hops[1].From != hop {
		t.Errorf("hops = %+v", res.Hops)
	}
	if sg.Hits() != 1 || g.Hits() != 1 || dest.Hits() != 0 {
		t.Errorf("hits share.google %d, www.google.com %d, destination %d; want 1, 1, 0", sg.Hits(), g.Hits(), dest.Hits())
	}
}

// TestHopsOffAllowlistNotRequested: a hop to a host outside the registry is
// returned without being requested.
func TestHopsOffAllowlistNotRequested(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://example.net/p?utm_source=x")})
	dest := n.HandleTLS("example.net", rvRedirect(301, "https://example.org/"))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	res := rvMust(t, r, "https://s.example.net/a")
	if res.URL != "https://example.net/p" || dest.Hits() != 0 {
		t.Errorf("URL %q, destination hits %d; want https://example.net/p, 0", res.URL, dest.Hits())
	}
}

// TestHopsWrapperToShortener: an input that unwraps offline to a short
// link resolves it; the wrapper is never requested.
func TestHopsWrapperToShortener(t *testing.T) {
	rvRequireEntry(t, "t.co")
	n := installNet(t)
	tco := n.HandleTLS("t.co", rvRoutes{"/abc": rvRedirect(301, "https://example.net/p?utm_source=x")})
	fb := n.HandleTLS("l.facebook.com", rvRedirect(500, ""))
	r := rvResolver(t, n, Config{})
	raw := "https://l.facebook.com/l.php?u=https%3A%2F%2Ft.co%2Fabc&h=AT0"
	res := rvMust(t, r, raw)
	if res.URL != "https://example.net/p" || tco.Hits() != 1 || fb.Hits() != 0 {
		t.Errorf("URL %q, t.co hits %d, l.facebook.com hits %d; want https://example.net/p, 1, 0", res.URL, tco.Hits(), fb.Hits())
	}
	pr, err := r.Process(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if got := rvHopCats(pr.Hops); got != "redirect l.facebook.com, unshortened t.co" {
		t.Errorf("Process hops = %s", got)
	}
}

// TestHopsUnwrapContinues: a destination that unwraps offline to another
// registered short link continues the loop; the wrapper is never requested.
func TestHopsUnwrapContinues(t *testing.T) {
	rvRequireEntry(t, "t.co", "amazon")
	n := installNet(t)
	n.HandleTLS("t.co", rvRoutes{"/abc": rvRedirect(301, "https://l.facebook.com/l.php?u=https%3A%2F%2Famzn.to%2Fxyz&h=AT0")})
	amz := n.HandleTLS("amzn.to", rvRoutes{"/xyz": rvRedirect(301, "https://www.amazon.com/dp/B0?ref_=x")})
	fb := n.HandleTLS("l.facebook.com", rvRedirect(500, ""))
	r := rvResolver(t, n, Config{})
	res := rvMust(t, r, "https://t.co/abc")
	if res.URL != "https://www.amazon.com/dp/B0" || len(res.Hops) != 2 || amz.Hits() != 1 || fb.Hits() != 0 {
		t.Errorf("URL %q, %d hops, amzn.to hits %d, l.facebook.com hits %d", res.URL, len(res.Hops), amz.Hits(), fb.Hits())
	}
	if len(res.Hops) == 2 && res.Hops[1].From != "https://amzn.to/xyz" {
		t.Errorf("second hop From = %q", res.Hops[1].From)
	}
}

// rvHopCats renders hops as "category fromHost, …".
func rvHopCats(hs []linkclean.Hop) string {
	var parts []string
	for _, h := range hs {
		c, _ := h.Category.MarshalText()
		parts = append(parts, string(c)+" "+h.FromHost)
	}
	return strings.Join(parts, ", ")
}

// --- Scheme (DR-F11) ---

// TestSchemeHTTPInput: http:// and upper-case inputs match the scheme-"*"
// pattern, and exactly one HTTPS request is made; the plain-http listener
// is never hit. An explicit :80 is dropped by the upgrade (Q2).
func TestSchemeHTTPInput(t *testing.T) {
	rvRequireEntry(t, "t.co")
	for _, raw := range []string{"http://t.co/abc", "HTTP://T.CO/abc", "http://t.co:80/abc"} {
		t.Run(raw, func(t *testing.T) {
			n := installNet(t)
			tls := n.HandleTLS("t.co", rvRoutes{"/abc": rvRedirect(301, "https://example.net/p")})
			plain := n.HandleHTTP("t.co", rvRedirect(500, ""))
			r := rvResolver(t, n, Config{})
			res := rvMust(t, r, raw)
			if res.URL != "https://example.net/p" {
				t.Errorf("URL = %q", res.URL)
			}
			if tls.Hits() != 1 || plain.Hits() != 0 {
				t.Errorf("https hits %d, http hits %d; want 1, 0", tls.Hits(), plain.Hits())
			}
			if len(res.Hops) == 1 && res.Hops[0].From != "https://t.co/abc" {
				t.Errorf("Hop.From = %q, want https://t.co/abc", res.Hops[0].From)
			}
			if reqs := tls.Requests(); len(reqs) == 1 && reqs[0].Host != "t.co" {
				t.Errorf("Host header %q, want t.co", reqs[0].Host)
			}
		})
	}
}

// TestSchemeMatchHTTP: Match is scheme-agnostic and offline.
func TestSchemeMatchHTTP(t *testing.T) {
	rvRequireEntry(t, "t.co", "amazon")
	r := newTestResolver(t, Config{})
	for raw, want := range map[string]string{
		"http://amzn.to/x":  "amazon",
		"https://amzn.to/x": "amazon",
		"HTTP://T.CO/x":     "t.co",
		"http://t.co:80/x":  "t.co",
	} {
		if e, ok := r.Match(raw); !ok || e != want {
			t.Errorf("Match(%q) = %q, %v; want %q, true", raw, e, ok, want)
		}
	}
}

// --- Validation ---

// TestValidateLocation: relative and protocol-relative Locations resolve
// against the request URL; an http Location to a registered host is
// requested over https.
func TestValidateLocation(t *testing.T) {
	t.Run("relative", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLS("s.example.net", rvRoutes{
			"/a":     rvRedirect(301, "/b?x=1"),
			"/b?x=1": rvRedirect(301, "https://example.net/p"),
		})
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
		res := rvMust(t, r, "https://s.example.net/a")
		if res.URL != "https://example.net/p" || len(res.Hops) != 2 || res.Hops[0].To != "https://s.example.net/b?x=1" || s.Hits() != 2 {
			t.Errorf("URL %q hops %+v hits %d", res.URL, res.Hops, s.Hits())
		}
	})
	t.Run("protocol-relative", func(t *testing.T) {
		n := installNet(t)
		n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "//example.net/p?utm_source=x")})
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
		res := rvMust(t, r, "https://s.example.net/a")
		if res.URL != "https://example.net/p" || len(res.Hops) != 1 || res.Hops[0].To != "https://example.net/p?utm_source=x" {
			t.Errorf("URL %q hops %+v", res.URL, res.Hops)
		}
	})
	t.Run("http-upgrade", func(t *testing.T) {
		n := installNet(t)
		n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "http://t.example.net/b")})
		tls := n.HandleTLS("t.example.net", rvRoutes{"/b": rvRedirect(301, "https://example.net/p")})
		plain := n.HandleHTTP("t.example.net", rvRedirect(500, ""))
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"), rvEntry("t.example.net"))})
		res := rvMust(t, r, "https://s.example.net/a")
		if res.URL != "https://example.net/p" || tls.Hits() != 1 || plain.Hits() != 0 {
			t.Errorf("URL %q, https hits %d, http hits %d", res.URL, tls.Hits(), plain.Hits())
		}
		if len(res.Hops) == 2 && (res.Hops[0].To != "http://t.example.net/b" || res.Hops[1].From != "https://t.example.net/b") {
			t.Errorf("hops %+v: To must be as returned, From the upgraded request form", res.Hops)
		}
	})
}

// rvBadTargets are next URLs validateNext must reject: each as a Location
// is FailBadTarget. The IP and IDNA forms are C2.10's.
var rvBadTargets = []string{
	"https://u:p@example.net/",
	"https://u@example.net/",
	"https://example.net:8080/",
	"https://93.184.216.34/",
	"https://127.1/",
	"https://0x7f.0.0.1/",
	"https://2130706433/",
	"https://0177.0.0.1/",
	"https://0x7f000001/",
	"https://\uff11\uff12\uff17\uff0e\uff10\uff0e\uff10\uff0e\uff11/",
	"https://127.0.0.1./",
	"https://[::ffff:7f00:1]/",
	"https://[::127.0.0.1]/",
	"https://[fe80::1%25eth0]/",
	"https://intranet/",
	"https://evil.example/x", // no ICANN site (Q4)
	"ftp://example.net/",
	"https://example.net/" + strings.Repeat("a", 8<<10),
}

// TestValidateRejected: every bad target, and a present but unparsable
// Location on a 301, fail as ErrBadTarget with URL = Clean(raw), and are
// cached (the second call makes no request).
func TestValidateRejected(t *testing.T) {
	locs := append([]string{"https://example.net/%zz"}, rvBadTargets...)
	routes := rvRoutes{}
	for i, loc := range locs {
		routes["/v"+strconv.Itoa(i)] = rvRedirect(301, loc)
	}
	n := installNet(t)
	s := n.HandleTLS("s.example.net", routes)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	for i, loc := range locs {
		raw := "https://s.example.net/v" + strconv.Itoa(i)
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			res, err := r.Resolve(context.Background(), raw)
			rvFailOpen(t, r, raw, res, err, ErrBadTarget)
			before := s.Hits()
			res, err = r.Resolve(context.Background(), raw)
			rvFailOpen(t, r, raw, res, err, ErrBadTarget)
			if s.Hits() != before {
				t.Errorf("Location %.80q: a cached ErrBadTarget made a request", loc)
			}
		})
	}
	if u := n.Unmapped(); len(u) != 0 {
		t.Errorf("dials to unmapped hosts: %q", u)
	}
}

// TestValidateRawNonASCII: a Location with raw non-ASCII or latin-1 bytes
// either validates (and the next URL passes rawurl.ParseTarget) or is
// ErrBadTarget. It never panics.
func TestValidateRawNonASCII(t *testing.T) {
	locs := []string{
		"https://example.net/caf\xc3\xa9?q=\xc3\xa9",
		"https://example.net/\xe9",
		"https://ex\xc3\xa4mple.net/",
		"https://ex\xe4mple.net/",
	}
	routes := rvRoutes{}
	for i, loc := range locs {
		routes["/n"+strconv.Itoa(i)] = rvRedirect(301, loc)
	}
	n := installNet(t)
	n.HandleTLSH1("s.example.net", routes)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	for i := range locs {
		raw := "https://s.example.net/n" + strconv.Itoa(i)
		res, err := r.Resolve(context.Background(), raw)
		if err != nil {
			rvFailOpen(t, r, raw, res, err, ErrBadTarget)
			continue
		}
		if len(res.Hops) != 1 {
			t.Fatalf("%d hops", len(res.Hops))
		}
		if _, ok := rawurl.ParseTarget(res.Hops[0].To); !ok {
			t.Errorf("accepted next %q fails rawurl.ParseTarget", res.Hops[0].To)
		}
		rvInvariant(t, r, res)
	}
}

// TestValidateDirectGet: a custom strategy's own Get of an IP or IDNA form,
// a %-host, an ambiguous authority, userinfo, an opaque URL, a non-default
// port or plain http is ErrBadTarget before any dial; a host outside the
// entry is ErrFetchDenied.
func TestValidateDirectGet(t *testing.T) {
	type target struct {
		u    *url.URL
		want error
	}
	https := func(host string) *url.URL { return &url.URL{Scheme: "https", Host: host, Path: "/x"} }
	targets := []target{
		{https("2130706433"), ErrBadTarget},
		{https("0177.0.0.1"), ErrBadTarget},
		{https("0x7f000001"), ErrBadTarget},
		{https("\uff11\uff12\uff17\uff0e\uff10\uff0e\uff10\uff0e\uff11"), ErrBadTarget},
		{https("127.0.0.1."), ErrBadTarget},
		{https("[::ffff:7f00:1]"), ErrBadTarget},
		{https("[::127.0.0.1]"), ErrBadTarget},
		{https("[fe80::1%eth0]"), ErrBadTarget},
		{https("127.1"), ErrBadTarget},
		{https("d%2eexample.net"), ErrBadTarget},
		{https(`d.example.net\@evil.example.net`), ErrBadTarget},
		{https("d.example.net:8443"), ErrBadTarget},
		{&url.URL{Scheme: "https", User: url.User("u"), Host: "d.example.net", Path: "/x"}, ErrBadTarget},
		{&url.URL{Scheme: "https", Opaque: "//d.example.net/x"}, ErrBadTarget},
		{&url.URL{Scheme: "http", Host: "d.example.net", Path: "/x"}, ErrBadTarget},
		{https("other.example.net"), ErrFetchDenied},
	}
	var mu sync.Mutex
	got := map[string]error{}
	strat := rvStrategy{name: "Direct", next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		i, _ := strconv.Atoi(strings.TrimPrefix(u.Path, "/t"))
		_, err := f.Get(ctx, targets[i].u, false)
		mu.Lock()
		got[u.Path] = err
		mu.Unlock()
		if err == nil {
			return nil, ErrNoMatch
		}
		return nil, err
	}}
	n := installNet(t)
	d := n.HandleTLS("d.example.net", rvRedirect(500, ""))
	other := n.HandleTLS("other.example.net", rvRedirect(500, ""))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, Entry{Name: "direct", Match: []string{"*://d.example.net/*"}, Strategy: strat})})
	for i, tg := range targets {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			raw := "https://d.example.net/t" + strconv.Itoa(i)
			res, err := r.Resolve(context.Background(), raw)
			rvFailOpen(t, r, raw, res, err, tg.want)
			mu.Lock()
			gerr := got["/t"+strconv.Itoa(i)]
			mu.Unlock()
			if !errors.Is(gerr, tg.want) {
				t.Errorf("Get(%#v) = %v, want %v", tg.u, gerr, tg.want)
			}
		})
	}
	if d.Hits() != 0 || other.Hits() != 0 {
		t.Errorf("hits d %d, other %d; want 0", d.Hits(), other.Hits())
	}
	if u := n.Unmapped(); len(u) != 0 {
		t.Errorf("refused Gets dialed: %q", u)
	}
}

// --- Loop, hop limit, budget ---

func TestLoop(t *testing.T) {
	t.Run("two-hop", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLS("s.example.net", rvRoutes{
			"/a": rvRedirect(301, "https://s.example.net/b"),
			"/b": rvRedirect(301, "https://s.example.net/a#x"),
		})
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
		raw := "https://s.example.net/a?utm_source=q"
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrLoop)
		if s.Hits() != 2 {
			t.Errorf("hits %d, want 2", s.Hits())
		}
		res, err = r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrLoop)
		if s.Hits() != 2 {
			t.Errorf("a recomputed loop made requests: hits %d", s.Hits())
		}
	})
	t.Run("self-upgraded", func(t *testing.T) {
		// http input upgraded to https; the Location is the https form (Q30).
		n := installNet(t)
		s := n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://s.example.net/a")})
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
		raw := "http://s.example.net/a"
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrLoop)
		if s.Hits() != 1 {
			t.Errorf("hits %d, want 1", s.Hits())
		}
	})
}

// TestHopLimit: a chain longer than MaxHops fails with ErrHopLimit after
// exactly MaxHops requests; 0 means 4 and more than 5 means 5.
func TestHopLimit(t *testing.T) {
	routes := rvRoutes{}
	for i := 1; i <= 7; i++ {
		routes["/"+strconv.Itoa(i)] = rvRedirect(301, "https://s.example.net/"+strconv.Itoa(i+1))
	}
	routes["/8"] = rvRedirect(301, "https://example.net/end")
	n := installNet(t)
	s := n.HandleTLS("s.example.net", routes)
	reg := rvRegistry(t, rvEntry("s.example.net"))
	for _, tc := range []struct{ maxHops, requests int }{{1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 5}, {0, 4}, {9, 5}} {
		t.Run(strconv.Itoa(tc.maxHops), func(t *testing.T) {
			r := rvResolver(t, n, Config{Registry: reg, MaxHops: tc.maxHops})
			before := s.Hits()
			raw := "https://s.example.net/1"
			res, err := r.Resolve(context.Background(), raw)
			rvFailOpen(t, r, raw, res, err, ErrHopLimit)
			if got := s.Hits() - before; got != tc.requests {
				t.Errorf("MaxHops %d: %d requests, want %d", tc.maxHops, got, tc.requests)
			}
		})
	}
	t.Run("within", func(t *testing.T) {
		r := rvResolver(t, n, Config{Registry: reg, MaxHops: 5})
		res := rvMust(t, r, "https://s.example.net/4")
		if res.URL != "https://example.net/end" || len(res.Hops) != 5 {
			t.Errorf("URL %q, %d hops; want https://example.net/end, 5", res.URL, len(res.Hops))
		}
	})
}

// TestBudget: a strategy's third Get is ErrBudget and sends nothing; the
// server sees exactly 2 requests; the ErrBudget outcome is cached.
func TestBudget(t *testing.T) {
	var errs [3]error
	strat := rvStrategy{name: "Greedy", next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		for i := range errs {
			_, errs[i] = f.Get(ctx, u, false)
		}
		return nil, errs[2]
	}}
	n := installNet(t)
	s := n.HandleTLS("b.example.net", rvBody("ok"))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, Entry{Name: "budget", Match: []string{"*://b.example.net/*"}, Strategy: strat})})
	raw := "https://b.example.net/x"
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrBudget)
	if errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], ErrBudget) {
		t.Errorf("Get errors %v, want nil, nil, ErrBudget", errs)
	}
	if s.Hits() != 2 {
		t.Errorf("server saw %d requests, want 2", s.Hits())
	}
	res, err = r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrBudget)
	if s.Hits() != 2 {
		t.Errorf("a cached ErrBudget made requests: %d", s.Hits())
	}
}

// TestBudgetConcurrent: 8 goroutines of one strategy call Get at once; at
// most 2 pass the budget (run under -race).
func TestBudgetConcurrent(t *testing.T) {
	var passed, refused atomic.Int64
	strat := rvStrategy{name: "Burst", next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := f.Get(ctx, u, false)
				switch {
				case err == nil:
					passed.Add(1)
				case errors.Is(err, ErrBudget):
					refused.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		return nil, ErrNoMatch
	}}
	n := installNet(t)
	s := n.HandleTLS("b.example.net", rvBody("ok"))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, Entry{Name: "burst", Match: []string{"*://b.example.net/*"}, Strategy: strat})})
	raw := "https://b.example.net/x"
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrNoMatch)
	if passed.Load() > 2 || passed.Load()+refused.Load() != 8 {
		t.Errorf("%d Gets passed and %d were refused with ErrBudget; want at most 2 and 8 in all", passed.Load(), refused.Load())
	}
	if s.Hits() > 2 {
		t.Errorf("server saw %d requests, want at most 2", s.Hits())
	}
}

// --- Lookup and skipped inputs ---

// TestLookupRequestable: a first hop with userinfo, a non-default port or
// an IP host matches nothing: no request, the offline result and a nil
// error (Q2). A default port is requestable.
func TestLookupRequestable(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	for _, raw := range []string{
		"https://u@s.example.net/x",
		"https://u:p@s.example.net/x?utm_source=q",
		"https://s.example.net:8443/x",
		"http://s.example.net:443/x",
		"https://93.184.216.34/x",
		"https://[2001:db8::1]/x",
	} {
		if e, ok := r.Match(raw); ok {
			t.Errorf("Match(%q) = %q, true; want no match", raw, e)
		}
		res, err := r.Resolve(context.Background(), raw)
		if err != nil || res.URL != r.c.Clean(raw) || len(res.Hops) != 0 || res.Resolved {
			t.Errorf("Resolve(%q) = %+v, %v; want the offline result and nil", raw, res, err)
		}
	}
	if s.Hits() != 0 {
		t.Errorf("%d requests, want 0", s.Hits())
	}
	if e, ok := r.Match("https://s.example.net:443/x"); !ok || e != "s.example.net" {
		t.Errorf("Match with the default port = %q, %v; want a match", e, ok)
	}
}

// TestSkippedNoNetwork: an input the offline clean skips (SkipTooLong) is
// returned unchanged and never requested, even though it looks registered:
// requesting it would send its tracking (Q1).
func TestSkippedNoNetwork(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	raw := "https://s.example.net/abc?utm_source=x&pad=" + strings.Repeat("a", 8300)
	if off := r.c.Explain(raw); off.Skipped != linkclean.SkipTooLong {
		t.Fatalf("Explain skipped = %v, want SkipTooLong", off.Skipped)
	}
	if _, ok := r.Match(raw); ok {
		t.Error("Match reports a skipped input")
	}
	res, err := r.Resolve(context.Background(), raw)
	if err != nil || res.URL != raw || len(res.Hops) != 0 {
		t.Errorf("Resolve = %.80q, %d hops, %v; want the input unchanged, nil", res.URL, len(res.Hops), err)
	}
	if s.Hits() != 0 {
		t.Errorf("%d requests, want 0", s.Hits())
	}
}

// TestSkippedDestinationNoRequest: a destination whose offline clean is
// skipped is the result as served; the short link it would unwrap to is
// never requested, and Clean(URL) == URL holds.
func TestSkippedDestinationNoRequest(t *testing.T) {
	t.Run("toolong", func(t *testing.T) {
		long := "https://l.facebook.com/l.php?u=https%3A%2F%2Fs.example.net%2Fsecond&h=AT0&pad=" + strings.Repeat("a", 2100)
		n := installNet(t)
		s := n.HandleTLS("s.example.net", rvRoutes{"/abc": rvRedirect(301, long), "/second": rvRedirect(301, "https://example.net/p")})
		fb := n.HandleTLS("l.facebook.com", rvRedirect(500, ""))
		c, err := linkclean.New(linkclean.Config{MaxURLLength: 2048})
		if err != nil {
			t.Fatal(err)
		}
		r := rvResolver(t, n, Config{Cleaner: c, Registry: rvRegistry(t, rvEntry("s.example.net"))})
		res := rvMust(t, r, "https://s.example.net/abc")
		if res.URL != long || len(res.Hops) != 1 {
			t.Errorf("URL %.80q (%d hops), want the destination as served", res.URL, len(res.Hops))
		}
		if rvPathHits(s, "/second") != 0 || fb.Hits() != 0 {
			t.Errorf("hits /second %d, l.facebook.com %d; want 0", rvPathHits(s, "/second"), fb.Hits())
		}
	})
	t.Run("archive", func(t *testing.T) {
		arch := "https://web.archive.org/web/2024/https://s.example.net/second"
		n := installNet(t)
		s := n.HandleTLS("s.example.net", rvRoutes{"/abc": rvRedirect(301, arch), "/second": rvRedirect(301, "https://example.net/p")})
		wa := n.HandleTLS("web.archive.org", rvRedirect(500, ""))
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
		res := rvMust(t, r, "https://s.example.net/abc")
		if res.URL != arch || rvPathHits(s, "/second") != 0 || wa.Hits() != 0 {
			t.Errorf("URL %q, /second hits %d, archive hits %d", res.URL, rvPathHits(s, "/second"), wa.Hits())
		}
	})
}

// --- Stop (ErrNotShortLink, Q31) ---

func rvStopRegistry(t *testing.T) *Registry {
	return rvRegistry(t,
		rvEntry("s.example.net"),
		Entry{Name: "stop", Match: []string{"*://stop.example.net/*"}, Strategy: rvStopper()},
		Entry{Name: "inter", Match: []string{"*://inter.example.net/*"}, Strategy: rvStopper(), Intermediate: true},
	)
}

// TestStopHop0: a stop at the first hop returns the offline result (fragment
// kept, scheme not upgraded) with a nil error, also from the cache.
func TestStopHop0(t *testing.T) {
	n := installNet(t)
	stop := n.HandleTLS("stop.example.net", rvBody("not a short link"))
	r := rvResolver(t, n, Config{Registry: rvStopRegistry(t)})
	raw := "http://stop.example.net/x?utm_source=q#f"
	off := r.c.Explain(raw)
	for i := range 2 {
		res, err := r.Resolve(context.Background(), raw)
		if err != nil || res.URL != off.URL || len(res.Hops) != 0 || res.Resolved || res.Cached {
			t.Errorf("call %d: %+v, %v; want URL %q, no hops, nil", i, res, err, off.URL)
		}
		pr, err := r.Process(context.Background(), raw)
		if err != nil || !rvSameResult(pr, off) {
			t.Errorf("call %d: Process = %+v, %v; want the offline result", i, pr, err)
		}
	}
	if stop.Hits() != 1 {
		t.Errorf("stop hits %d, want 1 (cached stop)", stop.Hits())
	}
}

// TestStopHop1: a stop on a non-Intermediate entry after a hop returns the
// offline clean of that URL as served.
func TestStopHop1(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://stop.example.net/p?utm_source=x&id=1")})
	stop := n.HandleTLS("stop.example.net", rvBody("not a short link"))
	r := rvResolver(t, n, Config{Registry: rvStopRegistry(t)})
	raw := "https://s.example.net/a"
	for i := range 2 {
		res := rvMust(t, r, raw)
		if res.URL != "https://stop.example.net/p?id=1" || len(res.Hops) != 1 || !res.Resolved || res.Cached != (i == 1) {
			t.Errorf("call %d: %+v", i, res)
		}
	}
	if stop.Hits() != 1 {
		t.Errorf("stop hits %d, want 1", stop.Hits())
	}
	if reqs := stop.Requests(); len(reqs) == 1 && reqs[0].RawQuery != "utm_source=x&id=1" {
		t.Errorf("the registered hop was requested with %q, want it uncleaned", reqs[0].RawQuery)
	}
}

// TestStopIntermediate: a stop on an Intermediate entry fails open: the
// redirector is never returned.
func TestStopIntermediate(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://inter.example.net/p?utm_source=x")})
	inter := n.HandleTLS("inter.example.net", rvBody("not a short link"))
	r := rvResolver(t, n, Config{Registry: rvStopRegistry(t)})
	raw := "https://s.example.net/a?utm_source=q"
	for range 2 {
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrNotShortLink)
		pr, perr := r.Process(context.Background(), raw)
		if !errors.Is(perr, ErrNotShortLink) || pr.URL != r.c.Clean(raw) {
			t.Errorf("Process = %q, %v", pr.URL, perr)
		}
	}
	if inter.Hits() != 1 {
		t.Errorf("inter hits %d, want 1", inter.Hits())
	}
}

// --- Fallbacks (N3) ---

// TestFallbackTargets: each shortener's own fallback page is ErrFallback with
// URL = Clean(raw), also when cached (0 requests); t.co to a real homepage is
// accepted.
func TestFallbackTargets(t *testing.T) {
	t.Run("amzn.to", func(t *testing.T) {
		rvRequireEntry(t, "amazon")
		n := installNet(t)
		amz := n.HandleTLS("amzn.to", rvRoutes{"/a": rvRedirect(301, "https://www.amazon.com/")})
		www := n.HandleTLS("www.amazon.com", rvRedirect(500, ""))
		rvFallbackCase(t, n, "https://amzn.to/a?utm_source=x", amz, www)
	})
	t.Run("vm.tiktok", func(t *testing.T) {
		rvRequireEntry(t, "tiktok")
		n := installNet(t)
		vm := n.HandleTLS("vm.tiktok.com", rvRoutes{"/a": rvRedirect(301, "https://www.tiktok.com/?_r=1")})
		www := n.HandleTLS("www.tiktok.com", http.NotFoundHandler()) // oEmbed: 404, definitive
		rvFallbackCase(t, n, "https://vm.tiktok.com/a", vm, nil)
		if rvPathHits(www, "/") != 0 {
			t.Error("the fallback page was requested")
		}
	})
	t.Run("reddit", func(t *testing.T) {
		rvRequireEntry(t, "reddit-share")
		n := installNet(t)
		rd := n.HandleTLS("www.reddit.com", rvRoutes{"/r/sub/s/abc": rvRedirect(301, "https://www.reddit.com/r/sub/")})
		rvFallbackCase(t, n, "https://www.reddit.com/r/sub/s/abc", rd, nil)
		if rvPathHits(rd, "/r/sub/") != 0 {
			t.Error("the fallback page was requested")
		}
	})
	t.Run("t.co-homepage", func(t *testing.T) {
		rvRequireEntry(t, "t.co")
		n := installNet(t)
		n.HandleTLS("t.co", rvRoutes{"/a": rvRedirect(301, "https://example.com/")})
		r := rvResolver(t, n, Config{})
		if res := rvMust(t, r, "https://t.co/a"); res.URL != "https://example.com/" || !res.Resolved {
			t.Errorf("URL %q Resolved %v; want https://example.com/ accepted", res.URL, res.Resolved)
		}
	})
}

func rvFallbackCase(t *testing.T, n *testNet, raw string, short, dest *testServer) {
	t.Helper()
	r := rvResolver(t, n, Config{})
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrFallback)
	hits := short.Hits()
	res, err = r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrFallback)
	if short.Hits() != hits {
		t.Errorf("a cached fallback made a request")
	}
	if dest != nil && dest.Hits() != 0 {
		t.Errorf("the fallback page was requested %d times", dest.Hits())
	}
}

// --- Policy, fallback outcome, request form (round 2) ---

// TestPolicySkip: under Skip a Marketing entry is unregistered: a hop to it
// is cleaned and never requested, and Match does not report it (U-1).
func TestPolicySkip(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/x": rvRedirect(301, "https://bit.ly/y?utm_source=a")})
	bl := n.HandleTLS("bit.ly", rvRedirect(500, ""))
	reg := rvRegistry(t, rvEntry("s.example.net"), rvBitlyEntry())
	r := rvResolver(t, n, Config{Registry: reg, Generic: Skip})
	res := rvMust(t, r, "https://s.example.net/x")
	if res.URL != "https://bit.ly/y" || bl.Hits() != 0 || len(res.Hops) != 1 {
		t.Errorf("URL %q, bit.ly hits %d, %d hops; want https://bit.ly/y, 0, 1", res.URL, bl.Hits(), len(res.Hops))
	}
	if _, ok := r.Match("https://bit.ly/y"); ok {
		t.Error("Match reports a Marketing entry under Skip")
	}
	if e, ok := r.WithCleaner(nil).Match("https://bit.ly/y"); ok {
		t.Errorf("a view's Match reports %q under Skip", e)
	}
	p := rvResolver(t, n, Config{Registry: reg})
	if e, ok := p.Match("https://bit.ly/y"); !ok || e != "bit.ly" {
		t.Errorf("Match under PreviewOnly = %q, %v; want bit.ly", e, ok)
	}
}

// TestFallbackOutcome: an oEmbed that stalls past ResponseHeaderTimeout and
// a Redirect fallback that answers 200 without Location are transient, not
// a cached ErrNoRedirect; the next call requests again (U-2).
func TestFallbackOutcome(t *testing.T) {
	n := installNet(t)
	vm := n.HandleTLS("vm.tiktok.com", rvRoutes{"/a": rvBody("no location")})
	tr := rvTransport(n, TransportOptions{ResponseHeaderTimeout: 100 * time.Millisecond})
	reg := rvRegistry(t, Entry{
		Name: "tiktok-test", Match: []string{"*://vm.tiktok.com/*"}, FetchHosts: []string{"www.tiktok.com"},
		Strategy: TikTokOEmbed(), Fallback: Redirect(),
	})
	r := rvResolver(t, n, Config{Registry: reg, Transport: tr, Timeout: 3 * time.Second})
	g := rvNewGate(t)
	www := n.HandleTLS("www.tiktok.com", g.hold(rvBody("{}")))
	raw := "https://vm.tiktok.com/a"
	for i := 1; i <= 2; i++ {
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrTransient)
		if errors.Is(err, ErrNoRedirect) {
			t.Errorf("call %d: %v wraps ErrNoRedirect; the outcome must be transient only", i, err)
		}
		r.st.group.Wait()
		if vm.Hits() != i || www.Hits() != i {
			t.Errorf("call %d: vm hits %d, www hits %d; want %d each (not cached)", i, vm.Hits(), www.Hits(), i)
		}
	}
}

// TestRequestFormKey: https://t.co/x#a and HTTP://T.CO/x share one request
// and one record, and no key holds the fragment (U-4).
func TestRequestFormKey(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{"/x": rvRedirect(301, "https://example.net/p")})
	spy := &rvCache{inner: NewMemoryCache(100)}
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Cache: spy})
	first := rvMust(t, r, "https://s.example.net/x#a")
	second := rvMust(t, r, "HTTP://S.EXAMPLE.NET/x")
	if s.Hits() != 1 || !second.Cached || first.Cached {
		t.Errorf("hits %d, Cached %v then %v; want 1 request, the second cached", s.Hits(), first.Cached, second.Cached)
	}
	if reqs := s.Requests(); len(reqs) == 1 && reqs[0].RequestURI != "/x" {
		t.Errorf("RequestURI %q, want /x", reqs[0].RequestURI)
	}
	gets, sets := spy.keys()
	if len(sets) != 1 {
		t.Errorf("%d records stored, want 1: %q", len(sets), sets)
	}
	for _, k := range append(gets, sets...) {
		if strings.Contains(k, "#") {
			t.Errorf("key %q holds a fragment", k)
		}
	}
	if len(first.Hops) == 1 && first.Hops[0].From != "https://s.example.net/x" {
		t.Errorf("Hop.From = %q", first.Hops[0].From)
	}
}

// --- Shared cache across policies (D4) ---

// TestWithCleanerShared: the keep view leaves an awin1 affiliate wrapper
// alone (no request) while the strip view unwraps it to amzn.to and
// resolves it; one t.co link resolved by both views makes one request, and
// each view gets its own policy's result.
func TestWithCleanerShared(t *testing.T) {
	rvRequireEntry(t, "t.co", "amazon")
	n := installNet(t)
	amz := n.HandleTLS("amzn.to", rvRoutes{"/abc": rvRedirect(301, "https://www.amazon.com/dp/B0?tag=aff-20&ref_=x")})
	tco := n.HandleTLS("t.co", rvRoutes{"/abc": rvRedirect(301, "https://www.amazon.com/dp/B0?tag=aff-20&linkCode=ll1&ref_=as_li")})
	awin := n.HandleTLS("www.awin1.com", rvRedirect(500, ""))
	keep := rvResolver(t, n, Config{})
	strip := keep.WithCleaner(rvStrip(t))
	if strip.st != keep.st {
		t.Fatal("WithCleaner does not share the network state")
	}

	wrapper := "https://www.awin1.com/cread.php?awinmid=1&awinaffid=2&ued=https%3A%2F%2Famzn.to%2Fabc"
	if res := rvMust(t, keep, wrapper); res.URL != wrapper || len(res.Hops) != 0 || amz.Hits() != 0 {
		t.Errorf("keep view: URL %q, %d hops, amzn.to hits %d; want the wrapper untouched, no request", res.URL, len(res.Hops), amz.Hits())
	}
	if res := rvMust(t, strip, wrapper); res.URL != "https://www.amazon.com/dp/B0" || amz.Hits() != 1 {
		t.Errorf("strip view: URL %q, amzn.to hits %d; want https://www.amazon.com/dp/B0, 1", res.URL, amz.Hits())
	}
	if awin.Hits() != 0 {
		t.Errorf("the wrapper was requested")
	}

	kp, err := keep.Process(context.Background(), "https://t.co/abc")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := strip.Process(context.Background(), "https://t.co/abc")
	if err != nil {
		t.Fatal(err)
	}
	if tco.Hits() != 1 {
		t.Errorf("t.co hits %d across both views, want 1", tco.Hits())
	}
	if kp.URL != "https://www.amazon.com/dp/B0?tag=aff-20&linkCode=ll1" || rvRemovedNames(kp.Removed) != "ref_:tracker" {
		t.Errorf("keep: %q removed %s", kp.URL, rvRemovedNames(kp.Removed))
	}
	if sp.URL != "https://www.amazon.com/dp/B0" || rvRemovedNames(sp.Removed) != "tag:affiliate linkCode:affiliate ref_:tracker" {
		t.Errorf("strip: %q removed %s", sp.URL, rvRemovedNames(sp.Removed))
	}
	if res := rvMust(t, strip, "https://t.co/abc"); !res.Cached {
		t.Error("the strip view's Resolve was not served from the shared cache")
	}
}

// rvRemovedNames renders removals as "name:category …".
func rvRemovedNames(rs []linkclean.Removal) string {
	var parts []string
	for _, r := range rs {
		c, _ := r.Category.MarshalText()
		parts = append(parts, r.Name+":"+string(c))
	}
	return strings.Join(parts, " ")
}

// --- Affiliate interplay (N2, ER-C5) ---

// rvFixture is one recorded hop fixture (testdata/responses/<host>/<case>.txtar).
type rvFixture struct {
	Entry    string `json:"entry"`
	Strategy string `json:"strategy"`
	Request  string `json:"request"`
	Expect   struct {
		Next    string `json:"next"`
		Failure string `json:"failure"`
		Status  int    `json:"status"`
	} `json:"expect"`
	SetCookie bool `json:"set_cookie"`
	status    int
	header    http.Header
	body      []byte
}

func rvLoadFixture(t *testing.T, path string) *rvFixture {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	_, files := parseTxtar(data)
	fx := &rvFixture{header: http.Header{}}
	for _, f := range files {
		switch f.Name {
		case "meta.json":
			if err := json.Unmarshal(f.Data, fx); err != nil {
				t.Fatalf("%s meta.json: %v", path, err)
			}
		case "response":
			lines := strings.Split(strings.TrimRight(string(f.Data), "\n"), "\n")
			fields := strings.Fields(lines[0])
			if len(fields) < 2 {
				t.Fatalf("%s: status line %q", path, lines[0])
			}
			if fx.status, err = strconv.Atoi(fields[1]); err != nil {
				t.Fatalf("%s: status line %q", path, lines[0])
			}
			for _, l := range lines[1:] {
				if k, v, ok := strings.Cut(l, ":"); ok {
					fx.header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
				}
			}
		case "body":
			fx.body = f.Data
		}
	}
	if fx.SetCookie {
		fx.header.Set("Set-Cookie", "s=REDACTED; Path=/")
	}
	return fx
}

func (fx *rvFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for k, vs := range fx.header {
		w.Header()[k] = vs
	}
	w.WriteHeader(fx.status)
	_, _ = w.Write(fx.body)
}

// TestAmznToAffiliate replays the amzn_to_affiliate fixture under both
// policies: tag and linkCode kept under Keep and stripped under Strip,
// ref_ always stripped; under Strip one CatUnshortened hop from amzn.to and
// Removed exactly tag (Amazon Associates, its ID), linkCode, ref_; the same
// Result again from the cache. R1's shape (a /ref= path, linkCode=ml1) is
// an extra synthetic row.
func TestAmznToAffiliate(t *testing.T) {
	rvRequireEntry(t, "amazon")
	fx := rvLoadFixture(t, filepath.Join("testdata", "responses", "amzn.to", "amzn_to_affiliate.txtar"))
	req, err := url.Parse(fx.Request)
	if err != nil || req.Host != "amzn.to" {
		t.Fatalf("fixture request %q: %v", fx.Request, err)
	}
	next, err := url.Parse(fx.Expect.Next)
	if err != nil {
		t.Fatal(err)
	}
	q := next.Query()
	if q.Get("tag") == "" || q.Get("linkCode") == "" || !q.Has("ref_") {
		t.Fatalf("fixture destination %q lacks tag, linkCode or ref_ (N2's shape)", fx.Expect.Next)
	}
	n := installNet(t)
	srv := n.HandleTLS("amzn.to", rvRoutes{
		req.RequestURI(): fx,
		"/r1":            rvRedirect(301, "https://www.amazon.com/dp/B0EXAMPLE/ref=cm_sw_r?linkCode=ml1&tag=somecreator-20"),
	})
	keep := rvResolver(t, n, Config{})
	strip := keep.WithCleaner(rvStrip(t))
	tag := q.Get("tag")

	kp, err := keep.Process(context.Background(), fx.Request)
	if err != nil {
		t.Fatal(err)
	}
	if kq, _ := url.Parse(kp.URL); kq == nil || kq.Query().Get("tag") != tag || kq.Query().Get("linkCode") == "" || kq.Query().Has("ref_") {
		t.Errorf("keep: %q, want tag and linkCode kept, ref_ removed", kp.URL)
	}
	sp, err := strip.Process(context.Background(), fx.Request)
	if err != nil {
		t.Fatal(err)
	}
	if sq, _ := url.Parse(sp.URL); sq == nil || sq.RawQuery != "" {
		t.Errorf("strip: %q, want no query", sp.URL)
	}
	if got := rvHopCats(sp.Hops); got != "unshortened amzn.to" {
		t.Errorf("strip hops = %s, want unshortened amzn.to", got)
	}
	var names []string
	for _, rm := range sp.Removed {
		names = append(names, rm.Name)
	}
	if strings.Join(names, " ") != "tag linkCode ref_" && strings.Join(names, " ") != "linkCode tag ref_" {
		t.Errorf("strip removed %q, want tag, linkCode, ref_ in query order", names)
	}
	for _, rm := range sp.Removed {
		switch rm.Name {
		case "tag":
			if rm.Category != linkclean.CatAffiliate || rm.Beneficiary != (linkclean.Beneficiary{Network: "Amazon Associates", ID: tag}) {
				t.Errorf("tag removal %+v", rm)
			}
		case "linkCode":
			if rm.Category != linkclean.CatAffiliate || rm.Beneficiary != (linkclean.Beneficiary{Network: "Amazon Associates"}) {
				t.Errorf("linkCode removal %+v", rm)
			}
		case "ref_":
			if rm.Category != linkclean.CatTracker {
				t.Errorf("ref_ removal %+v", rm)
			}
		}
	}
	hits := srv.Hits()
	again, err := strip.Process(context.Background(), fx.Request)
	if err != nil || !rvSameResult(again, sp) || srv.Hits() != hits {
		t.Errorf("cached Process differs or made a request: %+v, %v", again, err)
	}

	// R1's input shape.
	r1, err := strip.Process(context.Background(), "https://amzn.to/r1")
	if err != nil {
		t.Fatal(err)
	}
	if r1.URL != "https://www.amazon.com/dp/B0EXAMPLE" || rvRemovedNames(r1.Removed) != "/ref:path linkCode:affiliate tag:affiliate" {
		t.Errorf("R1 strip: %q removed %s", r1.URL, rvRemovedNames(r1.Removed))
	}
	if k1 := rvMust(t, keep, "https://amzn.to/r1"); k1.URL != "https://www.amazon.com/dp/B0EXAMPLE?linkCode=ml1&tag=somecreator-20" {
		t.Errorf("R1 keep: %q", k1.URL)
	}
}

// --- Categories (MA-2) ---

// TestCategories: every network hop is CatUnshortened from the short host;
// a destination that unwraps offline adds its CatRedirectUnwrapped hop
// after it, and its removals follow the first clean's; a cache hit gives
// the same Result; a failed resolution has no network hop.
func TestCategories(t *testing.T) {
	n := installNet(t)
	dest := "https://l.facebook.com/l.php?u=https%3A%2F%2Fexample.net%2Fa%3Futm_source%3Dx%26id%3D1&h=AT0"
	s := n.HandleTLS("s.example.net", rvRoutes{"/abc": rvRedirect(301, dest), "/fail": http.NotFoundHandler()})
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	raw := "https://s.example.net/abc?utm_source=y"
	pr, err := r.Process(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if pr.URL != "https://example.net/a?id=1" || pr.Input != raw || !pr.Changed {
		t.Errorf("URL %q Input %q Changed %v", pr.URL, pr.Input, pr.Changed)
	}
	if got := rvHopCats(pr.Hops); got != "unshortened s.example.net, redirect l.facebook.com" {
		t.Errorf("hops = %s", got)
	}
	if len(pr.Hops) == 2 {
		if h := pr.Hops[0]; h.From != "https://s.example.net/abc" || h.To != dest || h.Rule != "unshorten:s.example.net" {
			t.Errorf("network hop %+v", h)
		}
	}
	if got := rvRemovedNames(pr.Removed); got != "utm_source:tracker utm_source:tracker" {
		t.Errorf("removed = %s, want the first clean's utm_source, then the destination's", got)
	}
	if len(pr.Removed) == 2 && pr.Removed[0].Value != "y" {
		t.Errorf("first removal %+v, want the first clean's (utm_source=y)", pr.Removed[0])
	}
	again, err := r.Process(context.Background(), raw)
	if err != nil || !rvSameResult(again, pr) || s.Hits() != 1 {
		t.Errorf("cache hit: %+v, %v, hits %d; want the same Result and no request", again, err, s.Hits())
	}

	failRaw := "https://s.example.net/fail?utm_source=y"
	fr, err := r.Process(context.Background(), failRaw)
	if !errors.Is(err, ErrNoRedirect) {
		t.Errorf("failure: %v", err)
	}
	if off := r.c.Explain(failRaw); !rvSameResult(fr, off) {
		t.Errorf("a failed Process = %+v, want the offline result %+v", fr, off)
	}
	for _, h := range fr.Hops {
		if h.Category == linkclean.CatUnshortened {
			t.Errorf("a failed resolution reports a network hop %+v", h)
		}
	}
}

// rvWithFallback is rvEntry(host) with a Fallback.
func rvWithFallback(host string, fallback Strategy) Entry {
	e := rvEntry(host)
	e.Fallback = fallback
	return e
}

// rvSameResult compares two linkclean Results, treating nil and empty
// slices alike.
func rvSameResult(a, b linkclean.Result) bool {
	norm := func(r linkclean.Result) linkclean.Result {
		if len(r.Hops) == 0 {
			r.Hops = nil
		}
		if len(r.Removed) == 0 {
			r.Removed = nil
		}
		if len(r.Kept) == 0 {
			r.Kept = nil
		}
		return r
	}
	return reflect.DeepEqual(norm(a), norm(b))
}
