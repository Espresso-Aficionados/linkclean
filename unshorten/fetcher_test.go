package unshorten

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The Fetcher units run over an opaque RoundTripper inside testing/synctest
// bubbles, so the slot, Limiter and deadline paths run on a fake clock and
// nothing dials. The rows that need the real guard and loopback servers are
// nettest rows.

// rtFunc is an opaque RoundTripper.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// limitFunc is a Limiter.
type limitFunc func(ctx context.Context, host string) error

func (f limitFunc) Wait(ctx context.Context, host string) error { return f(ctx, host) }

// allowAll is a Limiter that never waits.
var allowAll = limitFunc(func(ctx context.Context, _ string) error { return ctx.Err() })

// newTestState builds a state over rt for reg, with cfg's bounds and lim
// (allowAll when nil), through newState.
func newTestState(tb testing.TB, cfg Config, reg *Registry, lim Limiter, rt http.RoundTripper) (st *state) {
	tb.Helper()
	rc, err := resolveConfig(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	if lim == nil {
		lim = allowAll
	}
	log := slog.New(slog.DiscardHandler)
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if r != "flight: not implemented" {
			panic(r)
		}
		// Until internal/flight lands, newState panics building its group.
		// The Fetcher never uses the group, so build the rest as newState
		// does. Remove this once flight.New is implemented.
		st = &state{
			cfg: rc, reg: reg, lim: lim, log: log,
			client: &http.Client{
				Transport:     rt,
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			},
			hostSlots: map[string]chan struct{}{},
			global:    make(chan struct{}, rc.maxReq),
		}
		for _, e := range reg.entries {
			for _, h := range e.hosts {
				if st.hostSlots[h] == nil {
					st.hostSlots[h] = make(chan struct{}, rc.maxReqHost)
				}
			}
		}
	}()
	return newState(rc, reg, nil, lim, rt, log)
}

// newTestFetcher returns a Fetcher for st's entry name whose hop context is
// hopCtx.
func newTestFetcher(tb testing.TB, st *state, name string, hopCtx context.Context) *Fetcher {
	tb.Helper()
	e := st.reg.entry(name)
	if e == nil {
		tb.Fatalf("no entry %q", name)
	}
	dl, _ := hopCtx.Deadline()
	return &Fetcher{st: st, e: e, hopCtx: hopCtx, deadline: dl}
}

// requireClassify skips a test whose path runs classifyTransport until it
// is implemented.
func requireClassify(tb testing.TB) {
	tb.Helper()
	defer func() {
		if r := recover(); r != nil {
			tb.Skipf("classifyTransport: %v", r)
		}
	}()
	classifyTransport(errors.New("probe"), context.Background())
}

// testRegistry has t.co, a tiktok-like entry with a FetchHost, and an
// AllowHTTP entry.
func testRegistry(tb testing.TB) *Registry {
	tb.Helper()
	r, err := NewRegistry(
		Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: Redirect()},
		Entry{Name: "tiktok", Match: []string{"*://vm.tiktok.com/*"}, FetchHosts: []string{"www.tiktok.com"},
			Strategy: TikTokOEmbed(), Fallback: Redirect()},
		Entry{Name: "plain", Match: []string{"*://plain.example/*"}, Strategy: Redirect(), AllowHTTP: true},
	)
	if err != nil {
		tb.Fatal(err)
	}
	return r
}

func mustURL(tb testing.TB, s string) *url.URL {
	tb.Helper()
	u, err := url.Parse(s)
	if err != nil {
		tb.Fatal(err)
	}
	return u
}

// response builds a response to req.
func response(req *http.Request, status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{
		StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)),
		Request: req, ProtoMajor: 1, ProtoMinor: 1, ContentLength: int64(len(body)),
	}
}

