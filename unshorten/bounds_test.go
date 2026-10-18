package unshorten

// Concurrency, cache and bound rows of the §12.8 table: coalescing, cache
// hits, the Limiter, caller cancellation, OnRequest, the request and
// in-flight bounds (D4, U-3, U-R3-1, U-R3-4), transient and definitive
// failures, and the cache's bounds as seen through the Resolver. The cache and limiter units are cache_test.go's and
// limiter_test.go's. Rows wait on barriers (Joiners, arrival counts,
// group.Wait), never on sleeps, except where a row asserts that something
// does NOT happen for a while.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rvGatedRT holds the requests hold selects until its gate opens, ignoring
// their context, then sends them through inner. A request it holds keeps
// its request slots past its own deadline, which no server stall can do.
// Used with AllowUnguardedTransport; inner is a NewTransport, so the real
// guard is still in the dial path.
type rvGatedRT struct {
	inner *http.Transport
	hold  func(*http.Request) bool
	g     *rvGate
}

func (rt *rvGatedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.hold(req) {
		rt.g.arrived.Add(1)
		<-rt.g.ch
	}
	return rt.inner.RoundTrip(req)
}

func (rt *rvGatedRT) CloseIdleConnections() { rt.inner.CloseIdleConnections() }

// rvFuncRT is an opaque RoundTripper (AllowUnguardedTransport) that never
// dials.
type rvFuncRT struct {
	calls atomic.Int64
	fn    func(*http.Request) (*http.Response, error)
}

func (rt *rvFuncRT) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.calls.Add(1)
	return rt.fn(req)
}

// rvResolveAll starts n Resolves of raw(i) at once, passes each error to
// report as it returns, and returns once all have.
func rvResolveAll(r *Resolver, n int, raw func(i int) string, report func(error)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := r.Resolve(context.Background(), raw(i))
			report(err)
		}()
	}
	close(start)
	wg.Wait()
}

// --- Coalescing ---

// TestCoalesce100: 100 concurrent Resolves of one short link make one
// request, also with NoCache and with an entry too large to store (U-6).
func TestCoalesce100(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache func() Cache
		loc   string
	}{
		{"memory", func() Cache { return NewMemoryCache(100) }, "https://example.net/p"},
		{"nocache", NoCache, "https://example.net/p"},
		{"large", func() Cache { return NewMemoryCache(100) }, "https://example.net/p?x=" + strings.Repeat("a", 2100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := installNet(t)
			r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Cache: tc.cache()})
			g := rvNewGate(t)
			s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, tc.loc)))
			key := rvKey(t, r, "https://s.example.net/x", false)
			var wg sync.WaitGroup
			results := make([]Result, 100)
			errs := make([]error, 100)
			for i := range 100 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i], errs[i] = r.Resolve(context.Background(), "https://s.example.net/x")
				}()
			}
			rvWaitFor(t, "100 joiners", func() bool { return r.st.group.Joiners(key) == 100 })
			g.open()
			wg.Wait()
			for i := range 100 {
				if errs[i] != nil || results[i].URL != r.c.Clean(tc.loc) {
					t.Fatalf("caller %d: %q, %v", i, results[i].URL, errs[i])
				}
			}
			if s.Hits() != 1 {
				t.Errorf("%d requests, want 1", s.Hits())
			}
		})
	}
}

// TestCoalesceCancelOne: cancelling one of three joined callers does not
// fail the others; the canceled one fails as transient with its ctx error.
func TestCoalesceCancelOne(t *testing.T) {
	n := installNet(t)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	g := rvNewGate(t)
	s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
	raw := "https://s.example.net/x"
	key := rvKey(t, r, raw, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 3)
	results := make([]Result, 3)
	canceled := make(chan struct{})
	for i := range 3 {
		c := context.Background()
		if i == 0 {
			c = ctx
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = r.Resolve(c, raw)
			if i == 0 {
				close(canceled)
			}
		}()
	}
	rvWaitFor(t, "3 joiners", func() bool { return r.st.group.Joiners(key) == 3 })
	cancel()
	<-canceled // the canceled caller returns while the hop is still held
	g.open()
	wg.Wait()
	rvFailOpen(t, r, raw, results[0], errs[0], ErrTransient, context.Canceled)
	for i := 1; i < 3; i++ {
		if errs[i] != nil || results[i].URL != "https://example.net/p" {
			t.Errorf("caller %d: %q, %v", i, results[i].URL, errs[i])
		}
	}
	if s.Hits() != 1 {
		t.Errorf("%d requests, want 1", s.Hits())
	}
}

// --- Cache ---

// TestCacheHit: a cached hop makes no request and is reported Cached.
func TestCacheHit(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{"/x": rvRedirect(301, "https://example.net/p")})
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	first := rvMust(t, r, "https://s.example.net/x")
	second := rvMust(t, r, "https://s.example.net/x")
	if first.Cached || !second.Cached || s.Hits() != 1 {
		t.Errorf("Cached %v then %v, hits %d; want false, true, 1", first.Cached, second.Cached, s.Hits())
	}
	first.Cached = true
	if first.URL != second.URL || len(first.Hops) != len(second.Hops) || (len(first.Hops) == 1 && first.Hops[0] != second.Hops[0]) {
		t.Errorf("cached Result %+v differs from %+v", second, first)
	}
}

