package unshorten

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"code.dny.dev/ssrf"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// guardVerdict runs the production guard on one dial as net.Dialer would
// pass it to Control: the network and address after Go's own resolution
// (an IPv4-mapped address is dialed as tcp4).
func guardVerdict(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return theGuard.control("tcp4", addr, nil)
	}
	network := "tcp6"
	if ap.Addr().Unmap().Is4() {
		network = "tcp4"
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return theGuard.control(network, ap.String(), nil)
}

// isDenial reports whether err is one of the guard's denials.
func isDenial(err error) bool {
	return errors.Is(err, ssrf.ErrProhibitedIP) || errors.Is(err, ssrf.ErrProhibitedPort) || errors.Is(err, ssrf.ErrProhibitedNetwork)
}

// ssrfTargets are the addresses an allowlisted host must never reach
// (U-R3-3). Each is denied by the guard before connect(2).
var ssrfTargets = []string{
	"127.0.0.1:443",
	"127.0.0.1:8443",
	"[::1]:443",
	"169.254.169.254:80",
	"100.64.0.1:443",
	"10.0.0.1:443",
	"192.168.1.1:443",
	"[::ffff:127.0.0.1]:443",
	"[::ffff:169.254.169.254]:443",
	"[64:ff9b::7f00:1]:443",
	"[64:ff9b::a9fe:a9fe]:443",
	"[fd00:ec2::254]:443",
	"[fe80::1]:443",
	"0.0.0.0:443",
	"168.63.129.16:80",
}

// TestSSRFTransport: an allowlisted name the test network maps to a
// non-public address is refused by the real guard in NewTransport's dial
// path, as a definitive FailTransport (U-R3-3).
func TestSSRFTransport(t *testing.T) {
	for _, target := range ssrfTargets {
		// Check the guard alone first, so a broken guard fails here and
		// the request below never reaches connect(2).
		if err := guardVerdict(target); !isDenial(err) {
			t.Fatalf("guard allows %s (%v); not sending the request", target, err)
		}
	}
	n := installNet(t)
	tr := guardedTransport(t, n, TransportOptions{})
	for _, target := range ssrfTargets {
		t.Run(target, func(t *testing.T) {
			n.MapRaw("t.co", "443", target)
			n.MapRaw("t.co", "80", target)
			for _, u := range []string{"https://t.co/x", "http://t.co/x"} {
				_, err := fetchVia(t, tr, u)
				if !isDenial(err) {
					t.Fatalf("%s → %s: %v, want the guard's denial", u, target, err)
				}
				if k, c := classifyTransport(err, context.Background()); k != FailTransport || c != classGuardDenied {
					t.Errorf("%s: classified %v/%v, want transport/guard denied", u, k, c)
				}
			}
		})
	}
}

// TestSSRFMapNameLocalhost: a name with both a loopback v6 and a v4
// address, dialed by Go's own resolver and Happy Eyeballs: [::1] is denied,
// the granted 127.0.0.1 listener is reached.
func TestSSRFMapNameLocalhost(t *testing.T) {
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	if !strings.HasPrefix(s.Addr(), "127.0.0.1:") {
		t.Skipf("listener on %s, not 127.0.0.1", s.Addr())
	}
	n.MapName("t.co", "localhost")
	tr := guardedTransport(t, n, TransportOptions{})
	resp, err := fetchVia(t, tr, "https://t.co/x")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || s.Hits() != 1 {
		t.Errorf("status %d, hits %d", resp.StatusCode, s.Hits())
	}
	// The same name on a port nothing is granted on is denied for both
	// families.
	_, port, _ := net.SplitHostPort(s.Addr())
	for _, a := range []string{"127.0.0.1:" + port, "[::1]:" + port} {
		if a == s.Addr() {
			continue
		}
		if err := guardVerdict(a); !isDenial(err) {
			t.Errorf("guard allows %s: %v", a, err)
		}
	}
}

