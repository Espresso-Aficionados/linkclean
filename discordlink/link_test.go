package discordlink

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

var kindNames = [...]string{Bare: "Bare", Suppressed: "Suppressed", Masked: "Masked", MaskedSuppressed: "MaskedSuppressed"}

// kindName is k's name in the testdata files.
func kindName(k Kind) string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("Kind(%d)", k)
}

func parseKind(s string) (Kind, bool) {
	for k, n := range kindNames {
		if n != "" && n == s {
			return Kind(k), true
		}
	}
	return 0, false
}

// formatLink renders l in the testdata link syntax, without offsets:
//
//	<Kind> <Go-quoted URL> [spoiler] [label=<Go-quoted>]
//
// label= is written iff Label is not empty.
func formatLink(l Link) string {
	s := kindName(l.Kind) + " " + strconv.Quote(l.URL)
	if l.Spoiler {
		s += " spoiler"
	}
	if l.Label != "" {
		s += " label=" + strconv.Quote(l.Label)
	}
	return s
}

// parseLink parses formatLink's syntax. The URL and the label may be string
// expressions (parseStrExpr). Start and End are left zero.
func parseLink(s string) (Link, error) {
	name, rest, _ := strings.Cut(s, " ")
	k, ok := parseKind(name)
	if !ok {
		return Link{}, fmt.Errorf("unknown kind %q", name)
	}
	l := Link{Kind: k}
	var err error
	if l.URL, rest, err = parseStrExpr(rest); err != nil {
		return Link{}, fmt.Errorf("URL: %v", err)
	}
	if r, ok := strings.CutPrefix(rest, "spoiler"); ok && (r == "" || r[0] == ' ') {
		l.Spoiler, rest = true, strings.TrimPrefix(r, " ")
	}
	if r, ok := strings.CutPrefix(rest, "label="); ok {
		if l.Label, rest, err = parseStrExpr(r); err != nil {
			return Link{}, fmt.Errorf("label: %v", err)
		}
		if l.Label == "" {
			return Link{}, fmt.Errorf("empty label= (omit it)")
		}
	}
	if rest != "" {
		return Link{}, fmt.Errorf("trailing %q", rest)
	}
	return l, nil
}

func TestWouldEmbed(t *testing.T) {
	for _, c := range []struct {
		k    Kind
		want bool
	}{{Bare, true}, {Suppressed, false}, {Masked, true}, {MaskedSuppressed, false}} {
		for _, sp := range []bool{false, true} {
			if got := (Link{Kind: c.k, Spoiler: sp}).WouldEmbed(); got != c.want {
				t.Errorf("%s spoiler=%v: WouldEmbed = %v, want %v", kindName(c.k), sp, got, c.want)
			}
		}
	}
}

func TestKindValues(t *testing.T) {
	// Kind's values are part of the API: the zero Kind is never reported.
	if Bare != 1 || Suppressed != 2 || Masked != 3 || MaskedSuppressed != 4 {
		t.Fatal("Kind values changed")
	}
}

func TestLinkSyntax(t *testing.T) {
	for _, l := range []Link{
		{Kind: Bare, URL: "https://a.com/x"},
		{Kind: Suppressed, URL: "https://a.com/x\ny", Spoiler: true},
		{Kind: Masked, URL: "https://a.com/x", Label: "a](b"},
		{Kind: MaskedSuppressed, URL: "https://a.com/ spoiler", Spoiler: true, Label: `q"`},
	} {
		s := formatLink(l)
		got, err := parseLink(s)
		if err != nil || got != l {
			t.Errorf("parseLink(%s) = %+v, %v; want %+v", s, got, err, l)
		}
	}
	if l, err := parseLink(`Bare "https://a.com/" + "x"*3`); err != nil || l.URL != "https://a.com/xxx" {
		t.Errorf("string expression: %+v, %v", l, err)
	}
	for _, s := range []string{`Bogus "u"`, `Bare u`, `Bare "u" spoilerx`, `Bare "u" label=""`, `Bare "u" label="l" spoiler`, `Bare "u" x`} {
		if _, err := parseLink(s); err == nil {
			t.Errorf("parseLink(%s): no error", s)
		}
	}
}
