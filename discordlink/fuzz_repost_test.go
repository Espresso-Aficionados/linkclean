package discordlink

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/Espresso-Aficionados/linkclean"
)

// findPlaceholder reports whether Find is still A0's placeholder (bare links
// and spoilers only), which cannot check the round trip: it reports
// "<https://a.com/x>" as the bare link "https://a.com/x>".
var findPlaceholder = sync.OnceValue(func() (placeholder bool) {
	defer func() {
		if r := recover(); r != nil {
			placeholder = true
		}
	}()
	got := Find("<https://a.com/x>")
	return len(got) == 1 && got[0].Kind == Bare && got[0].URL == "https://a.com/x>"
})

// repostFuzz is one FuzzRepost input decoded into Changes and options. Its
// Annotate and Overflow are deterministic functions of the input, so the
// checks can recompute what they returned.
type repostFuzz struct {
	cs                     []Change
	o                      RepostOptions
	annotate, header, ovf  bool
	pieces                 []string // Annotate line contents
	lineSeed, reqSeed, ovS int
	calls                  [2][]int // Annotate calls per Change, full and compact
	ovfCalls               []int    // omitted counts Overflow was called with
}

// Flag bits of FuzzRepost's flags argument.
const (
	fzWrapNone    = 1 << 0
	fzNoSpoiler   = 1 << 1
	fzMaxLenShift = 2  // 12 bits: MaxLen = 1 + v%4000
	fzBodiesShift = 14 // 2 bits: MaxBodies 0–3
	fzUnexShift   = 16 // 5 bits: Unexamined = v%21
	fzAnnotate    = 1 << 21
	fzHeader      = 1 << 22
	fzOverflow    = 1 << 23
	fzSpoilShift  = 24 // 12 bits: Link.Spoiler per Change
	fzSeedShift   = 36 // 3 × 8 bits: line, required and overflow seeds
	fzHTTPShift   = 60 // 1 bit: alternate http:// and https://
	fzNoChanges   = 1 << 61
)

func newRepostFuzz(urls string, flags uint64, ann, header string) *repostFuzz {
	fz := &repostFuzz{
		annotate: flags&fzAnnotate != 0,
		header:   flags&fzHeader != 0,
		ovf:      flags&fzOverflow != 0,
		pieces:   strings.Split(ann, "\x1e"),
		lineSeed: int(flags >> fzSeedShift & 0xFF),
		reqSeed:  int(flags >> (fzSeedShift + 8) & 0xFF),
		ovS:      int(flags >> (fzSeedShift + 16) & 0xFF),
	}
	fz.o = RepostOptions{
		NoSpoiler:  flags&fzNoSpoiler != 0,
		MaxLen:     1 + int(flags>>fzMaxLenShift&0xFFF)%4000,
		MaxBodies:  int(flags >> fzBodiesShift & 3),
		Unexamined: int(flags>>fzUnexShift&0x1F) % 21,
	}
	if flags&fzWrapNone != 0 {
		fz.o.Wrap = WrapNone
	}
	if flags&fzNoChanges == 0 {
		for i, p := range strings.Split(urls, "\x1e") {
			if i == 12 {
				break
			}
			scheme := "https://"
			if (i+int(flags>>fzHTTPShift&1))%3 == 0 {
				scheme = "http://"
			}
			// "m<i>-" makes every URL line distinct.
			u := scheme + "a.com/m" + strconv.Itoa(i) + "-" + p
			fz.cs = append(fz.cs, Change{
				Link:   Link{URL: u, Start: i, Kind: Bare, Spoiler: flags>>(fzSpoilShift+i)&1 != 0},
				Result: linkclean.Result{Input: u, URL: u, Changed: true},
			})
		}
	}
	fz.calls = [2][]int{make([]int, len(fz.cs)), make([]int, len(fz.cs))}
	if fz.annotate {
		fz.o.Annotate = func(c Change, compact bool) ([]string, int) {
			m := 0
			if compact {
				m = 1
			}
			fz.calls[m][c.Link.Start]++
			return fz.lines(c.Link.Start, compact)
		}
	}
	if fz.header {
		fz.o.Header = "-H " + header
	}
	if fz.ovf {
		fz.o.Overflow = func(omitted, unexamined int) string {
			fz.ovfCalls = append(fz.ovfCalls, omitted)
			return fz.overflowLine(omitted, unexamined)
		}
	}
	return fz
}

