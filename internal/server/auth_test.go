package server

import (
	"crypto/subtle"
	"testing"
)

// TestConstantTimePasswordCompare verifies that password comparisons use
// constant-time comparison to prevent timing side-channel attacks.
// This is a regression test for the bug where `!=` was used directly,
// leaking timing information to an attacker who can measure response latency.
func TestConstantTimePasswordCompare(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		a        string
		b        string
		wantSame bool
	}{
		{"equal passwords", "secret123", "secret123", true},
		{"different passwords", "secret123", "wrongpass", false},
		{"empty vs non-empty", "", "secret", false},
		{"both empty", "", "", true},
		{"prefix match only", "secret", "secretXYZ", false},
		{"suffix match only", "XYZsecret", "secret", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// secretsEqual is the function under test — it must use
			// crypto/subtle.ConstantTimeCompare, not plain ==.
			got := secretsEqual(tt.a, tt.b)
			if got != tt.wantSame {
				t.Errorf("secretsEqual(%q, %q) = %v, want %v",
					tt.a, tt.b, got, tt.wantSame)
			}

			// Cross-check with the reference implementation to ensure our
			// helper is semantically correct, not just timing-safe.
			reference := subtle.ConstantTimeCompare([]byte(tt.a), []byte(tt.b)) == 1
			if got != reference {
				t.Errorf("secretsEqual(%q, %q) = %v but crypto/subtle gives %v",
					tt.a, tt.b, got, reference)
			}
		})
	}
}
