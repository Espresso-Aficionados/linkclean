package rawurl

import (
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/idna"
)

// FuzzHostView is the MD-4 differential test (§4.1, §14): whenever Parse does
// not skip and url.Parse succeeds with a host, our host view equals
// url.Parse's Hostname() canonicalized the same way (ASCII-lowercase, trim
// trailing dots, IDNA when it succeeds, trim again), brackets aside.
func FuzzHostView(f *testing.F) {
	for _, s := range []string{
		"https://b%C3%BCcher.example/",
		"https://web.archiv%65.org/web/x",
		"http://ȇ.0",
		"http://]0",
		"https://www.youtube.com@evil.example/watch?si=X",
		"https://[::1]:80/",
		"https://EXAMPLE.com./",
		"https://bücher.example/",
		"https://xn--.com/",
		"https://web.archive.org。/web/x",
		"http://0x7f.1/",
		"https://a:b@c:d@e.example:8080/",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, skip := Parse(s)
		if skip != OK {
			return
		}
		if u.Host == "" {
			t.Fatalf("Parse(%q) accepted an empty host", s)
		}
		pu, err := url.Parse(s)
		if err != nil || pu.Host == "" {
			return
		}
		want := strings.TrimRight(asciiLower(pu.Hostname()), ".")
		if a, err := idna.Lookup.ToASCII(want); err == nil {
			want = strings.TrimRight(a, ".")
		}
		if got := strings.TrimSuffix(strings.TrimPrefix(u.Host, "["), "]"); got != want {
			t.Fatalf("Parse(%q) host view %q, net/url canonical %q", s, got, want)
		}
	})
}

// FuzzReserialize checks §14's re-serialization properties for every input
// ParseTarget accepts: no byte <= 0x20 or 0x7F, url.Parse succeeds, and
// Reserialize is idempotent through ParseTarget.
func FuzzReserialize(f *testing.F) {
	for _, s := range []string{
		"https://example.com/a b?q=a+b#x#y",
		"https://bücher.de/C++/%zz\n",
		"HTTPS://аpple.com:8443/\"<>",
		"https://example.com",
		"https://example.com?",
		"https://example.com/%E9t%C3%A9",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, ok := ParseTarget(s)
		if !ok {
			return
		}
		checkReserialized(t, Reserialize(&u))
	})
}
