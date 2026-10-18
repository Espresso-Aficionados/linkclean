package unshorten

// The test network: loopback TLS and plain-http listeners that production
// code reaches by their real hostnames, with the real guard in the dial path.
// installNet sets dialhook.Resolve (host:443 and host:80 → the registered
// listener) and dialhook.AllowAddr (exactly the registered listeners), so a
// request to https://t.co/x passes the Fetcher's allowlist, goes through
// guardDialer.DialContext and guardControl, and verifies t.co's certificate
// against the test CA. Every other address still meets the real ssrf policy.
//
// Cleanups run LIFO: call installNet before newTestResolver, so a Resolver's
// detached hops drain before the seams are restored and the servers close.
// Violations (a request breaking the fixed header set, a dial to an unmapped
// host) are recorded and reported from installNet's cleanup on the test
// goroutine, never from the goroutine that saw them.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
)

// netInstalled is set while a testNet is installed.
var netInstalled atomic.Bool

// testNet is one test's network. Only one may be installed at a time: a
// second concurrent installNet fails the test (no t.Parallel overlap).
type testNet struct {
	tb     testing.TB
	caDER  []byte
	caKey  *ecdsa.PrivateKey
	caCert *x509.Certificate
	pool   *x509.CertPool
	done   chan struct{} // closed first in cleanup: stall handlers return
	wg     sync.WaitGroup

	mu           sync.Mutex
	ua           string            // the User-Agent every request must carry
	routes       map[string]string // "host:port" (host lowercased) → address dialed
	allow        map[netip.AddrPort]bool
	dials        map[string]int // Resolve calls per "host:port"
	unmapped     []string
	unmappedSeen int // Unmapped has returned unmapped[:unmappedSeen]
	violations   []string
	closers      []func()
	leaves       map[string]tls.Certificate
	proxies      int
}

// testServer is one registered listener.
type testServer struct {
	n    *testNet
	host string
	addr string // the listener address
	mu   sync.Mutex
	reqs []recordedRequest
}

// recordedRequest is one request a testServer received.
type recordedRequest struct {
	Method     string
	Host       string // the Host header (r.Host)
	Path       string // r.URL.Path
	RawQuery   string
	RequestURI string // as received
	Proto      string // "HTTP/1.1" or "HTTP/2.0"
	Header     http.Header
	BodyLen    int
}

// installNet installs a test network for tb, generating a test CA (ECDSA
// P-256) that issues each registered host's leaf certificate, and restores
// the seams and closes every listener on cleanup.
func installNet(tb testing.TB) *testNet {
	tb.Helper()
	if !netInstalled.CompareAndSwap(false, true) {
		tb.Fatal("nettest: installNet: another test network is installed (tests using it must not run in parallel)")
	}
	n := &testNet{
		tb:     tb,
		done:   make(chan struct{}),
		ua:     defaultUA(),
		routes: map[string]string{},
		allow:  map[netip.AddrPort]bool{},
		dials:  map[string]int{},
		leaves: map[string]tls.Certificate{},
	}
	n.newCA()
	restoreResolve := dialhook.SetResolve(n.resolve)
	restoreAllow := dialhook.SetAllowAddr(n.allowAddr)
	tb.Cleanup(func() {
		close(n.done)
		n.mu.Lock()
		closers := n.closers
		n.closers = nil
		n.mu.Unlock()
		for _, c := range slices.Backward(closers) {
			c()
		}
		n.wg.Wait()
		restoreAllow()
		restoreResolve()
		netInstalled.Store(false)
		n.mu.Lock()
		defer n.mu.Unlock()
		for _, v := range n.violations {
			tb.Errorf("nettest: %s", v)
		}
		for _, u := range n.unmapped[n.unmappedSeen:] {
			tb.Errorf("nettest: dial to %s, which the test network does not map", u)
		}
	})
	return n
}

func (n *testNet) newCA() {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		n.tb.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "linkclean nettest CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		n.tb.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		n.tb.Fatal(err)
	}
	n.caDER, n.caKey, n.caCert = der, key, cert
	n.pool = x509.NewCertPool()
	n.pool.AddCert(cert)
}

