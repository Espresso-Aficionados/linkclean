package matchpattern

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"golang.org/x/net/idna"
)

// HostKind is the shape of a pattern's host part.
type HostKind uint8

const (
	HostAny    HostKind = iota + 1 // "*": any host, IPs included
	HostExact                      // "d": exactly d
	HostSuffix                     // "*.d": d and every subdomain of d; an IP only by equality
	HostLabel                      // "label.*" or "*.label.*": Site == label + "." + ICANN suffix (linkclean extension)
)

// Pattern is a compiled match pattern. Immutable; safe for concurrent use.
type Pattern struct {
	raw    string
	scheme string // "*", "http" or "https"
	kind   HostKind
	host   string // canonical host; "" for HostAny; the label for HostLabel
	port   int    // -1: any port

	path   glob // raw path glob
	normal glob // NORMAL-unescaped path glob; aliases path when equal
	// base is the NORMAL path minus a trailing "/*", so "P/*" also matches
	// "P"; valid iff slashStar.
	base      string
	slashStar bool
	escaped   bool // the NORMAL form differs from the raw form
	anyPath   bool // the path glob is "/*", which every pathForRequest matches
}

// Parse compiles one pattern of the §4.2 subset:
//
//	scheme "://" host [":" port] path
//
// The scheme is "*" (http or https), "http" or "https", case-sensitive. The
// host is "*", "*.d" or "d", canonicalized as URL hosts are (§4.1): ASCII
// lowercased, trailing dots trimmed and, if it has a non-ASCII byte, mapped by
// idna.Lookup.ToASCII to punycode. With allowLabel it may also be "label.*" or
// "*.label.*", where label is a single DNS label. A bracketed IPv6 literal is
// allowed as an exact host. The port is decimal 0-65535 without leading zeros.
// The path starts with '/' and may contain '*' wildcards anywhere.
//
// Everything else is an error: "<all_urls>", other schemes, '*' anywhere else
// in the host, url_pattern_index syntax ("||", "^"), a missing path, a host
// that IDNA rejects, and any non-hostname byte in the canonical host.
func Parse(p string, allowLabel bool) (*Pattern, error) {
	m, err := parse(p, allowLabel)
	if err != nil {
		return nil, fmt.Errorf("match pattern %q: %w", p, err)
	}
	return m, nil
}

func parse(p string, allowLabel bool) (*Pattern, error) {
	if p == "<all_urls>" {
		return nil, errors.New("<all_urls> is not supported")
	}
	scheme, rest, ok := strings.Cut(p, "://")
	if !ok {
		return nil, errors.New(`missing "://"`)
	}
	switch scheme {
	case "*", "http", "https":
	default:
		return nil, fmt.Errorf("scheme %q is not *, http or https", scheme)
	}
	slash := strings.IndexByte(rest, '/')
	switch {
	case slash == 0:
		return nil, errors.New("empty host")
	case slash < 0:
		return nil, errors.New("missing path")
	}
	m := &Pattern{raw: p, scheme: scheme, port: -1}
	host, err := m.parsePort(rest[:slash])
	if err != nil {
		return nil, err
	}
	if err := m.parseHost(host, allowLabel); err != nil {
		return nil, err
	}
	m.setPath(rest[slash:])
	return m, nil
}

// parsePort splits hostport, records the port and returns the host.
func (m *Pattern) parsePort(hostport string) (string, error) {
	host, port := hostport, ""
	hasPort := false
	if strings.HasPrefix(hostport, "[") {
		end := strings.IndexByte(hostport, ']')
		if end < 0 {
			return "", errors.New("unterminated IPv6 literal")
		}
		host = hostport[:end+1]
		if tail := hostport[end+1:]; tail != "" {
			if tail[0] != ':' {
				return "", errors.New("garbage after IPv6 literal")
			}
			port, hasPort = tail[1:], true
		}
	} else if i := strings.IndexByte(hostport, ':'); i >= 0 {
		host, port, hasPort = hostport[:i], hostport[i+1:], true
	}
	if hasPort {
		n, ok := parsePortNumber(port)
		if !ok || (len(port) > 1 && port[0] == '0') {
			return "", fmt.Errorf("invalid port %q", port)
		}
		m.port = n
	}
	if host == "" {
		return "", errors.New("empty host")
	}
	return host, nil
}