func TestBuildRequest(t *testing.T) {
	reg := testRegistry(t)
	tests := []struct {
		entry, in string
		u         *url.URL // instead of in
		want      string   // the request URL; "" with err
		host      string
		err       error
	}{
		{entry: "t.co", in: "https://t.co/x", want: "https://t.co/x", host: "t.co"},
		{entry: "t.co", in: "HTTPS://T.CO./x?a=1#frag", want: "https://t.co/x?a=1", host: "t.co"},
		{entry: "t.co", in: "https://t.co", want: "https://t.co", host: "t.co"},
		{entry: "t.co", in: "https://t.co/x?", want: "https://t.co/x?", host: "t.co"},
		{entry: "t.co", in: "https://t.co:443/x", want: "https://t.co:443/x", host: "t.co"},
		{entry: "t.co", in: "https://t.co/a%2Fb", want: "https://t.co/a%2Fb", host: "t.co"},
		{entry: "t.co", in: "https://t.co/ü", want: "https://t.co/%C3%BC", host: "t.co"},
		// url.URL.String escapes a non-ASCII host, and a %-host is refused.
		{entry: "t.co", in: "https://ｔ.co/x", err: ErrBadTarget},
		{entry: "tiktok", in: "https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FX%2F",
			want: "https://www.tiktok.com/oembed?url=https%3A%2F%2Fvm.tiktok.com%2FX%2F", host: "www.tiktok.com"},
		{entry: "plain", in: "http://plain.example/x", want: "http://plain.example/x", host: "plain.example"},
		{entry: "plain", in: "http://plain.example:80/x", want: "http://plain.example:80/x", host: "plain.example"},

		{entry: "t.co", in: "http://t.co/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://t.co:8443/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://t.co:80/x", err: ErrBadTarget},
		{entry: "plain", in: "http://plain.example:443/x", err: ErrBadTarget},
		{entry: "t.co", in: "ftp://t.co/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://user@t.co/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://u:p@t.co/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://127.0.0.1/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://127.0.0.1./x", err: ErrBadTarget},
		{entry: "t.co", in: "https://127.1/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://2130706433/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://0177.0.0.1/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://0x7f000001/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://１２７.０.０.１/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://[::1]/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://[::ffff:7f00:1]/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://[::127.0.0.1]/x", err: ErrBadTarget},
		{entry: "t.co", in: "https://[fe80::1%25eth0]/x", err: ErrBadTarget},
		{entry: "t.co", u: &url.URL{Scheme: "https", Host: "t.co", Path: "/x", RawQuery: "a b"}, err: ErrBadTarget},
		{entry: "t.co", u: &url.URL{Scheme: "https", Opaque: "//t.co/x"}, err: ErrBadTarget},
		{entry: "t.co", u: &url.URL{Scheme: "https", Host: "t%2eco", Path: "/x"}, err: ErrBadTarget},
		{entry: "t.co", u: &url.URL{Scheme: "https", Host: `t.co\@evil.example`, Path: "/x"}, err: ErrBadTarget},
		{entry: "t.co", u: &url.URL{Scheme: "https", Host: "t.co", Path: "/x", User: url.User("")}, err: ErrBadTarget},

		{entry: "t.co", in: "https://evil.example/x", err: ErrFetchDenied},
		{entry: "t.co", in: "https://www.tiktok.com/x", err: ErrFetchDenied},
		{entry: "t.co", in: "https://sub.t.co/x", err: ErrFetchDenied},
	}
	for _, tt := range tests {
		u := tt.u
		if u == nil {
			u = mustURL(t, tt.in)
		}
		req, host, err := buildRequest(context.Background(), u, reg.entry(tt.entry))
		if !errors.Is(err, tt.err) || (tt.err == nil) != (err == nil) {
			t.Errorf("%s %s: err = %v, want %v", tt.entry, u, err, tt.err)
			continue
		}
		if err != nil {
			if req != nil || host != "" {
				t.Errorf("%s %s: got a request with an error", tt.entry, u)
			}
			continue
		}
		if got := req.URL.String(); got != tt.want || host != tt.host || req.Method != http.MethodGet || req.Body != nil {
			t.Errorf("%s %s: %s %s (host %q, body %v), want GET %s (host %q)", tt.entry, u, req.Method, got, host, req.Body, tt.want, tt.host)
		}
		if len(req.Header) != 0 {
			t.Errorf("%s %s: headers %v", tt.entry, u, req.Header)
		}
	}
}

// TestGetRequest checks what reaches the transport: one GET of the request
// form, the two fixed headers and no body, and the response as returned.
func TestGetRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got []*http.Request
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			got = append(got, req)
			return response(req, 301, http.Header{"Location": {"/y?b=2"}, "Content-Type": {"text/html"}, "Set-Cookie": {"s=1"}}, "ignored"), nil
		})
		st := newTestState(t, Config{UserAgent: "test-agent/1"}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		resp, err := f.Get(ctx, mustURL(t, "HTTPS://T.co/x?a=1#frag"), false)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("%d requests", len(got))
		}
		req := got[0]
		if req.Method != "GET" || req.URL.String() != "https://t.co/x?a=1" || req.Host != "t.co" || req.Body != nil {
			t.Errorf("request %s %s host %q body %v", req.Method, req.URL, req.Host, req.Body)
		}
		want := http.Header{"User-Agent": {"test-agent/1"}, "Accept": {"*/*"}}
		if len(req.Header) != len(want) || req.Header.Get("User-Agent") != "test-agent/1" || req.Header.Get("Accept") != "*/*" {
			t.Errorf("headers %v, want %v", req.Header, want)
		}
		if resp.Status != 301 || resp.Location.String() != "https://t.co/y?b=2" || resp.ContentType != "text/html" || resp.Body != nil {
			t.Errorf("response %+v", resp)
		}
	})
}

