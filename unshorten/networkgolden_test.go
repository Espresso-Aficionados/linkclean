package unshorten

// TestNetworkGolden replays the root's testdata/golden/network.txtar (rows
// N1-N5; its header documents the grammar) over the test network. A change
// to an expectation there is a minor release, like any golden row's.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Espresso-Aficionados/linkclean"
)

const networkGolden = "../testdata/golden/network.txtar"

// netServerLine is one server: line and its body: line.
type netServerLine struct {
	method, url string
	status      int
	location    string
	body        string
}

// netCase is one section of network.txtar.
type netCase struct {
	ID, In, Want, WantStrip string
	Line                    int
	Registry                []string
	Generic                 GenericPolicy
	Servers                 []*netServerLine
	Listen                  []string
	Requests                map[string]int
	Err                     error
	Hops, Removed           map[string][]map[string]any // by policy "keep"/"strip"; nil: not compared
}

var netSentinels = map[string]error{
	"ErrHopLimit": ErrHopLimit, "ErrLoop": ErrLoop, "ErrFallback": ErrFallback, "ErrNoRedirect": ErrNoRedirect,
	"ErrNoMatch": ErrNoMatch, "ErrBadTarget": ErrBadTarget, "ErrFetchDenied": ErrFetchDenied, "ErrBudget": ErrBudget,
	"ErrNotShortLink": ErrNotShortLink, "ErrTransient": ErrTransient,
}

