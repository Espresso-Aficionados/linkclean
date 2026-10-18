package unshorten

// The Fetcher's network rows: requests over loopback sockets with the real
// guard in the dial path. Each row's Fetcher half builds the state New would
// (newState over the transport New's own check returns, checkTransport, so
// privateClone and its header clamp are in the path) and drives a Fetcher or
// a strategy directly. A row's Resolver half, the "resolve" subtest, goes
// through New.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// netRegistry is testRegistry plus a body strategy entry.
func netRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := testRegistry(t).With(Entry{
		Name: "body", Match: []string{"*://body.example.net/*"},
		Strategy: BodyRegexp(regexp.MustCompile(`<a href="([^"]+)">`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// netState builds the state New would build over tr (a guarded transport
// with the defaults when nil): newState over checkTransport's result. Call
// installNet first, so the idle connections close before the network does.
func netState(t *testing.T, n *testNet, cfg Config, reg *Registry, tr *http.Transport) *state {
	t.Helper()
	if tr == nil {
		tr = guardedTransport(t, n, TransportOptions{})
	}
	rt, err := checkTransport(tr, false, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("checkTransport: %v", err)
	}
	st := newTestState(t, cfg, reg, nil, rt)
	t.Cleanup(st.client.CloseIdleConnections)
	return st
}

// netFetcher returns a Fetcher for entry name whose hop ends after timeout,
// and the hop context.
func netFetcher(t *testing.T, st *state, name string, timeout time.Duration) (*Fetcher, context.Context) {
	t.Helper()
	hop, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return newTestFetcher(t, st, name, hop), hop
}

// slotsFree fails t if a request slot is still held.
func slotsFree(t *testing.T, st *state) {
	t.Helper()
	for h, c := range st.hostSlots {
		if len(c) != 0 {
			t.Errorf("host slot of %s held (%d)", h, len(c))
		}
	}
	if len(st.global) != 0 {
		t.Errorf("global slot held (%d)", len(st.global))
	}
}

// fetchClass returns err's kind and class, failing t unless err is a
// *fetchError.
func fetchClass(t *testing.T, err error) (FailureKind, errClass) {
	t.Helper()
	var fe *fetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v (%T), want a *fetchError", err, err)
	}
	return fe.kind, fe.class
}

// netStrategy is a custom Strategy.
type netStrategy struct {
	name string
	next func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error)
}

func (s netStrategy) Name() string { return s.name }
func (s netStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	return s.next(ctx, f, u)
}

// stallHandler never answers: it returns when the request or the test
// network ends.
func stallHandler(n *testNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-n.Done():
		case <-r.Context().Done():
		}
	})
}

// dripHandler sends a 200 header and then one body byte every 100 ms.
func dripHandler(n *testNet) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(200)
		rc := http.NewResponseController(w)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := w.Write([]byte(" ")); err != nil {
				return
			}
			if rc.Flush() != nil {
				return
			}
			select {
			case <-n.Done():
				return
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
		}
	})
}

// TestNetworkN1Scheme is N1's Fetcher half: the Fetcher requests
// only https for an entry without AllowHTTP, so an http:// or upper-case
// input reaches the TLS listener once, in its request form, and never the
// plain-http one. An AllowHTTP entry is requested over http. N1's Resolver
// half (the scheme upgrade) is TestNetworkGolden/N1.
func TestNetworkN1Scheme(t *testing.T) {
	n := installNet(t)
	tlsSrv := n.HandleTLS("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.net/p?utm_source=x")
		w.WriteHeader(301)
	}))
	plainSrv := n.HandleHTTP("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	allowHTTP := n.HandleHTTP("plain.example", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.net/q")
		w.WriteHeader(302)
	}))
	st := netState(t, n, Config{}, netRegistry(t), nil)

	for _, raw := range []string{"http://t.co/abc", "HTTP://T.CO/abc", "http://t.co:80/abc"} {
		f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
		if _, err := Redirect().Next(ctx, f, mustURL(t, raw)); !errors.Is(err, ErrBadTarget) {
			t.Errorf("Redirect(%s) = %v, want ErrBadTarget: the Fetcher never sends http for t.co", raw, err)
		}
	}
	for _, raw := range []string{"https://t.co/abc", "HTTPS://T.CO/abc#frag"} {
		f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
		next, err := Redirect().Next(ctx, f, mustURL(t, raw))
		if err != nil || next.String() != "https://example.net/p?utm_source=x" {
			t.Errorf("Redirect(%s) = %v, %v", raw, next, err)
		}
	}
	if tlsSrv.Hits() != 2 || plainSrv.Hits() != 0 {
		t.Errorf("https hits %d, http hits %d; want 2, 0", tlsSrv.Hits(), plainSrv.Hits())
	}
	for _, r := range tlsSrv.Requests() {
		if r.RequestURI != "/abc" || r.Host != "t.co" {
			t.Errorf("request %s, Host %q; want /abc, t.co", r.RequestURI, r.Host)
		}
	}
	if n.Dials("t.co", "80") != 0 {
		t.Errorf("t.co:80 dialed %d times", n.Dials("t.co", "80"))
	}

	f, ctx := netFetcher(t, st, "plain", 5*time.Second)
	next, err := Redirect().Next(ctx, f, mustURL(t, "http://plain.example/x"))
	if err != nil || next.String() != "https://example.net/q" || allowHTTP.Hits() != 1 {
		t.Errorf("AllowHTTP entry: %v, %v, %d hits", next, err, allowHTTP.Hits())
	}
}

