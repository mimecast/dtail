package dlog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/dlog/loggers"
)

// recordingLogger records whether a message arrived via the diagnostic (Log)
// path or the payload (Raw) path, so the test can assert how RawLog vs Raw route
// their messages. It reports SupportsColors()=false so the callers take their
// non-color branch (logger.Log / logger.Raw), which is what the routing test
// needs to observe.
type recordingLogger struct {
	mutex sync.Mutex
	logs  []string
	raws  []string
}

func (r *recordingLogger) Log(message string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.logs = append(r.logs, message)
}
func (r *recordingLogger) LogWithColors(message, colored string) { r.Log(message) }
func (r *recordingLogger) Raw(message string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.raws = append(r.raws, message)
}
func (r *recordingLogger) RawWithColors(message, colored string) { r.Raw(message) }
func (r *recordingLogger) Flush()                                {}
func (r *recordingLogger) SupportsColors() bool                  { return false }

var _ loggers.Logger = (*recordingLogger)(nil)

func TestDLogAcceptsLoggerWithoutOptionalLifecycleCapabilities(t *testing.T) {
	d := &DLog{logger: &recordingLogger{}}
	d.Pause()
	d.Resume()

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go d.start(ctx, &wg)
	cancel()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DLog with a core-only logger did not stop after cancellation")
	}
}

// TestRawLogUsesDiagnosticSink is the regression guard for the ReportServerError
// footgun: a server-error audit line must go through the diagnostic (Log) sink,
// not the payload (Raw) sink. Only the Log sink is written to the client log file
// by default (Client.LogPayload=false gates the Raw/payload sink out of the file),
// so a server error routed via Raw would silently vanish from the on-disk audit
// trail. This asserts RawLog -> Log and, for contrast, Raw -> Raw.
func TestRawLogUsesDiagnosticSink(t *testing.T) {
	rec := &recordingLogger{}
	d := &DLog{logger: rec}

	const serverError = "SERVER|srv1|ERROR|journal file targets require server capability journal-v1"
	d.RawLog(serverError)

	if len(rec.logs) != 1 || rec.logs[0] != serverError {
		t.Fatalf("RawLog must reach the diagnostic (Log) sink verbatim; got logs=%v raws=%v",
			rec.logs, rec.raws)
	}
	if len(rec.raws) != 0 {
		t.Fatalf("RawLog must NOT use the payload (Raw) sink (it would be gated out of the file); got raws=%v",
			rec.raws)
	}

	// Contrast: bulk payload still goes through the Raw/payload sink.
	d.Raw("payload-line\n")
	if len(rec.raws) != 1 || rec.raws[0] != "payload-line\n" {
		t.Fatalf("Raw must reach the payload (Raw) sink; got raws=%v", rec.raws)
	}
}

// bytesRecordingLogger is a recordingLogger with the byte-slice payload
// capability. It can optionally claim color support.
type bytesRecordingLogger struct {
	recordingLogger
	rawBytes      []string
	coloredRaws   []string
	supportsColor bool
}

func (r *bytesRecordingLogger) RawBytes(message []byte) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.rawBytes = append(r.rawBytes, string(message))
}

func (r *bytesRecordingLogger) RawWithColors(message, colored string) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.coloredRaws = append(r.coloredRaws, colored)
}

func (r *bytesRecordingLogger) SupportsColors() bool { return r.supportsColor }

var _ loggers.RawBytesWriter = (*bytesRecordingLogger)(nil)

type bracketColorizer struct{}

func (bracketColorizer) Colorfy(message string) string { return "[" + message + "]" }

// TestDLogRawBytesRouting checks the three payload routes of RawBytes: the byte
// path of a capable sink, the string Raw fallback, and colorizing, which needs
// a string and therefore bypasses the byte path.
func TestDLogRawBytesRouting(t *testing.T) {
	t.Run("byte sink", func(t *testing.T) {
		sink := &bytesRecordingLogger{}
		(&DLog{logger: sink}).RawBytes([]byte("line\n"))
		if len(sink.rawBytes) != 1 || sink.rawBytes[0] != "line\n" || len(sink.raws) != 0 {
			t.Fatalf("rawBytes=%q raws=%q, want one byte-path write", sink.rawBytes, sink.raws)
		}
	})

	t.Run("string fallback", func(t *testing.T) {
		sink := &recordingLogger{}
		(&DLog{logger: sink}).RawBytes([]byte("line\n"))
		if len(sink.raws) != 1 || sink.raws[0] != "line\n" {
			t.Fatalf("raws=%q, want the payload through Raw", sink.raws)
		}
	})

	t.Run("colors", func(t *testing.T) {
		sink := &bytesRecordingLogger{supportsColor: true}
		d := &DLog{logger: sink, colorizer: bracketColorizer{}, colorsEnabled: true}
		d.RawBytes([]byte("line"))
		if len(sink.coloredRaws) != 1 || sink.coloredRaws[0] != "[line]" || len(sink.rawBytes) != 0 {
			t.Fatalf("coloredRaws=%q rawBytes=%q, want one colorized write", sink.coloredRaws, sink.rawBytes)
		}
	})
}
