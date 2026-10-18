package discordlink

import (
	"fmt"
	"strings"
	"testing"
)

// TestFindUnit is the implementer's own spot check of each rule, with
// offsets. The golden, oracle and reference suites are independent of it.
func TestFindUnit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string // formatLink, then " @<Start>"
	}{
		{"", nil},
		{"no links here", nil},
		{"see https://a.com/x.", []string{`Bare "https://a.com/x" @4`}},
		{"\\https://a.com/x", []string{`Bare "https://a.com/x" @1`}},
		{"\\<https://a.com/x>", []string{`Bare "https://a.com/x>" @2`}},
		{"foohttps://a.com/x", []string{`Bare "https://a.com/x" @3`}},
		{"HTTPS://a.com/x", nil},
		{"\xffhttps://a.com/\xff", []string{`Bare "https://a.com/\xff" @1`}},

		// Autolinks and mentions.
		{"<https://a.com/x>", []string{`Suppressed "https://a.com/x" @1`}},
		{"<https://a.com/x\ny>", []string{`Suppressed "https://a.com/x\ny" @1`}},
		{"<ftp://x/y> <https://a.com/x y>", []string{`Bare "https://a.com/x" @13`}},
		{"<:a_:1> https://a.com/x_", []string{`Bare "https://a.com/x_" @8`}},
		{"</cmd sub_x:1> https://a.com/y_", []string{`Bare "https://a.com/y_" @15`}},
		{"<id:linked-roles:1>_https://a.com/x_", []string{`Bare "https://a.com/x" @20`}},
		{"<t:-5:R> <@&1> <@!1> <#1> <@$1>", nil},

		// Code.
		{"`https://a.com/x` https://b.com/y", []string{`Bare "https://b.com/y" @18`}},
		{"``a`https://a.com/x` https://b.com/z", []string{`Bare "https://a.com/x` + "`" + `" @4`, `Bare "https://b.com/z" @21`}},
		{"```js\nhttps://a.com/x\n``` https://b.com/y", []string{`Bare "https://b.com/y" @26`}},
		{"`a\nhttps://a.com/x`", nil},
		{"```https://a.com/x", []string{`Bare "https://a.com/x" @3`}},

		// Spoilers and emphasis.
		{"||https://a.com/x||", []string{`Bare "https://a.com/x" spoiler @2`}},
		{"||**https://a.com/x**||", []string{`Bare "https://a.com/x" spoiler @4`}},
		{"||a\\||https://a.com/x||", []string{`Bare "https://a.com/x||" @6`}},
		{"*https://a.com/x*", []string{`Bare "https://a.com/x" @1`}},
		{"***https://a.com/x*", []string{`Bare "https://a.com/x" @3`}},
		{"*a ***https://a.com/x*", []string{`Bare "https://a.com/x*" @6`}},
		{"**a\\** https://b.com/y**", []string{`Bare "https://b.com/y" @7`}},
		{"**a`**`https://a.com/x`", nil},
		{"*https://a.com/x\\ *", []string{`Bare "https://a.com/x\\" @1`}},
		{"*a **https://a.com/x** b*", []string{`Bare "https://a.com/x" @5`}},
		{"_https://a.com/x_", []string{`Bare "https://a.com/x" @1`}},
		{"__https://a.com/x__", []string{`Bare "https://a.com/x" @2`}},
		{"_https://a.com/x_a", []string{`Bare "https://a.com/x_a" @1`}},
		{"~~https://a.com/x~~", []string{`Bare "https://a.com/x" @2`}},
		{"~~https://a.com/x~~_", []string{`Bare "https://a.com/x~~_" @2`}},

		// Masked links.
		{"[l](https://a.com/x)", []string{`Masked "https://a.com/x" label="l" @4`}},
		{"[l](<https://a.com/x>)", []string{`MaskedSuppressed "https://a.com/x" label="l" @5`}},
		{"[l](<https://a.com/x) z", []string{`Masked "https://a.com/x" label="l" @5`}},
		{"[l](https://a.com/x>)", []string{`Masked "https://a.com/x" label="l" @4`}},
		{"[l]( <https://a.com/x> 't' )", []string{`MaskedSuppressed "https://a.com/x" label="l" @6`}},
		{"[l](\nhttps://a.com/x\n)", []string{`Masked "https://a.com/x" label="l" @5`}},
		{"[](https://a.com/x)", []string{`Masked "https://a.com/x" @3`}},
		{"[l](https://a.com/(x))", []string{`Masked "https://a.com/(x)" label="l" @4`}},
		{"[l](https://a.com/x\\))", []string{`Masked "https://a.com/x\\)" label="l" @4`}},
		{`[l](https://a.com/x "t")`, []string{`Masked "https://a.com/x" label="l" @4`}},
		{"[a](b](https://a.com/x)", []string{`Masked "https://a.com/x" label="a](b" @7`}},
		{"[a [b] c](https://a.com/x)", []string{`Masked "https://a.com/x" label="a [b] c" @10`}},
		{`[l](a(b "t") https://c.com/d`, []string{`Bare "https://c.com/d" @13`}},
		{`[l]( "https://a.com b")`, nil},
		{"[l](javascript:x) https://b.com/y", []string{`Bare "https://b.com/y" @18`}},
		{"[https://a.com/x](https://b.com/y)", []string{`Bare "https://a.com/x](https://b.com/y)" @1`}},
		{"[a](https://a.com/x)](https://b.com/y)", []string{`Bare "https://a.com/x)](https://b.com/y" @4`}},
		{"||[l](https://a.com/x)||", []string{`Masked "https://a.com/x" spoiler label="l" @6`}},
	} {
		got := Find(c.in)
		var gs []string
		for _, l := range got {
			gs = append(gs, fmt.Sprintf("%s @%d", formatLink(l), l.Start))
			if c.in[l.Start:l.End] != l.URL {
				t.Errorf("Find(%q): %+v: content[Start:End] != URL", c.in, l)
			}
		}
		if strings.Join(gs, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("Find(%q)\n got %q\nwant %q", c.in, gs, c.want)
		}
	}
}

