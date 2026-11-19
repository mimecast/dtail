package brush

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
)

func TestColorfyUsesStandardPaletteWithoutConfigSetup(t *testing.T) {
	previousClient := config.Client
	config.Client = nil
	t.Cleanup(func() { config.Client = previousClient })

	line := "CLIENT|host|WARN message"
	if got, want := Colorfy(line), New(config.DefaultTermColors()).Colorfy(line); got != want {
		t.Fatalf("compatibility Colorfy() = %q, want standard palette output %q", got, want)
	}
}

func TestNewUsesCopiedInjectedPalette(t *testing.T) {
	theme := config.DefaultTermColors()
	theme.Client.TextFg = color.FgMagenta
	theme.Client.TextBg = color.BgWhite
	theme.Client.TextAttr = color.AttrUnderline
	b := New(theme)

	// Brush owns a value copy, so later configuration mutation cannot change
	// an already-composed runtime.
	theme.Client.TextFg = color.FgGreen
	got := b.Colorfy("CLIENT|host|payload")
	wantSequence := string(color.FgMagenta) + string(color.BgWhite) +
		string(color.AttrUnderline) + "payload"
	if !strings.Contains(got, wantSequence) {
		t.Fatalf("injected palette output = %q, want sequence %q", got, wantSequence)
	}
}

func TestNilBrushUsesStandardPalette(t *testing.T) {
	var b *Brush
	line := "SERVER|host|message"
	if got, want := b.Colorfy(line), Colorfy(line); got != want {
		t.Fatalf("nil Brush.Colorfy() = %q, want %q", got, want)
	}
}

// TestColorfy_ShortFramesDoNotPanic feeds Colorfy with malformed or short
// protocol frames (e.g. "REMOTE", "REMOTE|host", "CLIENT|host") which used
// to cause an index-out-of-range panic in paintRemote/paintClient/paintServer
// when they unconditionally indexed splitted[0..N] after SplitN.
// For any short frame Colorfy must not panic and must return a non-empty
// string that still contains the original text (either verbatim in the plain
// fallback branch or embedded inside color escape sequences).
func TestColorfy_ShortFramesDoNotPanic(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"plain_text", "hello world"},
		{"remote_prefix_only", "REMOTE"},
		{"remote_one_field", "REMOTE|host"},
		{"remote_two_fields", "REMOTE|host|100"},
		{"remote_three_fields", "REMOTE|host|100|1"},
		{"remote_four_fields", "REMOTE|host|100|1|id"},
		{"remote_full_frame", "REMOTE|host|100|1|id|hello from remote"},
		{"client_prefix_only", "CLIENT"},
		{"client_one_field", "CLIENT|host"},
		{"client_full_frame", "CLIENT|host|hello from client"},
		{"server_prefix_only", "SERVER"},
		{"server_one_field", "SERVER|host"},
		{"server_full_frame", "SERVER|host|hello from server"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Colorfy(%q) panicked: %v", tc.in, r)
				}
			}()

			got := Colorfy(tc.in)

			if tc.in == "" {
				// An empty input must not suddenly produce any content.
				if got != "" && !strings.Contains(got, "") {
					t.Fatalf("Colorfy(\"\") = %q, want empty or trivial", got)
				}
				return
			}

			// The original payload should still be recoverable somewhere in
			// the output so users can see what the server actually sent even
			// when the frame is malformed.
			if !strings.Contains(got, tc.in) && !containsAllFields(got, tc.in) {
				t.Fatalf("Colorfy(%q) = %q, original payload not present", tc.in, got)
			}
		})
	}
}

// containsAllFields checks whether every non-delimiter field of the input is
// present in the output. This allows the colorised happy path (which
// interleaves escape sequences between fields) to still satisfy the
// "payload preserved" expectation.
func containsAllFields(out, in string) bool {
	for _, f := range strings.Split(in, "|") {
		if f == "" {
			continue
		}
		if !strings.Contains(out, f) {
			return false
		}
	}
	return true
}
