package unshorten

import (
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDefaultRegistry(t *testing.T) {
	r := DefaultRegistry()
	if r != DefaultRegistry() {
		t.Error("DefaultRegistry is rebuilt per call")
	}
	want := []string{"amazon", "fb.me", "lnkd.in", "pinterest", "pinterest-api", "reddit-share",
		"share.google", "share.google-hop", "spotify.link", "t.co", "tiktok"}
	if got := names(r.Entries()); !slices.Equal(got, want) {
		t.Errorf("entries %q, want %q", got, want)
	}
	var hosts []string
	for _, e := range r.entries {
		if e.Kind != FirstParty || e.AllowHTTP {
			t.Errorf("%s: Kind %d, AllowHTTP %v", e.Name, e.Kind, e.AllowHTTP)
		}
		if e.IsFallback == nil {
			t.Errorf("%s: no IsFallback", e.Name)
		}
		hosts = append(hosts, e.hosts...)
	}
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	wantHosts := []string{"a.co", "amzn.asia", "amzn.eu", "amzn.to", "api.pinterest.com", "fb.me", "google.com",
		"lnkd.in", "pin.it", "reddit.com", "share.google", "spotify.link", "t.co", "vm.tiktok.com", "vt.tiktok.com",
		"www.google.com", "www.reddit.com", "www.tiktok.com"}
	if !slices.Equal(hosts, wantHosts) {
		t.Errorf("hosts contacted %q, want %q", hosts, wantHosts)
	}
	intermediate := map[string]bool{"pinterest-api": true, "share.google-hop": true}
	for _, e := range r.entries {
		if e.Intermediate != intermediate[e.Name] {
			t.Errorf("%s: Intermediate %v", e.Name, e.Intermediate)
		}
	}
	if s := r.entry("tiktok"); s.Strategy.Name() != "TikTokOEmbed" || s.Fallback.Name() != "Redirect" ||
		!slices.Equal(s.FetchHosts, []string{"www.tiktok.com"}) {
		t.Errorf("tiktok: %+v", s.Entry)
	}
	if s := r.entry("lnkd.in"); s.Strategy.Name() != "BodyRegexp" {
		t.Errorf("lnkd.in: %s", s.Strategy.Name())
	}
}

// TestNotRegistered checks the generic shorteners stay out of
// DefaultRegistry, and that the entries doc.go shows compile.
func TestNotRegistered(t *testing.T) {
	for _, e := range DefaultRegistry().entries {
		for _, h := range e.hosts {
			if h == "bit.ly" || h == "bitly.com" || h == "tinyurl.com" {
				t.Errorf("%s contacts %s", e.Name, h)
			}
		}
	}
	r, err := DefaultRegistry().With(notRegisteredEntries()...)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"bit.ly", "tinyurl"} {
		if e := r.entry(n); e == nil || e.Kind != Marketing || e.Fallback == nil || e.Fallback.Name() != "Redirect" {
			t.Errorf("%s: %+v", n, e)
		}
	}
}

// TestDocNotRegistered keeps doc.go's With example in step with
// notRegisteredEntries.
func TestDocNotRegistered(t *testing.T) {
	src, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	_, sec, ok := strings.Cut(string(src), "// # Not registered by default\n")
	if !ok {
		t.Fatal("doc.go has no Not registered by default section")
	}
	for _, e := range notRegisteredEntries() {
		want := []string{`Name:     "` + e.Name + `"`, "Strategy: unshorten." + e.Strategy.Name() + "()",
			`Match:    []string{"` + strings.Join(e.Match, `", "`) + `"}`}
		for _, w := range want {
			if !strings.Contains(sec, w) {
				t.Errorf("doc.go section lacks %s", w)
			}
		}
	}
}

// TestPackageDocHosts keeps the package documentation's list of the hosts
// DefaultRegistry contacts equal to the registry's Fetcher allowlist (every
// entry's Match hosts and FetchHosts), in both directions. The list is the
// text after "the hosts contacted are:" up to the first sentence end, with
// parenthesised remarks removed.
func TestPackageDocHosts(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "doc.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	if f.Doc == nil {
		t.Fatal("doc.go has no package documentation")
	}
	_, list, ok := strings.Cut(f.Doc.Text(), "With DefaultRegistry the hosts contacted are:")
	if !ok {
		t.Fatal("package documentation has no host list")
	}
	end := regexp.MustCompile(`\.(\s|$)`).FindStringIndex(list)
	if end == nil {
		t.Fatal("host list does not end with a sentence")
	}
	list = regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(list[:end[0]], " ")
	docHosts := regexp.MustCompile(`[a-z0-9-]+(?:\.[a-z0-9-]+)+`).FindAllString(list, -1)
	slices.Sort(docHosts)
	if c := slices.Compact(slices.Clone(docHosts)); len(c) != len(docHosts) {
		t.Errorf("host list names a host twice: %q", docHosts)
	}
	docHosts = slices.Compact(docHosts)

	var regHosts []string
	for _, e := range DefaultRegistry().entries {
		regHosts = append(regHosts, e.hosts...)
	}
	slices.Sort(regHosts)
	regHosts = slices.Compact(regHosts)

	for _, h := range regHosts {
		if !slices.Contains(docHosts, h) {
			t.Errorf("DefaultRegistry contacts %s; the package documentation does not list it", h)
		}
	}
	for _, h := range docHosts {
		if !slices.Contains(regHosts, h) {
			t.Errorf("the package documentation lists %s; DefaultRegistry does not contact it", h)
		}
	}
}