func (m *Pattern) parseHost(host string, allowLabel bool) error {
	if host == "*" {
		m.kind = HostAny
		return nil
	}
	if strings.HasPrefix(host, "[") {
		a, err := netip.ParseAddr(host[1 : len(host)-1])
		if err != nil || !a.Is6() || a.Zone() != "" {
			return fmt.Errorf("invalid IPv6 literal %q", host)
		}
		m.kind, m.host = HostExact, asciiLower(host)
		return nil
	}
	m.kind = HostExact
	if d, ok := strings.CutPrefix(host, "*."); ok {
		m.kind, host = HostSuffix, d
	}
	if l, ok := strings.CutSuffix(host, ".*"); ok {
		if !allowLabel {
			return errors.New(`host extension "label.*" is not allowed here`)
		}
		if strings.IndexByte(l, '.') >= 0 {
			return fmt.Errorf("label.* host %q: label must be a single DNS label", host)
		}
		m.kind, host = HostLabel, l
	}
	host = strings.TrimRight(asciiLower(host), ".")
	if !isASCII(host) {
		a, err := idna.Lookup.ToASCII(host)
		if err != nil {
			return fmt.Errorf("host %q: %v", host, err)
		}
		host = strings.TrimRight(a, ".")
	}
	if host == "" {
		return errors.New("empty host")
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		switch {
		case c == '*':
			return errors.New(`'*' in host is only allowed as "*" or a leading "*."`)
		case c == '.':
			if i == 0 || host[i-1] == '.' {
				return errors.New("empty host label")
			}
		case 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return fmt.Errorf("invalid host byte %q", c)
		}
	}
	m.host = host
	return nil
}

func (m *Pattern) setPath(path string) {
	m.path = newGlob(path)
	m.normal = m.path
	n := UnescapeNormal(path)
	if n != path {
		m.escaped = true
		m.normal = newGlob(n)
	}
	m.base, m.slashStar = strings.CutSuffix(n, "/*")
	m.anyPath = path == "/*"
}

// String returns the pattern's source text.
func (m *Pattern) String() string { return m.raw }

// Host returns the host kind and the canonical host: "" for HostAny, the
// label for HostLabel.
func (m *Pattern) Host() (HostKind, string) { return m.kind, m.host }

// MatchesAll reports whether m matches every http(s) Target: "*://*/*".
func (m *Pattern) MatchesAll() bool {
	return m.scheme == "*" && m.kind == HostAny && m.port < 0 && m.anyPath
}

// Target is the per-call view a pattern is matched against. The engine fills
// the exported fields once per call from the URL's views and calls SetPath
// once per round. A Target caches state across Match calls, so it must not be
// shared between goroutines.
type Target struct {
	Scheme string // "http" or "https"
	Host   string // host view: lowercased, trailing dots trimmed, punycode
	IsIP   bool
	Port   string // effective port, decimal
	Site   string // ICANN eTLD+1 of Host; required before matching a HostLabel pattern ("" = no site)

	path, normal string
	normalDone   bool // normal and escaped are valid
	escaped      bool // normal != path
}

// SetPath sets the pathForRequest the path globs are matched against. Its
// UnescapeNormal form is computed on the first Match that needs it, which
// allocates only when the path has an escape that NORMAL decodes.
func (t *Target) SetPath(pathForRequest string) {
	t.path = pathForRequest
	t.normal = ""
	t.normalDone = false
	t.escaped = false
}

func (t *Target) ensureNormal() {
	if !t.normalDone {
		t.normal = UnescapeNormal(t.path)
		t.escaped = t.normal != t.path
		t.normalDone = true
	}
}

// Match reports whether t matches m (§4.2): the scheme; the host per its
// kind; the port against the effective port; and the path glob against the
// raw pathForRequest or its NORMAL form (pattern and path both unescaped, as
// Chromium does), where a pattern "P/*" also matches the path "P". It
// allocates only to compute the path's NORMAL form, once per SetPath, and
// never for a "/*" path glob.
func (m *Pattern) Match(t *Target) bool {
	if t.Scheme != "http" && t.Scheme != "https" {
		return false
	}
	if m.scheme != "*" && m.scheme != t.Scheme {
		return false
	}
	switch m.kind {
	case HostExact:
		if t.Host != m.host {
			return false
		}
	case HostSuffix:
		if t.Host != m.host && (t.IsIP || !isSubdomain(t.Host, m.host)) {
			return false
		}
	case HostLabel:
		if t.IsIP || !isLabelSite(t.Site, m.host) {
			return false
		}
	}
	if m.port >= 0 {
		if n, ok := parsePortNumber(t.Port); !ok || n != m.port {
			return false
		}
	}
	if m.anyPath && strings.HasPrefix(t.path, "/") {
		return true
	}
	t.ensureNormal()
	if m.slashStar && t.normal == m.base {
		return true
	}
	if m.normal.match(t.normal) {
		return true
	}
	if !m.escaped && !t.escaped {
		return false // the raw match would repeat the one above
	}
	return m.path.match(t.path)
}

// isSubdomain reports whether h is a proper subdomain of d.
func isSubdomain(h, d string) bool {
	return len(h) > len(d)+1 && h[len(h)-len(d)-1] == '.' && h[len(h)-len(d):] == d
}

// isLabelSite reports whether site is label "." suffix. Site is an eTLD+1, so
// its first label is the only one to compare.
func isLabelSite(site, label string) bool {
	return len(site) > len(label)+1 && site[len(label)] == '.' && site[:len(label)] == label
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiLower lowercases ASCII letters only. strings.ToLower would fold some
// non-ASCII runes into ASCII (U+212A KELVIN SIGN becomes 'k') differently
// from IDNA mapping, which canonicalizes the rest.
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// parsePortNumber parses a decimal port in 0-65535 without allocating.
// Leading zeros are accepted, as GURL accepts them in URLs.
func parsePortNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return 0, false
		}
	}
	return n, true
}
