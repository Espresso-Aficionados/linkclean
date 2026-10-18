package discordlink

import (
	"slices"
	"testing"
)

// fuzzRefMax bounds the inputs FuzzFind compares with the naive reference.
const fuzzRefMax = 512

// FuzzFind checks Find's invariants on any input: links in bounds,
// ascending and non-overlapping, content[Start:End] == URL, UTF-8 aligned
// edges, an http(s) prefix, a valid Kind, a Label only on masked links, and
// determinism. For inputs up to 512 bytes it also requires Find to equal
// findRef(content, maxDepth), and logs where the nesting limit changes the
// answer (findRef uncapped).
func FuzzFind(f *testing.F) {
	for _, c := range loadFindCases(f) {
		if c.shape == "" {
			f.Add(c.in)
		}
	}
	_, rows := loadOracle(f)
	for _, r := range rows {
		f.Add(r.in)
	}
	for _, sh := range hostileShapes {
		f.Add(sh.gen(2048))
	}
	f.Fuzz(func(t *testing.T, content string) {
		links := Find(content)
		checkLinks(t, content, links)
		if again := Find(content); !slices.Equal(links, again) {
			t.Fatalf("Find is not deterministic:\n%+v\n%+v", links, again)
		}
		if len(content) > fuzzRefMax {
			return
		}
		ref := findRef(content, maxDepth)
		checkLinks(t, content, ref)
		if !slices.Equal(links, ref) {
			t.Fatalf("Find and findRef(content, %d) disagree:\nFind    %+v\nfindRef %+v", maxDepth, links, ref)
		}
		if unc := findRef(content, 0); !slices.Equal(ref, unc) {
			t.Logf("the nesting limit changes the answer:\ncapped   %+v\nuncapped %+v", ref, unc)
		}
	})
}
