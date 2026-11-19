package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestDirectWriter_ServerlessPlain tests plain serverless mode output
func TestDirectWriter_ServerlessPlain(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, true)

	// Write a line without trailing newline
	err := w.WriteLineData([]byte("test line"), 1, "source.log")
	if err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	// Flush to get output
	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// In plain serverless mode, output should be just the content with newline
	expected := "test line\n"
	if buf.String() != expected {
		t.Errorf("Expected %q, got %q", expected, buf.String())
	}

	// Check stats
	lines, bytesWritten := w.Stats()
	if lines != 1 {
		t.Errorf("Expected 1 line written, got %d", lines)
	}
	if bytesWritten == 0 {
		t.Error("Expected non-zero bytes written")
	}
}

func TestNewNetworkWriterRejectsNilContext(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("NewNetworkWriter accepted a nil context")
		}
	}()
	var nilContext context.Context
	NewNetworkWriter(nilContext, nil, nil, "testhost", false, false, 0, nil, handlerTestLogger)
}

// TestDirectWriter_ServerlessPlainWithNewline tests that existing newlines are preserved
func TestDirectWriter_ServerlessPlainWithNewline(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, true)

	// Write a line with trailing newline
	err := w.WriteLineData([]byte("test line\n"), 1, "source.log")
	if err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Should not add extra newline
	expected := "test line\n"
	if buf.String() != expected {
		t.Errorf("Expected %q, got %q", expected, buf.String())
	}
}

