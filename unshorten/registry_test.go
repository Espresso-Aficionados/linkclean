package unshorten

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
)

// namedStrategy is a Strategy that only has a name.
type namedStrategy string

func (s namedStrategy) Name() string { return string(s) }

func (namedStrategy) Next(context.Context, *Fetcher, *url.URL) (*url.URL, error) {
	return nil, ErrNoMatch
}

func TestNewRegistryValidation(t *testing.T) {
	ok := Entry{Name: "ok", Match: []string{"*://ok.example/*"}, Strategy: Redirect()}
	with := func(f func(*Entry)) Entry {
		e := ok
		e.Match = slices.Clone(ok.Match)
		f(&e)
		return e
	}
	tests := []struct {
		name string
		e    Entry
		want string // substring of the error
	}{
		{"empty name", with(func(e *Entry) { e.Name = "" }), "empty name"},
		{"NUL name", with(func(e *Entry) { e.Name = "a\x00b" }), "NUL or ','"},
		{"comma name", with(func(e *Entry) { e.Name = "a,b" }), "NUL or ','"},
		{"no Match", with(func(e *Entry) { e.Match = nil }), "no Match"},
		{"https-only", with(func(e *Entry) { e.Match = []string{"https://ok.example/*"} }), "https-only"},
		{"https-only AllowHTTP", with(func(e *Entry) { e.Match = []string{"https://ok.example/*"}; e.AllowHTTP = true }), "https-only"},
		{"http-only", with(func(e *Entry) { e.Match = []string{"http://ok.example/*"} }), `scheme must be "*"`},
		{"no scheme", with(func(e *Entry) { e.Match = []string{"ok.example/*"} }), `missing "://"`},
		{"port", with(func(e *Entry) { e.Match = []string{"*://ok.example:443/*"} }), "port"},
		{"IPv4", with(func(e *Entry) { e.Match = []string{"*://127.0.0.1/*"} }), "IP literal"},
		{"IPv6", with(func(e *Entry) { e.Match = []string{"*://[::1]/*"} }), "port or IP literal"},
		{"wildcard host", with(func(e *Entry) { e.Match = []string{"*://*/*"} }), "exact"},
		{"suffix host", with(func(e *Entry) { e.Match = []string{"*://*.ok.example/*"} }), "exact"},
		{"label host", with(func(e *Entry) { e.Match = []string{"*://ok.*/*"} }), "not allowed"},
		{"no path", with(func(e *Entry) { e.Match = []string{"*://ok.example"} }), "missing path"},
		{"FetchHost upper", with(func(e *Entry) { e.FetchHosts = []string{"Www.ok.example"} }), "FetchHosts"},
		{"FetchHost port", with(func(e *Entry) { e.FetchHosts = []string{"www.ok.example:443"} }), "FetchHosts"},
		{"FetchHost IP", with(func(e *Entry) { e.FetchHosts = []string{"10.0.0.1"} }), "FetchHosts"},
		{"FetchHost IPv6", with(func(e *Entry) { e.FetchHosts = []string{"[::1]"} }), "FetchHosts"},
		{"FetchHost empty", with(func(e *Entry) { e.FetchHosts = []string{""} }), "FetchHosts"},
		{"FetchHost dot", with(func(e *Entry) { e.FetchHosts = []string{"www.ok.example."} }), "FetchHosts"},
		{"FetchHost non-ASCII", with(func(e *Entry) { e.FetchHosts = []string{"ｗww.ok.example"} }), "FetchHosts"},
		{"FetchHost path", with(func(e *Entry) { e.FetchHosts = []string{"www.ok.example/x"} }), "FetchHosts"},
		{"FetchHost userinfo", with(func(e *Entry) { e.FetchHosts = []string{"u@www.ok.example"} }), "FetchHosts"},
		{"nil Strategy", with(func(e *Entry) { e.Strategy = nil }), "nil Strategy"},
		{"bad Kind", with(func(e *Entry) { e.Kind = 2 }), "invalid Kind"},
	}
	for _, tt := range tests {
		r, err := NewRegistry(tt.e)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: NewRegistry = %v, %v; want an error containing %q", tt.name, r, err, tt.want)
		}
	}
	if _, err := NewRegistry(ok, ok); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate names: %v", err)
	}
	// Every problem is reported, not only the first.
	_, err := NewRegistry(Entry{Name: "x,y", FetchHosts: []string{"A"}})
	for _, want := range []string{"NUL or ','", "no Match", "FetchHosts", "nil Strategy"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("joined error %v lacks %q", err, want)
		}
	}
	// Valid shapes: a canonicalized host, an http-capable entry, FetchHosts.
	for _, e := range []Entry{
		ok,
		with(func(e *Entry) { e.Match = []string{"*://OK.Example./*"} }),
		with(func(e *Entry) { e.AllowHTTP = true; e.Kind = Marketing; e.Fallback = Redirect() }),
		with(func(e *Entry) { e.FetchHosts = []string{"www.ok.example", "xn--bcher-kva.example", "a_b.example"} }),
	} {
		if _, err := NewRegistry(e); err != nil {
			t.Errorf("NewRegistry(%+v) = %v", e, err)
		}
	}
}

