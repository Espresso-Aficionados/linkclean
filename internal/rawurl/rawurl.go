package rawurl

import (
	"net/netip"
	"strings"
)

// Skip says why Parse refused an input (§4.1). The engine maps it to a
// SkipReason.
type Skip uint8

const (
	OK          Skip = iota
	Scheme           // scheme is not http or https (ASCII case-insensitive)
	Unparseable      // no "://", invalid scheme characters, or empty host
	Ambiguous        // a byte <= 0x20 or 0x7F; '\' in the authority; '%' in the host; malformed brackets
)

func (s Skip) String() string {
	switch s {
	case OK:
		return "ok"
	case Scheme:
		return "scheme"
	case Unparseable:
		return "unparseable"
	case Ambiguous:
		return "ambiguous"
	}
	return "Skip(?)"
}

// URL is a span view of a raw URL. Every string field except Scheme and Host
// is a substring of Raw. Scheme is the constant "http" or "https". A URL is
// meaningful only as returned by Parse or ParseTarget.
type URL struct {
	Raw         string
	Scheme      string // "http" or "https"
	Prefix      string // Raw up to the end of the authority: scheme "://" authority, as written
	Authority   string // as written, userinfo and port included
	Port        string // as written, without ':'; "" if absent or empty
	Path        string // raw path; may be ""
	Query       string // without '?'; valid iff HasQuery
	HasQuery    bool   // a '?' occurs before the first '#'
	Fragment    string // without '#'; valid iff HasFragment
	HasFragment bool
	Host        string // host view (see HostView); an IPv6 literal keeps its brackets
	IsIP        bool   // dotted IPv4, bracketed IPv6, or a WHATWG "ends in a number" host
}

// Parse splits raw strictly (§4.1). The query starts at the first '?' before
// the first '#'. Userinfo is cut at the last '@' of the authority and the port
// is split bracket-aware. Parse does not check the length (the engine applies
// MaxURLLength first) or the port's syntax. It does not allocate for an ASCII
// host that is already lowercase and has no "xn--" label.
func Parse(raw string) (URL, Skip) {
	return split(raw, false)
}

// ParseTarget parses an extracted unwrap destination t and applies every §6
// VALID check that depends on t alone: scheme http or https; an authority
// without '@', '\', '%' or bytes <= 0x20 or 0x7F; a non-empty host that passes
// idna.Lookup.ToASCII, with an ASCII form that passes it unchanged, and is
// neither an IP literal nor single-label; a non-empty Site; a port that is
// empty or numeric in 1–65535. Bytes <= 0x20 or 0x7F outside the authority
// are allowed (Reserialize encodes them). The same-site and length checks are
// the engine's.
func ParseTarget(t string) (URL, bool) {
	u, skip := split(t, true)
	if skip != OK || u.IsIP || strings.IndexByte(u.Authority, '@') >= 0 {
		return URL{}, false
	}
	if strings.IndexByte(u.Host, '.') < 0 || Site(u.Host) == "" || !validPort(u.Port) {
		return URL{}, false
	}
	return u, true
}

// split implements Parse and ParseTarget. With target set, bytes <= 0x20 or
// 0x7F are refused only inside the authority and the host must pass IDNA.
func split(raw string, target bool) (URL, Skip) {
	i := strings.Index(raw, "://")
	if i <= 0 || !validScheme(raw[:i]) {
		return URL{}, Unparseable
	}
	var u URL
	switch scheme := raw[:i]; {
	case strings.EqualFold(scheme, "https"):
		u.Scheme = "https"
	case strings.EqualFold(scheme, "http"):
		u.Scheme = "http"
	default:
		return URL{}, Scheme
	}
	if !target && hasCTL(raw) {
		return URL{}, Ambiguous
	}
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	auth := rest[:end]
	if strings.IndexByte(auth, '\\') >= 0 || (target && hasCTL(auth)) {
		return URL{}, Ambiguous
	}
	hp := auth[strings.LastIndexByte(auth, '@')+1:]
	if strings.IndexByte(hp, '%') >= 0 {
		return URL{}, Ambiguous
	}
	host, port, ok := splitHostPort(hp)
	if !ok {
		return URL{}, Ambiguous
	}
	if host == "" {
		return URL{}, Unparseable
	}
	h, isIP, idnaOK := canonHost(host, target)
	if h == "" {
		return URL{}, Unparseable
	}
	if target && !idnaOK {
		return URL{}, Unparseable
	}

	u.Raw = raw
	u.Prefix = raw[:i+3+end]
	u.Authority = auth
	u.Port = port
	u.Host = h
	u.IsIP = isIP
	tail := rest[end:]
	if j := strings.IndexByte(tail, '#'); j >= 0 {
		u.HasFragment = true
		u.Fragment = tail[j+1:]
		tail = tail[:j]
	}
	if j := strings.IndexByte(tail, '?'); j >= 0 {
		u.HasQuery = true
		u.Query = tail[j+1:]
		tail = tail[:j]
	}
	u.Path = tail
	return u, OK
}