// TestCacheDefinitiveCached: a cached ErrNoRedirect and a cached fallback
// make no request and keep their sentinel.
func TestCacheDefinitiveCached(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{
		"/gone": http.NotFoundHandler(),
		"/home": rvRedirect(301, "https://home.example.net/"),
	})
	e := rvEntry("s.example.net")
	e.IsFallback = func(_, target *url.URL) bool { return target.Host == "home.example.net" && target.Path == "/" }
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, e)})
	for _, tc := range []struct {
		raw  string
		want error
	}{{"https://s.example.net/gone", ErrNoRedirect}, {"https://s.example.net/home", ErrFallback}} {
		for i := range 2 {
			res, err := r.Resolve(context.Background(), tc.raw)
			rvFailOpen(t, r, tc.raw, res, err, tc.want)
			if path := strings.TrimPrefix(tc.raw, "https://s.example.net"); rvPathHits(s, path) != 1 {
				t.Errorf("call %d of %s: %d requests, want 1", i, tc.raw, rvPathHits(s, path))
			}
		}
	}
}

// TestLimiterCalls: Limiter.Wait is called once per request, with the
// request host, and not for a cached hop.
func TestLimiterCalls(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://t.example.net/b"), "/gone": http.NotFoundHandler()})
	n.HandleTLS("t.example.net", rvRoutes{"/b": rvRedirect(301, "https://example.net/p")})
	lim := &rvLimiter{}
	reg := rvRegistry(t, rvWithFallback("s.example.net", Redirect()), rvEntry("t.example.net"))
	r := rvResolver(t, n, Config{Registry: reg, Limiter: lim})
	rvMust(t, r, "https://s.example.net/a")
	rvMust(t, r, "https://s.example.net/a")
	if lim.count("s.example.net") != 1 || lim.count("t.example.net") != 1 {
		t.Errorf("Waits s %d, t %d; want 1 each", lim.count("s.example.net"), lim.count("t.example.net"))
	}
	// Strategy and Fallback both answer 404: two requests, two Waits.
	_, err := r.Resolve(context.Background(), "https://s.example.net/gone")
	if !errors.Is(err, ErrNoRedirect) {
		t.Errorf("err %v", err)
	}
	if lim.count("s.example.net") != 3 {
		t.Errorf("Waits s %d, want 3", lim.count("s.example.net"))
	}
}

// TestLimiterErrorNotCached: a Limiter error fails open as ErrTransient
// with no request, is not cached, and the next call requests.
func TestLimiterErrorNotCached(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{"/x": rvRedirect(301, "https://example.net/p")})
	var reject atomic.Bool
	reject.Store(true)
	lim := &rvLimiter{err: func(string) error {
		if reject.Load() {
			return errors.New("rate limited")
		}
		return nil
	}}
	infos := &rvInfos{}
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Limiter: lim, OnRequest: infos.record})
	raw := "https://s.example.net/x"
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrTransient)
	if s.Hits() != 0 {
		t.Errorf("a rejected Wait made a request")
	}
	reject.Store(false)
	rvMust(t, r, raw)
	r.st.group.Wait()
	if s.Hits() != 1 || len(infos.all()) != 1 {
		t.Errorf("hits %d, OnRequest calls %d; want 1, 1", s.Hits(), len(infos.all()))
	}
}

// --- Caller cancellation (U-R3-2) ---

// TestCallerCancel: a caller whose ctx is canceled, or whose own deadline
// passes, while its hop is in flight fails with ErrUnresolved, ErrTransient
// and its ctx error, and no URL in the error; the detached hop completes
// and is cached.
func TestCallerCancel(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"canceled", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 300*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := installNet(t)
			r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Timeout: 10 * time.Second})
			g := rvNewGate(t)
			s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
			raw := "https://s.example.net/" + rvToken
			ctx, cancel := tc.ctx()
			defer cancel()
			done := make(chan struct{})
			var res Result
			var err error
			go func() {
				defer close(done)
				res, err = r.Resolve(ctx, raw)
			}()
			rvWaitFor(t, "the request", func() bool { return g.arrived.Load() == 1 })
			if tc.want == context.Canceled {
				cancel()
			}
			<-done
			rvFailOpen(t, r, raw, res, err, ErrTransient, tc.want)
			if strings.Contains(err.Error(), rvToken) {
				t.Errorf("error %q holds the URL", err)
			}
			g.open()
			r.st.group.Wait()
			again := rvMust(t, r, raw)
			if !again.Cached || s.Hits() != 1 {
				t.Errorf("Cached %v, hits %d; want the detached hop cached", again.Cached, s.Hits())
			}
		})
	}
}