// TestRequestHeaders checks every request on the wire, over h2 and h1:
// GET only (the server refuses anything else), exactly User-Agent and
// Accept: */*, no Cookie after a Set-Cookie (no jar), no Referer after a
// redirect, no Accept-Encoding (compression is off), no fragment, no body,
// and a Host header equal to the URL host. The test network's own check
// (testServer.check) also runs on every request.
func TestRequestHeaders(t *testing.T) {
	for _, c := range []struct {
		name  string
		h1    bool
		ua    string // Config.UserAgent; "" for the default
		proto string
	}{
		{"h2", false, "", "HTTP/2.0"},
		{"h1", true, "", "HTTP/1.1"},
		{"h2 custom UA", false, "test-agent/1 (+https://example.net/bot)", "HTTP/2.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			n := installNet(t)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					http.Error(w, "GET only", http.StatusMethodNotAllowed)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: "s", Value: "REDACTED", Path: "/", Domain: "t.co"})
				if r.URL.Path == "/first" {
					w.Header().Set("Location", "/second")
					w.WriteHeader(301)
					return
				}
				w.Header().Set("Content-Type", "text/html")
				io.WriteString(w, `<a href="https://example.net/p">`)
			})
			var s *testServer
			if c.h1 {
				s = n.HandleTLSH1("t.co", h)
			} else {
				s = n.HandleTLS("t.co", h)
			}
			ua := defaultUA()
			if c.ua != "" {
				n.SetUserAgent(c.ua)
				ua = c.ua
			}
			st := netState(t, n, Config{UserAgent: c.ua}, netRegistry(t), nil)

			f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
			resp, err := f.Get(ctx, mustURL(t, "HTTPS://T.CO./first?a=1#frag"), false)
			if err != nil || resp.Status != 301 || resp.Location == nil {
				t.Fatalf("first Get: %+v, %v", resp, err)
			}
			if _, err := Redirect().Next(ctx, f, resp.Location); !errors.Is(err, ErrNoRedirect) {
				t.Errorf("Redirect on the 200: %v, want ErrNoRedirect", err)
			}
			f2, ctx2 := netFetcher(t, st, "t.co", 5*time.Second)
			if resp, err := f2.Get(ctx2, mustURL(t, "https://t.co/second"), true); err != nil || !bytes.Contains(resp.Body, []byte("example.net")) {
				t.Errorf("body Get: %+v, %v", resp, err)
			}

			reqs := s.Requests()
			wantURIs := []string{"/first?a=1", "/second", "/second"}
			if len(reqs) != len(wantURIs) {
				t.Fatalf("%d requests, want %d", len(reqs), len(wantURIs))
			}
			for i, r := range reqs {
				if r.Method != http.MethodGet || r.RequestURI != wantURIs[i] || r.Host != "t.co" || r.Proto != c.proto || r.BodyLen != 0 {
					t.Errorf("request %d: %s %s Host %q %s body %d; want GET %s Host t.co %s", i, r.Method, r.RequestURI, r.Host, r.Proto, r.BodyLen, wantURIs[i], c.proto)
				}
				if len(r.Header) != 2 || r.Header.Get("User-Agent") != ua || r.Header.Get("Accept") != "*/*" {
					t.Errorf("request %d: headers %v, want exactly User-Agent %q and Accept */*", i, r.Header, ua)
				}
			}
			if v := n.TakeViolations(); len(v) != 0 {
				t.Errorf("header-set violations: %q", v)
			}
			slotsFree(t, st)
		})
	}

	// The Host header is the URL's host as requested: Go writes the port
	// as written, so an explicit default port is on the wire too.
	t.Run("explicit port", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLSH1("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
		st := netState(t, n, Config{}, netRegistry(t), nil)
		f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
		if _, err := f.Get(ctx, mustURL(t, "https://t.co:443/x"), false); err != nil {
			t.Fatal(err)
		}
		reqs := s.Requests()
		if len(reqs) != 1 || reqs[0].Host != "t.co:443" {
			t.Errorf("requests %+v, want one with Host t.co:443 (the URL's host)", reqs)
		}
	})

	// Through the Resolver: the request form keeps the port as written.
	t.Run("explicit port resolve", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLS("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://example.net/p")
			w.WriteHeader(301)
		}))
		r := newTestResolver(t, Config{Registry: netRegistry(t), Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit()})
		res, err := r.Resolve(context.Background(), "https://t.co:443/x")
		if err != nil || res.URL != "https://example.net/p" {
			t.Fatalf("Resolve = %q, %v", res.URL, err)
		}
		if reqs := s.Requests(); len(reqs) != 1 || reqs[0].Host != "t.co:443" {
			t.Errorf("requests %+v, want one with Host t.co:443", reqs)
		}
	})

	// The Resolver half: a chain whose first hop sets a cookie sends no
	// Cookie or Referer on the second, and requests the registered second
	// hop uncleaned.
	t.Run("resolve", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLS("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.SetCookie(w, &http.Cookie{Name: "s", Value: "REDACTED", Path: "/"})
			switch r.URL.Path {
			case "/first":
				w.Header().Set("Location", "https://t.co/second?utm_source=x")
			default:
				w.Header().Set("Location", "https://example.net/p")
			}
			w.WriteHeader(301)
		}))
		r := newTestResolver(t, Config{Registry: netRegistry(t), Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit()})
		res, err := r.Resolve(context.Background(), "https://t.co/first#frag")
		if err != nil || res.URL != "https://example.net/p" {
			t.Fatalf("Resolve = %q, %v", res.URL, err)
		}
		reqs := s.Requests()
		if len(reqs) != 2 || reqs[0].RequestURI != "/first" || reqs[1].RequestURI != "/second?utm_source=x" {
			t.Errorf("requests %+v", reqs)
		}
		for _, q := range reqs {
			if len(q.Header) != 2 {
				t.Errorf("%s: headers %v", q.RequestURI, q.Header)
			}
		}
	})
}

