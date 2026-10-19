package unshorten

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"code.dny.dev/ssrf"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// handlerOK answers 200 "ok".
var handlerOK = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

// guardedTransport is NewTransport(opts) trusting n's CA, closed at the end of
// the test (before n's cleanup, which was registered first).
func guardedTransport(t *testing.T, n *testNet, opts TransportOptions) *http.Transport {
	t.Helper()
	opts.TLSConfig = n.ClientTLS()
	tr := NewTransport(opts)
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

// fetch sends a GET for rawURL through rt with the production headers,
// reads and closes the body, and returns the response (body drained).
func fetchVia(t *testing.T, rt http.RoundTripper, rawURL string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return fetchViaCtx(ctx, rt, rawURL)
}

func fetchViaCtx(ctx context.Context, rt http.RoundTripper, rawURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUA())
	req.Header.Set("Accept", "*/*")
	c := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, nil
}

// capturedLog is a slog handler's output, counted by line.
type capturedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *capturedLog) lines() int { return strings.Count(c.String(), "\n") }

// opaqueTransport is a RoundTripper New cannot inspect. It never sends anything.
type opaqueTransport struct{ calls atomic.Int32 }

func (o *opaqueTransport) RoundTrip(*http.Request) (*http.Response, error) {
	o.calls.Add(1)
	return nil, errors.New("opaqueTransport: no network")
}

// dialSpy is a DialContext that records its calls and never dials.
type dialSpy struct {
	mu    sync.Mutex
	addrs []string
}

var errDialSpy = errors.New("dialSpy: no network")

func (s *dialSpy) DialContext(_ context.Context, _, addr string) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addrs = append(s.addrs, addr)
	return nil, errDialSpy
}

func (s *dialSpy) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.addrs...)
}