// TestCallerPastDeadlineStartsNoKey: a caller already past its deadline at
// hop 1 starts no key and makes no request (C3.6). Hop 0 comes from a Cache
// whose Get returns only once the caller's ctx has ended.
func TestCallerPastDeadlineStartsNoKey(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
	spy := &rvCache{inner: NoCache()}
	spy.get = func(ctx context.Context, key string) (CacheEntry, bool, bool) {
		if !strings.HasSuffix(key, "\x00https://s.example.net/one") {
			return CacheEntry{}, false, false
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return CacheEntry{Next: "https://s.example.net/two", Strategy: "Redirect", Status: 301, Requests: 1}, true, true
	}
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Cache: spy, Timeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	raw := "https://s.example.net/one"
	res, err := r.Resolve(ctx, raw)
	rvFailOpen(t, r, raw, res, err, ErrTransient, context.DeadlineExceeded)
	if total, forHost := r.st.group.InFlight("s.example.net"); total != 0 || forHost != 0 {
		t.Errorf("InFlight = %d, %d; want no key started", total, forHost)
	}
	r.st.group.Wait()
	if s.Hits() != 0 {
		t.Errorf("%d requests, want 0", s.Hits())
	}
}

// --- OnRequest (C1.8, C2.9) ---

// TestOnRequestCalls: one call per request, with entry, strategy, host,
// hop, status and duration and no error on success; none for a cached hop.
func TestOnRequestCalls(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRoutes{"/a": rvRedirect(301, "https://s.example.net/b"), "/b": rvRedirect(302, "https://example.net/p")})
	infos := &rvInfos{}
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), OnRequest: infos.record})
	rvMust(t, r, "https://s.example.net/a")
	r.st.group.Wait()
	got := infos.all()
	if len(got) != 2 {
		t.Fatalf("%d calls, want 2: %+v", len(got), got)
	}
	for i, info := range got {
		want := RequestInfo{Entry: "s.example.net", Strategy: "Redirect", Host: "s.example.net", Hop: i, Status: 301 + i, Duration: info.Duration}
		if info != want || info.Duration <= 0 {
			t.Errorf("call %d = %+v, want %+v with a positive Duration", i, info, want)
		}
	}
	rvMust(t, r, "https://s.example.net/a")
	r.st.group.Wait()
	if len(infos.all()) != 2 {
		t.Errorf("a cached Resolve called OnRequest")
	}
}

// TestOnRequestNotForRefusals: no call for a Fetcher refusal, a Limiter
// rejection or a request-slot wait that ended; the budget's third Get
// makes no call either.
func TestOnRequestNotForRefusals(t *testing.T) {
	t.Run("fetcher", func(t *testing.T) {
		var calls atomic.Int64
		strat := rvStrategy{name: "Refused", next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
			for _, s := range []string{"https://other.example.net/x", "https://127.0.0.1/x", "http://c.example.net/x"} {
				target, _ := url.Parse(s)
				if _, err := f.Get(ctx, target, false); err == nil {
					calls.Add(1)
				}
			}
			for range 3 { // two pass, the third is ErrBudget
				_, _ = f.Get(ctx, u, false)
			}
			return nil, ErrNoMatch
		}}
		n := installNet(t)
		s := n.HandleTLS("c.example.net", rvBody("ok"))
		infos := &rvInfos{}
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, Entry{Name: "c", Match: []string{"*://c.example.net/*"}, Strategy: strat}), OnRequest: infos.record})
		_, err := r.Resolve(context.Background(), "https://c.example.net/x")
		if !errors.Is(err, ErrNoMatch) {
			t.Errorf("err %v", err)
		}
		r.st.group.Wait()
		if calls.Load() != 0 || s.Hits() != 2 || len(infos.all()) != 2 {
			t.Errorf("refused Gets passed %d, hits %d, OnRequest calls %d; want 0, 2, 2", calls.Load(), s.Hits(), len(infos.all()))
		}
	})
	t.Run("limiter", func(t *testing.T) {
		n := installNet(t)
		n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
		infos := &rvInfos{}
		lim := &rvLimiter{err: func(string) error { return errors.New("no") }}
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Limiter: lim, OnRequest: infos.record})
		_, err := r.Resolve(context.Background(), "https://s.example.net/x")
		if !errors.Is(err, ErrTransient) {
			t.Errorf("err %v", err)
		}
		r.st.group.Wait()
		if len(infos.all()) != 0 {
			t.Errorf("OnRequest called for a Limiter rejection: %+v", infos.all())
		}
	})
	t.Run("slot", func(t *testing.T) {
		n := installNet(t)
		n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
		rt := &rvGatedRT{inner: rvTransport(n, TransportOptions{}), hold: func(req *http.Request) bool { return req.URL.Path == "/hold" }}
		infos := &rvInfos{}
		r := rvResolver(t, n, Config{
			Registry: rvRegistry(t, rvEntry("s.example.net")), Transport: rt, AllowUnguardedTransport: true,
			MaxConcurrentRequestsPerHost: 1, Timeout: 300 * time.Millisecond, OnRequest: infos.record,
		})
		rt.g = rvNewGate(t)
		go func() { _, _ = r.Resolve(context.Background(), "https://s.example.net/hold") }()
		rvWaitFor(t, "the slot holder", func() bool { return rt.g.arrived.Load() == 1 })
		_, err := r.Resolve(context.Background(), "https://s.example.net/waiter")
		if !errors.Is(err, ErrTransient) {
			t.Errorf("err %v", err)
		}
		rt.g.open()
		r.st.group.Wait()
		if got := infos.all(); len(got) != 1 {
			t.Errorf("%d OnRequest calls, want 1 (the holder only): %+v", len(got), got)
		}
	})
}