// leaf returns the CA-issued leaf certificate for name.
func (n *testNet) leaf(name string) tls.Certificate {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.leaves[name]; ok {
		return c
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		n.tb.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		n.tb.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, n.caCert, &key.PublicKey, n.caKey)
	if err != nil {
		n.tb.Fatal(err)
	}
	c := tls.Certificate{Certificate: [][]byte{der, n.caDER}, PrivateKey: key}
	n.leaves[name] = c
	return c
}

// resolve is the Resolve seam.
func (n *testNet) resolve(host, port string) (string, bool) {
	key := net.JoinHostPort(strings.ToLower(host), port)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dials[key]++
	addr, ok := n.routes[key]
	if !ok {
		n.unmapped = append(n.unmapped, key)
	}
	return addr, ok
}

// allowAddr is the AllowAddr seam.
func (n *testNet) allowAddr(a netip.AddrPort) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.allow[a]
}

// register maps host:port to addr, grants addr when allow is set, and
// adds closer to the cleanup.
func (n *testNet) register(host, port, addr string, allow bool, closer func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[net.JoinHostPort(strings.ToLower(host), port)] = addr
	if allow {
		ap, err := netip.ParseAddrPort(addr)
		if err != nil {
			n.tb.Fatalf("nettest: listener address %q: %v", addr, err)
		}
		n.allow[ap] = true
	}
	if closer != nil {
		n.closers = append(n.closers, closer)
	}
}

func (n *testNet) violate(format string, args ...any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.violations = append(n.violations, fmt.Sprintf(format, args...))
}

// ClientTLS returns the client TLS config trusting the test CA, for
// TransportOptions.TLSConfig.
func (n *testNet) ClientTLS() *tls.Config {
	return &tls.Config{RootCAs: n.pool}
}

// Done is closed when the test ends, before any listener closes. Stalling
// handlers select on it, so closing a server never waits on them.
func (n *testNet) Done() <-chan struct{} { return n.done }

// SetUserAgent sets the User-Agent every request must carry (default
// defaultUA()), for tests with Config.UserAgent set.
func (n *testNet) SetUserAgent(ua string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ua = ua
}

// Dials returns how many times Resolve was asked for host:port.
func (n *testNet) Dials(host, port string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.dials[net.JoinHostPort(strings.ToLower(host), port)]
}

// TakeViolations returns the header-set violations recorded so far and
// clears them, so a test that provokes one on purpose can check it rather
// than fail in cleanup.
func (n *testNet) TakeViolations() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	v := n.violations
	n.violations = nil
	return v
}

// HandleTLS serves h for host on port 443 (HTTP/2 and HTTP/1.1). Every
// request is checked against the production header set: GET, User-Agent
// defaultUA(), Accept */*, no Cookie, Referer, Authorization,
// Accept-Encoding or Accept-Language, an empty body, and a Host equal to the
// URL host.
//
// The check is strict: any header but User-Agent and Accept is a
// violation.
func (n *testNet) HandleTLS(host string, h http.Handler) *testServer {
	return n.serveTLS(host, host, h, true)
}

// HandleTLSH1 is HandleTLS with HTTP/1.1 only.
func (n *testNet) HandleTLSH1(host string, h http.Handler) *testServer {
	return n.serveTLS(host, host, h, false)
}

// WrongCert serves h for host on 443 with a leaf certificate for another
// name, so verification fails.
func (n *testNet) WrongCert(host string, h http.Handler) *testServer {
	return n.serveTLS(host, "wrong-name.invalid", h, true)
}

func (n *testNet) serveTLS(host, certName string, h http.Handler, h2 bool) *testServer {
	s := &testServer{n: n, host: strings.ToLower(host)}
	srv := httptest.NewUnstartedServer(s.wrap(h))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // handshake failures are expected in some tests
	srv.EnableHTTP2 = h2
	protos := []string{"http/1.1"}
	if h2 {
		protos = []string{"h2", "http/1.1"}
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{n.leaf(certName)}, NextProtos: protos}
	srv.StartTLS()
	s.addr = srv.Listener.Addr().String()
	n.register(host, "443", srv.Listener.Addr().String(), true, srv.Close)
	return s
}

