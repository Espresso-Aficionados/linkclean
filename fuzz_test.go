package linkclean

import (
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/query"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// Fuzz targets of §14.5. Run each with
//
//	go test -run='^$' -fuzz=FuzzClean -fuzztime=60s .

// addSeeds adds every corpus, golden and semantics input, and the
// known-hard shapes of rows 126-129, which random mutation did not find.
func addSeeds(f *testing.F) {
	for _, it := range readCorpus(f) {
		f.Add(it.Input)
	}
	for _, c := range readCases(f, "testdata/golden/positive.txtar") {
		f.Add(c.In)
	}
	data, err := os.ReadFile("testdata/golden/negative.txt")
	if err != nil {
		f.Fatal(err)
	}
	negs, err := parseNegatives(data)
	if err != nil {
		f.Fatal(err)
	}
	for _, n := range negs {
		f.Add(n.URL)
	}
	for _, c := range semanticsCases(f) {
		f.Add(c.In)
	}
	for _, s := range []string{
		"https://example.com/?ck_subscriber_id=1&u=https://example.net/unsubscribe?utm_source=x",
		"https://example.com/?h_sid=1&next=https://example.net/email/view?fbclid=x",
		"https://web.archiv%65.org/web/2024/https://example.com/?utm_source=x",
		"https://b%C3%BCcher.example/?utm_source=x",
		"http://ȇ.0/?utm_source=x",
		"http://]0/",
		"https://www.amazon.com.be/dp/B0X?tag=foo-21&linkCode=ll1&th=1&ref_=x",
	} {
		f.Add(s)
	}
}

// checkFuzzInvariants asserts the §5.6 invariants for one input.
func checkFuzzInvariants(t *testing.T, c *Cleaner, in string, builtins bool) Result {
	out := c.Clean(in)
	r := c.Explain(in)
	if r.URL != out {
		t.Fatalf("Explain(%q).URL = %q, Clean = %q", in, r.URL, out)
	}
	if again := c.Clean(out); again != out {
		t.Fatalf("not idempotent: %q -> %q -> %q", in, out, again)
	}
	if builtins && r.Skipped == SkipRoundCap {
		t.Fatalf("built-in rules reached the round cap on %q", in)
	}
	if r.Skipped != NotSkipped && out != in {
		t.Fatalf("skipped (%d) but changed: %q -> %q", r.Skipped, in, out)
	}
	nested := false
	for _, m := range r.Removed {
		nested = nested || m.Part == PartNested
	}
	if len(r.Removed) == 0 && len(r.Hops) == 0 && out != in {
		t.Fatalf("nothing reported but changed: %q -> %q", in, out)
	}
	if out == in {
		return r
	}
	for i := 0; i < len(out); i++ {
		if out[i] <= 0x20 || out[i] == 0x7f {
			t.Fatalf("output byte %#x: %q -> %q", out[i], in, out)
		}
	}
	if len(r.Hops) == 0 {
		u, _ := rawurl.Parse(in)
		if !strings.HasPrefix(out, u.Prefix) {
			t.Fatalf("scheme/authority changed without a hop: %q -> %q", in, out)
		}
		if !nested && !isSubsequence(out, in) {
			t.Fatalf("not a subsequence: %q -> %q", in, out)
		}
	}
	if _, err := url.Parse(in); err == nil || len(r.Hops) > 0 {
		if _, err := url.Parse(out); err != nil {
			t.Fatalf("url.Parse(%q) from %q: %v", out, in, err)
		}
	}
	return r
}

func isSubsequence(sub, s string) bool {
	j := 0
	for i := 0; i < len(s) && j < len(sub); i++ {
		if s[i] == sub[j] {
			j++
		}
	}
	return j == len(sub)
}

func FuzzClean(f *testing.F) {
	addSeeds(f)
	c := Default()
	f.Fuzz(func(t *testing.T, in string) {
		r := checkFuzzInvariants(t, c, in, true)
		checkResultShape(t, r, AffiliateKeep)
	})
}

// FuzzCleanAmended is FuzzClean under AffiliateStrip, with every debounce
// unwrapper active: a non-zero Beneficiary appears only on CatAffiliate
// items (checkResultShape).
func FuzzCleanAmended(f *testing.F) {
	addSeeds(f)
	for _, s := range []string{
		"https://www.shareasale.com/r.cfm?b=1&u=2&m=3&urllink=www.example.com%2Fproduct&afftrack=",
		"https://www.awin1.com/cread.php?awinmid=1&awinaffid=2&ued=https%3A%2F%2Fwww.example.com%2Fp",
		"https://www.anrdoezrs.net/click-1234567-13579?url=https%3A%2F%2Fwww.example.com%2Fp%3Futm_source%3Dcj",
		"https://example.sjv.io/c/1234/5678/9012?u=https%3A%2F%2Fwww.example-shop.net%2Fp",
		"https://go.skimresources.com/?id=12345X678901&xs=1&url=https%3A%2F%2Fwww.example.net%2Fp",
		"https://click.linksynergy.com/deeplink?id=AbC1dEf2GhI&mid=1234&murl=https%3A%2F%2Fwww.example.net%2Fp",
		"https://us1.mailchimp.com/mctx/clicks?url=https%3A%2F%2Fwww.example.net%2Fa%3Futm_source%3Dmc&xid=1",
		"https://click.pstmrk.it/3s/www.example.net%2Fa/abc/def",
		"https://shareasale.com/r.cfm?u=2&m=3&urllink=https%3A%2F%2Fwww.example.com%2Fx",
		"https://www.awin1.com/cread.php?awinmid=1&awinaffid=2&ued=https%3A%2F%2Fwww.amazon.de%2Fdp%2FB0%3Ftag%3Dx-21%26ref_%3Dy",
	} {
		f.Add(s)
	}
	c := MustNew(Config{Affiliate: AffiliateStrip})
	f.Fuzz(func(t *testing.T, in string) {
		r := checkFuzzInvariants(t, c, in, true)
		checkResultShape(t, r, AffiliateStrip)
	})
}

// FuzzUnwrap wraps a fuzzed destination in each built-in wrapper shape:
// hops never exceed the cap, and the result is http(s) or the input.
func FuzzUnwrap(f *testing.F) {
	for _, s := range []string{
		"https://example.net/a?utm_source=x",
		"www.example.com/p",
		"https://www.google.com/url?q=https%3A%2F%2Fexample.net%2F",
		"https://l.facebook.com/l.php?u=https%3A%2F%2Fl.instagram.com%2F%3Fu%3Dhttps%253A%252F%252Fexample.net%252F",
		"http://10.0.0.1/",
		"https://xn--pple-43d.com/a b#c#d",
		"https://web.archive.org/web/1/https://example.com/?fbclid=1",
	} {
		for k := uint8(0); k < 4; k++ {
			f.Add(k, s, true)
			f.Add(k, query.EncodeComponent(s), false)
		}
	}
	wrappers := []string{
		"https://l.facebook.com/l.php?h=AT0&u=",
		"https://www.google.com/url?sa=t&q=",
		"https://www.shareasale.com/r.cfm?u=2&urllink=",
		"https://www.awin1.com/cread.php?awinaffid=2&ued=",
	}
	cs := []*Cleaner{Default(), MustNew(Config{Affiliate: AffiliateStrip}), MustNew(Config{MaxUnwrap: 2})}
	caps := []int{5, 5, 2}
	f.Fuzz(func(t *testing.T, k uint8, dest string, raw bool) {
		v := dest
		if !raw {
			v = query.EncodeComponent(dest)
		}
		in := wrappers[int(k)%len(wrappers)] + v
		for i, c := range cs {
			r := checkFuzzInvariants(t, c, in, true)
			if len(r.Hops) > caps[i] {
				t.Fatalf("%d hops > cap %d: %q", len(r.Hops), caps[i], in)
			}
			if r.URL != in {
				if u, sk := rawurl.Parse(r.URL); sk != rawurl.OK || (u.Scheme != "http" && u.Scheme != "https") {
					t.Fatalf("result %q of %q is not http(s)", r.URL, in)
				}
			}
			if r.UnwrapAbandoned && len(r.Hops) != 0 {
				t.Fatalf("abandoned with hops: %q", in)
			}
		}
	})
}

// FuzzRoundCap: with a 9-rule Extra chain loaded, Clean never panics, is
// idempotent, and a SkipRoundCap result equals its input.
func FuzzRoundCap(f *testing.F) {
	for _, n := range []int{3, 7, 8, 9, 12} {
		f.Add("https://example.com/a?" + chainQuery(n))
		f.Add("https://example.org/p?utm_source=1&u=" + query.EncodeComponent("https://example.net/b?"+chainQuery(n)))
	}
	c := MustNew(Config{Extra: chainRules(9)})
	f.Fuzz(func(t *testing.T, in string) {
		r := checkFuzzInvariants(t, c, in, false)
		if r.Skipped == SkipRoundCap && (r.URL != in || r.Removed != nil || r.Hops != nil) {
			t.Fatalf("cap result is not the input: %+v", r)
		}
	})
}

// FuzzExtraUnwrap loads Extra Unwrap rules whose scope reads query or path
// text that a strip can remove (R1-IDEM-1), so idempotence depends on the
// unwrap retry after the rounds.
func FuzzExtraUnwrap(f *testing.F) {
	for _, s := range []string{
		"https://w.example.net/r?utm_source=1&url=https%3A%2F%2Fd.example.org%2Fp",
		"https://w.example.net/r?fbclid=1&gclid=2&url=https%3A%2F%2Fw.example.net%2Fr%3Furl%3Dhttps%253A%252F%252Fd.example.org%252F",
		"https://x.example.net/r?utm_medium=1&to=https%3A%2F%2Fd.example.org%2F",
		"https://www.amazon.com/go/ref=abc?url=https%3A%2F%2Fd.example.org%2Fp",
		"https://www.amazon.com/go/ref=a/ref=b?url=https%3A%2F%2Fw.example.net%2Fr%3Futm_source%3D1%26url%3Dhttps%253A%252F%252Fd.example.org",
		"https://l.facebook.com/l.php?goto=https%3A%2F%2Fd.example.org%2Fa%2Ftrk%3D1&u=https%3A%2F%2Fe.example.org%2F",
		"https://d.example.org/a/trk=1/b?goto=https%3A%2F%2Fe.example.org%2Ftrk%3D2%3Futm_source%3D1",
	} {
		f.Add(s)
	}
	c := MustNew(Config{Extra: []Rule{
		{ID: "glob", Action: Unwrap, Include: []string{"*://w.example.net/r?url=*"}, Unwrap: &UnwrapSpec{QueryParams: []string{"url"}}},
		{ID: "re", Action: Unwrap, Include: []string{"*://x.example.net/*"}, URLRegexps: []string{`\?to=`}, Unwrap: &UnwrapSpec{QueryParams: []string{"to"}}},
		{ID: "amz", Action: Unwrap, Include: []string{"*://www.amazon.com/go?*"}, Unwrap: &UnwrapSpec{QueryParams: []string{"url"}}},
		// Global specials, merged with each host's (Q3-1).
		{ID: "any", Action: Unwrap, Include: []string{"*://*/*"}, URLRegexps: []string{`[?&]goto=`}, Unwrap: &UnwrapSpec{QueryParams: []string{"goto"}}},
		{ID: "trk", Action: Strip, Include: []string{"*://*/*"}, PathRegexps: []string{`/trk=[^/]*`}},
	}})
	f.Fuzz(func(t *testing.T, in string) {
		checkFuzzInvariants(t, c, in, false)
	})
}
