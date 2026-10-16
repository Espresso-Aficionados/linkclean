package hostindex

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/matchpattern"
)

func mustParse(tb testing.TB, p string) *matchpattern.Pattern {
	tb.Helper()
	m, err := matchpattern.Parse(p, true)
	if err != nil {
		tb.Fatal(err)
	}
	return m
}

func build(tb testing.TB, rules [][]string) *Index {
	tb.Helper()
	var b Builder
	for ord, incs := range rules {
		for _, p := range incs {
			b.Add(int32(ord), mustParse(tb, p))
		}
	}
	return b.Build()
}

func TestCandidates(t *testing.T) {
	ix := build(t, [][]string{
		0: {"*://*/*"},
		1: {"*://www.example.com/*"},
		2: {"*://*.example.com/*", "*://*.example.com/a*"}, // two includes, one key
		3: {"*://*.amazon.*/*", "*://*.amazon.com.be/*"},
		4: {"*://*.com/*"},
		5: {"*://10.0.0.1/*", "*://*.0.0.1/*"},
		6: {"*://*.10.0.0.1/*"},
		7: {"https://*/*", "*://x.example/*"},
		8: {"*://amazon.*/*"},
	})
	for _, c := range []struct {
		host     string
		isIP     bool
		want     []int32
		needSite bool
	}{
		{"www.example.com", false, []int32{0, 1, 2, 4, 7}, false},
		{"example.com", false, []int32{0, 2, 4, 7}, false},
		{"badexample.com", false, []int32{0, 4, 7}, false},
		{"www.amazon.co.uk", false, []int32{0, 3, 7, 8}, true},
		{"www.amazon.com.be", false, []int32{0, 3, 7, 8}, true},
		{"amazon.com", false, []int32{0, 3, 4, 7, 8}, true},
		{"10.0.0.1", true, []int32{0, 5, 6, 7}, false},
		{"0.0.1", false, []int32{0, 5, 7}, false},
		{"[::1]", true, []int32{0, 7}, false},
		{"x.example", false, []int32{0, 7}, false},
		{"", false, []int32{0, 7}, false},
	} {
		got, needSite := ix.Candidates(nil, c.host, c.isIP)
		if !slices.Equal(got, c.want) || needSite != c.needSite {
			t.Errorf("Candidates(%q, %v) = %v %v, want %v %v", c.host, c.isIP, got, needSite, c.want, c.needSite)
		}
	}
	if ix.Len() != 9 {
		t.Errorf("Len() = %d, want 9", ix.Len())
	}

	// The prefix of dst is preserved and not merged into the output order.
	got, _ := ix.Candidates([]int32{99, 3}, "www.example.com", false)
	if want := []int32{99, 3, 0, 1, 2, 4, 7}; !slices.Equal(got, want) {
		t.Errorf("with prefix: %v, want %v", got, want)
	}
}

func TestEmptyIndex(t *testing.T) {
	var b Builder
	ix := b.Build()
	if got, needSite := ix.Candidates(nil, "example.com", false); len(got) != 0 || needSite || ix.Len() != 0 {
		t.Errorf("empty index: %v %v %d", got, needSite, ix.Len())
	}
}

func TestCandidatesAllocs(t *testing.T) {
	ix := build(t, [][]string{
		{"*://*/*"}, {"*://*.example.com/*"}, {"*://www.example.com/*"}, {"*://*.example.*/*"}, {"*://*.com/*"},
	})
	dst := make([]int32, 0, 64)
	if n := testing.AllocsPerRun(100, func() {
		dst, _ = ix.Candidates(dst[:0], "a.b.www.example.com", false)
	}); n != 0 {
		t.Errorf("Candidates: %v allocs, want 0", n)
	}
}

// fakeSite stands in for the ICANN site: the last two labels. Candidates
// looks up every label, so the superset property must hold for any site whose
// first label is one of the host's labels.
func fakeSite(host string) string {
	i := strings.LastIndexByte(host, '.')
	if i <= 0 {
		return ""
	}
	if j := strings.LastIndexByte(host[:i], '.'); j >= 0 {
		return host[j+1:]
	}
	return host
}

// concretize turns a path glob into a path it matches.
func concretize(glob string) string { return strings.ReplaceAll(glob, "*", "x") }