// HandleHTTP serves h for host on port 80, plain http.
func (n *testNet) HandleHTTP(host string, h http.Handler) *testServer {
	s := &testServer{n: n, host: strings.ToLower(host)}
	srv := httptest.NewUnstartedServer(s.wrap(h))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	s.addr = srv.Listener.Addr().String()
	n.register(host, "80", srv.Listener.Addr().String(), true, srv.Close)
	return s
}

// StallTLS maps host:443 to a listener that accepts and never handshakes.
func (n *testNet) StallTLS(host string) {
	ln := n.listen()
	var mu sync.Mutex
	var conns []net.Conn
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	n.register(host, "443", ln.Addr().String(), true, func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
}

func (n *testNet) listen() net.Listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		n.tb.Fatalf("nettest: listen: %v", err)
	}
	return ln
}

// MapRaw maps host:port to addr with NO AllowAddr grant, so the dial meets
// the real guard (the SSRF rows: 127.0.0.1:other, [::1], 169.254.169.254…).
func (n *testNet) MapRaw(host, port, addr string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[net.JoinHostPort(strings.ToLower(host), port)] = addr
}

// MapName maps host:443 to name and the port of host's registered listener,
// so Go's own resolver and Happy Eyeballs run on name (e.g. "localhost":
// [::1] denied, 127.0.0.1:N allowed). It skips the test if name does not
// resolve to both address families.
func (n *testNet) MapName(host, name string) {
	n.tb.Helper()
	key := net.JoinHostPort(strings.ToLower(host), "443")
	n.mu.Lock()
	addr, ok := n.routes[key]
	n.mu.Unlock()
	if !ok {
		n.tb.Fatalf("nettest: MapName(%q): register a listener for it first", host)
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		n.tb.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", name)
	if err != nil {
		n.tb.Skipf("nettest: %q does not resolve: %v", name, err)
	}
	var v4, v6 bool
	for _, ip := range ips {
		v4 = v4 || ip.Unmap().Is4()
		v6 = v6 || (ip.Is6() && !ip.Is4In6())
	}
	if !v4 || !v6 {
		n.tb.Skipf("nettest: %q resolves to %v, not to both address families", name, ips)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.routes[key] = net.JoinHostPort(name, port)
}

// Refuse maps host:443 to a socket that is bound but not listening, with an
// AllowAddr grant, so the dial gets ECONNREFUSED (unix only).
//
// On Linux (CI) the socket stays bound until cleanup, so no other listener
// can take its port while the test runs. Elsewhere a SYN to such a socket
// may be dropped rather than reset (macOS), so the port is taken from a
// listener that is closed at once; another process could reuse it.
func (n *testNet) Refuse(host string) {
	n.tb.Helper()
	if runtime.GOOS != "linux" {
		ln := n.listen()
		addr := ln.Addr().String()
		ln.Close()
		n.register(host, "443", addr, true, nil)
		return
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		n.tb.Skipf("nettest: Refuse: socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		syscall.Close(fd)
		n.tb.Fatalf("nettest: Refuse: bind: %v", err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		syscall.Close(fd)
		n.tb.Fatalf("nettest: Refuse: getsockname: %v", err)
	}
	in4, ok := sa.(*syscall.SockaddrInet4)
	if !ok {
		syscall.Close(fd)
		n.tb.Fatalf("nettest: Refuse: getsockname returned %T", sa)
	}
	addr := netip.AddrPortFrom(netip.AddrFrom4(in4.Addr), uint16(in4.Port)).String()
	n.register(host, "443", addr, true, func() { syscall.Close(fd) })
}

// Proxy starts a loopback proxy listener serving h, mapped through Resolve
// under a .test name, and returns its URL for TransportOptions.Proxy.
//
// The URL is http://proxyN.test:3128. The guarded dialer exempts exactly
// that address from the guard and maps it through Resolve, so the proxy is
// reached with a plain dial and needs no AllowAddr grant.
func (n *testNet) Proxy(h http.Handler) *url.URL {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	n.mu.Lock()
	n.proxies++
	name := fmt.Sprintf("proxy%d.test", n.proxies)
	n.mu.Unlock()
	n.register(name, "3128", srv.Listener.Addr().String(), false, srv.Close)
	return &url.URL{Scheme: "http", Host: name + ":3128"}
}

// ConnectProxy returns a CONNECT-only proxy handler for Proxy. It dials a
// tunnel's target through the test network's map, and only to a listener
// the network registered: a target mapped elsewhere (MapRaw) or not at all
// gets 502, so the proxy never dials outside the test network.
func (n *testNet) ConnectProxy() *testProxy {
	return &testProxy{n: n}
}

// testProxy is ConnectProxy's handler.
type testProxy struct {
	n        *testNet
	mu       sync.Mutex
	requests []string // "METHOD target" per request
}

// Requests returns "METHOD target" for every request the proxy received.
func (p *testProxy) Requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}

func (p *testProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.requests = append(p.requests, r.Method+" "+r.Host)
	p.mu.Unlock()
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	n := p.n
	n.mu.Lock()
	addr, ok := n.routes[net.JoinHostPort(strings.ToLower(host), port)]
	if ok {
		ap, err := netip.ParseAddrPort(addr)
		ok = err == nil && n.allow[ap]
	}
	n.mu.Unlock()
	if !ok {
		http.Error(w, "target not on the test network", http.StatusBadGateway)
		return
	}
	up, err := net.Dial("tcp", addr)
	if err != nil {
		http.Error(w, "dial failed", http.StatusBadGateway)
		return
	}
	c, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.Close()
		return
	}
	n.wg.Add(1)
	defer n.wg.Done()
	if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		c.Close()
		up.Close()
		return
	}
	stop := context.AfterFunc(doneCtx(n.done), func() { c.Close(); up.Close() })
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(up, c); up.Close() }()
	go func() { defer wg.Done(); io.Copy(c, up); c.Close() }()
	wg.Wait()
}