func TestDirectWriter_ServerlessColored(t *testing.T) {
	previousClient := config.Client
	config.Client = nil
	t.Cleanup(func() { config.Client = previousClient })

	theme := config.DefaultTermColors()
	theme.Remote.TextFg = color.FgMagenta
	theme.Remote.TextBg = color.BgWhite
	theme.Remote.TextAttr = color.AttrUnderline
	var output bytes.Buffer
	writer := NewDirectWriterWithColorizer(&output, "testhost", false, true, brush.New(theme))
	if err := writer.WriteLineData([]byte("payload"), 1, "source.log"); err != nil {
		t.Fatalf("WriteLineData() error = %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	wantSequence := string(color.FgMagenta) + string(color.BgWhite) +
		string(color.AttrUnderline) + "payload"
	if !strings.Contains(output.String(), wantSequence) {
		t.Fatalf("colored output = %q, want injected sequence %q", output.String(), wantSequence)
	}
}

func TestDirectWriterServerlessColoredPreservesTerminalMessageDelimiter(t *testing.T) {
	originalClient := config.Client
	config.Client = nil
	t.Cleanup(func() {
		config.Client = originalClient
	})

	content := append([]byte("payload"), protocol.MessageDelimiter)
	var output bytes.Buffer
	writer := NewDirectWriter(&output, "testhost", false, true)
	if err := writer.WriteLineData(content, 7, "source.log"); err != nil {
		t.Fatalf("WriteLineData() error = %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	if !bytes.Contains(output.Bytes(), content) {
		t.Fatalf("colored output %q does not preserve terminal payload byte in %q", output.Bytes(), content)
	}
}

// TestDirectWriter_NetworkPlain tests plain network mode output
func TestDirectWriter_NetworkPlain(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, false)

	err := w.WriteLineData([]byte("test line"), 1, "source.log")
	if err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// In plain network mode, output should be just the content with newline
	expected := "test line\n"
	if buf.String() != expected {
		t.Errorf("Expected %q, got %q", expected, buf.String())
	}
}

// TestDirectWriter_NetworkFormatted tests formatted network mode output
func TestDirectWriter_NetworkFormatted(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", false, false)

	err := w.WriteLineData([]byte("test line"), 99, "myfile.log")
	if err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	err = w.Flush()
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	output := buf.String()

	// In formatted network mode, output should have protocol structure
	if !strings.HasPrefix(output, "REMOTE") {
		t.Errorf("Expected output to start with REMOTE, got %q", output)
	}
	if !strings.Contains(output, "testhost") {
		t.Errorf("Expected output to contain hostname, got %q", output)
	}
	if !strings.Contains(output, "99") {
		t.Errorf("Expected output to contain line number 99, got %q", output)
	}
	if !strings.Contains(output, "myfile.log") {
		t.Errorf("Expected output to contain source ID, got %q", output)
	}
	// Should end with message delimiter
	if output[len(output)-1] != protocol.MessageDelimiter {
		t.Errorf("Expected output to end with message delimiter, got %q", output)
	}
}

// TestDirectWriter_WriteServerMessage tests server message writing
func TestDirectWriter_WriteServerMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", false, false)

	err := w.WriteServerMessage("Hello from server")
	if err != nil {
		t.Fatalf("WriteServerMessage failed: %v", err)
	}

	output := buf.String()

	if !strings.HasPrefix(output, "SERVER") {
		t.Errorf("Expected output to start with SERVER, got %q", output)
	}
	if !strings.Contains(output, "testhost") {
		t.Errorf("Expected output to contain hostname, got %q", output)
	}
	if !strings.Contains(output, "Hello from server") {
		t.Errorf("Expected output to contain message, got %q", output)
	}
}

// TestDirectWriter_WriteServerMessage_Serverless tests that server messages are skipped in serverless mode
func TestDirectWriter_WriteServerMessage_Serverless(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", false, true)

	err := w.WriteServerMessage("Hello from server")
	if err != nil {
		t.Fatalf("WriteServerMessage failed: %v", err)
	}

	// In serverless mode, server messages should be skipped
	if buf.Len() != 0 {
		t.Errorf("Expected no output in serverless mode, got %q", buf.String())
	}
}

func TestNetworkWriterSelectsServerMessageSinkAtConstruction(t *testing.T) {
	tests := []struct {
		name       string
		serverless bool
	}{
		{name: "network routes message"},
		{name: "serverless discards message", serverless: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			messages := make(chan string, 1)
			writer := NewNetworkWriter(context.Background(), nil, messages, "testhost",
				true, test.serverless, 7, nil, handlerTestLogger)

			if err := writer.WriteServerMessage("hello"); err != nil {
				t.Fatalf("WriteServerMessage: %v", err)
			}

			select {
			case encoded := <-messages:
				generation, got := decodeGeneratedMessage(encoded)
				if test.serverless {
					t.Fatalf("serverless writer routed message %q", got)
				}
				if generation != 7 || got != "hello" {
					t.Fatalf("routed message = (%d, %q), want (7, %q)", generation, got, "hello")
				}
			default:
				if !test.serverless {
					t.Fatal("network writer discarded server message")
				}
			}
		})
	}
}

