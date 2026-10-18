//go:build race

package unshorten

// raceEnabled reports whether the race detector is on. Under it, timing and
// allocation counts are not deterministic.
const raceEnabled = true
