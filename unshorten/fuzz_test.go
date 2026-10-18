package unshorten

// Fuzz targets: the resolution loop over scripted strategies,
// next-URL validation with the Fetcher's request checks, and the body
// extractors. None of them touches a network: FuzzResolve's strategies
// never dial (a Get goes to an opaque transport that fails), and the
// others are pure.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Espresso-Aficionados/linkclean"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// fzPrefixes are what a script line or input starting with a digit expands
// to: registered hosts, an intermediate one, a Marketing one, the
// fallback root, an unregistered destination, a wrapper around a short
// link and request-form variants. Every one but the fallback root embeds
// the token.
var fzPrefixes = [10]string{
	"https://fa.example.net/" + rvToken,
	"https://fi.example.net/" + rvToken,
	"https://mk.example.net/" + rvToken,
	"https://home.example.net/",
	"https://example.net/" + rvToken,
	"https://l.facebook.com/l.php?h=AT0&u=https%3A%2F%2Ffa.example.net%2F" + rvToken,
	"http://fa.example.net/" + rvToken,
	"https://FA.EXAMPLE.NET/" + rvToken,
	"https://fa.example.net:443/" + rvToken,
	"https://example.net/" + rvToken + "?utm_source=x&",
}

// fzExpand expands a leading digit to its prefix; any other string is used
// as written.
func fzExpand(s string) string {
	if s != "" && '0' <= s[0] && s[0] <= '9' {
		return fzPrefixes[s[0]-'0'] + s[1:]
	}
	return s
}

// fzErrors are the errors a "!x" script line returns, by x.
var fzErrors = map[byte]error{
	'r': ErrNoRedirect, 'm': ErrNoMatch, 'b': ErrBadTarget, 'd': ErrFetchDenied, 'u': ErrBudget,
	's': ErrNotShortLink, 'h': ErrHopLimit, 'l': ErrLoop, 'f': ErrFallback, 't': ErrTransient,
	'4': &StatusError{Host: "fa.example.net", Status: 404}, '5': &StatusError{Host: "fa.example.net", Status: 503},
	'c': context.Canceled, 'x': context.DeadlineExceeded,
}

// fzScript is a strategy driven by script lines: attempt a of hop h reads
// line 2h+a, so the same script gives the same chain on every call.
// Lines: "!x" returns fzErrors[x] (another x: an error holding the
// token); "g" makes a Get of u through the failing transport; "p" panics
// with a value holding the URL; "n" returns a nil URL; anything else is
// expanded (fzExpand) and returned as the next URL, or ErrNoMatch if
// url.Parse refuses it.
func fzScript(name string, attempt int, lines []string) Strategy {
	return rvStrategy{name: name, next: func(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
		line := ""
		if i := 2*f.hop + attempt; i < len(lines) {
			line = lines[i]
		}
		switch {
		case strings.HasPrefix(line, "!"):
			if len(line) > 1 {
				if err, ok := fzErrors[line[1]]; ok {
					return nil, err
				}
			}
			return nil, errors.New("scripted failure for " + u.String())
		case line == "g":
			if _, err := f.Get(ctx, u, false); err != nil {
				return nil, err
			}
			return nil, ErrNoRedirect
		case line == "p":
			panic("scripted panic for " + u.String())
		case line == "n":
			return nil, nil
		}
		next, err := url.Parse(fzExpand(line))
		if err != nil {
			return nil, ErrNoMatch
		}
		return next, nil
	}}
}