func TestWritersUseInjectedLineFormatter(t *testing.T) {
	formatter := func(dst *bytes.Buffer, _ []byte, _ uint64, _ string) {
		dst.WriteString("injected")
	}

	t.Run("direct", func(t *testing.T) {
		var output bytes.Buffer
		writer := newDirectWriter(&output, formatter)
		if err := writer.WriteLineData([]byte("ignored"), 3, "direct.log"); err != nil {
			t.Fatalf("WriteLineData: %v", err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := output.String(); got != "injected" {
			t.Fatalf("output = %q, want %q", got, "injected")
		}
	})

	t.Run("network", func(t *testing.T) {
		output := make(chan []byte, 1)
		writer := newNetworkWriter(context.Background(), output, formatter, 0, nil, handlerTestLogger)
		if err := writer.WriteLineData([]byte("ignored"), 4, "network.log"); err != nil {
			t.Fatalf("WriteLineData: %v", err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if got := string(<-output); got != "injected" {
			t.Fatalf("output = %q, want %q", got, "injected")
		}
	})
}

// TestDirectWriter_WriteServerMessage_HiddenMessage tests hidden message handling
func TestDirectWriter_WriteServerMessage_HiddenMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", false, false)

	err := w.WriteServerMessage(".hidden")
	if err != nil {
		t.Fatalf("WriteServerMessage failed: %v", err)
	}

	output := buf.String()

	// Hidden messages (starting with .) should be written directly
	if !strings.HasPrefix(output, ".hidden") {
		t.Errorf("Expected output to start with .hidden, got %q", output)
	}
	// Should NOT have SERVER prefix
	if strings.Contains(output, "SERVER") {
		t.Errorf("Hidden message should not have SERVER prefix, got %q", output)
	}
}

// TestDirectWriter_MultipleLines tests writing multiple lines
func TestDirectWriter_MultipleLines(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, true)

	for i := uint64(1); i <= 5; i++ {
		err := w.WriteLineData([]byte("line content"), i, "source.log")
		if err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}

	err := w.Flush()
	if err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	lines, _ := w.Stats()
	if lines != 5 {
		t.Errorf("Expected 5 lines written, got %d", lines)
	}

	// Count actual lines in output
	outputLines := strings.Count(buf.String(), "\n")
	if outputLines != 5 {
		t.Errorf("Expected 5 lines in output, got %d", outputLines)
	}
}

type shortWriter struct {
	maxChunk int
	buf      bytes.Buffer
}

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := len(p)
	if w.maxChunk > 0 && n > w.maxChunk {
		n = w.maxChunk
	}
	w.buf.Write(p[:n])
	return n, nil
}

func TestDirectWriter_FlushHandlesShortWrites(t *testing.T) {
	writer := &shortWriter{maxChunk: 5}
	w := NewDirectWriter(writer, "testhost", true, true)

	if err := w.WriteLineData([]byte("abcdefghij"), 1, "source.log"); err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	if got, want := writer.buf.String(), "abcdefghij\n"; got != want {
		t.Fatalf("expected full output %q, got %q", want, got)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write(p []byte) (int, error) {
	return 0, nil
}

func TestDirectWriter_FlushFailsOnZeroProgress(t *testing.T) {
	w := NewDirectWriter(zeroWriter{}, "testhost", true, true)

	if err := w.WriteLineData([]byte("data"), 1, "source.log"); err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	if err := w.Flush(); err == nil {
		t.Fatal("expected Flush to fail on zero-progress writes")
	} else if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("expected io.ErrShortWrite, got %v", err)
	}
}

func TestNetworkWriterWriteLineDataStopsOnCancellation(t *testing.T) {

	outputLines := make(chan []byte, 1)
	outputLines <- []byte("occupied")

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	writer := NewNetworkWriter(ctx, outputLines, nil, "testhost", true, false,
		1, activeGeneration.Load, handlerTestLogger)
	writer.bufSize = 0

	done := make(chan error, 1)
	go func() {
		done <- writer.WriteLineData([]byte("stale line"), 1, "app.log")
	}()

	waitForNetworkWriterSending(t, writer)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteLineData returned unexpected error: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WriteLineData did not stop after cancellation")
	}
}

func TestNetworkWriterStopsBlockedSendAfterGenerationAdvance(t *testing.T) {

	outputLines := make(chan []byte, 1)
	outputLines <- []byte("occupied")

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)

	writer := NewNetworkWriter(context.Background(), outputLines, nil, "testhost", true, false,
		1, activeGeneration.Load, handlerTestLogger)
	writer.bufSize = 0

	done := make(chan error, 1)
	go func() {
		done <- writer.WriteLineData([]byte("stale line"), 1, "app.log")
	}()

	waitForNetworkWriterSending(t, writer)
	activeGeneration.Store(2)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WriteLineData returned unexpected error: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WriteLineData did not stop after generation advanced")
	}

	select {
	case got := <-outputLines:
		if string(got) != "occupied" {
			t.Fatalf("expected to drain occupied buffer first, got %q", string(got))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out draining occupied output channel entry")
	}

	select {
	case got := <-outputLines:
		t.Fatalf("unexpected stale output after generation advanced: %q", string(got))
	default:
	}
}