// TestOnRequestBlockingDoesNotHoldKey: a blocked OnRequest holds no flight
// key: Resolve returns, InFlight drops to 0 and a second key for the host
// starts under MaxInFlightPerHost 1.
func TestOnRequestBlockingDoesNotHoldKey(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
	unblock := make(chan struct{})
	var once sync.Once
	var entered, calls atomic.Int64
	onReq := func(RequestInfo) {
		calls.Add(1)
		if entered.Add(1) == 1 {
			<-unblock
		}
	}
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), MaxInFlightPerHost: 1, OnRequest: onReq})
	t.Cleanup(func() { once.Do(func() { close(unblock) }) })
	rvMust(t, r, "https://s.example.net/a")
	rvWaitFor(t, "the blocked OnRequest", func() bool { return entered.Load() == 1 })
	if total, forHost := r.st.group.InFlight("s.example.net"); total != 0 || forHost != 0 {
		t.Errorf("InFlight = %d, %d while OnRequest blocks; want 0, 0", total, forHost)
	}
	rvMust(t, r, "https://s.example.net/b")
	once.Do(func() { close(unblock) })
	r.st.group.Wait()
	if calls.Load() != 2 {
		t.Errorf("%d OnRequest calls, want 2", calls.Load())
	}
}

// --- Request bounds (D4, U-3) ---

// rvHosts returns n FirstParty Redirect entries g1.example.net … gn.example.net.
func rvHosts(n int) (hosts []string, entries []Entry) {
	for i := 1; i <= n; i++ {
		h := "g" + strconv.Itoa(i) + ".example.net"
		hosts = append(hosts, h)
		entries = append(entries, rvEntry(h))
	}
	return hosts, entries
}

// TestBoundGlobalSlots: with MaxConcurrentRequests 8 and 8 requests to
// distinct hosts stalled, a 9th waits for a slot. The slot is released after
// Body.Close, and after a RoundTrip error.
func TestBoundGlobalSlots(t *testing.T) {
	for _, release := range []string{"body-close", "roundtrip-error"} {
		t.Run(release, func(t *testing.T) {
			hosts, entries := rvHosts(9)
			n := installNet(t)
			r := rvResolver(t, n, Config{Registry: rvRegistry(t, entries...), MaxConcurrentRequests: 8, Timeout: 20 * time.Second})
			first, rest := rvNewGate(t), rvNewGate(t)
			ok := rvRedirect(301, "https://example.net/p")
			abort := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
			h1 := ok
			if release == "roundtrip-error" {
				h1 = abort
			}
			n.HandleTLS(hosts[0], first.hold(h1))
			for _, h := range hosts[1:8] {
				n.HandleTLS(h, rest.hold(ok))
			}
			ninth := n.HandleTLS(hosts[8], ok)
			var wg sync.WaitGroup
			for _, h := range hosts[:8] {
				wg.Add(1)
				go func() { defer wg.Done(); _, _ = r.Resolve(context.Background(), "https://"+h+"/x") }()
			}
			rvWaitFor(t, "8 requests in progress", func() bool { return first.arrived.Load()+rest.arrived.Load() == 8 })
			done := make(chan error, 1)
			go func() { _, err := r.Resolve(context.Background(), "https://"+hosts[8]+"/x"); done <- err }()
			time.Sleep(200 * time.Millisecond)
			if ninth.Hits() != 0 {
				t.Fatalf("the 9th request did not wait for a global slot")
			}
			first.open()
			if err := <-done; err != nil {
				t.Errorf("9th: %v", err)
			}
			if ninth.Hits() != 1 || rest.arrived.Load() != 7 {
				t.Errorf("9th hits %d; want 1 with the other 7 still stalled", ninth.Hits())
			}
			rest.open()
			wg.Wait()
		})
	}
}

