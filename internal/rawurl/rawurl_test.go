package rawurl

import (
	"math/rand"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/idna"
)

func TestParseSkips(t *testing.T) {
	tests := []struct {
		in   string
		want Skip
	}{
		{"https://example.com/", OK},
		{"HTTPS://example.com/", OK},
		{"Http://example.com", OK},
		{"ftp://example.com/", Scheme},
		{"javascript://example.com/%0Aalert(1)", Scheme},
		{"httpx://example.com/", Scheme},
		{"h-t.t+p://example.com/", Scheme},
		{"example.com/path", Unparseable},
		{"https:example.com", Unparseable},
		{"https:/example.com", Unparseable},
		{"://example.com", Unparseable},
		{"1http://example.com", Unparseable},
		{"ht tp://example.com", Unparseable},
		{"https://", Unparseable},
		{"https:///path", Unparseable},
		{"https://?q=1", Unparseable},
		{"https://user@/x", Unparseable},
		{"https://:443/x", Unparseable},
		{"https://.../x", Unparseable},  // empty after trimming dots
		{"https://xn--/x", Unparseable}, // IDNA maps "xn--" to ""
		{"https://example.com/a b", Ambiguous},
		{"https://example.com/?q=a\tb", Ambiguous},
		{"https://example.com/#x\n", Ambiguous},
		{"https://example.com/\x7f", Ambiguous},
		{"https://example.com/\x00", Ambiguous},
		{" https://example.com/", Unparseable}, // invalid scheme character comes first
		{"https://exa\\mple.com/", Ambiguous},
		{"https://example.com\\@evil.example/", Ambiguous},
		{"https://example.com/a\\b", OK}, // '\' outside the authority is ordinary
		{"https://web.archiv%65.org/web/x", Ambiguous},
		{"https://b%C3%BCcher.example/", Ambiguous},
		{"https://example.com:%38%30/", Ambiguous},
		{"https://us%65r@example.com/", OK}, // '%' in userinfo is not the host
		{"https://[::1]/", OK},
		{"https://[::1]:8080/", OK},
		{"https://[2001:DB8::1]/", OK},
		{"https://user@[::1]:80/", OK},
		{"https://[::1/", Ambiguous},
		{"https://::1]/", Ambiguous},
		{"https://]0/", Ambiguous},
		{"http://]0", Ambiguous},
		{"https://[::1]]/", Ambiguous},
		{"https://[[::1]/", Ambiguous},
		{"https://[::1]x/", Ambiguous},
		{"https://[::1]:80]/", Ambiguous},
		{"https://[::1]:[80/", Ambiguous},
		{"https://ex[ample.com/", Ambiguous},
		{"https://[1.2.3.4]/", Ambiguous},     // not an IPv6 literal
		{"https://[example.com]/", Ambiguous}, // not an IP literal
		{"https://[a.]/", Ambiguous},
		{"https://[]/", Ambiguous},
		{"https://ex]ample.com/", Ambiguous},
		{"https://[x]@example.com/", OK}, // brackets in userinfo are not the host's
	}
	for _, tt := range tests {
		if _, got := Parse(tt.in); got != tt.want {
			t.Errorf("Parse(%q) skip = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseViews(t *testing.T) {
	tests := []struct {
		in   string
		want URL // Raw is filled in from in
	}{
		{"https://example.com/a/b?x=1&y=2#frag", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Path: "/a/b", Query: "x=1&y=2", HasQuery: true, Fragment: "frag", HasFragment: true,
			Host: "example.com",
		}},
		{"HTTP://User:Pw@WWW.Example.COM:8080", URL{
			Scheme: "http", Prefix: "HTTP://User:Pw@WWW.Example.COM:8080", Authority: "User:Pw@WWW.Example.COM:8080",
			Port: "8080", Host: "www.example.com",
		}},
		// Userinfo ends at the last '@', as in net/url.
		{"https://www.youtube.com@evil.example/watch?si=X", URL{
			Scheme: "https", Prefix: "https://www.youtube.com@evil.example", Authority: "www.youtube.com@evil.example",
			Path: "/watch", Query: "si=X", HasQuery: true, Host: "evil.example",
		}},
		{"https://a@b@c.example/", URL{
			Scheme: "https", Prefix: "https://a@b@c.example", Authority: "a@b@c.example",
			Path: "/", Host: "c.example",
		}},
		{"https://example.com:443/", URL{
			Scheme: "https", Prefix: "https://example.com:443", Authority: "example.com:443",
			Port: "443", Path: "/", Host: "example.com",
		}},
		{"https://example.com:/", URL{
			Scheme: "https", Prefix: "https://example.com:", Authority: "example.com:",
			Path: "/", Host: "example.com",
		}},
		{"https://example.com?a=1", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Query: "a=1", HasQuery: true, Host: "example.com",
		}},
		{"https://example.com#x?y=1", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Fragment: "x?y=1", HasFragment: true, Host: "example.com",
		}},
		{"https://example.com/p#a#b?c", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Path: "/p", Fragment: "a#b?c", HasFragment: true, Host: "example.com",
		}},
		{"https://example.com/p?", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Path: "/p", HasQuery: true, Host: "example.com",
		}},
		{"https://example.com/p?#", URL{
			Scheme: "https", Prefix: "https://example.com", Authority: "example.com",
			Path: "/p", HasQuery: true, HasFragment: true, Host: "example.com",
		}},
		{"https://Example.COM.../", URL{
			Scheme: "https", Prefix: "https://Example.COM...", Authority: "Example.COM...",
			Path: "/", Host: "example.com",
		}},
		{"https://bücher.example/", URL{
			Scheme: "https", Prefix: "https://bücher.example", Authority: "bücher.example",
			Path: "/", Host: "xn--bcher-kva.example",
		}},
		{"https://BÜCHER.example/", URL{
			Scheme: "https", Prefix: "https://BÜCHER.example", Authority: "BÜCHER.example",
			Path: "/", Host: "xn--bcher-kva.example",
		}},
		{"https://web.archive.org\u3002/web/x", URL{
			Scheme: "https", Prefix: "https://web.archive.org\u3002", Authority: "web.archive.org\u3002",
			Path: "/web/x", Host: "web.archive.org",
		}},
		{"https://a_b.example/", URL{
			Scheme: "https", Prefix: "https://a_b.example", Authority: "a_b.example",
			Path: "/", Host: "a_b.example",
		}},
		{"http://192.168.0.1:80/x", URL{
			Scheme: "http", Prefix: "http://192.168.0.1:80", Authority: "192.168.0.1:80",
			Port: "80", Path: "/x", Host: "192.168.0.1", IsIP: true,
		}},
		{"http://0x7f.1/", URL{
			Scheme: "http", Prefix: "http://0x7f.1", Authority: "0x7f.1",
			Path: "/", Host: "0x7f.1", IsIP: true,
		}},
		{"http://2130706433/", URL{
			Scheme: "http", Prefix: "http://2130706433", Authority: "2130706433",
			Path: "/", Host: "2130706433", IsIP: true,
		}},
		{"http://example.0x/", URL{
			Scheme: "http", Prefix: "http://example.0x", Authority: "example.0x",
			Path: "/", Host: "example.0x", IsIP: true,
		}},
		{"http://1.2.3.4./", URL{
			Scheme: "http", Prefix: "http://1.2.3.4.", Authority: "1.2.3.4.",
			Path: "/", Host: "1.2.3.4", IsIP: true,
		}},
		{"http://１.２.３.４/", URL{
			Scheme: "http", Prefix: "http://１.２.３.４", Authority: "１.２.３.４",
			Path: "/", Host: "1.2.3.4", IsIP: true,
		}},
		// IP flags apply after IDNA (FuzzHostView find).
		{"http://ȇ.0", URL{
			Scheme: "http", Prefix: "http://ȇ.0", Authority: "ȇ.0",
			Host: "xn--gla.0", IsIP: true,
		}},
		{"http://example.123x/", URL{
			Scheme: "http", Prefix: "http://example.123x", Authority: "example.123x",
			Path: "/", Host: "example.123x",
		}},
		{"https://[2001:DB8::1]:8443/x", URL{
			Scheme: "https", Prefix: "https://[2001:DB8::1]:8443", Authority: "[2001:DB8::1]:8443",
			Port: "8443", Path: "/x", Host: "[2001:db8::1]", IsIP: true,
		}},
		{"https://xn--.com/", URL{
			Scheme: "https", Prefix: "https://xn--.com", Authority: "xn--.com",
			Path: "/", Host: ".com",
		}},
	}
	for _, tt := range tests {
		got, skip := Parse(tt.in)
		tt.want.Raw = tt.in
		if skip != OK || got != tt.want {
			t.Errorf("Parse(%q) =\n\t%+v, %v\nwant\n\t%+v", tt.in, got, skip, tt.want)
		}
	}
}