func urlOf(t testing.TB, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestNewTransportFields(t *testing.T) {
	tc := &tls.Config{ServerName: "kept"}
	tr := NewTransport(TransportOptions{
		DialTimeout:           time.Second,
		TLSHandshakeTimeout:   3 * time.Second,
		ResponseHeaderTimeout: 4 * time.Second,
		TLSConfig:             tc,
	})
	a, ok := attested(tr)
	switch {
	case !ok:
		t.Fatal("NewTransport's transport is not attested")
	case funcPC(tr.DialContext) != guardDialPC:
		t.Error("DialContext is not the guard")
	case tr.Proxy != nil || a.proxy != nil:
		t.Error("Proxy set without opts.Proxy")
	case tr.Dial != nil || tr.DialTLS != nil || tr.DialTLSContext != nil:
		t.Error("a Dial* field is set")
	case a.dial.d.Timeout != time.Second:
		t.Errorf("dial timeout %v", a.dial.d.Timeout)
	case a.dial.d.Control == nil || funcPC(a.dial.d.Control) != funcPC(theGuard.control):
		t.Error("the dialer's Control is not the guard")
	case a.dial.proxyAddr != "":
		t.Errorf("proxyAddr %q without a proxy", a.dial.proxyAddr)
	case tr.TLSHandshakeTimeout != 3*time.Second, tr.ResponseHeaderTimeout != 4*time.Second:
		t.Errorf("timeouts %v %v", tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
	case tr.TLSClientConfig == tc || tr.TLSClientConfig == nil || tr.TLSClientConfig.ServerName != "kept":
		t.Error("TLSClientConfig is not a clone of opts.TLSConfig")
	case tr.MaxResponseHeaderBytes != 32<<10, !tr.ForceAttemptHTTP2, tr.MaxIdleConnsPerHost != 2,
		tr.MaxConnsPerHost != 4, tr.IdleConnTimeout != 30*time.Second, !tr.DisableCompression:
		t.Errorf("fixed settings: %+v", tr)
	}

	for _, d := range []time.Duration{0, -time.Second} {
		tr := NewTransport(TransportOptions{DialTimeout: d, TLSHandshakeTimeout: d, ResponseHeaderTimeout: d})
		a, _ := attested(tr)
		if a.dial.d.Timeout != 2*time.Second || tr.TLSHandshakeTimeout != 2*time.Second || tr.ResponseHeaderTimeout != 3*time.Second {
			t.Errorf("timeouts %v: dial %v, TLS %v, header %v", d, a.dial.d.Timeout, tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout)
		}
		if tr.TLSClientConfig != nil {
			t.Error("TLSClientConfig set without opts.TLSConfig")
		}
	}

	pu := urlOf(t, "http://u:pw@Proxy.Example:3128")
	tr = NewTransport(TransportOptions{Proxy: pu})
	a, _ = attested(tr)
	if a.proxy == nil || tr.Proxy == nil || funcPC(tr.Proxy) != fixedProxyPC {
		t.Fatal("proxy not installed")
	}
	if a.dial.proxyAddr != "Proxy.Example:3128" {
		t.Errorf("proxyAddr %q", a.dial.proxyAddr)
	}
	got, err := tr.Proxy(nil)
	if err != nil || got.String() != pu.String() || got == pu {
		t.Errorf("Proxy() = %v, %v: want a copy of opts.Proxy", got, err)
	}
	pu.Host = "elsewhere.example:1"
	if got, _ := tr.Proxy(nil); got.Host != "Proxy.Example:3128" {
		t.Errorf("changing the caller's URL moved the proxy to %q", got.Host)
	}

	tr = NewTransport(TransportOptions{Proxy: urlOf(t, "ftp://h")})
	if _, err := tr.Proxy(nil); err == nil {
		t.Error("an invalid proxy URL gives no error per request")
	}
	if a, _ := attested(tr); a.dial.proxyAddr != "" {
		t.Errorf("proxyAddr %q for an invalid proxy", a.dial.proxyAddr)
	}
}

// TestNewTransportRequest sends requests through NewTransport over HTTP/2,
// HTTP/1.1 and plain http, with the real guard in the dial path, and checks
// what the servers saw.
func TestNewTransportRequest(t *testing.T) {
	n := installNet(t)
	h2 := n.HandleTLS("t.co", handlerOK)
	h1 := n.HandleTLSH1("amzn.to", handlerOK)
	plain := n.HandleHTTP("t.co", handlerOK)
	tr := guardedTransport(t, n, TransportOptions{})
	for _, c := range []struct {
		url, proto string
		s          *testServer
	}{
		{"https://t.co/abc?x=1#frag", "HTTP/2.0", h2},
		{"https://amzn.to/abc", "HTTP/1.1", h1},
		{"http://t.co/p", "HTTP/1.1", plain},
	} {
		resp, err := fetchVia(t, tr, c.url)
		if err != nil {
			t.Fatalf("%s: %v", c.url, err)
		}
		if resp.StatusCode != 200 || resp.Proto != c.proto {
			t.Errorf("%s: %d %s, want 200 %s", c.url, resp.StatusCode, resp.Proto, c.proto)
		}
		reqs := c.s.Requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests", c.url, len(reqs))
		}
		if strings.Contains(reqs[0].RequestURI, "#") || reqs[0].Header.Get("Accept-Encoding") != "" {
			t.Errorf("%s: server saw %+v", c.url, reqs[0])
		}
	}
	if got := h2.Requests()[0]; got.Path != "/abc" || got.RawQuery != "x=1" || got.Host != "t.co" {
		t.Errorf("recorded %+v", got)
	}
}

// TestNettestHarness checks the harness's own bookkeeping: header-set
// violations, unmapped dials, Resolve call counts.
func TestNettestHarness(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	tr := guardedTransport(t, n, TransportOptions{})

	req, _ := http.NewRequest(http.MethodGet, "https://t.co/x", nil)
	req.Header.Set("User-Agent", "other")
	req.Header.Set("Cookie", "a=b")
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	v := strings.Join(n.TakeViolations(), "\n")
	for _, want := range []string{"unexpected header Cookie", "User-Agent", "Accept"} {
		if !strings.Contains(v, want) {
			t.Errorf("violations %q lack %q", v, want)
		}
	}
	if s.Hits() != 1 || n.Dials("T.CO", "443") != 1 {
		t.Errorf("hits %d, dials %d", s.Hits(), n.Dials("t.co", "443"))
	}

	_, err = fetchVia(t, tr, "https://nowhere.test/x")
	if !errors.Is(err, dialhook.ErrUnmapped) {
		t.Errorf("unmapped host: %v", err)
	}
	if got := n.Unmapped(); len(got) != 1 || got[0] != "nowhere.test:443" {
		t.Errorf("Unmapped() = %q", got)
	}
}

func TestTransportFieldsReviewed(t *testing.T) {
	// Every http.Transport field that can hold behaviour (a func,
	// interface, map, chan or pointer) as of go1.26, and why New's private
	// transport is still guarded with it set. A Go release that adds one
	// fails this test until it is reviewed here.
	reviewed := map[string]string{
		"Proxy":                  "attested by code pointer (fixedProxy); privateClone installs the recorded fixedProxy",
		"OnProxyConnectResponse": "sees the proxy's CONNECT reply; cannot change what is dialed",
		"DialContext":            "attested by code pointer (guardDialer); privateClone installs the recorded guardDialer",
		"Dial":                   "must be nil (verifyAttested, GuardTransport); privateClone resets it",
		"DialTLSContext":         "must be nil (verifyAttested, GuardTransport); privateClone resets it",
		"DialTLS":                "must be nil (verifyAttested, GuardTransport); privateClone resets it",
		"TLSClientConfig":        "the caller's TLS policy; dials still go through DialContext",
		"TLSNextProto":           "privateClone resets it to nil: net/http's own HTTP/2, which dials through DialContext",
		"ProxyConnectHeader":     "headers for the proxy only",
		"GetProxyConnectHeader":  "headers for the proxy only",
		"HTTP2":                  "HTTP/2 tuning (CountError is a metrics hook); dials through DialContext",
		"Protocols":              "HTTP/1, HTTP/2 and unencrypted HTTP/2 dial through DialContext; HTTP/3 needs a RegisterProtocol entry, which Clone does not copy",
	}
	seen := map[string]bool{}
	rt := reflect.TypeFor[http.Transport]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Func, reflect.Interface, reflect.Map, reflect.Chan, reflect.Pointer, reflect.UnsafePointer:
		default:
			continue
		}
		seen[f.Name] = true
		if reviewed[f.Name] == "" {
			t.Errorf("http.Transport.%s (%s) is not reviewed: decide how New's private transport handles it, then add it here", f.Name, f.Type)
		}
	}
	for name := range reviewed {
		if !seen[name] {
			t.Errorf("reviewed field %s is not a behaviour field of http.Transport", name)
		}
	}
}

