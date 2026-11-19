package loggers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// recordingSink is an injectable Logger that records which messages reached it
// via the diagnostic path (Log) versus the payload path (Raw). It lets the fout
// routing tests assert exactly which sink gets diagnostics and which gets
// payload without touching real files or stdout.
type recordingSink struct {
	mutex sync.Mutex
	logs  []string
	raws  []string
}

func (r *recordingSink) Log(message string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.logs = append(r.logs, message)
}

func (r *recordingSink) LogWithColors(message, colored string) {
	r.Log(message)
}

func (r *recordingSink) Raw(message string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.raws = append(r.raws, message)
}

func (r *recordingSink) RawWithColors(message, colored string) {
	r.Raw(message)
}

func (r *recordingSink) Flush()               {}
func (r *recordingSink) SupportsColors() bool { return false }

func (r *recordingSink) logCount() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.logs)
}

func (r *recordingSink) rawCount() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return len(r.raws)
}

var _ Logger = (*recordingSink)(nil)

func TestFoutAcceptsSinksWithOnlyCoreLoggerMethods(t *testing.T) {
	f := newFoutWithSinks(&recordingSink{}, &recordingSink{}, false)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	f.Start(ctx, &wg)
	f.Pause()
	f.Resume()
	f.Rotate()
	cancel()
	wg.Wait()
}

// TestFoutDefaultKeepsPayloadOutOfFile proves the footgun fix: with LogPayload
// disabled (the default), diagnostics reach the file sink but retrieved payload
// does not — while stdout still receives BOTH, so the terminal output is
// unchanged.
func TestFoutDefaultKeepsPayloadOutOfFile(t *testing.T) {
	file := &recordingSink{}
	stdout := &recordingSink{}
	f := newFoutWithSinks(file, stdout, false)

	f.Log("diagnostic-line")  // connection INFO/WARN/ERROR audit line
	f.Raw("payload-line-1\n") // bulk dcat/dgrep/dtail output
	f.Raw("payload-line-2\n")

	// File: exactly the diagnostic, no payload.
	if got := file.logCount(); got != 1 {
		t.Fatalf("file diagnostics: got %d, want 1", got)
	}
	if got := file.rawCount(); got != 0 {
		t.Fatalf("file payload leaked: got %d raws, want 0 (default must be diagnostics-only)", got)
	}
	// Stdout: diagnostic + both payload lines (terminal output unchanged).
	if got := stdout.logCount(); got != 1 {
		t.Fatalf("stdout diagnostics: got %d, want 1", got)
	}
	if got := stdout.rawCount(); got != 2 {
		t.Fatalf("stdout payload: got %d, want 2", got)
	}
}

// TestFoutOptInTeesPayloadToFile proves --log-payload / Client.LogPayload
// restores the legacy behaviour: payload is teed to the file too, and
// diagnostics still land in the file.
func TestFoutOptInTeesPayloadToFile(t *testing.T) {
	file := &recordingSink{}
	stdout := &recordingSink{}
	f := newFoutWithSinks(file, stdout, true)

	f.Log("diagnostic-line")
	f.Raw("payload-line-1\n")
	f.Raw("payload-line-2\n")

	if got := file.logCount(); got != 1 {
		t.Fatalf("file diagnostics: got %d, want 1", got)
	}
	if got := file.rawCount(); got != 2 {
		t.Fatalf("file payload with opt-in: got %d, want 2", got)
	}
	if got := stdout.rawCount(); got != 2 {
		t.Fatalf("stdout payload: got %d, want 2", got)
	}
}

// TestFoutServerErrorDiagnosticReachesFileByDefault is the end-to-end guard for
// the ReportServerError footgun: a server-error audit line, sent via the
// diagnostic (Log) path over a REAL on-disk file sink with LogPayload=false,
// must actually land in the daily log file — while bulk payload (Raw) must not.
// This exercises the real file-write path the regression would have skipped.
func TestFoutServerErrorDiagnosticReachesFileByDefault(t *testing.T) {
	tmp := t.TempDir()

	// SignalRotation with a fixed FileBase gives a deterministic file name.
	fileSink := newFile(Strategy{Rotation: SignalRotation, FileBase: "servererr"}, tmp)
	stdout := &recordingSink{}
	f := newFoutWithSinks(fileSink, stdout, false)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	f.Start(ctx, &wg)

	serverError := "SERVER|srv1|ERROR|journal file targets require server capability journal-v1"
	f.Log(serverError) // diagnostic / audit line (ReportServerError path)
	f.Raw("payload\n") // bulk payload, must stay out of the file

	f.Flush()
	cancel()
	wg.Wait()

	content, err := os.ReadFile(filepath.Join(tmp, "servererr.log"))
	if err != nil {
		t.Fatalf("reading log file: %v", err)
	}
	if !strings.Contains(string(content), serverError) {
		t.Fatalf("server-error diagnostic missing from default log file:\n%s", content)
	}
	if strings.Contains(string(content), "payload") {
		t.Fatalf("payload leaked into the default log file:\n%s", content)
	}
	// Stdout still receives both (terminal output unchanged).
	if stdout.logCount() != 1 || stdout.rawCount() != 1 {
		t.Fatalf("stdout must receive both diagnostic and payload; logs=%d raws=%d",
			stdout.logCount(), stdout.rawCount())
	}
}