// TestBoundHostSlots: one host stalls its 4 slots, a 5th request to it
// waits while another host proceeds at once; a host-slot wait that times
// out never reaches the Limiter.
func TestBoundHostSlots(t *testing.T) {
	t.Run("stall", func(t *testing.T) {
		n := installNet(t)
		lim := &rvLimiter{}
		reg := rvRegistry(t, rvEntry("h1.example.net"), rvEntry("h2.example.net"))
		r := rvResolver(t, n, Config{Registry: reg, Limiter: lim, Timeout: 20 * time.Second})
		g := rvNewGate(t)
		h1 := n.HandleTLS("h1.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
		h2 := n.HandleTLS("h2.example.net", rvRedirect(301, "https://example.net/q"))
		var wg sync.WaitGroup
		for i := range 5 {
			if i == 4 {
				rvWaitFor(t, "4 stalled requests", func() bool { return g.arrived.Load() == 4 })
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.Resolve(context.Background(), "https://h1.example.net/"+strconv.Itoa(i))
			}()
		}
		rvMust(t, r, "https://h2.example.net/x")
		time.Sleep(200 * time.Millisecond)
		if h1.Hits() != 4 || h2.Hits() != 1 || lim.count("h1.example.net") != 4 {
			t.Errorf("h1 hits %d, h2 hits %d, h1 Waits %d; want 4, 1, 4", h1.Hits(), h2.Hits(), lim.count("h1.example.net"))
		}
		g.open()
		wg.Wait()
		if h1.Hits() != 5 {
			t.Errorf("h1 hits %d after release, want 5", h1.Hits())
		}
	})
	t.Run("slot-timeout", func(t *testing.T) {
		n := installNet(t)
		n.HandleTLS("h1.example.net", rvRedirect(301, "https://example.net/p"))
		lim := &rvLimiter{}
		rt := &rvGatedRT{inner: rvTransport(n, TransportOptions{}), hold: func(req *http.Request) bool { return req.URL.Path == "/hold" }}
		r := rvResolver(t, n, Config{
			Registry: rvRegistry(t, rvEntry("h1.example.net")), Transport: rt, AllowUnguardedTransport: true,
			Limiter: lim, MaxConcurrentRequestsPerHost: 1, Timeout: 300 * time.Millisecond,
		})
		rt.g = rvNewGate(t)
		go func() { _, _ = r.Resolve(context.Background(), "https://h1.example.net/hold") }()
		rvWaitFor(t, "the slot holder", func() bool { return rt.g.arrived.Load() == 1 })
		raw := "https://h1.example.net/waiter"
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrTransient)
		// The waiter's detached hop ends at its deadline; only the holder's key
		// stays (it is held until the gate opens).
		rvWaitFor(t, "the waiter's hop to end", func() bool { _, forHost := r.st.group.InFlight("h1.example.net"); return forHost == 1 })
		if got := lim.count("h1.example.net"); got != 1 {
			t.Errorf("h1 Waits %d, want 1 (the holder's): a slot wait that failed reached the Limiter", got)
		}
		rt.g.open()
	})
}

// TestBoundInFlight: with MaxInFlight 4 a 5th distinct uncached hop fails
// at once with ErrTransient and no request; 64 goroutines starting distinct
// keys at once start exactly 4 (U-R3-4).
func TestBoundInFlight(t *testing.T) {
	t.Run("fifth", func(t *testing.T) {
		n := installNet(t)
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), MaxInFlight: 4, Timeout: 20 * time.Second})
		g := rvNewGate(t)
		s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
		var wg sync.WaitGroup
		started := make([]error, 4)
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, started[i] = r.Resolve(context.Background(), "https://s.example.net/"+strconv.Itoa(i))
			}()
		}
		rvWaitFor(t, "4 requests", func() bool { return g.arrived.Load() == 4 })
		raw := "https://s.example.net/5"
		start := time.Now()
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrTransient)
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("the 5th hop took %v to fail; want at once", d)
		}
		g.open()
		wg.Wait()
		for i, err := range started {
			if err != nil {
				t.Errorf("started key %d: %v", i, err)
			}
		}
		if s.Hits() != 4 {
			t.Errorf("%d requests, want 4", s.Hits())
		}
	})
	t.Run("64", func(t *testing.T) {
		n := installNet(t)
		r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), MaxInFlight: 4, MaxInFlightPerHost: 64, Timeout: 20 * time.Second})
		g := rvNewGate(t)
		s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
		var transient, ok atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			rvResolveAll(r, 64, func(i int) string { return "https://s.example.net/k" + strconv.Itoa(i) }, func(err error) {
				switch {
				case err == nil:
					ok.Add(1)
				case errors.Is(err, ErrTransient):
					transient.Add(1)
				default:
					t.Errorf("unexpected error %v", err)
				}
			})
		}()
		// 60 fail at once; the 4 started keys wait on the gate. Open it only
		// once all 60 have failed, so no late starter can take a freed key.
		rvWaitFor(t, "60 refusals and 4 started keys", func() bool {
			total, _ := r.st.group.InFlight("s.example.net")
			return transient.Load() == 60 && total == 4 && g.arrived.Load() == 4
		})
		g.open()
		<-done
		if ok.Load() != 4 || transient.Load() != 60 || s.Hits() != 4 {
			t.Errorf("%d succeeded, %d transient, %d requests; want 4, 60, 4", ok.Load(), transient.Load(), s.Hits())
		}
	})
}