// TestTransportCheckHeaderLimit: with Allow, the warning for an
// *http.Transport names its MaxResponseHeaderBytes when response headers
// are not capped at 32 KiB, since New then uses the transport as is.
func TestTransportCheckHeaderLimit(t *testing.T) {
	for _, c := range []struct {
		name string
		rt   *http.Transport
		want string // "" means the attribute is absent
	}{
		{"zero", &http.Transport{}, "max_response_header_bytes=0"},
		{"large", &http.Transport{MaxResponseHeaderBytes: 1 << 20}, "max_response_header_bytes=1048576"},
		{"capped clone", NewTransport(TransportOptions{}).Clone(), ""},
		{"small", &http.Transport{MaxResponseHeaderBytes: 4096}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logs capturedLog
			out, err := checkTransport(c.rt, true, logs.logger())
			if err != nil || out != c.rt {
				t.Fatalf("got %p, %v: want the caller's transport", out, err)
			}
			if logs.lines() != 1 {
				t.Fatalf("%d log lines, want 1: %s", logs.lines(), logs.String())
			}
			has := strings.Contains(logs.String(), "max_response_header_bytes")
			if c.want == "" && has || c.want != "" && !strings.Contains(logs.String(), c.want) {
				t.Errorf("warning %q, want header limit %q", logs.String(), c.want)
			}
		})
	}
}

