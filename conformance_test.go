package linkclean

import (
	"testing"
)

// Documented deviations from brave-core (design §4.6). Each test asserts
// linkclean's behaviour on the built-in rules and states, in its comment,
// what brave-core does and which part of brave-core it mirrors. No
// brave-core code or test data is copied; the vectors are our own.
//
// The Chromium URLPattern vectors (BSD-3) are in
// internal/matchpattern/conformance_test.go, against the matcher directly.

type dvCase struct {
	in, keep, strip string // strip "" means as keep; "unchanged" means the input
	cfg             Config
}

func runDV(t *testing.T, cases []dvCase) {
	t.Helper()
	for _, tc := range cases {
		for _, p := range []AffiliatePolicy{AffiliateKeep, AffiliateStrip} {
			want := tc.keep
			if p == AffiliateStrip && tc.strip != "" {
				want = tc.strip
			}
			if want == "unchanged" {
				want = tc.in
			}
			cfg := tc.cfg
			cfg.Affiliate = p
			c := MustNew(cfg)
			got := c.Clean(tc.in)
			if got != want {
				t.Errorf("policy %d:\n  in   %s\n  got  %s\n  want %s", p, tc.in, got, want)
			}
			checkInvariants(t, c, tc.in, got)
		}
	}
}

// DV-1. brave-core applies query-filter.json through url_pattern_index (the
// adblock engine's substring/anchor matcher over the whole URL spec), so
// "*://*.youtube.com/*" also fires on a foreign host whose query mentions
// youtube.com, and strips si there. linkclean matches both strip files with
// the URLPattern subset on the parsed host and path (golden rows 77-79).
func TestDeviation_DV1(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://evil.example/page?si=SHARE123&u=https://www.youtube.com/watch", keep: "unchanged"},
		{in: "https://evil.example/page?igsh=X#https://www.instagram.com/x", keep: "unchanged"},
		{in: "https://www.youtube.com@evil.example/watch?v=dQw4w9WgXcQ&si=Xk3v", keep: "unchanged"},
		{in: "https://www.youtube.com/watch?v=abc&si=XYZ&t=42", keep: "https://www.youtube.com/watch?v=abc&t=42"},
	})
}

// DV-2. brave-core compares parameter names exactly and case-sensitively on
// the raw keys (components/query_filter). linkclean compares canon(name):
// %XX decoded once, ASCII-lowercased (rows 6, 7, 99; corpus negatives-054).
func TestDeviation_DV2(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/?UTM_SOURCE=x&id=7", keep: "https://example.com/?id=7"},
		{in: "https://example.com/?%75tm_source=x&id=7", keep: "https://example.com/?id=7"},
		{in: "https://example.com/article?utm%5Fsource=newsletter&id=9", keep: "https://example.com/article?id=9"},
		{in: "https://www.facebook.com/x?__cft__%5B0%5D=abc&id=1", keep: "https://www.facebook.com/x?id=1"},
	})
}

// DV-3. brave-core strips a listed parameter only when it has a non-empty
// value. linkclean strips "k", "k=" and "k=v" alike (rows 4, 5).
func TestDeviation_DV3(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/?utm_source=", keep: "https://example.com/"},
		{in: "https://example.com/?utm_source", keep: "https://example.com/"},
		{in: "https://example.com/?fbclid&a=1", keep: "https://example.com/?a=1"},
	})
}

// DV-4. brave-core mutates the query rule by rule and mis-parses "=k=v".
// linkclean decides each segment once per round, iterated to a fixed point;
// "=k=v" has an empty name and is never stripped (rows 12, 13).
func TestDeviation_DV4(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/?=utm_source=x&a=1", keep: "unchanged"},
		{in: "https://example.com/?=utm_source&a=1", keep: "unchanged"},
		{in: "https://example.com/?utm_source==x&a=1", keep: "https://example.com/?a=1"},
	})
}

// DV-5. brave-core ignores an exclude pattern it cannot parse, so the rule
// applies everywhere its include does (fail open). linkclean fails New, and
// so the rules-compile gate (§4.5.2).
func TestDeviation_DV5(t *testing.T) {
	for _, ex := range []string{"||example.com^", "*://exa*mple.com/*", "<all_urls>", "file:///*"} {
		_, err := New(Config{Sources: []RuleSet{{Name: "brave/test", Rules: []Rule{{
			ID: "r", Action: Strip, Include: []string{"*://*/*"}, Exclude: []string{ex}, Params: []string{"p"},
		}}}}})
		if err == nil {
			t.Errorf("exclude %q accepted", ex)
		}
	}
}

