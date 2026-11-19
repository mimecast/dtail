//go:build !proprietary

package logformat

import "testing"

// TestProbeMimecastStubsAreEmpty checks that the mimecast formats, stubs whose
// parser cannot be built in the open source build, yield an empty result and
// no error, so a probe of every format leaves them out.
func TestProbeMimecastStubsAreEmpty(t *testing.T) {
	sample := probeSampleOf("a", probeStatsLine, "service=web")
	for _, name := range []string{"mimecast", "mimecastgeneric"} {
		result := mustProbe(t, name, sample)
		if !result.Empty() {
			t.Errorf("Probe(%q) = %+v, want empty", name, result)
		}
	}
}