func parseNetCases(data []byte) ([]netCase, error) {
	_, files := parseTxtar(data)
	var out []netCase
	for _, f := range files {
		c := netCase{ID: f.Name, Line: f.Line, Requests: map[string]int{},
			Hops: map[string][]map[string]any{}, Removed: map[string][]map[string]any{}}
		hopsSet, removedSet := map[string]bool{}, map[string]bool{}
		var hasIn, hasWant bool
		sc := bufio.NewScanner(bytes.NewReader(f.Data))
		sc.Buffer(nil, 1<<20)
		for n := f.Line + 1; sc.Scan(); n++ {
			line := sc.Text()
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, val, ok := strings.Cut(line, ": ")
			if !ok {
				return nil, fmt.Errorf("%s:%d: %q is not \"key: value\"", networkGolden, n, line)
			}
			fail := func(format string, args ...any) error {
				return fmt.Errorf("%s:%d: %s: %s", networkGolden, n, key, fmt.Sprintf(format, args...))
			}
			obj := func() (map[string]any, error) {
				var m map[string]any
				dec := json.NewDecoder(strings.NewReader(val))
				dec.UseNumber()
				if err := dec.Decode(&m); err != nil {
					return nil, fail("%v", err)
				}
				return m, nil
			}
			// policies a hop/removed key applies to
			pols := func(base string) []string {
				switch strings.TrimPrefix(key, base) {
				case "-keep":
					return []string{"keep"}
				case "-strip":
					return []string{"strip"}
				}
				return []string{"keep", "strip"}
			}
			switch key {
			case "in":
				if hasIn {
					return nil, fail("second in:")
				}
				c.In, hasIn = val, true
			case "want":
				c.Want, hasWant = val, true
			case "want-strip":
				c.WantStrip = val
			case "registry":
				c.Registry = strings.Fields(val)
			case "generic":
				p, ok := map[string]GenericPolicy{"PreviewOnly": PreviewOnly, "Follow": Follow, "Skip": Skip}[val]
				if !ok {
					return nil, fail("unknown policy %q", val)
				}
				c.Generic = p
			case "server":
				req, resp, ok := strings.Cut(val, " \u2192 ")
				method, u, ok2 := strings.Cut(req, " ")
				if !ok || !ok2 {
					return nil, fail("%q is not METHOD URL \u2192 STATUS [LOCATION]", val)
				}
				status, loc, _ := strings.Cut(resp, " ")
				code, err := strconv.Atoi(status)
				if err != nil {
					return nil, fail("status %q", status)
				}
				c.Servers = append(c.Servers, &netServerLine{method: method, url: u, status: code, location: loc})
			case "body":
				if len(c.Servers) == 0 {
					return nil, fail("body: before any server:")
				}
				c.Servers[len(c.Servers)-1].body = val
			case "listen":
				c.Listen = append(c.Listen, val)
			case "requests":
				u, cnt, ok := strings.Cut(val, " ")
				k, err := strconv.Atoi(cnt)
				if !ok || err != nil {
					return nil, fail("%q is not URL N", val)
				}
				c.Requests[u] = k
			case "err":
				if val != "none" {
					s, ok := netSentinels[val]
					if !ok {
						return nil, fail("unknown sentinel %q", val)
					}
					c.Err = s
				}
			case "hops", "hops-keep", "hops-strip", "hop", "hop-keep", "hop-strip":
				base := "hop"
				if strings.HasPrefix(key, "hops") {
					base = "hops"
				}
				for _, p := range pols(base) {
					hopsSet[p] = true
					if c.Hops[p] == nil {
						c.Hops[p] = []map[string]any{}
					}
					if base == "hops" {
						if val != "none" {
							return nil, fail("%q (want none)", val)
						}
						continue
					}
					m, err := obj()
					if err != nil {
						return nil, err
					}
					c.Hops[p] = append(c.Hops[p], m)
				}
			case "removed", "removed-keep", "removed-strip":
				for _, p := range pols("removed") {
					removedSet[p] = true
					if c.Removed[p] == nil {
						c.Removed[p] = []map[string]any{}
					}
					if val == "none" {
						continue
					}
					m, err := obj()
					if err != nil {
						return nil, err
					}
					c.Removed[p] = append(c.Removed[p], m)
				}
			case "note":
			default:
				return nil, fail("unknown key")
			}
		}
		if !hasIn || !hasWant {
			return nil, fmt.Errorf("%s:%d: case %q needs in: and want:", networkGolden, f.Line, c.ID)
		}
		for _, p := range []string{"keep", "strip"} {
			if !hopsSet[p] {
				delete(c.Hops, p)
			}
			if !removedSet[p] {
				delete(c.Removed, p)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// netOrigin is one listener of a case: the requests it answers, and the
// requests it got that it does not.
type netOrigin struct {
	routes map[string]*netServerLine // by RequestURI
	mu     sync.Mutex
	stray  []string
}

func (o *netOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sl := o.routes[r.RequestURI]
	if sl == nil || sl.method != r.Method {
		o.mu.Lock()
		o.stray = append(o.stray, r.Method+" "+r.RequestURI)
		o.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if sl.location != "" {
		w.Header().Set("Location", sl.location)
	}
	if sl.body != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.WriteHeader(sl.status)
	_, _ = w.Write([]byte(sl.body))
}

func TestNetworkGolden(t *testing.T) {
	data, err := os.ReadFile(networkGolden)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := parseNetCases(data)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	if want := []string{"N1", "N2", "N3", "N4", "N5"}; !slices.Equal(ids, want) {
		t.Fatalf("network.txtar sections %q, want exactly %q", ids, want)
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) { runNetCase(t, c) })
	}
}

func runNetCase(t *testing.T, c netCase) {
	n := installNet(t)
	origins := map[string]*netOrigin{}
	origin := func(raw string) (*netOrigin, string) {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			t.Fatalf("line %d: URL %q", c.Line, raw)
		}
		key := u.Scheme + "://" + u.Host
		if origins[key] == nil {
			origins[key] = &netOrigin{routes: map[string]*netServerLine{}}
		}
		return origins[key], u.RequestURI()
	}
	for _, s := range c.Servers {
		o, reqURI := origin(s.url)
		o.routes[reqURI] = s
	}
	for _, l := range c.Listen {
		origin(l)
	}
	servers := map[string]*testServer{}
	for key, o := range origins {
		scheme, host, _ := strings.Cut(key, "://")
		if scheme == "http" {
			servers[key] = n.HandleHTTP(host, o)
		} else {
			servers[key] = n.HandleTLS(host, o)
		}
	}
	reg := DefaultRegistry()
	for _, name := range c.Registry {
		switch name {
		case "bit.ly":
			reg = rvDefaultWith(t, rvBitlyEntry())
		default:
			t.Fatalf("unknown registry entry %q", name)
		}
	}
	keep := rvResolver(t, n, Config{Registry: reg, Generic: c.Generic})
	views := []struct {
		name string
		r    *Resolver
		want string
	}{{"keep", keep, c.Want}, {"strip", keep.WithCleaner(rvStrip(t)), c.Want}}
	if c.WantStrip != "" {
		views[1].want = c.WantStrip
	}
	hits := func() int {
		total := 0
		for _, s := range servers {
			total += s.Hits()
		}
		return total
	}
	first := -1
	for vi, v := range views {
		res, err := v.r.Resolve(context.Background(), c.In)
		pr, perr := v.r.Process(context.Background(), c.In)
		if vi == 0 {
			first = hits()
		}
		where := c.ID + " [" + v.name + "]"
		if res.URL != v.want {
			t.Errorf("%s: Resolve URL %q, want %q", where, res.URL, v.want)
		}
		if pr.URL != res.URL {
			t.Errorf("%s: Process URL %q != Resolve URL %q", where, pr.URL, res.URL)
		}
		if got := v.r.c.Clean(res.URL); got != res.URL {
			t.Errorf("%s: Clean(URL) = %q, want URL %q", where, got, res.URL)
		}
		for _, e := range []error{err, perr} {
			switch {
			case c.Err == nil && e != nil:
				t.Errorf("%s: unexpected error %v", where, e)
			case c.Err != nil && (!errors.Is(e, c.Err) || !errors.Is(e, ErrUnresolved)):
				t.Errorf("%s: error %v, want %v and ErrUnresolved", where, e, c.Err)
			}
		}
		if c.Err != nil && res.URL != v.r.c.Clean(c.In) {
			t.Errorf("%s: failed URL %q, want Clean(in) %q", where, res.URL, v.r.c.Clean(c.In))
		}
		if vi > 0 && len(res.Hops) > 0 && !res.Cached {
			t.Errorf("%s: not served from the cache", where)
		}
		if h, ok := c.Hops[v.name]; ok {
			compareGolden(t, where+" hops", goldenHopMaps(pr.Hops), h)
		}
		if rm, ok := c.Removed[v.name]; ok {
			compareGolden(t, where+" removed", goldenRemovalMaps(pr.Removed), rm)
		}
	}
	if got := hits(); got != first {
		t.Errorf("%s: %d requests after the first run; the cache must serve every later run", c.ID, got-first)
	}
	for key, want := range c.Requests {
		s := servers[key]
		if s == nil {
			t.Errorf("%s: requests: %s has no server: or listen: line", c.ID, key)
			continue
		}
		if s.Hits() != want {
			t.Errorf("%s: %s got %d requests, want %d", c.ID, key, s.Hits(), want)
		}
	}
	for key, o := range origins {
		o.mu.Lock()
		if len(o.stray) > 0 {
			t.Errorf("%s: %s got requests the case does not list: %q", c.ID, key, o.stray)
		}
		o.mu.Unlock()
	}
}

// TestNetworkGoldenN5NoMatch is N5's other half: an info page without the
// destination is ErrNoMatch, and the input is returned (only GET /abc+ is
// made: Redirect runs only under Follow).
func TestNetworkGoldenN5NoMatch(t *testing.T) {
	n := installNet(t)
	bl := n.HandleTLS("bit.ly", rvRoutes{"/abc+": rvBody("<html><body>Sign in to see this link</body></html>")})
	r := rvResolver(t, n, Config{Registry: rvDefaultWith(t, rvBitlyEntry())})
	raw := "https://bit.ly/abc"
	res, err := r.Resolve(context.Background(), raw)
	rvFailOpen(t, r, raw, res, err, ErrNoMatch)
	if res.URL != raw {
		t.Errorf("URL %q, want %q", res.URL, raw)
	}
	reqs := bl.Requests()
	if len(reqs) != 1 || reqs[0].RequestURI != "/abc+" {
		t.Errorf("bit.ly requests %+v, want exactly GET /abc+", reqs)
	}
}

// goldenHopMaps and goldenRemovalMaps render Hops and Removals with
// positive.txtar's field names.
func goldenHopMaps(hs []linkclean.Hop) []map[string]any {
	var out []map[string]any
	for _, h := range hs {
		out = append(out, map[string]any{
			"from": h.From, "to": h.To, "rule": h.Rule, "category": goldenText(h.Category),
			"fromHost": h.FromHost, "network": h.Beneficiary.Network, "id": h.Beneficiary.ID,
		})
	}
	return out
}

func goldenRemovalMaps(rs []linkclean.Removal) []map[string]any {
	parts := [...]string{"", "path", "query", "fragment", "nested"}
	var out []map[string]any
	for _, r := range rs {
		part := ""
		if int(r.Part) < len(parts) {
			part = parts[r.Part]
		}
		out = append(out, map[string]any{
			"part": part, "category": goldenText(r.Category), "name": r.Name, "value": r.Value,
			"text": r.Text, "nestedIn": r.NestedIn, "rule": r.Rule,
			"network": r.Beneficiary.Network, "id": r.Beneficiary.ID,
		})
	}
	return out
}

func goldenText(c linkclean.Category) string {
	b, err := c.MarshalText()
	if err != nil {
		return fmt.Sprint(uint8(c))
	}
	return string(b)
}

// compareGolden compares got with want entry by entry, on the keys want
// lists; the counts must match.
func compareGolden(t *testing.T, what string, got, want []map[string]any) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d entries, want %d:\n  got  %v\n  want %v", what, len(got), len(want), got, want)
		return
	}
	for i := range want {
		for k, w := range want[i] {
			g, ok := got[i][k]
			if !ok {
				t.Errorf("%s[%d]: unknown key %q", what, i, k)
				continue
			}
			if fmt.Sprint(g) != fmt.Sprint(w) {
				t.Errorf("%s[%d].%s = %q, want %q", what, i, k, g, w)
			}
		}
	}
}