// TestTransportCheck is New's transport decision table, on
// checkTransport.
func TestTransportCheck(t *testing.T) {
	type tc struct {
		name    string
		rt      func() http.RoundTripper
		ok      bool   // accepted without Allow
		mention string // in the error and the warning
		warn    bool   // with Allow: one warning
	}
	guarded, err := GuardTransport(NewTransport(TransportOptions{}).Clone(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(f func(*http.Transport)) func() http.RoundTripper {
		return func() http.RoundTripper {
			tr := NewTransport(TransportOptions{})
			f(tr)
			return tr
		}
	}
	cases := []tc{
		{name: "DefaultTransport", rt: func() http.RoundTripper { return origDefaultTransport }, mention: "ProxyFromEnvironment", warn: true},
		{name: "NewTransport", rt: func() http.RoundTripper { return NewTransport(TransportOptions{}) }, ok: true},
		{name: "GuardTransport", rt: func() http.RoundTripper { return guarded }, ok: true},
		{name: "Clone", rt: func() http.RoundTripper { return NewTransport(TransportOptions{}).Clone() }, mention: "not built by", warn: true},
		{name: "plain", rt: func() http.RoundTripper { return &http.Transport{} }, mention: "not built by", warn: true},
		{name: "opaque", rt: func() http.RoundTripper { return &opaqueTransport{} }, mention: "not an *http.Transport", warn: true},
		{name: "ProxyFromEnvironment", rt: mutate(func(tr *http.Transport) { tr.Proxy = http.ProxyFromEnvironment }), mention: "ProxyFromEnvironment", warn: true},
		{name: "foreign DialContext", rt: mutate(func(tr *http.Transport) { tr.DialContext = (&net.Dialer{}).DialContext }), mention: "DialContext", warn: true},
		{name: "nil DialContext", rt: mutate(func(tr *http.Transport) { tr.DialContext = nil }), mention: "DialContext", warn: true},
		{name: "DialTLSContext", rt: mutate(func(tr *http.Transport) { tr.DialTLSContext = (&dialSpy{}).DialContext }), mention: "DialTLSContext", warn: true},
		{name: "Dial", rt: mutate(func(tr *http.Transport) { tr.Dial = net.Dial }), mention: "Dial is set", warn: true},
		{name: "DialTLS", rt: mutate(func(tr *http.Transport) { tr.DialTLS = net.Dial }), mention: "DialTLS is set", warn: true},
		{name: "ProxyURL", rt: mutate(func(tr *http.Transport) { tr.Proxy = http.ProxyURL(urlOf(t, "http://p.test")) }), mention: "Proxy was set", warn: true},
		{name: "invalid proxy", rt: func() http.RoundTripper { return NewTransport(TransportOptions{Proxy: urlOf(t, "ftp://p.test")}) }, mention: "invalid proxy URL", warn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.rt()
			var logs capturedLog
			out, err := checkTransport(in, false, logs.logger())
			if c.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				ot, isT := out.(*http.Transport)
				if !isT || ot == in {
					t.Fatalf("got %T %p for %p: want a private clone", out, out, in)
				}
				a, ok := attested(ot)
				if !ok || verifyAttested(ot, a) != nil {
					t.Error("the private clone does not verify")
				}
			} else {
				if !errors.Is(err, ErrUnguardedTransport) || out != nil {
					t.Fatalf("got %v, %v: want ErrUnguardedTransport", out, err)
				}
				if !strings.Contains(err.Error(), c.mention) {
					t.Errorf("error %q does not mention %q", err, c.mention)
				}
			}
			if logs.lines() != 0 {
				t.Errorf("logged without Allow: %s", logs.String())
			}

			out, err = checkTransport(in, true, logs.logger())
			if err != nil {
				t.Fatalf("with Allow: %v", err)
			}
			if !c.ok && out != in {
				t.Errorf("with Allow: got %p, want the caller's transport %p as is", out, in)
			}
			wantLines := 0
			if c.warn {
				wantLines = 1
			}
			if got := logs.lines(); got != wantLines {
				t.Errorf("with Allow: %d log lines, want %d: %s", got, wantLines, logs.String())
			}
			if c.warn && (!strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), c.mention)) {
				t.Errorf("warning %q does not mention %q", logs.String(), c.mention)
			}
		})
	}

	t.Run("nil", func(t *testing.T) {
		out, err := checkTransport(nil, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		ot, ok := out.(*http.Transport)
		if !ok {
			t.Fatalf("%T", out)
		}
		if a, ok := attested(ot); !ok || verifyAttested(ot, a) != nil || ot.Proxy != nil {
			t.Error("nil does not give a guarded NewTransport")
		}
	})
}

// TestTransportCheckTypedNil: a nil *http.Transport in Config.Transport is
// ErrUnguardedTransport, with AllowUnguardedTransport too, never a panic.
func TestTransportCheckTypedNil(t *testing.T) {
	for _, allow := range []bool{false, true} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("allow %v: New panicked: %v", allow, r)
				}
			}()
			r, err := New(Config{Transport: (*http.Transport)(nil), AllowUnguardedTransport: allow})
			if r != nil || !errors.Is(err, ErrUnguardedTransport) {
				t.Errorf("allow %v: New = %v, %v; want ErrUnguardedTransport", allow, r, err)
			}
		}()
	}
}

