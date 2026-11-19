package handlers

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// framingTestContent holds characters whose UTF-8 encoding contains the
// MessageDelimiter byte 0xAC ('€', 'Ь', '¬'), and a raw 0xFF byte.
const framingTestContent = "price 5€ ok\nsoft Ь sign\nnot ¬ sign\nbyte \xff\xac end\n"

// splitFrames cuts a server-to-client stream at every MessageDelimiter and
// decodes each frame in framing, as a client receive loop does.
func splitFrames(stream []byte, framing protocol.Framing) []string {
	var frames []string
	var scratch []byte
	for len(stream) > 0 {
		end := bytes.IndexByte(stream, protocol.MessageDelimiter)
		if end < 0 {
			break
		}
		frames = append(frames, string(framing.DecodeFrame(stream[:end], &scratch)))
		stream = stream[end+1:]
	}
	return frames
}

func TestSessionFramerSwitchesToEscapedFramingAfterCapabilitiesFrame(t *testing.T) {
	handler := newReadTestHandler()
	capabilities := protocol.HiddenCapabilitiesPrefix + protocol.CapabilityEscapedFramingV1
	handler.serverMessages <- capabilities

	enabled := make(chan bool, 1)
	go func() { enabled <- handler.enableEscapedFraming(context.Background()) }()
	select {
	case <-enabled:
		t.Fatal("escaped framing was enabled before the capabilities frame was encoded")
	case <-time.After(50 * time.Millisecond):
	}

	if got := splitFrames(readExactly(t, handler, 4096, len(capabilities)+1),
		protocol.RawFraming); len(got) != 1 || got[0] != capabilities {
		t.Fatalf("capabilities frame = %q, want raw %q", got, capabilities)
	}
	select {
	case ok := <-enabled:
		if !ok {
			t.Fatal("enableEscapedFraming() = false, want true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("escaped framing was not enabled after the capabilities frame")
	}
	// No command arrives through Write here to settle the session framing,
	// without which Read holds back every frame after the capabilities.
	handler.settleFraming()

	handler.serverMessages <- "server 5€"
	handler.maprMessages <- "group≔Ь¬"
	var want bytes.Buffer
	protocol.EscapedFraming.EncodeMessage(&want, protocol.Message{
		Kind: protocol.MessageServer, Hostname: "testhost", Content: "server 5€"})
	protocol.EscapedFraming.EncodeMessage(&want, protocol.Message{
		Kind: protocol.MessageAggregate, Hostname: "testhost", Content: "group≔Ь¬"})
	stream := readExactly(t, handler, 4096, want.Len())
	if !bytes.Equal(stream, want.Bytes()) {
		t.Fatalf("escaped frames = %q, want %q", stream, want.Bytes())
	}
	frames := splitFrames(stream, protocol.EscapedFraming)
	if len(frames) != 2 || frames[0] != "SERVER|testhost|server 5€" ||
		frames[1] != "AGGREGATE|testhost|group≔Ь¬" {
		t.Fatalf("decoded frames = %q", frames)
	}
}

func TestSessionFramerEnableEscapedFramingGivesUpWithContext(t *testing.T) {
	handler := newReadTestHandler()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if handler.enableEscapedFraming(ctx) {
		t.Fatal("enableEscapedFraming() = true without an encoded capabilities frame")
	}
	if handler.framing() != protocol.RawFraming {
		t.Fatal("session left raw framing although the switch gave up")
	}
}

func newFramingTestServerHandler(t *testing.T, capabilities []string) *ServerHandler {
	t.Helper()
	return newFramingTestServerHandlerWithConfig(t, capabilities,
		&config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true})
}

func newFramingTestServerHandlerWithConfig(t *testing.T, capabilities []string,
	serverCfg *config.ServerConfig) *ServerHandler {
	t.Helper()
	return newFramingTestServerHandlerWithLogger(t, capabilities, serverCfg, handlerTestLogger)
}

func newFramingTestServerHandlerWithLogger(t *testing.T, capabilities []string,
	serverCfg *config.ServerConfig, logger logging.Logger) *ServerHandler {
	t.Helper()
	handler, err := NewServerHandler(context.Background(), &userserver.User{Name: config.ContinuousUser},
		Dependencies{
			CatLimiter:   make(chan struct{}, 4),
			TailLimiter:  make(chan struct{}, 4),
			ServerConfig: serverCfg,
			AuthKeyStore: authkey.New(time.Hour, 5),
			Loggers:      HandlerLoggers{Diagnostics: logger, Reader: logger},
			Capabilities: capabilities,
			Hostname:     "testhost",
		})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	return handler
}

// runFramingCatSession runs one cat session, with the escaped framing
// request first when requested, and returns all bytes sent to the client.
func runFramingCatSession(t *testing.T, handler *ServerHandler, path, options string,
	request bool) []byte {
	t.Helper()
	var first []string
	if request {
		first = []string{protocol.EscapedFramingCommand}
	}
	return runCatSessionAfter(t, handler, path, options, first...)
}

// runCatSessionAfter runs one cat session whose command frames follow the
// first ones, and returns all bytes sent to the client.
func runCatSessionAfter(t *testing.T, handler *ServerHandler, path, options string,
	first ...string) []byte {
	t.Helper()
	spec := session.Spec{Mode: omode.CatClient, Files: []string{path}, Options: options, Regex: "."}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	commands = append(slices.Clone(first), commands...)

	commandWg := wrapHandlerCommandsForJoin(handler)
	output := &testOutput{}
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, output, &writeMu)
	var frames strings.Builder
	for _, command := range commands {
		frames.WriteString(encodeTestCommand(command))
	}
	writeMu.Lock()
	_, writeErr := handler.Write([]byte(frames.String()))
	writeMu.Unlock()
	if writeErr != nil {
		t.Fatalf("write commands: %v", writeErr)
	}
	select {
	case <-handler.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("session did not shut down after the cat command finished")
	}
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	waitForCommandJoin(t, commandWg, 10*time.Second)
	return []byte(output.String())
}

