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

// build indexes rules the way the engine does: global ("*") Includes are not
// indexed. Rules in special are also added as special.
func build(tb testing.TB, rules [][]string, special ...int32) *Index {
	tb.Helper()
	var b Builder
	for ord, incs := range rules {
		for _, p := range incs {
			if m := mustParse(tb, p); !isGlobal(m) {
				b.Add(int32(ord), m, slices.Contains(special, int32(ord)))
			}
		}
	}
	return b.Build()
}

func isGlobal(m *matchpattern.Pattern) bool {
	k, _ := m.Host()
	return k == matchpattern.HostAny
}

func candidates(ix *Index, host string, isIP, special bool) []int32 {
	return Merge(nil, ix.Lookup(nil, host, isIP), special)
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
	}, 2, 3, 6)
	for _, c := range []struct {
		host          string
		isIP          bool
		want, special []int32
	}{
		{"www.example.com", false, []int32{1, 2, 4}, []int32{2}},
		{"example.com", false, []int32{2, 4}, []int32{2}},
		{"badexample.com", false, []int32{4}, nil},
		{"www.amazon.co.uk", false, []int32{3, 8}, []int32{3}},
		{"www.amazon.com.be", false, []int32{3, 8}, []int32{3}},
		{"amazon.com", false, []int32{3, 4, 8}, []int32{3}},
		{"10.0.0.1", true, []int32{5, 6}, []int32{6}},
		{"0.0.1", false, []int32{5}, nil},
		{"[::1]", true, nil, nil},
		{"x.example", false, []int32{7}, nil},
		{"", false, nil, nil},
	} {
		if got := candidates(ix, c.host, c.isIP, false); !slices.Equal(got, c.want) {
			t.Errorf("candidates(%q, %v) = %v, want %v", c.host, c.isIP, got, c.want)
		}
		if got := candidates(ix, c.host, c.isIP, true); !slices.Equal(got, c.special) {
			t.Errorf("special candidates(%q, %v) = %v, want %v", c.host, c.isIP, got, c.special)
		}
		bs := ix.Lookup(nil, c.host, c.isIP)
		for ord := range int32(9) {
			if got, want := Contains(bs, ord), slices.Contains(c.want, ord); got != want {
				t.Errorf("Contains(%q, %d) = %v, want %v", c.host, ord, got, want)
			}
		}
	}

	// The prefix of dst is preserved and not merged into the output order.
	got := Merge([]int32{99, 3}, ix.Lookup(nil, "www.example.com", false), false)
	if want := []int32{99, 3, 1, 2, 4}; !slices.Equal(got, want) {
		t.Errorf("with prefix: %v, want %v", got, want)
	}
}

func TestAddGlobalPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Add of a global pattern did not panic")
		}
	}()
	var b Builder
	b.Add(0, mustParse(t, "*://*/x*"), false)
}

func TestEmptyIndex(t *testing.T) {
	var b Builder
	ix := b.Build()
	if got := candidates(ix, "example.com", false, false); len(got) != 0 {
		t.Errorf("empty index: %v", got)
	}
}

func TestLookupAllocs(t *testing.T) {
	ix := build(t, [][]string{
		{"*://*.example.com/*"}, {"*://www.example.com/*"}, {"*://*.example.*/*"}, {"*://*.com/*"},
	})
	bs := make([]*Bucket, 0, 16)
	dst := make([]int32, 0, 64)
	if n := testing.AllocsPerRun(100, func() {
		bs = ix.Lookup(bs[:0], "a.b.www.example.com", false)
		dst = Merge(dst[:0], bs, false)
	}); n != 0 {
		t.Errorf("Lookup+Merge: %v allocs, want 0", n)
	}
}

// naiveLookup is Lookup without the suffixLabels bound: every parent domain.
func naiveLookup(ix *Index, host string, isIP bool) []int32 {
	var bs []*Bucket
	add := func(b *Bucket) {
		if b != nil {
			bs = append(bs, b)
		}
	}
	add(ix.exact[host])
	add(ix.suffix[host])
	if !isIP {
		for s := host; strings.Contains(s, "."); {
			_, s, _ = strings.Cut(s, ".")
			add(ix.suffix[s])
		}
		for _, l := range strings.Split(host, ".") {
			add(ix.label[l])
		}
	}
	return Merge(nil, bs, false)
}

