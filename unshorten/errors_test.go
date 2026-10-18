package unshorten

// Failure rows of the §12.8 table (PLAN §6.2, owner A4t): the failure
// invariant over every FailureKind, TestRequestInfoNoURL over every failure
// path and error class (fresh and cached, Resolve and Process), and the
// panic log (C2.9). One table of failure paths serves all three.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"code.dny.dev/ssrf"
	"github.com/Espresso-Aficionados/linkclean"
)

// rvLog is a concurrency-safe log sink.
type rvLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *rvLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *rvLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *rvLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// rvFailRegistry is the failure table's registry:
//   - short (s.example.net): Redirect; a home.example.net root is its fallback;
//   - body (b.example.net): BodyRegexp of href="…";
//   - script (c.example.net, may also fetch f.example.net): strat;
//   - inter (i.example.net, Intermediate): fetches once, then ErrNotShortLink.
func rvFailRegistry(t *testing.T, strat Strategy) *Registry {
	short := rvEntry("s.example.net")
	short.Name = "short"
	short.IsFallback = func(_, target *url.URL) bool { return target.Host == "home.example.net" && target.Path == "/" }
	if strat == nil {
		strat = rvStopper()
	}
	return rvRegistry(t, short,
		Entry{Name: "body", Match: []string{"*://b.example.net/*"}, Strategy: BodyRegexp(regexp.MustCompile(`href="([^"]+)"`))},
		Entry{Name: "script", Match: []string{"*://c.example.net/*"}, FetchHosts: []string{"f.example.net"}, Strategy: strat},
		Entry{Name: "inter", Match: []string{"*://i.example.net/*"}, Strategy: rvStopper(), Intermediate: true},
	)
}

// rvStall waits for the request to end.
var rvStall = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })

// rvFailCase is one failure path.
type rvFailCase struct {
	name   string
	kind   FailureKind
	class  errClass // the fresh path's class, for the coverage check only
	want   []error  // sentinels besides ErrUnresolved
	status int      // a *StatusError with this status is wrapped
	cached bool     // definitive: later calls make no request
	infoIs []error  // every RequestInfo.Err wraps these
	offNet bool     // an opaque transport: no test network
	// setup registers servers on n (nil when offNet) and sets cfg's
	// Registry and anything else; it returns the input, which holds rvToken.
	setup func(t *testing.T, n *testNet, cfg *Config) string
	// after, if set, runs once the Resolver exists; its release, if any,
	// runs before the final drain.
	after func(t *testing.T, n *testNet, r *Resolver) (release func())
	ctx   func() (context.Context, context.CancelFunc)
}

const rvTok = "/" + rvToken

// rvScript returns a setup with a scripted strategy on c.example.net.
func rvScript(next func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error)) func(*testing.T, *testNet, *Config) string {
	return func(t *testing.T, n *testNet, cfg *Config) string {
		if n != nil {
			n.HandleTLS("c.example.net", rvBody("ok"))
			n.HandleTLS("f.example.net", rvBody("ok"))
		}
		cfg.Registry = rvFailRegistry(t, rvStrategy{name: "Script", next: next})
		return "https://c.example.net" + rvTok + "?utm_source=x"
	}
}

// rvShort returns a setup where s.example.net answers h.
func rvShort(h http.Handler, edit func(cfg *Config)) func(*testing.T, *testNet, *Config) string {
	return func(t *testing.T, n *testNet, cfg *Config) string {
		n.HandleTLS("s.example.net", h)
		cfg.Registry = rvFailRegistry(t, nil)
		if edit != nil {
			edit(cfg)
		}
		return "https://s.example.net" + rvTok + "?utm_source=x"
	}
}

// rvOpaque returns an offNet setup whose transport fails every request with
// a *url.Error around err.
func rvOpaque(err error) func(*testing.T, *testNet, *Config) string {
	return func(t *testing.T, _ *testNet, cfg *Config) string {
		cfg.Registry = rvFailRegistry(t, nil)
		cfg.AllowUnguardedTransport = true
		cfg.Transport = &rvFuncRT{fn: func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
		}}
		return "https://s.example.net" + rvTok
	}
}