// TestSSRFResolverCached: through the Resolver, a rebinding of an
// allowlisted host to a private address is a definitive failure, cached:
// the second call dials nothing (U-R3-3).
func TestSSRFResolverCached(t *testing.T) {
	needResolver(t)
	const target = "10.0.0.1:443"
	if err := guardVerdict(target); !isDenial(err) {
		t.Fatalf("guard allows %s", target)
	}
	n := installNet(t)
	n.MapRaw("t.co", "443", target)
	reg, err := NewRegistry(Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: Redirect()})
	if err != nil {
		t.Fatal(err)
	}
	var infos atomic.Int32
	var infoErr atomic.Value
	r := newTestResolver(t, Config{
		Registry:  reg,
		Transport: guardedTransport(t, n, TransportOptions{}),
		Limiter:   NoLimit(),
		OnRequest: func(ri RequestInfo) {
			infos.Add(1)
			if ri.Err != nil {
				infoErr.Store(ri.Err)
			}
		},
	})
	for i := range 2 {
		res, err := r.Resolve(context.Background(), "https://t.co/abc")
		if !errors.Is(err, ErrUnresolved) || errors.Is(err, ErrTransient) {
			t.Fatalf("call %d: %v, want a definitive failure", i, err)
		}
		if res.URL != "https://t.co/abc" || strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "/abc") {
			t.Errorf("call %d: URL %q, err %q", i, res.URL, err)
		}
		r.st.group.Wait()
		if got := n.Dials("t.co", "443"); got != 1 {
			t.Errorf("call %d: %d dials of t.co:443, want 1 (the failure is cached)", i, got)
		}
	}
	if infos.Load() != 1 {
		t.Errorf("OnRequest called %d times, want 1", infos.Load())
	}
	if e, _ := infoErr.Load().(error); !errors.Is(e, ssrf.ErrProhibitedIP) || !errors.Is(e, ErrUnresolved) {
		t.Errorf("RequestInfo.Err = %v, want ErrUnresolved and ssrf.ErrProhibitedIP", e)
	}
}

// TestTransportCheckNew is TestTransportCheck's table through New, with
// AllowUnguardedTransport and a captured Logger.
func TestTransportCheckNew(t *testing.T) {
	needResolver(t)
	for name, c := range map[string]struct {
		rt         http.RoundTripper
		ok, warned bool
	}{
		"nil":              {nil, true, false},
		"NewTransport":     {NewTransport(TransportOptions{}), true, false},
		"DefaultTransport": {origDefaultTransport, false, true},
		"Clone":            {NewTransport(TransportOptions{}).Clone(), false, true},
		"opaque":           {&opaqueTransport{}, false, false},
	} {
		var logs capturedLog
		_, err := New(Config{Transport: c.rt, Logger: logs.logger()})
		if c.ok != (err == nil) || (err != nil && !errors.Is(err, ErrUnguardedTransport)) {
			t.Errorf("%s: New: %v", name, err)
		}
		if name == "DefaultTransport" && (err == nil || !strings.Contains(err.Error(), "ProxyFromEnvironment")) {
			t.Errorf("%s: error %v does not name ProxyFromEnvironment", name, err)
		}
		_, err = New(Config{Transport: c.rt, Logger: logs.logger(), AllowUnguardedTransport: true})
		if err != nil {
			t.Errorf("%s: with Allow: %v", name, err)
		}
		want := 0
		if c.warned {
			want = 1
		}
		if logs.lines() != want {
			t.Errorf("%s: %d log lines, want %d: %s", name, logs.lines(), want, logs.String())
		}
	}
}

// TestAttestAfterNew: after New, changing the caller's transport does not
// affect the Resolver.
func TestAttestAfterNew(t *testing.T) {
	needResolver(t)
	n := installNet(t)
	s := n.HandleTLS("t.co", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.net/p", http.StatusMovedPermanently)
	}))
	reg, err := NewRegistry(Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: Redirect()})
	if err != nil {
		t.Fatal(err)
	}
	tr := guardedTransport(t, n, TransportOptions{})
	r := newTestResolver(t, Config{Registry: reg, Transport: tr, Limiter: NoLimit()})
	var spy dialSpy
	tr.DialContext = spy.DialContext
	tr.Proxy = http.ProxyFromEnvironment
	res, err := r.Resolve(context.Background(), "https://t.co/abc")
	if err != nil || res.URL != "https://example.net/p" {
		t.Fatalf("Resolve = %q, %v", res.URL, err)
	}
	if len(spy.calls()) != 0 || s.Hits() != 1 {
		t.Errorf("caller's DialContext calls %q, hits %d", spy.calls(), s.Hits())
	}
}

// TestProxyErrorRedactedNew: New's error and warning never name the proxy
// URL.
func TestProxyErrorRedactedNew(t *testing.T) {
	needResolver(t)
	tr := NewTransport(TransportOptions{Proxy: urlOf(t, "http://user:s3cret@proxy.test/s3cret")})
	var logs capturedLog
	_, err := New(Config{Transport: tr, Logger: logs.logger()})
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("New: %v", err)
	}
	if _, err := New(Config{Transport: tr, Logger: logs.logger(), AllowUnguardedTransport: true}); err != nil {
		t.Fatal(err)
	}
	if logs.lines() != 1 || strings.Contains(logs.String(), "s3cret") {
		t.Errorf("warning: %s", logs.String())
	}
}

