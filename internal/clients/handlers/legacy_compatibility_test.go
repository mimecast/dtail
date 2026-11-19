package handlers

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestFirstFrameRejectsUnsafeLegacyServer(t *testing.T) {
	for _, first := range [][]byte{
		capabilitiesFrame(protocol.CapabilityQueryUpdateV1),
		payloadFrames(protocol.RawFraming, "old server without capabilities"),
		payloadFrames(protocol.RawFraming, protocol.CapabilityEscapedFramingV1),
	} {
		handler := NewClientHandler("fixture", nil)
		defer handler.Shutdown()
		writeInChunks(t, handler, first, 1)
		if handler.Status() != 1 {
			t.Fatalf("status = %d, want 1", handler.Status())
		}
		// Later payload resembling capabilities must not rehabilitate the session.
		writeInChunks(t, handler, capabilitiesFrame(protocol.CapabilityEscapedFramingV1), 1)
		if handler.Status() != 1 {
			t.Fatal("late capabilities cleared the compatibility error")
		}
	}
}

func TestEscapedServerRemainsSupported(t *testing.T) {
	for _, capabilities := range [][]string{
		{protocol.CapabilityEscapedFramingV1},
		{protocol.CapabilityEscapedFramingV1, protocol.CapabilityControlFramesV1},
	} {
		recorder := &payloadRecorder{}
		handler := NewClientHandler("fixture", recorder)
		defer handler.Shutdown()
		framing := protocol.EscapedFraming
		if len(capabilities) == 2 {
			framing = protocol.EscapedControlFraming
		}
		input := append(capabilitiesFrame(capabilities...), payloadFrames(framing, framingTestLines...)...)
		writeInChunks(t, handler, input, 1)
		if handler.Status() > 0 {
			t.Fatalf("supported server status = %d", handler.Status())
		}
		if got := recorder.payload.String(); got != strings.Join(framingTestLines, "") {
			t.Fatalf("payload = %q", got)
		}
	}
}