// FuzzResolve runs Resolve and Process over a registry of scripted
// strategies, with a fuzzed policy, MaxHops and Cleaner, NoCache and
// NoLimit (so every run is deterministic) and a transport that fails
// every request. Neither may panic; a failure returns Clean(raw) and wraps
// ErrUnresolved; Clean(URL) == URL always; both agree on the URL and on
// failing; there are at most MaxHops hops, each a CatUnshortened hop of
// Process; no error holds the token every scripted URL carries; every
// hop's From (the URL requested and keyed) is what net/url sends as
// written.
func FuzzResolve(f *testing.F) {
	for _, s := range []struct {
		in, script string
		policy     uint8
		maxHops    uint8
		strip      bool
	}{
		{"6/abc", "4/p?utm_source=x", 0, 0, false},                          // N1
		{"0/xyz", "4/dp/B0?tag=aff-20&linkCode=ll1&ref_=as_li", 0, 0, true}, // N2
		{"0/xyz", "3", 0, 0, false},                                         // N3
		{"0/abc", "https://evil.example.net/x", 0, 0, false},                // N4
		{"2/abc", "4/dest?utm_source=bitly", 0, 0, false},                   // N5
		{"2/abc", "!m\n4/dest", 1, 0, false},                                // N5 under Follow: the fallback
		{"2/abc", "4/dest", 2, 0, false},                                    // Skip: no request
		{"0/a", "0/b\n\n0/a#x", 0, 0, false},                                // loop
		{"0/a", "0/a", 0, 0, false},                                         // self loop
		{"6/a", "0/a", 0, 0, false},                                         // self loop after the upgrade
		{"0/1", "0/2\n\n0/3\n\n0/4\n\n0/5\n\n0/6\n\n0/7", 0, 3, false},      // hop limit
		{"0/1", "0/2\n\n0/3\n\n0/4\n\n0/5\n\n0/6\n\n0/7", 0, 9, false},      // clamped
		{"0/a", "5/b", 0, 0, false},                                         // a wrapper around a short link
		{"5/a", "4/b?utm_source=y", 0, 0, true},                             // a wrapped input
		{"0/a", "1/b\n\n!s", 0, 0, false},                                   // stop on an intermediate entry
		{"0/a", "4/b\n\n!s", 0, 0, false},                                   // destination left the registry
		{"0/a", "0/b?utm_source=q\n\n!s", 0, 0, false},                      // stop after a hop
		{"0/a", "!s", 0, 0, false},                                          // stop at the first hop
		{"0/a", "!5\n!r", 0, 0, false},                                      // transient, then definitive
		{"0/a", "g\ng", 0, 0, false},                                        // Gets through the failing transport
		{"0/a", "p", 0, 0, false},                                           // panic
		{"0/a", "n\n4/x", 0, 0, false},                                      // nil URL, then the fallback
		{"0/a", "!z\n4/x", 0, 0, false},                                     // an unknown error
		{"0/a", "https://u:p@example.net/\n=https://127.0.0.1/", 0, 0, false},
		{"0/a", "https://example.net:8080/\nhttps://example.net/" + strings.Repeat("a", 9000), 0, 0, false},
		{"7/a?utm_source=x#frag", "4/b", 0, 0, false},
		{"8/a", "4/b", 0, 0, false},
		{"https://example.org/x?utm_source=z", "", 0, 0, false},
		{"0/\xc3\xa4", "4/b", 0, 0, false}, // raw non-ASCII: not requestable
		{"0/%zz", "4/b", 0, 0, false},      // an invalid escape: not requestable
		{"0/%C3%A4", "4/b", 0, 0, false},
	} {
		f.Add(s.in, s.script, s.policy, s.maxHops, s.strip)
	}
	strip, err := linkclean.New(linkclean.Config{Affiliate: linkclean.AffiliateStrip})
	if err != nil {
		f.Fatal(err)
	}
	discard := slog.New(slog.DiscardHandler)
	rt := &rvFuncRT{fn: func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("fuzz transport: no network")
	}}
	fallback := func(_, target *url.URL) bool {
		return target.Host == "home.example.net" && (target.Path == "" || target.Path == "/") && target.RawQuery == ""
	}
	f.Fuzz(func(t *testing.T, in, script string, policy, maxHops uint8, useStrip bool) {
		lines := strings.Split(script, "\n")
		a, b := fzScript("ScriptA", 0, lines), fzScript("ScriptB", 1, lines)
		reg, err := NewRegistry(
			Entry{Name: "fa", Match: []string{"*://fa.example.net/*"}, Strategy: a, Fallback: b, IsFallback: fallback},
			Entry{Name: "fi", Match: []string{"*://fi.example.net/*"}, Strategy: a, Intermediate: true},
			Entry{Name: "mk", Match: []string{"*://mk.example.net/*"}, Strategy: a, Fallback: b, Kind: Marketing},
		)
		if err != nil {
			t.Fatal(err)
		}
		c := linkclean.Default()
		if useStrip {
			c = strip
		}
		mh := int(maxHops % 7)
		r, err := New(Config{
			Cleaner: c, Registry: reg, Cache: NoCache(), Limiter: NoLimit(), Logger: discard,
			Transport: rt, AllowUnguardedTransport: true,
			Generic: GenericPolicy(policy % 3), MaxHops: mh, Timeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer r.st.group.Wait()
		limit := r.st.cfg.maxHops
		raw := fzExpand(in)

		res, err := r.Resolve(context.Background(), raw)
		pr, perr := r.Process(context.Background(), raw)
		if err != nil {
			if !errors.Is(err, ErrUnresolved) {
				t.Errorf("Resolve error %v does not wrap ErrUnresolved", err)
			}
			if clean := c.Clean(raw); res.URL != clean {
				t.Errorf("failed Resolve URL %q, want Clean(raw) %q", res.URL, clean)
			}
			if len(res.Hops) != 0 {
				t.Errorf("failed Resolve has hops %+v", res.Hops)
			}
		}
		if (err == nil) != (perr == nil) {
			t.Errorf("Resolve error %v, Process error %v", err, perr)
		}
		for _, e := range []error{err, perr} {
			if e != nil && strings.Contains(e.Error(), rvToken) {
				t.Errorf("error %q holds the token", e)
			}
		}
		if got := c.Clean(res.URL); got != res.URL {
			t.Errorf("Clean(URL) = %q, want URL %q", got, res.URL)
		}
		if pr.URL != res.URL {
			t.Errorf("Process URL %q, Resolve URL %q", pr.URL, res.URL)
		}
		if len(res.Hops) > limit {
			t.Errorf("%d hops, MaxHops %d", len(res.Hops), limit)
		}
		for _, h := range res.Hops {
			ru, skip := rawurl.Parse(h.From)
			pu, perr := url.Parse(h.From)
			if skip != rawurl.OK || perr != nil || pu.RequestURI() != ru.PathForRequest() {
				t.Errorf("hop From %q is not sent as written", h.From)
			}
		}
		network := 0
		for _, h := range pr.Hops {
			if strings.HasPrefix(h.Rule, "unshorten:") {
				network++
				if h.Category != linkclean.CatUnshortened {
					t.Errorf("network hop %+v is not CatUnshortened", h)
				}
			} else if h.Category == linkclean.CatUnshortened {
				t.Errorf("hop %+v is CatUnshortened without an unshorten rule", h)
			}
		}
		if network != len(res.Hops) {
			t.Errorf("Process reports %d network hops, Resolve %d", network, len(res.Hops))
		}
	})
}

// FuzzNextURL resolves a Location against a base, as Get does, and
// validates it. An accepted next URL is at most 8 KiB, parses, passes
// rawurl.ParseTarget with a port of "", 80 or 443, and validates to
// itself again. buildRequest then accepts it only if rawurl.Parse does,
// and only with a request URI equal to its request form's path and query.
func FuzzNextURL(f *testing.F) {
	for _, s := range [][2]string{
		{"https://t.co/abc", "https://example.net/p?utm_source=x"},
		{"https://t.co/abc", "/b?x=1"},
		{"https://t.co/abc", "//example.net/p"},
		{"https://t.co/abc", "http://example.net/p"},
		{"https://t.co/abc", "https://example.net:8080/"},
		{"https://t.co/abc", "https://u:p@example.net/"},
		{"https://t.co/abc", "https://127.1/"},
		{"https://t.co/abc", "https://0x7f000001/"},
		{"https://t.co/abc", "https://\uff11\uff12\uff17\uff0e\uff10\uff0e\uff10\uff0e\uff11/"},
		{"https://t.co/abc", "https://[::ffff:7f00:1]/"},
		{"https://t.co/abc", "https://intranet/"},
		{"https://t.co/abc", "https://example.net/caf\xc3\xa9?q=\xe9"},
		{"https://t.co/abc", "https://ex\xc3\xa4mple.net/"},
		{"https://t.co/abc", "https://example.net/a%2Fb?x=%zz#f"},
		{"https://t.co/abc", "https://example.net/%zz"},
		{"https://t.co/abc", "https://example.net/a b"},
		{"https://t.co/abc", "https://EXAMPLE.net./p?"},
		{"https://t.co/abc", "https://example.net/" + strings.Repeat("a", 8200)},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, base, loc string) {
		b, err := url.Parse(base)
		if err != nil || !b.IsAbs() {
			return
		}
		u, err := b.Parse(loc)
		if err != nil {
			return
		}
		next, ok := validateNext(u)
		if !ok {
			return
		}
		if len(next) > maxNext {
			t.Fatalf("accepted %d bytes", len(next))
		}
		parsed, err := url.Parse(next)
		if err != nil {
			t.Fatalf("accepted %q, which url.Parse refuses: %v", next, err)
		}
		target, ok := rawurl.ParseTarget(next)
		if !ok {
			t.Fatalf("accepted %q, which rawurl.ParseTarget refuses", next)
		}
		switch target.Port {
		case "", "80", "443":
		default:
			t.Fatalf("accepted %q with port %q", next, target.Port)
		}
		if again, ok := validateNext(parsed); !ok || again != next {
			t.Fatalf("validateNext(%q) = %q, %v; not idempotent", next, again, ok)
		}

		rv, skip := rawurl.Parse(next)
		host := target.Host
		if skip == rawurl.OK {
			host = rv.Host
		}
		e := &compiledEntry{Entry: Entry{Name: "fuzz", AllowHTTP: true}, hosts: []string{host}}
		req, gotHost, err := buildRequest(context.Background(), parsed, e)
		if err != nil {
			return
		}
		if skip != rawurl.OK {
			t.Fatalf("buildRequest accepted %q, which rawurl.Parse refuses (%v)", next, skip)
		}
		if got, want := req.URL.RequestURI(), rv.PathForRequest(); got != want {
			t.Fatalf("buildRequest(%q): RequestURI %q, want %q", next, got, want)
		}
		if gotHost != rv.Host {
			t.Fatalf("buildRequest(%q): host %q, want %q", next, gotHost, rv.Host)
		}
	})
}

