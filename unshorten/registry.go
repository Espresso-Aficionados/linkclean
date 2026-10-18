package unshorten

import (
	"net/url"

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
// a non-nil Strategy and a valid Kind.
func NewRegistry(entries ...Entry) (*Registry, error) {
	panic("unshorten: not implemented")
}

// With returns a copy of r with entries added, or replaced by Name.
func (r *Registry) With(entries ...Entry) (*Registry, error) {
	panic("unshorten: not implemented")
}

// Without returns a copy of r without the named entries; an unknown name is
// an error.
func (r *Registry) Without(names ...string) (*Registry, error) {
	panic("unshorten: not implemented")
}

// Entries returns copies of r's entries, sorted by Name.
func (r *Registry) Entries() []Entry {
	panic("unshorten: not implemented")
}

// lookup returns the first entry, in Name order among those for u's host,
// whose patterns match u and which is eligible under p; Intermediate
// entries only when intermediate is set. A URL that is not requestable
// (userinfo, an IP host, a port other than "" or its scheme's default)
// never matches. Every lookup of the resolver, and Match, use it.
func (r *Registry) lookup(u *rawurl.URL, intermediate bool, p GenericPolicy) (*compiledEntry, bool) {
	panic("unshorten: not implemented")
}

// eligible returns the strategies e may use under p: FirstParty its Strategy
// and Fallback; Marketing its Strategy under PreviewOnly, both under Follow,
// none under Skip. fallback is nil when e has none or it is not eligible;
// primary is nil only when e is not eligible at all.
func (e *compiledEntry) eligible(p GenericPolicy) (primary, fallback Strategy) {
	panic("unshorten: not implemented")
}
