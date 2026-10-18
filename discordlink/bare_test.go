package discordlink

import (
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBareEnd(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// Scheme: lowercase http(s) only, then at least two runes.
		{"https://a.com/x", "https://a.com/x"},
		{"http://ab", "http://ab"},
		{"http://a", ""},
		{"https://", ""},
		{"https:// a", ""},
		{"https://<a", ""},
		{"HTTPS://a.com", ""},
		{"Https://a.com", ""},
		{"https:/a.com", ""},
		{"ftp://a.com", ""},
		{"httpx://a.com", ""},
		// Last-character rule.
		{"https://a.com/x.", "https://a.com/x"},
		{"https://a.com/x,", "https://a.com/x"},
		{"https://a.com/x:", "https://a.com/x"},
		{"https://a.com/x;", "https://a.com/x"},
		{`https://a.com/x"`, "https://a.com/x"},
		{"https://a.com/x'", "https://a.com/x"},
		{"https://a.com/x]", "https://a.com/x"},
		{`https://a.com/x.,:;"']`, "https://a.com/x"},
		{"https://a.com/x.y", "https://a.com/x.y"},
		{"https://..", ""},
		{"https://.", ""},
		{"https://a.", ""},
		{"https://x.a", "https://x.a"},
		{"https://a.com/x>", "https://a.com/x>"},
		{"https://a.com/x|", "https://a.com/x|"},
		{"https://a.com/x`", "https://a.com/x`"},
		{"https://a.com/x*", "https://a.com/x*"},
		{"https://a.com/x_", "https://a.com/x_"},
		{"https://a.com/x~", "https://a.com/x~"},
		{`https://a.com/x\`, `https://a.com/x\`},
		{"https://a.com/x||y", "https://a.com/x||y"},
		// Where the run ends.
		{"https://a.com/x y", "https://a.com/x"},
		{"https://a.com/x<y", "https://a.com/x"},
		{"https://a.com/x\ny", "https://a.com/x"},
		{"https://a.com/x\ty", "https://a.com/x"},
		{"https://a.com/x\u00a0y", "https://a.com/x"},
		{"https://a.com/x\u3000y", "https://a.com/x"},
		{"https://a.com/x\ufeffy", "https://a.com/x"},
		{"https://a.com/x\u2028y", "https://a.com/x"},
		{"https://a.com/x\u200by", "https://a.com/x\u200by"},
		{"https://a.com/x\u0085y", "https://a.com/x\u0085y"},
		{"https://a.com/x\u200b", "https://a.com/x\u200b"},
		{"https://a.com/é", "https://a.com/é"},
		{"https://a.com/x.\u00a0", "https://a.com/x"},
		// Invalid UTF-8: a one-byte rune, neither whitespace nor excluded.
		{"https://a.com/\xff", "https://a.com/\xff"},
		{"https://a.com/x\xe2\x80", "https://a.com/x\xe2\x80"},
		{"https://\xff\xfe", "https://\xff\xfe"},
		{"https://\xff", ""},
		// Astral runes count once (see bareEnd).
		{"https://😀", ""},
		{"https://😀😀", "https://😀😀"},
		// U1: an unbalanced trailing ')' is dropped, then the rule reapplies.
		{"https://a.com/x)", "https://a.com/x"},
		{"https://a.com/x))", "https://a.com/x"},
		{"https://a.com/(x)", "https://a.com/(x)"},
		{"https://a.com/(x))", "https://a.com/(x)"},
		{"https://en.wikipedia.org/wiki/Mercury_(planet)", "https://en.wikipedia.org/wiki/Mercury_(planet)"},
		{"https://a.com/x).", "https://a.com/x"},
		{"https://a.com/x.)", "https://a.com/x"},
		{"https://a.com/x.).)", "https://a.com/x"},
		{"https://a.com/)x", "https://a.com/)x"},
		{"https://a.com/((x)", "https://a.com/((x)"},
		{"https://a.com/x)(", "https://a.com/x)("},
		{"https://ab)", "https://ab"},
		{"https://a)", ""},
		{"https://a))", ""},
		{"https://))", ""},
		{"https://a.)", ""},
	} {
		want := -1
		if c.want != "" {
			want = len(c.want)
			if !strings.HasPrefix(c.in, c.want) {
				t.Fatalf("bad case %q", c.in)
			}
		}
		if got := bareEnd(c.in, 0, len(c.in)); got != want {
			t.Errorf("bareEnd(%q) = %d, want %d (%q)", c.in, got, want, c.want)
		}
		if got, ref := bareEnd(c.in, 0, len(c.in)), bareRef(c.in); got != ref {
			t.Errorf("bareEnd(%q) = %d, reference %d", c.in, got, ref)
		}
	}
}

func TestBareEndBounds(t *testing.T) {
	s := "x(https://a.com/x) y"
	if got := bareEnd(s, 2, len(s)); got != len("x(https://a.com/x") {
		t.Errorf("from 2: %d", got)
	}
	if got := bareEnd(s, 0, len(s)); got != -1 {
		t.Errorf("from 0: %d", got)
	}
	// hi bounds the scheme, the run and the trim.
	for hi := range len(s) + 1 {
		got := bareEnd(s, 2, max(hi, 2))
		ref := bareRef(s[2:max(hi, 2)])
		if ref >= 0 {
			ref += 2
		}
		if got != ref {
			t.Errorf("hi %d: %d, reference %d", hi, got, ref)
		}
	}
	// A rune cut by hi is a one-byte rune.
	if got := bareEnd("https://ab\u00a0", 0, len("https://ab")+1); got != len("https://ab")+1 {
		t.Errorf("cut NBSP: %d", got)
	}
}

// bareRe is the bare-link regexp, with JavaScript's \s spelled out.
var bareRe = regexp.MustCompile(`^https?://[^` + jsSpaceClass + `<]+[^<.,:;"'\]` + jsSpaceClass + `]`)

const jsSpaceClass = `\t\n\v\f\r \x{A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// bareRef is an independent form of bareEnd(s, 0, len(s)): match the
// regexp, then drop an unbalanced trailing ')' and match again.
func bareRef(s string) int {
	m := bareRe.FindString(s)
	for m != "" && strings.HasSuffix(m, ")") && strings.Count(m, ")") > strings.Count(m, "(") {
		m = bareRe.FindString(m[:len(m)-1])
	}
	if m == "" {
		return -1
	}
	return len(m)
}

func TestBareEndRandom(t *testing.T) {
	pieces := []string{"https://", "http://", "a", "b.c", "/", ".", ",", ":", ";", `"`, "'", "]", "[",
		"(", ")", "<", ">", " ", "\n", "|", "*", "\u00a0", "\u200b", "\u0085", "\u3000", "é", "😀", "\xff", "\xe2\x80", "%2E"}
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50000 {
		var b strings.Builder
		b.WriteString("https://")
		for range rng.IntN(12) {
			b.WriteString(pieces[rng.IntN(len(pieces))])
		}
		s := b.String()
		if got, ref := bareEnd(s, 0, len(s)), bareRef(s); got != ref {
			t.Fatalf("bareEnd(%q) = %d, reference %d", s, got, ref)
		}
	}
}