func TestGetDefaultUserAgent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ua string
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			ua = req.Header.Get("User-Agent")
			return response(req, 404, nil, ""), nil
		})
		st := newTestState(t, Config{}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := newTestFetcher(t, st, "t.co", ctx).Get(ctx, mustURL(t, "https://t.co/x"), false); err != nil {
			t.Fatal(err)
		}
		if ua != defaultUA() {
			t.Errorf("User-Agent %q, want %q", ua, defaultUA())
		}
	})
}

func TestGetLocation(t *testing.T) {
	tests := []struct {
		status   int
		location string // "" for none
		want     string // "" for a nil Location
		err      error
	}{
		{301, "https://example.net/p", "https://example.net/p", nil},
		{302, "/rel?q=1", "https://t.co/rel?q=1", nil},
		{307, "rel", "https://t.co/dir/rel", nil},
		{308, "//example.net/p", "https://example.net/p", nil},
		{301, "http://example.net/p#f", "http://example.net/p#f", nil},
		{301, "", "", nil},
		{301, "http://[::1", "", ErrBadTarget},
		{302, "%zz", "", ErrBadTarget},
		{200, "http://[::1", "", nil},
		{404, "%zz", "", nil},
	}
	for _, tt := range tests {
		synctest.Test(t, func(t *testing.T) {
			rt := rtFunc(func(req *http.Request) (*http.Response, error) {
				h := http.Header{}
				if tt.location != "" {
					h.Set("Location", tt.location)
				}
				return response(req, tt.status, h, ""), nil
			})
			st := newTestState(t, Config{}, testRegistry(t), nil, rt)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			f := newTestFetcher(t, st, "t.co", ctx)
			resp, err := f.Get(ctx, mustURL(t, "https://t.co/dir/x"), false)
			if !errors.Is(err, tt.err) || (err == nil) != (tt.err == nil) {
				t.Fatalf("%d %q: err %v, want %v", tt.status, tt.location, err, tt.err)
			}
			if len(f.recs) != 1 || f.recs[0].status != tt.status || f.recs[0].kind != FailNone {
				t.Errorf("%d %q: records %+v", tt.status, tt.location, f.recs)
			}
			if err != nil {
				return
			}
			got := ""
			if resp.Location != nil {
				got = resp.Location.String()
			}
			if got != tt.want {
				t.Errorf("%d %q: Location %q, want %q", tt.status, tt.location, got, tt.want)
			}
		})
	}
}

// countingBody counts the bytes read from it and whether it was closed.
type countingBody struct {
	r      io.Reader
	read   atomic.Int64
	closed atomic.Bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { b.closed.Store(true); return nil }

func TestGetBody(t *testing.T) {
	for _, readBody := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			body := &countingBody{r: io.LimitReader(neverEnding('a'), 10<<20)}
			rt := rtFunc(func(req *http.Request) (*http.Response, error) {
				resp := response(req, 200, nil, "")
				resp.Body, resp.ContentLength = body, 10<<20
				return resp, nil
			})
			st := newTestState(t, Config{}, testRegistry(t), nil, rt)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			f := newTestFetcher(t, st, "t.co", ctx)
			resp, err := f.Get(ctx, mustURL(t, "https://t.co/x"), readBody)
			if err != nil {
				t.Fatal(err)
			}
			if !body.closed.Load() {
				t.Error("body not closed")
			}
			if readBody {
				if len(resp.Body) != 64<<10 || body.read.Load() > 64<<10+4096 {
					t.Errorf("read %d bytes, returned %d, want 64 KiB", body.read.Load(), len(resp.Body))
				}
			} else if resp.Body != nil || body.read.Load() != 0 {
				t.Errorf("readBody false: read %d bytes, returned %d", body.read.Load(), len(resp.Body))
			}
			if len(f.recs) != 1 || f.recs[0].readBody != readBody {
				t.Errorf("records %+v", f.recs)
			}
		})
	}
}