// TestFetchDenied checks that a Get for a host outside the entry's Match
// hosts and FetchHosts is ErrFetchDenied before any dial, even when the host
// is another entry's.
func TestFetchDenied(t *testing.T) {
	n := installNet(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	evil := n.HandleTLS("evil.example.net", ok)
	tiktok := n.HandleTLS("www.tiktok.com", ok)
	st := netState(t, n, Config{}, netRegistry(t), nil)

	f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
	for _, u := range []string{"https://evil.example.net/x", "https://www.tiktok.com/oembed?url=x", "https://sub.t.co/x"} {
		if _, err := f.Get(ctx, mustURL(t, u), false); !errors.Is(err, ErrFetchDenied) {
			t.Errorf("t.co Get(%s) = %v, want ErrFetchDenied", u, err)
		}
	}
	if evil.Hits() != 0 || tiktok.Hits() != 0 || n.Dials("evil.example.net", "443") != 0 || n.Dials("www.tiktok.com", "443") != 0 {
		t.Errorf("refused hosts reached: %d/%d hits, %d/%d dials", evil.Hits(), tiktok.Hits(),
			n.Dials("evil.example.net", "443"), n.Dials("www.tiktok.com", "443"))
	}
	if len(f.recs) != 0 || f.used != 0 {
		t.Errorf("refusals left %d records, used %d", len(f.recs), f.used)
	}

	// www.tiktok.com is the tiktok entry's FetchHost.
	f, ctx = netFetcher(t, st, "tiktok", 5*time.Second)
	if _, err := f.Get(ctx, mustURL(t, "https://www.tiktok.com/oembed?url=x"), false); err != nil || tiktok.Hits() != 1 {
		t.Errorf("tiktok Get of its FetchHost: %v, %d hits", err, tiktok.Hits())
	}

	// Through the Resolver: a custom strategy's undeclared Get fails the
	// hop with ErrFetchDenied, cached, and never reaches the host.
	t.Run("resolve", func(t *testing.T) {
		var calls atomic.Int32
		evilURL := mustURL(t, "https://evil.example.net/x")
		reg, err := NewRegistry(Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: netStrategy{"Escape",
			func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
				calls.Add(1)
				if _, err := f.Get(ctx, evilURL, false); err != nil {
					return nil, err
				}
				return nil, ErrNoMatch
			}}})
		if err != nil {
			t.Fatal(err)
		}
		r := newTestResolver(t, Config{Registry: reg, Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit()})
		for i := range 2 {
			res, err := r.Resolve(context.Background(), "https://t.co/abc")
			if !errors.Is(err, ErrFetchDenied) || !errors.Is(err, ErrUnresolved) || res.URL != "https://t.co/abc" {
				t.Errorf("call %d: %q, %v; want the input and ErrFetchDenied", i, res.URL, err)
			}
		}
		if calls.Load() != 1 || evil.Hits() != 0 {
			t.Errorf("strategy ran %d times (want 1: the failure is cached), evil.example.net hits %d", calls.Load(), evil.Hits())
		}
	})
}

