package discordlink

import (
	"strings"
	"unicode/utf8"
)

// appendURLLine appends the URL line for u to b: u percent-encoded as Repost
// documents, wrapped in <…> under WrapAngle (or under WrapNone when the
// encoded URL would not render as one bare link), then in ||…|| if spoiler.
// The appended length is exactly the length Repost budgets.
//
// It allocates nothing beyond b's growth unless w is WrapNone and u has a
// byte to encode (then one string, for the bareEnd check).
func appendURLLine(b []byte, u string, w Wrap, spoiler bool) []byte {
	if spoiler {
		b = append(b, "||"...)
	}
	start := len(b)
	b = append(b, '<')
	b = appendEncoded(b, u)
	if w == WrapNone && bareWhole(b[start+1:], u) {
		copy(b[start:], b[start+1:])
		b = b[:len(b)-1]
	} else {
		b = append(b, '>')
	}
	if spoiler {
		b = append(b, "||"...)
	}
	return b
}

// bareWhole reports whether enc, the encoding of u, renders as exactly one
// bare link spanning all of it.
func bareWhole(enc []byte, u string) bool {
	e := u
	if len(enc) != len(u) { // encoding always lengthens
		e = string(enc)
	}
	return bareEnd(e, 0, len(e)) == len(e)
}

// appendEncoded appends u to b with every byte that URL lines must not carry
// raw percent-encoded as %XX (uppercase hex): '`', '|', '<', '>', the bytes
// 0x00–0x20 and 0x7F, every byte of invalid UTF-8, and the UTF-8 bytes of
// U+0085, U+061C, U+200B–U+200F, U+2028–U+202E, U+2060–U+2064,
// U+2066–U+2069 and U+FEFF. '%' is kept, so the encoding is idempotent.
func appendEncoded(b []byte, u string) []byte {
	const hex = "0123456789ABCDEF"
	for i := 0; i < len(u); {
		n, enc := urlUnit(u, i)
		if !enc {
			b = append(b, u[i:i+n]...)
		} else {
			for k := i; k < i+n; k++ {
				b = append(b, '%', hex[u[k]>>4], hex[u[k]&15])
			}
		}
		i += n
	}
	return b
}

// urlUnit returns the length of the unit at u[i] (one byte, or one valid
// UTF-8 rune) and whether appendEncoded encodes it.
func urlUnit(u string, i int) (n int, enc bool) {
	if c := u[i]; c < utf8.RuneSelf {
		return 1, c <= ' ' || c == 0x7F || c == '`' || c == '|' || c == '<' || c == '>'
	}
	r, n := utf8.DecodeRuneInString(u[i:])
	if r == utf8.RuneError && n == 1 {
		return 1, true
	}
	return n, encodedRune(r)
}

// encodedRune reports whether the non-ASCII rune r is one of the line
// separators, bidi formatting or zero-width code points URL lines encode.
func encodedRune(r rune) bool {
	switch {
	case r == 0x0085, r == 0x061C, r == 0xFEFF:
		return true
	case 0x200B <= r && r <= 0x200F, // ZWSP, ZWNJ, ZWJ, LRM, RLM
		0x2028 <= r && r <= 0x202E, // LS, PS, LRE, RLE, PDF, LRO, RLO
		0x2060 <= r && r <= 0x2064, // WJ and the invisible operators
		0x2066 <= r && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	}
	return false
}

// stripCRLF returns s without its CR and LF bytes, allocating only if it has
// any.
func stripCRLF(s string) string {
	i := strings.IndexAny(s, "\r\n")
	if i < 0 {
		return s
	}
	b := make([]byte, i, len(s)-1)
	copy(b, s[:i])
	for ; i < len(s); i++ {
		if c := s[i]; c != '\r' && c != '\n' {
			b = append(b, c)
		}
	}
	return string(b)
}