// TestBoundFloodDefaults: with the defaults and t.co stalling, 300 unique
// t.co hops hold 16 keys, the other 284 fail at once with ErrTransient and
// make no request, and an uncached amzn.to hop is still fetched (U-R3-1).
func TestBoundFloodDefaults(t *testing.T) {
	rvRequireEntry(t, "t.co", "amazon")
	n := installNet(t)
	amz := n.HandleTLS("amzn.to", rvRoutes{"/x": rvRedirect(301, "https://www.amazon.com/dp/B0")})
	r := newTestResolver(t, Config{Transport: rvTransport(n, TransportOptions{})}) // the default Limiter and bounds
	g := rvNewGate(t)
	tco := n.HandleTLS("t.co", g.hold(rvRedirect(301, "https://example.net/p")))
	var failed atomic.Int64
	var wg sync.WaitGroup
	for i := range 300 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Resolve(context.Background(), "https://t.co/f"+strconv.Itoa(i)); errors.Is(err, ErrTransient) {
				failed.Add(1)
			}
		}()
	}
	rvWaitFor(t, "284 fast failures", func() bool { return failed.Load() >= 284 })
	if _, forHost := r.st.group.InFlight("t.co"); forHost != 16 {
		t.Errorf("t.co holds %d keys, want 16", forHost)
	}
	if tco.Hits() > 4 {
		t.Errorf("t.co got %d requests while stalled, want at most 4 (its host slots)", tco.Hits())
	}
	if res := rvMust(t, r, "https://amzn.to/x"); res.URL != "https://www.amazon.com/dp/B0" || amz.Hits() != 1 {
		t.Errorf("amzn.to: %q, hits %d", res.URL, amz.Hits())
	}
	g.open()
	wg.Wait()
}

// TestBoundSharedFetchHost: the tiktok match hosts all fetch www.tiktok.com
// for oEmbed; with it stalled, each match host holds 16 keys (48 in all)
// and t.co still resolves (C3.5).
func TestBoundSharedFetchHost(t *testing.T) {
	rvRequireEntry(t, "tiktok", "t.co")
	n := installNet(t)
	n.HandleTLS("vm.tiktok.com", rvRedirect(301, "https://www.tiktok.com/@u/video/1"))
	n.HandleTLS("vt.tiktok.com", rvRedirect(301, "https://www.tiktok.com/@u/video/1"))
	n.HandleTLS("t.co", rvRoutes{"/x": rvRedirect(301, "https://example.net/p")})
	r := rvResolver(t, n, Config{Timeout: 20 * time.Second})
	g := rvNewGate(t)
	n.HandleTLS("www.tiktok.com", g.hold(rvRedirect(301, "https://www.tiktok.com/@u/video/1")))
	var failed atomic.Int64
	var wg sync.WaitGroup
	for _, prefix := range []string{"https://vm.tiktok.com/", "https://vt.tiktok.com/", "https://www.tiktok.com/t/"} {
		for i := range 20 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := r.Resolve(context.Background(), prefix+"k"+strconv.Itoa(i)); errors.Is(err, ErrTransient) {
					failed.Add(1)
				}
			}()
		}
	}
	rvWaitFor(t, "12 fast failures", func() bool { return failed.Load() >= 12 })
	for _, h := range []string{"vm.tiktok.com", "vt.tiktok.com", "www.tiktok.com"} {
		if total, forHost := r.st.group.InFlight(h); forHost != 16 || total != 48 {
			t.Errorf("InFlight(%s) = %d, %d; want 48, 16", h, total, forHost)
		}
	}
	if res := rvMust(t, r, "https://t.co/x"); res.URL != "https://example.net/p" {
		t.Errorf("t.co: %q", res.URL)
	}
	g.open()
	wg.Wait()
}

// --- Transient failures: never cached ---

// TestTransientDetachedDeadline: a hop whose detached deadline expires is
// transient and not cached.
func TestTransientDetachedDeadline(t *testing.T) {
	n := installNet(t)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Timeout: 300 * time.Millisecond})
	g := rvNewGate(t)
	s := n.HandleTLS("s.example.net", g.hold(rvRedirect(301, "https://example.net/p")))
	raw := "https://s.example.net/x"
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrTransient)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a hop timeout wraps context.DeadlineExceeded: %v", err)
	}
	r.st.group.Wait()
	g.open()
	rvMust(t, r, raw)
	if s.Hits() != 2 {
		t.Errorf("%d requests, want 2 (the expired hop was not cached)", s.Hits())
	}
}

// TestTransientHTTPStatus: 429, 502, 503 and 504 are transient, wrap a
// *StatusError naming the host and status, and are not cached.
func TestTransientHTTPStatus(t *testing.T) {
	codes := []int{429, 502, 503, 504}
	var mu sync.Mutex
	seen := map[string]int{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		first := seen[r.URL.Path] == 1
		mu.Unlock()
		if first {
			code, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Location", "https://example.net/p")
		w.WriteHeader(301)
	})
	n := installNet(t)
	s := n.HandleTLS("s.example.net", h)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	for _, code := range codes {
		raw := "https://s.example.net/" + strconv.Itoa(code)
		res, err := r.Resolve(context.Background(), raw)
		rvFailOpen(t, r, raw, res, err, ErrTransient)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != code || se.Host != "s.example.net" {
			t.Errorf("%d: StatusError %+v", code, se)
		}
		rvMust(t, r, raw)
	}
	if s.Hits() != 2*len(codes) {
		t.Errorf("%d requests, want %d", s.Hits(), 2*len(codes))
	}
}