// needResolver skips the test while New and the parts it builds on panic
// "not implemented": on a branch without them, the Resolver-level rows
// cannot run. Once they are merged the probe passes and the test runs;
// remove the helper then.
func needResolver(t *testing.T) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if s, ok := r.(string); ok && strings.HasSuffix(s, ": not implemented") {
				t.Skipf("needs the Resolver: %s", s)
			}
			panic(r)
		}
	}()
	reg, err := NewRegistry(Entry{Name: "t.co", Match: []string{"*://t.co/*"}, Strategy: Redirect()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{Registry: reg, Limiter: NoLimit()})
	if err != nil {
		t.Fatal(err)
	}
	r.Match("https://t.co/x")
	DefaultRegistry()
}

// TestProxyEnvIgnored: HTTP(S)_PROXY is never read, by NewTransport or by
// GuardTransport of a ProxyFromEnvironment transport. ProxyFromEnvironment
// reads the environment once per process, so the check runs in a child
// process with the variables set, pointing at a listener here that must
// see no connection.
func TestProxyEnvIgnored(t *testing.T) {
	if os.Getenv(proxyEnvHelper) != "" {
		t.Skip("in the helper process")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()
	proxy := "http://" + ln.Addr().String()
	cmd := exec.Command(os.Args[0], "-test.run=^TestProxyEnvHelper$", "-test.count=1", "-test.v")
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "no_proxy") && !strings.HasSuffix(strings.ToLower(k), "_proxy") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, proxyEnvHelper+"=1", "HTTPS_PROXY="+proxy, "HTTP_PROXY="+proxy, "https_proxy="+proxy, "http_proxy="+proxy, "ALL_PROXY="+proxy)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestProxyEnvHelper") {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if conns.Load() != 0 {
		t.Errorf("the environment's proxy got %d connections", conns.Load())
	}
}

const proxyEnvHelper = "LINKCLEAN_TEST_PROXY_ENV_HELPER"

// TestProxyEnvHelper is TestProxyEnvIgnored's child process.
func TestProxyEnvHelper(t *testing.T) {
	if os.Getenv(proxyEnvHelper) == "" {
		t.Skip("run by TestProxyEnvIgnored")
	}
	if p, _ := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: "t.co"}}); p == nil {
		t.Fatal("the environment's proxy is not set in the helper")
	}
	n := installNet(t)
	s := n.HandleTLS("t.co", handlerOK)
	envTr := origDefaultTransport.(*http.Transport).Clone()
	envTr.DialContext = nil
	envTr.TLSClientConfig = n.ClientTLS()
	guarded, err := GuardTransport(envTr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(guarded.CloseIdleConnections)
	for _, rt := range []http.RoundTripper{guardedTransport(t, n, TransportOptions{}), guarded} {
		if _, err := fetchVia(t, rt, "https://t.co/x"); err != nil {
			t.Fatal(err)
		}
	}
	if s.Hits() != 2 {
		t.Errorf("hits %d, want 2 direct requests", s.Hits())
	}
}