func TestPrivateClone(t *testing.T) {
	for _, c := range []struct{ in, want int64 }{{0, 32 << 10}, {-1, 32 << 10}, {1 << 10, 1 << 10}, {32 << 10, 32 << 10}, {1 << 20, 32 << 10}} {
		tr, err := GuardTransport(&http.Transport{MaxResponseHeaderBytes: c.in, TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		out, err := checkTransport(tr, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		ot := out.(*http.Transport)
		if ot.MaxResponseHeaderBytes != c.want {
			t.Errorf("MaxResponseHeaderBytes %d → %d, want %d", c.in, ot.MaxResponseHeaderBytes, c.want)
		}
		if ot.TLSNextProto != nil || !ot.DisableCompression || ot.Proxy != nil {
			t.Errorf("clone: TLSNextProto %v, DisableCompression %v", ot.TLSNextProto, ot.DisableCompression)
		}
	}
}

// TestAttestAfterCheck: once checkTransport has made New's private clone,
// changing the caller's transport has no effect on it.
func TestAttestAfterCheck(t *testing.T) {
	n := installNet(t)
	n.MapRaw("t.co", "443", "127.0.0.1:443") // no grant: the guard must deny it
	tr := guardedTransport(t, n, TransportOptions{})
	out, err := checkTransport(tr, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var spy dialSpy
	tr.DialContext = spy.DialContext
	tr.Proxy = http.ProxyFromEnvironment
	_, err = fetchVia(t, out, "https://t.co/x")
	if !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Errorf("after mutating the caller's transport: %v, want the guard's denial", err)
	}
	if len(spy.calls()) != 0 {
		t.Errorf("the caller's new DialContext was used: %q", spy.calls())
	}
}

// TestAttestProxySwap: a fixedProxy func taken from another NewTransport
// passes the code-pointer check, but the private clone uses the recorded
// proxy, so the swapped-in one sees nothing.
func TestAttestProxySwap(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	p1, p2 := n.ConnectProxy(), n.ConnectProxy()
	u1, u2 := n.Proxy(p1), n.Proxy(p2)
	tA := guardedTransport(t, n, TransportOptions{Proxy: u1})
	tB := guardedTransport(t, n, TransportOptions{Proxy: u2})
	tA.Proxy = tB.Proxy
	out, err := checkTransport(tA, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out.(*http.Transport).CloseIdleConnections)
	if _, err := fetchVia(t, out, "https://t.co/x"); err != nil {
		t.Fatal(err)
	}
	if got := p1.Requests(); len(got) != 1 || got[0] != "CONNECT t.co:443" {
		t.Errorf("recorded proxy saw %q", got)
	}
	if got := p2.Requests(); len(got) != 0 {
		t.Errorf("swapped-in proxy saw %q", got)
	}
	if s.Hits() != 1 {
		t.Errorf("server hits %d", s.Hits())
	}

	// Removing the proxy, or adding one to a transport built without, is
	// refused.
	tA2 := NewTransport(TransportOptions{Proxy: u1})
	tA2.Proxy = nil
	tN := NewTransport(TransportOptions{})
	tN.Proxy = tB.Proxy
	for name, tr := range map[string]*http.Transport{"removed": tA2, "added": tN} {
		if _, err := checkTransport(tr, false, nil); !errors.Is(err, ErrUnguardedTransport) {
			t.Errorf("proxy %s: %v", name, err)
		}
	}
}

// TestAttestTLSNextProto: a caller's TLSNextProto entry is dropped, so its
// RoundTripper never serves New's requests.
func TestAttestTLSNextProto(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	tr := guardedTransport(t, n, TransportOptions{})
	var spy opaqueTransport
	tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{
		"h2": func(string, *tls.Conn) http.RoundTripper { return &spy },
	}
	out, err := checkTransport(tr, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out.(*http.Transport).CloseIdleConnections)
	resp, err := fetchVia(t, out, "https://t.co/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Proto != "HTTP/2.0" || spy.calls.Load() != 0 || s.Hits() != 1 {
		t.Errorf("proto %s, caller's h2 RoundTripper calls %d, hits %d", resp.Proto, spy.calls.Load(), s.Hits())
	}
}

// TestAttestRegisterProtocol: Clone does not copy RegisterProtocol entries.
func TestAttestRegisterProtocol(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	tr := guardedTransport(t, n, TransportOptions{})
	var spy opaqueTransport
	tr.RegisterProtocol("https", &spy)
	out, err := checkTransport(tr, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(out.(*http.Transport).CloseIdleConnections)
	if _, err := fetchVia(t, out, "https://t.co/x"); err != nil {
		t.Fatal(err)
	}
	if spy.calls.Load() != 0 || s.Hits() != 1 {
		t.Errorf("registered RoundTripper calls %d, hits %d", spy.calls.Load(), s.Hits())
	}
}

// TestAttestRegistryShrinks: the registry does not keep transports alive.
// AddCleanup runs asynchronously, so this asserts a ratio.
func TestAttestRegistryShrinks(t *testing.T) {
	const count = 10_000
	before := attestationCount()
	func() {
		keep := make([]*http.Transport, count)
		for i := range keep {
			keep[i] = NewTransport(TransportOptions{})
		}
		if got := attestationCount(); got < count {
			t.Fatalf("registry holds %d after %d NewTransports", got, count)
		}
		runtime.KeepAlive(keep)
	}()
	for i := 0; i < 50 && attestationCount() > before+count/10; i++ {
		runtime.GC()
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if got := attestationCount(); got > before+count/10 {
		t.Errorf("registry holds %d of %d collectable transports after GC", got-before, count)
	}
}

// TestAttestSharedTLSConfig: two transports built from one tls.Config are
// used concurrently (run under -race).
func TestAttestSharedTLSConfig(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	tc := n.ClientTLS()
	t1, t2 := NewTransport(TransportOptions{TLSConfig: tc}), NewTransport(TransportOptions{TLSConfig: tc})
	t.Cleanup(t1.CloseIdleConnections)
	t.Cleanup(t2.CloseIdleConnections)
	var wg sync.WaitGroup
	for i := range 8 {
		tr := t1
		if i%2 == 1 {
			tr = t2
		}
		wg.Go(func() {
			if _, err := fetchVia(t, tr, "https://t.co/x"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if s.Hits() != 8 {
		t.Errorf("hits %d", s.Hits())
	}
}

func TestGuardTransport(t *testing.T) {
	for name, in := range map[string]*http.Transport{
		"zero":               {}, // a nil DialContext is not "set to anything other than a guard"
		"NewTransport clone": NewTransport(TransportOptions{}).Clone(),
		"GuardTransport":     mustTransport(GuardTransport(&http.Transport{}, nil)),
	} {
		out, err := GuardTransport(in, nil)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		a, ok := attested(out)
		switch {
		case !ok || verifyAttested(out, a) != nil:
			t.Errorf("%s: result does not verify", name)
		case out == in, out.Proxy != nil, !out.DisableCompression:
			t.Errorf("%s: result %+v", name, out)
		case a.dial.d.Timeout != 2*time.Second || a.dial.proxyAddr != "":
			t.Errorf("%s: dialer %+v", name, a.dial)
		}
	}

	refused := map[string]*http.Transport{
		"nil":                       nil,
		"DefaultTransport clone":    origDefaultTransport.(*http.Transport).Clone(),
		"foreign DialContext":       {DialContext: (&net.Dialer{}).DialContext},
		"DialTLSContext":            {DialTLSContext: (&dialSpy{}).DialContext},
		"Dial":                      {Dial: net.Dial},
		"DialTLS":                   {DialTLS: net.Dial},
		"guard plus DialTLSContext": {DialContext: newGuardDialer(time.Second).DialContext, DialTLSContext: (&dialSpy{}).DialContext},
		"closure around a guardDial": {DialContext: func(ctx context.Context, n, a string) (net.Conn, error) {
			return newGuardDialer(time.Second).DialContext(ctx, n, a)
		}},
	}
	for name, in := range refused {
		if out, err := GuardTransport(in, nil); !errors.Is(err, ErrUnguardedTransport) || out != nil {
			t.Errorf("%s: %v, %v: want ErrUnguardedTransport", name, out, err)
		}
	}

	out, err := GuardTransport(origDefaultTransport.(*http.Transport).Clone(), nil)
	if err == nil || out != nil {
		t.Error("GuardTransport accepted DefaultTransport's dialer")
	}

	out, err = GuardTransport(&http.Transport{Proxy: http.ProxyFromEnvironment}, urlOf(t, "socks5://proxy.test"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := attested(out)
	if funcPC(out.Proxy) != fixedProxyPC || a.proxy == nil || a.dial.proxyAddr != "proxy.test:1080" {
		t.Errorf("proxy not installed: %+v", a.dial)
	}
	if _, err := GuardTransport(&http.Transport{}, urlOf(t, "http://proxy.test/path")); err == nil {
		t.Error("a proxy URL with a path is accepted")
	}
}

func mustTransport(v *http.Transport, err error) *http.Transport {
	if err != nil {
		panic(err)
	}
	return v
}

func TestCanonicalProxyAddr(t *testing.T) {
	good := map[string]string{
		"http://PROXY.test":           "PROXY.test:80",
		"http://proxy.test:8080":      "proxy.test:8080",
		"http://proxy.test:8080/":     "proxy.test:8080",
		"https://proxy.test":          "proxy.test:443",
		"socks5://h.test":             "h.test:1080",
		"socks5h://h.test:9050":       "h.test:9050",
		"http://u:pw@proxy.test:3128": "proxy.test:3128",
		"http://[2001:db8::1]:3128":   "[2001:db8::1]:3128",
		"http://[2001:db8::1]":        "[2001:db8::1]:80",
		"http://192.0.2.1":            "192.0.2.1:80",
		"http://proxy.test:":          "proxy.test:80",
	}
	for in, want := range good {
		u := urlOf(t, in)
		got, err := canonicalProxyAddr(u)
		if err != nil || got != want {
			t.Errorf("canonicalProxyAddr(%s) = %q, %v; want %q", in, got, err, want)
			continue
		}
		// It is what net/http dials, for https and http targets alike.
		for _, target := range []string{"https://target.test/", "http://target.test/"} {
			var spy dialSpy
			tr := &http.Transport{Proxy: http.ProxyURL(u), DialContext: spy.DialContext}
			if _, err := fetchVia(t, tr, target); !errors.Is(err, errDialSpy) {
				t.Errorf("%s via %s: %v", target, in, err)
			}
			if got := spy.calls(); len(got) != 1 || got[0] != want {
				t.Errorf("%s via %s: net/http dialed %q, want %q", target, in, got, want)
			}
		}
	}
	for _, in := range []string{"ftp://h.test", "http://", "http:opaque", "http://h.test/path", "http://h.test?q",
		"http://h.test#f", "http://h.test:0", "http://h.test:99999", "http://ünï.test", "HTTP-x://h.test"} {
		u, err := url.Parse(in)
		if err != nil {
			continue
		}
		if got, err := canonicalProxyAddr(u); err == nil {
			t.Errorf("canonicalProxyAddr(%s) = %q, want an error", in, got)
		}
	}
	if _, err := canonicalProxyAddr(nil); err == nil {
		t.Error("nil URL accepted")
	}
}

// TestProxyConnect: a loopback CONNECT proxy reached through the exempt
// proxy address; https targets are tunnelled, plain-http ones are sent to
// the proxy as absolute URIs.
func TestProxyConnect(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	p := n.ConnectProxy()
	tr := guardedTransport(t, n, TransportOptions{Proxy: n.Proxy(p)})
	resp, err := fetchVia(t, tr, "https://t.co/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || s.Hits() != 1 {
		t.Errorf("status %d, hits %d", resp.StatusCode, s.Hits())
	}
	resp, err = fetchVia(t, tr, "http://t.co/y")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("plain http via the proxy: %d", resp.StatusCode)
	}
	if got := p.Requests(); len(got) != 2 || got[0] != "CONNECT t.co:443" || got[1] != "GET t.co" {
		t.Errorf("proxy saw %q", got)
	}

	// A proxy that refuses CONNECT is a transient failure.
	deny := n.Proxy(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusForbidden) }))
	tr = guardedTransport(t, n, TransportOptions{Proxy: deny})
	_, err = fetchVia(t, tr, "https://t.co/x")
	if err == nil {
		t.Fatal("CONNECT refused, but the request succeeded")
	}
	if k, c := classifyTransport(err, context.Background()); k != FailTransient || c != classOther {
		t.Errorf("CONNECT refusal classified %v/%v", k, c)
	}
}

// TestProxySameHostOtherPort: the exemption is the proxy's exact
// host:port as net/http dials it; the same host on another port, or
// written in another case, meets the guard.
func TestProxySameHostOtherPort(t *testing.T) {
	n := installNet(t)
	ln := n.listen() // not granted: only the proxy exemption reaches it
	t.Cleanup(func() { ln.Close() })
	n.MapRaw("proxy9.test", "3128", ln.Addr().String())
	n.MapRaw("proxy9.test", "3129", ln.Addr().String())
	gd := newGuardDialer(time.Second)
	gd.proxyAddr = "proxy9.test:3128"
	ctx := context.Background()
	c, err := gd.DialContext(ctx, "tcp", "proxy9.test:3128")
	if err != nil {
		t.Fatalf("the proxy address: %v", err)
	}
	c.Close()
	for _, addr := range []string{"proxy9.test:3129", "PROXY9.test:3128"} {
		_, err := gd.DialContext(ctx, "tcp", addr)
		if !errors.Is(err, ssrf.ErrProhibitedPort) && !errors.Is(err, ssrf.ErrProhibitedIP) {
			t.Errorf("%s: %v, want the guard's denial", addr, err)
		}
	}
}

// TestProxyUnmappedFailsClosed: a configured proxy the test network does
// not map fails before any DNS lookup.
func TestProxyUnmappedFailsClosed(t *testing.T) {
	n := installNet(t)
	n.HandleTLS("t.co", handlerOK)
	tr := guardedTransport(t, n, TransportOptions{Proxy: urlOf(t, "http://proxy.invalid:3128")})
	_, err := fetchVia(t, tr, "https://t.co/x")
	if !errors.Is(err, dialhook.ErrUnmapped) {
		t.Fatalf("%v, want ErrUnmapped", err)
	}
	if k, _ := classifyTransport(err, context.Background()); k != FailTransient {
		t.Errorf("classified %v", k)
	}
	if got := n.Unmapped(); len(got) != 1 || got[0] != "proxy.invalid:3128" {
		t.Errorf("Unmapped() = %q", got)
	}
}

// TestProxyErrorRedacted: no error or log line names the proxy URL, whose
// userinfo can hold credentials.
func TestProxyErrorRedacted(t *testing.T) {
	const secret = "s3cret"
	for _, raw := range []string{
		"http://user:" + secret + "@proxy.test/" + secret,
		"ftp://user:" + secret + "@proxy.test",
		"http://user:" + secret + "@proxy.test?" + secret,
	} {
		u := urlOf(t, raw)
		_, err := GuardTransport(&http.Transport{}, u)
		if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "proxy.test") {
			t.Errorf("GuardTransport(%s): %v", raw, err)
		}
		tr := NewTransport(TransportOptions{Proxy: u})
		if _, err := tr.Proxy(nil); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("Proxy() for %s: %v", raw, err)
		}
		var logs capturedLog
		_, err = checkTransport(tr, false, logs.logger())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("checkTransport(%s): %v", raw, err)
		}
		if _, err := checkTransport(tr, true, logs.logger()); err != nil {
			t.Fatal(err)
		}
		if logs.lines() != 1 || strings.Contains(logs.String(), secret) {
			t.Errorf("warning for %s: %s", raw, logs.String())
		}
	}

	// A valid proxy with credentials, on a transport that was changed.
	tr := NewTransport(TransportOptions{Proxy: urlOf(t, "http://user:"+secret+"@proxy.test:3128")})
	tr.DialTLSContext = (&dialSpy{}).DialContext
	var logs capturedLog
	_, err := checkTransport(tr, false, logs.logger())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("changed transport: %v", err)
	}
	checkTransport(tr, true, logs.logger())
	if strings.Contains(logs.String(), secret) {
		t.Errorf("warning: %s", logs.String())
	}
}

func TestGuardDialer(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("granted.test", handlerOK) // a listener the guard grants
	ctx := context.Background()
	denied := "127.0.0.1:443"

	var hook atomic.Int32
	control := func(string, string, syscall.RawConn) error { hook.Add(1); return nil }
	d := &net.Dialer{Timeout: time.Second, Control: control}
	gd := GuardDialer(d)
	if funcPC(d.Control) != funcPC(control) {
		t.Error("GuardDialer changed its argument")
	}
	if _, err := gd.DialContext(ctx, "tcp", denied); !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Errorf("Control chain, %s: %v", denied, err)
	}
	if hook.Load() != 0 {
		t.Error("the caller's Control ran after a denial")
	}
	c, err := gd.DialContext(ctx, "tcp", s.Addr())
	if err != nil {
		t.Fatalf("Control chain, granted listener: %v", err)
	}
	c.Close()
	if hook.Load() != 1 {
		t.Errorf("the caller's Control ran %d times, want 1", hook.Load())
	}

	// ControlContext wins over Control in net.Dialer, so the guard goes
	// there and Control is left alone.
	var hookCtx, hookPlain atomic.Int32
	controlCtx := func(context.Context, string, string, syscall.RawConn) error { hookCtx.Add(1); return nil }
	plain := func(string, string, syscall.RawConn) error { hookPlain.Add(1); return nil }
	gd = GuardDialer(&net.Dialer{Timeout: time.Second, ControlContext: controlCtx, Control: plain})
	if funcPC(gd.Control) != funcPC(plain) {
		t.Error("Control was replaced on a ControlContext dialer")
	}
	if _, err := gd.DialContext(ctx, "tcp", denied); !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Errorf("ControlContext chain, %s: %v", denied, err)
	}
	c, err = gd.DialContext(ctx, "tcp", s.Addr())
	if err != nil {
		t.Fatalf("ControlContext chain, granted listener: %v", err)
	}
	c.Close()
	if hookCtx.Load() != 1 || hookPlain.Load() != 0 {
		t.Errorf("ControlContext ran %d times, Control %d; want 1 and 0", hookCtx.Load(), hookPlain.Load())
	}

	// A hook error is returned after the guard allowed.
	errHook := errors.New("hook")
	gd = GuardDialer(&net.Dialer{Control: func(string, string, syscall.RawConn) error { return errHook }})
	if _, err := gd.DialContext(ctx, "tcp", s.Addr()); !errors.Is(err, errHook) {
		t.Errorf("hook error: %v", err)
	}

	gd = GuardDialer(nil)
	if _, err := gd.DialContext(ctx, "tcp", denied); !errors.Is(err, ssrf.ErrProhibitedIP) {
		t.Errorf("GuardDialer(nil): %v", err)
	}
}
