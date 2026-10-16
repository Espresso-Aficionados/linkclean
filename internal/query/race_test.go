//go:build race

package query

// The race detector makes some otherwise elided allocations real.
const raceEnabled = true
