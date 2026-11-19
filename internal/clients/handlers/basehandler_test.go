package handlers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/protocol"
)

func TestParseAuthKeyMessage(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantAuth bool
		wantOK   bool
		wantInfo string
	}{
		{
			name:     "server formatted success",
			message:  fmt.Sprintf("SERVER%s%s%sAUTHKEY OK\n", protocol.FieldSeparator(), "host1", protocol.FieldSeparator()),
			wantAuth: true,
			wantOK:   true,
		},
		{
			name:     "server formatted error",
			message:  fmt.Sprintf("SERVER%s%s%sAUTHKEY ERR feature disabled\n", protocol.FieldSeparator(), "host1", protocol.FieldSeparator()),
			wantAuth: true,
			wantOK:   false,
			wantInfo: "feature disabled",
		},
		{
			name:     "plain response success",
			message:  "AUTHKEY OK",
			wantAuth: true,
			wantOK:   true,
		},
		{
			name:     "not an authkey message",
			message:  fmt.Sprintf("SERVER%s%s%ssome other message", protocol.FieldSeparator(), "host1", protocol.FieldSeparator()),
			wantAuth: false,
			wantOK:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotAuth, gotOK, gotInfo := parseAuthKeyMessage(tc.message)
			if gotAuth != tc.wantAuth {
				t.Fatalf("Unexpected auth marker: got %v want %v", gotAuth, tc.wantAuth)
			}
			if gotOK != tc.wantOK {
				t.Fatalf("Unexpected ok marker: got %v want %v", gotOK, tc.wantOK)
			}
			if gotInfo != tc.wantInfo {
				t.Fatalf("Unexpected info: got %q want %q", gotInfo, tc.wantInfo)
			}
		})
	}
}

func TestHandleCapabilitiesMessage(t *testing.T) {
	handler := baseHandler{
		done:           internal.NewDone(),
		capabilities:   make(map[string]struct{}),
		capabilitiesCh: make(chan struct{}),
		sessionAcks:    make(chan SessionAck, 1),
	}

	handler.handleHiddenMessage(".syn capabilities query-update-v1 feature-two")

	if !handler.HasCapability(protocol.CapabilityQueryUpdateV1) {
		t.Fatalf("expected handler to track %q", protocol.CapabilityQueryUpdateV1)
	}
	if !handler.HasCapability("feature-two") {
		t.Fatalf("expected handler to track feature-two")
	}
	if handler.WaitForCapabilities(10*time.Millisecond) != true {
		t.Fatalf("expected capabilities wait to succeed")
	}

	capabilities := handler.Capabilities()
	if len(capabilities) != 2 {
		t.Fatalf("unexpected capabilities: %#v", capabilities)
	}
}

func TestWaitForCapabilitiesTimeout(t *testing.T) {
	handler := baseHandler{
		done:           internal.NewDone(),
		capabilities:   make(map[string]struct{}),
		capabilitiesCh: make(chan struct{}),
		sessionAcks:    make(chan SessionAck, 1),
	}

	if handler.WaitForCapabilities(5 * time.Millisecond) {
		t.Fatalf("expected capabilities wait to time out")
	}
}

func TestFormatServerErrorMessage(t *testing.T) {
	got := formatServerErrorMessage("srv1", "journal file targets require server capability journal-v1")
	// No trailing newline: the message is emitted via the diagnostic (Log) sink,
	// which appends the newline itself (see ReportServerError / RawLog).
	want := "SERVER|srv1|ERROR|journal file targets require server capability journal-v1"
	if got != want {
		t.Fatalf("formatServerErrorMessage() = %q, want %q", got, want)
	}
}

func TestParseSessionAckMessage(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    SessionAck
		wantOK  bool
	}{
		{
			name:    "start ok",
			message: ".syn session start ok 7",
			want: SessionAck{
				Action:     "start",
				Generation: 7,
			},
			wantOK: true,
		},
		{
			name:    "update ok",
			message: ".syn session update ok 8",
			want: SessionAck{
				Action:     "update",
				Generation: 8,
			},
			wantOK: true,
		},
		{
			name:    "error",
			message: ".syn session err query sessions not supported yet",
			want: SessionAck{
				Action: "error",
				Error:  "query sessions not supported yet",
			},
			wantOK: true,
		},
		{
			name:    "invalid",
			message: ".syn session start ok nope",
			wantOK:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseSessionAckMessage(tc.message)
			if ok != tc.wantOK {
				t.Fatalf("unexpected ok flag: got %v want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Fatalf("unexpected ack: got %#v want %#v", got, tc.want)
			}
		})
	}
}

func TestHandleSessionAckMessage(t *testing.T) {
	handler := baseHandler{
		done:        internal.NewDone(),
		sessionAcks: make(chan SessionAck, 1),
	}

	handler.handleHiddenMessage(".syn session update ok 4")

	ack, ok := handler.WaitForSessionAck(10 * time.Millisecond)
	if !ok {
		t.Fatalf("expected session ack")
	}
	if ack.Action != "update" || ack.Generation != 4 {
		t.Fatalf("unexpected session ack: %#v", ack)
	}
}

