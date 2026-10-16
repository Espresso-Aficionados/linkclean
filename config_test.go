package linkclean

import (
	"errors"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// Config semantics of §14.1 and §9 against the built-in rules.

func TestDisableRulesExact(t *testing.T) {
	const amazon = "https://www.amazon.com/dp/B0?ref_=nav"
	if Clean(amazon) == amazon {
		t.Fatal("control: Brave's Amazon rule does not strip ref_")
	}
	c := MustNew(Config{DisableRules: []string{"brave/clean-urls:*://*.amazon.ae/*"}})
	if w := c.Warnings(); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
	if got := c.Clean(amazon); got != amazon {
		t.Errorf("rule not disabled: %s", got)
	}
	if got := c.Clean("https://example.com/?ck_subscriber_id=1"); got != "https://example.com/" {
		t.Errorf("other rules disabled too: %s", got)
	}
}

func TestDisableRulesStarIsLiteral(t *testing.T) {
	c := MustNew(Config{DisableRules: []string{"brave/clean-urls:*://*/*"}})
	w := c.Warnings()
	if len(w) != 1 || w[0].Field != "DisableRules" || w[0].Entry != "brave/clean-urls:*://*/*" || !errors.Is(w[0].Err, ErrUnknownRule) {
		t.Errorf("warnings: %v", w)
	}
	if got := c.Clean("https://example.com/?ck_subscriber_id=1&h_sid=2"); got != "https://example.com/" {
		t.Errorf("a global rule was disabled: %s", got)
	}
	if !strings.Contains(w[0].Error(), "brave/clean-urls:*://*/*") {
		t.Errorf("Warning.Error() = %q", w[0].Error())
	}
}

func TestDisableRulePrefixes(t *testing.T) {
	c := MustNew(Config{DisableRulePrefixes: []string{"brave/clean-urls:*://*/*", "linkclean/nope:"}})
	in := "https://example.com/?ck_subscriber_id=1&h_sid=2"
	if got := c.Clean(in); got != in {
		t.Errorf("both global clean-urls rules should be off: %s", got)
	}
	w := c.Warnings()
	if len(w) != 1 || w[0].Field != "DisableRulePrefixes" || w[0].Entry != "linkclean/nope:" || !errors.Is(w[0].Err, ErrUnknownRule) {
		t.Errorf("warnings: %v", w)
	}
}

func TestDisableTags(t *testing.T) {
	in := "https://www.shareasale.com/r.cfm?b=1&u=2&m=3&urllink=www.example.com%2Fproduct&afftrack="
	if got := MustNew(Config{Affiliate: AffiliateStrip}).Clean(in); got == in {
		t.Fatal("control: debounce does not unwrap row 85 under AffiliateStrip")
	}
	for _, cfg := range []Config{
		{Affiliate: AffiliateStrip, DisableTags: []string{"debounce"}},
		{Affiliate: AffiliateStrip, DisableRulePrefixes: []string{"brave/debounce:"}},
	} {
		c := MustNew(cfg)
		if got := c.Clean(in); got != in {
			t.Errorf("%+v: %s", cfg, got)
		}
		if len(c.Warnings()) != 0 {
			t.Errorf("%+v: warnings %v", cfg, c.Warnings())
		}
	}
	c := MustNew(Config{DisableTags: []string{"lowconf", "no-such-tag"}})
	if got := c.Clean("https://www.wsj.com/articles/x?st=abc123"); got != "https://www.wsj.com/articles/x" {
		t.Errorf("lowconf off: %s", got)
	}
	w := c.Warnings()
	if len(w) != 1 || w[0].Field != "DisableTags" || !errors.Is(w[0].Err, ErrUnknownTag) {
		t.Errorf("warnings: %v", w)
	}
	w[0].Entry = "mutated"
	if c.Warnings()[0].Entry != "no-such-tag" {
		t.Error("Warnings is not a copy")
	}
}

func TestOptOutAmazonLinkCode(t *testing.T) {
	// §4.4 "Opting out": strip linkCode/linkId while keeping tag.
	c := MustNew(Config{
		DisableRules: []string{"linkclean/overlay:amazon-affiliate-keep"},
		Extra:        []Rule{{ID: "keep-tag", Action: Keep, Include: []string{"*://*.amazon.*/*"}, Params: []string{"tag"}}},
	})
	got := c.Clean("https://www.amazon.com/dp/B0?linkCode=ll1&tag=x-20&linkId=abc")
	if got != "https://www.amazon.com/dp/B0?tag=x-20" {
		t.Errorf("got %s", got)
	}
}

func TestSourcesAndNotices(t *testing.T) {
	c := MustNew(Config{Extra: []Rule{{ID: "x", Action: Strip, Include: []string{"*://*/*"}, Params: []string{"x"}}}})
	s := c.Sources()
	names := []string{}
	for _, si := range s {
		names = append(names, si.Name)
	}
	if strings.Join(names, ",") != "user,brave/clean-urls,brave/query-filter,brave/debounce,linkclean/overlay" {
		t.Fatalf("Sources: %v", names)
	}
	lock, err := readLock(rulesFS)
	if err != nil {
		t.Fatal(err)
	}
	cu := s[1]
	if cu.License != "MPL-2.0" || cu.Commit != lock.Upstream.Commit || cu.SHA256 != lock.Upstream.Files["brave-lists/clean-urls.json"].SHA256 ||
		cu.Rules != 51 || cu.Params != 425 || cu.Upstream != "github.com/brave/adblock-lists brave-lists/clean-urls.json" {
		t.Errorf("clean-urls source: %+v", cu)
	}
	if s[3].Rules != 49 || s[4].License != "MIT" || s[4].Regexps == 0 || s[0].Rules != 1 {
		t.Errorf("sources: %+v", s)
	}
	s[0].Name = "mutated"
	if c.Sources()[0].Name != "user" {
		t.Error("Sources is not a copy")
	}
	n := c.Notices()
	for _, want := range []string{lock.Upstream.Commit, "rules/overlay/debounce-classes.json", "MPL-2.0", "MIT", "Source: https://github.com/brave/adblock-lists/tree/"} {
		if !strings.Contains(n, want) {
			t.Errorf("Notices lacks %q", want)
		}
	}
}

func TestDefault(t *testing.T) {
	if a, b := Default(), Default(); a != b {
		t.Error("Default is not shared")
	}
	in := "https://example.com/?utm_source=x&id=7"
	if Clean(in) != "https://example.com/?id=7" || Default().Clean(in) != Clean(in) {
		t.Error("Clean")
	}
	if w := Default().Warnings(); len(w) != 0 {
		t.Errorf("default warnings: %v", w)
	}
}

func TestMaxUnwrapClamp(t *testing.T) {
	// 11 nested Google /url wrappers: MaxUnwrap 20 is clamped to 10.
	in := "https://example.net/a"
	for i := 0; i < 11; i++ {
		in = "https://www.google.com/url?q=" + strings.NewReplacer(":", "%3A", "/", "%2F", "?", "%3F", "=", "%3D", "%", "%25", "&", "%26").Replace(in)
	}
	r := MustNew(Config{MaxUnwrap: 20}).Explain(in)
	if !r.UnwrapAbandoned {
		t.Errorf("11 hops with MaxUnwrap 20 (clamped to 10) not abandoned: %d hops", len(r.Hops))
	}
}

func TestTextMarshaling(t *testing.T) {
	for a := Strip; a <= Unwrap; a++ {
		b, err := a.MarshalText()
		var back Action
		if err != nil || back.UnmarshalText(b) != nil || back != a {
			t.Errorf("Action %d: %q %v -> %d", a, b, err, back)
		}
	}
	for c := CatTracker; c <= CatUnshortened; c++ {
		b, err := c.MarshalText()
		var back Category
		if err != nil || back.UnmarshalText(b) != nil || back != c {
			t.Errorf("Category %d: %q %v -> %d", c, b, err, back)
		}
	}
	for _, a := range []Action{0, Unwrap + 1} {
		if _, err := a.MarshalText(); err == nil {
			t.Errorf("Action(%d).MarshalText: no error", a)
		}
	}
	for _, c := range []Category{0, CatUnshortened + 1} {
		if _, err := c.MarshalText(); err == nil {
			t.Errorf("Category(%d).MarshalText: no error", c)
		}
	}
	var a Action
	var c Category
	for _, s := range []string{"", "Strip", "redirect"} {
		if a.UnmarshalText([]byte(s)) == nil {
			t.Errorf("Action.UnmarshalText(%q): no error", s)
		}
	}
	for _, s := range []string{"", "Tracker", "strip"} {
		if c.UnmarshalText([]byte(s)) == nil {
			t.Errorf("Category.UnmarshalText(%q): no error", s)
		}
	}
}

func TestMustNewPanics(t *testing.T) {
	defer func() {
		err, _ := recover().(error)
		var re *RuleError
		if !errors.As(err, &re) || re.Field != "MaxUnwrap" {
			t.Errorf("recovered %v, want a *RuleError for MaxUnwrap", err)
		}
	}()
	MustNew(Config{MaxUnwrap: -1})
	t.Error("MustNew did not panic")
}

// Q2: errors from the embedded data path arrive joined twice (compiler, then
// readDefaults); every leaf must still be a *RuleError.
func TestConvertErrsNested(t *testing.T) {
	in := errors.Join(errors.Join(&rule.Error{Set: "s", RuleID: "r", Field: "f", Err: errors.New("x")}), errors.New("plain"))
	out := convertErrs(in)
	if len(out) != 2 {
		t.Fatalf("got %d errors, want 2: %v", len(out), out)
	}
	var re *RuleError
	if !errors.As(out[0], &re) || re.Set != "s" || re.RuleID != "r" || re.Field != "f" {
		t.Errorf("leaf 0 = %#v, want *RuleError s:r f", out[0])
	}
	if got, want := out[0].Error(), "linkclean: s:r: f: x"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