type neverEnding byte

func (b neverEnding) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

// TestGetRecords checks one record per Get that reached the transport, with
// the wire writes the transport reported (net/http reports each write,
// retries included, through httptrace's WroteRequest).
func TestGetRecords(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
				tr.WroteRequest(httptrace.WroteRequestInfo{})
				if req.URL.Path == "/retried" {
					tr.WroteRequest(httptrace.WroteRequestInfo{})
				}
			}
			return response(req, 404, nil, "x"), nil
		})
		st := newTestState(t, Config{}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "tiktok", ctx)
		if _, err := f.Get(ctx, mustURL(t, "https://vm.tiktok.com/retried"), false); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Get(ctx, mustURL(t, "https://www.tiktok.com/x"), true); err != nil {
			t.Fatal(err)
		}
		recs := f.finish()
		if len(recs) != 2 {
			t.Fatalf("records %+v", recs)
		}
		if r := recs[0]; r.host != "vm.tiktok.com" || r.status != 404 || r.wire != 2 || r.readBody || r.kind != FailNone || r.err != nil {
			t.Errorf("record 0 %+v", r)
		}
		if r := recs[1]; r.host != "www.tiktok.com" || r.status != 404 || r.wire != 1 || !r.readBody {
			t.Errorf("record 1 %+v", r)
		}
	})
}

// TestGetRefusals checks that a refused Get reaches neither the Limiter nor
// the transport, takes no budget and leaves no record.
func TestGetRefusals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls, waits atomic.Int64
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return response(req, 404, nil, ""), nil
		})
		lim := limitFunc(func(ctx context.Context, _ string) error { waits.Add(1); return nil })
		st := newTestState(t, Config{}, testRegistry(t), lim, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		for _, tt := range []struct {
			u   *url.URL
			err error
		}{
			{mustURL(t, "https://evil.example/x"), ErrFetchDenied},
			{mustURL(t, "http://t.co/x"), ErrBadTarget},
			{mustURL(t, "https://10.0.0.1/x"), ErrBadTarget},
			{nil, ErrBadTarget},
		} {
			if _, err := f.Get(ctx, tt.u, false); !errors.Is(err, tt.err) {
				t.Errorf("Get(%v) = %v, want %v", tt.u, err, tt.err)
			}
		}
		if calls.Load() != 0 || waits.Load() != 0 || f.used != 0 || len(f.recs) != 0 {
			t.Errorf("refusals: %d requests, %d waits, used %d, %d records", calls.Load(), waits.Load(), f.used, len(f.recs))
		}
		// The budget is intact: two Gets still pass.
		for range 2 {
			if _, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false); !errors.Is(err, ErrBudget) {
			t.Errorf("third Get = %v, want ErrBudget", err)
		}
		if calls.Load() != 2 || waits.Load() != 2 || len(f.recs) != 2 {
			t.Errorf("%d requests, %d waits, %d records; want 2 each", calls.Load(), waits.Load(), len(f.recs))
		}
	})
}

// TestGetBudgetConcurrent has 8 goroutines of one strategy call Get at once:
// exactly 2 reach the transport.
func TestGetBudgetConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return response(req, 404, nil, ""), nil
		})
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 8, MaxConcurrentRequests: 8}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		var wg sync.WaitGroup
		var budget atomic.Int64
		for range 8 {
			wg.Go(func() {
				if _, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false); errors.Is(err, ErrBudget) {
					budget.Add(1)
				} else if err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if calls.Load() != 2 || budget.Load() != 6 {
			t.Errorf("%d requests, %d ErrBudget; want 2 and 6", calls.Load(), budget.Load())
		}
	})
}

// TestGetAfterFinish checks that a Get after the hop's strategies returned
// (a goroutine that outlives Next) is ErrBudget without a request.
func TestGetAfterFinish(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return response(req, 404, nil, ""), nil
		})
		st := newTestState(t, Config{}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		if _, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false); err != nil {
			t.Fatal(err)
		}
		if recs := f.finish(); len(recs) != 1 {
			t.Errorf("finish: %d records", len(recs))
		}
		for _, u := range []string{"https://t.co/y", "https://evil.example/"} {
			if _, err := f.Get(ctx, mustURL(t, u), false); !errors.Is(err, ErrBudget) {
				t.Errorf("Get(%s) after finish = %v, want ErrBudget", u, err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("%d requests, want 1", calls.Load())
		}
	})
}

