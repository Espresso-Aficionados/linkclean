package unshorten

import (
	"context"
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strings"
)

// Strategy turns one URL on its entry into the next URL. All I/O goes through f.
//
// A Fetcher is valid only during the Next call it was passed to; Get after
// Next returns fails with ErrBudget. Do not call Resolve or Process from a
// Strategy: a Strategy that resolves its own key waits on itself until the
// hop deadline. Next may return ErrNotShortLink to stop resolution without a
// failure. Any error other than the package's sentinels, a *StatusError or
// an error from Get counts as transient.
type Strategy interface {
	Name() string
	Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error)
}

// Redirect returns the strategy named "Redirect": GET; 301/302/303/307/308 +
// Location ⇒ next; 429, 502, 503 and 504 are a transient *StatusError; any
// other status is ErrNoRedirect.
func Redirect() Strategy {
	return redirectStrategy{}
}

type redirectStrategy struct{}

func (redirectStrategy) Name() string { return "Redirect" }

func (redirectStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	resp, err := f.Get(ctx, u, false)
	if err != nil {
		return nil, err
	}
	switch resp.Status {
	case 301, 302, 303, 307, 308:
		if resp.Location != nil {
			return resp.Location, nil
		}
	}
	if transientStatus(resp.Status) {
		return nil, &StatusError{Host: u.Hostname(), Status: resp.Status}
	}
	return nil, ErrNoRedirect
}

// BodyRegexp returns the strategy named "BodyRegexp": GET (body); a status
// other than 200 is a *StatusError (transient for 429, 502, 503 and 504);
// group 1 of re's first match, html.UnescapeString, resolved against u; no
// match is ErrNoMatch. It panics if re has no capturing group.
func BodyRegexp(re *regexp.Regexp) Strategy {
	if re == nil || re.NumSubexp() < 1 {
		panic("unshorten: BodyRegexp needs a regexp with a capturing group")
	}
	return bodyRegexpStrategy{re: re}
}

type bodyRegexpStrategy struct{ re *regexp.Regexp }

func (bodyRegexpStrategy) Name() string { return "BodyRegexp" }

func (s bodyRegexpStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	body, err := getBody(ctx, f, u)
	if err != nil {
		return nil, err
	}
	t, err := extractRegexp(s.re, body)
	if err != nil {
		return nil, err
	}
	next, err := u.Parse(t)
	if err != nil {
		return nil, ErrBadTarget
	}
	return next, nil
}

// TikTokOEmbed returns the strategy named "TikTokOEmbed": GET
// https://www.tiktok.com/oembed?url=<u>. The canonical URL is the cite URL of
// the blockquote in the JSON's html (https://www.tiktok.com/@<user>/video/<id>
// or /photo/<id>), checked against embed_product_id when that is present;
// anything else is ErrNoMatch.
func TikTokOEmbed() Strategy {
	return tiktokStrategy{}
}

type tiktokStrategy struct{}

func (tiktokStrategy) Name() string { return "TikTokOEmbed" }

func (tiktokStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	oe := &url.URL{Scheme: "https", Host: "www.tiktok.com", Path: "/oembed", RawQuery: "url=" + url.QueryEscape(u.String())}
	body, err := getBody(ctx, f, oe)
	if err != nil {
		return nil, err
	}
	t, err := extractOEmbed(body)
	if err != nil {
		return nil, err
	}
	return parseAbsolute(t)
}

// TinyURLPreview returns the strategy named "TinyURLPreview": GET
// tinyurl.com/preview/<alias>; destination from the preview page, which must
// carry a scheme (else ErrNoMatch). A URL without an alias (an empty first
// path segment) is ErrNotShortLink, with no request.
func TinyURLPreview() Strategy {
	return tinyURLStrategy{}
}

type tinyURLStrategy struct{}

func (tinyURLStrategy) Name() string { return "TinyURLPreview" }

func (tinyURLStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	alias := firstSegment(u)
	if alias == "" {
		return nil, ErrNotShortLink
	}
	pu, err := url.Parse("https://tinyurl.com/preview/" + alias)
	if err != nil {
		return nil, ErrBadTarget
	}
	body, err := getBody(ctx, f, pu)
	if err != nil {
		return nil, err
	}
	t, err := extractTinyURL(body)
	if err != nil {
		return nil, err
	}
	return parseAbsolute(t)
}

