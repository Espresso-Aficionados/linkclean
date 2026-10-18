package unshorten

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"syscall"
	"time"

	"code.dny.dev/ssrf"
)

// TransportOptions configures NewTransport.
type TransportOptions struct {
	DialTimeout           time.Duration // 0 means 2s
	TLSHandshakeTimeout   time.Duration // 0 means 2s
	ResponseHeaderTimeout time.Duration // 0 means 3s
	TLSConfig             *tls.Config   // nil means Go defaults; NewTransport uses a clone
	// Proxy routes every request through one fixed HTTP/HTTPS/SOCKS5 proxy. The dial
	// guard EXEMPTS exactly this proxy's host:port and guards every other dial. With
	// a proxy the target is dialed by the proxy (HTTPS via CONNECT), so egress policy
	// MUST be enforced by the proxy. The Fetcher's hostname allowlist and IP-literal
	// rejection still apply. HTTP(S)_PROXY environment variables are never read.
	Proxy *url.URL
}

// NewTransport returns a guarded *http.Transport:
// Proxy: fixed opts.Proxy or nil; DialContext: plain dialer for the proxy addr,
// else net.Dialer{Timeout, Control: GuardControl()}; TLSHandshakeTimeout;
// ResponseHeaderTimeout; MaxResponseHeaderBytes 32 KiB; ForceAttemptHTTP2;
// MaxIdleConnsPerHost 2; MaxConnsPerHost 4; IdleConnTimeout 30s;
// DisableCompression, so no Accept-Encoding header is added.
// Wrap it freely (otelhttp.NewTransport(t)) and set AllowUnguardedTransport,
// since New cannot see through wrappers. An invalid opts.Proxy makes New
// refuse the transport and fails each request made with it directly.
func NewTransport(opts TransportOptions) *http.Transport {
	panic("unshorten: not implemented")
}

// GuardTransport returns a clone of t with the dial guard installed (with the
// default 2s dial timeout), Proxy set to the fixed proxy (or nil, never
// ProxyFromEnvironment), and DisableCompression, and records it as guarded.
// It REFUSES (error wrapping ErrUnguardedTransport) a t whose Dial, DialTLS or
// DialTLSContext is set, or whose DialContext is set to anything other than a
// guard installed by this package (as on NewTransport(...).Clone()): it cannot
// install the guard "on top of" an arbitrary dial func without silently
// discarding it. To keep a custom dialer, wrap its net.Dialer with GuardDialer
// and set AllowUnguardedTransport. An invalid proxy URL is an error; no error
// contains the proxy URL.
func GuardTransport(t *http.Transport, proxy *url.URL) (*http.Transport, error) {
	panic("unshorten: not implemented")
}

// GuardControl returns a net.Dialer.Control (code.dny.dev/ssrf) that allows only
// public unicast IPs on tcp ports 80 and 443. It denies loopback, RFC 1918, CGNAT
// 100.64/10, link-local including 169.254.169.254, ULA fc00::/7, IPv4-mapped forms,
// NAT64 64:ff9b::/96, Teredo, 6to4, multicast and IANA special-purpose ranges,
// and 168.63.129.16 (Azure WireServer). It checks the IP actually dialed, per
// attempt, so DNS rebinding is covered.
func GuardControl() func(network, address string, c syscall.RawConn) error {
	panic("unshorten: not implemented")
}

// GuardDialer returns a copy of d whose Control runs GuardControl, then d's
// existing Control. If d has a ControlContext, which Go uses instead of
// Control, the guard is chained into ControlContext (guard first) and Control
// is left alone.
func GuardDialer(d *net.Dialer) *net.Dialer {
	panic("unshorten: not implemented")
}

// guard wraps the one ssrf.Guardian every guarded dial uses.
type guard struct{ g *ssrf.Guardian }

// theGuard is ssrf's defaults (tcp4/tcp6, ports 80 and 443, the IANA
// special-purpose ranges) plus 168.63.129.16/32, a public address ssrf
// allows.
var theGuard = &guard{g: ssrf.New(ssrf.WithDeniedV4Prefixes(netip.MustParsePrefix("168.63.129.16/32")))}

// control is GuardControl's func: theGuard's Safe, except that an address
// dialhook.AllowAddr reports true for is allowed (tests only).
func (g *guard) control(network, address string, c syscall.RawConn) error {
	panic("unshorten: not implemented")
}

// guardDialer is the DialContext of every guarded transport. Its type is
// unexported, so no value outside the package shares its method value's code
// pointer (guardDialPC).
type guardDialer struct {
	d         net.Dialer // Control: theGuard.control
	proxyAddr string     // canonicalProxyAddr of the fixed proxy, or ""
}

// DialContext dials addr through the guard; the proxy's exact address is
// dialed without it. With dialhook.Resolve set, addr (the proxy's too) is
// mapped through it first, and an unmapped one fails with
// dialhook.ErrUnmapped before any DNS lookup.
func (g *guardDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	panic("unshorten: not implemented")
}

// fixedProxy is the Proxy func of a guarded transport with a proxy. err is
// set for an invalid proxy URL; its text never contains the URL.
type fixedProxy struct {
	u   *url.URL
	err error
}

func (p *fixedProxy) Proxy(*http.Request) (*url.URL, error) {
	panic("unshorten: not implemented")
}

// attestation records the guard and proxy funcs installed on a transport.
type attestation struct {
	dial  *guardDialer
	proxy *fixedProxy // nil: no proxy
}

// attest records a for t, in a registry keyed weakly by t.
func attest(t *http.Transport, a *attestation) {
	panic("unshorten: not implemented")
}

// attested returns t's attestation.
func attested(t *http.Transport) (*attestation, bool) {
	panic("unshorten: not implemented")
}

// verifyAttested reports, as an error wrapping ErrUnguardedTransport, a field
// of t that no longer matches a: DialContext not the guard, Dial, DialTLS or
// DialTLSContext set, Proxy added, removed or not fixedProxy, or a.proxy.err.
func verifyAttested(t *http.Transport, a *attestation) error {
	panic("unshorten: not implemented")
}

// privateClone builds New's own transport from t and a's funcs (never t's),
// with Dial*, TLSNextProto reset, DisableCompression set and
// MaxResponseHeaderBytes clamped to (0, 32 KiB], and attests it.
func privateClone(t *http.Transport, a *attestation) *http.Transport {
	panic("unshorten: not implemented")
}

// canonicalProxyAddr returns the host:port net/http dials for proxy u: the
// host as written, the port or 80 for http, 443 for https, 1080 for socks5
// and socks5h.
func canonicalProxyAddr(u *url.URL) (string, error) {
	panic("unshorten: not implemented")
}

// guardDialPC and fixedProxyPC are the code pointers of the method values
// (&guardDialer{}).DialContext and (&fixedProxy{}).Proxy: their -fm
// wrappers, shared by every method value of each method. Method
// expressions have other pointers and must not be compared.
var (
	guardDialPC  = reflect.ValueOf((&guardDialer{}).DialContext).Pointer()
	fixedProxyPC = reflect.ValueOf((&fixedProxy{}).Proxy).Pointer()
)