// TestIntermediateNotFromInput checks that an Intermediate entry's URL
// posted by a user (google.com/share.google…, the Pinterest API's redirect)
// never matches as a first hop: it is cleaned offline and not requested.
func TestIntermediateNotFromInput(t *testing.T) {
	inputs := map[string]string{
		"https://www.google.com/share.google?q=TOKEN1":                 "share.google-hop",
		"https://google.com/share.google/TOKEN1":                       "share.google-hop",
		"https://api.pinterest.com/url_shortener/TOKEN1/redirect/":     "pinterest-api",
		"http://www.google.com/share.google?q=TOKEN1&utm_source=foo#x": "share.google-hop",
	}
	reg := DefaultRegistry()
	for raw, entry := range inputs {
		u, skip := rawurl.Parse(raw)
		if skip != rawurl.OK {
			t.Fatalf("%s: skip %v", raw, skip)
		}
		for _, p := range []GenericPolicy{PreviewOnly, Follow, Skip} {
			if e, ok := reg.lookup(&u, false, p); ok {
				t.Errorf("%s matches %s as a first hop under policy %d", raw, e.Name, p)
			}
			if e, ok := reg.lookup(&u, true, p); !ok || e.Name != entry {
				t.Errorf("%s as a later hop: %v, %v; want %s", raw, e, ok, entry)
			}
		}
	}

	t.Run("resolve", func(t *testing.T) {
		n := installNet(t)
		redirect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "https://example.net/p")
			w.WriteHeader(301)
		})
		servers := []*testServer{
			n.HandleTLS("www.google.com", redirect), n.HandleTLS("google.com", redirect),
			n.HandleTLS("api.pinterest.com", redirect), n.HandleHTTP("www.google.com", redirect),
		}
		r := newTestResolver(t, Config{Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit()})
		for raw := range inputs {
			if e, ok := r.Match(raw); ok {
				t.Errorf("Match(%s) = %s", raw, e)
			}
			res, err := r.Resolve(context.Background(), raw)
			want := r.c.Clean(raw)
			if err != nil || res.URL != want || len(res.Hops) != 0 {
				t.Errorf("Resolve(%s) = %q, %d hops, %v; want %q offline", raw, res.URL, len(res.Hops), err, want)
			}
		}
		for _, s := range servers {
			if s.Hits() != 0 {
				t.Errorf("%s: %d hits", s.host, s.Hits())
			}
		}
	})
}

// bodyCapHandler serves a 200 of total bytes, with s at offset at, and
// reports how many bytes it wrote before the client stopped reading.
func bodyCapHandler(n *testNet, total, at int, s string, wrote chan<- int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Length", fmt.Sprint(total))
		w.WriteHeader(200)
		chunk := bytes.Repeat([]byte(" "), 32<<10)
		sent := 0
		defer func() { wrote <- sent }()
		for sent < total {
			b := chunk[:min(len(chunk), total-sent)]
			if sent <= at && at < sent+len(b) {
				b = bytes.Clone(b)
				copy(b[at-sent:], s)
			}
			m, err := w.Write(b)
			sent += m
			if err != nil {
				return
			}
			select {
			case <-n.Done():
				return
			default:
			}
		}
	})
}

// TestBodyCap checks the 64 KiB body cap over h2 and h1: a body strategy
// sees exactly the first 64 KiB of a large body (an anchor just past it is
// not found), and Redirect closes the body unread. Either way the client
// stops reading, so the server cannot send the whole body. The body is
// 64 MiB, more than loopback socket buffers and the h2 window can hold.
func TestBodyCap(t *testing.T) {
	const total = 64 << 20
	anchor := `<a href="https://example.net/found">`
	for _, proto := range []string{"h2", "h1"} {
		t.Run(proto, func(t *testing.T) {
			for _, c := range []struct {
				name     string
				at       int // the anchor's offset
				strategy Strategy
				entry    string
				want     string // the next URL; "" for err
				err      error
			}{
				{"within the cap", maxBody - len(anchor), nil, "body", "https://example.net/found", nil},
				{"past the cap", maxBody - len(anchor) + 1, nil, "body", "", ErrNoMatch},
				{"Redirect reads nothing", 0, Redirect(), "t.co", "", ErrNoRedirect},
			} {
				t.Run(c.name, func(t *testing.T) {
					n := installNet(t)
					wrote := make(chan int, 1)
					h := bodyCapHandler(n, total, c.at, anchor, wrote)
					host := "body.example.net"
					if c.entry == "t.co" {
						host = "t.co"
					}
					if proto == "h1" {
						n.HandleTLSH1(host, h)
					} else {
						n.HandleTLS(host, h)
					}
					st := netState(t, n, Config{}, netRegistry(t), nil)
					f, ctx := netFetcher(t, st, c.entry, 5*time.Second)
					s := c.strategy
					if s == nil {
						s = st.reg.entry(c.entry).Strategy
					}
					next, err := s.Next(ctx, f, mustURL(t, "https://"+host+"/TOKEN1"))
					if !errors.Is(err, c.err) || (c.err == nil) != (err == nil) {
						t.Fatalf("Next = %v, %v; want %s, %v", next, err, c.want, c.err)
					}
					if err == nil && next.String() != c.want {
						t.Errorf("next %s, want %s", next, c.want)
					}
					recs := f.finish()
					if len(recs) != 1 || recs[0].readBody != (c.strategy == nil) {
						t.Errorf("records %+v", recs)
					}
					select {
					case sent := <-wrote:
						if sent >= total {
							t.Errorf("the server sent the whole %d-byte body: the client read it all", sent)
						}
					case <-time.After(10 * time.Second):
						t.Error("the server is still sending 10 s after the Get returned: the client did not close the body")
					}
					slotsFree(t, st)
				})
			}
		})
	}

	// A direct Get: the body returned is the first 64 KiB.
	n := installNet(t)
	wrote := make(chan int, 1)
	n.HandleTLS("body.example.net", bodyCapHandler(n, total, 0, "x", wrote))
	st := netState(t, n, Config{}, netRegistry(t), nil)
	f, ctx := netFetcher(t, st, "body", 5*time.Second)
	resp, err := f.Get(ctx, mustURL(t, "https://body.example.net/TOKEN1"), true)
	if err != nil || len(resp.Body) != maxBody || resp.Body[0] != 'x' {
		t.Errorf("Get: %d bytes, %v; want the first %d", len(resp.Body), err, maxBody)
	}
}

