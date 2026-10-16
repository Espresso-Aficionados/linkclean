package query

import (
	"slices"
	"strings"
)

// Segment is one non-empty '&'-separated piece of a raw query or fragment.
type Segment struct {
	Text  string // raw bytes between separators
	Name  string // Text before the first '=', or all of Text if it has none
	Value string // Text after the first '='; "" if none
	HasEq bool
}

// Parse appends the non-empty segments of q to dst. Empty segments ("&&", a
// leading or trailing '&') carry no name, so no rule can act on them, and the
// engine rebuilds a changed query without them (§5.1). ';' is not a
// separator. Every string is a substring of q. Parse does not allocate when
// dst has room, and grows dst at most once.
func Parse(dst []Segment, q string) []Segment {
	if room := cap(dst) - len(dst); len(q)/2+1 > room {
		if n := count(q); n > room {
			dst = slices.Grow(dst, n)
		}
	}
	for q != "" {
		text, rest, _ := strings.Cut(q, "&")
		if text != "" {
			name, value, eq := strings.Cut(text, "=")
			dst = append(dst, Segment{Text: text, Name: name, Value: value, HasEq: eq})
		}
		q = rest
	}
	return dst
}

// count returns the number of non-empty segments of q.
func count(q string) int {
	n := 0
	for q != "" {
		i := strings.IndexByte(q, '&')
		if i < 0 {
			return n + 1
		}
		if i > 0 {
			n++
		}
		q = q[i+1:]
	}
	return n
}

// Canon returns canon(name) (§4.1): "%XX" decoded once (invalid escapes stay
// literal, '+' is untouched), then ASCII-lowercased. It returns name itself
// when that changes nothing.
func Canon(name string) string {
	for i := 0; i < len(name); i++ {
		if c := name[i]; 'A' <= c && c <= 'Z' || c == '%' && escapeAt(name, i) {
			return string(AppendCanon(make([]byte, 0, len(name)), name))
		}
	}
	return name
}

// AppendCanon appends canon(name) to dst. With a stack buffer as dst, a map
// lookup m[string(b)] on the result does not allocate.
func AppendCanon(dst []byte, name string) []byte {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '%' && escapeAt(name, i) {
			c = unhex(name[i+1])<<4 | unhex(name[i+2])
			i += 2
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

// DecodeValue decodes "%XX" exactly once; invalid escapes stay literal and
// '+' stays literal (§5.4, DV-12). It returns s itself when s has no valid
// escape.
func DecodeValue(s string) string {
	i := strings.IndexByte(s, '%')
	for i >= 0 && !escapeAt(s, i) {
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			return s
		}
		i += 1 + j
	}
	if i < 0 {
		return s
	}
	b := make([]byte, i, len(s))
	copy(b, s[:i])
	for ; i < len(s); i++ {
		c := s[i]
		if c == '%' && escapeAt(s, i) {
			c = unhex(s[i+1])<<4 | unhex(s[i+2])
			i += 2
		}
		b = append(b, c)
	}
	return string(b)
}

// EncodeComponent keeps the RFC 3986 unreserved bytes [A-Za-z0-9-._~] and
// turns every other byte into uppercase "%XX" (§5.4). DecodeValue inverts it.
// It returns s itself when every byte is unreserved.
func EncodeComponent(s string) string {
	i := 0
	for i < len(s) && unreserved(s[i]) {
		i++
	}
	if i == len(s) {
		return s
	}
	const hex = "0123456789ABCDEF"
	b := make([]byte, i, len(s)+2*(len(s)-i))
	copy(b, s[:i])
	for ; i < len(s); i++ {
		if c := s[i]; unreserved(c) {
			b = append(b, c)
		} else {
			b = append(b, '%', hex[c>>4], hex[c&15])
		}
	}
	return string(b)
}

// HasHTTPScheme reports whether v, decoded once (§5.4), starts with "http://"
// or "https://" (ASCII case-insensitive), without decoding all of v.
func HasHTTPScheme(v string) bool {
	var b [8]byte
	n := 0
	for i := 0; i < len(v) && n < len(b); i++ {
		c := v[i]
		if c == '%' && escapeAt(v, i) {
			c = unhex(v[i+1])<<4 | unhex(v[i+2])
			i += 2
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[n] = c
		n++
	}
	return n >= 7 && string(b[:7]) == "http://" || n == 8 && string(b[:8]) == "https://"
}

// IsFragmentParams reports whether fragment f is parameter-shaped (§5.3): it
// contains '=', contains neither '/' nor '?', and the name of every non-empty
// '&' segment matches ^[A-Za-z0-9_.\-%\[\]]+$.
func IsFragmentParams(f string) bool {
	if strings.IndexByte(f, '=') < 0 || strings.ContainsAny(f, "/?") {
		return false
	}
	for f != "" {
		var seg string
		seg, f, _ = strings.Cut(f, "&")
		if seg == "" {
			continue
		}
		name, _, _ := strings.Cut(seg, "=")
		if name == "" {
			return false
		}
		for i := 0; i < len(name); i++ {
			if !fragNameByte(name[i]) {
				return false
			}
		}
	}
	return true
}

func fragNameByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '_' || c == '.' || c == '-' || c == '%' || c == '[' || c == ']'
}

func unreserved(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
		c == '-' || c == '.' || c == '_' || c == '~'
}

// escapeAt reports whether s[i] (a '%') starts a "%XX" escape.
func escapeAt(s string, i int) bool {
	return i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2])
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c >= 'a':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}