func TestBareEndLinear(t *testing.T) {
	// Long trims stay correct; their cost is linear (one walk back).
	n := 1 << 16
	for _, c := range []struct{ in, want string }{
		{"https://ab" + strings.Repeat(")", n), "https://ab"},
		{"https://ab(" + strings.Repeat(")", n), "https://ab()"},
		{"https://a.com/" + strings.Repeat(".)", n), "https://a.com/"},
		{"https://a.com/x" + strings.Repeat(".", n), "https://a.com/x"},
		{"https://" + strings.Repeat(".", n), ""},
	} {
		want := -1
		if c.want != "" {
			want = len(c.want)
		}
		if got := bareEnd(c.in, 0, len(c.in)); got != want {
			t.Errorf("bareEnd(%.20q…) = %d, want %d", c.in, got, want)
		}
	}
}

func TestIsJSSpace(t *testing.T) {
	const ws = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	if n := utf8.RuneCountInString(ws); n != 25 {
		t.Fatalf("%d code points", n)
	}
	for r := rune(0); r <= utf8.MaxRune; r++ {
		if got, want := isJSSpace(r), strings.ContainsRune(ws, r); got != want {
			t.Errorf("isJSSpace(%U) = %v", r, got)
		}
	}
	if isJSSpace(utf8.RuneError) {
		t.Error("RuneError is space")
	}
}

func TestIsLineTerm(t *testing.T) {
	for r := rune(0); r <= utf8.MaxRune; r++ {
		want := r == '\n' || r == '\r' || r == 0x2028 || r == 0x2029
		if got := isLineTerm(r); got != want {
			t.Errorf("isLineTerm(%U) = %v", r, got)
		}
	}
}