// TestHeaderStallDefault is the one row at the default timeouts: a server
// that never sends response headers is abandoned at ResponseHeaderTimeout
// (3 s), a transport timeout well before the hop deadline (Timeout, 6 s).
func TestHeaderStallDefault(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", stallHandler(n))
	st := netState(t, n, Config{}, netRegistry(t), nil)
	if st.cfg.timeout != 6*time.Second {
		t.Fatalf("default Timeout %v", st.cfg.timeout)
	}
	f, ctx := netFetcher(t, st, "t.co", st.cfg.timeout)
	start := time.Now()
	_, err := Redirect().Next(ctx, f, mustURL(t, "https://t.co/x"))
	elapsed := time.Since(start)
	if kind, class := fetchClass(t, err); kind != FailTransient || class != classTimeout {
		t.Errorf("%v/%v (%v), want transient/timeout: the header timeout, not the hop deadline", kind, class, err)
	}
	if !errors.Is(err, ErrTransient) || errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v: want ErrTransient and not context.DeadlineExceeded", err)
	}
	if elapsed < defaultHeaderTimeout || elapsed >= st.cfg.timeout {
		t.Errorf("returned after %v, want between ResponseHeaderTimeout (%v) and the hop deadline (%v)", elapsed, defaultHeaderTimeout, st.cfg.timeout)
	}
	if s.Hits() != 1 {
		t.Errorf("%d hits", s.Hits())
	}
	slotsFree(t, st)
}

// TestCtxDeadline checks that a Get ends at the earlier of the strategy's
// ctx deadline and the hop deadline, before any transport timeout.
func TestCtxDeadline(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("t.co", stallHandler(n))
	st := netState(t, n, Config{}, netRegistry(t), nil)
	const short = 200 * time.Millisecond
	for _, c := range []struct {
		name       string
		hop, strat time.Duration // 0: no deadline on the strategy's ctx
		class      errClass
	}{
		{"strategy deadline", 5 * time.Second, short, classTimeout},
		{"hop deadline", short, 0, classDeadline},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := netFetcher(t, st, "t.co", c.hop)
			ctx := context.Background()
			if c.strat > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, c.strat)
				defer cancel()
			}
			start := time.Now()
			_, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false)
			elapsed := time.Since(start)
			if kind, class := fetchClass(t, err); kind != FailTransient || class != c.class {
				t.Errorf("%v/%v (%v), want transient/%v", kind, class, err, c.class)
			}
			if elapsed < short || elapsed > 2*time.Second {
				t.Errorf("returned after %v, want about %v (well before the 3 s header timeout)", elapsed, short)
			}
			slotsFree(t, st)
		})
	}

	// Through the Resolver: the caller's deadline ends Resolve, which
	// wraps the caller's ctx error.
	t.Run("resolve", func(t *testing.T) {
		r := newTestResolver(t, Config{Registry: netRegistry(t), Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit(), Timeout: time.Second})
		ctx, cancel := context.WithTimeout(context.Background(), short)
		defer cancel()
		start := time.Now()
		res, err := r.Resolve(ctx, "https://t.co/y")
		if !errors.Is(err, ErrTransient) || !errors.Is(err, context.DeadlineExceeded) || res.URL != "https://t.co/y" {
			t.Errorf("Resolve = %q, %v; want the input, ErrTransient and context.DeadlineExceeded", res.URL, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("Resolve returned after %v", d)
		}
	})
}

