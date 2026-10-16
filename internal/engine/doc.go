// Package engine compiles normalized rules (internal/rule) and cleans URLs:
// guards, the bounded unwrap loop, fixed-point strip rounds with nested
// re-encoding inside each round, fragment handling and reassembly (design
// §4.3–§6, §8).
package engine
