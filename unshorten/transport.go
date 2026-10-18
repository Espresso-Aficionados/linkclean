package unshorten

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"weak"

	"code.dny.dev/ssrf"

	"github.com/Espresso-Aficionados/linkclean/unshorten/internal/dialhook"
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

// NewTransport's settings.
const (
	defaultDialTimeout     = 2 * time.Second
	defaultTLSTimeout      = 2 * time.Second
	defaultHeaderTimeout   = 3 * time.Second
	maxResponseHeaderBytes = 32 << 10
)

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
	a := &attestation{dial: newGuardDialer(orDefault(opts.DialTimeout, defaultDialTimeout))}
	if opts.Proxy != nil {
		a.proxy = newFixedProxy(opts.Proxy)
		a.dial.proxyAddr, _ = canonicalProxyAddr(a.proxy.u)
	}
	t := &http.Transport{
		DialContext:            a.dial.DialContext,
		TLSClientConfig:        opts.TLSConfig.Clone(), // the h2 setup edits NextProtos in place
		TLSHandshakeTimeout:    orDefault(opts.TLSHandshakeTimeout, defaultTLSTimeout),
		ResponseHeaderTimeout:  orDefault(opts.ResponseHeaderTimeout, defaultHeaderTimeout),
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		ForceAttemptHTTP2:      true,
		MaxIdleConnsPerHost:    2,
		MaxConnsPerHost:        4,
		IdleConnTimeout:        30 * time.Second,
		DisableCompression:     true,
	}
	if a.proxy != nil {
		t.Proxy = a.proxy.Proxy
	}
	attest(t, a)
	return t
}

// orDefault returns d for a zero or negative v. A negative timeout is not
// passed through: net/http reads a negative ResponseHeaderTimeout as none.
func orDefault(v, d time.Duration) time.Duration {
	if v <= 0 {
		return d
	}
	return v
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
	switch {
	case t == nil:
		return nil, unguarded("GuardTransport: nil transport")
	case t.Dial != nil:
		return nil, unguarded("GuardTransport: Dial is set")
	case t.DialTLS != nil:
		return nil, unguarded("GuardTransport: DialTLS is set")
	case t.DialTLSContext != nil:
		return nil, unguarded("GuardTransport: DialTLSContext is set")
	case t.DialContext != nil && funcPC(t.DialContext) != guardDialPC:
		return nil, unguarded("GuardTransport: DialContext is set to a dial func this package did not install")
	}
	a := &attestation{dial: newGuardDialer(defaultDialTimeout)}
	if proxy != nil {
		a.proxy = newFixedProxy(proxy)
		if a.proxy.err != nil {
			return nil, a.proxy.err
		}
		a.dial.proxyAddr, _ = canonicalProxyAddr(a.proxy.u)
	}
	c := t.Clone()
	c.DialContext = a.dial.DialContext
	c.Proxy = nil
	if a.proxy != nil {
		c.Proxy = a.proxy.Proxy
	}
	c.DisableCompression = true
	attest(c, a)
	return c, nil
}

// GuardControl returns a net.Dialer.Control (code.dny.dev/ssrf) that allows only
// public unicast IPs on tcp ports 80 and 443. It denies loopback, RFC 1918, CGNAT
// 100.64/10, link-local including 169.254.169.254, ULA fc00::/7, IPv4-mapped forms,
// NAT64 64:ff9b::/96, Teredo, 6to4, multicast and IANA special-purpose ranges,
// and 168.63.129.16 (Azure WireServer). It checks the IP actually dialed, per
// attempt, so DNS rebinding is covered.
func GuardControl() func(network, address string, c syscall.RawConn) error {
	return theGuard.control
}

// GuardDialer returns a copy of d whose Control runs GuardControl, then d's
// existing Control. If d has a ControlContext, which Go uses instead of
// Control, the guard is chained into ControlContext (guard first) and Control
// is left alone.
func GuardDialer(d *net.Dialer) *net.Dialer {
	var c net.Dialer
	if d != nil {
		c = *d
	}
	if next := c.ControlContext; next != nil {
		c.ControlContext = func(ctx context.Context, network, address string, rc syscall.RawConn) error {
			if err := theGuard.control(network, address, rc); err != nil {
				return err
			}
			return next(ctx, network, address, rc)
		}
		return &c
	}
	next := c.Control
	c.Control = func(network, address string, rc syscall.RawConn) error {
		if err := theGuard.control(network, address, rc); err != nil {
			return err
		}
		if next != nil {
			return next(network, address, rc)
		}
		return nil
	}
	return &c
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
	if allow := dialhook.AllowAddr(); allow != nil && (network == "tcp4" || network == "tcp6") {
		// The exemption skips the IP and the port check (test listeners
		// have random ports), never the network check.
		if ap, err := netip.ParseAddrPort(address); err == nil && allow(ap) {
			return nil
		}
	}
	return g.g.Safe(network, address, c)
}

