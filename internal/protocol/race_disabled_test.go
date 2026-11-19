//go:build !race

package protocol

// raceEnabled tells tests that the race detector instruments this build (see
// race_enabled_test.go).
const raceEnabled = false