// FuzzExtract runs every body extractor on a fuzzed body. None may
// panic; an extractor fails only with ErrNoMatch; what one returns is
// validated or rejected, never a panic, and an accepted next URL passes
// rawurl.ParseTarget. The oEmbed extractor returns only a canonical
// video or photo URL.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{
		`<a class="item-detail--url" href="https://example.net/dest?utm_source=bitly" title="x">x</a>`,
		`<a id="long-link" href="https://example.net/long?a=1&amp;b=2">x</a>`,
		`<a id="long-link" href="example.net/no-scheme">x</a>`,
		`<a data-tracking-control-name="external_url_click" href="https://example.net/l?x=1&amp;y=2">x</a>`,
		`<a data-tracking-control-name="external_url_click" href="/relative">x</a>`,
		`{"html":"<blockquote class=\"tiktok-embed\" cite=\"https://www.tiktok.com/@u/video/123\" data-video-id=\"123\"></blockquote>","embed_product_id":"123"}`,
		`{"html":"<blockquote cite=\"https://www.tiktok.com/@u/photo/7\"></blockquote>","embed_product_id":7}`,
		`{"html":"<blockquote cite=\"https://www.tiktok.com/@u/video/1\"></blockquote>","embed_product_id":"2"}`,
		`{"html":5}`,
		``,
	} {
		f.Add([]byte(s))
	}
	base, err := url.Parse("https://lnkd.in/" + rvToken)
	if err != nil {
		f.Fatal(err)
	}
	type extractor struct {
		name    string
		extract func([]byte) (string, error)
		resolve func(string) (*url.URL, error) // as the strategy turns the text into a URL
	}
	extractors := []extractor{
		{"BodyRegexp", func(b []byte) (string, error) { return extractRegexp(lnkdLink, b) }, base.Parse},
		{"TinyURLPreview", extractTinyURL, parseAbsolute},
		{"BitlyInfo", extractBitly, parseAbsolute},
		{"TikTokOEmbed", extractOEmbed, parseAbsolute},
	}
	canonical := regexp.MustCompile(`^https://www\.tiktok\.com/@[A-Za-z0-9._-]+/(?:video|photo)/[0-9]+$`)
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, x := range extractors {
			s, err := x.extract(body)
			if err != nil {
				if !errors.Is(err, ErrNoMatch) {
					t.Errorf("%s: error %v, want ErrNoMatch", x.name, err)
				}
				continue
			}
			if x.name == "TikTokOEmbed" && !canonical.MatchString(s) {
				t.Errorf("TikTokOEmbed returned %q, not a canonical URL", s)
			}
			u, err := x.resolve(s)
			if err != nil {
				continue
			}
			if next, ok := validateNext(u); ok {
				if _, ok := rawurl.ParseTarget(next); !ok {
					t.Errorf("%s: accepted %q, which rawurl.ParseTarget refuses", x.name, next)
				}
			}
		}
	})
}
