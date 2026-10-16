package rawurl

// Byte classes of §5.4: pathSafe is unreserved ∪ sub-delims ∪ ":@/";
// querySafe adds '?'. querySafe also governs the fragment.
var (
	pathSafe  = byteSet("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~" + "!$&'()*+,;=" + ":@/")
	querySafe = byteSet("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~" + "!$&'()*+,;=" + ":@/" + "?")
)

func byteSet(s string) (t [256]bool) {
	for i := 0; i < len(s); i++ {
		t[s[i]] = true
	}
	return t
}

// Reserialize renders a URL accepted by ParseTarget (§5.4):
//
//	lower(scheme) "://" asciiHost [":" port]
//	+ enc(path, pathSafe) ["?" enc(query, querySafe)] ["#" enc(fragment, querySafe)]
//
// asciiHost is the host view, which ParseTarget guarantees is the
// idna.Lookup.ToASCII form, so Unicode hosts come out as punycode. enc copies
// safe bytes and well-formed "%XX" escapes and turns every other byte into
// uppercase "%XX"; only the first '#' delimits, so later ones become %23. An
// empty path stays empty. The output has no byte <= 0x20 or 0x7F, and
// Reserialize is idempotent through ParseTarget.
func Reserialize(u *URL) string {
	n := len(u.Scheme) + 3 + len(u.Host) + 1 + len(u.Port) + len(u.Path) + 1 + len(u.Query) + 1 + len(u.Fragment)
	b := make([]byte, 0, n+n/4)
	b = append(b, u.Scheme...)
	b = append(b, "://"...)
	b = append(b, u.Host...)
	if u.Port != "" {
		b = append(b, ':')
		b = append(b, u.Port...)
	}
	b = appendEnc(b, u.Path, &pathSafe)
	if u.HasQuery {
		b = append(b, '?')
		b = appendEnc(b, u.Query, &querySafe)
	}
	if u.HasFragment {
		b = append(b, '#')
		b = appendEnc(b, u.Fragment, &querySafe)
	}
	return string(b)
}

const upperHex = "0123456789ABCDEF"

func appendEnc(b []byte, s string, safe *[256]bool) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case safe[c]:
			b = append(b, c)
		case c == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b = append(b, s[i:i+3]...)
			i += 2
		default:
			b = append(b, '%', upperHex[c>>4], upperHex[c&15])
		}
	}
	return b
}