// TestFindNestingLimit checks the cap through a scan that starts at a given
// depth: at maxDepth paired rules are not tried, so closers stay inside the
// link, and a link in a capped span that contains "||" is spoilered.
func TestFindNestingLimit(t *testing.T) {
	scanAt := func(in string, depth int) []string {
		x := &index{s: in, nhttp: 1}
		x.prepare()
		sc := scanner{x: x}
		sc.scan(0, len(in), false, depth)
		var out []string
		for _, l := range sc.out {
			out = append(out, formatLink(l))
		}
		return out
	}
	for _, c := range []struct {
		in    string
		depth int
		want  string
	}{
		{"**https://a.com/x**", maxDepth - 1, `Bare "https://a.com/x"`},
		{"**https://a.com/x**", maxDepth, `Bare "https://a.com/x**"`},
		{"||https://a.com/x||", maxDepth - 1, `Bare "https://a.com/x" spoiler`},
		{"||https://a.com/x||", maxDepth, `Bare "https://a.com/x||" spoiler`},
		{"a|| https://a.com/x", maxDepth, `Bare "https://a.com/x" spoiler`},
		{"a| https://a.com/x |", maxDepth, `Bare "https://a.com/x"`},
		{"||a [l](https://a.com/x)", maxDepth, `Masked "https://a.com/x" spoiler label="l"`},
		{"`**` <https://a.com/x>", maxDepth, `Suppressed "https://a.com/x"`},
	} {
		if got := strings.Join(scanAt(c.in, c.depth), "\n"); got != c.want {
			t.Errorf("scan(%q, depth %d) = %q, want %q", c.in, c.depth, got, c.want)
		}
	}
	// '*' runs nest about half their length deep; the run stops nesting at
	// the cap and the link after it is still found.
	if links := Find(strings.Repeat("*", 200) + " https://a.com/x"); len(links) != 1 || links[0].Spoiler || links[0].URL != "https://a.com/x" {
		t.Fatalf("got %+v", links)
	}
}