// TestSlowDrip checks that a body sent one byte per 100 ms is abandoned at
// the hop deadline (Timeout, 300 ms here) as a transient failure, with both
// slots released.
func TestSlowDrip(t *testing.T) {
	const timeout = 300 * time.Millisecond
	for _, proto := range []string{"h2", "h1"} {
		t.Run(proto, func(t *testing.T) {
			n := installNet(t)
			if proto == "h1" {
				n.HandleTLSH1("body.example.net", dripHandler(n))
			} else {
				n.HandleTLS("body.example.net", dripHandler(n))
			}
			st := netState(t, n, Config{Timeout: timeout}, netRegistry(t), nil)
			f, ctx := netFetcher(t, st, "body", st.cfg.timeout)
			start := time.Now()
			_, err := st.reg.entry("body").Strategy.Next(ctx, f, mustURL(t, "https://body.example.net/x"))
			elapsed := time.Since(start)
			if kind, _ := fetchClass(t, err); kind != FailTransient || !errors.Is(err, ErrTransient) {
				t.Errorf("%v (%v), want transient", kind, err)
			}
			if elapsed < timeout || elapsed > 2*time.Second {
				t.Errorf("returned after %v, want about %v", elapsed, timeout)
			}
			if recs := f.finish(); len(recs) != 1 || recs[0].kind != FailTransient || recs[0].err == nil {
				t.Errorf("records %+v", recs)
			}
			slotsFree(t, st)
		})
	}

	// Through the Resolver: transient at Timeout, not cached.
	t.Run("resolve", func(t *testing.T) {
		n := installNet(t)
		s := n.HandleTLS("body.example.net", dripHandler(n))
		r := newTestResolver(t, Config{Registry: netRegistry(t), Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit(), Timeout: timeout})
		for i := range 2 {
			res, err := r.Resolve(context.Background(), "https://body.example.net/x")
			if !errors.Is(err, ErrTransient) || res.URL != "https://body.example.net/x" {
				t.Errorf("call %d: %q, %v; want the input and ErrTransient", i, res.URL, err)
			}
			r.st.group.Wait()
		}
		if s.Hits() != 2 {
			t.Errorf("%d hits, want 2: a transient failure is not cached", s.Hits())
		}
		slotsFree(t, r.st)
	})
}

// headerPadHandler answers 301 with about size bytes of extra headers.
func headerPadHandler(size int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := strings.Repeat("a", 1000)
		for i := range size / 1024 {
			w.Header().Set(fmt.Sprintf("X-Pad-%03d", i), v)
		}
		w.Header().Set("Location", "https://example.net/p")
		w.WriteHeader(301)
	})
}

// TestHeaderCap checks the 32 KiB response header cap over h1 and h2: 40 KiB
// of headers is a transient failure, 20 KiB is not. It holds for
// NewTransport and, through New's clamp, for GuardTransport of a transport
// whose MaxResponseHeaderBytes is 0 (net/http's 10 MiB default).
func TestHeaderCap(t *testing.T) {
	for _, proto := range []string{"h1", "h2"} {
		t.Run(proto, func(t *testing.T) {
			n := installNet(t)
			serve := n.HandleTLS
			if proto == "h1" {
				serve = n.HandleTLSH1
			}
			big := serve("t.co", headerPadHandler(40<<10))
			small := serve("vm.tiktok.com", headerPadHandler(20<<10))
			guarded, err := GuardTransport(&http.Transport{TLSClientConfig: n.ClientTLS(), ForceAttemptHTTP2: true}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(guarded.CloseIdleConnections)
			if guarded.MaxResponseHeaderBytes != 0 {
				t.Fatalf("GuardTransport set MaxResponseHeaderBytes %d; the row needs 0", guarded.MaxResponseHeaderBytes)
			}
			for name, tr := range map[string]*http.Transport{
				"NewTransport":   guardedTransport(t, n, TransportOptions{}),
				"GuardTransport": guarded,
			} {
				t.Run(name, func(t *testing.T) {
					st := netState(t, n, Config{}, netRegistry(t), tr)
					f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
					start := time.Now()
					_, err := Redirect().Next(ctx, f, mustURL(t, "https://t.co/x"))
					if kind, class := fetchClass(t, err); kind != FailTransient || class == classDeadline || !errors.Is(err, ErrTransient) {
						t.Errorf("40 KiB of headers: %v/%v (%v), want transient before the deadline", kind, class, err)
					}
					if d := time.Since(start); d > 2*time.Second {
						t.Errorf("40 KiB of headers: returned after %v", d)
					}
					f, ctx = netFetcher(t, st, "tiktok", 5*time.Second)
					if next, err := Redirect().Next(ctx, f, mustURL(t, "https://vm.tiktok.com/x")); err != nil || next.String() != "https://example.net/p" {
						t.Errorf("20 KiB of headers: %v, %v", next, err)
					}
					slotsFree(t, st)
				})
			}
			if big.Hits() != 2 || small.Hits() != 2 {
				t.Errorf("hits %d/%d, want 2/2", big.Hits(), small.Hits())
			}

			// Through the Resolver: transient, not cached.
			t.Run("resolve", func(t *testing.T) {
				r := newTestResolver(t, Config{Registry: netRegistry(t), Transport: guarded, Limiter: NoLimit()})
				before := big.Hits()
				for i := range 2 {
					res, err := r.Resolve(context.Background(), "https://t.co/y")
					if !errors.Is(err, ErrTransient) || res.URL != "https://t.co/y" {
						t.Errorf("call %d: %q, %v; want the input and ErrTransient", i, res.URL, err)
					}
					r.st.group.Wait()
				}
				if got := big.Hits() - before; got != 2 {
					t.Errorf("%d requests, want 2: not cached", got)
				}
			})
		})
	}
}

// TestInformationalFlood checks that 1,000 103 Early Hints responses before
// the final one end the Get with an error before the hop deadline, over h1
// and h2: net/http bounds informational responses, so a server cannot hold
// the request open with them.
func TestInformationalFlood(t *testing.T) {
	const hop = 3 * time.Second
	for _, proto := range []string{"h1", "h2"} {
		t.Run(proto, func(t *testing.T) {
			n := installNet(t)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for range 1000 {
					w.Header().Set("Link", "</style.css>; rel=preload; as=style")
					w.WriteHeader(http.StatusEarlyHints)
					if r.Context().Err() != nil {
						return
					}
				}
				w.Header().Del("Link")
				w.Header().Set("Location", "https://example.net/p")
				w.WriteHeader(301)
			})
			if proto == "h1" {
				n.HandleTLSH1("t.co", h)
			} else {
				n.HandleTLS("t.co", h)
			}
			st := netState(t, n, Config{}, netRegistry(t), nil)
			f, ctx := netFetcher(t, st, "t.co", hop)
			start := time.Now()
			next, err := Redirect().Next(ctx, f, mustURL(t, "https://t.co/x"))
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("Next = %v after 1,000 informational responses, want an error", next)
			}
			if kind, class := fetchClass(t, err); kind != FailTransient || class == classDeadline {
				t.Errorf("%v/%v (%v), want transient before the hop deadline", kind, class, err)
			}
			if elapsed >= hop {
				t.Errorf("returned after %v, at the hop deadline", elapsed)
			}
			slotsFree(t, st)
		})
	}
}

