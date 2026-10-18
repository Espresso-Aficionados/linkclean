package discordlink

import (
	"runtime"
	"strings"
	"testing"
)

var findSink []Link

// TestFindAllocs pins Find's allocations: none without "http"; the index and
// the output slice for bare links; and one arena on top for every opener
// class.
func TestFindAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not deterministic under -race")
	}
	for _, c := range []struct {
		name string
		in   string
		max  float64
	}{
		{"no http", repeatTo("plain text with *stars*, _under_ [brackets](x) and `code` ", 2048), 0},
		{"3 bare", "see https://a.com/x and https://b.com/y, then https://c.com/z.", 2},
		{"3 among every opener", "\\* ``` `c` <@1> <https://a.com/x> [l](https://b.com/y) ||s|| **b** __u__ ~~d~~ *e* _f_ https://c.com/z", 4},
	} {
		if strings.Contains(c.in, "http") != (c.max > 0) {
			t.Fatalf("%s: bad input", c.name)
		}
		if n := testing.AllocsPerRun(100, func() { findSink = Find(c.in) }); n > c.max {
			t.Errorf("%s: %v allocs, want ≤ %v", c.name, n, c.max)
		}
	}
	if got := len(Find("\\* ``` `c` <@1> <https://a.com/x> [l](https://b.com/y) ||s|| **b** __u__ ~~d~~ *e* _f_ https://c.com/z")); got != 3 {
		t.Errorf("every opener: %d links, want 3", got)
	}
}

// TestFindMemory pins the memory Find's doc states: its tables take at most
// 48 bytes per byte of content, even for content that holds every opener
// class (about 40 in practice).
func TestFindMemory(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation sizes are not deterministic under -race")
	}
	s := "\\* ``` `c` <@1> <https://a.com/x> [l](https://b.com/y) ||s|| **b** __u__ ~~d~~ *e* _f_ https://c.com/z " + strings.Repeat("a", 1<<20)
	const runs = 4
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	for range runs {
		findSink = Find(s)
	}
	runtime.ReadMemStats(&m1)
	if len(findSink) != 3 {
		t.Fatalf("%d links, want 3", len(findSink))
	}
	perByte := float64(m1.TotalAlloc-m0.TotalAlloc) / runs / float64(len(s))
	t.Logf("%.1f bytes allocated per byte of content", perByte)
	if perByte > 48 {
		t.Errorf("%.1f bytes allocated per byte of content, want at most 48 (Find's doc states it)", perByte)
	}
}
