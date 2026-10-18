//go:build race

package discordlink

// raceEnabled reports whether the race detector is on. Under it, allocation
// counts are not deterministic, so allocation tests skip.
const raceEnabled = true
