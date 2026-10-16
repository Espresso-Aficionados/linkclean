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
// tag) pays the PSL lookup, one allocation inside x/net/publicsuffix.
func TestCleanUnchangedAllocs(t *testing.T) {
	c := Default()
	for _, in := range []string{
		benchUnchanged,
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&t=42",
		"https://github.com/o/r/blob/main/x.go?ref=feature",
		"https://en.wikipedia.org/wiki/Espresso",
	} {
		if c.Clean(in) != in {
			t.Fatalf("%s changed", in)
		}
		if n := testing.AllocsPerRun(100, func() { benchSink = c.Clean(in) }); n != 0 {
			t.Errorf("Clean(%s): %v allocs, want 0", in, n)
		}
	}
}

// manyExtra returns n Extra Strip rules: global rules naming one param each
// (trivial scope) or, with offHost, rules scoped to a host that is never
// cleaned, each with a ParamRegexps entry.
func manyExtra(n int, offHost bool) []Rule {
	rs := make([]Rule, n)
	for i := range rs {
		rs[i] = Rule{ID: fmt.Sprint("r", i), Action: Strip, Include: []string{"*://*/*"}, Params: []string{fmt.Sprint("zz", i)}}
		if offHost {
			rs[i].Include = []string{"*://*.other-host.example/*"}
			rs[i].Params, rs[i].ParamRegexps = nil, []string{fmt.Sprintf("zz%d_.*", i)}
		}
	}
	return rs
}

// Global Params-only rules and off-host ParamRegexps rules cost nothing per
// URL (§8, PERF_REVIEW #8): an unchanged URL stays at 0 allocs at any n.
func TestCleanUnchangedAllocsManyExtra(t *testing.T) {
	for _, offHost := range []bool{false, true} {
		c := MustNew(Config{Extra: manyExtra(2000, offHost)})
		for _, in := range []string{benchUnchanged, "https://github.com/o/r/blob/main/x.go?ref=feature"} {
			if c.Clean(in) != in {
				t.Fatalf("%s changed", in)
			}
			if n := testing.AllocsPerRun(20, func() { benchSink = c.Clean(in) }); n != 0 {
				t.Errorf("offHost=%v Clean(%s): %v allocs, want 0", offHost, in, n)
			}
		}
	}
}

// BenchmarkManyExtra should be flat in n for both rule shapes.
func BenchmarkManyExtra(b *testing.B) {
	for _, offHost := range []bool{false, true} {
		for _, n := range []int{0, 100, 10000} {
			c := MustNew(Config{Extra: manyExtra(n, offHost)})
			b.Run(fmt.Sprintf("offHost=%v/n=%d", offHost, n), func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					benchSink = c.Clean(benchUnchanged)
					benchSink = c.Clean(benchChanged)
				}
			})
		}
	}
}

// BenchmarkLongQuery covers the long-query shapes of R1-PERF-1 and PERF-3:
// only separators, many tiny params, and a signed URL whose utm_* names
// reach the ParamRegexps Keep scan (PERF-4).
func BenchmarkLongQuery(b *testing.B) {
	c := Default()
	for name, in := range map[string]string{
		"separators": "https://example.com/?" + strings.Repeat("&", 8100),
		"tiny":       "https://www.example.com/x?" + strings.Repeat("a=1&", 2025),
		"signed":     "https://example.com/f?X-Amz-Signature=1&" + strings.Repeat("utm_source=1&a=1&", 400),
	} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				benchSink = c.Clean(in)
			}
		})
	}
}
