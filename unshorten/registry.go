package unshorten

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// Kind is the kind of shortener an Entry describes.
type Kind uint8

const (
	FirstParty Kind = iota // platform share links: t.co, a.co, pin.it, reddit /s/
	Marketing              // generic shorteners with per-recipient campaign links: subject to GenericPolicy
)

// Entry describes one shortener.
type Entry struct {
	Name string // unique and non-empty, without NUL or ',', e.g. "t.co", "reddit-share"
	// Match: linkclean match patterns (same grammar as Rule.Include). Each
	// must use scheme "*", an exact host and no port ("*://t.co/*"); other
	// patterns are rejected (an https-only pattern would make http:// short
	// links silently never match). Matching ignores the scheme.
	Match []string
	// Intermediate entries are only resolvable when reached as a hop, never from
	// user input (api.pinterest.com, www.google.com/share.google…).
	Intermediate bool
	// FetchHosts are extra hosts the strategy may contact (oEmbed/preview
	// hosts), each lowercase ASCII without a port and not an IP. The Fetcher
	// refuses any other host with ErrFetchDenied.
	FetchHosts []string
	Strategy   Strategy
	Fallback   Strategy // optional: FirstParty after Strategy fails, or Marketing under Follow
	Kind       Kind
	// AllowHTTP lets the Fetcher request plain http URLs for this entry, and an
	// http input is then requested over http. Without it an http input is
	// upgraded to https before any request.
	AllowHTTP bool
	// IsFallback detects THIS shortener's own fallback/homepage targets (e.g.
	// amzn.to → amazon.<tld>/). It is platform-specific; a legitimate target that
	// is some other site's homepage is never a fallback. nil means none.
	IsFallback func(short, target *url.URL) bool
}

// Registry is immutable; With and Without return modified copies.
type Registry struct {
	entries []*compiledEntry            // sorted by Name
	byHost  map[string][]*compiledEntry // Match host → entries, in Name order
}

// compiledEntry is an Entry with its patterns compiled and its hosts.
type compiledEntry struct {
	Entry
	pats  []*matchpattern.Pattern
	hosts []string // Match hosts ∪ FetchHosts: the Fetcher allowlist, sorted, unique
}

// NewRegistry validates and compiles entries: names unique, non-empty and
// without NUL or ','; Match non-empty, each pattern with scheme "*", an exact
// host and no port; FetchHosts lowercase ASCII without port and not an IP;
// a non-nil Strategy and a valid Kind. The Registry keeps its own copies of
// the Match and FetchHosts slices.
func NewRegistry(entries ...Entry) (*Registry, error) {
	return build(nil, entries, nil)
}

// With returns a copy of r with entries added, or replaced by Name.
func (r *Registry) With(entries ...Entry) (*Registry, error) {
	return build(r, entries, nil)
}

// Without returns a copy of r without the named entries; an unknown name is
// an error.
func (r *Registry) Without(names ...string) (*Registry, error) {
	var errs []error
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		if r.entry(n) == nil {
			errs = append(errs, fmt.Errorf("unshorten: Registry.Without: no entry named %q", n))
		}
		drop[n] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return build(r, nil, drop)
}

// Entries returns copies of r's entries, sorted by Name.
func (r *Registry) Entries() []Entry {
	if r == nil {
		return nil
	}
	out := make([]Entry, len(r.entries))
	for i, ce := range r.entries {
		out[i] = copyEntry(ce.Entry)
	}
	return out
}

// entry returns the compiled entry named name, or nil.
func (r *Registry) entry(name string) *compiledEntry {
	if r == nil {
		return nil
	}
	i, ok := slices.BinarySearchFunc(r.entries, name, func(ce *compiledEntry, n string) int {
		return strings.Compare(ce.Name, n)
	})
	if !ok {
		return nil
	}
	return r.entries[i]
}

// build returns a new Registry holding base's entries except those named in
// drop, with add compiled and added or replacing base's by Name. Compiled
// entries are immutable, so base's are shared, not recompiled.
func build(base *Registry, add []Entry, drop map[string]bool) (*Registry, error) {
	byName := map[string]*compiledEntry{}
	if base != nil {
		for _, ce := range base.entries {
			if !drop[ce.Name] {
				byName[ce.Name] = ce
			}
		}
	}
	var errs []error
	added := make(map[string]bool, len(add))
	for i := range add {
		ce, err := compile(add[i])
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if added[ce.Name] {
			errs = append(errs, fmt.Errorf("unshorten: registry entry %q: duplicate name", ce.Name))
			continue
		}
		added[ce.Name] = true
		byName[ce.Name] = ce
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	r := &Registry{byHost: map[string][]*compiledEntry{}}
	for _, ce := range byName {
		r.entries = append(r.entries, ce)
	}
	slices.SortFunc(r.entries, func(a, b *compiledEntry) int { return strings.Compare(a.Name, b.Name) })
	for _, ce := range r.entries {
		var hosts []string
		for _, p := range ce.pats {
			_, h := p.Host()
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
				r.byHost[h] = append(r.byHost[h], ce)
			}
		}
	}
	return r, nil
}

