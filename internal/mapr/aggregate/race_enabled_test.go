//go:build race

package aggregate

// raceEnabled reports whether the test binary runs under the race detector,
// which makes sync.Pool drop pooled items at random and so defeats allocation
// counting on pooled paths.
const raceEnabled = true