func rvFailCases() []rvFailCase {
	return []rvFailCase{
		{name: "hop_limit", kind: FailHopLimit, want: []error{ErrHopLimit}, cached: true,
			setup: rvShort(rvRedirect(301, "https://s.example.net"+rvTok+"2"), func(cfg *Config) { cfg.MaxHops = 1 })},
		{name: "loop", kind: FailLoop, want: []error{ErrLoop}, cached: true,
			setup: rvShort(rvRedirect(301, "https://s.example.net"+rvTok+"?utm_source=x"), nil)},
		{name: "fallback", kind: FailFallback, want: []error{ErrFallback}, cached: true,
			setup: rvShort(rvRedirect(301, "https://home.example.net/"), nil)},
		{name: "no_redirect", kind: FailNoRedirect, want: []error{ErrNoRedirect}, cached: true,
			setup: rvShort(http.NotFoundHandler(), nil)},
		{name: "no_match", kind: FailNoMatch, want: []error{ErrNoMatch}, cached: true,
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.HandleTLS("b.example.net", rvBody("no link here"))
				cfg.Registry = rvFailRegistry(t, nil)
				return "https://b.example.net" + rvTok
			}},
		{name: "bad_target", kind: FailBadTarget, class: classBadTarget, want: []error{ErrBadTarget}, cached: true,
			setup: rvShort(rvRedirect(301, "https://127.0.0.1"+rvTok), nil)},
		{name: "status", kind: FailStatus, class: classStatus, status: 404, cached: true,
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.HandleTLS("b.example.net", http.NotFoundHandler())
				cfg.Registry = rvFailRegistry(t, nil)
				return "https://b.example.net" + rvTok
			}},
		{name: "fetch_denied", kind: FailFetchDenied, class: classFetchDenied, want: []error{ErrFetchDenied}, cached: true,
			setup: rvScript(func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
				_, err := f.Get(ctx, &url.URL{Scheme: "https", Host: "elsewhere.example.net", Path: rvTok}, false)
				return nil, err
			})},
		{name: "budget", kind: FailBudget, class: classBudget, want: []error{ErrBudget}, cached: true,
			setup: rvScript(func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
				var err error
				for range 3 {
					_, err = f.Get(ctx, u, false)
				}
				return nil, err
			})},
		{name: "refused", kind: FailTransport, class: classRefused, cached: true,
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.Refuse("s.example.net")
				cfg.Registry = rvFailRegistry(t, nil)
				return "https://s.example.net" + rvTok
			}},
		{name: "tls_verify", kind: FailTransport, class: classTLSVerify, cached: true,
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.WrongCert("s.example.net", rvRedirect(301, "https://example.net/p"))
				cfg.Registry = rvFailRegistry(t, nil)
				return "https://s.example.net" + rvTok
			}},
		{name: "guard_denied", kind: FailTransport, class: classGuardDenied, cached: true, infoIs: []error{ssrf.ErrProhibitedIP},
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.MapRaw("s.example.net", "443", "10.0.0.1:443")
				cfg.Registry = rvFailRegistry(t, nil)
				return "https://s.example.net" + rvTok
			}},
		{name: "dns_not_found", kind: FailTransport, class: classDNSNotFound, cached: true, offNet: true,
			setup: rvOpaque(&net.DNSError{Err: "no such host", Name: "s.example.net", IsNotFound: true})},
		{name: "dns_temporary", kind: FailTransient, class: classDNSTemporary, want: []error{ErrTransient}, offNet: true,
			setup: rvOpaque(&net.DNSError{Err: "server misbehaving", Name: "s.example.net", IsTemporary: true})},
		{name: "header_timeout", kind: FailTransient, class: classTimeout, want: []error{ErrTransient}, infoIs: []error{ErrTransient},
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				raw := rvShort(rvStall, nil)(t, n, cfg)
				cfg.Transport = rvTransport(n, TransportOptions{ResponseHeaderTimeout: 100 * time.Millisecond})
				return raw
			}},
		{name: "dial_timeout", kind: FailTransient, class: classTimeout, want: []error{ErrTransient}, infoIs: []error{ErrTransient},
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				raw := rvShort(rvRedirect(301, "https://example.net/p"), nil)(t, n, cfg)
				cfg.Transport = rvTransport(n, TransportOptions{DialTimeout: time.Nanosecond})
				return raw
			}},
		{name: "tls_stall", kind: FailTransient, class: classTimeout, want: []error{ErrTransient}, infoIs: []error{ErrTransient},
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.StallTLS("s.example.net")
				cfg.Registry = rvFailRegistry(t, nil)
				cfg.Transport = rvTransport(n, TransportOptions{TLSHandshakeTimeout: 100 * time.Millisecond})
				return "https://s.example.net" + rvTok
			}},
		{name: "deadline", kind: FailTransient, class: classDeadline, want: []error{ErrTransient},
			setup: rvShort(rvStall, func(cfg *Config) { cfg.Timeout = 300 * time.Millisecond })},
		{name: "canceled", kind: FailTransient, class: classCanceled, want: []error{ErrTransient, context.Canceled},
			setup: rvShort(rvStall, func(cfg *Config) { cfg.Timeout = time.Second }),
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(200*time.Millisecond, cancel)
				return ctx, cancel
			}},
		{name: "limiter", kind: FailTransient, class: classLimiter, want: []error{ErrTransient},
			setup: rvShort(rvRedirect(301, "https://example.net/p"), func(cfg *Config) {
				cfg.Limiter = &rvLimiter{err: func(string) error { return errors.New("limited " + rvToken) }}
			})},
		{name: "in_flight", kind: FailTransient, class: classInFlight, want: []error{ErrTransient},
			setup: rvShort(rvRoutes{"/hold": rvStall}, func(cfg *Config) { cfg.MaxInFlight = 1; cfg.Timeout = 2 * time.Second }),
			after: func(t *testing.T, n *testNet, r *Resolver) func() {
				go func() { _, _ = r.Resolve(context.Background(), "https://s.example.net/hold") }()
				rvWaitFor(t, "the in-flight holder", func() bool { total, _ := r.st.group.InFlight("s.example.net"); return total == 1 })
				return nil
			}},
		{name: "slot", kind: FailTransient, class: classSlot, want: []error{ErrTransient},
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				raw := rvShort(rvRedirect(301, "https://example.net/p"), func(cfg *Config) {
					cfg.MaxConcurrentRequestsPerHost = 1
					cfg.Timeout = 300 * time.Millisecond
					cfg.AllowUnguardedTransport = true
				})(t, n, cfg)
				cfg.Transport = &rvGatedRT{inner: rvTransport(n, TransportOptions{}), hold: func(req *http.Request) bool { return req.URL.Path == "/hold" }}
				return raw
			},
			after: func(t *testing.T, n *testNet, r *Resolver) func() {
				rt, ok := r.st.client.Transport.(*rvGatedRT)
				if !ok {
					t.Fatalf("New did not use the unguarded transport as given: %T", r.st.client.Transport)
				}
				rt.g = rvNewGate(t)
				go func() { _, _ = r.Resolve(context.Background(), "https://s.example.net/hold") }()
				rvWaitFor(t, "the slot holder", func() bool { return rt.g.arrived.Load() == 1 })
				return rt.g.open
			}},
		{name: "status_503", kind: FailTransient, class: classStatus, want: []error{ErrTransient}, status: 503,
			setup: rvShort(rvRedirect(503, ""), nil)},
		{name: "other", kind: FailTransient, class: classOther, want: []error{ErrTransient},
			setup: rvScript(func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
				return nil, errors.New("boom " + u.String())
			})},
		{name: "panic", kind: FailTransient, class: classOther, want: []error{ErrTransient},
			setup: rvScript(func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
				panic(fmt.Sprintf("bad %s", u))
			})},
		{name: "stop_intermediate", kind: FailNone, want: []error{ErrNotShortLink}, cached: true,
			setup: func(t *testing.T, n *testNet, cfg *Config) string {
				n.HandleTLS("i.example.net", rvBody("ok"))
				return rvShort(rvRedirect(301, "https://i.example.net"+rvTok+"?utm_source=y"), nil)(t, n, cfg)
			}},
	}
}