// payloadOf returns the log content a client prints from decoded frames:
// plain frames verbatim, REMOTE frames by their content field.
func payloadOf(t *testing.T, frames []string) string {
	t.Helper()
	var payload strings.Builder
	for _, frame := range frames {
		switch {
		case strings.HasPrefix(frame, "."):
		case strings.HasPrefix(frame, protocol.LineMessageID+"|"):
			line, err := protocol.DecodeLine(frame)
			if err != nil {
				t.Fatalf("DecodeLine(%q): %v", frame, err)
			}
			payload.WriteString(line.Content)
		default:
			payload.WriteString(frame)
		}
	}
	return payload.String()
}

func TestServerHandlerEscapesFramesOnlyWhenNegotiated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utf8.log")
	if err := os.WriteFile(path, []byte(framingTestContent), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	advertised := ServerCapabilities("linux", false)
	tests := []struct {
		name         string
		capabilities []string
		request      bool
		wantFraming  protocol.Framing
	}{
		{name: "advertised and requested", capabilities: advertised, request: true,
			wantFraming: protocol.EscapedFraming},
		{name: "advertised, older client", capabilities: advertised, request: false,
			wantFraming: protocol.RawFraming},
		{name: "requested, not advertised", capabilities: nil, request: true,
			wantFraming: protocol.RawFraming},
	}
	for _, test := range tests {
		for _, options := range []string{"plain=true", ""} {
			t.Run(test.name+"/"+options, func(t *testing.T) {
				handler := newFramingTestServerHandler(t, test.capabilities)
				stream := runFramingCatSession(t, handler, path, options, test.request)
				frames := splitFrames(stream, test.wantFraming)
				got := payloadOf(t, frames)
				if test.wantFraming == protocol.EscapedFraming && got != framingTestContent {
					t.Fatalf("payload = %q, want %q", got, framingTestContent)
				}
				if test.wantFraming == protocol.RawFraming &&
					bytes.Contains(stream, []byte{protocol.FrameEscape, 0x01}) {
					t.Fatalf("raw session sent an escape sequence: %q", stream)
				}
				if test.wantFraming == protocol.RawFraming && got == framingTestContent {
					t.Fatal("raw session unexpectedly kept lines with 0xAC whole")
				}
			})
		}
	}
}