func waitForNetworkWriterSending(t *testing.T, writer *NetworkWriter) {
	t.Helper()

	deadline := time.After(250 * time.Millisecond)
	for {
		writer.mutex.Lock()
		got := writer.sending
		writer.mutex.Unlock()

		if got {
			return
		}

		select {
		case <-deadline:
			t.Fatal("timed out waiting for writer to start sending")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestNetworkWriterFlushWaitsForBufferedDataAndInFlightSend(t *testing.T) {

	outputLines := make(chan []byte, 1)
	outputLines <- []byte("occupied")

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	writer := NewNetworkWriter(ctx, outputLines, nil, "testhost", true, false,
		1, activeGeneration.Load, handlerTestLogger)
	writer.bufSize = 8

	if err := writer.WriteLineData([]byte("first"), 1, "app.log"); err != nil {
		t.Fatalf("first WriteLineData failed: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writer.WriteLineData([]byte("second"), 2, "app.log")
	}()

	waitForNetworkWriterSending(t, writer)

	if err := writer.WriteLineData([]byte("third"), 3, "app.log"); err != nil {
		t.Fatalf("third WriteLineData failed: %v", err)
	}

	flushDone := make(chan error, 1)
	go func() {
		flushDone <- writer.Flush()
	}()

	select {
	case err := <-flushDone:
		t.Fatalf("Flush returned early while buffered data was still blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	select {
	case got := <-outputLines:
		if string(got) != "occupied" {
			t.Fatalf("expected to drain occupied buffer first, got %q", string(got))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out draining occupied output channel entry")
	}

	select {
	case got := <-outputLines:
		generation, payload := decodeGeneratedBytes(got)
		if generation != 1 {
			t.Fatalf("expected generation 1, got %d", generation)
		}
		expected := []byte{'f', 'i', 'r', 's', 't', protocol.MessageDelimiter, 's', 'e', 'c', 'o', 'n', 'd', protocol.MessageDelimiter}
		if !bytes.Equal(payload, expected) {
			t.Fatalf("expected buffered send to contain first and second lines, got %q", string(payload))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for buffered send to reach output channel")
	}

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("WriteLineData returned unexpected error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second WriteLineData did not finish after the channel drained")
	}

	select {
	case got := <-outputLines:
		generation, payload := decodeGeneratedBytes(got)
		if generation != 1 {
			t.Fatalf("expected generation 1, got %d", generation)
		}
		expected := []byte{'t', 'h', 'i', 'r', 'd', protocol.MessageDelimiter}
		if !bytes.Equal(payload, expected) {
			t.Fatalf("expected buffered flush to send third line, got %q", string(payload))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for buffered Flush output")
	}

	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("Flush returned unexpected error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Flush did not return after buffered data was drained")
	}
}

func TestNetworkWriterStopsWaitingWhenContextIsCancelled(t *testing.T) {

	outputLines := make(chan []byte, 1)
	outputLines <- []byte("occupied")

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	writer := NewNetworkWriter(ctx, outputLines, nil, "testhost", true, false,
		1, activeGeneration.Load, handlerTestLogger)
	writer.bufSize = 0

	done := make(chan error, 1)
	go func() {
		done <- writer.WriteLineData([]byte("stale line"), 1, "app.log")
	}()

	waitForNetworkWriterSending(t, writer)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteLineData returned unexpected error: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WriteLineData did not stop after cancellation")
	}

	select {
	case got := <-outputLines:
		if string(got) != "occupied" {
			t.Fatalf("expected to drain occupied buffer first, got %q", string(got))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out draining occupied output channel entry")
	}

	select {
	case got := <-outputLines:
		t.Fatalf("unexpected stale output after cancellation: %q", string(got))
	default:
	}
}

func TestNetworkWriterFlushCancelsWhileWaitingOnInFlightSend(t *testing.T) {

	outputLines := make(chan []byte, 1)
	outputLines <- []byte("occupied")

	var activeGeneration atomic.Uint64
	activeGeneration.Store(1)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	writer := NewNetworkWriter(ctx, outputLines, nil, "testhost", true, false,
		1, activeGeneration.Load, handlerTestLogger)
	writer.bufSize = 8

	if err := writer.WriteLineData([]byte("first"), 1, "app.log"); err != nil {
		t.Fatalf("first WriteLineData failed: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- writer.WriteLineData([]byte("second"), 2, "app.log")
	}()

	waitForNetworkWriterSending(t, writer)

	flushDone := make(chan error, 1)
	flushStarted := make(chan struct{})
	go func() {
		close(flushStarted)
		flushDone <- writer.Flush()
	}()

	select {
	case <-flushStarted:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Flush goroutine did not start")
	}

	cancel()

	// On a cancelled Flush with an in-flight send, BOTH nil and
	// context.Canceled are correct outcomes — do not tighten this to a single
	// value or the test becomes flaky again.
	//
	//   - context.Canceled: waitForSendAvailability's select observed ctx.Done()
	//     before the in-flight send finished, so Flush propagates the cancel.
	//   - nil: the in-flight send goroutine finished first (its finishSending
	//     closes sendStateCh and clears "sending"), so waitForSendAvailability
	//     takes the closed channel, sees !sending, and Flush finds an empty
	//     writeBuf (the buffered "second" line was intentionally dropped when
	//     its send failed on the cancelled context) — there is genuinely
	//     nothing left to flush.
	//
	// Both leave the writer in a consistent state (asserted below: only the
	// pre-existing "occupied" entry remains, no stale output). The invariants
	// that actually matter are that Flush returns promptly after cancel (the
	// timeout guards against a hang/deadlock) and returns one of the two
	// acceptable values (guards against a panic or a bogus error).
	select {
	case err := <-flushDone:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Flush returned unexpected error: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Flush did not stop after cancellation")
	}

	select {
	case err := <-writeDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WriteLineData returned unexpected error: %v", err)
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatal("WriteLineData did not stop after cancellation")
	}

	select {
	case got := <-outputLines:
		if string(got) != "occupied" {
			t.Fatalf("expected to drain occupied buffer first, got %q", string(got))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out draining occupied output channel entry")
	}

	select {
	case got := <-outputLines:
		t.Fatalf("unexpected stale output after flush cancellation: %q", string(got))
	default:
	}
}

// TestDirectWriter_StatsBytesWrittenMatchesOutput verifies that the
// bytesWritten stat counts each line's formatted bytes exactly once even
// though writeBuf accumulates many lines before flushing. Regression test:
// the stat used to add the whole accumulated buffer length per line, so it
// grew quadratically with the number of buffered lines.
func TestDirectWriter_StatsBytesWrittenMatchesOutput(t *testing.T) {
	tests := []struct {
		name       string
		plain      bool
		serverless bool
	}{
		{name: "serverless plain", plain: true, serverless: true},
		{name: "network plain", plain: true, serverless: false},
		{name: "network formatted", plain: false, serverless: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewDirectWriter(&buf, "testhost", tt.plain, tt.serverless)

			// All lines stay buffered (well below the 64KB flush threshold)
			// until Flush, so bytesWritten must equal the flushed output size.
			const numLines = 100
			for i := uint64(1); i <= numLines; i++ {
				if err := w.WriteLineData([]byte("some log line content"), i, "source.log"); err != nil {
					t.Fatalf("WriteLineData failed on line %d: %v", i, err)
				}
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush failed: %v", err)
			}

			lines, bytesWritten := w.Stats()
			if lines != numLines {
				t.Errorf("Expected %d lines written, got %d", numLines, lines)
			}
			if want := uint64(buf.Len()); bytesWritten != want {
				t.Errorf("Expected bytesWritten %d (flushed output size), got %d", want, bytesWritten)
			}
		})
	}
}

// TestDirectWriter_StatsBytesWrittenServerlessColored covers the colored
// serverless branch of writeServerlessLine: each line is protocol formatted,
// run through the injected colorizer and appended to the accumulating writeBuf, so the
// bytesWritten stat must equal the flushed output size. A zero-value
// The compatibility colorizer is safe without ClientConfig setup.
func TestDirectWriter_StatsBytesWrittenServerlessColored(t *testing.T) {
	originalClient := config.Client
	config.Client = nil
	t.Cleanup(func() {
		config.Client = originalClient
	})

	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", false, true)

	// All lines stay buffered (well below the 64KB flush threshold) until
	// Flush, so bytesWritten must equal the flushed output size.
	const numLines = 100
	for i := uint64(1); i <= numLines; i++ {
		if err := w.WriteLineData([]byte("some log line content"), i, "source.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	lines, bytesWritten := w.Stats()
	if lines != numLines {
		t.Errorf("Expected %d lines written, got %d", numLines, lines)
	}
	if want := uint64(buf.Len()); bytesWritten != want {
		t.Errorf("Expected bytesWritten %d (flushed output size), got %d", want, bytesWritten)
	}
}

// TestDirectWriter_StatsBytesWrittenAcrossFlushThreshold verifies the
// bytesWritten stat stays exact when the buffer crosses the flush threshold
// several times during the write sequence.
func TestDirectWriter_StatsBytesWrittenAcrossFlushThreshold(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, true)
	w.bufSize = 32 // Force several intermediate flushes.

	const numLines = 20
	line := []byte("0123456789") // 11 bytes once the newline is appended.
	for i := uint64(1); i <= numLines; i++ {
		if err := w.WriteLineData(line, i, "source.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	lines, bytesWritten := w.Stats()
	if lines != numLines {
		t.Errorf("Expected %d lines written, got %d", numLines, lines)
	}
	want := uint64(numLines * (len(line) + 1))
	if bytesWritten != want {
		t.Errorf("Expected bytesWritten %d, got %d", want, bytesWritten)
	}
	if got := uint64(buf.Len()); got != want {
		t.Errorf("Expected flushed output size %d, got %d", want, got)
	}
}

// TestDirectWriter_StatsEmptyLine pins the empty-line edge semantics of
// plain mode: an empty line appends nothing (not even a newline), so it
// contributes zero bytes to the stat while still counting as a written line.
// This is a characterization test, not regression evidence for the delta fix.
func TestDirectWriter_StatsEmptyLine(t *testing.T) {
	var buf bytes.Buffer
	w := NewDirectWriter(&buf, "testhost", true, true)

	if err := w.WriteLineData([]byte{}, 1, "source.log"); err != nil {
		t.Fatalf("WriteLineData failed for empty line: %v", err)
	}
	if err := w.WriteLineData([]byte("a"), 2, "source.log"); err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	lines, bytesWritten := w.Stats()
	if lines != 2 {
		t.Errorf("Expected 2 lines written, got %d", lines)
	}
	// The empty line appends nothing in plain mode; only "a\n" is written.
	if want := uint64(len("a\n")); bytesWritten != want {
		t.Errorf("Expected bytesWritten %d, got %d", want, bytesWritten)
	}
	if got, want := buf.String(), "a\n"; got != want {
		t.Errorf("Expected output %q, got %q", want, got)
	}
}

// TestNetworkWriter_StatsBytesWrittenBelowThreshold verifies that the
// network writer's bytesWritten stat counts only per-line deltas while lines
// accumulate in writeBuf below the flush threshold, and that the stat matches
// the payload eventually flushed to the output channel.
func TestNetworkWriter_StatsBytesWrittenBelowThreshold(t *testing.T) {

	outputLines := make(chan []byte, 1)
	writer := NewNetworkWriter(context.Background(), outputLines, nil, "testhost",
		true, false, 0, nil, handlerTestLogger)

	const numLines = 10
	content := []byte("test line") // +1 byte for the message delimiter.
	for i := uint64(1); i <= numLines; i++ {
		if err := writer.WriteLineData(content, i, "app.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}

	want := uint64(numLines * (len(content) + 1))
	lines, bytesWritten := writer.Stats()
	if lines != numLines {
		t.Errorf("Expected %d lines written, got %d", numLines, lines)
	}
	if bytesWritten != want {
		t.Errorf("Expected bytesWritten %d, got %d", want, bytesWritten)
	}

	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	select {
	case data := <-outputLines:
		// Generation is 0, so the payload is the raw buffered bytes.
		if got := uint64(len(data)); got != want {
			t.Errorf("Expected flushed payload size %d, got %d", want, got)
		}
	default:
		t.Fatal("Expected flushed payload in output channel, got none")
	}
}

// TestNetworkWriter_StatsBytesWrittenAcrossFlushThreshold verifies that
// the network writer's bytesWritten stat stays exact when writes repeatedly
// cross the flush threshold and trigger channel sends. bufSize is chosen so
// three lines accumulate per flush cycle: the old cumulative-length stat
// would count 11+22+33 bytes per cycle instead of the correct 33, so this
// test fails against the pre-fix code.
func TestNetworkWriter_StatsBytesWrittenAcrossFlushThreshold(t *testing.T) {

	const numLines = 20
	outputLines := make(chan []byte, numLines)
	writer := NewNetworkWriter(context.Background(), outputLines, nil, "testhost",
		true, false, 0, nil, handlerTestLogger)
	writer.bufSize = 32 // Three 11-byte lines accumulate before each send.

	content := []byte("0123456789") // 11 bytes with the message delimiter.
	for i := uint64(1); i <= numLines; i++ {
		if err := writer.WriteLineData(content, i, "app.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	var total uint64
drain:
	for {
		select {
		case data := <-outputLines:
			total += uint64(len(data))
		default:
			break drain
		}
	}

	want := uint64(numLines * (len(content) + 1))
	lines, bytesWritten := writer.Stats()
	if lines != numLines {
		t.Errorf("Expected %d lines written, got %d", numLines, lines)
	}
	if bytesWritten != want {
		t.Errorf("Expected bytesWritten %d, got %d", want, bytesWritten)
	}
	if total != want {
		t.Errorf("Expected total channel payload size %d, got %d", want, total)
	}
}

// TestNewNetworkWriter_SetsBufSize is the core regression test for the
// dead-batching bug: the production constructor must set bufSize so lines
// coalesce instead of being sent one payload per line. Before the fix,
// makeWriter built the writer with a bare struct literal that left bufSize
// at zero, so this assertion would fail.
func TestNewNetworkWriter_SetsBufSize(t *testing.T) {
	ch := make(chan []byte, 1)
	msgCh := make(chan string, 1)
	w := NewNetworkWriter(context.Background(), ch, msgCh, "testhost",
		true, false, 0, nil, handlerTestLogger)

	if w.bufSize != 64*1024 {
		t.Fatalf("expected bufSize 64KB, got %d", w.bufSize)
	}
}

// TestNetworkWriter_BatchesSmallLines proves the batching mechanism works
// end to end through the real constructor: many small lines below the 64KB
// threshold must be coalesced into a single output-channel payload on Flush,
// NOT emitted as one payload per line. This is the direct regression assertion
// for the ~5.7x server-mode speedup — against the pre-fix code (bufSize == 0)
// each WriteLineData would have produced its own payload.
func TestNetworkWriter_BatchesSmallLines(t *testing.T) {

	// Buffered enough that each line COULD land as its own payload if batching
	// were broken; the test asserts it does not.
	const numLines = 100
	outputLines := make(chan []byte, numLines)
	w := NewNetworkWriter(context.Background(), outputLines, nil, "testhost",
		true, false, 0, nil, handlerTestLogger)

	content := []byte("small log line") // +1 byte for the message delimiter.
	for i := uint64(1); i <= numLines; i++ {
		if err := w.WriteLineData(content, i, "app.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}

	// Nothing should have been sent yet: all lines fit under the 64KB buffer.
	select {
	case data := <-outputLines:
		t.Fatalf("expected no premature send below threshold, got payload %q", string(data))
	default:
	}

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Collect every payload and assert the lines were batched into very few
	// sends (one here), not one-per-line.
	var payloads [][]byte
	var total uint64
drain:
	for {
		select {
		case data := <-outputLines:
			payloads = append(payloads, data)
			_, decoded := decodeGeneratedBytes(data)
			total += uint64(len(decoded))
		default:
			break drain
		}
	}

	if len(payloads) >= numLines {
		t.Fatalf("expected batched sends (few payloads), got %d payloads for %d lines", len(payloads), numLines)
	}
	if len(payloads) != 1 {
		t.Fatalf("expected exactly 1 batched payload below threshold, got %d", len(payloads))
	}
	want := uint64(numLines * (len(content) + 1))
	if total != want {
		t.Fatalf("expected %d total batched bytes, got %d", want, total)
	}
}

// TestNetworkWriter_FlushEmitsPartialBufferPromptly models the follow-mode
// (dtail tail) boundary: a small number of lines well below the flush threshold
// must be emitted promptly when Flush is called (as tailWithProcessorOptimized
// does after every read chunk), not held back waiting for the buffer to fill.
func TestNetworkWriter_FlushEmitsPartialBufferPromptly(t *testing.T) {

	outputLines := make(chan []byte, 4)
	w := NewNetworkWriter(context.Background(), outputLines, nil, "testhost",
		true, false, 0, nil, handlerTestLogger)

	if err := w.WriteLineData([]byte("tail line"), 1, "app.log"); err != nil {
		t.Fatalf("WriteLineData failed: %v", err)
	}

	// Partial buffer must not be on the wire until Flush.
	select {
	case data := <-outputLines:
		t.Fatalf("partial buffer sent before Flush: %q", string(data))
	default:
	}

	if err := w.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	select {
	case data := <-outputLines:
		_, payload := decodeGeneratedBytes(data)
		expected := append([]byte("tail line"), protocol.MessageDelimiter)
		if !bytes.Equal(payload, expected) {
			t.Fatalf("expected flushed partial buffer %q, got %q", string(expected), string(payload))
		}
	default:
		t.Fatal("Flush did not emit the partial buffer to the output channel")
	}
}

// TestNetworkWriter_LineCrossingThresholdFlushes is the boundary case: once
// accumulated lines cross bufSize, WriteLineData sends the batch immediately
// without needing an explicit Flush.
func TestNetworkWriter_LineCrossingThresholdFlushes(t *testing.T) {

	outputLines := make(chan []byte, 8)
	w := NewNetworkWriter(context.Background(), outputLines, nil, "testhost",
		true, false, 0, nil, handlerTestLogger)
	w.bufSize = 32 // Small threshold so a few 11-byte lines cross it.

	content := []byte("0123456789") // 11 bytes with the message delimiter.
	// Write three lines: 11, 22, 33 bytes accumulated. The third crosses 32
	// and must trigger an automatic send.
	for i := uint64(1); i <= 3; i++ {
		if err := w.WriteLineData(content, i, "app.log"); err != nil {
			t.Fatalf("WriteLineData failed on line %d: %v", i, err)
		}
	}

	select {
	case data := <-outputLines:
		_, payload := decodeGeneratedBytes(data)
		if want := uint64(3 * (len(content) + 1)); uint64(len(payload)) != want {
			t.Fatalf("expected threshold-crossing send of %d bytes, got %d", want, len(payload))
		}
	default:
		t.Fatal("expected automatic send after crossing bufSize threshold, got none")
	}
}

// TestDirectLineProcessor tests the line processor wrapper.
func TestDirectLineProcessor(t *testing.T) {
	var output bytes.Buffer
	writer := NewDirectWriter(&output, "testhost", true, true)
	processor := NewDirectLineProcessor(writer, "glob", handlerTestLogger)
	lineContent := bytes.NewBufferString("payload")
	if err := processor.ProcessLine(lineContent, 1, "source"); err != nil {
		t.Fatalf("ProcessLine: %v", err)
	}
	if err := processor.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got, want := output.String(), "payload\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

// TestDirectLineProcessor_Close tests the close method.
func TestDirectLineProcessor_Close(t *testing.T) {
	var output bytes.Buffer
	writer := NewDirectWriter(&output, "testhost", true, true)
	processor := NewDirectLineProcessor(writer, "glob", handlerTestLogger)
	lineContent := bytes.NewBufferString("payload")
	if err := processor.ProcessLine(lineContent, 1, "source"); err != nil {
		t.Fatalf("ProcessLine: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := output.String(), "payload\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