func TestRegistryHosts(t *testing.T) {
	r, err := NewRegistry(Entry{
		Name: "e", Match: []string{"*://b.example/x/*", "*://A.example/*", "*://b.example/y/*"},
		FetchHosts: []string{"c.example", "a.example"}, Strategy: Redirect(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e := r.entry("e")
	if want := []string{"a.example", "b.example", "c.example"}; !slices.Equal(e.hosts, want) {
		t.Errorf("hosts %q, want %q", e.hosts, want)
	}
	// byHost holds Match hosts only, each entry once.
	if len(r.byHost) != 2 || len(r.byHost["a.example"]) != 1 || len(r.byHost["b.example"]) != 1 || r.byHost["c.example"] != nil {
		t.Errorf("byHost %v", r.byHost)
	}
}

func TestRegistryWithWithout(t *testing.T) {
	a := Entry{Name: "a", Match: []string{"*://a.example/*"}, Strategy: Redirect()}
	b := Entry{Name: "b", Match: []string{"*://b.example/*"}, Strategy: Redirect()}
	r, err := NewRegistry(b, a)
	if err != nil {
		t.Fatal(err)
	}
	b2 := Entry{Name: "b", Match: []string{"*://b2.example/*"}, Strategy: BitlyInfo()}
	c := Entry{Name: "c", Match: []string{"*://c.example/*"}, Strategy: Redirect()}
	r2, err := r.With(c, b2)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(r2.Entries()); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("With: %q", got)
	}
	if got := r2.entry("b"); got.Match[0] != "*://b2.example/*" || got.Strategy.Name() != "BitlyInfo" {
		t.Errorf("With did not replace b: %+v", got.Entry)
	}
	if r2.byHost["b.example"] != nil || len(r2.byHost["b2.example"]) != 1 {
		t.Errorf("With: byHost %v", r2.byHost)
	}
	// r is unchanged.
	if got := names(r.Entries()); !slices.Equal(got, []string{"a", "b"}) || r.entry("b").Match[0] != "*://b.example/*" {
		t.Errorf("r changed: %q", got)
	}
	if _, err := r.With(c, c); err == nil {
		t.Error("With: duplicate names accepted")
	}
	if _, err := r.With(Entry{Name: "bad"}); err == nil {
		t.Error("With: invalid entry accepted")
	}

	r3, err := r2.Without("a", "c")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(r3.Entries()); !slices.Equal(got, []string{"b"}) || r3.byHost["a.example"] != nil {
		t.Errorf("Without: %q, byHost %v", got, r3.byHost)
	}
	if got := names(r2.Entries()); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("r2 changed: %q", got)
	}
	if _, err := r2.Without("a", "nope"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("Without unknown: %v", err)
	}
	empty, err := r3.Without("b")
	if err != nil || len(empty.Entries()) != 0 {
		t.Errorf("Without all: %v, %v", empty.Entries(), err)
	}
}

func TestEntriesCopies(t *testing.T) {
	match := []string{"*://a.example/*"}
	fetch := []string{"f.example"}
	r, err := NewRegistry(Entry{Name: "a", Match: match, FetchHosts: fetch, Strategy: Redirect()})
	if err != nil {
		t.Fatal(err)
	}
	match[0], fetch[0] = "*://evil.example/*", "evil.example"
	es := r.Entries()
	if es[0].Match[0] != "*://a.example/*" || es[0].FetchHosts[0] != "f.example" {
		t.Errorf("registry aliases the caller's slices: %+v", es[0])
	}
	es[0].Match[0], es[0].FetchHosts[0] = "*://evil.example/*", "evil.example"
	es2 := r.Entries()
	if es2[0].Match[0] != "*://a.example/*" || es2[0].FetchHosts[0] != "f.example" {
		t.Errorf("Entries aliases the registry: %+v", es2[0])
	}
	if !slices.Equal(r.entry("a").hosts, []string{"a.example", "f.example"}) {
		t.Errorf("hosts %q", r.entry("a").hosts)
	}
}

func names(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestEligible(t *testing.T) {
	p, f := namedStrategy("P"), namedStrategy("F")
	tests := []struct {
		kind           Kind
		fallback       bool
		policy         GenericPolicy
		primary, fback string
	}{
		{FirstParty, true, PreviewOnly, "P", "F"},
		{FirstParty, true, Follow, "P", "F"},
		{FirstParty, true, Skip, "P", "F"},
		{FirstParty, false, Skip, "P", ""},
		{Marketing, true, PreviewOnly, "P", ""},
		{Marketing, true, Follow, "P", "F"},
		{Marketing, false, Follow, "P", ""},
		{Marketing, true, Skip, "", ""},
		{Marketing, true, Skip + 1, "", ""},
	}
	for _, tt := range tests {
		e := &compiledEntry{Entry: Entry{Kind: tt.kind, Strategy: p}}
		if tt.fallback {
			e.Fallback = f
		}
		gp, gf := e.eligible(tt.policy)
		if name(gp) != tt.primary || name(gf) != tt.fback {
			t.Errorf("kind %d fallback %v policy %d: %q, %q; want %q, %q", tt.kind, tt.fallback, tt.policy, name(gp), name(gf), tt.primary, tt.fback)
		}
	}
}

func name(s Strategy) string {
	if s == nil {
		return ""
	}
	return s.Name()
}

func TestLookup(t *testing.T) {
	r, err := NewRegistry(
		Entry{Name: "b-first", Match: []string{"*://s.example/x/*"}, Strategy: namedStrategy("b")},
		Entry{Name: "a-first", Match: []string{"*://s.example/*"}, Strategy: namedStrategy("a")},
		Entry{Name: "mkt", Match: []string{"*://m.example/*"}, Strategy: namedStrategy("m"), Kind: Marketing},
		Entry{Name: "hop", Match: []string{"*://h.example/share*"}, Strategy: namedStrategy("h"), Intermediate: true},
		Entry{Name: "reddit", Match: []string{"*://www.reddit.com/r/*/s/*"}, Strategy: namedStrategy("r")},
		Entry{Name: "plain", Match: []string{"*://p.example/*"}, Strategy: namedStrategy("p"), AllowHTTP: true,
			FetchHosts: []string{"f.example"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		in           string
		intermediate bool
		policy       GenericPolicy
		want         string // "" for no match
	}{
		{"https://s.example/x/1", false, PreviewOnly, "a-first"}, // Name order: a-first before b-first
		{"https://s.example/y", false, PreviewOnly, "a-first"},
		{"http://s.example/y", false, PreviewOnly, "a-first"}, // scheme-agnostic
		{"HTTPS://S.EXAMPLE./y", false, PreviewOnly, "a-first"},
		{"https://s.example", false, PreviewOnly, "a-first"},
		{"https://s.example:443/y", false, PreviewOnly, "a-first"},
		{"http://s.example:80/y", false, PreviewOnly, "a-first"},
		{"https://s.example:/y", false, PreviewOnly, "a-first"},
		{"https://s.example:80/y", false, PreviewOnly, ""},
		{"http://s.example:443/y", false, PreviewOnly, ""},
		{"https://s.example:8443/y", false, PreviewOnly, ""},
		{"https://s.example:0443/y", false, PreviewOnly, ""},
		{"https://u@s.example/y", false, PreviewOnly, ""},
		{"https://u:p@s.example/y", false, PreviewOnly, ""},
		{"https://@s.example/y", false, PreviewOnly, ""},
		{"https://sub.s.example/y", false, PreviewOnly, ""},
		{"https://f.example/y", false, PreviewOnly, ""}, // FetchHosts never match
		{"https://other.example/", false, PreviewOnly, ""},

		{"https://m.example/x", false, PreviewOnly, "mkt"},
		{"https://m.example/x", false, Follow, "mkt"},
		{"https://m.example/x", false, Skip, ""},
		{"https://m.example/x", true, Skip, ""},

		{"https://h.example/share.x", false, PreviewOnly, ""},
		{"https://h.example/share.x", true, PreviewOnly, "hop"},
		{"https://h.example/other", true, PreviewOnly, ""},
		{"https://s.example/y", true, PreviewOnly, "a-first"}, // a hop lookup also finds non-intermediate entries

		{"https://www.reddit.com/r/golang/s/TOKEN1", false, PreviewOnly, "reddit"},
		{"https://www.reddit.com/r/golang/comments/x", false, PreviewOnly, ""},
		{"https://www.reddit.com/r/golang/", false, PreviewOnly, ""},
	}
	for _, tt := range tests {
		u, skip := rawurl.Parse(tt.in)
		if skip != rawurl.OK {
			t.Fatalf("%s: skip %d", tt.in, skip)
		}
		e, ok := r.lookup(&u, tt.intermediate, tt.policy)
		got := ""
		if ok {
			got = e.Name
		}
		if got != tt.want || ok != (e != nil) {
			t.Errorf("lookup(%s, %v, %d) = %q, want %q", tt.in, tt.intermediate, tt.policy, got, tt.want)
		}
	}
	// An IP host never matches, even when a registry could hold it.
	for _, in := range []string{"https://127.0.0.1/x", "https://[::1]/x", "https://2130706433/x"} {
		u, _ := rawurl.Parse(in)
		if _, ok := r.lookup(&u, true, Follow); ok {
			t.Errorf("lookup(%s) matched", in)
		}
	}
	var nilReg *Registry
	u, _ := rawurl.Parse("https://s.example/y")
	if _, ok := nilReg.lookup(&u, true, Follow); ok || nilReg.Entries() != nil {
		t.Error("nil Registry matched or has entries")
	}
}