// TestServerHandlerHonoursOnlyAFirstEscapedFramingRequest: a framing request
// switches the session only as its first command; a later one is ignored, so
// the session stays raw, and a repeated one changes nothing.
func TestServerHandlerHonoursOnlyAFirstEscapedFramingRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "utf8.log")
	if err := os.WriteFile(path, []byte(framingTestContent), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	tests := []struct {
		name        string
		first       []string
		wantFraming protocol.Framing
	}{
		{name: "late request", first: []string{".ack late first", protocol.EscapedFramingCommand},
			wantFraming: protocol.RawFraming},
		{name: "repeated request",
			first:       []string{protocol.EscapedFramingCommand, protocol.EscapedFramingCommand},
			wantFraming: protocol.EscapedFraming},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := newFramingTestServerHandler(t, ServerCapabilities("linux", false))
			stream := runCatSessionAfter(t, handler, path, "plain=true", test.first...)
			if handler.framing() != test.wantFraming {
				t.Fatalf("session framing = %s, want %s", handler.framing(), test.wantFraming)
			}
			escaped := bytes.Contains(stream, []byte{protocol.FrameEscape, 0x01})
			if escaped != (test.wantFraming == protocol.EscapedFraming) {
				t.Fatalf("stream has escape sequences = %v, want framing %s: %q",
					escaped, test.wantFraming, stream)
			}
			if test.wantFraming == protocol.EscapedFraming {
				if got := payloadOf(t, splitFrames(stream, test.wantFraming)); got != framingTestContent {
					t.Fatalf("payload = %q, want %q", got, framingTestContent)
				}
			}
		})
	}
}

// escapedMaxLineLength is the MaxLineLength of the escaped output buffer
// tests: far above a 64 KiB writer batch, so that an escaped maximum-length
// line no longer fits the minimum raw buffer cap, yet quick to read.
const escapedMaxLineLength = 256 * 1024

// writeMaxLengthLineFile writes a file holding a short line, a line of
// exactly escapedMaxLineLength bytes before its newline (MaxLineLength does
// not count the "\n"), built from unit and padded with 'x' when unit does not
// divide the length, and another short line, and returns its path and content.
func writeMaxLengthLineFile(t *testing.T, unit string) (string, string) {
	t.Helper()
	long := strings.Repeat(unit, escapedMaxLineLength/len(unit)) +
		strings.Repeat("x", escapedMaxLineLength%len(unit))
	content := "first line\n" + long + "\n" + "tail line\n"
	path := filepath.Join(t.TempDir(), "long.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	return path, content
}

// TestServerHandlerDeliversEscapedMaxLengthLinesAtMinimumBuffer: at the
// smallest OutputBufferMaxBytes the handler accepts, a maximum-length line of
// 0xAC bytes (twice as long once escaped) or of '€' still reaches the client
// whole, together with the lines batched before and after it, in both line
// formats.
func TestServerHandlerDeliversEscapedMaxLengthLinesAtMinimumBuffer(t *testing.T) {
	for _, unit := range []string{"\xac", "€", "\xff"} {
		path, content := writeMaxLengthLineFile(t, unit)
		for _, options := range []string{"plain=true", ""} {
			t.Run(fmt.Sprintf("%q/%s", unit, options), func(t *testing.T) {
				handler := newFramingTestServerHandlerWithConfig(t, ServerCapabilities("linux", false),
					&config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true,
						MaxLineLength:        escapedMaxLineLength,
						OutputBufferMaxBytes: mustMinimumOutputBufferMaxBytes(t, escapedMaxLineLength)})
				stream := runFramingCatSession(t, handler, path, options, true)
				frames := splitFrames(stream, protocol.EscapedFraming)
				for _, frame := range frames {
					if strings.Contains(frame, protocol.HiddenCommandFailedPrefix) {
						t.Fatalf("session reported a failed command: %q", frame)
					}
				}
				if got := payloadOf(t, frames); got != content {
					t.Fatalf("payload of %d bytes, want the %d bytes of the file", len(got), len(content))
				}
			})
		}
	}
}