// doneCtx is a context canceled when done closes.
func doneCtx(done <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-done; cancel() }()
	return ctx
}

// Unmapped returns the host:port dials Resolve could not map, in order.
// The ones it returns are not reported again in cleanup: a test that
// provokes an unmapped dial on purpose checks it here.
func (n *testNet) Unmapped() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unmappedSeen = len(n.unmapped)
	return slices.Clone(n.unmapped)
}

// Addr returns the listener address, for tests that dial it directly.
func (s *testServer) Addr() string { return s.addr }

// Hits returns the number of requests s received.
func (s *testServer) Hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// Requests returns the requests s received, in order.
func (s *testServer) Requests() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// wrap records each request and checks it against the production header
// set before h serves it.
func (s *testServer) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, recordedRequest{
			Method:     r.Method,
			Host:       r.Host,
			Path:       r.URL.Path,
			RawQuery:   r.URL.RawQuery,
			RequestURI: r.RequestURI,
			Proto:      r.Proto,
			Header:     r.Header.Clone(),
			BodyLen:    int(body),
		})
		s.mu.Unlock()
		s.check(r, body)
		h.ServeHTTP(w, r)
	})
}

// check records a violation of the production header set.
func (s *testServer) check(r *http.Request, body int64) {
	n := s.n
	n.mu.Lock()
	ua := n.ua
	n.mu.Unlock()
	where := s.host + " " + r.Proto
	if r.Method != http.MethodGet {
		n.violate("%s: method %s, want GET", where, r.Method)
	}
	// Go sends the URL host as written, so an explicit default port (t.co:443)
	// is the URL host too.
	port := ":80"
	if r.TLS != nil {
		port = ":443"
	}
	if r.Host != s.host && r.Host != s.host+port {
		n.violate("%s: Host %q, want the URL host %q", where, r.Host, s.host)
	}
	for name := range r.Header {
		if name != "User-Agent" && name != "Accept" {
			n.violate("%s: unexpected header %s", where, name)
		}
	}
	if got := r.Header.Values("User-Agent"); len(got) != 1 || got[0] != ua {
		n.violate("%s: User-Agent %q, want %q", where, got, ua)
	}
	if got := r.Header.Values("Accept"); len(got) != 1 || got[0] != "*/*" {
		n.violate("%s: Accept %q, want */*", where, got)
	}
	if body != 0 || r.ContentLength > 0 {
		n.violate("%s: request body of %d bytes", where, max(body, r.ContentLength))
	}
}