// Candidates ⊇ every rule with a matching Include, over the vendored Brave
// patterns, the overlay (when present) and hosts derived from them.
func TestCandidatesSuperset(t *testing.T) {
	var rules [][]string
	for _, f := range []string{"clean-urls.json", "query-filter.json", "debounce.json"} {
		var rs []struct{ Include []string }
		readJSON(t, "../../rules/brave/"+f, &rs)
		for _, r := range rs {
			rules = append(rules, r.Include)
		}
	}
	if b, err := os.ReadFile("../../rules/overlay/overlay.json"); err == nil {
		var rs []struct{ Include []string }
		if err := json.Unmarshal(b, &rs); err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			rules = append(rules, r.Include)
		}
	}
	rules = append(rules, []string{"*://*.amazon.*/*"}, []string{"*://10.0.0.1/*"}, []string{"*://*.10.0.0.1/*"})

	pats := make([][]*matchpattern.Pattern, len(rules))
	var b Builder
	var hosts []string
	for ord, incs := range rules {
		for _, p := range incs {
			m := mustParse(t, p)
			pats[ord] = append(pats[ord], m)
			b.Add(int32(ord), m)
			kind, h := m.Host()
			switch kind {
			case matchpattern.HostExact, matchpattern.HostSuffix:
				hosts = append(hosts, h, "www."+h, "a.b."+h, "x"+h, h+".evil.example")
			case matchpattern.HostLabel:
				hosts = append(hosts, h+".com", "www."+h+".co.uk", h+".evil.example", "evil."+h)
			}
		}
	}
	ix := b.Build()
	rng := rand.New(rand.NewPCG(1, 2))
	for range 2000 {
		h := hosts[rng.IntN(len(hosts))]
		hosts = append(hosts, h[rng.IntN(min(3, len(h)+1)):])
	}
	hosts = append(hosts, "10.0.0.1", "127.0.0.1", "[::1]", "localhost", "")

	var buf []int32
	matched := 0
	for _, host := range hosts {
		isIP := host == "10.0.0.1" || host == "127.0.0.1" || host == "[::1]"
		cands, needSite := ix.Candidates(buf[:0], host, isIP)
		buf = cands
		if !slices.IsSorted(cands) || len(slices.Compact(slices.Clone(cands))) != len(cands) {
			t.Fatalf("%q: candidates not ascending and unique: %v", host, cands)
		}
		for ord, ps := range pats {
			for _, p := range ps {
				tg := &matchpattern.Target{Scheme: "https", Host: host, IsIP: isIP, Port: "443"}
				if kind, _ := p.Host(); kind == matchpattern.HostLabel && !isIP {
					tg.Site = fakeSite(host)
					if _, found := slices.BinarySearch(cands, int32(ord)); found && !needSite {
						t.Errorf("%q: label rule %d is a candidate but needSite is false", host, ord)
					}
				}
				scheme, rest, _ := strings.Cut(p.String(), "://")
				if scheme == "http" {
					tg.Scheme = "http"
				}
				_, path, _ := strings.Cut(rest, "/")
				tg.SetPath("/" + concretize(path))
				if p.Match(tg) {
					if kind, _ := p.Host(); kind != matchpattern.HostAny {
						matched++
					}
					if _, found := slices.BinarySearch(cands, int32(ord)); !found {
						t.Errorf("%q: rule %d (%s) matches but is not a candidate", host, ord, p)
					}
				}
			}
		}
	}
	if matched < len(hosts)/2 {
		t.Errorf("only %d host-scoped matches over %d hosts: the property is nearly vacuous", matched, len(hosts))
	}
	t.Logf("%d rules, %d hosts, %d host-scoped (host, include) matches", len(rules), len(hosts), matched)
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// Candidates cost is flat in the number of host-scoped rules.
func BenchmarkCandidates(b *testing.B) {
	for _, n := range []int{0, 1000, 10000} {
		var bl Builder
		bl.Add(0, mustParse(b, "*://*/*"))
		for i := 1; i <= n; i++ {
			pat := fmt.Sprintf("*://*.h%d.example/*", i)
			if i%2 == 0 {
				pat = fmt.Sprintf("*://www.h%d.example/*", i)
			}
			bl.Add(int32(i), mustParse(b, pat))
		}
		ix := bl.Build()
		dst := make([]int32, 0, 64)
		for _, host := range []string{"www.unrelated.example.com", "a.b.h1.example"} {
			b.Run(fmt.Sprintf("rules=%d/%s", n, host), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					dst, _ = ix.Candidates(dst[:0], host, false)
				}
			})
		}
	}
}
