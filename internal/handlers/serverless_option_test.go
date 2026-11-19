package handlers

// Regression tests for a remote session claiming serverless mode. Serverless
// mode redirects the session payload to the process' own output instead of
// the session transport; it is a property of the runtime that built the
// handler (Dependencies.ServerlessOutput), not of the session. dserver used
// to honour the client-supplied "serverless=true" option, which let any
// authenticated remote client divert its payload into the dserver process
// stdout (its own log with --logger stdout) and starved the SSH channel. It
// also opened the serverless-only stdin path (readPipe), which no readfiles
// permission covers.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

const serverlessOptionTestLine = "serverless-option-regression-payload"

// TestRemoteSessionCannotDivertPayloadToProcessStdout drives a real
// ServerHandler built the way dserver builds it (no ServerlessOutput) with a
// cat command carrying the remote option serverless=true. The payload must
// reach the session transport, and the process stdout must stay untouched.
func TestRemoteSessionCannotDivertPayloadToProcessStdout(t *testing.T) {
	handler := newMapTestHandler(t)
	if handler.serverless {
		t.Fatal("handler without a runtime output writer started in serverless mode")
	}
	stdout := captureProcessStdout(t)
	path := writeServerlessOptionTestFile(t)

	output := runCatWithServerlessOption(t, handler, path)

	if captured := stdout.contents(t); strings.Contains(captured, serverlessOptionTestLine) {
		t.Fatalf("remote serverless option diverted payload to process stdout: %q", captured)
	}
	if !strings.Contains(output, serverlessOptionTestLine) {
		t.Fatalf("payload did not reach the session transport: %q", output)
	}
}

// TestServerlessRuntimeWritesPayloadToItsOutput is the positive counterpart:
// the in-process runtime supplies an output writer, so the very same command
// stream must deliver the payload there instead of the transport.
func TestServerlessRuntimeWritesPayloadToItsOutput(t *testing.T) {
	serverlessOutput := &testOutput{}
	handler := newMapTestHandlerWithOutput(t, handlerTestLogger, serverlessOutput)
	if !handler.serverless {
		t.Fatal("handler with a runtime output writer did not start in serverless mode")
	}
	path := writeServerlessOptionTestFile(t)

	output := runCatWithServerlessOption(t, handler, path)

	if got := serverlessOutput.String(); !strings.Contains(got, serverlessOptionTestLine) {
		t.Fatalf("payload did not reach the runtime output writer: %q", got)
	}
	if strings.Contains(output, serverlessOptionTestLine) {
		t.Fatalf("serverless session also sent its payload to the transport: %q", output)
	}
}

// TestRemoteSessionCannotReadProcessStdin covers the permission side of the
// same option. A serverless read of "-" takes readPipe, which reads the
// process' own stdin and never calls PrepareReadTarget, so it is subject to
// no readfiles permission at all. A remote session must therefore not be able
// to reach it: dserver's own stdin must stay unread.
func TestRemoteSessionCannotReadProcessStdin(t *testing.T) {
	handler := newMapTestHandler(t)
	// "-" matches no file, and the read gives up after one retry interval.
	handler.readTimings.globRetryInterval = time.Millisecond
	stdin := captureProcessStdin(t, serverlessOptionTestLine+"\n")
	stdout := captureProcessStdout(t)

	output := runCatWithServerlessOption(t, handler, "-")

	if remaining := stdin.remaining(t); remaining != serverlessOptionTestLine+"\n" {
		t.Fatalf("remote serverless option let the session read the process stdin, left %q", remaining)
	}
	if captured := stdout.contents(t); strings.Contains(captured, serverlessOptionTestLine) {
		t.Fatalf("process stdin content reached the process stdout: %q", captured)
	}
	if strings.Contains(output, serverlessOptionTestLine) {
		t.Fatalf("process stdin content reached the session transport: %q", output)
	}
}

// runCatWithServerlessOption runs one cat command whose options contain
// serverless=true, exactly as a client of a serverless session serializes
// them, and returns everything the handler sent to its transport.
func runCatWithServerlessOption(t *testing.T, handler *ServerHandler, path string) string {
	t.Helper()
	spec := session.Spec{
		Mode:    omode.CatClient,
		Files:   []string{path},
		Options: "plain=true:serverless=true",
		Regex:   ".",
	}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}

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

	return output.String()
}

func writeServerlessOptionTestFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.log")
	if err := os.WriteFile(path, []byte(serverlessOptionTestLine+"\n"), 0o644); err != nil {
		t.Fatalf("write payload file: %v", err)
	}
	return path
}

// stdoutCapture replaces the process stdout for the duration of one test, so
// a payload write that escapes the session transport becomes observable.
type stdoutCapture struct {
	file  *os.File
	saved *os.File
}

// captureProcessStdout redirects os.Stdout into a temporary file. Tests in
// this package never run in parallel, so the swap cannot race with another
// test's output.
func captureProcessStdout(t *testing.T) *stdoutCapture {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatalf("create stdout capture file: %v", err)
	}
	capture := &stdoutCapture{file: file, saved: os.Stdout}
	os.Stdout = file
	t.Cleanup(func() {
		os.Stdout = capture.saved
		if err := file.Close(); err != nil {
			t.Errorf("close stdout capture file: %v", err)
		}
	})
	return capture
}

func (c *stdoutCapture) contents(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(c.file.Name())
	if err != nil {
		t.Fatalf("read stdout capture file: %v", err)
	}
	return string(content)
}

// stdinCapture replaces the process stdin with a pipe holding content, so a
// read of the process' own stdin becomes observable: whatever the session
// consumed is missing from what the test reads back.
type stdinCapture struct {
	reader *os.File
	saved  *os.File
}

// captureProcessStdin puts content into a pipe and makes it the process
// stdin. The write end is closed right away, so a reader of it sees EOF after
// content instead of blocking. Tests in this package never run in parallel,
// so the swap cannot race with another test.
func captureProcessStdin(t *testing.T, content string) *stdinCapture {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdin pipe: %v", err)
	}
	if _, err := writer.WriteString(content); err != nil {
		t.Fatalf("fill stdin pipe: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close stdin pipe writer: %v", err)
	}
	capture := &stdinCapture{reader: reader, saved: os.Stdin}
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = capture.saved
		if err := reader.Close(); err != nil {
			t.Errorf("close stdin pipe reader: %v", err)
		}
	})
	return capture
}

// remaining returns what is still in the stdin pipe.
func (c *stdinCapture) remaining(t *testing.T) string {
	t.Helper()
	content, err := io.ReadAll(c.reader)
	if err != nil {
		t.Fatalf("read remaining stdin: %v", err)
	}
	return string(content)
}