// compile validates e and compiles its patterns.
func compile(e Entry) (*compiledEntry, error) {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("unshorten: registry entry %q: "+format, append([]any{e.Name}, args...)...))
	}
	switch {
	case e.Name == "":
		bad("empty name")
	case strings.ContainsAny(e.Name, "\x00,"):
		bad("name contains NUL or ','")
	}
	if len(e.Match) == 0 {
		bad("no Match patterns")
	}
	ce := &compiledEntry{Entry: copyEntry(e)}
	for _, p := range e.Match {
		pat, err := compilePattern(p)
		if err != nil {
			bad("Match %q: %v", p, err)
			continue
		}
		ce.pats = append(ce.pats, pat)
		_, h := pat.Host()
		ce.hosts = append(ce.hosts, h)
	}
	for _, h := range e.FetchHosts {
		if !isHostView(h) {
			bad("FetchHosts %q: not a lowercase ASCII host name without a port", h)
			continue
		}
		ce.hosts = append(ce.hosts, h)
	}
	if e.Strategy == nil {
		bad("nil Strategy")
	}
	if e.Kind > Marketing {
		bad("invalid Kind %d", e.Kind)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	slices.Sort(ce.hosts)
	ce.hosts = slices.Compact(ce.hosts)
	return ce, nil
}

// compilePattern parses a registry Match pattern: scheme "*", an exact host
// that is not an IP literal, and no port. Matching then ignores the scheme,
// and the pattern's host is a host the Fetcher may contact.
func compilePattern(p string) (*matchpattern.Pattern, error) {
	scheme, rest, ok := strings.Cut(p, "://")
	if !ok {
		return nil, errors.New(`missing "://"`)
	}
	switch scheme {
	case "*":
	case "https":
		return nil, errors.New(`https-only pattern: http:// links would never match; use scheme "*"`)
	default:
		return nil, errors.New(`scheme must be "*"`)
	}
	auth, _, _ := strings.Cut(rest, "/")
	if strings.ContainsAny(auth, ":[]") {
		return nil, errors.New("a port or IP literal is not allowed")
	}
	pat, err := matchpattern.Parse(p, false)
	if err != nil {
		return nil, err
	}
	kind, host := pat.Host()
	if kind != matchpattern.HostExact {
		return nil, errors.New("host must be exact (no '*')")
	}
	if !isHostView(host) {
		return nil, errors.New("host must be a host name, not an IP literal")
	}
	return pat, nil
}

// isHostView reports whether h is a non-empty lowercase ASCII host name that
// is its own host view (see rawurl.Parse), without a port and not an IP.
func isHostView(h string) bool {
	if h == "" || strings.ContainsAny(h, ":/?#@[]") {
		return false
	}
	for i := 0; i < len(h); i++ {
		if c := h[i]; c >= 0x80 || 'A' <= c && c <= 'Z' {
			return false
		}
	}
	u, skip := rawurl.Parse("https://" + h + "/")
	return skip == rawurl.OK && u.Host == h && !u.IsIP
}

// copyEntry returns e with its slices copied.
func copyEntry(e Entry) Entry {
	e.Match = slices.Clone(e.Match)
	e.FetchHosts = slices.Clone(e.FetchHosts)
	return e
}

// lookup returns the first entry, in Name order among those for u's host,
// whose patterns match u and which is eligible under p; Intermediate
// entries only when intermediate is set. A URL that is not requestable
// (userinfo, an IP host, a port other than "" or its scheme's default)
// never matches. Every lookup of the resolver, and Match, use it.
func (r *Registry) lookup(u *rawurl.URL, intermediate bool, p GenericPolicy) (*compiledEntry, bool) {
	if r == nil || !requestable(u) {
		return nil, false
	}
	cands := r.byHost[u.Host]
	if len(cands) == 0 {
		return nil, false
	}
	t := matchpattern.Target{Scheme: u.Scheme, Host: u.Host, IsIP: u.IsIP, Port: u.EffectivePort()}
	t.SetPath(u.PathForRequest())
	for _, e := range cands {
		if e.Intermediate && !intermediate {
			continue
		}
		if primary, _ := e.eligible(p); primary == nil {
			continue
		}
		for _, pat := range e.pats {
			if pat.Match(&t) {
				return e, true
			}
		}
	}
	return nil, false
}

// requestable reports whether u is a URL the Fetcher could request: no
// userinfo, not an IP host, and a port that is "" or its scheme's default.
func requestable(u *rawurl.URL) bool {
	if u.Host == "" || u.IsIP || strings.IndexByte(u.Authority, '@') >= 0 {
		return false
	}
	return u.Port == "" || u.Port == defaultPort(u.Scheme)
}

// defaultPort is the port of scheme "https" or "http".
func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

// eligible returns the strategies e may use under p: FirstParty its Strategy
// and Fallback; Marketing its Strategy under PreviewOnly, both under Follow,
// none under Skip. fallback is nil when e has none or it is not eligible;
// primary is nil only when e is not eligible at all.
func (e *compiledEntry) eligible(p GenericPolicy) (primary, fallback Strategy) {
	if e.Kind == FirstParty {
		return e.Strategy, e.Fallback
	}
	switch p {
	case PreviewOnly:
		return e.Strategy, nil
	case Follow:
		return e.Strategy, e.Fallback
	}
	return nil, nil
}
