package linkclean

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/query"
)

// Benchmarks report against the §8 targets (not CI-gating): unchanged URL
// <= 2 µs and 0 allocs, changed URL <= 10 µs, New with defaults <= 10 ms,
// crafted 8 KiB URL <= 1 ms.

var benchSink string

const (
	benchUnchanged = "https://www.example.com/blog/2026/10/espresso-grinder-review?page=2&sort=new#comments"
	benchChanged   = "https://www.amazon.com/Some-Title/dp/B0ABC/ref=sr_1_3?crid=X&keywords=k&qid=1&sr=8-3&linkCode=ll1&tag=aff-20"
)

func BenchmarkCleanUnchanged(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean(benchUnchanged)
	}
}

func BenchmarkCleanUnchangedYouTube(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean("https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42")
	}
}

func BenchmarkCleanChanged(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean(benchChanged)
	}
}

func BenchmarkCleanChangedUTM(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean("https://example.com/article?utm_source=newsletter&utm_medium=email&id=9")
	}
}

func BenchmarkCleanUnwrap(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean("https://l.facebook.com/l.php?u=https%3A%2F%2Fexample.net%2Fa%3Ffbclid%3DIw%26id%3D1&h=AT0")
	}
}

func BenchmarkExplainChanged(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	for b.Loop() {
		_ = c.Explain(benchChanged)
	}
}

func BenchmarkCorpus(b *testing.B) {
	c := Default()
	items := readCorpus(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		benchSink = c.Clean(items[i%len(items)].Input)
	}
}

func BenchmarkNewDefaults(b *testing.B) {
	srcs := DefaultSources()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(Config{Sources: srcs}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewSharedBase(b *testing.B) {
	Default()
	extra := []Rule{{ID: "x", Action: Strip, Include: []string{"*://*.example.com/*"}, Params: []string{"foo"}}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := New(Config{Extra: extra, Affiliate: AffiliateStrip, DisableTags: []string{"lowconf"}}); err != nil {
			b.Fatal(err)
		}
	}
}

// crafted8K is a worst case of §8: 8 KiB of URL-valued query values, each a
// full inner clean, plus regex-trigger names.
func crafted8K() string {
	var b strings.Builder
	b.WriteString("https://www.example.com/unsubscribe/x?")
	inner := query.EncodeComponent("https://www.example.net/p?utm_source=x&mkt_tok=1&fbclid=2&id=3")
	for i := 0; b.Len() < 8000; i++ {
		if i > 0 {
			b.WriteByte('&')
		}
		if i%2 == 0 {
			b.WriteString("u=" + inner)
		} else {
			b.WriteString("utm_x" + strings.Repeat("a", i%7) + "=1&mkt_tok=1")
		}
	}
	return b.String()[:8000]
}

func BenchmarkCrafted8K(b *testing.B) {
	c := Default()
	in := crafted8K()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean(in)
	}
}

func BenchmarkNestedMany(b *testing.B) {
	c := Default()
	inner := query.EncodeComponent("https://www.example.net/p?id=3")
	var sb strings.Builder
	sb.WriteString("https://www.example.com/x?")
	for i := 0; i < 80; i++ {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString("u=" + inner)
	}
	in := sb.String()
	b.ReportAllocs()
	for b.Loop() {
		benchSink = c.Clean(in)
	}
}

func BenchmarkParallelUnchanged(b *testing.B) {
	c := Default()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var s string
		for pb.Next() {
			s = c.Clean(benchUnchanged)
		}
		_ = s
	})
}

