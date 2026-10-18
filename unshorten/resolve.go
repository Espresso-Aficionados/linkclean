package unshorten

import "net/url"

// hopKey is the Cache and flight key of a hop: e's name, NUL, the names of
// the strategies eligible under p joined by ',', NUL, and the hop URL in
// request form. Entry names hold no NUL, so the key up to its first NUL is
// always the entry name.
func hopKey(e *compiledEntry, p GenericPolicy, reqForm string) string {
	primary, fallback := e.eligible(p)
	names := ""
	if primary != nil {
		names = primary.Name()
	}
	if fallback != nil {
		names += "," + fallback.Name()
	}
	return e.Name + "\x00" + names + "\x00" + reqForm
}

// validateNext validates a strategy's next URL: at most 8 KiB as
// u.String(), accepted by rawurl.ParseTarget, port "", 80 or 443. next is
// u.String().
func validateNext(u *url.URL) (next string, ok bool) {
	panic("unshorten: not implemented")
}