func TestHandleCloseConnectionAcknowledgesBeforeShutdown(t *testing.T) {
	handler := baseHandler{
		done:     internal.NewDone(),
		server:   "server-under-test",
		commands: make(chan string, 1),
	}

	handler.handleHiddenMessage(".syn close connection")

	select {
	case command := <-handler.commands:
		if command == "" {
			t.Fatal("expected close acknowledgement command")
		}
	case <-time.After(10 * time.Millisecond):
		t.Fatal("expected close acknowledgement command to be queued")
	}

	select {
	case <-handler.Done():
	default:
		t.Fatal("expected handler to be shut down after close acknowledgement")
	}
}

// TestReadDrainsAckBeforeEOF verifies that Read() always delivers the
// '.ack close connection' command before returning io.EOF when Done() fires
// concurrently. Without the priority-select fix, Go's non-deterministic select
// would randomly pick the Done() case and drop the queued ack ~50% of the time.
func TestReadDrainsAckBeforeEOF(t *testing.T) {
	// Run many iterations to catch the race reliably even with -race.
	const iterations = 500
	for i := 0; i < iterations; i++ {
		// Use an unbuffered commands channel so the ack write and Done() close
		// are truly concurrent, exposing the original non-deterministic select.
		handler := baseHandler{
			done:     internal.NewDone(),
			server:   "server-under-test",
			commands: make(chan string, 1),
		}

		// handleHiddenMessage calls SendMessage (queues ack) then Shutdown (closes Done).
		// After this call both handler.commands and handler.Done() are ready.
		handler.handleHiddenMessage(".syn close connection")

		buf := make([]byte, 4096)
		n, err := handler.Read(buf)
		if err != nil {
			// Done() won the select — the ack was dropped. This is the bug.
			t.Fatalf("iteration %d: Read returned io.EOF before draining the ack", i)
		}
		if n == 0 {
			t.Fatalf("iteration %d: Read returned 0 bytes without an error", i)
		}

		// Second Read must now return EOF since Done() is closed and commands is empty.
		_, err = handler.Read(buf)
		if err == nil {
			t.Fatalf("iteration %d: second Read should return io.EOF", i)
		}
	}
}

// newCommandReadTestHandler returns a handler for exercising Read directly
// and registers a cleanup that unblocks any Read still waiting on the
// commands channel, so a regression cannot leak a blocked goroutine.
func newCommandReadTestHandler(t *testing.T) *baseHandler {
	t.Helper()

	handler := &baseHandler{
		done:     internal.NewDone(),
		server:   "server-under-test",
		commands: make(chan string, 2),
	}
	t.Cleanup(handler.done.Shutdown)
	return handler
}

// readCommandsWithin drains wantLen bytes from the handler on a separate
// goroutine and fails the test when the reads do not finish within the
// timeout. The deadline matters: under a regression that drops the remainder
// of a partially delivered command, the next Read would block forever on the
// empty commands channel; the timeout turns that hang into a clean assertion
// (and shuts the handler down so the reader goroutine exits via Done).
func readCommandsWithin(t *testing.T, handler *baseHandler, bufSize, wantLen int) []byte {
	t.Helper()

	type result struct {
		data []byte
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		var got []byte
		p := make([]byte, bufSize)
		for len(got) < wantLen {
			n, err := handler.Read(p)
			if err != nil {
				resultCh <- result{got, err}
				return
			}
			got = append(got, p[:n]...)
		}
		resultCh <- result{got, nil}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Read() error after %d of %d bytes: %v",
				len(res.data), wantLen, res.err)
		}
		return res.data
	case <-time.After(5 * time.Second):
		handler.done.Shutdown()
		t.Fatalf("Read stalled: timed out waiting for %d bytes", wantLen)
		return nil
	}
}

// TestBaseHandlerReadLargeCommandAcrossMultipleReads verifies that a command
// frame larger than the caller's buffer (e.g. a huge base64-encoded regex or
// MapReduce query) is delivered completely across multiple Read calls instead
// of being truncated at the buffer boundary.
func TestBaseHandlerReadLargeCommandAcrossMultipleReads(t *testing.T) {
	handler := newCommandReadTestHandler(t)

	command := "grep " + strings.Repeat("x", 1000) + ";"
	handler.commands <- command

	got := readCommandsWithin(t, handler, 32, len(command))
	if string(got) != command {
		t.Fatalf("large command corrupted across reads:\ngot  %q\nwant %q",
			got, command)
	}
}

// TestBaseHandlerReadDrainsRemainderBeforeNextCommand verifies that the
// remainder of a partially delivered command is fully drained before the next
// queued command starts, so frames are never interleaved, and that an
// exact-fit read leaves no stale remainder behind.
func TestBaseHandlerReadDrainsRemainderBeforeNextCommand(t *testing.T) {
	handler := newCommandReadTestHandler(t)

	first := "first command payload;"
	second := "second;"
	handler.commands <- first
	handler.commands <- second

	// len(first) = 22, so first fills exactly two 11-byte reads (the second
	// of which is the remainder), then second must start fresh.
	got := readCommandsWithin(t, handler, 11, len(first)+len(second))
	if string(got) != first+second {
		t.Fatalf("commands interleaved or corrupted:\ngot  %q\nwant %q",
			got, first+second)
	}
}
