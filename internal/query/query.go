package query

import "strings"

// Segment is one '&'-separated piece of a raw query or fragment.
type Segment struct {
	Text  string // raw bytes between separators; may be ""
	Name  string // Text before the first '=', or all of Text if it has none
	Value string // Text after the first '='; "" if none
	HasEq bool
}

// Parse appends every segment of q to dst, empty ones included, so a query
// with n separators yields n+1 segments ("" yields one empty segment). ';' is
// not a separator. Every string is a substring of q; Parse does not allocate
// when dst has capacity.
func Parse(dst []Segment, q string) []Segment {
	for {
		text, rest, more := strings.Cut(q, "&")
		name, value, eq := strings.Cut(text, "=")
		dst = append(dst, Segment{Text: text, Name: name, Value: value, HasEq: eq})
		if !more {
			return dst
		}
		q = rest
	}
}

// Join joins with '&' the Text of every segment that is non-empty and not
// dropped; drop may be nil. Empty segments therefore collapse, which the
// engine relies on only after something changed (§5.1, DV-7).
func Join(segs []Segment, drop func(i int) bool) string {
	n := 0
	for i, s := range segs {
		if s.Text != "" && (drop == nil || !drop(i)) {
			n += len(s.Text) + 1
		}
	}
	if n == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n - 1)
	for i, s := range segs {
		if s.Text == "" || drop != nil && drop(i) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(s.Text)
	}
	return b.String()
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