// splitHostPort splits the authority after its userinfo into host and port.
// ok is false for malformed brackets: any '[' or ']' other than one leading
// "[…]" holding an IPv6 address, optionally followed by ":port".
func splitHostPort(hp string) (host, port string, ok bool) {
	if hp == "" || hp[0] != '[' {
		if strings.ContainsAny(hp, "[]") {
			return "", "", false
		}
		if k := strings.LastIndexByte(hp, ':'); k >= 0 {
			return hp[:k], hp[k+1:], true
		}
		return hp, "", true
	}
	k := strings.IndexByte(hp, ']')
	if k < 0 || strings.IndexByte(hp[1:k], '[') >= 0 {
		return "", "", false
	}
	tail := hp[k+1:]
	if tail != "" {
		if tail[0] != ':' || strings.ContainsAny(tail, "[]") {
			return "", "", false
		}
		port = tail[1:]
	}
	if a, err := netip.ParseAddr(hp[1:k]); err != nil || !a.Is6() {
		return "", "", false
	}
	return hp[:k+1], port, true
}

// validScheme reports whether s is an RFC 3986 scheme: ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ).
func validScheme(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z':
		case i > 0 && ('0' <= c && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return s != ""
}

// hasCTL reports whether s has a byte <= 0x20 or 0x7F.
func hasCTL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// validPort reports whether p is empty or a decimal number in 1–65535.
func validPort(p string) bool {
	if p == "" {
		return true
	}
	n := 0
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return false
		}
	}
	return n >= 1
}

func (u *URL) defaultPort() string {
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// EffectivePort returns Port, or "443" or "80" by scheme when Port is "".
func (u *URL) EffectivePort() string {
	if u.Port != "" {
		return u.Port
	}
	return u.defaultPort()
}

// PathForRequest returns path' + ("?" + Query if HasQuery), where path' is
// Path, or "/" if Path is empty (§4.1). It does not allocate when Path is
// non-empty: the result is then a substring of Raw.
func (u *URL) PathForRequest() string {
	if u.Path == "" {
		if !u.HasQuery {
			return "/"
		}
		return "/?" + u.Query
	}
	start := len(u.Prefix)
	end := start + len(u.Path)
	if u.HasQuery {
		end += 1 + len(u.Query)
	}
	return u.Raw[start:end]
}

// MatchString returns lower(scheme) "://" Host [":" Port, if written and not
// the scheme default] path' ["?" Query] ["#" Fragment], the view URLRegexps
// search (§4.1). Userinfo is never present.
func (u *URL) MatchString() string {
	var b strings.Builder
	b.Grow(len(u.Scheme) + 3 + len(u.Host) + 1 + len(u.Port) + 1 + len(u.Path) + 1 + len(u.Query) + 1 + len(u.Fragment))
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	if u.Port != "" && u.Port != u.defaultPort() {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	if u.Path == "" {
		b.WriteByte('/')
	} else {
		b.WriteString(u.Path)
	}
	if u.HasQuery {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	if u.HasFragment {
		b.WriteByte('#')
		b.WriteString(u.Fragment)
	}
	return b.String()
}