func TestDefaultIsFallback(t *testing.T) {
	r, err := DefaultRegistry().With(notRegisteredEntries()...)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		entry, short string
		fallback     []string
		not          []string
	}{
		{"t.co", "https://t.co/TOKEN1",
			[]string{"https://x.com", "https://x.com/", "https://twitter.com/", "http://X.COM./", "https://x.com/?", "https://x.com/#f"},
			[]string{"https://x.com/user", "https://x.com/?s=20", "https://www.x.com/", "https://mobile.twitter.com/",
				"https://example.com/", "https://t.co/", "https://x.com/home"}},
		{"spotify.link", "https://spotify.link/TOKEN1",
			[]string{"https://spotify.com/", "https://open.spotify.com/", "https://www.spotify.com"},
			[]string{"https://open.spotify.com/track/TOKEN2", "https://open.spotify.com/?si=x", "https://spotify.link/",
				"https://notspotify.com/"}},
		{"amazon", "https://amzn.to/TOKEN1",
			[]string{"https://www.amazon.com/", "https://amazon.com", "https://www.amazon.co.uk/", "https://www.amazon.de/",
				"https://smile.amazon.com/", "https://www.amazon.com.be/", "https://amazon.com.be/"},
			[]string{"https://www.amazon.com/dp/B0", "https://www.amazon.com/?tag=aff-20", "https://www.amazon.github.io/",
				"https://amazon.example/", "https://example.com/"}},
		{"pinterest", "https://pin.it/TOKEN1",
			[]string{"https://www.pinterest.com/", "https://pinterest.co.uk/", "https://pinterest.de"},
			[]string{"https://www.pinterest.com/pin/1/", "https://www.pinterest.com/?x=1", "https://pinterest.example.com/"}},
		{"pinterest-api", "https://api.pinterest.com/url_shortener/TOKEN1/redirect/",
			[]string{"https://www.pinterest.com/"},
			[]string{"https://www.pinterest.com/pin/1/"}},
		{"reddit-share", "https://www.reddit.com/r/sub/s/TOKEN1",
			[]string{"https://www.reddit.com/r/sub/", "https://www.reddit.com/r/sub", "https://reddit.com/r/sub/?x=1",
				"https://old.reddit.com/r/sub/"},
			[]string{"https://www.reddit.com/r/sub/comments/x/y/", "https://www.reddit.com/", "https://www.reddit.com/r/",
				"https://www.reddit.com/user/x/", "https://example.com/r/sub/"}},
		{"share.google", "https://share.google/TOKEN1",
			[]string{"https://www.google.com/", "https://google.com", "https://www.google.co.uk/"},
			[]string{"https://www.google.com/search?q=x", "https://www.google.com/?hl=en", "https://share.google/error",
				"https://google.example.com/"}},
		{"share.google-hop", "https://www.google.com/share.google?q=TOKEN1",
			[]string{"https://www.google.com/"},
			[]string{"https://www.google.com/maps"}},
		{"tiktok", "https://vm.tiktok.com/TOKEN1/",
			[]string{"https://www.tiktok.com/", "https://www.tiktok.com/?_r=1", "https://tiktok.com", "https://m.tiktok.com/?_r=1"},
			[]string{"https://www.tiktok.com/@u/video/1", "https://www.tiktok.com/?_r=2", "https://www.tiktok.com/?_r=1&x=1",
				"https://www.tiktok.com/?lang=en", "https://tiktok.example.com/"}},
		{"lnkd.in", "https://lnkd.in/TOKEN1",
			[]string{"https://www.linkedin.com", "https://www.linkedin.com/", "https://linkedin.cn/"},
			[]string{"https://www.linkedin.com/feed/", "https://www.linkedin.com/?trk=x", "https://example.net/preprints/2026/TOKEN2/"}},
		{"fb.me", "https://fb.me/TOKEN1",
			[]string{"https://www.facebook.com/", "https://m.facebook.com/", "https://facebook.com"},
			[]string{"https://www.facebook.com/page", "https://www.facebook.com/?ref=x"}},
		{"bit.ly", "https://bit.ly/TOKEN1",
			[]string{"https://bitly.com/", "https://www.bitly.com/", "https://bit.ly/"},
			[]string{"https://bitly.com/pages/about", "https://example.net/"}},
	}
	for _, tt := range tests {
		e := r.entry(tt.entry)
		if e == nil || e.IsFallback == nil {
			t.Errorf("%s: no IsFallback", tt.entry)
			continue
		}
		short := mustURL(t, tt.short)
		for _, s := range tt.fallback {
			if !e.IsFallback(short, mustURL(t, s)) {
				t.Errorf("%s: IsFallback(%s) = false, want true", tt.entry, s)
			}
		}
		for _, s := range tt.not {
			if e.IsFallback(short, mustURL(t, s)) {
				t.Errorf("%s: IsFallback(%s) = true, want false", tt.entry, s)
			}
		}
		if e.IsFallback(short, nil) || e.IsFallback(short, &url.URL{Scheme: "https", Host: "127.0.0.1", Path: "/"}) {
			t.Errorf("%s: IsFallback of nil or an IP", tt.entry)
		}
	}
	if r.entry("tinyurl").IsFallback != nil {
		t.Error("tinyurl has an IsFallback")
	}
}