func TestEscapedFramingRaisesOnlyAnEscapedSessionsBufferCap(t *testing.T) {
	minimum := mustMinimumOutputBufferMaxBytes(t, escapedMaxLineLength)
	escaped, ok := escapedOutputBufferMaxBytes(escapedMaxLineLength)
	if !ok || escaped != 2*escapedMaxLineLength+outputBufferLineOverhead {
		t.Fatalf("escapedOutputBufferMaxBytes = %d, %v", escaped, ok)
	}
	for _, test := range []struct {
		name       string
		request    bool
		configured int
		want       int
	}{
		{name: "raw session keeps the configured cap", configured: minimum, want: minimum},
		{name: "escaped session raises a smaller cap", request: true, configured: minimum, want: escaped},
		{name: "escaped session keeps a larger cap", request: true, configured: 4 * escaped,
			want: 4 * escaped},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newFramingTestServerHandlerWithConfig(t, ServerCapabilities("linux", false),
				&config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true,
					MaxLineLength: escapedMaxLineLength, OutputBufferMaxBytes: test.configured})
			path := filepath.Join(t.TempDir(), "short.log")
			if err := os.WriteFile(path, []byte("short\n"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			runFramingCatSession(t, handler, path, "plain=true", test.request)
			handler.output.mu.Lock()
			got := handler.output.resolvedBufferMaxBytes()
			handler.output.mu.Unlock()
			if got != test.want {
				t.Fatalf("session buffer cap = %d, want %d", got, test.want)
			}
		})
	}
}

func TestEscapedOutputBufferMaxBytesRejectsOverflow(t *testing.T) {
	if _, ok := escapedOutputBufferMaxBytes(math.MaxInt / 2); ok {
		t.Fatal("escapedOutputBufferMaxBytes accepted a sum that does not fit an int")
	}
	largest := (math.MaxInt - outputBufferLineOverhead) / 2
	if got, ok := escapedOutputBufferMaxBytes(largest); !ok || got < largest {
		t.Fatalf("escapedOutputBufferMaxBytes(%d) = %d, %v", largest, got, ok)
	}
	if _, ok := escapedOutputBufferMaxBytes(largest + 1); ok {
		t.Fatalf("escapedOutputBufferMaxBytes(%d) accepted a sum that does not fit an int", largest+1)
	}
}

// TestServerHandlerReportsOutputTooLargeForTheBuffer: output that does not
// fit the session's output buffer is reported to the client with a visible
// warning and a failed-command reason, never dropped silently.
func TestServerHandlerReportsOutputTooLargeForTheBuffer(t *testing.T) {
	path, _ := writeMaxLengthLineFile(t, "a")
	handler := newFramingTestServerHandlerWithLogger(t, ServerCapabilities("linux", false),
		&config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true,
			MaxLineLength: escapedMaxLineLength}, &framingWarnLogger{})
	// No valid configuration gets here; shrink the cap below one line batch
	// the way an oversized frame would exceed it.
	handler.output.mu.Lock()
	handler.output.bufferMaxBytes = escapedMaxLineLength / 2
	handler.output.mu.Unlock()

	frames := splitFrames(runFramingCatSession(t, handler, path, "", false), protocol.RawFraming)
	var warned, reported bool
	for _, frame := range frames {
		warned = warned || strings.HasPrefix(frame, "SERVER|testhost|") &&
			strings.Contains(frame, "Dropped output too large for the session output buffer")
		reported = reported ||
			strings.TrimSuffix(frame, "\n") == protocol.HiddenCommandFailedPrefix+protocol.CommandFailureOutputTooLarge
	}
	if !warned || !reported {
		t.Fatalf("warning sent = %v, failure reported = %v, want both: %q", warned, reported, frames)
	}
}

// framingWarnLogger returns the text of its warnings, as the real logger
// returns the formatted line, so that warnings sent to the client show.
type framingWarnLogger struct {
	logging.NopLogger
}

func (*framingWarnLogger) Warn(args ...any) string {
	return fmt.Sprint(args...)
}
