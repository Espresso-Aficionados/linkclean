package linkclean

import (
	"testing"
)

// TestConcurrentExplain calls Explain and Clean on shared Cleaners from many
// goroutines, including two Cleaners built on the shared compiled base
// (§8). Run under -race.
func TestConcurrentExplain(t *testing.T) {
	var inputs []string
	for _, it := range readCorpus(t) {
		inputs = append(inputs, it.Input)
	}
	for _, c := range readCases(t, "testdata/golden/positive.txtar") {
		inputs = append(inputs, c.In)
	}
	cleaners := []*Cleaner{
		Default(),
		MustNew(Config{Affiliate: AffiliateStrip}),
		MustNew(Config{DisableTags: []string{"lowconf"}, Extra: chainRules(3)}),
	}
	want := make([][]Result, len(cleaners))
	for i, c := range cleaners {
		for _, in := range inputs {
			want[i] = append(want[i], c.Explain(in))
		}
	}
	done := make(chan bool)
	for g := 0; g < 8; g++ {
		go func() {
			defer func() { done <- true }()
			for k := 0; k < 3; k++ {
				for i, c := range cleaners {
					for j := (g + k) % 7; j < len(inputs); j += 7 {
						r := c.Explain(inputs[j])
						if r.URL != want[i][j].URL || len(r.Removed) != len(want[i][j].Removed) || len(r.Hops) != len(want[i][j].Hops) {
							t.Errorf("cleaner %d, %s: concurrent result differs", i, inputs[j])
						}
						if c.Clean(inputs[j]) != r.URL {
							t.Errorf("cleaner %d, %s: Clean differs", i, inputs[j])
						}
					}
				}
			}
		}()
	}
	for g := 0; g < 8; g++ {
		<-done
	}
}

func BenchmarkParallelExplain(b *testing.B) {
	c := Default()
	items := readCorpus(b)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = c.Explain(items[i%len(items)].Input)
			i++
		}
	})
}
