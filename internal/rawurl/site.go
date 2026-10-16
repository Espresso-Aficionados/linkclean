package rawurl

import (
	"strings"

	"golang.org/x/net/publicsuffix"
)

// Site returns the ICANN-only eTLD+1 of a host view (§4.1): it walks
// publicsuffix.PublicSuffix past private suffixes until it reaches an ICANN
// one, then takes one more label. So amazon.github.io has site github.io, and
// www.amazon.co.uk has site amazon.co.uk. Site is "" for single-label hosts,
// hosts that are themselves a public suffix, hosts under a TLD the list does
// not know, and hosts whose site label would be empty. Callers handle IP
// literals themselves (an IP has no site).
//
// The result is a substring of host. Single-label hosts return without
// consulting the list; otherwise each lookup allocates, so call Site lazily.
func Site(host string) string {
	if strings.IndexByte(host, '.') < 0 {
		return ""
	}
	d := host
	ps := ""
	for {
		var icann bool
		ps, icann = publicsuffix.PublicSuffix(d)
		if icann {
			break
		}
		k := strings.IndexByte(ps, '.')
		if k < 0 {
			return ""
		}
		d = ps[k+1:]
	}
	n := len(host) - len(ps) - 1 // index of the '.' before ps
	if ps == "" || n <= 0 || host[n] != '.' || host[n+1:] != ps {
		return ""
	}
	label := host[strings.LastIndexByte(host[:n], '.')+1 : n]
	if label == "" {
		return ""
	}
	return host[n-len(label):]
}
