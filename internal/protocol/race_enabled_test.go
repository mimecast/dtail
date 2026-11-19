//go:build race

package protocol

// raceEnabled tells tests that the race detector instruments this build. It
// disables the compiler's fusing of bytes.Buffer's append(nil, make(...)...)
// growth into one allocation, so a grow allocates its storage twice.
const raceEnabled = true