// guardDialer is the DialContext of every guarded transport. Its type is
// unexported, so no value outside the package shares its method value's code
// pointer (guardDialPC). It is never modified after its transport is built.
type guardDialer struct {
	d         net.Dialer // Control: theGuard.control
	proxyAddr string     // canonicalProxyAddr of the fixed proxy, or ""
}

func newGuardDialer(timeout time.Duration) *guardDialer {
	return &guardDialer{d: net.Dialer{Timeout: timeout, Control: theGuard.control}}
}

// DialContext dials addr through the guard; the proxy's exact address is
// dialed without it. With dialhook.Resolve set, addr (the proxy's too) is
// mapped through it first, and an unmapped one fails with
// dialhook.ErrUnmapped before any DNS lookup.
func (g *guardDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	proxy := g.proxyAddr != "" && addr == g.proxyAddr
	if resolve := dialhook.Resolve(); resolve != nil {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		mapped, ok := resolve(host, port)
		if !ok {
			return nil, dialhook.ErrUnmapped
		}
		addr = mapped
	}
	if proxy {
		d := net.Dialer{Timeout: g.d.Timeout}
		return d.DialContext(ctx, network, addr)
	}
	return g.d.DialContext(ctx, network, addr)
}

// fixedProxy is the Proxy func of a guarded transport with a proxy. err is
// set for an invalid proxy URL; its text never contains the URL.
type fixedProxy struct {
	u   *url.URL // nil with err
	err error
}

// newFixedProxy copies u, so a later change to the caller's URL cannot
// move the proxy away from the address the guard exempts.
func newFixedProxy(u *url.URL) *fixedProxy {
	if _, err := canonicalProxyAddr(u); err != nil {
		return &fixedProxy{err: err}
	}
	c := *u
	return &fixedProxy{u: &c}
}

func (p *fixedProxy) Proxy(*http.Request) (*url.URL, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.u, nil
}

// proxyDefaultPorts are the ports net/http dials for a proxy URL without one.
var proxyDefaultPorts = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}

// canonicalProxyAddr returns the host:port net/http dials for proxy u: the
// host as written, the port or 80 for http, 443 for https, 1080 for socks5
// and socks5h.
//
// The error names the problem and at most the scheme, never u: userinfo
// can hold credentials. A non-ASCII host is refused rather than converted,
// so the exempted address is always byte for byte what net/http dials.
func canonicalProxyAddr(u *url.URL) (string, error) {
	const bad = "unshorten: invalid proxy URL: "
	if u == nil {
		return "", errors.New(bad + "nil")
	}
	port, ok := proxyDefaultPorts[u.Scheme]
	switch {
	case !ok:
		return "", errors.New(bad + "scheme " + strconv.Quote(u.Scheme) + " is not http, https, socks5 or socks5h")
	case u.Opaque != "":
		return "", errors.New(bad + "opaque URL")
	case u.Path != "" && u.Path != "/":
		return "", errors.New(bad + "it has a path")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", errors.New(bad + "it has a query or fragment")
	}
	host := u.Hostname()
	if host == "" {
		return "", errors.New(bad + "no host")
	}
	for i := 0; i < len(host); i++ {
		if c := host[i]; c <= ' ' || c >= 0x7f {
			return "", errors.New(bad + "the host is not printable ASCII")
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return "", errors.New(bad + "bad port")
		}
		port = p
	}
	return net.JoinHostPort(host, port), nil
}

// attestation records the guard and proxy funcs installed on a transport.
type attestation struct {
	dial  *guardDialer
	proxy *fixedProxy // nil: no proxy
}

// attestations maps a weak pointer to every transport NewTransport,
// GuardTransport or privateClone built to its attestation. A strong key
// would keep every transport alive; a uintptr key could attest a new
// transport allocated at a collected one's address. Each entry is removed
// by a cleanup once its transport is collected.
var attestations sync.Map // weak.Pointer[http.Transport] → *attestation

// attest records a for t, in a registry keyed weakly by t.
func attest(t *http.Transport, a *attestation) {
	wp := weak.Make(t)
	attestations.Store(wp, a)
	runtime.AddCleanup(t, func(wp weak.Pointer[http.Transport]) { attestations.Delete(wp) }, wp)
}

// attested returns t's attestation.
func attested(t *http.Transport) (*attestation, bool) {
	if t == nil {
		return nil, false
	}
	v, ok := attestations.Load(weak.Make(t))
	if !ok {
		return nil, false
	}
	return v.(*attestation), true
}