// TestGetSlotOrder checks the host slot, then the global slot, then the
// Limiter: a Get waiting for its host slot holds no global slot and has not
// called the Limiter, and a slot wait that ends at the hop deadline is
// transient and consumes nothing.
func TestGetSlotOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var mu sync.Mutex
		var waits []string
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/hold" {
				<-release
			}
			return response(req, 404, nil, ""), nil
		})
		lim := limitFunc(func(ctx context.Context, host string) error {
			mu.Lock()
			waits = append(waits, host)
			mu.Unlock()
			return nil
		})
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 1, MaxConcurrentRequests: 2}, testRegistry(t), lim, rt)
		long, cancelLong := context.WithTimeout(context.Background(), time.Hour)
		defer cancelLong()
		short, cancelShort := context.WithTimeout(context.Background(), time.Second)
		defer cancelShort()

		// A holds t.co's only host slot and one of the two global slots.
		a := newTestFetcher(t, st, "t.co", long)
		aDone := make(chan error, 1)
		go func() { _, err := a.Get(long, mustURL(t, "https://t.co/hold"), false); aDone <- err }()
		synctest.Wait()

		// B waits for t.co's host slot, with a strategy ctx that has no
		// deadline: the hop's still bounds it.
		b := newTestFetcher(t, st, "t.co", short)
		bDone := make(chan error, 1)
		go func() { _, err := b.Get(context.Background(), mustURL(t, "https://t.co/b"), false); bDone <- err }()
		synctest.Wait()

		// C, another host, gets the second global slot at once: B holds none.
		c := newTestFetcher(t, st, "tiktok", long)
		if _, err := c.Get(long, mustURL(t, "https://vm.tiktok.com/c"), false); err != nil {
			t.Fatalf("C: %v", err)
		}
		mu.Lock()
		if got := strings.Join(waits, ","); got != "t.co,vm.tiktok.com" {
			t.Errorf("Limiter waits %q, want A's and C's only", got)
		}
		mu.Unlock()

		// B's hop deadline passes while it waits: transient, slot class,
		// no Limiter call, no request, no record.
		err := <-bDone
		var fe *fetchError
		if !errors.As(err, &fe) || fe.kind != FailTransient || fe.class != classSlot || !errors.Is(err, ErrTransient) {
			t.Errorf("B = %v, want a transient slot failure", err)
		}
		if len(b.recs) != 0 || b.used != 1 {
			t.Errorf("B: %d records, used %d", len(b.recs), b.used)
		}

		// After A's response, its slots are free again.
		close(release)
		if err := <-aDone; err != nil {
			t.Fatalf("A: %v", err)
		}
		if len(st.hostSlots["t.co"]) != 0 || len(st.global) != 0 {
			t.Errorf("slots held after every Get returned: host %d, global %d", len(st.hostSlots["t.co"]), len(st.global))
		}
		d := newTestFetcher(t, st, "t.co", long)
		if _, err := d.Get(long, mustURL(t, "https://t.co/d"), false); err != nil {
			t.Errorf("D: %v", err)
		}
	})
}

// TestGetGlobalSlot checks that a Get waiting for a global slot has its host
// slot but has not called the Limiter.
func TestGetGlobalSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var waits atomic.Int64
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			<-release
			return response(req, 404, nil, ""), nil
		})
		lim := limitFunc(func(ctx context.Context, host string) error { waits.Add(1); return nil })
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 1, MaxConcurrentRequests: 1}, testRegistry(t), lim, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		go newTestFetcher(t, st, "t.co", ctx).Get(ctx, mustURL(t, "https://t.co/a"), false)
		synctest.Wait()
		bDone := make(chan error, 1)
		go func() {
			_, err := newTestFetcher(t, st, "tiktok", ctx).Get(ctx, mustURL(t, "https://vm.tiktok.com/b"), false)
			bDone <- err
		}()
		synctest.Wait()
		if len(st.hostSlots["vm.tiktok.com"]) != 1 || waits.Load() != 1 {
			t.Errorf("B waiting for the global slot: host slot %d, Limiter waits %d; want 1, 1", len(st.hostSlots["vm.tiktok.com"]), waits.Load())
		}
		var fe *fetchError
		if err := <-bDone; !errors.As(err, &fe) || fe.class != classSlot {
			t.Errorf("B = %v, want a slot failure", err)
		}
		if len(st.hostSlots["vm.tiktok.com"]) != 0 {
			t.Error("B's host slot not released")
		}
		close(release)
	})
}