func TestDerivedViews(t *testing.T) {
	tests := []struct {
		in               string
		port, pfr, match string
	}{
		{"https://example.com", "443", "/", "https://example.com/"},
		{"http://example.com", "80", "/", "http://example.com/"},
		{"https://example.com?a=1#f", "443", "/?a=1", "https://example.com/?a=1#f"},
		{"HTTPS://u:p@Example.com:443/a?b#c", "443", "/a?b", "https://example.com/a?b#c"},
		{"https://example.com:8443/a?b#c", "8443", "/a?b", "https://example.com:8443/a?b#c"},
		{"http://example.com:443/", "443", "/", "http://example.com:443/"},
		{"http://example.com:/p?", "80", "/p?", "http://example.com/p?"},
		{"https://[::1]:80/x#", "80", "/x", "https://[::1]:80/x#"},
	}
	for _, tt := range tests {
		u, skip := Parse(tt.in)
		if skip != OK {
			t.Fatalf("Parse(%q): %v", tt.in, skip)
		}
		if got := u.EffectivePort(); got != tt.port {
			t.Errorf("%q EffectivePort = %q, want %q", tt.in, got, tt.port)
		}
		if got := u.PathForRequest(); got != tt.pfr {
			t.Errorf("%q PathForRequest = %q, want %q", tt.in, got, tt.pfr)
		}
		if got := u.MatchString(); got != tt.match {
			t.Errorf("%q MatchString = %q, want %q", tt.in, got, tt.match)
		}
	}
}

