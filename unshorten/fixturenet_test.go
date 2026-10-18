package unshorten

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// fixtureHandler answers fx's fetch URL with the recorded response
// (Set-Cookie added when set_cookie, the body padded to body_len) and
// anything else with 404.
func fixtureHandler(fx *fixture) http.Handler {
	fetch, _ := url.Parse(fx.fetchURL())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != fetch.RequestURI() {
			http.NotFound(w, r)
			return
		}
		for k, vs := range fx.header {
			w.Header()[k] = vs
		}
		if fx.meta.SetCookie {
			w.Header().Set("Set-Cookie", "s=REDACTED; Path=/")
		}
		w.WriteHeader(fx.status)
		w.Write(fx.replayBody())
	})
}

// serveFixture registers fx's listeners on n: its fetch host serves the
// recording; its request host, when another, serves nothing but must not be
// requested. It returns the two servers (the same one when the hosts are
// equal).
func serveFixture(t *testing.T, n *testNet, fx *fixture) (fetchSrv, reqSrv *testServer) {
	t.Helper()
	fetch, req := mustURL(t, fx.fetchURL()), mustURL(t, fx.meta.Request)
	fetchSrv = n.HandleTLS(fetch.Host, fixtureHandler(fx))
	reqSrv = fetchSrv
	if req.Host != fetch.Host {
		reqSrv = n.HandleTLS(req.Host, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
	}
	return fetchSrv, reqSrv
}

// spyCache misses every Get and records every Set.
type spyCache struct {
	mu   sync.Mutex
	sets map[string]CacheEntry
}

func (c *spyCache) Get(context.Context, string) (CacheEntry, bool) { return CacheEntry{}, false }

func (c *spyCache) Set(_ context.Context, key string, e CacheEntry, _ time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sets == nil {
		c.sets = map[string]CacheEntry{}
	}
	c.sets[key] = e
}

func (c *spyCache) get(key string) (CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.sets[key]
	return e, ok
}

// TestRecordedFixtures replays every fixture through the test network, with
// the real guard and the production transport path: a listener for the
// fixture's fetch host serves the recording. The entry's strategy, run
// through a Fetcher, must make exactly the recorded request and reach
// meta.expect. The Resolver half resolves the fixture's request URL and
// checks the hop record it caches, and, when the next URL leaves the
// registry, the Result.
func TestRecordedFixtures(t *testing.T) {
	all := fixtureEntries(t)
	fxs := loadFixtures(t)
	if len(fxs) == 0 {
		t.Fatal("no fixtures")
	}
	for _, fx := range fxs {
		t.Run(filepath.ToSlash(strings.TrimPrefix(fx.path, filepath.Join("testdata", "responses")+string(filepath.Separator))), func(t *testing.T) {
			e := all.entry(fx.meta.Entry)
			if e == nil {
				t.Fatalf("unknown entry %q", fx.meta.Entry)
			}
			s := e.Strategy
			if fx.meta.Strategy != name(s) {
				s = e.Fallback
			}
			reg, err := NewRegistry(e.Entry)
			if err != nil {
				t.Fatal(err)
			}
			fetch := mustURL(t, fx.fetchURL())

			n := installNet(t)
			fetchSrv, reqSrv := serveFixture(t, n, fx)
			st := netState(t, n, Config{}, reg, nil)
			f, ctx := netFetcher(t, st, e.Name, 5*time.Second)
			next, err := s.Next(ctx, f, mustURL(t, fx.meta.Request))
			recs := f.finish()
			checkFixtureOutcome(t, fx, next, err, recs)
			reqs := fetchSrv.Requests()
			if len(reqs) != 1 || reqs[0].RequestURI != fetch.RequestURI() || reqs[0].Host != fetch.Host {
				t.Errorf("requests %+v, want only GET %s", reqs, fx.fetchURL())
			}
			if reqSrv != fetchSrv && reqSrv.Hits() != 0 {
				t.Errorf("the request host was requested %d times; the strategy fetches %s", reqSrv.Hits(), fetch.Host)
			}
			if len(recs) != 1 || recs[0].wire != 1 {
				t.Errorf("records %+v, want one request written once", recs)
			}
			slotsFree(t, st)

			t.Run("resolve", func(t *testing.T) {
				requireResolver(t)
				resolveFixture(t, n, all, fx, fetchSrv)
			})
		})
	}
}

// resolveFixture resolves fx's request URL with registry all over n, where
// fetchSrv serves fx, and checks the cached hop record against
// meta.expect, then the Result when the next URL leaves the registry.
func resolveFixture(t *testing.T, n *testNet, all *Registry, fx *fixture, fetchSrv *testServer) {
	t.Helper()
	before := fetchSrv.Hits()
	cache := &spyCache{}
	r := newTestResolver(t, Config{
		Registry: all, Cache: cache, Limiter: NoLimit(),
		Transport: guardedTransport(t, n, TransportOptions{}),
	})
	e := all.entry(fx.meta.Entry)
	res, err := r.Resolve(context.Background(), fx.meta.Request)
	r.st.group.Wait()

	wantKind, _ := failureByName(fx.meta.Expect.Failure)
	key := hopKey(e, PreviewOnly, fx.meta.Request)
	got, ok := cache.get(key)
	switch {
	case wantKind == FailTransient:
		if ok {
			t.Errorf("transient hop cached: %+v", got)
		}
	case !ok:
		t.Errorf("no hop record cached under %q", key)
	default:
		want := CacheEntry{Next: fx.meta.Expect.Next, Strategy: fx.meta.Strategy, Status: fx.meta.Expect.Status, Requests: 1, Failure: wantKind}
		if got != want {
			t.Errorf("cached hop %+v, want %+v", got, want)
		}
	}
	if got := fetchSrv.Hits() - before; got < 1 {
		t.Errorf("fetch host hits %d, want at least 1", got)
	}

	if wantKind != FailNone {
		if !errors.Is(err, ErrUnresolved) || res.URL != r.c.Clean(fx.meta.Request) {
			t.Errorf("Resolve = %q, %v; want the cleaned input and ErrUnresolved", res.URL, err)
		}
		return
	}
	nextURL := mustURL(t, fx.meta.Expect.Next)
	if nu, skip := rawurl.Parse(fx.meta.Expect.Next); skip == rawurl.OK {
		if ne, ok := all.lookup(&nu, true, PreviewOnly); ok {
			t.Logf("next %s is registered (%s): the chain continues past this fixture; Result not checked", fx.meta.Expect.Next, ne.Name)
			return
		}
	}
	if e.IsFallback != nil && e.IsFallback(mustURL(t, fx.meta.Request), nextURL) {
		if !errors.Is(err, ErrFallback) || res.URL != r.c.Clean(fx.meta.Request) {
			t.Errorf("Resolve = %q, %v; want the cleaned input and ErrFallback", res.URL, err)
		}
		return
	}
	wantHop := Hop{From: fx.meta.Request, To: fx.meta.Expect.Next, Entry: fx.meta.Entry, Strategy: fx.meta.Strategy, Status: fx.meta.Expect.Status, Requests: 1}
	if err != nil || len(res.Hops) != 1 || res.Hops[0] != wantHop || res.URL != r.c.Clean(fx.meta.Expect.Next) {
		t.Errorf("Resolve = %q, hops %+v, %v; want %q, [%+v]", res.URL, res.Hops, err, r.c.Clean(fx.meta.Expect.Next), wantHop)
	}
}
