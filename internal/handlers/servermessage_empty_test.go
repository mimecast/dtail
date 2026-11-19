package handlers

import (
	"bytes"
	"strings"
	"testing"
)

// emptyServerMessageCase is one server message given to a framed (plain=false,
// serverless=false) output path, and whether that path must frame it.
type emptyServerMessageCase struct {
	name    string
	message string
	// generation, when non-zero, binds the message to that (current) session
	// generation, so it is not dropped as stale output but must still be
	// dropped for its empty text.
	generation uint64
	wantFrame  bool
}

// emptyServerMessageCases covers the empty server messages a framed client
// would print as a bare "SERVER|<host>|" line, and one message with text. The
// empty ones used to be dropped in plain mode only.
var emptyServerMessageCases = []emptyServerMessageCase{
	{name: "empty", message: ""},
	{name: "newline", message: "\n"},
	{name: "generation empty", message: "", generation: 1},
	{name: "generation newline", message: "\n", generation: 1},
	{name: "text", message: "x", wantFrame: true},
	{name: "generation text", message: "x", generation: 1, wantFrame: true},
}

// TestSessionFramerDropsEmptyServerMessagesWhenFramed checks that the session
// framer of a framed server-mode session encodes no frame for an empty server
// message, also when it carries a generation prefix, and frames one with text.
func TestSessionFramerDropsEmptyServerMessagesWhenFramed(t *testing.T) {
	for _, tc := range emptyServerMessageCases {
		t.Run(tc.name, func(t *testing.T) {
			// Active generation 1: a generation-1 message is current output.
			handler := newGenerationTestHandler(1)
			if handler.plainMode() || handler.serverless {
				t.Fatalf("handler plain=%v serverless=%v, want a framed session",
					handler.plainMode(), handler.serverless)
			}
			message := encodeGeneratedMessage(tc.generation, tc.message)

			buf := make([]byte, 256)
			// readServerMessage is promoted from the embedded sessionFramer.
			n := handler.readServerMessage(buf, message)
			got := string(buf[:n])
			assertServerMessageFrame(t, got, tc)
		})
	}
}

// TestDirectWriterDropsEmptyServerMessagesWhenFramed checks the same for a
// framed DirectWriter. A DirectWriter never sees a generation prefix: it is
// bound to its generation instead, so those cases use a writer of the current
// generation.
func TestDirectWriterDropsEmptyServerMessagesWhenFramed(t *testing.T) {
	for _, tc := range emptyServerMessageCases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			w := NewDirectWriter(&out, "testhost", false, false)
			if tc.generation != 0 {
				w = NewGeneratedDirectWriter(&out, "testhost", false, false,
					tc.generation, func() uint64 { return tc.generation })
			}

			if err := w.WriteServerMessage(tc.message); err != nil {
				t.Fatalf("WriteServerMessage(%q) error = %v", tc.message, err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush() error = %v", err)
			}
			assertServerMessageFrame(t, out.String(), tc)
		})
	}
}

func assertServerMessageFrame(t *testing.T, got string, tc emptyServerMessageCase) {
	t.Helper()

	if !tc.wantFrame {
		if got != "" {
			t.Fatalf("server message %q produced %q, want nothing", tc.message, got)
		}
		return
	}
	if !strings.HasPrefix(got, "SERVER") || !strings.Contains(got, "testhost") ||
		!strings.Contains(got, tc.message) {
		t.Fatalf("server message %q produced %q, want a SERVER frame from testhost",
			tc.message, got)
	}
}