// DV-6. brave-core matches against the GURL-canonicalized URL: dot segments
// resolved, '\' turned into '/' in the path, the host IDNA- and
// percent-decoded. linkclean canonicalizes only the host view; dot segments
// stay (so an exclude written for the canonical path does not apply), and
// '\' in the authority or any control byte makes the input ambiguous.
func TestDeviation_DV6(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://dev-pages.bravesoftware.com/clean-urls/exempted/?brave_testing1=foo", keep: "unchanged"},
		{
			in:   "https://dev-pages.bravesoftware.com/clean-urls/x/../exempted/?brave_testing1=foo",
			keep: "https://dev-pages.bravesoftware.com/clean-urls/x/../exempted/",
		},
		{in: "https://example.com/a/../b?utm_source=x", keep: "https://example.com/a/../b"},
		{in: `https://www.youtube.com\@evil.example/watch?v=x&si=Y`, keep: "unchanged"},
		{in: "https://example.com/a b?utm_source=x", keep: "unchanged"},
	})
}

// DV-7. brave-core's SanitizeURL drops a bare or emptied '?' (ClearQuery).
// linkclean returns unchanged input byte-identical, bare '?' included, and
// drops '?' only when it removed every segment (rows 3, 18, 112).
func TestDeviation_DV7(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/?", keep: "unchanged"},
		{in: "https://example.com/page?", keep: "unchanged"},
		{in: "https://example.com/?utm_source=x", keep: "https://example.com/"},
	})
}

// DV-8. brave-core keeps ck_subscriber_id, h_sid/h_slt and mkt_tok on
// unsubscribe and email pages through kConditionalQueryStringTrackers in
// components/query_filter/browser/utils.cc, code applied only by the
// navigation query filter. linkclean expresses them as overlay Keep rules
// with URLRegexps (C-1..C-3), which beat strips from every source.
func TestDeviation_DV8(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/unsubscribe?mkt_tok=abc&x=1", keep: "unchanged"},
		{in: "https://example.com/emailWebview?mkt_tok=abc&utm_source=x", keep: "https://example.com/emailWebview?mkt_tok=abc"},
		{in: "https://example.com/email/view?h_sid=1&x=1", keep: "unchanged"},
		{in: "https://example.com/unsubscribe?ck_subscriber_id=1", keep: "unchanged"},
		{in: "https://example.com/?ck_subscriber_id=1&x=1", keep: "https://example.com/?x=1"},
		{in: "https://example.com/?mkt_tok=abc&x=1", keep: "https://example.com/?x=1"},
	})
}

// DV-9. brave-core exempts urldefense.com through kExemptedHostnames, code in
// components/query_filter/browser/utils.cc. linkclean puts urldefense hosts
// in the archive guard, which returns the input unchanged (row 82).
func TestDeviation_DV9(t *testing.T) {
	c := Default()
	for _, in := range []string{
		"https://urldefense.com/v3/__https://example.com/?utm_source=x__;!!AbC$",
		"https://urldefense.proofpoint.com/v2/url?u=https-3A__example.com_&d=DwM&utm_source=x",
	} {
		if r := c.Explain(in); r.URL != in || r.Skipped != SkipArchive {
			t.Errorf("%s: %+v", in, r)
		}
	}
}

// DV-10. brave-core tests kConditionalQueryStringTrackers against the
// original spec, before any stripping, so a trigger inside a parameter it
// strips in the same pass still protects. linkclean evaluates Keep
// URLRegexps against the current round's URL, with nested re-encoding inside
// the round, so a trigger that lives only in a stripped or re-encoded value
// does not protect (rows 121, 126, 127; idempotence).
func TestDeviation_DV10(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://example.com/landing?utm_campaign=unsubscribe-flow&mkt_tok=abc", keep: "https://example.com/landing"},
		{
			in:   "https://example.com/?ck_subscriber_id=1&u=https://example.net/unsubscribe?utm_source=x",
			keep: "https://example.com/?u=https%3A%2F%2Fexample.net%2Funsubscribe",
		},
		{
			in:   "https://example.com/?h_sid=1&next=https://example.net/email/view?fbclid=x",
			keep: "https://example.com/?next=https%3A%2F%2Fexample.net%2Femail%2Fview",
		},
	})
}

