package linkclean

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/query"
)

// The semantics suite (§14.1) runs synthetic rules only
// (testdata/semantics/rules.json), so it never changes on a data refresh.

func semanticsSet(t testing.TB) RuleSet {
	t.Helper()
	data, err := os.ReadFile("testdata/semantics/rules.json")
	if err != nil {
		t.Fatal(err)
	}
	var s RuleSet
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func semanticsCases(t testing.TB) []goldenCase {
	files, err := filepath.Glob("testdata/semantics/*.txtar")
	if err != nil || len(files) == 0 {
		t.Fatalf("no semantics cases: %v", err)
	}
	var out []goldenCase
	for _, f := range files {
		out = append(out, readCases(t, f)...)
	}
	return out
}

func TestSemanticsFilesParse(t *testing.T) {
	s := semanticsSet(t)
	if _, err := New(Config{Sources: []RuleSet{s}}); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rules, %d cases", len(s.Rules), len(semanticsCases(t)))
}

func TestSemantics(t *testing.T) {
	set := []RuleSet{semanticsSet(t)}
	for _, c := range semanticsCases(t) {
		t.Run(c.ID, func(t *testing.T) {
			checkCase(t, &c, set, "semantics")
			for _, pw := range c.policies() {
				cl := caseCleaner(t, &c, pw.p, set, "semantics")
				if w := cl.Warnings(); len(w) > 0 {
					t.Errorf("warnings: %v", w)
				}
				checkResultShape(t, cl.Explain(c.In), pw.p)
			}
		})
	}
}

// checkResultShape asserts the §3/§3.1 properties every Result has: one
// category per change, Name/Value/Text consistent, beneficiaries only on
// affiliate changes, no affiliate change under AffiliateKeep, and nothing
// reported for a skipped input.
func checkResultShape(t testing.TB, r Result, p AffiliatePolicy) {
	t.Helper()
	if r.Skipped != NotSkipped && (r.URL != r.Input || r.Hops != nil || r.Removed != nil || r.Kept != nil) {
		t.Errorf("skipped result carries data: %+v", r)
	}
	for _, h := range r.Hops {
		switch h.Category {
		case CatRedirectUnwrapped, CatAffiliate:
		default:
			t.Errorf("hop category %d", h.Category)
		}
		if h.Category != CatAffiliate && h.Beneficiary != (Beneficiary{}) {
			t.Errorf("beneficiary on a %s hop", catName(h.Category))
		}
		if h.Category == CatAffiliate && p == AffiliateKeep {
			t.Errorf("affiliate hop under AffiliateKeep")
		}
	}
	for _, m := range r.Removed {
		switch m.Category {
		case CatTracker, CatAffiliate, CatPathSegment:
		default:
			t.Errorf("removal category %d", m.Category)
		}
		if (m.Category == CatPathSegment) != (m.Part == PartPath) && m.Part != PartNested {
			t.Errorf("removal %+v: path category iff path part", m)
		}
		if m.Category != CatAffiliate && m.Beneficiary != (Beneficiary{}) {
			t.Errorf("beneficiary on a %s removal", catName(m.Category))
		}
		if m.Category == CatAffiliate && p == AffiliateKeep {
			t.Errorf("affiliate removal under AffiliateKeep")
		}
		want := m.Name
		if strings.Contains(m.Text, "=") {
			want = m.Name + "=" + m.Value
		} else if m.Value != "" {
			t.Errorf("removal %+v: value without '='", m)
		}
		if want != m.Text {
			t.Errorf("removal %+v: Name/Value do not split Text", m)
		}
		if m.Part != PartNested && m.NestedIn != "" {
			t.Errorf("removal %+v: NestedIn outside PartNested", m)
		}
		if m.Rule == "" {
			t.Errorf("removal %+v: no rule", m)
		}
	}
}

// TestHostileIDs: an ID is passed through byte for byte after one decode
// (§3 Beneficiary; display safety is the consumer's).
func TestHostileIDs(t *testing.T) {
	c := MustNew(Config{Sources: []RuleSet{semanticsSet(t)}, Affiliate: AffiliateStrip})
	for raw, want := range map[string]string{
		"%3C%40everyone%3E": "<@everyone>",
		"%60x%60":           "`x`",
		"%2541":             "%41",
		"a%0Ab":             "a\nb",
		"%E2%80%AEevil-20":  "\u202eevil-20",
		"%FF%FE":            "\xff\xfe",
		"a+b":               "a+b",
		"%%zz":              "%%zz",
		"||s||":             "||s||",
	} {
		in := "https://shop.example.net/p?tag=" + raw
		r := c.Explain(in)
		if r.URL != "https://shop.example.net/p" || len(r.Removed) != 1 {
			t.Errorf("%s: %+v", in, r)
			continue
		}
		m := r.Removed[0]
		if m.Category != CatAffiliate || m.Beneficiary.ID != want || m.Value != raw || m.Beneficiary.Network != "Shop Net" {
			t.Errorf("%s: removal %+v, want ID %q", in, m, want)
		}
	}
}

// TestNestedAffiliateID: row E18 shape, the ID is decoded once relative to
// the nested URL.
func TestNestedAffiliateID(t *testing.T) {
	c := MustNew(Config{Sources: []RuleSet{semanticsSet(t)}, Affiliate: AffiliateStrip})
	in := "https://example.net/?u=https%3A%2F%2Fshop.example.net%2Fp%3Ftag%3Dfoo%252D20%26ref%3Dx"
	r := c.Explain(in)
	if r.URL != "https://example.net/?u=https%3A%2F%2Fshop.example.net%2Fp" || len(r.Removed) != 2 {
		t.Fatalf("%+v", r)
	}
	tag, ref := r.Removed[0], r.Removed[1]
	if tag.Part != PartNested || tag.NestedIn != "u" || tag.Category != CatAffiliate || tag.Value != "foo%2D20" || tag.Beneficiary.ID != "foo-20" {
		t.Errorf("tag removal %+v", tag)
	}
	if ref.Part != PartNested || ref.Category != CatTracker || ref.Beneficiary != (Beneficiary{}) {
		t.Errorf("ref removal %+v", ref)
	}
}

// chainRules builds n Extra Strip rules where rule i is in scope only while
// xk<i>x is the first query param, so each round enables the next (§4.3).
func chainRules(n int) []Rule {
	var rs []Rule
	for i := 0; i < n; i++ {
		rs = append(rs, Rule{
			ID: fmt.Sprintf("r%d", i), Action: Strip, Include: []string{"*://*/*"},
			URLRegexps: []string{fmt.Sprintf(`\?xk%dx=`, i)}, Params: []string{fmt.Sprintf("xk%dx", i)},
		})
	}
	return rs
}

func chainQuery(n int) string {
	var segs []string
	for i := 0; i < n; i++ {
		segs = append(segs, fmt.Sprintf("xk%dx=1", i))
	}
	return strings.Join(segs, "&")
}

func chainCleaner(t testing.TB, n int) *Cleaner {
	return MustNew(Config{Extra: chainRules(n), Sources: []RuleSet{semanticsSet(t)}})
}

// TestRoundCap pins D4: 7 chained rules converge in 8 rounds; 8 or more
// return the input with SkipRoundCap, even when the 8th round empties the
// query and the confirmation round would change nothing.
func TestRoundCap(t *testing.T) {
	c7 := chainCleaner(t, 7)
	for _, tail := range []string{"", "&keep=1"} {
		in := "https://example.net/a?" + chainQuery(7) + tail
		want := "https://example.net/a"
		if tail != "" {
			want += "?keep=1"
		}
		r := c7.Explain(in)
		if r.Skipped != NotSkipped || r.URL != want || len(r.Removed) != 7 {
			t.Errorf("n=7: %+v", r)
		}
	}
	for _, n := range []int{8, 9, 20} {
		c := chainCleaner(t, n)
		for _, tail := range []string{"", "&keep=1"} {
			in := "https://example.net/a?" + chainQuery(n) + tail
			r := c.Explain(in)
			if r.Skipped != SkipRoundCap || r.URL != in || r.Hops != nil || r.Removed != nil || r.Kept != nil || r.Changed {
				t.Errorf("n=%d%s: %+v", n, tail, r)
			}
			if c.Clean(in) != in || c.Clean(c.Clean(in)) != c.Clean(in) {
				t.Errorf("n=%d: Clean at the cap", n)
			}
		}
	}
}

func TestRoundCapAfterUnwrap(t *testing.T) {
	c := chainCleaner(t, 9)
	in := "https://wrap.example.org/?u=" + query.EncodeComponent("https://example.net/a?"+chainQuery(9))
	r := c.Explain(in)
	if r.Skipped != SkipRoundCap || r.URL != in || r.Hops != nil {
		t.Errorf("%+v", r)
	}
}

func TestRoundCapNested(t *testing.T) {
	c := chainCleaner(t, 9)
	inner := query.EncodeComponent("https://example.org/b?" + chainQuery(9))
	in := "https://example.net/p?utm_source=x&u=" + inner
	r := c.Explain(in)
	if want := "https://example.net/p?u=" + inner; r.Skipped != NotSkipped || r.URL != want {
		t.Errorf("got %+v, want URL %s", r, want)
	}
	if c.Clean(r.URL) != r.URL {
		t.Error("not idempotent")
	}
}

// TestNewRejects: New reports every problem as a *RuleError naming its set,
// rule and field, and never drops a rule silently.
func TestNewRejects(t *testing.T) {
	strip := func(f func(r *Rule)) []Rule {
		r := Rule{ID: "r", Action: Strip, Include: []string{"*://*/*"}, Params: []string{"p"}}
		f(&r)
		return []Rule{r}
	}
	for _, tc := range []struct {
		name               string
		cfg                Config
		set, ruleID, field string
	}{
		{"affiliate with path", Config{Extra: strip(func(r *Rule) { r.Affiliate, r.PathRegexps = true, []string{"/x"} })}, "user", "r", "affiliate"},
		{"idParam not in params", Config{Extra: strip(func(r *Rule) { r.Affiliate, r.IDParam = true, "tag" })}, "user", "r", "idParam"},
		{"idPath zero groups", Config{Extra: []Rule{{ID: "u", Action: Unwrap, Affiliate: true, IDPathRegexp: "^/c/", Include: []string{"*://*/*"}, Unwrap: &UnwrapSpec{QueryParams: []string{"u"}}}}}, "user", "u", "idPathRegexp"},
		{"idPath two groups", Config{Extra: []Rule{{ID: "u", Action: Unwrap, Affiliate: true, IDPathRegexp: "^/(c)/([0-9]+)", Include: []string{"*://*/*"}, Unwrap: &UnwrapSpec{QueryParams: []string{"u"}}}}}, "user", "u", "idPathRegexp"},
		{"invalid network", Config{Extra: strip(func(r *Rule) { r.Affiliate, r.Network = true, "**bold**" })}, "user", "r", "network"},
		{"no include", Config{Extra: strip(func(r *Rule) { r.Include = nil })}, "user", "r", "include"},
		{"bad pattern", Config{Extra: strip(func(r *Rule) { r.Include = []string{"*://ex*ample.com/*"} })}, "user", "r", "include[0]"},
		{"zero action", Config{Extra: strip(func(r *Rule) { r.Action = 0 })}, "user", "r", "action"},
		{"source label in brave", Config{Sources: []RuleSet{{Name: "brave/x", Rules: strip(func(r *Rule) { r.Include = []string{"*://*.amazon.*/*"} })}}}, "brave/x", "r", "include[0]"},
		{"reserved set name", Config{Sources: []RuleSet{{Name: "user", Rules: strip(func(*Rule) {})}}}, "user", "", "Sources[0].Name"},
		{"duplicate set name", Config{Sources: []RuleSet{{Name: "a"}, {Name: "a"}}}, "a", "", "Sources[1].Name"},
		{"empty set name", Config{Sources: []RuleSet{{}}}, "", "", "Sources[0].Name"},
		{"negative MaxUnwrap", Config{MaxUnwrap: -1}, "", "", "MaxUnwrap"},
		{"negative MaxURLLength", Config{MaxURLLength: -1}, "", "", "MaxURLLength"},
		{"bad archive pattern", Config{ExtraArchivePatterns: []string{"archive.example.org"}}, "", "", "ExtraArchivePatterns[0]"},
		{"empty signed param", Config{ExtraSignedParams: []string{""}}, "", "", "ExtraSignedParams[0]"},
		{"prefix without set", Config{DisableRulePrefixes: []string{"amazon-"}}, "", "", "DisableRulePrefixes[0]"},
		{"strict disable", Config{DisableRules: []string{"nope:x"}, StrictDisable: true}, "", "", "DisableRules[0]"},
		{"strict disable tag", Config{DisableTags: []string{"nope"}, StrictDisable: true}, "", "", "DisableTags[0]"},
		{"invalid policy", Config{Affiliate: 7}, "", "", "Affiliate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New(tc.cfg)
			if err == nil {
				t.Fatalf("accepted: %v", c.Warnings())
			}
			found := false
			for _, e := range err.(interface{ Unwrap() []error }).Unwrap() {
				var re *RuleError
				if errors.As(e, &re) && re.Set == tc.set && re.RuleID == tc.ruleID && re.Field == tc.field {
					found = true
				}
			}
			if !found {
				t.Errorf("want a *RuleError{%q, %q, %q}, have:\n%v", tc.set, tc.ruleID, tc.field, err)
			}
		})
	}
	_, err := New(Config{DisableRules: []string{"nope:x"}, StrictDisable: true})
	if !errors.Is(err, ErrUnknownRule) {
		t.Errorf("StrictDisable error %v is not ErrUnknownRule", err)
	}
}