// rvFailRun is one run of a failure case: the errors and Results of a fresh
// Resolve, a Process and a second Resolve, and what was reported.
type rvFailRun struct {
	r       *Resolver
	raw     string
	results []Result
	procs   []linkclean.Result
	errs    []error
	infos   []RequestInfo
	log     string
}

func rvRunFailCase(t *testing.T, tc rvFailCase) *rvFailRun {
	t.Helper()
	var n *testNet
	if !tc.offNet {
		n = installNet(t)
	}
	infos := &rvInfos{}
	logs := &rvLog{}
	cfg := Config{OnRequest: infos.record, Logger: logs.logger(), Limiter: NoLimit()}
	raw := tc.setup(t, n, &cfg)
	if !strings.Contains(raw, rvToken) {
		t.Fatalf("input %q lacks the token", raw)
	}
	var r *Resolver
	if n != nil {
		r = rvResolver(t, n, cfg)
	} else {
		r = newTestResolver(t, cfg)
	}
	var release func()
	if tc.after != nil {
		release = tc.after(t, n, r)
	}
	run := &rvFailRun{r: r, raw: raw}
	ctx := func() (context.Context, context.CancelFunc) {
		if tc.ctx != nil {
			return tc.ctx()
		}
		return context.WithCancel(context.Background())
	}
	settled := -1
	for i := range 3 {
		c, cancel := ctx()
		if i == 1 {
			pr, err := r.Process(c, raw)
			run.procs = append(run.procs, pr)
			run.errs = append(run.errs, err)
		} else {
			res, err := r.Resolve(c, raw)
			run.results = append(run.results, res)
			run.errs = append(run.errs, err)
		}
		cancel()
		if tc.cached {
			r.st.group.Wait()
			if i == 0 {
				settled = len(infos.all())
			} else if got := len(infos.all()); got != settled {
				t.Errorf("%s: call %d made %d requests; a definitive outcome is cached", tc.name, i, got-settled)
			}
		}
	}
	if release != nil {
		release()
	}
	r.st.group.Wait()
	if tc.name == "panic" {
		rvWaitFor(t, "the panic log", func() bool { return strings.Contains(logs.String(), "panic_type") })
	}
	run.infos = infos.all()
	run.log = logs.String()
	return run
}

