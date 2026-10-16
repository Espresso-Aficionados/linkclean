package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

func strip(id string, params ...string) rule.Rule {
	return rule.Rule{ID: id, Action: rule.Strip, Include: []string{"*://*/*"}, Params: params}
}

// TestCompileErrors pins every New rejection of §3 and §4.4.1, each reported
// with its set, rule ID and field.
func TestCompileErrors(t *testing.T) {
	mod := func(f func(r *rule.Rule)) rule.Rule {
		r := strip("r", "p")
		f(&r)
		return r
	}
	unwrap := func(f func(r *rule.Rule)) rule.Rule {
		r := rule.Rule{ID: "r", Action: rule.Unwrap, Include: []string{"*://*.example.com/*"},
			Unwrap: &rule.UnwrapSpec{QueryParams: []string{"u"}}}
		f(&r)
		return r
	}
	for _, tc := range []struct {
		name  string
		set   string
		rules []rule.Rule
		field string
		msg   string
	}{
		{"no id", "s", []rule.Rule{mod(func(r *rule.Rule) { r.ID = "" })}, "id", "missing"},
		{"dup id", "s", []rule.Rule{strip("a", "p"), strip("a", "q")}, "id", "duplicate"},
		{"bad action", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Action = 9 })}, "action", "invalid"},
		{"no include", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Include = nil })}, "include", "required"},
		{"bad include", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Include = []string{"*://*/*", "<all_urls>"} })}, "include[1]", "all_urls"},
		{"bad exclude", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Exclude = []string{"||example.com^"} })}, "exclude[0]", ""},
		{"label in brave", "brave/x", []rule.Rule{mod(func(r *rule.Rule) { r.Include = []string{"*://*.amazon.*/*"} })}, "include[0]", "label"},
		{"bad urlRegexp", "s", []rule.Rule{mod(func(r *rule.Rule) { r.URLRegexps = []string{"("} })}, "urlRegexps[0]", ""},
		{"bad paramRegexp", "s", []rule.Rule{mod(func(r *rule.Rule) { r.ParamRegexps = []string{"["} })}, "paramRegexps[0]", ""},
		{"empty param", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Params = []string{""} })}, "params[0]", "empty"},
		{"strip nothing", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Params = nil })}, "params", "needs"},
		{"keep nothing", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Action, r.Params = rule.Keep, nil })}, "params", "needs"},
		{"path on keep", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Action, r.PathRegexps = rule.Keep, []string{"/ref=.*"} })}, "pathRegexps", "only Strip"},
		{"path matches empty", "s", []rule.Rule{mod(func(r *rule.Rule) { r.PathRegexps = []string{"x*"} })}, "pathRegexps[0]", "empty string"},
		{"params on unwrap", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Params = []string{"p"} })}, "params", "only Strip and Keep"},
		{"inFragment on unwrap", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.InFragment = true })}, "inFragment", ""},
		{"unwrap without spec", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Unwrap = nil })}, "unwrap", "needs"},
		{"spec on strip", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Unwrap = &rule.UnwrapSpec{QueryParams: []string{"u"}} })}, "unwrap", "only Unwrap"},
		{"no queryParams", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Unwrap.QueryParams = nil })}, "unwrap.queryParams", "required"},
		{"bad prependScheme", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Unwrap.PrependScheme = "ftp" })}, "unwrap.prependScheme", ""},
		{"network without affiliate", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Network = "X" })}, "network", "only affiliate"},
		{"affiliate with path", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Affiliate, r.PathRegexps = true, []string{"/x"} })}, "affiliate", "pathRegexps"},
		{"bad network", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Affiliate, r.Network = true, "<@everyone>" })}, "network", ""},
		{"long network", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Affiliate, r.Network = true, strings.Repeat("a", 41) })}, "network", ""},
		{"idParam not a param", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Affiliate, r.IDParam = true, "tag" })}, "idParam", "not one of params"},
		{"idPath on strip", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Affiliate, r.IDPathRegexp = true, "^/c/([0-9]+)/" })}, "idPathRegexp", "only Unwrap"},
		{"idPath zero groups", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Affiliate, r.IDPathRegexp = true, "^/c/[0-9]+/" })}, "idPathRegexp", "0 capture groups"},
		{"idPath two groups", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Affiliate, r.IDPathRegexp = true, "^/(c)/([0-9]+)/" })}, "idPathRegexp", "2 capture groups"},
		{"idParam and idPath", "s", []rule.Rule{unwrap(func(r *rule.Rule) { r.Affiliate, r.IDParam, r.IDPathRegexp = true, "id", "^/c/([0-9]+)/" })}, "idPathRegexp", "exclusive"},
		{"empty tag", "s", []rule.Rule{mod(func(r *rule.Rule) { r.Tags = []string{"a", ""} })}, "tags[1]", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile([]rule.Set{{Name: tc.set, Rules: tc.rules}})
			if err == nil {
				t.Fatal("accepted")
			}
			var re *rule.Error
			if !errors.As(err, &re) {
				t.Fatalf("%v is not a *rule.Error", err)
			}
			found := false
			for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
				re := e.(*rule.Error)
				if re.Set == tc.set && re.Field == tc.field && strings.Contains(re.Error(), tc.msg) {
					found = true
				}
			}
			if !found {
				t.Errorf("want an error on %s.%s containing %q, have:\n%v", tc.set, tc.field, tc.msg, err)
			}
		})
	}
}

