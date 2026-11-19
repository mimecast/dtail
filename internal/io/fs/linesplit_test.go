package fs

import (
	"context"
	"testing"
)

// TestSplitLine checks each kind of token SplitLine returns, at a limit of 4.
func TestSplitLine(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		atEOF       bool
		wantAdvance int
		wantToken   string
		wantEnd     LineEnd
	}{
		{"no data left", "", true, 0, "", LineEndNone},
		{"more data needed below the limit", "abc", false, 0, "", LineEndNone},
		{"more data needed at exactly the limit", "abcd", false, 0, "", LineEndNone},
		{"whole line below the limit", "ab\ncd", false, 3, "ab\n", LineEndNewline},
		{"whole line of exactly the limit", "abcd\n", false, 5, "abcd\n", LineEndNewline},
		{"CRLF line of exactly the limit", "abc\r\n", false, 5, "abc\r\n", LineEndNewline},
		{"CRLF line above the limit", "abcd\r\n", false, 4, "abcd", LineEndCut},
		{"newline past the limit", "abcde\n", false, 4, "abcd", LineEndCut},
		{"no newline past the limit", "abcde", false, 4, "abcd", LineEndCut},
		{"final line below the limit", "abc", true, 3, "abc", LineEndEOF},
		{"final line of exactly the limit", "abcd", true, 4, "abcd", LineEndEOF},
		{"final line above the limit", "abcdef", true, 4, "abcd", LineEndCut},
		{"empty line", "\nx", false, 1, "\n", LineEndNewline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			advance, token, end := SplitLine([]byte(tt.data), tt.atEOF, 4)
			if advance != tt.wantAdvance || string(token) != tt.wantToken || end != tt.wantEnd {
				t.Fatalf("SplitLine(%q, %v, 4) = %d, %q, %d; want %d, %q, %d", tt.data, tt.atEOF,
					advance, token, end, tt.wantAdvance, tt.wantToken, tt.wantEnd)
			}
		})
	}
}

func TestLineLimitDefaultsToOneMiB(t *testing.T) {
	for maxLineLength, want := range map[int]int{-1: mib, 0: mib, 1: 1, 3 * mib: 3 * mib} {
		if got := LineLimit(maxLineLength); got != want {
			t.Errorf("LineLimit(%d) = %d, want %d", maxLineLength, got, want)
		}
	}
}

func TestSendLongLineWarning(t *testing.T) {
	if !SendLongLineWarning(context.Background(), nil, "file", nil) {
		t.Fatal("warning without a channel reported a cancellation")
	}

	messages := make(chan string, 1)
	if !SendLongLineWarning(context.Background(), nil, "file", messages) {
		t.Fatal("warning to a channel with room reported a cancellation")
	}
	if got, want := <-messages, LongLineWarning(nil, "file"); got != want {
		t.Fatalf("warning = %q, want %q", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if SendLongLineWarning(ctx, nil, "file", make(chan string)) {
		t.Fatal("warning to a blocked channel of a canceled read reported success")
	}
}