// rvCheckFailOpen asserts the failure contract on every call of a run.
func rvCheckFailOpen(t *testing.T, tc rvFailCase, run *rvFailRun) {
	t.Helper()
	r, raw := run.r, run.raw
	for i, err := range run.errs {
		for _, w := range append([]error{ErrUnresolved}, tc.want...) {
			if !errors.Is(err, w) {
				t.Errorf("call %d: %v does not wrap %q", i, err, w)
			}
		}
		if tc.status != 0 {
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Errorf("call %d: %v: want a *StatusError with status %d", i, err, tc.status)
			}
		}
		if tc.kind != FailTransient && errors.Is(err, ErrTransient) {
			t.Errorf("call %d: a definitive %s wraps ErrTransient", i, tc.kind)
		}
		if tc.ctx == nil && errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("call %d: %v wraps context.DeadlineExceeded though the caller's ctx did not end", i, err)
		}
	}
	for i, res := range run.results { // calls 0 and 2
		rvFailOpen(t, r, raw, res, run.errs[2*i], tc.want...)
	}
	clean := r.c.Clean(raw)
	for _, pr := range run.procs {
		if pr.URL != clean || pr.Input != raw || r.c.Clean(pr.URL) != pr.URL {
			t.Errorf("Process URL %q Input %q; want Clean(raw) %q", pr.URL, pr.Input, clean)
		}
		for _, h := range pr.Hops {
			if h.Category == linkclean.CatUnshortened {
				t.Errorf("a failed Process reports network hop %+v", h)
			}
		}
	}
}

// TestFailureInvariant: every failure path, one per FailureKind and error
// class at least, returns Clean(raw) with Clean(URL) == URL, from Resolve and
// from Process, fresh and cached, and wraps ErrUnresolved and its sentinel.
func TestFailureInvariant(t *testing.T) {
	kinds := map[FailureKind]bool{}
	classes := map[errClass]bool{}
	for _, tc := range rvFailCases() {
		kinds[tc.kind] = true
		classes[tc.class] = true
	}
	for k := FailHopLimit; k <= FailTransient; k++ {
		if !kinds[k] {
			t.Errorf("no failure case for %s", k)
		}
	}
	for c := classTimeout; c <= classOther; c++ {
		if !classes[c] {
			t.Errorf("no failure case for class %s", c)
		}
	}
	for _, tc := range rvFailCases() {
		t.Run(tc.name, func(t *testing.T) {
			rvCheckFailOpen(t, tc, rvRunFailCase(t, tc))
		})
	}
}

