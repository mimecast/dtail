package pgo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCountRawSamples verifies that the zero-sample sanity check correctly
// distinguishes a profile with samples from an idle (zero-sample) capture like
// the one that silently produced a useless dserver.pprof. This is the testable
// seam behind verifyProfileNonEmpty.
func TestCountRawSamples(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{
			name: "two samples",
			raw: strings.Join([]string{
				"PeriodType: cpu nanoseconds",
				"Period: 10000000",
				"Duration: 8.0",
				"Samples:",
				"samples/count cpu/nanoseconds",
				"          1   10000000: 1 2 3 4 5 6 7 8 ",
				"          3   30000000: 9 10 11 5 6 7 8 ",
				"Locations",
				"     1: 0x1234 foo",
			}, "\n"),
			want: 2,
		},
		{
			name: "idle capture has zero samples",
			raw: strings.Join([]string{
				"PeriodType: cpu nanoseconds",
				"Period: 10000000",
				"Duration: 15.0",
				"Samples:",
				"samples/count cpu/nanoseconds",
				"Locations",
				"Mappings",
			}, "\n"),
			want: 0,
		},
		{
			name: "empty input",
			raw:  "",
			want: 0,
		},
		{
			name: "no samples header",
			raw:  "PeriodType: cpu nanoseconds\nLocations\n",
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := countRawSamples(tc.raw); got != tc.want {
				t.Errorf("countRawSamples() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDetectHandshakeFailure verifies the representativeness guard behind the
// dtail workload: a client that logged an SSH handshake failure fell back to
// reconnect churn and must be rejected, while clean streaming output passes.
func TestDetectHandshakeFailure(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "clean streaming output",
			output: "2026-07-17T10:04:30 Hello line 40 ERROR test\nHello line 41 ERROR test\n",
			want:   "",
		},
		{
			name:   "handshake failed",
			output: "CLIENT|earth|WARN|localhost:12223|SSH handshake failed for localhost:12223: ...",
			want:   "SSH handshake failed",
		},
		{
			name:   "unable to authenticate",
			output: "ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey]",
			want:   "unable to authenticate",
		},
		{
			name:   "no key available",
			output: "Unable to find private SSH key information",
			want:   "Unable to find private SSH key",
		},
		{
			name:   "empty output",
			output: "",
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectHandshakeFailure(tc.output); got != tc.want {
				t.Errorf("detectHandshakeFailure() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVerifyProfileNonEmpty exercises verifyProfileNonEmpty against a missing
// file and a zero-byte file (both must fail); the go tool pprof-backed sample
// check is covered separately by TestCountRawSamples.
func TestVerifyProfileNonEmpty(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "missing.pprof")
	if err := verifyProfileNonEmpty("dcat", missing); err == nil {
		t.Error("expected error for missing profile, got nil")
	}

	empty := filepath.Join(dir, "empty.pprof")
	if err := os.WriteFile(empty, []byte{}, 0644); err != nil {
		t.Fatalf("creating empty profile: %v", err)
	}
	if err := verifyProfileNonEmpty("dtail", empty); err == nil {
		t.Error("expected error for zero-byte profile, got nil")
	}
}
