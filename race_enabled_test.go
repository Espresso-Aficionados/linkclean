//go:build race

package linkclean

// raceEnabled reports whether the race detector is on. Under it, sync.Pool
// drops items at random, so pooled paths allocate nondeterministically.
const raceEnabled = true