// rvLeaks matches what a sanitized text must never hold besides the token:
// an IPv4 address or a run of 4+ digits (a listener port).
var rvLeaks = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|\d{4,}|\[[0-9a-fA-F:]*:[0-9a-fA-F:]*\]`)

func rvCheckClean(t *testing.T, what, s string) {
	t.Helper()
	if strings.Contains(s, rvToken) {
		t.Errorf("%s holds the URL token: %q", what, s)
	}
	if m := rvLeaks.FindString(s); m != "" {
		t.Errorf("%s holds an address or port (%q): %q", what, m, s)
	}
	if strings.Contains(s, "://") {
		t.Errorf("%s holds a URL: %q", what, s)
	}
}

// TestRequestInfoNoURL: on every failure path (each FailureKind, each error
// class, fresh and cached, Resolve and Process) neither the returned error
// (as Error, %v and %+v) nor any RequestInfo's Err, Entry, Strategy or Host,
// nor the log, holds the token from the URL's path, an IP address or the
// listener port. RequestInfo.Err wraps ErrUnresolved, and the class
// sentinel where one exists.
func TestRequestInfoNoURL(t *testing.T) {
	for _, tc := range rvFailCases() {
		t.Run(tc.name, func(t *testing.T) {
			run := rvRunFailCase(t, tc)
			for i, err := range run.errs {
				if err == nil {
					t.Fatalf("call %d: nil error", i)
				}
				for _, s := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
					rvCheckClean(t, fmt.Sprintf("call %d error", i), s)
				}
			}
			for _, info := range run.infos {
				for _, s := range []string{info.Entry, info.Strategy, info.Host} {
					rvCheckClean(t, "RequestInfo", s)
				}
				if strings.ContainsAny(info.Host, "/?#@:") {
					t.Errorf("RequestInfo.Host %q is not a bare host", info.Host)
				}
				if info.Err == nil {
					continue
				}
				rvCheckClean(t, "RequestInfo.Err", info.Err.Error())
				rvCheckClean(t, "RequestInfo.Err %+v", fmt.Sprintf("%+v", info.Err))
				if !errors.Is(info.Err, ErrUnresolved) {
					t.Errorf("RequestInfo.Err %v does not wrap ErrUnresolved", info.Err)
				}
				if errors.Is(info.Err, context.DeadlineExceeded) {
					t.Errorf("RequestInfo.Err %v wraps context.DeadlineExceeded", info.Err)
				}
				for _, w := range tc.infoIs {
					if !errors.Is(info.Err, w) {
						t.Errorf("RequestInfo.Err %v does not wrap %v", info.Err, w)
					}
				}
			}
			if len(tc.infoIs) > 0 && len(run.infos) == 0 {
				t.Errorf("no RequestInfo to check")
			}
			// The log may hold a stack trace (addresses, line numbers): check
			// the token only.
			if strings.Contains(run.log, rvToken) {
				t.Errorf("the log holds the URL token:\n%s", run.log)
			}
		})
	}
}

// TestPanicLogNoURL: a strategy that panics with a value built from the URL
// fails the hop as transient and is logged at Error level with the entry,
// the host and the value's type, never the value (C2.9).
func TestPanicLogNoURL(t *testing.T) {
	var tc rvFailCase
	for _, c := range rvFailCases() {
		if c.name == "panic" {
			tc = c
		}
	}
	run := rvRunFailCase(t, tc)
	rvCheckFailOpen(t, tc, run)
	for _, want := range []string{"level=ERROR", "entry=script", "host=c.example.net", "panic_type=string", "stack="} {
		if !strings.Contains(run.log, want) {
			t.Errorf("log lacks %q:\n%s", want, run.log)
		}
	}
	if strings.Contains(run.log, rvToken) || strings.Contains(run.log, "bad https") {
		t.Errorf("the log holds the panic value:\n%s", run.log)
	}
}