// verifyAttested reports, as an error wrapping ErrUnguardedTransport, a field
// of t that no longer matches a: DialContext not the guard, Dial, DialTLS or
// DialTLSContext set, Proxy added, removed or not fixedProxy, or a.proxy.err.
func verifyAttested(t *http.Transport, a *attestation) error {
	switch {
	case t.DialContext == nil || funcPC(t.DialContext) != guardDialPC:
		return unguarded("DialContext was changed from the guard")
	case t.Dial != nil:
		return unguarded("Dial is set")
	case t.DialTLS != nil:
		return unguarded("DialTLS is set")
	case t.DialTLSContext != nil:
		return unguarded("DialTLSContext is set")
	case a.proxy == nil && t.Proxy != nil:
		return unguarded("Proxy was set" + proxyFuncName(t.Proxy))
	case a.proxy != nil && t.Proxy == nil:
		return unguarded("Proxy was removed")
	case t.Proxy != nil && funcPC(t.Proxy) != fixedProxyPC:
		return unguarded("Proxy was changed from the fixed proxy" + proxyFuncName(t.Proxy))
	case a.proxy != nil && a.proxy.err != nil:
		return unguarded("Proxy: " + a.proxy.err.Error())
	}
	return nil
}

// privateClone builds New's own transport from t and a's funcs (never t's),
// with Dial*, TLSNextProto reset, DisableCompression set and
// MaxResponseHeaderBytes clamped to (0, 32 KiB], and attests it.
//
// TLSNextProto nil restores net/http's own HTTP/2 setup, so an entry
// returning another RoundTripper is dropped; Clone does not copy
// RegisterProtocol entries. The other settings (timeouts, connection
// limits, TLSClientConfig) are the caller's: Timeout and the request slots
// bound them.
func privateClone(t *http.Transport, a *attestation) *http.Transport {
	c := t.Clone()
	c.DialContext = a.dial.DialContext
	c.Dial, c.DialTLS, c.DialTLSContext = nil, nil, nil
	c.Proxy = nil
	if a.proxy != nil {
		c.Proxy = a.proxy.Proxy
	}
	c.TLSNextProto = nil
	c.DisableCompression = true
	if c.MaxResponseHeaderBytes <= 0 || c.MaxResponseHeaderBytes > maxResponseHeaderBytes {
		c.MaxResponseHeaderBytes = maxResponseHeaderBytes
	}
	attest(c, a)
	return c
}

// checkTransport is New's transport decision. It returns the RoundTripper
// the Resolver uses:
//
//   - nil: a fresh NewTransport(TransportOptions{});
//   - an attested *http.Transport that verifies: its privateClone;
//   - an attested *http.Transport that was changed, or any other
//     *http.Transport: an error wrapping ErrUnguardedTransport, or with
//     allow, rt itself after one warning on log;
//   - any other RoundTripper: the same error, or with allow, rt itself
//     without a warning (New cannot tell whether it wraps a guarded
//     transport).
//
// No error or log line contains a proxy URL.
func checkTransport(rt http.RoundTripper, allow bool, log *slog.Logger) (http.RoundTripper, error) {
	if rt == nil {
		return NewTransport(TransportOptions{}), nil
	}
	t, ok := rt.(*http.Transport)
	if !ok {
		if allow {
			return rt, nil
		}
		return nil, unguarded("Config.Transport is not an *http.Transport; set AllowUnguardedTransport if it wraps a guarded one")
	}
	var err error
	if a, ok := attested(t); ok {
		if err = verifyAttested(t, a); err == nil {
			return privateClone(t, a), nil
		}
	} else {
		err = unguarded("the *http.Transport was not built by NewTransport or GuardTransport" + proxyFuncName(t.Proxy))
	}
	if !allow {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	log.Warn("unshorten: using a transport that is not verifiably SSRF-guarded (Config.AllowUnguardedTransport)",
		slog.String("reason", err.Error()))
	return rt, nil
}

func unguarded(reason string) error {
	return &unguardedError{reason: reason}
}

// unguardedError is ErrUnguardedTransport with a reason.
type unguardedError struct{ reason string }

func (e *unguardedError) Error() string { return ErrUnguardedTransport.Error() + ": " + e.reason }
func (e *unguardedError) Unwrap() error { return ErrUnguardedTransport }

// proxyFuncName names a Proxy func New can recognise: http.ProxyFromEnvironment,
// which http.DefaultTransport uses.
func proxyFuncName(f func(*http.Request) (*url.URL, error)) string {
	if f != nil && funcPC(f) == proxyFromEnvironmentPC {
		return " (its Proxy is http.ProxyFromEnvironment)"
	}
	return ""
}

// funcPC returns the code pointer of a func value.
func funcPC(f any) uintptr { return reflect.ValueOf(f).Pointer() }

// guardDialPC and fixedProxyPC are the code pointers of the method values
// (&guardDialer{}).DialContext and (&fixedProxy{}).Proxy: their -fm
// wrappers, shared by every method value of each method. Method
// expressions have other pointers and must not be compared.
//
// Closures are never attested: their code pointers vary with inlining.
// The ci.yml step that runs the attestation tests with -gcflags=all=-l
// checks the pointers without inlining; it does not cover PGO builds, but
// an -fm wrapper is one dupok symbol there too.
var (
	guardDialPC  = reflect.ValueOf((&guardDialer{}).DialContext).Pointer()
	fixedProxyPC = reflect.ValueOf((&fixedProxy{}).Proxy).Pointer()

	proxyFromEnvironmentPC = reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
)