// DV-11. GURL percent-decodes the host before matching, so
// web.archiv%65.org is web.archive.org to brave-core. linkclean treats '%' in
// the host as ambiguous and returns the input unchanged, so no host-scoped
// guard or Keep can be bypassed by an escaped host (rows 128, 129).
func TestDeviation_DV11(t *testing.T) {
	c := Default()
	for _, in := range []string{
		"https://web.archiv%65.org/web/2024/https://example.com/?utm_source=x",
		"https://b%C3%BCcher.example/?utm_source=x",
	} {
		if r := c.Explain(in); r.URL != in || r.Skipped != SkipAmbiguous {
			t.Errorf("%s: %+v", in, r)
		}
	}
}

// DV-12. brave-core's debounce extracts the destination with
// GetValueForKeyInQuery, which unescapes with REPLACE_PLUS_WITH_SPACE
// (components/debounce/core/browser/debounce_rule.cc). linkclean decodes %XX
// once and keeps '+' literal, for debounce and overlay unwrappers alike
// (row 88).
func TestDeviation_DV12(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://l.facebook.com/l.php?u=https%3A%2F%2Fexample.net%2Fa+b%3Fx%3D1&h=AT0", keep: "https://example.net/a+b?x=1"},
		{
			in:    "https://www.awin1.com/cread.php?awinmid=1&awinaffid=2&ued=https%3A%2F%2Fwww.example.com%2FC++%2F",
			keep:  "unchanged",
			strip: "https://www.example.com/C++/",
		},
	})
}

// DV-13. brave-core treats a prepend_scheme rule as erroneous, and does not
// apply it, when the extracted value already parses as a URL
// (debounce_rule.cc). linkclean prepends the scheme only when the decoded
// value lacks http:// or https://, so ShareASale links unwrap in both forms
// (rows 85, 145).
func TestDeviation_DV13(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://shareasale.com/r.cfm?u=2&m=3&urllink=https%3A%2F%2Fwww.example.com%2Fx", keep: "unchanged", strip: "https://www.example.com/x"},
		{in: "https://www.shareasale.com/r.cfm?b=1&u=2&m=3&urllink=www.example.com%2Fproduct&afftrack=", keep: "unchanged", strip: "https://www.example.com/product"},
	})
}

// DV-14. brave-core matches the debounce param exactly and case-sensitively
// against raw query keys (debounce_rule.cc). linkclean matches canon(name),
// as for every other parameter.
func TestDeviation_DV14(t *testing.T) {
	runDV(t, []dvCase{
		{in: "https://www.shareasale.com/r.cfm?u=2&URLLINK=www.example.com%2Fp", keep: "unchanged", strip: "https://www.example.com/p"},
		{in: "https://www.awin1.com/cread.php?awinaffid=2&%75ed=https%3A%2F%2Fwww.example.com%2Fp", keep: "unchanged", strip: "https://www.example.com/p"},
	})
}

// Brave-behaviour vectors that are not deviations: the same results as
// brave-core on its own data. Each mirrors a behaviour of
// components/query_filter or components/debounce.
func TestBraveBehaviour(t *testing.T) {
	runDV(t, []dvCase{
		// Apex match: "*://*.x.com/*" matches x.com itself (row 26).
		{in: "https://x.com/user/status/1?s=20&t=abc", keep: "https://x.com/user/status/1"},
		// Per-rule exclude (row 59) and a query that is part of the path glob (row 60).
		{in: "https://dev-pages.bravesoftware.com/clean-urls/exempted/?brave_testing1=foo&utm_content=x", keep: "https://dev-pages.bravesoftware.com/clean-urls/exempted/?brave_testing1=foo"},
		{in: "https://dev-pages.bravesoftware.com/clean-urls?brave_testing1=foo", keep: "unchanged"},
		// Debounce same-site safeguard (IsSameETLDForDebounce): a Nordstrom
		// Linkshare redirect to nordstrom.com stays wrapped (§4.4.1).
		{in: "https://www.nordstrom.com/Linkshare?url=https%3A%2F%2Fshop.nordstrom.com%2Fx", keep: "unchanged"},
		// Debounce exact-path glob without '*': inert on real links (rows 147, 148).
		{in: "https://www.tkqlhce.com/click-1234567-13579?url=https%3A%2F%2Fwww.example.com%2Fp", keep: "unchanged"},
		{in: "https://t.lever-analytics.com/email-link?dest=https%3A%2F%2Fjobs.example.net%2Fx&eid=1", keep: "unchanged"},
	})
}