// TestCleanUnchangedAllocs pins §8's "unchanged URL: 0 allocs" on common
// shapes. A URL whose kept params are protected by a label.* Keep (Amazon
// tag) needs its site, and x/net/publicsuffix.PublicSuffix allocates once
// for it: it first tries netip.ParseAddr, whose error for a non-IP host is
// heap-allocated.
func TestCleanUnchangedAllocs(t *testing.T) {
	c := Default()
	for _, in := range []string{
		benchUnchanged,
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42",
		"https://github.com/o/r/blob/main/x.go?ref=feature",
		"https://en.wikipedia.org/wiki/Espresso",
		"https://example.com?id=1", // empty path: no "/?" + query per round (PERF2-6)
	} {
		if c.Clean(in) != in {
			t.Fatalf("%s changed", in)
		}
		if n := testing.AllocsPerRun(100, func() { benchSink = c.Clean(in) }); n != 0 {
			t.Errorf("Clean(%s): %v allocs, want 0", in, n)
		}
	}
}

// Long unchanged inputs cost no allocation per segment: a parameter-shaped
// fragment of thousands of segments allocates nothing (PERF3-3), and names
// longer than the stack buffers share one buffer per call (PERF3-4).
func TestCleanUnchangedAllocsLong(t *testing.T) {
	c := Default()
	long := strings.Repeat("n", 300)
	for _, tc := range []struct {
		in  string
		max float64
	}{
		{"https://www.example.com/x#" + strings.Repeat("a&", 3980) + "a=1", 0},
		{"https://www.example.com/x?" + strings.Repeat(long+"=1&", 25) + "id=1", 1},
		{"https://www.example.com/x?" + strings.Repeat(long[:100]+"=1&", 70) + "id=1", 0},
	} {
		if c.Clean(tc.in) != tc.in {
			t.Fatalf("%.40s… changed", tc.in)
		}
		if n := testing.AllocsPerRun(20, func() { benchSink = c.Clean(tc.in) }); n > tc.max {
			t.Errorf("Clean(%.40s…): %v allocs, want <= %v", tc.in, n, tc.max)
		}
	}
}

// manyExtra returns n Extra rules of one shape, each naming its own param:
//   - global: Strip, Include *://*/*, Params (trivial scope)
//   - offHostRx: Strip, a host that is never cleaned, ParamRegexps
//   - globalRx: Strip, Include *://*/*, ParamRegexps with a literal prefix
//   - globalExclude: Strip, Include *://*/* with an Exclude, Params
//   - offHostRxNoPrefix: offHostRx with a ParamRegexps entry without a
//     literal prefix (PERF3-2)
//   - sameHost: Strip, Include *://www.example.com/* (benchUnchanged's host), Params
func manyExtra(n int, shape string) []Rule {
	rs := make([]Rule, n)
	for i := range rs {
		r := Rule{ID: fmt.Sprint("r", i), Action: Strip, Include: []string{"*://*/*"}, Params: []string{fmt.Sprint("zz", i)}}
		rx := []string{fmt.Sprintf("zz%d_.*", i)}
		switch shape {
		case "offHostRx":
			r.Include, r.Params, r.ParamRegexps = []string{"*://*.other-host.example/*"}, nil, rx
		case "offHostRxNoPrefix":
			r.Include, r.Params, r.ParamRegexps = []string{"*://*.other-host.example/*"}, nil, []string{fmt.Sprintf("(?i)zz%d_.*", i)}
		case "globalRx":
			r.Params, r.ParamRegexps = nil, rx
		case "globalExclude":
			r.Exclude = []string{"*://*.other-host.example/*"}
		case "sameHost":
			r.Include = []string{"*://www.example.com/*"}
		}
		rs[i] = r
	}
	return rs
}

var manyExtraShapes = []string{"global", "offHostRx", "offHostRxNoPrefix", "globalRx", "globalExclude", "sameHost"}

