package matchpattern

import (
	"net/netip"
	"strings"
	"testing"
)

// target builds a Target from a simple absolute URL the way the engine does
// from rawurl's views (lowercased host, trailing dots trimmed, effective port,
// pathForRequest without the fragment). Hosts in tests are already ASCII.
// site is the ICANN eTLD+1 the caller asserts for the host.
func target(tb testing.TB, raw, site string) *Target {
	tb.Helper()
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		tb.Fatalf("target %q: no scheme", raw)
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	auth, tail := rest[:end], rest[end:]
	if i := strings.LastIndexByte(auth, '@'); i >= 0 {
		auth = auth[i+1:]
	}
	host, port := auth, ""
	if strings.HasPrefix(auth, "[") {
		i := strings.IndexByte(auth, ']')
		host = auth[:i+1]
		port = strings.TrimPrefix(auth[i+1:], ":")
	} else if i := strings.LastIndexByte(auth, ':'); i >= 0 {
		host, port = auth[:i], auth[i+1:]
	}
	host = strings.TrimRight(strings.ToLower(host), ".")
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[strings.ToLower(scheme)]
	}
	tail, _, _ = strings.Cut(tail, "#")
	if tail == "" || tail[0] == '?' {
		tail = "/" + tail
	}
	t := &Target{
		Scheme: strings.ToLower(scheme),
		Host:   host,
		IsIP:   isIPHost(host),
		Port:   port,
		Site:   site,
	}
	t.SetPath(tail)
	return t
}

func isIPHost(h string) bool {
	if strings.HasPrefix(h, "[") {
		return true
	}
	_, err := netip.ParseAddr(h)
	return err == nil
}

func mustParse(tb testing.TB, p string, allowLabel bool) *Pattern {
	tb.Helper()
	m, err := Parse(p, allowLabel)
	if err != nil {
		tb.Fatal(err)
	}
	return m
}

// matchCase is one (pattern, URL) vector. site is the Target's Site.
type matchCase struct {
	pattern, url, site string
	want               bool
}

func runMatchCases(t *testing.T, cases []matchCase) {
	t.Helper()
	for _, c := range cases {
		m := mustParse(t, c.pattern, true)
		if got := m.Match(target(t, c.url, c.site)); got != c.want {
			t.Errorf("%s vs %s (site %q) = %v, want %v", c.pattern, c.url, c.site, got, c.want)
		}
	}
}
