package matchpattern

import "strings"

// glob is a path glob split at its '*' wildcards. A glob with no '*' is one
// literal that must equal the text; otherwise the text must start with the
// first literal, end with the last, and contain the ones in between in order.
// Every other byte, '?' and '\' included, is literal.
//
// For a '*'-only grammar the leftmost occurrence of each middle literal is
// always a valid choice, so matching needs no backtracking: one prefix
// compare, one suffix compare and one strings.Index per middle literal, which
// is linear in the text in practice. Chromium's base::MatchPattern gives up
// past 16 wildcards; this matcher does not, which only matters for patterns
// no list uses.
type glob []string

func newGlob(pattern string) glob { return strings.Split(pattern, "*") }

func (g glob) match(s string) bool {
	if len(g) == 1 {
		return s == g[0]
	}
	pre, suf := g[0], g[len(g)-1]
	if len(s) < len(pre)+len(suf) || s[:len(pre)] != pre || s[len(s)-len(suf):] != suf {
		return false
	}
	s = s[len(pre) : len(s)-len(suf)]
	for _, lit := range g[1 : len(g)-1] {
		i := strings.Index(s, lit)
		if i < 0 {
			return false
		}
		s = s[i+len(lit):]
	}
	return true
}

// UnescapeNormal decodes every %XX escape once, except those Chromium's
// UnescapeRule::NORMAL leaves alone: bytes <= 0x20, 0x7F and
// / \ ; ? : @ & = + $ , # %. Invalid escapes stay literal. It returns s itself
// (no allocation) when s has no escape it would decode.
//
// Chromium additionally refuses to decode escapes that do not form valid
// UTF-8 and a few spoofable code points; this port decodes every byte >= 0x80.
// Patterns in the vendored lists are ASCII, and the raw-form match runs as
// well, so the difference is confined to the GURL canonicalization gap of
// DV-6.
func UnescapeNormal(s string) string {
	i := firstDecodable(s, 0)
	if i < 0 {
		return s
	}
	b := make([]byte, 0, len(s))
	b = append(b, s[:i]...)
	for i < len(s) {
		if v, ok := decodable(s, i); ok {
			b = append(b, v)
			i += 3
			continue
		}
		b = append(b, s[i])
		i++
	}
	return string(b)
}

// firstDecodable returns the index of the first escape at or after i that
// UnescapeNormal decodes, or -1.
func firstDecodable(s string, i int) int {
	for ; i < len(s); i++ {
		if _, ok := decodable(s, i); ok {
			return i
		}
	}
	return -1
}

// decodable reports whether s[i:] starts with an escape that NORMAL decodes,
// and its byte.
func decodable(s string, i int) (byte, bool) {
	if s[i] != '%' || i+2 >= len(s) {
		return 0, false
	}
	hi, ok1 := unhex(s[i+1])
	lo, ok2 := unhex(s[i+2])
	if !ok1 || !ok2 {
		return 0, false
	}
	v := hi<<4 | lo
	if v <= 0x20 || v == 0x7f || strings.IndexByte(`/\;?:@&=+$,#%`, v) >= 0 {
		return 0, false
	}
	return v, true
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
