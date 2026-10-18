package discordlink

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// pct is the uppercase %XX encoding of every byte of s, written
// independently of appendEncoded.
func pct(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		fmt.Fprintf(&b, "%%%02X", s[i])
	}
	return b.String()
}

// listedRanges are the non-ASCII code points URL lines encode (§7 plus Q15),
// as inclusive ranges.
var listedRanges = [][2]rune{
	{0x0085, 0x0085}, {0x061C, 0x061C}, {0x200B, 0x200F}, {0x2028, 0x2029},
	{0x202A, 0x202E}, {0x2060, 0x2064}, {0x2066, 0x2069}, {0xFEFF, 0xFEFF},
}

func listedRune(r rune) bool {
	for _, rg := range listedRanges {
		if rg[0] <= r && r <= rg[1] {
			return true
		}
	}
	return false
}

func urlLine(u string, w Wrap, spoiler bool) string {
	return string(appendURLLine(nil, u, w, spoiler))
}

func TestAppendURLLineASCII(t *testing.T) {
	for c := 0; c < utf8.RuneSelf; c++ {
		s := string(rune(c))
		want := "https://a.com/x" + s + "y"
		if c <= ' ' || c == 0x7F || strings.ContainsRune("`|<>", rune(c)) {
			want = "https://a.com/x" + pct(s) + "y"
		}
		if got := urlLine("https://a.com/x"+s+"y", WrapAngle, false); got != "<"+want+">" {
			t.Errorf("byte %#02x, WrapAngle: %q, want %q", c, got, "<"+want+">")
		}
		got := urlLine("https://a.com/x"+s+"y", WrapNone, false)
		if got != want && got != "<"+want+">" {
			t.Errorf("byte %#02x, WrapNone: %q, want %q (bare or wrapped)", c, got, want)
		}
	}
}

func TestAppendURLLineListed(t *testing.T) {
	for _, rg := range listedRanges {
		for r := rg[0]; r <= rg[1]; r++ {
			s := string(r)
			want := "https://a.com/x" + pct(s) + "y"
			for _, w := range []Wrap{WrapAngle, WrapNone} {
				got := urlLine("https://a.com/x"+s+"y", w, false)
				if w == WrapAngle {
					got = strings.TrimSuffix(strings.TrimPrefix(got, "<"), ">")
				}
				if got != want {
					t.Errorf("U+%04X, wrap %d: %q, want %q", r, w, got, want)
				}
			}
		}
	}
	// Neighbours of the listed ranges, and other non-ASCII code points, stay
	// raw (U+200A is JS whitespace, so check it under WrapAngle only).
	for _, r := range []rune{0x0084, 0x0086, 0x00A0, 0x061B, 0x061D, 0x200A, 0x2010, 0x2027, 0x202F, 0x205F, 0x2065, 0x206A, 0xFEFE, 0xFFFD, 0x1F600, 'é'} {
		s := string(r)
		if got, want := urlLine("https://a.com/x"+s+"y", WrapAngle, false), "<https://a.com/x"+s+"y>"; got != want {
			t.Errorf("U+%04X: %q, want %q", r, got, want)
		}
	}
	// Listed code points are matched on runes: a URL spelling U+202E only as
	// %E2%80%AE is untouched.
	if got := urlLine("https://a.com/%E2%80%AE", WrapNone, false); got != "https://a.com/%E2%80%AE" {
		t.Errorf("pre-encoded U+202E: %q", got)
	}
}