// The bounded parent walk finds what walking every parent finds, including
// for hosts far deeper than any suffix key (PERF2-1).
func TestLookupBoundedWalk(t *testing.T) {
	ix := build(t, [][]string{
		{"*://*.com/*"}, {"*://*.co.uk/*"}, {"*://*.a.b.example.co.uk/*"}, {"*://www.example.com/*"}, {"*://*.amazon.*/*"},
	})
	if ix.suffixLabels != 5 {
		t.Fatalf("suffixLabels = %d, want 5", ix.suffixLabels)
	}
	for _, h := range []string{
		"com", "x.com", "www.example.com", "a.b.example.co.uk", "x.a.b.example.co.uk", "y.x.a.b.example.co.uk",
		"amazon.co.uk", "..com", "a..co.uk", ".", "",
		strings.Repeat("a.", 4000) + "example.com", strings.Repeat("amazon.", 500) + "b.example.co.uk",
	} {
		if got, want := candidates(ix, h, false, false), naiveLookup(ix, h, false); !slices.Equal(got, want) {
			t.Errorf("%.40q: Lookup %v, every parent %v", h, got, want)
		}
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

// The candidates ⊇ every rule with a matching host-scoped Include, over the
// vendored Brave patterns, the overlay and hosts derived from them.
func TestCandidatesSuperset(t *testing.T) {
	var rules [][]string
	for _, f := range []string{"clean-urls.json", "query-filter.json", "debounce.json"} {
		var rs []struct{ Include []string }
		readJSON(t, "../../rules/brave/"+f, &rs)
		for _, r := range rs {
			rules = append(rules, r.Include)
		}
	}
	var ov []struct{ Include []string }
	readJSON(t, "../../rules/overlay/overlay.json", &ov)
	for _, r := range ov {
		rules = append(rules, r.Include)
	}
	rules = append(rules, []string{"*://*.amazon.*/*"}, []string{"*://10.0.0.1/*"}, []string{"*://*.10.0.0.1/*"})

	pats := make([][]*matchpattern.Pattern, len(rules))
	var b Builder
	var hosts []string
	for ord, incs := range rules {
		for _, p := range incs {
			m := mustParse(t, p)
			pats[ord] = append(pats[ord], m)
			if !isGlobal(m) {
				b.Add(int32(ord), m, false)
			}
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

	matched := 0
	for _, host := range hosts {
		isIP := host == "10.0.0.1" || host == "127.0.0.1" || host == "[::1]"
		cands := candidates(ix, host, isIP, false)
		if !slices.IsSorted(cands) || len(slices.Compact(slices.Clone(cands))) != len(cands) {
			t.Fatalf("%q: candidates not ascending and unique: %v", host, cands)
		}
		for ord, ps := range pats {
			for _, p := range ps {
				tg := &matchpattern.Target{Scheme: "https", Host: host, IsIP: isIP, Port: "443"}
				if kind, _ := p.Host(); kind == matchpattern.HostLabel && !isIP {
					tg.Site = fakeSite(host)
				}
				scheme, rest, _ := strings.Cut(p.String(), "://")
				if scheme == "http" {
					tg.Scheme = "http"
				}
				_, path, _ := strings.Cut(rest, "/")
				tg.SetPath("/" + concretize(path))
				if p.Match(tg) && !isGlobal(p) {
					matched++
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

// Lookup cost is flat in the number of host-scoped rules and linear in the
// host length (labels=N: a host of N labels).
func BenchmarkCandidates(b *testing.B) {
	for _, n := range []int{0, 1000, 10000} {
		var bl Builder
		for i := 1; i <= n; i++ {
			pat := fmt.Sprintf("*://*.h%d.example/*", i)
			if i%2 == 0 {
				pat = fmt.Sprintf("*://www.h%d.example/*", i)
			}
			bl.Add(int32(i), mustParse(b, pat), false)
		}
		bl.Add(int32(n+1), mustParse(b, "*://*.co.uk/*"), false)
		ix := bl.Build()
		hosts := []string{"www.unrelated.example.com", "a.b.h1.example"}
		if n == 1000 {
			for _, labels := range []int{500, 1000, 2000, 4000} {
				hosts = append(hosts, strings.Repeat("a.", labels-2)+"example.com")
			}
		}
		bs := make([]*Bucket, 0, 16)
		dst := make([]int32, 0, 64)
		for _, host := range hosts {
			name := host
			if len(host) > 64 {
				name = fmt.Sprint("labels=", strings.Count(host, ".")+1)
			}
			b.Run(fmt.Sprintf("rules=%d/%s", n, name), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					bs = ix.Lookup(bs[:0], host, false)
					dst = Merge(dst[:0], bs, false)
				}
			})
		}
	}
}