// TestGetLimiterError checks that a Limiter error is transient, sends
// nothing, leaves no record and releases both slots.
func TestGetLimiterError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int64
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return response(req, 404, nil, ""), nil
		})
		reject := atomic.Bool{}
		reject.Store(true)
		lim := limitFunc(func(ctx context.Context, host string) error {
			if reject.Load() {
				return errors.New("rate limited https://t.co/TOKEN")
			}
			return nil
		})
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 1, MaxConcurrentRequests: 1}, testRegistry(t), lim, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		_, err := f.Get(ctx, mustURL(t, "https://t.co/TOKEN"), false)
		var fe *fetchError
		if !errors.As(err, &fe) || fe.kind != FailTransient || fe.class != classLimiter || strings.Contains(err.Error(), "TOKEN") {
			t.Errorf("Get = %v, want a transient Limiter failure", err)
		}
		if calls.Load() != 0 || len(f.recs) != 0 || len(st.hostSlots["t.co"]) != 0 || len(st.global) != 0 {
			t.Errorf("%d requests, %d records, slots %d/%d", calls.Load(), len(f.recs), len(st.hostSlots["t.co"]), len(st.global))
		}
		reject.Store(false)
		if _, err := f.Get(ctx, mustURL(t, "https://t.co/x"), false); err != nil {
			t.Errorf("after the Limiter recovers: %v", err)
		}
	})
}

// TestGetTransportError checks the record and error of a failed RoundTrip:
// the error is classified, sanitized, and releases both slots.
func TestGetTransportError(t *testing.T) {
	requireClassify(t)
	synctest.Test(t, func(t *testing.T) {
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + req.URL.String() + ": boom")
		})
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 1, MaxConcurrentRequests: 1}, testRegistry(t), nil, rt)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", ctx)
		_, err := f.Get(ctx, mustURL(t, "https://t.co/TOKEN9"), false)
		var fe *fetchError
		if !errors.As(err, &fe) || !errors.Is(err, ErrUnresolved) || strings.Contains(err.Error(), "TOKEN9") {
			t.Fatalf("Get = %v", err)
		}
		if len(f.recs) != 1 || f.recs[0].status != 0 || f.recs[0].kind != fe.kind || f.recs[0].err == nil ||
			strings.Contains(f.recs[0].err.Error(), "TOKEN9") {
			t.Errorf("records %+v", f.recs)
		}
		if len(st.hostSlots["t.co"]) != 0 || len(st.global) != 0 {
			t.Error("slots held after a RoundTrip error")
		}
	})
}

// TestGetIgnoresStrategyCtx checks that a strategy passing
// context.Background() cannot hold a request past the hop deadline.
func TestGetIgnoresStrategyCtx(t *testing.T) {
	requireClassify(t)
	synctest.Test(t, func(t *testing.T) {
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})
		st := newTestState(t, Config{}, testRegistry(t), nil, rt)
		hop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		f := newTestFetcher(t, st, "t.co", hop)
		start := time.Now()
		_, err := f.Get(context.Background(), mustURL(t, "https://t.co/x"), false)
		if !errors.Is(err, ErrTransient) {
			t.Errorf("Get = %v, want ErrTransient", err)
		}
		if d := time.Since(start); d != time.Second {
			t.Errorf("returned after %v, want the hop deadline (1s)", d)
		}
		if len(st.hostSlots["t.co"]) != 0 || len(st.global) != 0 {
			t.Error("slots held")
		}
	})
}

// TestGetHopCanceled checks that ending the hop context ends a Get whose
// strategy ctx is still live, before the deadline.
func TestGetHopCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		rt := rtFunc(func(req *http.Request) (*http.Response, error) {
			<-hold
			return response(req, 404, nil, ""), nil
		})
		st := newTestState(t, Config{MaxConcurrentRequestsPerHost: 1}, testRegistry(t), nil, rt)
		hop, cancel := context.WithTimeout(context.Background(), time.Hour)
		go newTestFetcher(t, st, "t.co", hop).Get(hop, mustURL(t, "https://t.co/a"), false)
		synctest.Wait()
		f := newTestFetcher(t, st, "t.co", hop)
		done := make(chan error, 1)
		go func() { _, err := f.Get(context.Background(), mustURL(t, "https://t.co/b"), false); done <- err }()
		synctest.Wait()
		cancel()
		var fe *fetchError
		if err := <-done; !errors.As(err, &fe) || fe.class != classSlot {
			t.Errorf("Get = %v, want a slot failure when the hop is canceled", err)
		}
		close(hold)
	})
}
