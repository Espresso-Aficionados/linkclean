package matchpattern

import (
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

func TestParseKinds(t *testing.T) {
	for _, c := range []struct {
		p    string
		kind HostKind
		host string
	}{
		{"*://*/*", HostAny, ""},
		{"*://*:8080/*", HostAny, ""},
		{"*://example.com/*", HostExact, "example.com"},
		{"*://*.example.com/*", HostSuffix, "example.com"},
		{"*://amazon.*/*", HostLabel, "amazon"},
		{"*://*.amazon.*/*", HostLabel, "amazon"},
		{"*://*.Amazon.*/*", HostLabel, "amazon"},
		{"*://under_score.example/*", HostExact, "under_score.example"},
		{"*://xn--bcher-kva.example/*", HostExact, "xn--bcher-kva.example"},
	} {
		m := mustParse(t, c.p, true)
		if k, h := m.Host(); k != c.kind || h != c.host {
			t.Errorf("%s: Host() = %d %q, want %d %q", c.p, k, h, c.kind, c.host)
		}
		if m.String() != c.p {
			t.Errorf("String() = %q, want %q", m.String(), c.p)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct {
		p          string
		allowLabel bool
		want       string // substring of the error
	}{
		{"*://*.amazon.*/*", false, "label.*"},
		{"*://amazon.*/*", false, "label.*"},
		{"*://*.amazon.co.*/*", true, "single DNS label"},
		{"*://.*/*", true, "empty host"},
		{"*://*.*/*", true, "'*' in host"},
		{"*://a*b.example/*", true, "'*' in host"},
		{"*://*.example.*.com/*", true, "'*' in host"},
		{"*://a..b/*", true, "empty host label"},
		{"*://.a/*", true, "empty host label"},
		{"*://a\u200db.example/*", true, "idna"}, // ZWJ outside a joining context
		{"*://\uff0a.example/*", true, "idna"},   // FULLWIDTH ASTERISK maps to '*'
		{"*://a%41.example/*", true, "invalid host byte"},
		{"*://a b/*", true, "invalid host byte"},
		{"*://user@host/*", true, "invalid host byte"},
		{"*://*.[::1]/*", true, "invalid port"},
		{"*://[fe80::1%25eth0]/*", true, "IPv6"},
		{"*://[1.2.3.4]/*", true, "IPv6"},
		{"*://x:0443/*", true, "port"},
		{"*://x:99999/*", true, "port"},
		{"||example.com^", true, "://"},
		{"|https://example.com/", true, "scheme"},
		{"ws://example.com/*", true, "scheme"},
		{"Http://example.com/*", true, "scheme"},
		{"<all_urls>", true, "all_urls"},
		{"*://example.com", true, "missing path"},
	} {
		_, err := Parse(c.p, c.allowLabel)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q, %v) = %v, want error containing %q", c.p, c.allowLabel, err, c.want)
		}
	}
}

// §14.1 matcher pitfalls.
func TestMatchPitfalls(t *testing.T) {
	runMatchCases(t, []matchCase{
		// Suffix patterns respect label boundaries.
		{"*://*.example.com/*", "https://badexample.com/", "", false},
		{"*://*.example.com/*", "https://a.badexample.com/", "", false},
		{"*://*.example.com/*", "https://example.com.evil.example/", "", false},
		{"*://*.example.com/*", "https://a.b.example.com/", "", true},
		// Userinfo never reaches the host view.
		{"*://*.youtube.com/*", "https://www.youtube.com@evil.example/watch?si=X", "", false},
		// Punycode on both sides.
		{"*://*.xn--bcher-kva.example/*", "https://www.xn--bcher-kva.example/", "", true},
		{"*://xn--bcher-kva.example/*", "https://XN--BCHER-KVA.example/", "", true},
		// Literal '?' in a path glob.
		{"*://x.example/r?*", "https://x.example/r?u=1", "", true},
		{"*://x.example/r?*", "https://x.example/rx", "", false},
		// :443 is the https default port.
		{"https://x.example:443/*", "https://x.example/", "", true},
		{"https://x.example:443/*", "https://x.example:443/", "", true},
		{"http://x.example:443/*", "http://x.example/", "", false},
		// "*.x" never matches an IPv6 literal or an IPv4 address.
		{"*://*.1/*", "https://[::1]/", "", false},
		{"*://*.1/*", "https://10.0.0.1/", "", false},
		{"*://*.0.1/*", "https://10.0.0.1/", "", false},
		// label.*: matches when the ICANN site's first label is the label.
		{"*://*.amazon.*/*", "https://amazon.com/dp/X", "amazon.com", true},
		{"*://*.amazon.*/*", "https://www.amazon.co.uk/dp/X", "amazon.co.uk", true},
		{"*://*.amazon.*/*", "https://smile.amazon.de/dp/X", "amazon.de", true},
		{"*://amazon.*/*", "https://www.amazon.co.uk/dp/X", "amazon.co.uk", true},
		{"*://*.amazon.*/*", "https://amazon.evil.example/dp/X", "evil.example", false},
		{"*://*.amazon.*/*", "https://amazon.github.io/dp/X", "github.io", false},
		{"*://*.amazon.*/*", "https://amazon.blogspot.com/dp/X", "blogspot.com", false},
		{"*://*.amazon.*/*", "https://www.amazon.com.be/dp/X", "com.be", false},
		{"*://*.amazon.*/*", "https://amazonx.com/", "amazonx.com", false},
		{"*://*.amazon.*/*", "https://amazon/", "", false},
		{"*://*.amazon.*/*", "https://amazon.com/", "", false}, // Site not set: no match
		{"*://*.amazon.*/*", "https://[::1]/", "amazon.com", false},
		{"https://*.amazon.*/gp/*", "http://amazon.com/gp/x", "amazon.com", false},
		{"https://*.amazon.*/gp/*", "https://amazon.com/dp/x", "amazon.com", false},
	})
}

func TestUnescapeNormal(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"/plain", "/plain"},
		{"/%41%42c", "/ABc"},
		{"/%7e%7E", "/~~"},
		{"/%2F%2f%3F%23%25%20%0A%00%7F%5C%3B%3A%40%26%3D%2B%24%2C", "/%2F%2f%3F%23%25%20%0A%00%7F%5C%3B%3A%40%26%3D%2B%24%2C"},
		{"/%2541", "/%2541"}, // decoded once only
		{"/%%41", "/%A"},
		{"/%4", "/%4"},
		{"/%", "/%"},
		{"/%zz%41", "/%zzA"},
		{"/%C3%A9", "/é"},
		{"/%2A", "/*"},
	} {
		if got := UnescapeNormal(c.in); got != c.want {
			t.Errorf("UnescapeNormal(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A NORMAL-decoded "%2A" in a pattern is a wildcard, as in Chromium (SetPath
// escapes only '?' and '\' before unescaping); in a path it is a literal '*'.
func TestEscapedStar(t *testing.T) {
	runMatchCases(t, []matchCase{
		{"*://x.example/a%2Az", "https://x.example/abcz", "", true},
		{"*://x.example/a*z", "https://x.example/a%2Az", "", true},
		{"*://x.example/a%2Az", "https://x.example/a%2Az", "", true},
	})
}

func TestMatchSchemeGuard(t *testing.T) {
	m := mustParse(t, "*://*/*", false)
	tg := &Target{Scheme: "ftp", Host: "x", Port: "21"}
	tg.SetPath("/")
	if m.Match(tg) {
		t.Error(`"*" scheme matched ftp`)
	}
}

func TestMatchPortLeadingZeros(t *testing.T) {
	m := mustParse(t, "https://x.example:443/*", false)
	for port, want := range map[string]bool{"443": true, "0443": true, "00443": true, "4430": false, "": false, "x": false, "99999999": false} {
		tg := &Target{Scheme: "https", Host: "x.example", Port: port}
		tg.SetPath("/")
		if got := m.Match(tg); got != want {
			t.Errorf("port %q: %v, want %v", port, got, want)
		}
	}
}

func TestMatchAllocs(t *testing.T) {
	pats := []*Pattern{
		mustParse(t, "*://*/*", false),
		mustParse(t, "*://*.example.com/a*b*c", false),
		mustParse(t, "*://*.amazon.*/dp/*", true),
		mustParse(t, "https://x.example.com:443/%41*", false),
	}
	tg := target(t, "https://www.x.example.com/abc?q=%41b", "example.com")
	if n := testing.AllocsPerRun(100, func() {
		for _, p := range pats {
			p.Match(tg)
		}
	}); n != 0 {
		t.Errorf("Match: %v allocs, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { tg.SetPath("/plain/path?q=1&r=%2F") }); n != 0 {
		t.Errorf("SetPath without decodable escapes: %v allocs, want 0", n)
	}
}

// SetRootPath(q) matches as SetPath("/?" + q) does, and builds nothing for
// "/*" globs (PERF2-6).
func TestSetRootPath(t *testing.T) {
	pats := []*Pattern{
		mustParse(t, "*://*/*", false),
		mustParse(t, "*://x.example/?*", false),
		mustParse(t, "*://x.example/?a=1", false),
		mustParse(t, "*://x.example/search?*", false),
		mustParse(t, "*://x.example/%3F*", false),
	}
	for _, q := range []string{"", "a=1", "a=%31", "b"} {
		root := &Target{Scheme: "https", Host: "x.example", Port: "443"}
		full := *root
		root.SetRootPath(q)
		full.SetPath("/?" + q)
		for _, p := range pats {
			if got, want := p.Match(root), p.Match(&full); got != want {
				t.Errorf("%s on root query %q: %v, want %v", p, q, got, want)
			}
		}
	}
	tg := &Target{Scheme: "https", Host: "x.example", Port: "443"}
	if n := testing.AllocsPerRun(100, func() {
		tg.SetRootPath("a=1")
		pats[0].Match(tg)
	}); n != 0 {
		t.Errorf(`SetRootPath + "/*" Match: %v allocs, want 0`, n)
	}
}

func BenchmarkMatch(b *testing.B) {
	m := mustParse(b, "*://*.youtube.com/watch?*", false)
	tg := target(b, "https://www.youtube.com/watch?v=dQw4w9WgXcQ&si=abc&feature=share", "youtube.com")
	b.ReportAllocs()
	for b.Loop() {
		m.Match(tg)
	}
}

func BenchmarkMatchLongPath(b *testing.B) {
	m := mustParse(b, "*://*/*a*b*c*d*e*f*g*h*", false)
	tg := target(b, "https://x.example/"+strings.Repeat("abcdefg", 1170), "")
	b.ReportAllocs()
	for b.Loop() {
		m.Match(tg)
	}
}

// Pattern hosts are canonicalized as URL hosts are (§4.2): a Unicode host
// compiles to its punycode view and matches every spelling with that view.
func TestUnicodePatternHost(t *testing.T) {
	for _, c := range []struct{ in, host string }{
		{"http://bücher.example/*", "xn--bcher-kva.example"},
		{"http://*.BÜCHER.example\u3002/*", "xn--bcher-kva.example"},
		{"http://\u212aelvin.example/*", "kelvin.example"}, // KELVIN SIGN
		{"http://bücher.*/*", "xn--bcher-kva"},
	} {
		if _, h := mustParse(t, c.in, true).Host(); h != c.host {
			t.Errorf("%s: host %q, want %q", c.in, h, c.host)
		}
	}
	p := mustParse(t, "*://bücher.example/*", false)
	for _, h := range []string{"bücher.example", "BÜCHER.example", "xn--bcher-kva.example"} {
		u, sk := rawurl.Parse("https://" + h + "/")
		if sk != rawurl.OK {
			t.Fatalf("Parse(%q): %v", h, sk)
		}
		v := u.Host
		tg := &Target{Scheme: "https", Host: v, Port: "443"}
		tg.SetPath("/")
		if !p.Match(tg) {
			t.Errorf("%s does not match host %q (view %q)", p, h, v)
		}
	}
}