// lines is Annotate's result for Change i: 0–3 lines, each starting with
// "-# " (a URL line never does), with CR/LF possible inside, and required
// anywhere in [-2, len+3].
func (fz *repostFuzz) lines(i int, compact bool) ([]string, int) {
	m := 0
	if compact {
		m = 1
	}
	n := (fz.lineSeed + 3*i + m) % 4
	var ls []string
	for j := range n {
		ls = append(ls, fmt.Sprintf("-# %d%c%d %s", i, "fc"[m], j, fz.pieces[(i+j+m)%len(fz.pieces)]))
	}
	return ls, (fz.reqSeed+5*i+2*m)%(n+6) - 2
}

// overflowLine is Overflow's result: "" for some counts, otherwise
// "-O <omitted> <unexamined>" and padding whose length is not monotone in
// omitted, with CRs in it.
func (fz *repostFuzz) overflowLine(omitted, unexamined int) string {
	if (omitted+fz.ovS)%4 == 0 {
		return ""
	}
	return fmt.Sprintf("-O %d %d ", omitted, unexamined) + strings.Repeat("p\r", (omitted*37+fz.ovS)%29)
}

// pctDecode decodes every valid %XX in s.
func pctDecode(s string) string {
	unhex := func(c byte) int {
		switch {
		case '0' <= c && c <= '9':
			return int(c - '0')
		case 'a' <= c && c <= 'f':
			return int(c-'a') + 10
		case 'A' <= c && c <= 'F':
			return int(c-'A') + 10
		}
		return -1
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s)+0 && i+2 <= len(s)-1 {
			if h, l := unhex(s[i+1]), unhex(s[i+2]); h >= 0 && l >= 0 {
				b.WriteByte(byte(h<<4 | l))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// emittedURL is one URL line found in a body.
type emittedURL struct {
	change  int
	inner   string // the encoded URL, without wrappers
	angle   bool   // wrapped in <…>
	spoiler bool   // wrapped in ||…||
}

func (fz *repostFuzz) check(t *testing.T, bodies []string, st repostStats) {
	t.Helper()
	checkRepostBasics(t, fz.cs, fz.o, bodies, st)
	for m := range 2 {
		want := st.annotate
		if m == 1 {
			want = st.compact
		}
		if !slices.Equal(fz.calls[m], want) {
			t.Errorf("Annotate calls (compact=%v) %v, stats say %v", m == 1, fz.calls[m], want)
		}
	}
	for i := range fz.cs {
		if st.compact[i] > 0 {
			if _, req := fz.lines(i, false); req <= 0 {
				t.Errorf("change %d: compact call after required %d", i, req)
			}
		}
	}
	if len(fz.ovfCalls) > len(fz.cs)+1 {
		t.Errorf("Overflow called %d times for %d changes", len(fz.ovfCalls), len(fz.cs))
	}
	unexamined := max(fz.o.Unexamined, 0)

	var emitted []emittedURL
	next := 0 // the next change an URL line may belong to
	for bi, body := range bodies {
		lines := strings.Split(body, "\n")
		if fz.header {
			if lines[0] != stripCRLF(fz.o.Header) {
				t.Fatalf("body %d: first line %q, want the header", bi, lines[0])
			}
			lines = lines[1:]
		}
		if len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "-O ") {
			if bi != len(bodies)-1 {
				t.Errorf("body %d of %d has an overflow line", bi, len(bodies))
			}
			lines = lines[:len(lines)-1]
		}
		if len(lines) == 0 || strings.HasPrefix(lines[0], "-") {
			t.Fatalf("body %d does not start with a URL line: %q", bi, body)
		}
		for k := 0; k < len(lines); {
			e := fz.checkURLLine(t, lines[k], next)
			emitted = append(emitted, e)
			next = e.change + 1
			k++
			var ann []string
			for k < len(lines) && strings.HasPrefix(lines[k], "-# ") {
				ann = append(ann, lines[k])
				k++
			}
			if k < len(lines) && strings.HasPrefix(lines[k], "-") {
				t.Fatalf("body %d: stray line %q", bi, lines[k])
			}
			fz.checkAnnotation(t, e.change, ann, st)
		}
	}
	if st.emitted != len(emitted) {
		t.Errorf("stats.emitted %d, %d URL lines", st.emitted, len(emitted))
	}
	// The overflow line: only on the last body, for the final counts.
	omitted := len(fz.cs) - len(emitted)
	var want string
	if fz.ovf && len(bodies) > 0 && omitted+unexamined > 0 {
		want = stripCRLF(fz.overflowLine(omitted, unexamined))
	}
	var got string
	if len(bodies) > 0 {
		last := bodies[len(bodies)-1]
		if l := last[strings.LastIndexByte(last, '\n')+1:]; strings.HasPrefix(l, "-O ") {
			got = l
		}
	}
	if got != want {
		t.Errorf("overflow line %q, want %q (omitted %d, unexamined %d)", got, want, omitted, unexamined)
	}
	if fz.annotate || fz.header || fz.ovf {
		return
	}

	// URL-only bodies: no mention syntax outside URLs.
	for _, b := range bodies {
		rest := b
		for _, e := range emitted {
			rest = strings.Replace(rest, e.inner, "", 1)
		}
		for _, m := range []string{"@everyone", "@here", "<@", "<@&", "<#"} {
			if strings.Contains(rest, m) {
				t.Errorf("%q outside URLs in %q", m, b)
			}
		}
	}
	// Round trip: Find on the bodies returns exactly the emitted URLs.
	if findPlaceholder() {
		t.Skip("round trip skipped: Find is the A0 placeholder (not implemented)")
	}
	var found []Link
	for _, b := range bodies {
		for _, l := range Find(b) {
			found = append(found, Link{URL: l.URL, Kind: l.Kind, Spoiler: l.Spoiler, Label: l.Label})
		}
	}
	var wantLinks []Link
	for _, e := range emitted {
		k := Bare
		if e.angle {
			k = Suppressed
		}
		wantLinks = append(wantLinks, Link{URL: e.inner, Kind: k, Spoiler: e.spoiler})
	}
	if !slices.Equal(found, wantLinks) {
		t.Errorf("round trip:\n%s\nFind:\n%v\nwant:\n%v", quoteBodies(bodies), found, wantLinks)
	}
}

// checkURLLine checks one URL line and returns what it holds. The line must
// belong to a change at index ≥ from (blocks stay in order).
func (fz *repostFuzz) checkURLLine(t *testing.T, line string, from int) emittedURL {
	t.Helper()
	e := emittedURL{change: -1}
	s := line
	if strings.HasPrefix(s, "||") {
		if len(s) < 4 || !strings.HasSuffix(s, "||") {
			t.Fatalf("unbalanced spoiler bars in %q", line)
		}
		e.spoiler, s = true, s[2:len(s)-2]
	}
	if strings.HasPrefix(s, "<") {
		if !strings.HasSuffix(s, ">") {
			t.Fatalf("unbalanced <> in %q", line)
		}
		e.angle, s = true, s[1:len(s)-1]
	}
	e.inner = s
	for j := from; j < len(fz.cs); j++ {
		if pctDecode(s) == pctDecode(fz.cs[j].Result.URL) {
			e.change = j
			break
		}
	}
	if e.change < 0 {
		t.Fatalf("URL line %q matches no change at or after %d", line, from)
	}
	c := fz.cs[e.change]
	if want := c.Link.Spoiler && !fz.o.NoSpoiler; e.spoiler != want {
		t.Errorf("%q: spoiler bars %v, want %v", line, e.spoiler, want)
	}
	bare := bareEnd(s, 0, len(s)) == len(s)
	switch {
	case fz.o.Wrap == WrapAngle && !e.angle:
		t.Errorf("%q: WrapAngle line without <>", line)
	case fz.o.Wrap == WrapNone && e.angle && bare:
		t.Errorf("%q: WrapNone line wrapped although it is one bare link", line)
	case fz.o.Wrap == WrapNone && !e.angle && !bare:
		t.Errorf("%q: WrapNone line is not one bare link", line)
	}
	if !utf8.ValidString(s) {
		t.Errorf("%q: invalid UTF-8 in a URL line", line)
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7F || strings.ContainsRune("`|<>", r) || listedRune(r) {
			t.Errorf("%q: raw %U in a URL line", line, r)
		}
	}
	return e
}

// checkAnnotation checks the lines under change i's URL line: verbatim
// (CR/LF removed) trailing lines of the full result, at least its required
// ones, or exactly the required trailing lines of the compact result.
func (fz *repostFuzz) checkAnnotation(t *testing.T, i int, got []string, st repostStats) {
	t.Helper()
	if !fz.annotate {
		if len(got) > 0 {
			t.Errorf("change %d: lines %q without Annotate", i, got)
		}
		return
	}
	strip := func(ls []string, req int) ([]string, int) {
		out := make([]string, len(ls))
		for k, l := range ls {
			out[k] = stripCRLF(l)
		}
		return out, min(max(req, 0), len(ls))
	}
	full, req := strip(fz.lines(i, false))
	if len(got) <= len(full) && len(got) >= req && slices.Equal(got, full[len(full)-len(got):]) {
		return
	}
	if st.compact[i] > 0 {
		cl, creq := strip(fz.lines(i, true))
		if creq > 0 && slices.Equal(got, cl[len(cl)-creq:]) {
			return
		}
	}
	t.Errorf("change %d: lines %q are not a valid degrade of %q (required %d)", i, got, full, req)
}

func FuzzRepost(f *testing.F) {
	const (
		base     = uint64(1999) << fzMaxLenShift // MaxLen 2000
		withAll  = fzAnnotate | fzHeader | fzOverflow
		spoilers = uint64(0xAAA) << fzSpoilShift
	)
	paths := []string{
		"x`y", "a|b", "a||b", "*b*", "_u_", "~~s~~", "<@1>", "x\u202ey", "z\u200b", "w\u2066q",
		"l\r\nm", "dot.", "paren)", "comma,", "(bal)", "q\"", "brk]", "%7C%60", "\xe2\x80", "\xff",
		"sp ace", "tab\t", "\u00a0nb", "\u2028ls", "\u0085nel", "@everyone", "<#1>", "x\\",
		strings.Repeat("p", 1900),
	}
	for i, p := range paths {
		seedFlags := []uint64{
			base, base | fzWrapNone, base | spoilers, base | fzWrapNone | spoilers | fzNoSpoiler,
			base | withAll | uint64(i)<<fzSeedShift | 3<<fzUnexShift, base | fzWrapNone | withAll | 1<<fzBodiesShift | 7<<fzUnexShift,
		}
		for _, fl := range seedFlags {
			f.Add(p+"\x1e"+p+"/2", fl, "note\x1ewith\r\nbreak\x1e`code`", "top\r\nline")
		}
	}
	f.Add("a\x1eb\x1ec\x1ed\x1ee\x1ef\x1eg\x1eh\x1ei\x1ej\x1ek\x1el", uint64(39)<<fzMaxLenShift|fzWrapNone|withAll|2<<fzBodiesShift, strings.Repeat("n", 30), "h")
	f.Add("", uint64(0), "", "")
	f.Add("x", uint64(fzNoChanges|fzOverflow|5<<fzUnexShift), "", "")
	f.Fuzz(func(t *testing.T, urls string, flags uint64, ann, header string) {
		fz := newRepostFuzz(urls, flags, ann, header)
		bodies, st := repost(fz.cs, fz.o)
		fz.check(t, bodies, st)
	})
}

// TestRepostRoundTrip runs FuzzRepost's round trip on fixed inputs, so a
// skip is visible in the test log.
func TestRepostRoundTrip(t *testing.T) {
	if findPlaceholder() {
		t.Skip("Find is the A0 placeholder (not implemented); the round trip needs the real Find")
	}
	urls := "x`y\x1ea||b\x1e*b*_u_~~s~~\x1e<@1>@everyone\x1edot.\x1eparen)\x1e(bal)\x1eq\"\x1ebrk]\x1e\u00a0nb\x1ex\\\x1ew\u2066q"
	for _, fl := range []uint64{0, fzWrapNone, uint64(0xFFF) << fzSpoilShift, fzWrapNone | uint64(0x5A5)<<fzSpoilShift, fzWrapNone | fzNoSpoiler | uint64(0xFFF)<<fzSpoilShift} {
		fl |= uint64(1999) << fzMaxLenShift
		fz := newRepostFuzz(urls, fl, "", "")
		bodies, st := repost(fz.cs, fz.o)
		if len(bodies) == 0 {
			t.Fatalf("flags %#x: no bodies", fl)
		}
		fz.check(t, bodies, st)
	}
}
