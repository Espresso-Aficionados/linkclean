package linkclean

import (
	"math/rand/v2"
	"slices"
	"testing"
)

// TestDeterminism: shuffling the order of Sources, of the rules in each set
// and of Extra changes no Clean output over the corpus and the goldens
// (§14.6). Only unwrap priority depends on order (§6), and no input here has
// two wrappers that extract different targets.
func TestDeterminism(t *testing.T) {
	var inputs []string
	for _, it := range readCorpus(t) {
		inputs = append(inputs, it.Input)
	}
	for _, c := range readCases(t, "testdata/golden/positive.txtar") {
		inputs = append(inputs, c.In)
	}
	extra := []Rule{
		{ID: "a", Action: Strip, Include: []string{"*://*/*"}, Params: []string{"ref_src", "si"}},
		{ID: "b", Action: Keep, Include: []string{"*://*.youtube.com/*"}, Params: []string{"t"}},
		{ID: "c", Action: Strip, Include: []string{"*://*.amazon.*/*"}, PathRegexps: []string{"/gp/r\\.html"}},
	}
	for _, p := range []AffiliatePolicy{AffiliateKeep, AffiliateStrip} {
		base := MustNew(Config{Extra: extra, Affiliate: p})
		want := make([]string, len(inputs))
		for i, in := range inputs {
			want[i] = base.Clean(in)
		}
		rng := rand.New(rand.NewPCG(1, uint64(p)))
		for trial := 0; trial < 5; trial++ {
			srcs := DefaultSources()
			rng.Shuffle(len(srcs), func(i, j int) { srcs[i], srcs[j] = srcs[j], srcs[i] })
			for k := range srcs {
				rs := srcs[k].Rules
				rng.Shuffle(len(rs), func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
			}
			ex := slices.Clone(extra)
			rng.Shuffle(len(ex), func(i, j int) { ex[i], ex[j] = ex[j], ex[i] })
			c := MustNew(Config{Sources: srcs, Extra: ex, Affiliate: p})
			for i, in := range inputs {
				if got := c.Clean(in); got != want[i] {
					t.Errorf("policy %d trial %d: %s\n  got  %s\n  want %s", p, trial, in, got, want[i])
				}
			}
		}
	}
}