// TestTransientTransport: a response-header stall, a dial timeout and a
// stalled TLS handshake are each transient, and the next call requests
// again.
func TestTransientTransport(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  TransportOptions
		setup func(n *testNet) *testServer
	}{
		{"header-stall", TransportOptions{ResponseHeaderTimeout: 100 * time.Millisecond}, func(n *testNet) *testServer {
			stall := make(chan struct{})
			return n.HandleTLS("s.example.net", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-stall:
				case <-r.Context().Done():
				}
			}))
		}},
		{"dial-timeout", TransportOptions{DialTimeout: time.Nanosecond}, func(n *testNet) *testServer {
			return n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
		}},
		{"tls-stall", TransportOptions{TLSHandshakeTimeout: 100 * time.Millisecond}, func(n *testNet) *testServer {
			n.StallTLS("s.example.net")
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := installNet(t)
			s := tc.setup(n)
			infos := &rvInfos{}
			r := rvResolver(t, n, Config{
				Registry: rvRegistry(t, rvEntry("s.example.net")), Transport: rvTransport(n, tc.opts),
				Timeout: 5 * time.Second, OnRequest: infos.record,
			})
			raw := "https://s.example.net/x"
			for range 2 {
				res, err := r.Resolve(context.Background(), raw)
				rvFailOpen(t, r, raw, res, err, ErrTransient)
				if errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("a transport timeout wraps context.DeadlineExceeded: %v", err)
				}
				r.st.group.Wait()
			}
			got := infos.all()
			if len(got) != 2 {
				t.Fatalf("%d OnRequest calls, want 2 (one request per call, not cached)", len(got))
			}
			for _, info := range got {
				if info.Failure != FailTransient || !errors.Is(info.Err, ErrTransient) || errors.Is(info.Err, context.DeadlineExceeded) {
					t.Errorf("RequestInfo %+v: want FailTransient wrapping ErrTransient and not DeadlineExceeded", info)
				}
			}
			if tc.name == "dial-timeout" && s.Hits() != 0 {
				t.Errorf("a timed-out dial reached the server")
			}
		})
	}
}

// --- Definitive failures: cached for NegativeTTL ---

// TestDefinitiveTransport: a refused connection, a TLS verification failure
// and NXDOMAIN are FailTransport (not transient) and cached: the second
// call makes no request.
func TestDefinitiveTransport(t *testing.T) {
	for _, name := range []string{"refused", "tls-verify"} {
		t.Run(name, func(t *testing.T) {
			n := installNet(t)
			var s *testServer
			if name == "refused" {
				n.Refuse("s.example.net")
			} else {
				s = n.WrongCert("s.example.net", rvRedirect(301, "https://example.net/p"))
			}
			infos := &rvInfos{}
			r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), OnRequest: infos.record})
			raw := "https://s.example.net/x"
			for range 2 {
				res, err := r.Resolve(context.Background(), raw)
				rvFailOpen(t, r, raw, res, err)
				if errors.Is(err, ErrTransient) {
					t.Errorf("%s is transient: %v", name, err)
				}
				r.st.group.Wait()
			}
			if got := infos.all(); len(got) != 1 || got[0].Failure != FailTransport {
				t.Errorf("OnRequest calls %+v; want one FailTransport (the second call cached)", got)
			}
			if s != nil && s.Hits() != 0 {
				t.Errorf("the server saw a request over an unverified connection")
			}
		})
	}
	t.Run("nxdomain", func(t *testing.T) {
		rt := &rvFuncRT{fn: func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: &net.OpError{Op: "dial", Net: "tcp",
				Err: &net.DNSError{Err: "no such host", Name: req.URL.Hostname(), IsNotFound: true}}}
		}}
		r := newTestResolver(t, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Transport: rt, AllowUnguardedTransport: true})
		raw := "https://s.example.net/" + rvToken
		for range 2 {
			res, err := r.Resolve(context.Background(), raw)
			rvFailOpen(t, r, raw, res, err)
			if errors.Is(err, ErrTransient) || strings.Contains(err.Error(), rvToken) || strings.Contains(err.Error(), "no such host") {
				t.Errorf("err %q: want definitive, without the URL or the DNS error text", err)
			}
		}
		if rt.calls.Load() != 1 {
			t.Errorf("%d RoundTrips, want 1 (cached)", rt.calls.Load())
		}
	})
}

