package discordlink

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// hostileShape is one adversarial input family for Find (BenchmarkFindHostile,
// TestFindLinear, the 4 KB goldens and the fuzz seeds).
type hostileShape struct {
	name string
	// depth bounds the nesting depth of paired rules (spoiler, strong, em,
	// u, del) in Discord's grammar without the cap, for every n; maxDepth
	// means "reaches the cap". TestFindLinear allows
	// C·n·(1+min(depth, maxDepth)) steps. The values were measured with
	// simple-markdown at 1, 4 and 16 KB: the '*' and '_' runs nest about
	// n/2 deep uncapped, and star-wrap-k nests 16, 17 and 19 deep for
	// k = 34, 36 and 40.
	depth int
	// gen returns the shape at exactly n bytes, for n ≥ minShapeLen. It is
	// deterministic, returns valid UTF-8 and always contains "http", so Find's
	// no-http fast path never hides the shape.
	gen func(n int) string
}

const minShapeLen = 256

// shapeURL starts most shapes: one plain link, so every shape has "http".
const shapeURL = "https://a.example/x "

// fill returns prefix, then as many copies of unit as fit, then suffix,
// padded with spaces before suffix to exactly n bytes (prefix and suffix
// always included).
func fill(prefix, unit, suffix string, n int) string {
	k := max(0, (n-len(prefix)-len(suffix))/len(unit))
	pad := max(0, n-len(prefix)-len(suffix)-k*len(unit))
	return prefix + strings.Repeat(unit, k) + strings.Repeat(" ", pad) + suffix
}

