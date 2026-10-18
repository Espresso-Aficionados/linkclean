package unshorten

import (
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// DefaultRegistry returns the built-in registry: the shorteners listed in the
// package documentation. It is built once and shared; like every Registry it
// is immutable.
func DefaultRegistry() *Registry {
	return defaultRegistry()
}

var defaultRegistry = sync.OnceValue(func() *Registry {
	r, err := NewRegistry(defaultEntries()...)
	if err != nil {
		panic("unshorten: invalid default registry: " + err.Error())
	}
	return r
})

// lnkdLink is the destination anchor of LinkedIn's external-link
// interstitial.
var lnkdLink = regexp.MustCompile(`<a [^>]*data-tracking-control-name="external_url_click"[^>]*href="([^"]+)"`)

// defaultEntries are DefaultRegistry's entries. Entries and patterns the
// live harness has not confirmed are pruned before a release that ships
// them.
func defaultEntries() []Entry {
	return []Entry{
		{
			Name: "t.co", Match: []string{"*://t.co/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: tcoFallback,
		},
		{
			Name: "spotify.link", Match: []string{"*://spotify.link/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: spotifyFallback,
		},
		{
			Name:     "amazon",
			Match:    []string{"*://a.co/d/*", "*://amzn.to/*", "*://amzn.eu/*", "*://amzn.asia/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: amazonFallback,
		},
		{
			Name: "pinterest", Match: []string{"*://pin.it/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: pinterestFallback,
		},
		{
			Name: "pinterest-api", Match: []string{"*://api.pinterest.com/url_shortener/*"}, Intermediate: true,
			Strategy: Redirect(), Kind: FirstParty, IsFallback: pinterestFallback,
		},
		{
			Name: "reddit-share", Match: []string{"*://www.reddit.com/r/*/s/*", "*://reddit.com/r/*/s/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: redditFallback,
		},
		{
			Name: "share.google", Match: []string{"*://share.google/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: googleFallback,
		},
		{
			Name: "share.google-hop", Match: []string{"*://www.google.com/share.google*", "*://google.com/share.google*"},
			Intermediate: true,
			Strategy:     Redirect(), Kind: FirstParty, IsFallback: googleFallback,
		},
		{
			Name:       "tiktok",
			Match:      []string{"*://vm.tiktok.com/*", "*://vt.tiktok.com/*", "*://www.tiktok.com/t/*"},
			FetchHosts: []string{"www.tiktok.com"},
			Strategy:   TikTokOEmbed(), Fallback: Redirect(), Kind: FirstParty, IsFallback: tiktokFallback,
		},
		{
			Name: "lnkd.in", Match: []string{"*://lnkd.in/*"},
			Strategy: BodyRegexp(lnkdLink), Kind: FirstParty, IsFallback: linkedinFallback,
		},
		{
			Name: "fb.me", Match: []string{"*://fb.me/*"},
			Strategy: Redirect(), Kind: FirstParty, IsFallback: facebookFallback,
		},
	}
}

// notRegisteredEntries are the entries doc.go's "Not registered by default"
// section shows (bitlyFallback added): generic shorteners whose preview
// endpoints the package supports but DefaultRegistry does not register.
// TestDocNotRegistered keeps the two in step.
func notRegisteredEntries() []Entry {
	return []Entry{
		{
			Name: "bit.ly", Match: []string{"*://bit.ly/*", "*://bitly.com/*"},
			Strategy: BitlyInfo(), Fallback: Redirect(), Kind: Marketing, IsFallback: bitlyFallback,
		},
		{
			Name: "tinyurl", Match: []string{"*://tinyurl.com/*"},
			Strategy: TinyURLPreview(), Fallback: Redirect(), Kind: Marketing,
		},
	}
}

// hostShape is the host view, site and shape of a fallback candidate.
type hostShape struct {
	host, site string
	path       string // raw path
	query      string // raw query; "" for none or a bare '?'
}

// view returns u's hostShape, or ok false when u has no requestable host view.
func view(u *url.URL) (t hostShape, ok bool) {
	if u == nil {
		return hostShape{}, false
	}
	rv, skip := rawurl.Parse(u.String())
	if skip != rawurl.OK || rv.IsIP {
		return hostShape{}, false
	}
	return hostShape{host: rv.Host, site: rawurl.Site(rv.Host), path: rv.Path, query: rv.Query}, true
}

// root reports a path of "" or "/" with an empty query.
func (t hostShape) root() bool {
	return (t.path == "" || t.path == "/") && t.query == ""
}

// labelSite reports whether t's site is label "." an ICANN suffix.
func (t hostShape) labelSite(label string) bool {
	return len(t.site) > len(label)+1 && t.site[len(label)] == '.' && strings.HasPrefix(t.site, label)
}

// rootOf returns an IsFallback that reports a root target for which on holds.
func rootOf(on func(hostShape) bool) func(short, target *url.URL) bool {
	return func(_, u *url.URL) bool {
		t, ok := view(u)
		return ok && t.root() && on(t)
	}
}

var (
	tcoFallback = rootOf(func(t hostShape) bool { return t.host == "x.com" || t.host == "twitter.com" })

	spotifyFallback = rootOf(func(t hostShape) bool { return t.site == "spotify.com" })

	amazonFallback = rootOf(func(t hostShape) bool {
		mt := matchpattern.Target{Scheme: "https", Host: t.host, Port: "443", Site: t.site}
		mt.SetPath("/")
		for _, p := range amzPatterns {
			if p.Match(&mt) {
				return true
			}
		}
		return false
	})

	pinterestFallback = rootOf(func(t hostShape) bool { return t.labelSite("pinterest") })
	googleFallback    = rootOf(func(t hostShape) bool { return t.labelSite("google") })
	linkedinFallback  = rootOf(func(t hostShape) bool { return t.labelSite("linkedin") })
	facebookFallback  = rootOf(func(t hostShape) bool { return t.labelSite("facebook") })
	bitlyFallback     = rootOf(func(t hostShape) bool { return t.site == "bitly.com" || t.site == "bit.ly" })
)

// amzPatterns is AMZ, linkclean's set of Amazon storefront hosts.
var amzPatterns = func() []*matchpattern.Pattern {
	var ps []*matchpattern.Pattern
	for _, s := range []string{"*://*.amazon.*/*", "*://*.amazon.com.be/*"} {
		p, err := matchpattern.Parse(s, true)
		if err != nil {
			panic(err)
		}
		ps = append(ps, p)
	}
	return ps
}()

// redditSub is a subreddit's front page, where an invalid share code lands.
var redditSub = regexp.MustCompile(`^/r/[^/]+/?$`)

// redditFallback reports a reddit subreddit front page, whatever the query.
func redditFallback(_, u *url.URL) bool {
	t, ok := view(u)
	return ok && t.site == "reddit.com" && redditSub.MatchString(t.path)
}

// tiktokFallback reports TikTok's root, also with the query exactly "_r=1".
func tiktokFallback(_, u *url.URL) bool {
	t, ok := view(u)
	return ok && t.site == "tiktok.com" && (t.path == "" || t.path == "/") && (t.query == "" || t.query == "_r=1")
}