// TestLimiterFloodRecovery is the real-guard row: after 1,000 immediate
// rejections from the Resolver's own NewHostLimiter(20, 1), a request to
// that host succeeds within about 1/perSecond, because rejections consume
// no token. (If they did, the host would be 50 s in debt and the request
// would fail as transient.) The precise GCRA check is limiter_test.go's.
func TestLimiterFloodRecovery(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("s.example.net", rvRedirect(301, "https://example.net/p"))
	lim := NewHostLimiter(20, 1)
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net")), Limiter: lim, Timeout: 2 * time.Second})
	rvMust(t, r, "https://s.example.net/0")
	rejected := 0
	for deadline := time.Now().Add(20 * time.Second); rejected < 1000; {
		if time.Now().After(deadline) {
			t.Fatalf("only %d rejections", rejected)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		if err := lim.Wait(ctx, "s.example.net"); err != nil {
			rejected++
		}
		cancel()
	}
	start := time.Now()
	rvMust(t, r, "https://s.example.net/1")
	if d := time.Since(start); d > time.Second {
		t.Errorf("the request after the flood took %v; want about 1/perSecond (50ms)", d)
	}
}

// --- Cache bounds through the Resolver (U-6, C2.1) ---

// rvParallel runs fn(i) for i in [0, n) on 8 workers.
func rvParallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	next := atomic.Int64{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

// rvFloodConfig lets a flood of distinct keys run 8 at a time.
func rvFloodConfig(reg *Registry, c Cache) Config {
	return Config{Registry: reg, Cache: c, MaxInFlightPerHost: 64, MaxConcurrentRequestsPerHost: 16}
}

// TestCacheNegativeBound: unique failing keys never evict a cached success.
// NewMemoryCache(100) holds 25 failures; 400 is far past both bounds. (The
// 10,000-key unit is cache_test.go's.)
func TestCacheNegativeBound(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{"/ok": rvRedirect(301, "https://example.net/p")}) // the rest 404
	r := rvResolver(t, n, rvFloodConfig(rvRegistry(t, rvEntry("s.example.net")), NewMemoryCache(100)))
	rvMust(t, r, "https://s.example.net/ok")
	rvParallel(400, func(i int) {
		if _, err := r.Resolve(context.Background(), "https://s.example.net/f"+strconv.Itoa(i)); !errors.Is(err, ErrNoRedirect) {
			t.Errorf("f%d: %v", i, err)
		}
	})
	if res := rvMust(t, r, "https://s.example.net/ok"); !res.Cached || rvPathHits(s, "/ok") != 1 {
		t.Errorf("the success was evicted: Cached %v, hits %d", res.Cached, rvPathHits(s, "/ok"))
	}
}

// TestCacheFallbackFlood: 1,000 unique amzn.to hops answered with Amazon's
// homepage, and then 1,000 stops, do not evict a cached success from
// NewMemoryCache(100): the Resolver stores them in the negative bound (C2.1).
func TestCacheFallbackFlood(t *testing.T) {
	rvRequireEntry(t, "amazon")
	n := installNet(t)
	s := n.HandleTLS("s.example.net", rvRoutes{"/ok": rvRedirect(301, "https://example.net/p")})
	n.HandleTLS("amzn.to", rvRedirect(301, "https://www.amazon.com/"))
	n.HandleTLS("stop.example.net", rvBody("not a short link"))
	reg := rvDefaultWith(t, rvEntry("s.example.net"), Entry{Name: "stop", Match: []string{"*://stop.example.net/*"}, Strategy: rvStopper()})
	r := rvResolver(t, n, rvFloodConfig(reg, NewMemoryCache(100)))
	rvMust(t, r, "https://s.example.net/ok")
	rvParallel(1000, func(i int) {
		if _, err := r.Resolve(context.Background(), "https://amzn.to/f"+strconv.Itoa(i)); !errors.Is(err, ErrFallback) {
			t.Errorf("amzn.to/f%d: %v", i, err)
		}
	})
	if res := rvMust(t, r, "https://s.example.net/ok"); !res.Cached {
		t.Errorf("fallback hops evicted the success")
	}
	rvParallel(1000, func(i int) {
		if _, err := r.Resolve(context.Background(), "https://stop.example.net/s"+strconv.Itoa(i)); err != nil {
			t.Errorf("stop/s%d: %v", i, err)
		}
	})
	if res := rvMust(t, r, "https://s.example.net/ok"); !res.Cached || rvPathHits(s, "/ok") != 1 {
		t.Errorf("stops evicted the success: Cached %v, hits %d", res.Cached, rvPathHits(s, "/ok"))
	}
}

// TestCacheEntryCap: a hop whose key plus Next exceeds 2 KiB is not stored,
// and the next call requests again (U-6).
func TestCacheEntryCap(t *testing.T) {
	n := installNet(t)
	loc := "https://example.net/p?x=" + strings.Repeat("a", 2100)
	s := n.HandleTLS("s.example.net", rvRoutes{"/x": rvRedirect(301, loc)})
	r := rvResolver(t, n, Config{Registry: rvRegistry(t, rvEntry("s.example.net"))})
	rvMust(t, r, "https://s.example.net/x")
	if res := rvMust(t, r, "https://s.example.net/x"); res.Cached || s.Hits() != 2 {
		t.Errorf("Cached %v, hits %d; want the oversized hop requested again", res.Cached, s.Hits())
	}
}
