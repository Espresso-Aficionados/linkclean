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
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"
)

// testNet is one test's network. Only one may be installed at a time: a
// second concurrent installNet fails the test (no t.Parallel overlap).
type testNet struct{}

// testServer is one registered listener.
type testServer struct{}

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
	panic("nettest: not implemented")
}

// ClientTLS returns the client TLS config trusting the test CA, for
// TransportOptions.TLSConfig.
func (n *testNet) ClientTLS() *tls.Config {
	panic("nettest: not implemented")
}

// HandleTLS serves h for host on port 443 (HTTP/2 and HTTP/1.1). Every
// request is checked against the production header set: GET, User-Agent
// defaultUA(), Accept */*, no Cookie, Referer, Authorization,
// Accept-Encoding or Accept-Language, an empty body, and a Host equal to the
// URL host.
func (n *testNet) HandleTLS(host string, h http.Handler) *testServer {
	panic("nettest: not implemented")
}

// HandleTLSH1 is HandleTLS with HTTP/1.1 only.
func (n *testNet) HandleTLSH1(host string, h http.Handler) *testServer {
	panic("nettest: not implemented")
}

// HandleHTTP serves h for host on port 80, plain http.
func (n *testNet) HandleHTTP(host string, h http.Handler) *testServer {
	panic("nettest: not implemented")
}

// StallTLS maps host:443 to a listener that accepts and never handshakes.
func (n *testNet) StallTLS(host string) {
	panic("nettest: not implemented")
}

// MapRaw maps host:port to addr with NO AllowAddr grant, so the dial meets
// the real guard (the SSRF rows: 127.0.0.1:other, [::1], 169.254.169.254…).
func (n *testNet) MapRaw(host, port, addr string) {
	panic("nettest: not implemented")
}

// MapName maps host:443 to name and the port of host's registered listener,
// so Go's own resolver and Happy Eyeballs run on name (e.g. "localhost":
// [::1] denied, 127.0.0.1:N allowed). It skips the test if name does not
// resolve to both address families.
func (n *testNet) MapName(host, name string) {
	panic("nettest: not implemented")
}

// Refuse maps host:443 to a socket that is bound but not listening, with an
// AllowAddr grant, so the dial gets ECONNREFUSED (unix only).
func (n *testNet) Refuse(host string) {
	panic("nettest: not implemented")
}

// WrongCert serves h for host on 443 with a leaf certificate for another
// name, so verification fails.
func (n *testNet) WrongCert(host string, h http.Handler) *testServer {
	panic("nettest: not implemented")
}

// Proxy starts a loopback proxy listener serving h, mapped through Resolve
// under a .test name, and returns its URL for TransportOptions.Proxy.
func (n *testNet) Proxy(h http.Handler) *url.URL {
	panic("nettest: not implemented")
}

// Unmapped returns the host:port dials Resolve could not map, in order.
func (n *testNet) Unmapped() []string {
	panic("nettest: not implemented")
}

// Hits returns the number of requests s received.
func (s *testServer) Hits() int {
	panic("nettest: not implemented")
}

// Requests returns the requests s received, in order.
func (s *testServer) Requests() []recordedRequest {
	panic("nettest: not implemented")
}
