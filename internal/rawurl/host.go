package rawurl

import (
	"strings"

	"golang.org/x/net/idna"
)

// HostView canonicalizes a raw host (no userinfo, no port; an IPv6 literal
// with its brackets): ASCII-lowercase, trim trailing dots,
// idna.Lookup.ToASCII (on failure, keep the lowercased trimmed host), trim
// trailing dots again, then flag IP literals on the ASCII form (§4.1).
//
// A host made only of [a-z0-9._-] with no "xn--" label skips IDNA: on such
// hosts ToASCII returns its input or fails, and failure keeps the input, so
// the shortcut is exact. HostView does not allocate for such a host when it
// is already lowercase.
//
// The second trim covers IDNA mapping a trailing U+3002 or U+FF0E to '.':
// without it "web.archive.org。" would not equal "web.archive.org." for
// matching, and "1.2.3.4。" would not be flagged as an IP.
func HostView(rawHost string) (host string, isIP bool) {
	host, isIP, _ = canonHost(rawHost, false)
	return host, isIP
}

// canonHost implements HostView. With strict set it always runs IDNA, and
// idnaOK reports whether ToASCII succeeded (ParseTarget's VALID check);
// otherwise idnaOK is meaningless.
func canonHost(raw string, strict bool) (host string, isIP, idnaOK bool) {
	h := strings.TrimRight(asciiLower(raw), ".")
	if strings.HasPrefix(h, "[") {
		// Parse admits brackets only around an IPv6 address.
		return h, true, !strict
	}
	if strict || !idnaShortcut(h) {
		a, err := idna.Lookup.ToASCII(h)
		idnaOK = err == nil
		if idnaOK {
			a = strings.TrimRight(a, ".")
			if strict && a != h {
				// The ASCII form must itself pass unchanged, or
				// Reserialize would not be idempotent: x/net/idna
				// encodes labels of a few thousand bytes that its own
				// punycode decoder then rejects.
				b, err := idna.Lookup.ToASCII(a)
				idnaOK = err == nil && strings.TrimRight(b, ".") == a
			}
			h = a
		}
	}
	return h, endsInNumber(h), idnaOK
}

// asciiLower returns s with A–Z lowercased; s itself if it has none.
func asciiLower(s string) string {
	i := 0
	for i < len(s) && !('A' <= s[i] && s[i] <= 'Z') {
		i++
	}
	if i == len(s) {
		return s
	}
	b := []byte(s)
	for ; i < len(b); i++ {
		if 'A' <= b[i] && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// idnaShortcut reports whether lowercase host h can skip IDNA: it consists of
// [a-z0-9._-] only and no label starts with "xn--".
func idnaShortcut(h string) bool {
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
		if c == 'x' && (i == 0 || h[i-1] == '.') && strings.HasPrefix(h[i:], "xn--") {
			return false
		}
	}
	return true
}

// endsInNumber implements the WHATWG host parser's "ends in a number" check
// on an ASCII host without trailing dots: the last label is all digits, or
// "0x" followed by zero or more hex digits. Dotted IPv4 hosts satisfy it.
func endsInNumber(h string) bool {
	last := h[strings.LastIndexByte(h, '.')+1:]
	if last == "" {
		return false
	}
	if strings.HasPrefix(last, "0x") || strings.HasPrefix(last, "0X") {
		for i := 2; i < len(last); i++ {
			if !isHex(last[i]) {
				return false
			}
		}
		return true
	}
	for i := 0; i < len(last); i++ {
		if last[i] < '0' || last[i] > '9' {
			return false
		}
	}
	return true
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
