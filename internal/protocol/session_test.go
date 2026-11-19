package protocol

import "testing"

// TestIsFileReadFailure pins which failed-command reasons are file read
// failures. The scheduler writes a partial outfile after a job's TimeRange
// ended only when every failure is one, so a reason moving between the two
// kinds changes what a failed scheduled job writes. In particular, output
// dropped as too large for the session output buffer is no file read
// failure: the file was read, but its lines were lost on the way; nor is a
// map result whose final serialization did not reach the client in full.
func TestIsFileReadFailure(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{CommandFailureNoFile, true},
		{CommandFailureGlobCapped, true},
		{CommandFailurePermission, true},
		{CommandFailureReader, true},
		{CommandFailureReadingFile, true},
		{CommandFailureOutputTooLarge, false},
		{CommandFailureDecode, false},
		{CommandFailureRejected, false},
		{CommandFailureUnknown, false},
		{CommandFailureMapQuery, false},
		{CommandFailureMapIncomplete, false},
		{CommandFailureReadCommand, false},
		{"", false},
		{"read: a reason this client does not know", false},
	}
	for _, tt := range tests {
		if got := IsFileReadFailure(tt.reason); got != tt.want {
			t.Errorf("IsFileReadFailure(%q) = %v, want %v", tt.reason, got, tt.want)
		}
	}
}