// TestFoutWithColorsRouting mirrors the two tests above for the colored paths
// (LogWithColors always to file, RawWithColors gated by the opt-in), because the
// client uses the *WithColors variants when terminal colors are enabled.
func TestFoutWithColorsRouting(t *testing.T) {

	t.Run("default", func(t *testing.T) {
		file := &recordingSink{}
		stdout := &recordingSink{}
		f := newFoutWithSinks(file, stdout, false)
		f.LogWithColors("diag", "\x1b[1mdiag\x1b[0m")
		f.RawWithColors("payload\n", "\x1b[1mpayload\x1b[0m\n")
		if got := file.logCount(); got != 1 {
			t.Fatalf("file diagnostics: got %d, want 1", got)
		}
		if got := file.rawCount(); got != 0 {
			t.Fatalf("file payload leaked: got %d, want 0", got)
		}
		if got := stdout.rawCount(); got != 1 {
			t.Fatalf("stdout payload: got %d, want 1", got)
		}
	})

	t.Run("optin", func(t *testing.T) {
		file := &recordingSink{}
		stdout := &recordingSink{}
		f := newFoutWithSinks(file, stdout, true)
		f.RawWithColors("payload\n", "\x1b[1mpayload\x1b[0m\n")
		if got := file.rawCount(); got != 1 {
			t.Fatalf("file payload with opt-in: got %d, want 1", got)
		}
	})
}

// TestFoutRawFileOnlyHonorsPayloadOptIn covers the serverless tee hook: it
// never writes to stdout (the caller already did), and it reaches the file only
// when payload logging was opted in.
func TestFoutRawFileOnlyHonorsPayloadOptIn(t *testing.T) {
	tests := []struct {
		name         string
		logPayload   bool
		wantFileRaws int
	}{
		{name: "default keeps payload out of file", logPayload: false, wantFileRaws: 0},
		{name: "opt-in tees payload to file", logPayload: true, wantFileRaws: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := &recordingSink{}
			stdout := &recordingSink{}
			f := newFoutWithSinks(file, stdout, tt.logPayload)

			f.RawFileOnly("payload\n")

			if got := file.rawCount(); got != tt.wantFileRaws {
				t.Fatalf("file payload: got %d, want %d", got, tt.wantFileRaws)
			}
			if got := stdout.rawCount() + stdout.logCount(); got != 0 {
				t.Fatalf("RawFileOnly wrote %d messages to stdout, want 0", got)
			}
			if got := file.logCount(); got != 0 {
				t.Fatalf("RawFileOnly wrote %d diagnostics to file, want 0", got)
			}
		})
	}
}

// bytesRecordingSink is a recordingSink that also has the byte-slice payload
// capability, recording those writes separately from string Raw calls.
type bytesRecordingSink struct {
	recordingSink
	rawBytes []string
}

func (r *bytesRecordingSink) RawBytes(message []byte) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.rawBytes = append(r.rawBytes, string(message))
}

var _ RawBytesWriter = (*bytesRecordingSink)(nil)

// TestFoutRawBytesHonorsPayloadGate checks that RawBytes follows the same
// --log-payload gate as Raw, uses the byte path of a sink that has one, and
// falls back to Raw for a sink that does not.
func TestFoutRawBytesHonorsPayloadGate(t *testing.T) {
	for _, logPayload := range []bool{false, true} {
		t.Run(fmt.Sprintf("logPayload=%v", logPayload), func(t *testing.T) {
			file := &recordingSink{}
			stdout := &bytesRecordingSink{}
			f := newFoutWithSinks(file, stdout, logPayload)

			f.RawBytes([]byte("payload\n"))

			if len(stdout.rawBytes) != 1 || stdout.rawBytes[0] != "payload\n" || stdout.rawCount() != 0 {
				t.Fatalf("stdout got rawBytes=%q raws=%q, want one byte-path write", stdout.rawBytes, stdout.raws)
			}
			if !logPayload {
				if file.rawCount() != 0 {
					t.Fatalf("file received payload without --log-payload: %q", file.raws)
				}
				return
			}
			if file.rawCount() != 1 || file.raws[0] != "payload\n" {
				t.Fatalf("file raws = %q, want the payload through the Raw fallback", file.raws)
			}
		})
	}
}