func TestAppendURLLineInvalidUTF8(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://a.com/\xff", "https://a.com/%FF"},
		{"https://a.com/\xe2\x80a", "https://a.com/%E2%80a"},        // U+202E's first two bytes, then ASCII
		{"https://a.com/\xe2\x80", "https://a.com/%E2%80"},          // truncated at the end
		{"https://a.com/\xed\xa0\x80x", "https://a.com/%ED%A0%80x"}, // a surrogate
		{"https://a.com/\xc0\xafx", "https://a.com/%C0%AFx"},        // overlong '/'
		{"https://a.com/\x80\xe2\x80\xae", "https://a.com/%80%E2%80%AE"},
		{"https://a.com/\xef\xbf\xbdx", "https://a.com/\xef\xbf\xbdx"}, // a valid U+FFFD stays
	} {
		if got := urlLine(c.in, WrapNone, false); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAppendURLLineIdempotent(t *testing.T) {
	ins := []string{
		"https://a.com/%7C%60%3C%3E%E2%80%AE",
		"https://a.com/a|b`c<d>e f\r\ng\x7f",
		"https://a.com/\xe2\x80\xae\xe2\x80\x8b\xef\xbb\xbf\xc2\x85\xff\xe2\x80",
		"https://a.com/%%%7%G0%",
		"",
	}
	for _, in := range ins {
		once := string(appendEncoded(nil, in))
		if twice := string(appendEncoded(nil, once)); twice != once {
			t.Errorf("%q: enc = %q, enc(enc) = %q", in, once, twice)
		}
		if !utf8.ValidString(once) {
			t.Errorf("%q: enc = %q is not valid UTF-8", in, once)
		}
	}
	if got := urlLine(ins[0], WrapNone, false); got != ins[0] {
		t.Errorf("already encoded: %q", got)
	}
}

func TestAppendURLLineWrapNoneFallback(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"https://a.com/x", "https://a.com/x"},
		{"https://a.com/(x)", "https://a.com/(x)"},
		{"https://a.com/x)", "<https://a.com/x)>"}, // unbalanced ')' (U1)
		{"https://a.com/x]", "<https://a.com/x]>"},
		{"https://.", "<https://.>"},
		{"https://", "<https://>"},
		{"https://a", "<https://a>"},
		{"https://ab", "https://ab"},
		{"HTTPS://a.com/x", "<HTTPS://a.com/x>"},
		{"ftp://a.com/x", "<ftp://a.com/x>"},
		{"", "<>"},
		{"https://a.com/x\u00a0y", "<https://a.com/x\u00a0y>"}, // NBSP ends a bare link
		{"https://a.com/x ", "https://a.com/x%20"},             // encoded, so no fallback
		{"https://a.com/x<@1>", "https://a.com/x%3C@1%3E"},
		{"https://a.com/x>", "https://a.com/x%3E"},
		{"https://a.com/x|", "https://a.com/x%7C"},
		{"https://a.com/x*_~\\", "https://a.com/x*_~\\"}, // Q29: raw
		{"https://a.com/x\"^{}", "https://a.com/x\"^{}"},
	} {
		if got := urlLine(c.in, WrapNone, false); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	for _, p := range ".,:;\"']" {
		in := "https://a.com/x" + string(p)
		if got, want := urlLine(in, WrapNone, false), "<"+in+">"; got != want {
			t.Errorf("trailing %q: %q, want %q", p, got, want)
		}
		if got, want := urlLine(in+"y", WrapNone, false), in+"y"; got != want {
			t.Errorf("inner %q: %q, want %q", p, got, want)
		}
	}
}

func TestAppendURLLineWrapsAndAppends(t *testing.T) {
	for _, c := range []struct {
		in      string
		w       Wrap
		spoiler bool
		want    string
	}{
		{"https://a.com/x", WrapAngle, false, "<https://a.com/x>"},
		{"https://a.com/x", WrapAngle, true, "||<https://a.com/x>||"},
		{"https://a.com/x", WrapNone, true, "||https://a.com/x||"},
		{"https://a.com/x.", WrapNone, true, "||<https://a.com/x.>||"},
		{"https://a.com/x||y", WrapNone, true, "||https://a.com/x%7C%7Cy||"},
		{"https://a.com/x|", WrapAngle, true, "||<https://a.com/x%7C>||"},
	} {
		b := appendURLLine([]byte("pre\n"), c.in, c.w, c.spoiler)
		if got := string(b); got != "pre\n"+c.want {
			t.Errorf("%q wrap %d spoiler %v: %q, want %q", c.in, c.w, c.spoiler, got, "pre\n"+c.want)
		}
	}
}

func TestAppendURLLineAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not meaningful under -race")
	}
	buf := make([]byte, 0, 256)
	for _, w := range []Wrap{WrapAngle, WrapNone} {
		for _, u := range []string{"https://a.com/x?q=1", "https://a.com/x."} {
			n := testing.AllocsPerRun(100, func() { buf = appendURLLine(buf[:0], u, w, true) })
			if n != 0 {
				t.Errorf("%q wrap %d: %v allocs, want 0", u, w, n)
			}
		}
	}
	n := testing.AllocsPerRun(100, func() { buf = appendURLLine(buf[:0], "https://a.com/|", WrapNone, false) })
	if n > 1 {
		t.Errorf("WrapNone with encoding: %v allocs, want ≤ 1", n)
	}
}

func TestStripCRLF(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""}, {"a", "a"}, {"\n", ""}, {"\r\n", ""}, {"a\r\nb\nc\rd", "abcd"},
		{"-# x\u2028y\u0085", "-# x\u2028y\u0085"}, // other line separators are kept
	} {
		if got := stripCRLF(c.in); got != c.want {
			t.Errorf("stripCRLF(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if raceEnabled {
		return
	}
	s := strings.Repeat("abc", 100)
	if n := testing.AllocsPerRun(100, func() { _ = stripCRLF(s) }); n != 0 {
		t.Errorf("no CR/LF: %v allocs, want 0", n)
	}
}