// TestFetcherIgnoresStrategyCtx checks that a custom strategy passing
// context.Background() to Get cannot hold a request past the hop deadline:
// against a server that never completes the TLS handshake and one that
// drips its body, the Get ends at Timeout (300 ms), before the transport's
// own timeouts, and releases its slots.
func TestFetcherIgnoresStrategyCtx(t *testing.T) {
	const timeout = 300 * time.Millisecond
	background := netStrategy{"Background", func(_ context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		_, err := f.Get(context.Background(), u, true)
		if err != nil {
			return nil, err
		}
		return nil, ErrNoMatch
	}}
	reg, err := NewRegistry(
		Entry{Name: "stall", Match: []string{"*://stall.example.net/*"}, Strategy: background},
		Entry{Name: "drip", Match: []string{"*://drip.example.net/*"}, Strategy: background},
	)
	if err != nil {
		t.Fatal(err)
	}
	n := installNet(t)
	n.StallTLS("stall.example.net")
	drip := n.HandleTLS("drip.example.net", dripHandler(n))
	st := netState(t, n, Config{Timeout: timeout}, reg, nil)
	for _, name := range []string{"stall", "drip"} {
		t.Run(name, func(t *testing.T) {
			f, ctx := netFetcher(t, st, name, st.cfg.timeout)
			start := time.Now()
			_, err := background.Next(ctx, f, mustURL(t, "https://"+name+".example.net/x"))
			elapsed := time.Since(start)
			if kind, class := fetchClass(t, err); kind != FailTransient || class != classDeadline {
				t.Errorf("%v/%v (%v), want transient at the hop deadline", kind, class, err)
			}
			if elapsed < timeout || elapsed > 1500*time.Millisecond {
				t.Errorf("returned after %v, want about %v (the TLS handshake timeout is 2 s)", elapsed, timeout)
			}
			slotsFree(t, st)
		})
	}
	if drip.Hits() != 1 {
		t.Errorf("drip hits %d", drip.Hits())
	}

	// Through the Resolver: ErrTransient at Timeout, and nothing is left
	// in flight or holding a slot.
	t.Run("resolve", func(t *testing.T) {
		r := newTestResolver(t, Config{Registry: reg, Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit(), Timeout: timeout})
		for _, name := range []string{"stall", "drip"} {
			host := name + ".example.net"
			res, err := r.Resolve(context.Background(), "https://"+host+"/y")
			if !errors.Is(err, ErrTransient) || res.URL != "https://"+host+"/y" {
				t.Errorf("%s: %q, %v; want the input and ErrTransient", name, res.URL, err)
			}
			r.st.group.Wait()
			if total, forHost := r.st.group.InFlight(host); total != 0 || forHost != 0 {
				t.Errorf("%s: in flight %d/%d after the hop", name, total, forHost)
			}
			slotsFree(t, r.st)
		}
	})
}

