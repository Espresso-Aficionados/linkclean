package discordlink

import (
	"strings"
	"testing"
)

// linearC is the step constant of TestFindLinear: every shape must finish in
// at most linearC·n·(1+min(depth, maxDepth)) steps. The largest measured
// ratio is about 8.3 (masked-close-chain, depth 0).
const linearC = 12

// TestFindLinear checks D4's linear closer lookup without timing: every
// hostile shape at 64 KB and 1 MB stays within its step budget, which
// findSteps enforces, so a quadratic regression aborts at once instead of
// hanging CI; and no cursor query ever goes backwards.
func TestFindLinear(t *testing.T) {
	for _, sh := range hostileShapes {
		for _, n := range []int{64 << 10, 1 << 20} {
			s := sh.gen(n)
			budget := linearC * n * (1 + min(sh.depth, maxDepth))
			_, steps, rescans, aborted := findSteps(s, budget)
			switch {
			case aborted:
				t.Errorf("%s/%d: aborted after %d steps (budget %d)", sh.name, n, steps, budget)
			case rescans != 0:
				t.Errorf("%s/%d: %d rescans", sh.name, n, rescans)
			}
			t.Logf("%s/%d: %.2f steps/byte", sh.name, n, float64(steps)/float64(n))
		}
	}
}

// TestFindLinearBudget checks the abort itself: a budget that is too small
// stops the scan soon after the prepass (one step per byte, uninterrupted).
func TestFindLinearBudget(t *testing.T) {
	s := "https://a.example/x " + strings.Repeat("[a](b ", 1000)
	if _, steps, _, aborted := findSteps(s, 100); !aborted || steps > len(s)+100 {
		t.Errorf("budget 100: aborted %v after %d steps", aborted, steps)
	}
	if links, _, _, aborted := findSteps(s, 0); aborted || len(links) != 1 {
		t.Errorf("budget 0: aborted %v, %d links", aborted, len(links))
	}
}