func TestSite(t *testing.T) {
	tests := []struct{ host, want string }{
		{"www.amazon.co.uk", "amazon.co.uk"},
		{"amazon.co.uk", "amazon.co.uk"},
		{"amazon.com", "amazon.com"},
		{"smile.amazon.de", "amazon.de"},
		{"www.amazon.com.be", "com.be"}, // com.be is not on the PSL (§4.2 limitation)
		{"amazon.com.be", "com.be"},
		{"amazon.github.io", "github.io"}, // private suffix skipped (DR-F7)
		{"a.github.io", "github.io"},
		{"amazon.blogspot.com", "blogspot.com"},
		{"x.y.s3.amazonaws.com", "amazonaws.com"},
		{"amazon.evil.example", ""}, // .example is not an ICANN TLD
		{"evil.example", ""},
		{"github.io", "github.io"},
		{"co.uk", ""},
		{"com", ""},
		{"localhost", ""},
		{"", ""},
		{"a..com", ""},
		{".com", ""},
		{"x.a..com", ""},
		{".a.com", "a.com"},
		{"xn--bcher-kva.de", "xn--bcher-kva.de"},
		{"www.xn--bcher-kva.de", "xn--bcher-kva.de"},
	}
	for _, tt := range tests {
		if got := Site(tt.host); got != tt.want {
			t.Errorf("Site(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestParseTarget(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"https://example.com/x", true},
		{"HTTP://Example.COM", true},
		{"https://example.com:8443/x", true},
		{"https://example.com:/x", true},
		{"https://example.com/a b\n?q=\x7f#\t", true}, // controls outside the authority are encoded later
		{"https://bücher.de/", true},
		{"https://аpple.com/", true},
		{"https://a.github.io/", true}, // site github.io; same-site is the engine's check
		{"ftp://example.com/", false},
		{"//example.com/", false},
		{"example.com", false},
		{"https://user@example.com/", false},
		{"https://user:pw@example.com/", false},
		{"https://@example.com/", false},
		{"https://exa\\mple.com/", false},
		{"https://exa mple.com/", false},
		{"https://example.com\t/", false},
		{"https://ex\x7fample.com/", false},
		{"https://web.archiv%65.org/", false},
		{"https://example.com:%38/", false},
		{"https://127.0.0.1/", false},
		{"https://0x7f.1/", false},
		{"https://2130706433/", false},
		{"https://[::1]/", false},
		{"https://[::1/", false},
		{"https://localhost/", false},
		{"https://intranet:8080/", false},
		{"https://co.uk/", false},
		{"https://example.invalidtld/", false},
		{"https://a_b.example.com/", false}, // fails IDNA (STD3)
		{"https://-a.example.com/", false},
		{"https://a..com/", false},
		{"https://example.com:0/", false},
		{"https://example.com:65535/", true},
		{"https://example.com:65536/", false},
		{"https://example.com:99999999999999999999/", false},
		{"https://example.com:8a/", false},
		{"https://example.com:+80/", false},
		{"https:///x", false},
	}
	// x/net/idna encodes this label but rejects its own encoding (FuzzReserialize find).
	tests = append(tests, struct {
		in   string
		want bool
	}{"http://" + strings.Repeat("u", 2600) + "\u04fd\u00e2.ac", false})
	for _, tt := range tests {
		if _, got := ParseTarget(tt.in); got != tt.want {
			t.Errorf("ParseTarget(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestReserialize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://example.com/a b", "https://example.com/a%20b"},
		{"https://example.com/C++/a+b?q=a+b", "https://example.com/C++/a+b?q=a+b"},
		{"https://example.com/x\n?y=\n#\n", "https://example.com/x%0A?y=%0A#%0A"},
		{"https://example.com/x%0A", "https://example.com/x%0A"},
		{`https://example.com/"<>\^` + "`{|}[]", "https://example.com/%22%3C%3E%5C%5E%60%7B%7C%7D%5B%5D"},
		{"https://example.com/caf\u00e9", "https://example.com/caf%C3%A9"},
		{"https://example.com/%zz%4%41%e9", "https://example.com/%25zz%254%41%e9"},
		{"https://bücher.de/x", "https://xn--bcher-kva.de/x"},
		{"https://аpple.com/", "https://xn--pple-43d.com/"},
		{"HTTPS://WWW.Example.COM./P", "https://www.example.com/P"},
		{"https://example.com", "https://example.com"},
		{"https://example.com?q", "https://example.com?q"},
		{"https://example.com:8443", "https://example.com:8443"},
		{"https://example.com:/x", "https://example.com/x"},
		{"https://example.com/p?a=1&b=:@/?#f#g?h", "https://example.com/p?a=1&b=:@/?#f%23g?h"},
		{"https://example.com/p?x#", "https://example.com/p?x#"},
		{"https://example.com/!$&'()*+,;=:@~._-", "https://example.com/!$&'()*+,;=:@~._-"},
		{"https://example.com/a?b?c", "https://example.com/a?b?c"},
		{"https://example.com/\xff\x00", "https://example.com/%FF%00"},
	}
	for _, tt := range tests {
		u, ok := ParseTarget(tt.in)
		if !ok {
			t.Errorf("ParseTarget(%q) rejected", tt.in)
			continue
		}
		got := Reserialize(&u)
		if got != tt.want {
			t.Errorf("Reserialize(%q) = %q, want %q", tt.in, got, tt.want)
		}
		checkReserialized(t, got)
	}
}

// checkReserialized asserts the §5.4/§14 properties of a Reserialize output.
func checkReserialized(t *testing.T, out string) {
	t.Helper()
	if hasCTL(out) {
		t.Fatalf("Reserialize output %q has a byte <= 0x20 or 0x7F", out)
	}
	if _, err := url.Parse(out); err != nil {
		t.Fatalf("url.Parse(%q): %v", out, err)
	}
	u, ok := ParseTarget(out)
	if !ok {
		t.Fatalf("ParseTarget rejects Reserialize output %q", out)
	}
	if again := Reserialize(&u); again != out {
		t.Fatalf("Reserialize not idempotent: %q -> %q", out, again)
	}
}

// TestIDNAShortcut checks that skipping IDNA for [a-z0-9._-] hosts without
// an "xn--" label gives exactly what IDNA would.
func TestIDNAShortcut(t *testing.T) {
	hosts := []string{"a_b.com", "-a.com", "a-.com", "a..b.com", "ab--c.com", "_dmarc.example.com",
		"a.b-.c", "0x.com", "1a.com", ".", "..a", "a.", "-", "_", "x-n--a.com", "axn--b.com"}
	r := rand.New(rand.NewSource(1))
	const alpha = "abxn-_.019"
	for range 200000 {
		b := make([]byte, 1+r.Intn(12))
		for j := range b {
			b[j] = alpha[r.Intn(len(alpha))]
		}
		hosts = append(hosts, string(b))
	}
	for _, h := range hosts {
		h = strings.TrimRight(h, ".")
		if !idnaShortcut(h) {
			continue
		}
		want := h
		if a, err := idna.Lookup.ToASCII(h); err == nil {
			want = strings.TrimRight(a, ".")
		}
		if want != h {
			t.Errorf("shortcut host %q: IDNA gives %q", h, want)
		}
	}
	for _, h := range []string{"xn--bcher-kva.de", "a.xn--p1ai", "xn--", "bücher.de", "a!b.com"} {
		if idnaShortcut(h) {
			t.Errorf("idnaShortcut(%q) = true", h)
		}
	}
}

func TestAllocs(t *testing.T) {
	const raw = "https://www.example.com/path/to/page?utm_source=x&id=123&b=2#frag"
	var u URL
	if n := testing.AllocsPerRun(100, func() { u, _ = Parse(raw) }); n != 0 {
		t.Errorf("Parse allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = u.PathForRequest() }); n != 0 {
		t.Errorf("PathForRequest allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = Site("localhost") }); n != 0 {
		t.Errorf("Site(single-label) allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = HostView("www.example.com") }); n != 0 {
		t.Errorf("HostView(ASCII) allocs = %v, want 0", n)
	}
}

func BenchmarkParse(b *testing.B) {
	const raw = "https://www.example.com/path/to/page?utm_source=x&id=123&b=2#frag"
	b.ReportAllocs()
	for b.Loop() {
		Parse(raw)
	}
}

func BenchmarkHostView(b *testing.B) {
	for _, h := range []string{"www.example.com", "WWW.Example.COM", "bücher.example", "xn--bcher-kva.example"} {
		b.Run(h, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				HostView(h)
			}
		})
	}
}

func BenchmarkSite(b *testing.B) {
	for _, h := range []string{"www.amazon.co.uk", "amazon.github.io", "localhost"} {
		b.Run(h, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				Site(h)
			}
		})
	}
}