// TestFetcherAfterReturn checks that a Get from a goroutine that outlives
// its strategy's Next is ErrBudget and sends nothing.
func TestFetcherAfterReturn(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	late := make(chan error, 1)
	release := make(chan struct{})
	leaky := netStrategy{"Leaky", func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		go func() {
			<-release
			_, err := f.Get(context.Background(), u, false)
			late <- err
		}()
		return nil, ErrNoMatch
	}}
	reg, err := NewRegistry(Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: leaky})
	if err != nil {
		t.Fatal(err)
	}
	st := netState(t, n, Config{}, reg, nil)
	f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
	if _, err := leaky.Next(ctx, f, mustURL(t, "https://t.co/x")); !errors.Is(err, ErrNoMatch) {
		t.Fatal(err)
	}
	f.finish() // what the hop does once its last strategy returns
	close(release)
	if err := <-late; !errors.Is(err, ErrBudget) {
		t.Errorf("Get after Next returned = %v, want ErrBudget", err)
	}
	if s.Hits() != 0 {
		t.Errorf("%d hits, want 0", s.Hits())
	}

	// Through the Resolver: the hop marks its Fetcher done itself.
	t.Run("resolve", func(t *testing.T) {
		release = make(chan struct{})
		r := newTestResolver(t, Config{Registry: reg, Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit()})
		if _, err := r.Resolve(context.Background(), "https://t.co/y"); !errors.Is(err, ErrNoMatch) {
			t.Errorf("Resolve = %v, want ErrNoMatch", err)
		}
		r.st.group.Wait()
		close(release)
		if err := <-late; !errors.Is(err, ErrBudget) {
			t.Errorf("Get after the hop = %v, want ErrBudget", err)
		}
		if s.Hits() != 0 {
			t.Errorf("%d hits, want 0", s.Hits())
		}
	})
}

// retryHandler answers 301 to every request but the second, whose
// connection it closes unanswered. The second request arrives on the
// connection the first one left idle, so net/http retries it on a new one.
func retryHandler(hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 2 {
			c, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				c.Close()
			}
			return
		}
		w.Header().Set("Location", "https://example.net"+r.URL.Path)
		w.WriteHeader(301)
	})
}

// TestTransportRetryCounted checks that a request net/http silently resends
// (a bodyless GET on a reused h1 connection the server closed unanswered)
// is counted as two wire writes in one record. The h2 variant (GOAWAY
// mid-request) is not tested: httptest's h2 server cannot send one on
// demand.
func TestTransportRetryCounted(t *testing.T) {
	n := installNet(t)
	var hits atomic.Int32
	s := n.HandleTLSH1("t.co", retryHandler(&hits))
	st := netState(t, n, Config{}, netRegistry(t), nil)

	f, ctx := netFetcher(t, st, "t.co", 5*time.Second)
	if _, err := Redirect().Next(ctx, f, mustURL(t, "https://t.co/warm")); err != nil {
		t.Fatal(err)
	}
	f, ctx = netFetcher(t, st, "t.co", 5*time.Second)
	next, err := Redirect().Next(ctx, f, mustURL(t, "https://t.co/retried"))
	if err != nil || next.String() != "https://example.net/retried" {
		t.Fatalf("retried Get: %v, %v", next, err)
	}
	recs := f.finish()
	if len(recs) != 1 || recs[0].wire != 2 || recs[0].status != 301 {
		t.Errorf("records %+v, want one with 2 wire writes", recs)
	}
	var paths []string
	for _, r := range s.Requests() {
		paths = append(paths, r.RequestURI)
	}
	if strings.Join(paths, " ") != "/warm /retried /retried" {
		t.Errorf("server saw %q", paths)
	}

	// Through the Resolver: Hop.Requests is 2 and OnRequest fires once.
	t.Run("resolve", func(t *testing.T) {
		hits.Store(0)
		var infos atomic.Int32
		r := newTestResolver(t, Config{
			Registry: netRegistry(t), Transport: guardedTransport(t, n, TransportOptions{}), Limiter: NoLimit(),
			OnRequest: func(RequestInfo) { infos.Add(1) },
		})
		if _, err := r.Resolve(context.Background(), "https://t.co/warm2"); err != nil {
			t.Fatal(err)
		}
		r.st.group.Wait()
		infos.Store(0)
		res, err := r.Resolve(context.Background(), "https://t.co/retried2")
		if err != nil || len(res.Hops) != 1 || res.Hops[0].Requests != 2 {
			t.Fatalf("Resolve = %+v, %v; want one hop with Requests 2", res.Hops, err)
		}
		r.st.group.Wait()
		if infos.Load() != 1 {
			t.Errorf("OnRequest fired %d times, want once", infos.Load())
		}
	})
}