// No Extra rule shape costs an allocation per unchanged URL, at any n (§8,
// PERF_REVIEW #8, PERF2-2).
func TestCleanUnchangedAllocsManyExtra(t *testing.T) {
	for _, shape := range manyExtraShapes {
		for _, n := range []int{100, 1000, 10000} {
			c := MustNew(Config{Extra: manyExtra(n, shape)})
			for _, in := range []string{benchUnchanged, "https://github.com/o/r/blob/main/x.go?ref=feature"} {
				if c.Clean(in) != in {
					t.Fatalf("%s changed", in)
				}
				if a := testing.AllocsPerRun(20, func() { benchSink = c.Clean(in) }); a != 0 {
					t.Errorf("%s/n=%d Clean(%s): %v allocs, want 0", shape, n, in, a)
				}
			}
			// The rules work: the last one strips its param.
			in := "https://www.example.com/x?zz" + fmt.Sprint(n-1) + "_=1&id=1"
			if shape == "global" || shape == "globalExclude" || shape == "sameHost" {
				in = "https://www.example.com/x?zz" + fmt.Sprint(n-1) + "=1&id=1"
			}
			want := "https://www.example.com/x?id=1"
			if shape == "offHostRx" || shape == "offHostRxNoPrefix" {
				want = in
			}
			if got := c.Clean(in); got != want {
				t.Errorf("%s/n=%d Clean(%s) = %s, want %s", shape, n, in, got, want)
			}
		}
	}
}

// BenchmarkManyExtra should be flat in n for every rule shape.
func BenchmarkManyExtra(b *testing.B) {
	for _, shape := range manyExtraShapes {
		for _, n := range []int{0, 100, 1000, 10000} {
			c := MustNew(Config{Extra: manyExtra(n, shape)})
			b.Run(fmt.Sprintf("%s/n=%d", shape, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink = c.Clean(benchUnchanged)
					benchSink = c.Clean(benchChanged)
				}
			})
		}
	}
}

// BenchmarkScopeMemo: two global rules with URLRegexps, named alternately by
// a 7.9 KiB query, with ordinals gap apart. The cost must not depend on gap
// (PERF3-1); gap 4096 puts the memo in the pool.
func BenchmarkScopeMemo(b *testing.B) {
	in := "https://www.example.com/x?" + strings.Repeat("AA=1&BB=1&", 790) + "aa=1&bb=1"
	for _, gap := range []int{63, 64, 4096} {
		rs := []Rule{{ID: "A", Action: Strip, Include: []string{"*://*/*"}, URLRegexps: []string{`[?&]aa=`}, Params: []string{"aa"}}}
		for i := 1; i < gap; i++ {
			rs = append(rs, Rule{ID: fmt.Sprint("f", i), Action: Strip, Include: []string{"*://*.filler.example/*"}, Params: []string{fmt.Sprint("f", i)}})
		}
		rs = append(rs, Rule{ID: "B", Action: Strip, Include: []string{"*://*/*"}, URLRegexps: []string{`[?&]bb=`}, Params: []string{"bb"}})
		c := MustNew(Config{Extra: rs, Sources: []RuleSet{}})
		b.Run(fmt.Sprintf("gap=%d", gap), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = c.Clean(in)
			}
		})
	}
}

// BenchmarkLongQuery covers the long-query shapes of R1-PERF-1 and PERF-3:
// only separators, many tiny params, a signed URL whose utm_* names reach the
// ParamRegexps Keep scan (PERF-4), and two 7.6 KiB URLs that put a global
// rule with URLRegexps in scope (PERF2-3, PERF2-4).
func BenchmarkLongQuery(b *testing.B) {
	c := Default()
	for name, in := range map[string]string{
		"separators": "https://example.com/?" + strings.Repeat("&", 8100),
		"tiny":       "https://www.example.com/x?" + strings.Repeat("a=1&", 2025),
		"signed":     "https://example.com/f?X-Amz-Signature=1&" + strings.Repeat("utm_source=1&a=1&", 400),
		"mkt_tok":    "https://www.example.com/x?mkt_tok=1" + strings.Repeat("&p=v", 1900),
		"_pos":       "https://www.example.com/products/x?a=1" + strings.Repeat("&p=v", 1900) + "&_pos=1",
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = c.Clean(in)
			}
		})
	}
}