// BitlyInfo returns the strategy named "BitlyInfo": GET bit.ly/<id>+ ;
// destination from the info page. A URL without an id (an empty first path
// segment) is ErrNotShortLink, with no request.
func BitlyInfo() Strategy {
	return bitlyStrategy{}
}

type bitlyStrategy struct{}

func (bitlyStrategy) Name() string { return "BitlyInfo" }

func (bitlyStrategy) Next(ctx context.Context, f *Fetcher, u *url.URL) (*url.URL, error) {
	// A posted info-page link (bit.ly/<id>+) asks for the same page.
	id := strings.TrimSuffix(firstSegment(u), "+")
	if id == "" {
		return nil, ErrNotShortLink
	}
	iu, err := url.Parse("https://bit.ly/" + id + "+")
	if err != nil {
		return nil, ErrBadTarget
	}
	body, err := getBody(ctx, f, iu)
	if err != nil {
		return nil, err
	}
	t, err := extractBitly(body)
	if err != nil {
		return nil, err
	}
	return parseAbsolute(t)
}

// getBody GETs u with its body and requires status 200: any other status is
// a *StatusError, which classifies as transient for 429, 502, 503 and 504.
func getBody(ctx context.Context, f *Fetcher, u *url.URL) ([]byte, error) {
	resp, err := f.Get(ctx, u, true)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, &StatusError{Host: u.Hostname(), Status: resp.Status}
	}
	return resp.Body, nil
}

// firstSegment returns the first segment of u's escaped path.
func firstSegment(u *url.URL) string {
	seg, _, _ := strings.Cut(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	return seg
}

// parseAbsolute parses an extracted destination that must be absolute: one
// without a scheme and host is ErrNoMatch (a preview page's scheme-less
// value), one that does not parse is ErrBadTarget.
func parseAbsolute(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, ErrBadTarget
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, ErrNoMatch
	}
	return u, nil
}

// The extractors are pure functions of the body. Each returns the
// HTML-unescaped destination text, or ErrNoMatch.

// extractRegexp returns group 1 of re's first match in body.
func extractRegexp(re *regexp.Regexp, body []byte) (string, error) {
	m := re.FindSubmatch(body)
	if m == nil || len(m) < 2 || len(m[1]) == 0 {
		return "", ErrNoMatch
	}
	return html.UnescapeString(string(m[1])), nil
}

// tinyURLLink is the preview page's destination anchor (capture of
// 2026-10-15): <a id="long-link" href="…">.
var tinyURLLink = regexp.MustCompile(`<a\s[^>]*\bid="long-link"[^>]*\bhref="([^"]*)"`)

func extractTinyURL(body []byte) (string, error) {
	return extractRegexp(tinyURLLink, body)
}

// bitlyLink is the info page's first destination anchor (capture of
// 2026-10-15): <a class="item-detail--url" href="…">.
var bitlyLink = regexp.MustCompile(`<a\s[^>]*\bclass="item-detail--url"[^>]*\bhref="([^"]*)"`)

func extractBitly(body []byte) (string, error) {
	return extractRegexp(bitlyLink, body)
}

// oembedCite is the cite attribute of the blockquote in an oEmbed html.
var oembedCite = regexp.MustCompile(`<blockquote\s[^>]*\bcite="([^"]*)"`)

// tiktokCanonical is the canonical URL a cite must be: a video or photo
// page, capturing its id.
var tiktokCanonical = regexp.MustCompile(`^https://www\.tiktok\.com/@[A-Za-z0-9._-]+/(?:video|photo)/([0-9]+)$`)

// extractOEmbed returns the canonical URL from TikTok's oEmbed JSON: the
// blockquote's cite in html, which must be a video or photo URL whose id
// equals embed_product_id when that is present (a string or a number).
func extractOEmbed(body []byte) (string, error) {
	var doc struct {
		HTML      string          `json:"html"`
		ProductID json.RawMessage `json:"embed_product_id"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", ErrNoMatch
	}
	cite, err := extractRegexp(oembedCite, []byte(doc.HTML))
	if err != nil {
		return "", err
	}
	m := tiktokCanonical.FindStringSubmatch(cite)
	if m == nil {
		return "", ErrNoMatch
	}
	if len(doc.ProductID) > 0 && string(doc.ProductID) != "null" {
		var id string
		if json.Unmarshal(doc.ProductID, &id) != nil {
			var n json.Number
			if json.Unmarshal(doc.ProductID, &n) != nil {
				return "", ErrNoMatch
			}
			id = n.String()
		}
		if id != m[1] {
			return "", ErrNoMatch
		}
	}
	return cite, nil
}
