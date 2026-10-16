package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Espresso-Aficionados/linkclean/internal/rawurl"
	"github.com/Espresso-Aficionados/linkclean/internal/rule"
)

// TestScopeMemo: every rule's scope is evaluated at most once per round,
// whatever its ordinal, so rules whose ordinals differ by a multiple of 64
// do not evict each other (PERF3-1). Covers the stack memo and the pooled
// one of a Cleaner with more than stackRules rules.
func TestScopeMemo(t *testing.T) {
	for _, n := range []int{130, stackRules + 70} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			rs := make([]rule.Rule, n)
			for i := range rs {
				rs[i] = rule.Rule{ID: fmt.Sprint("r", i), Action: rule.Strip, Include: []string{"*://*/*"},
					URLRegexps: []string{fmt.Sprintf(`[?&]p%d=`, i)}, Params: []string{fmt.Sprint("p", i)}}
			}
			p, err := Compile([]rule.Set{{Name: "s", Rules: rs}})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := NewCleaner([]*Program{p}, Options{MaxUnwrap: 5, MaxURLLength: 8192}, Lists{})
			u, _ := rawurl.Parse("https://example.com/?p0=1&p64=1")
			cl := call{c: c}
			if c.scopeWords > stackRules/64 {
				cl.bigScope = c.scopePool.Get().(*[]uint64)
				defer c.scopePool.Put(cl.bigScope)
			}
			cl.reset(&u)
			ords := []int32{0, 64, 128, int32(n - 1)}
			for range 2 {
				for _, o := range ords {
					want := o == 0 || o == 64
					if got := cl.inScope(o, c.rule(o)); got != want {
						t.Errorf("inScope(%d) = %v, want %v", o, got, want)
					}
				}
			}
			bits := cl.scopeBits()
			for _, o := range ords {
				if bits[o/64]&(1<<(o%64)) == 0 {
					t.Errorf("rule %d: answer not memoized", o)
				}
			}
			cl.newRound()
			for _, o := range ords {
				if bits[o/64]&(1<<(o%64)) != 0 {
					t.Errorf("rule %d: memo survives newRound", o)
				}
			}
		})
	}
}

// TestUnwrapTargetLength: VALID rejects a decoded target longer than
// MaxURLLength (§6). Through Clean only a wrapper that is itself an earlier
// hop's re-serialized target can be that long, so the check is driven
// directly.
func TestUnwrapTargetLength(t *testing.T) {
	p, err := Compile([]rule.Set{{Name: "s", Rules: []rule.Rule{{
		ID: "w", Action: rule.Unwrap, Include: []string{"*://wrap.example.org/*"},
		Unwrap: &rule.UnwrapSpec{QueryParams: []string{"u"}, PrependScheme: "https"},
	}}}})
	if err != nil {
		t.Fatal(err)
	}
	target := "example.net/" + strings.Repeat("a", 40) // 52 bytes, 60 with the scheme
	u, _ := rawurl.Parse("https://wrap.example.org/?u=" + target)
	for _, tc := range []struct {
		max int
		ok  bool
	}{{59, false}, {60, true}} {
		c, _ := NewCleaner([]*Program{p}, Options{MaxUnwrap: 5, MaxURLLength: tc.max}, Lists{})
		cl := call{c: c}
		cl.reset(&u)
		got, _, ok := cl.unwrapOnce()
		if ok != tc.ok || ok && got != "https://"+target {
			t.Errorf("MaxURLLength %d: %q, %v; want ok %v", tc.max, got, ok, tc.ok)
		}
	}
}