func TestCompileCollectsAll(t *testing.T) {
	_, err := Compile([]rule.Set{{Name: "s", Rules: []rule.Rule{
		{ID: "a", Action: rule.Strip},
		{ID: "b", Action: rule.Keep, Include: []string{"x"}},
	}}})
	if n := len(err.(interface{ Unwrap() []error }).Unwrap()); n < 4 {
		t.Errorf("%d errors, want every problem of both rules:\n%v", n, err)
	}
}

func TestCompileValid(t *testing.T) {
	p, err := Compile([]rule.Set{
		{Name: "a", Rules: []rule.Rule{
			strip("s", "UTM_Source", "%66bclid"),
			{ID: "k", Action: rule.Keep, Affiliate: true, Network: "Amazon Associates", IDParam: "TAG",
				Include: []string{"*://*.amazon.*/*"}, Params: []string{"tag"}},
			{ID: "u", Action: rule.Unwrap, Affiliate: true, Network: "Impact", IDPathRegexp: "^/c/([0-9]+)/",
				Include: []string{"*://*.sjv.io/c/*"}, Unwrap: &rule.UnwrapSpec{QueryParams: []string{"U"}, PrependScheme: "https"}},
		}},
		{Name: "b", Rules: []rule.Rule{strip("s", "fbclid")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.rules) != 4 {
		t.Fatalf("%d rules", len(p.rules))
	}
	if got := p.params["fbclid"]; len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Errorf("params[fbclid] = %v, want [0 3] (canonical, ascending)", got)
	}
	if got := p.params["utm_source"]; len(got) != 1 {
		t.Errorf("params[utm_source] = %v", got)
	}
	if r := p.rules[1]; r.idParam != "tag" || !r.needSite || r.id != "a:k" {
		t.Errorf("keep rule compiled as %+v", r)
	}
	if r := p.rules[2]; r.unwrapParams[0] != "u" || r.idPath == nil || r.prependScheme != "https" {
		t.Errorf("unwrap rule compiled as %+v", r)
	}
	if s := p.Sets(); len(s) != 2 || s[0].Rules != 3 || s[0].Params != 3 || s[0].Regexps != 1 {
		t.Errorf("Sets() = %+v", s)
	}
}

func TestNewCleanerDisable(t *testing.T) {
	p, err := Compile([]rule.Set{{Name: "s", Rules: []rule.Rule{
		strip("x:1", "a"),
		{ID: "x:2", Action: rule.Strip, Include: []string{"*://*/*"}, Params: []string{"b"}, Tags: []string{"t"}},
		{ID: "aff", Action: rule.Strip, Affiliate: true, Include: []string{"*://*/*"}, Params: []string{"c"}},
		{ID: "affkeep", Action: rule.Keep, Affiliate: true, Include: []string{"*://*/*"}, Params: []string{"c"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	c, un := NewCleaner([]*Program{p}, Options{
		DisableRules:        []string{"s:x:1", "s:x"},
		DisableRulePrefixes: []string{"s:x:", "nope:"},
		DisableTags:         []string{"t", "u"},
		MaxUnwrap:           5, MaxURLLength: 8192,
	})
	if c.isActive(0) || c.isActive(1) || c.isActive(2) || !c.isActive(3) {
		t.Errorf("active = %b", c.active[0])
	}
	want := []Unmatched{{"DisableRules", 1}, {"DisableRulePrefixes", 1}, {"DisableTags", 1}}
	if len(un) != len(want) {
		t.Fatalf("unmatched %v, want %v", un, want)
	}
	for i := range want {
		if un[i] != want[i] {
			t.Errorf("unmatched %v, want %v", un, want)
		}
	}
	c, _ = NewCleaner([]*Program{p}, Options{AffiliateStrip: true, MaxUnwrap: 5, MaxURLLength: 8192})
	if !c.isActive(2) || c.isActive(3) {
		t.Errorf("AffiliateStrip: active = %b", c.active[0])
	}
}
