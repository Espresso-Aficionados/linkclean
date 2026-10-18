package unshorten

import (
	"context"
	"net/url"
	"regexp"
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
	panic("unshorten: not implemented")
}

// BodyRegexp returns the strategy named "BodyRegexp": GET (body); a status
// other than 200 is a *StatusError (transient for 429, 502, 503 and 504);
// group 1 of re's first match, html.UnescapeString, resolved against u; no
// match is ErrNoMatch. It panics if re has no capturing group.
func BodyRegexp(re *regexp.Regexp) Strategy {
	panic("unshorten: not implemented")
}

// TikTokOEmbed returns the strategy named "TikTokOEmbed": GET
// https://www.tiktok.com/oembed?url=<u>. The canonical URL is the cite URL of
// the blockquote in the JSON's html (https://www.tiktok.com/@<user>/video/<id>
// or /photo/<id>), checked against embed_product_id when that is present;
// anything else is ErrNoMatch.
func TikTokOEmbed() Strategy {
	panic("unshorten: not implemented")
}

// TinyURLPreview returns the strategy named "TinyURLPreview": GET
// tinyurl.com/preview/<alias>; destination from the preview page, which must
// carry a scheme (else ErrNoMatch).
func TinyURLPreview() Strategy {
	panic("unshorten: not implemented")
}

// BitlyInfo returns the strategy named "BitlyInfo": GET bit.ly/<id>+ ;
// destination from the info page.
func BitlyInfo() Strategy {
	panic("unshorten: not implemented")
}