// TestParseNetCases checks the parser on a small archive.
func TestParseNetCases(t *testing.T) {
	cs, err := parseNetCases([]byte("hdr\n-- X --\nin: https://t.co/a\nserver: GET https://t.co/a \u2192 301 https://example.net/\nbody: <a>\nlisten: http://t.co\nrequests: https://t.co 1\nwant: https://example.net/\nerr: ErrFallback\nhops: none\nremoved-strip: {\"name\":\"x\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 {
		t.Fatalf("%d cases", len(cs))
	}
	c := cs[0]
	if c.In != "https://t.co/a" || len(c.Servers) != 1 || c.Servers[0].status != 301 || c.Servers[0].location != "https://example.net/" ||
		c.Servers[0].body != "<a>" || c.Requests["https://t.co"] != 1 || c.Err != ErrFallback || len(c.Listen) != 1 {
		t.Errorf("parsed %+v", c)
	}
	if h, ok := c.Hops["keep"]; !ok || len(h) != 0 {
		t.Errorf("hops: none parsed as %v", c.Hops)
	}
	if _, ok := c.Removed["keep"]; ok || len(c.Removed["strip"]) != 1 {
		t.Errorf("removed-strip parsed as %v", c.Removed)
	}
	for _, bad := range []string{"-- X --\nwant: a\n", "-- X --\nin: a\nwant: b\nerr: ErrNope\n", "-- X --\nin: a\nwant: b\nserver: GET x\n", "-- X --\nin: a\nwant: b\nbogus: 1\n"} {
		if _, err := parseNetCases([]byte(bad)); err == nil {
			t.Errorf("parseNetCases(%q) accepted", bad)
		}
	}
}

// TestNetworkGoldenParses checks network.txtar itself, without the
// network: it parses, and its sections are exactly N1-N5, each a delimited
// row ID the CHANGELOG can list.
func TestNetworkGoldenParses(t *testing.T) {
	data, err := os.ReadFile(networkGolden)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := parseNetCases(data)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cases {
		ids = append(ids, c.ID)
		if len(c.Servers) == 0 || len(c.Requests) == 0 {
			t.Errorf("%s: no server: or requests: line", c.ID)
		}
	}
	if want := []string{"N1", "N2", "N3", "N4", "N5"}; !slices.Equal(ids, want) {
		t.Errorf("sections %q, want exactly %q", ids, want)
	}
}