// repeatTo repeats unit and cuts it to n bytes at a rune boundary, padding
// with spaces.
func repeatTo(unit string, n int) string {
	s := strings.Repeat(unit, n/len(unit)+1)[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + strings.Repeat(" ", n-len(s))
}

// realisticMessage is a plausible chat message (about 600 bytes) with every
// link kind, mentions, emoji, emphasis, code and a spoiler.
const realisticMessage = "hey <@123456789012345678> did you see this? https://www.amazon.com/dp/B0ABCDEF12?tag=x-20&ref_=nav_logo " +
	"it's **way** cheaper than <https://www.example.com/shop/item?id=42&utm_source=newsletter> imo :fire: <:pog:987654321098765432> " +
	"also [the review](https://youtu.be/dQw4w9WgXcQ?si=AbCdEfGh) and ||the ending https://en.wikipedia.org/wiki/Mercury_(planet) is wild|| " +
	"use `go test -run TestX ./...` then check <#112233445566778899> before <t:1700000000:R>, ~~old link~~ " +
	"[docs](<https://pkg.go.dev/net/url#URL.Query>) _really_ __helps__ 🎉🎉 " +
	"```go\nfmt.Println(\"https://not.a.link/in/code\")\n``` " +
	"fwiw https://twitter.com/someone/status/1234567890?s=20&t=abcDEF, cc <@&55555555555555555>\n"

// starWrap is k '*', a link, then k '*': alternating strong and em nesting
// that reaches the cap for k ≥ 34.
func starWrap(k int) func(n int) string {
	unit := strings.Repeat("*", k) + "https://a.com/x" + strings.Repeat("*", k) + " "
	return func(n int) string { return fill(shapeURL, unit, "", n) }
}

// nestedOpen and nestedClose are six distinct paired delimiters, each
// followed (or preceded) by an unmatched '[', so a masked-link opener sits at
// every level the six can reach.
const (
	nestedOpen  = "||[ **[ __[ ~~[ *[ _[ " + shapeURL
	nestedClose = " ]_ ]* ]~~ ]__ ]** ]||"
)

var hostileShapes = []hostileShape{
	// Masked links: label chains, candidates, href backtracking.
	{"open-bracket", 0, func(n int) string { return fill(shapeURL, "[", "", n) }},
	{"label-chain", 0, func(n int) string {
		m := (n - len(shapeURL)) / 2
		return fill(shapeURL+strings.Repeat("[", m), "]", "", n)
	}},
	{"masked-candidates", 0, func(n int) string { return fill(shapeURL, "[a](b", "", n) }},
	{"masked-close-chain", 0, func(n int) string { return fill(shapeURL+"[a", "](x", "", n) }},
	{"href-paren-quote", 0, func(n int) string { return fill(shapeURL+"[l](", `(a "`, "", n) }},
	{"bracket-gap", 0, func(n int) string {
		m := (n - len(shapeURL)) / 2
		return fill(shapeURL+strings.Repeat("[", m)+"]", "a", "", n)
	}},
	// The three shapes above hold no "](", so Find's prepass skips the
	// masked rule; these variants end in one and walk the label chains.
	{"open-bracket-paren", 0, func(n int) string { return fill(shapeURL, "[", "](", n) }},
	{"label-chain-paren", 0, func(n int) string {
		m := (n - len(shapeURL)) / 2
		return fill(shapeURL+strings.Repeat("[", m), "]", "](x", n)
	}},
	{"bracket-gap-paren", 0, func(n int) string {
		m := (n - len(shapeURL)) / 2
		return fill(shapeURL+strings.Repeat("[", m)+"]", "a", "](", n)
	}},
	{"masked-unclosed", 0, func(n int) string { return fill(shapeURL, "[a](https://x ", "", n) }},
	{"masked-every-level", 6, func(n int) string { return fill(nestedOpen, "a](b ", nestedClose, n) }},
	// Spoilers, code, autolinks, mentions, escapes.
	{"spoiler-pairs", 1, func(n int) string { return fill(shapeURL, "||", "", n) }},
	{"open-backtick", 0, func(n int) string { return fill(shapeURL+"`", "a ", "", n) }},
	{"backtick-runs", 0, func(n int) string {
		var b strings.Builder
		b.WriteString(shapeURL)
		for k := 1; b.Len()+k+1 <= n; k++ {
			b.WriteString(strings.Repeat("`", k))
			b.WriteByte(' ')
		}
		return fill(b.String(), " ", "", n)
	}},
	{"open-angle", 0, func(n int) string { return fill(shapeURL, "<https://x ", "", n) }},
	{"mention-colon", 0, func(n int) string { return fill(shapeURL, "<:word ", "", n) }},
	{"backslash-run", 0, func(n int) string { return fill(shapeURL, `\`, "", n) }},
	// Emphasis runs and the depth cap.
	{"star-run", maxDepth, func(n int) string { return fill(shapeURL, "*", "", n) }},
	{"underscore-run", maxDepth, func(n int) string { return fill(shapeURL, "_", "", n) }},
	{"mixed-delims", 1, func(n int) string { return fill(shapeURL, "*a_b~", "", n) }},
	{"star-wrap-34", maxDepth, starWrap(34)},
	{"star-wrap-36", maxDepth, starWrap(36)},
	{"star-wrap-40", maxDepth, starWrap(40)},
	{"nested-open", 6, func(n int) string { return fill("||**__~~*_"+shapeURL, "[a](https://x ", "_*~~__**||", n) }},
	// Bare links.
	{"paren-run", 0, func(n int) string { return fill("(https://a.com/", ")", "", n) }},
	{"paren-units", 0, func(n int) string { return fill("", "(https://a.com/x)) ", "", n) }},
	// Ordinary content at scale.
	{"realistic", 1, func(n int) string { return repeatTo(realisticMessage, n) }},
}

// shapeByName returns the shape called name.
func shapeByName(name string) (hostileShape, bool) {
	for _, s := range hostileShapes {
		if s.name == name {
			return s, true
		}
	}
	return hostileShape{}, false
}

func TestHostileShapes(t *testing.T) {
	seen := map[string]bool{}
	for _, sh := range hostileShapes {
		if seen[sh.name] {
			t.Errorf("duplicate shape %q", sh.name)
		}
		seen[sh.name] = true
		if sh.depth < 0 || sh.depth > maxDepth {
			t.Errorf("%s: depth %d", sh.name, sh.depth)
		}
		for _, n := range []int{minShapeLen, 2048, 4096, 65536} {
			s := sh.gen(n)
			if len(s) != n {
				t.Errorf("%s(%d): %d bytes", sh.name, n, len(s))
			}
			if !utf8.ValidString(s) || !strings.Contains(s, "http") {
				t.Errorf("%s(%d): invalid UTF-8 or no http", sh.name, n)
			}
			if s != sh.gen(n) {
				t.Errorf("%s(%d): not deterministic", sh.name, n)
			}
		}
	}
	if _, ok := shapeByName("open-bracket"); !ok {
		t.Error("shapeByName")
	}
}