func TestRuleErrorString(t *testing.T) {
	for _, tc := range []struct {
		e    RuleError
		want string
	}{
		{RuleError{Set: "brave/clean-urls", RuleID: "*://*.amazon.ae/*", Field: "exclude[2]", Err: errors.New("x")}, "linkclean: brave/clean-urls:*://*.amazon.ae/*: exclude[2]: x"},
		{RuleError{Set: "s", RuleID: "r", Err: errors.New("x")}, "linkclean: s:r: x"},
		{RuleError{Set: "brave/debounce", Field: "f", Err: errors.New("x")}, "linkclean: brave/debounce: f: x"},
		{RuleError{Field: "MaxUnwrap", Err: errors.New("x")}, "linkclean: MaxUnwrap: x"},
		{RuleError{Err: errors.New("x")}, "linkclean: x"},
	} {
		if got := tc.e.Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// TestNewDeepCopies: New neither keeps nor modifies caller slices.
func TestNewDeepCopies(t *testing.T) {
	extra := []Rule{{ID: "s", Action: Strip, Include: []string{"*://*/*"}, Params: []string{"Foo"}}}
	src := []RuleSet{semanticsSet(t)}
	disable := []string{"semantics:frag"}
	c := MustNew(Config{Extra: extra, Sources: src, DisableRules: disable})
	if extra[0].Params[0] != "Foo" || disable[0] != "semantics:frag" || src[0].Rules[0].Params[5] != "%66oo" {
		t.Error("New modified a caller slice")
	}
	extra[0].Params[0] = "bar"
	src[0].Rules[0].Params[0] = "nothing"
	disable[0] = "semantics:track"
	if got := c.Clean("https://example.net/?foo=1&utm_source=2&bar=3#xtor=1"); got != "https://example.net/?bar=3#xtor=1" {
		t.Errorf("caller mutation leaked into the Cleaner: %s", got)
	}
}

// Rounds walk the query segment by segment and keep one drop bit per
// segment: on the stack up to 2,048 segments, then in a per-call buffer.
// Padding a query with neutral params, short or past that bound, must leave
// what is removed, kept and unwrapped unchanged (PERF2-4).
func TestLongQueryWalk(t *testing.T) {
	for _, n := range []int{40, 2100} {
		testPaddedQuery(t, n)
	}
}

func testPaddedQuery(t *testing.T, n int) {
	var pad strings.Builder
	for i := range n {
		fmt.Fprintf(&pad, "&k%d=v", i)
	}
	c := MustNew(Config{Affiliate: AffiliateStrip, MaxURLLength: 1 << 16})
	for _, base := range []string{
		"https://example.com/a?id=1&utm_source=x&fbclid=y",
		"https://example.com/a?id=1&u=https%3A%2F%2Fexample.net%2F%3Futm_medium%3Dz%26q%3D1",
		"https://example.com/a?id=1&X-Amz-Signature=s&utm_source=x",
		"https://www.amazon.com/dp/B0ABC/ref=sr_1_3?id=1&tag=aff-20&crid=X",
		"https://example.com/a?id=1&utm_campaign=c#utm_source=x&utm_medium=y",
		"https://l.facebook.com/l.php?id=1&u=https%3A%2F%2Fexample.net%2Fa%3Ffbclid%3DIw%26id%3D1&h=AT0",
	} {
		short, long := c.Explain(base), c.Explain(strings.Replace(base, "id=1", "id=1"+pad.String(), 1))
		wantURL := short.URL
		if len(short.Hops) == 0 {
			wantURL = strings.Replace(short.URL, "id=1", "id=1"+pad.String(), 1)
		}
		if long.URL != wantURL || !reflect.DeepEqual(long.Removed, short.Removed) ||
			!reflect.DeepEqual(long.Kept, short.Kept) || len(long.Hops) != len(short.Hops) || long.Signed != short.Signed {
			t.Errorf("%s padded with %d params cleans differently:\n short %+v\n long  %+v", base, n, short, long)
		}
	}
}
