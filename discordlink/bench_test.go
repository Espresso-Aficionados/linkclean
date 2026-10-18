package discordlink

import "testing"

// §8: 1 MB ≤ 50 ms, not CI-gating. B/op is the arena plus the output
// (ceiling 48 bytes per input byte).

func BenchmarkFindHostile(b *testing.B) {
	for _, sh := range hostileShapes {
		for _, sz := range []struct {
			name string
			n    int
		}{{"64KB", 64 << 10}, {"1MB", 1 << 20}} {
			s := sh.gen(sz.n)
			b.Run(sh.name+"/"+sz.name, func(b *testing.B) {
				b.SetBytes(int64(len(s)))
				b.ReportAllocs()
				for b.Loop() {
					findSink = Find(s)
				}
			})
		}
	}
}

func BenchmarkFindRealistic(b *testing.B) {
	b.SetBytes(int64(len(realisticMessage)))
	b.ReportAllocs()
	for b.Loop() {
		findSink = Find(realisticMessage)
	}
}