func TestGuardControlTable(t *testing.T) {
	if dialhook.AllowAddr() != nil {
		t.Fatal("an AllowAddr seam is installed")
	}
	type row struct {
		network, addr string
		want          error // nil: allowed
	}
	rows := []row{
		{"tcp4", "1.1.1.1:443", nil},
		{"tcp4", "1.1.1.1:80", nil},
		{"tcp4", "1.1.1.1:22", ssrf.ErrProhibitedPort},
		{"tcp4", "1.1.1.1:8080", ssrf.ErrProhibitedPort},
		{"tcp", "1.1.1.1:443", ssrf.ErrProhibitedNetwork},
		{"udp4", "1.1.1.1:443", ssrf.ErrProhibitedNetwork},
		{"tcp4", "1.1.1.1", ssrf.ErrInvalidHostPort},
		{"tcp4", "192.0.0.192:443", ssrf.ErrProhibitedIP},
		{"tcp4", "100.100.100.200:443", ssrf.ErrProhibitedIP},
		{"tcp4", "255.255.255.255:443", ssrf.ErrProhibitedIP},
		{"tcp4", "0.0.0.0:443", ssrf.ErrProhibitedIP},
		{"tcp4", "169.254.169.254:80", ssrf.ErrProhibitedIP},
		{"tcp4", "168.63.129.16:443", ssrf.ErrProhibitedIP}, // Azure WireServer (Q9)
		{"tcp4", "168.63.129.15:443", nil},
		{"tcp4", "168.63.129.17:443", nil},
		{"tcp6", "[2001::1]:443", ssrf.ErrProhibitedIP},       // Teredo, in 2001::/23
		{"tcp6", "[2002:7f00:1::]:443", ssrf.ErrProhibitedIP}, // 6to4
		{"tcp6", "[5f00::1]:443", ssrf.ErrProhibitedIP},       // outside 2000::/3
		{"tcp6", "[100::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[::]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[fe80::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[fe80::1%eth0]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[fc00::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[fd00:ec2::254]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[ff02::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[::ffff:127.0.0.1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[::ffff:8.8.8.8]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[::127.0.0.1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[64:ff9b::7f00:1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[64:ff9b::808:808]:443", ssrf.ErrProhibitedIP}, // NAT64 is denied whatever it embeds
		{"tcp6", "[64:ff9b:1::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[2001:db8::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[3fff::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[2620:4f:8000::1]:443", ssrf.ErrProhibitedIP},
		{"tcp6", "[2606:4700:4700::1111]:443", nil},
		{"tcp6", "[2606:4700:4700::1111]:22", ssrf.ErrProhibitedPort},
		// Documented limits (doc.go # Transport; verified against
		// code.dny.dev/ssrf v0.3.0 on 2026-10-18): addresses inside
		// 2000::/3 that embed a private IPv4 are allowed. A network-specific
		// NAT64 prefix (RFC 6052) here maps to 169.254.169.254; an ISATAP
		// interface ID (::5efe:a.b.c.d) embeds it too. Only an attacker who
		// controls DNS for an allowlisted host can steer a dial there.
		{"tcp6", "[2001:67c:2960:6464::a9fe:a9fe]:443", nil},
		{"tcp6", "[2600:1f00::5efe:a9fe:a9fe]:443", nil},
	}
	ctrl := GuardControl()
	check := func(r row) {
		t.Helper()
		err := ctrl(r.network, r.addr, nil)
		switch {
		case r.want == nil && err != nil:
			t.Errorf("%s %s: denied (%v), want allowed", r.network, r.addr, err)
		case r.want != nil && !errors.Is(err, r.want):
			t.Errorf("%s %s: %v, want %v", r.network, r.addr, err, r.want)
		}
	}
	for _, r := range rows {
		check(r)
	}
	// The first and last address of every special-purpose block, and of
	// everything outside 2000::/3.
	prefixes := append([]netip.Prefix{}, ssrf.IPv4DeniedPrefixes...)
	prefixes = append(prefixes, ssrf.IPv6DeniedPrefixes...)
	for _, s := range []string{"::/3", "4000::/2", "8000::/1", "64:ff9b::/96", "64:ff9b:1::/48", "fc00::/7", "fe80::/10", "ff00::/8", "168.63.129.16/32"} {
		prefixes = append(prefixes, netip.MustParsePrefix(s))
	}
	for _, p := range prefixes {
		network := "tcp4"
		if p.Addr().Is6() {
			network = "tcp6"
		}
		for _, a := range []netip.Addr{p.Masked().Addr(), lastAddr(p)} {
			check(row{network, netip.AddrPortFrom(a, 443).String(), ssrf.ErrProhibitedIP})
		}
	}
	// GuardControl is the one guard every transport uses.
	if funcPC(ctrl) != funcPC(theGuard.control) {
		t.Error("GuardControl is not theGuard.control")
	}
}

// lastAddr returns the last address of p.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// TestGuardAllowAddr: the AllowAddr seam exempts exactly the addresses it
// grants, never another port or the network check.
func TestGuardAllowAddr(t *testing.T) {
	granted := netip.MustParseAddrPort("127.0.0.1:4321")
	restore := dialhook.SetAllowAddr(func(a netip.AddrPort) bool { return a == granted })
	defer restore()
	for _, c := range []struct {
		network, addr string
		allowed       bool
	}{
		{"tcp4", "127.0.0.1:4321", true},
		{"tcp4", "127.0.0.1:4322", false},
		{"tcp4", "127.0.0.2:4321", false},
		{"tcp", "127.0.0.1:4321", false},
		{"tcp6", "[::1]:4321", false},
	} {
		err := theGuard.control(c.network, c.addr, nil)
		if (err == nil) != c.allowed {
			t.Errorf("%s %s: %v, allowed want %v", c.network, c.addr, err, c.allowed)
		}
	}
}
