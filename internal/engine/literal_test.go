package engine

import (
	"math/rand/v2"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
)

func TestRequiredLiterals(t *testing.T) {
	for _, c := range []struct {
		re   string
		want []string
	}{
		{`/unsubscribe`, []string{"/unsubscribe"}},
		{`[Uu]nsubscribe|emailWebview`, []string{"Unsubscribe", "emailWebview", "unsubscribe"}},
		{`[?&]campid=`, []string{"&campid=", "?campid="}},
		{`/ref=[^/?]*`, []string{"/ref="}},
		{`^https?://x\.example/`, []string{"http://x.example/", "https://x.example/"}},
		{`a+bc`, []string{"bc"}},
		{`(?:ab)+`, []string{"ab"}},
		{`\bfoo\b`, []string{"foo"}},
		{`é+x`, []string{"é"}},
		{`x.*yz`, []string{"yz"}},
		{`(?i)uN`, []string{"UN", "Un", "uN", "un"}},
		{`(?i)k`, []string{"K", "k", "\u212a"}},
		// s folds to U+017F too: 2·2·3 variants of "uns", and "unsu" would be 24.
		{`(?i)unsubscribe`, []string{"UNS", "UNs", "UN\u017f", "UnS", "Uns", "Un\u017f", "uNS", "uNs", "uN\u017f", "unS", "uns", "un\u017f"}},
		{`\x{FFFD}ab`, nil},
		{`ab\x{FFFD}`, []string{"ab"}},
		{`[a-z]+`, nil},
		{`.*`, nil},
		{`a?`, nil},
		{`x|.*`, nil},
		{`\x{FFFD}`, nil},
		{`[^a]`, nil},
	} {
		tree, err := syntax.Parse(c.re, syntax.Perl)
		if err != nil {
			t.Fatal(err)
		}
		if got := requiredLiterals(tree.Simplify()); !slices.Equal(got, c.want) {
			t.Errorf("%s: %q, want %q", c.re, got, c.want)
		}
	}
}

// The prefilter never rejects a string the regexp matches.
func TestLiteralPrefilterSound(t *testing.T) {
	exprs := []string{
		`[?&]_(pos|ss|psq)=`, `[Uu]nsubscribe|emailWebview`, `/email/`, `[?&]campid=`, `/ref=[^/?]*`,
		`^https?://(www\.)?ex(a|am)ple\.(com|net)/`, `a(b|c)?d`, `(ab|)c`, `x(y?z)+w`, `(?m)^ab$`,
		`\bq=`, `(foo|bar)(baz|qux)(1|2|3)`, `(a|b|c|d|e)(f|g|h|i)(j|k)`, `é(x|y)`, `[ab][cd][ef][gh][ij]`,
		`(?:x{2,3})y`, `(?s)a.b`, `zz[0-9]+_`, `(?i:A)bc`, `ab|cd|`, `(?i)ks`, `(?i)xkcd\x{FFFD}`,
	}
	rng := rand.New(rand.NewPCG(3, 4))
	for _, expr := range exprs {
		l, err := compileLRE(expr)
		if err != nil {
			t.Fatal(err)
		}
		alpha := []string{"", "\n", "\xff", "\xc3", "\u212a", "\u017f", "K", "S"}
		for _, r := range expr {
			if r != '\\' && r != '(' && r != ')' && r != '|' && r != '[' && r != ']' {
				alpha = append(alpha, string(r))
			}
		}
		matched := 0
		for range 20000 {
			var b strings.Builder
			for range rng.IntN(12) {
				b.WriteString(alpha[rng.IntN(len(alpha))])
			}
			s := b.String()
			if l.Regexp.MatchString(s) {
				matched++
				if !l.MatchString(s) {
					t.Errorf("%s: prefilter %q rejects %q, which matches", expr, l.lits, s)
				}
			}
		}
		if matched == 0 {
			t.Logf("%s: no random string matched (lits %q)", expr, l.lits)
		}
	}
}
