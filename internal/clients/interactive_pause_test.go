package clients

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/term"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

// pausingTestLogger is a client logger whose payload writes block while it
// is paused, as those of the stdout logger do (loggers.stdout.lockUnpaused).
// Pauses are depth counted like there.
type pausingTestLogger struct {
	clientlog.NopLogger
	mu      sync.Mutex
	cond    *sync.Cond
	depth   int
	written []string
	// onRaw, if set, is called after every payload write, outside the lock.
	onRaw func()
}

func newPausingTestLogger() *pausingTestLogger {
	l := &pausingTestLogger{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

func (l *pausingTestLogger) Pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.depth++
}

func (l *pausingTestLogger) Resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.depth > 0 {
		l.depth--
	}
	l.cond.Broadcast()
}

func (l *pausingTestLogger) Raw(message string) string {
	l.mu.Lock()
	for l.depth > 0 {
		l.cond.Wait()
	}
	l.written = append(l.written, message)
	onRaw := l.onRaw
	l.mu.Unlock()
	if onRaw != nil {
		onRaw()
	}
	return message
}

// log records a log line, blocking while paused like Raw, but without
// calling onRaw: it is no payload.
func (l *pausingTestLogger) log(message string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.depth > 0 {
		l.cond.Wait()
	}
	l.written = append(l.written, message)
	return message
}

// RawBytes is Raw for the client handlers' payload writes
// (clientlog.RawBytes), which would otherwise reach the embedded NopLogger.
func (l *pausingTestLogger) RawBytes(message []byte) {
	l.Raw(string(message))
}

// Mapreduce records a stats table, blocking while paused like Raw: the
// stdout logger writes stats through the same paused sink.
func (l *pausingTestLogger) Mapreduce(table string, _ map[string]any) string {
	return l.log(table)
}

func (l *pausingTestLogger) writtenLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.written...)
}

func (l *pausingTestLogger) pauseDepth() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.depth
}

// startWrite writes through write in a goroutine and returns a channel
// closed once the write returned.
func startWrite(write func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		write()
	}()
	return done
}

func requireBlocked(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("write went through while the output was paused")
	case <-time.After(50 * time.Millisecond):
	}
}

func requireDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write still blocked after the output was resumed")
	}
}

// TestOutputGateBlocksWritesWhilePaused checks that the gate of the
// serverless direct output holds a write back while paused and lets it
// through, unchanged, on resume.
func TestOutputGateBlocksWritesWhilePaused(t *testing.T) {
	gate := newOutputGate()
	var out bytes.Buffer
	writer := gate.writer(&out)

	gate.Pause()
	done := startWrite(func() { _, _ = writer.Write([]byte("line\n")) })
	requireBlocked(t, done)
	gate.Resume()
	requireDone(t, done)
	if out.String() != "line\n" {
		t.Fatalf("output = %q, want %q", out.String(), "line\n")
	}

	// Not paused (also after a resume without a pause), writes go through.
	gate.Resume()
	if _, err := writer.Write([]byte("more\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out.String() != "line\nmore\n" {
		t.Fatalf("output = %q", out.String())
	}
}

// TestNilOutputGateNeverBlocks covers a runtime without a gate (e.g. one
// built as a struct literal in tests): the writer is passed through as is and
// pausing does nothing.
func TestNilOutputGateNeverBlocks(t *testing.T) {
	var gate *outputGate
	var out bytes.Buffer
	if got := gate.writer(&out); got != &out {
		t.Fatalf("nil gate wrapped the writer: %T", got)
	}
	gate.Pause()
	gate.Resume()
	gate.wait()
}

func newPauseTestClient(logger *pausingTestLogger) *baseClient {
	args := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	return &baseClient{
		mu:          newBaseClientMu(),
		Args:        args,
		sessionSpec: SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"},
		loggers:     LoggerDependencies{Client: logger},
		runtime:     &clientRuntimeBoundary{outputGate: newOutputGate()},
	}
}

// TestHandleInteractiveLineTogglesPause checks that an empty line pauses the
// client logger and the serverless output gate and prints the help, that
// :show and a rejected command keep the output paused, and that the next
// empty line resumes the output and lets the blocked writes through.
func TestHandleInteractiveLineTogglesPause(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var tty bytes.Buffer

	if quit, err := client.handleInteractiveLine(&tty, "", pause); quit || err != nil {
		t.Fatalf("empty line: quit=%v err=%v", quit, err)
	}
	wantPaused := interactivePausedMessage + "\n" + interactiveHelpText(client.Args) + "\n"
	if tty.String() != wantPaused {
		t.Fatalf("tty = %q, want %q", tty.String(), wantPaused)
	}
	if logger.pauseDepth() != 1 {
		t.Fatalf("logger pause depth = %d, want 1", logger.pauseDepth())
	}
	payload := startWrite(func() { logger.Raw("payload") })
	var direct bytes.Buffer
	directWriter := client.runtime.outputGate.writer(&direct)
	serverless := startWrite(func() { _, _ = directWriter.Write([]byte("direct")) })
	requireBlocked(t, payload)
	requireBlocked(t, serverless)

	for _, line := range []string{":show", ":bogus"} {
		tty.Reset()
		if quit, err := client.handleInteractiveLine(&tty, line, pause); quit || err != nil {
			t.Fatalf("%s: quit=%v err=%v", line, quit, err)
		}
		if strings.Contains(tty.String(), interactiveResumedMessage) || logger.pauseDepth() != 1 {
			t.Fatalf("%s resumed the output: tty=%q depth=%d", line, tty.String(), logger.pauseDepth())
		}
	}
	requireBlocked(t, payload)

	tty.Reset()
	// A blank line (spaces, a carriage return) counts as empty.
	if quit, err := client.handleInteractiveLine(&tty, " \t\r", pause); quit || err != nil {
		t.Fatalf("blank line: quit=%v err=%v", quit, err)
	}
	if tty.String() != interactiveResumedMessage+"\n" {
		t.Fatalf("tty = %q, want %q", tty.String(), interactiveResumedMessage+"\n")
	}
	requireDone(t, payload)
	requireDone(t, serverless)
	if logger.pauseDepth() != 0 || direct.String() != "direct" {
		t.Fatalf("after resume: depth=%d direct=%q", logger.pauseDepth(), direct.String())
	}
	// Resuming again is a no-op: the loop's deferred resume must not undo
	// a pause of the host key prompt or the interrupt statistics.
	logger.Pause()
	if pause.resume() || logger.pauseDepth() != 1 {
		t.Fatalf("resume without own pause changed the depth to %d", logger.pauseDepth())
	}
}

// TestHandleInteractiveLineReloadResumesFirst checks that a :reload typed
// while paused resumes the output before it runs (the servers'
// acknowledgement is read by the handlers the pause blocks), also when the
// reload then fails, while :quit leaves the resume to the loop's end.
func TestHandleInteractiveLineReloadResumesFirst(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var tty bytes.Buffer

	if _, err := client.handleInteractiveLine(&tty, "", pause); err != nil {
		t.Fatalf("pause: %v", err)
	}
	tty.Reset()
	quit, err := client.handleInteractiveLine(&tty, ":reload --regex WARN", pause)
	if quit || err != nil {
		t.Fatalf("reload: quit=%v err=%v", quit, err)
	}
	// No connection takes the reload, so it fails, after the resume.
	want := interactiveResumedMessage + "\nreload failed: no active connections\n"
	if tty.String() != want {
		t.Fatalf("tty = %q, want %q", tty.String(), want)
	}
	if logger.pauseDepth() != 0 {
		t.Fatalf("pause depth = %d after reload, want 0", logger.pauseDepth())
	}

	// A reload that is not paused prints no resume message.
	tty.Reset()
	if _, reloadErr := client.handleInteractiveLine(&tty, ":reload --regex WARN", pause); reloadErr != nil {
		t.Fatalf("reload: %v", reloadErr)
	}
	if strings.Contains(tty.String(), interactiveResumedMessage) {
		t.Fatalf("unpaused reload printed %q", tty.String())
	}

	if _, pauseErr := client.handleInteractiveLine(&tty, "", pause); pauseErr != nil {
		t.Fatalf("pause: %v", pauseErr)
	}
	quit, err = client.handleInteractiveLine(&tty, ":quit", pause)
	if !quit || err != nil {
		t.Fatalf("quit: quit=%v err=%v", quit, err)
	}
	if logger.pauseDepth() != 1 {
		t.Fatalf("pause depth = %d after :quit, want 1 until the loop ends", logger.pauseDepth())
	}
	if !pause.resume() || logger.pauseDepth() != 0 {
		t.Fatalf("loop-end resume left depth %d", logger.pauseDepth())
	}
}

// TestHandleInteractiveLineRejectedKeepsPause checks that a command rejected
// while explicitly paused is answered with its error and leaves the output
// paused (doc/interactive-query.md, "While explicitly paused"), also a
// :reload or :probe line, which resume only once they will run: flag,
// tokenizer, validation and query errors all keep the pause.
func TestHandleInteractiveLineRejectedKeepsPause(t *testing.T) {
	mapArgs := config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: "from STATS select count($line) group by $hostname"}
	tests := []struct {
		name string
		args *config.Args
		line string
	}{
		{name: "unknown flag", line: ":reload --bogus 1"},
		{name: "unterminated quote", line: ":reload --regex 'unterminated"},
		{name: "no flags", line: ":reload"},
		{name: "blank files", line: ":reload --files ''"},
		{name: "empty grep", line: ":reload --grep ''"},
		{name: "leftover argument", line: ":reload --regex x y"},
		{name: "invalid utf8", line: ":reload --regex \xff"},
		{name: "invalid query", args: &mapArgs, line: ":reload --query 'select'"},
		{name: "query invert", args: &mapArgs, line: ":reload --invert"},
		{name: "probe argument", line: ":probe now"},
		{name: "fields arguments", line: ":fields a b"},
		{name: "unknown command", line: ":reloadx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := newPausingTestLogger()
			client := newPauseTestClient(logger)
			if tt.args != nil {
				client.Args = *tt.args
			}
			pause := client.newInteractivePause()
			var tty bytes.Buffer
			if _, err := client.handleInteractiveLine(&tty, "", pause); err != nil {
				t.Fatalf("pause: %v", err)
			}
			tty.Reset()
			if quit, err := client.handleInteractiveLine(&tty, tt.line, pause); quit || err != nil {
				t.Fatalf("%q: quit=%v err=%v", tt.line, quit, err)
			}
			got := tty.String()
			if strings.Contains(got, interactiveResumedMessage) || logger.pauseDepth() != 1 {
				t.Fatalf("%q resumed the output: tty=%q depth=%d", tt.line, got, logger.pauseDepth())
			}
			if !strings.HasPrefix(got, "interactive query error: ") {
				t.Fatalf("%q answered %q, want the rejection", tt.line, got)
			}
		})
	}
}

// TestHandleInteractiveLinePauseWriteError checks that a failed TTY write
// while pausing is returned (it ends the control loop, which resumes).
func TestHandleInteractiveLinePauseWriteError(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	writeErr := errors.New("tty gone")

	_, err := client.handleInteractiveLine(failedPayloadWriter{err: writeErr}, "", pause)
	if !errors.Is(err, writeErr) {
		t.Fatalf("err = %v, want %v", err, writeErr)
	}
	if !pause.paused {
		t.Fatal("output not marked paused, the loop's end would not resume it")
	}
}

// TestServerlessOutputWriterIsGated checks that the serverless direct output
// the runtime hands to the in-process server, with and without the payload
// file tee, waits while the interactive output is paused and is written in
// full on resume.
func TestServerlessOutputWriterIsGated(t *testing.T) {
	for _, logPayload := range []bool{false, true} {
		t.Run(fmt.Sprint(logPayload), func(t *testing.T) {
			var stdout bytes.Buffer
			tee := &bytePayloadTeeLogger{}
			r := newClientRuntimeBoundary(config.RuntimeConfig{
				Client: &config.ClientConfig{LogPayload: logPayload},
			}, NewLoggerDependencies(tee, logging.NopLogger{}, logging.NopLogger{}))
			r.stdout = func() io.Writer { return &stdout }
			writer := r.serverlessOutputWriter()

			r.outputGate.Pause()
			done := startWrite(func() { _, _ = writer.Write([]byte("payload\n")) })
			requireBlocked(t, done)
			if stdout.Len() != 0 || tee.byteFile.Len() != 0 {
				t.Fatalf("paused output written: stdout=%q tee=%q", stdout.String(), tee.byteFile.String())
			}
			r.outputGate.Resume()
			requireDone(t, done)
			if stdout.String() != "payload\n" {
				t.Fatalf("stdout = %q after resume", stdout.String())
			}
			wantTee := ""
			if logPayload {
				wantTee = "payload\n"
			}
			if tee.byteFile.String() != wantTee {
				t.Fatalf("tee = %q, want %q", tee.byteFile.String(), wantTee)
			}
		})
	}
}

// TestStatsReceiveInterruptWhileLoggerPaused covers Ctrl-C while the output
// is paused: a connection count change makes the stats loop log the periodic
// stats, which wait for the paused logger. The loop must still take the
// interrupt from statsCh, as the interrupt handler drops a Ctrl-C it cannot
// hand over (internal/io/signal) and then never exits on the second one.
// The stats are logged once the logger resumes.
func TestStatsReceiveInterruptWhileLoggerPaused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		logger := newPausingTestLogger()
		s := newTailStats(1, nil, time.Second, logger)
		statsCh := make(chan string)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		logger.Pause()
		go s.Start(ctx, make(chan struct{}, 1), statsCh, false)

		s.connectionsEstCh <- struct{}{}
		time.Sleep(4 * time.Second) // Past the 3 s stats tick.
		synctest.Wait()
		if got := logger.writtenLines(); len(got) != 0 {
			t.Fatalf("stats logged while paused: %q", got)
		}
		select {
		case statsCh <- "Hint: Hit Ctrl+C again to exit":
		default:
			t.Fatal("stats loop does not take the interrupt while the logger is paused")
		}

		logger.Resume()
		time.Sleep(2 * time.Second) // Past the interrupt pause.
		synctest.Wait()
		if got := logger.writtenLines(); len(got) != 1 || got[0] != "STATS" {
			t.Fatalf("logged after resume = %q, want one STATS table", got)
		}
	})
}

// TestHandleInteractiveLineTypingPause covers the pause the line editor
// starts on the first key of a command: the line's end resumes it silently
// whatever the line was, while an explicit pause (empty line) stays
// explicit when a command is typed into it.
func TestHandleInteractiveLineTypingPause(t *testing.T) {
	for _, tt := range []struct {
		line     string
		wantQuit bool
		wantTTY  string // prefix; "" means no output at all
	}{
		{line: ":show", wantTTY: "mode="},
		{line: "  :show  ", wantTTY: "mode="},
		{line: ":bogus", wantTTY: "interactive query error: "},
		{line: ":reload --regex WARN", wantTTY: "reload failed: no active connections"},
		{line: "", wantTTY: ""},
		{line: ":quit", wantQuit: true, wantTTY: "quitting interactive session"},
	} {
		t.Run(tt.line, func(t *testing.T) {
			logger := newPausingTestLogger()
			client := newPauseTestClient(logger)
			pause := client.newInteractivePause()
			var tty bytes.Buffer

			pause.pauseTyping()
			if logger.pauseDepth() != 1 || !pause.typing {
				t.Fatalf("pauseTyping: depth %d typing %v", logger.pauseDepth(), pause.typing)
			}
			quit, err := client.handleInteractiveLine(&tty, tt.line, pause)
			if err != nil || quit != tt.wantQuit {
				t.Fatalf("quit=%v err=%v", quit, err)
			}
			if logger.pauseDepth() != 0 || pause.paused || pause.typing {
				t.Fatalf("not resumed after the line: depth %d paused %v typing %v",
					logger.pauseDepth(), pause.paused, pause.typing)
			}
			if strings.Contains(tty.String(), interactiveResumedMessage) {
				t.Fatalf("typing resume printed %q", tty.String())
			}
			if (tt.wantTTY == "") != (tty.Len() == 0) || !strings.HasPrefix(tty.String(), tt.wantTTY) {
				t.Fatalf("tty = %q, want prefix %q", tty.String(), tt.wantTTY)
			}
		})
	}

	// Typing into an explicit pause keeps it explicit: a command leaves the
	// output paused, as without the line editor.
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var tty bytes.Buffer
	if _, err := client.handleInteractiveLine(&tty, "", pause); err != nil {
		t.Fatal(err)
	}
	pause.pauseTyping()
	if _, err := client.handleInteractiveLine(&tty, ":show", pause); err != nil {
		t.Fatal(err)
	}
	if logger.pauseDepth() != 1 || !pause.paused || pause.typing {
		t.Fatalf("explicit pause lost: depth %d paused %v typing %v", logger.pauseDepth(), pause.paused, pause.typing)
	}
}

// TestEditorKey checks the line editor's key hook: Enter and Ctrl-D neither
// pause nor change the line, any other key pauses for typing, and Tab
// completes from the client's current arguments.
func TestEditorKey(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var out bytes.Buffer
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(""), &out}, "")
	editor := &interactiveEditor{terminal: terminal}

	// Keys that insert nothing must not pause: unbound control keys (Ctrl-G),
	// x/term's private codes for unbound escape sequences (PageUp, F-keys)
	// and the line endings.
	for _, key := range []rune{'\r', '\n', 4, 7, 0x1b, 0xd800, 0xd80f, 0xdbff} {
		if _, _, ok := client.editorKey(editor, pause, "", 0, key); ok || pause.paused {
			t.Fatalf("key %#x: ok %v paused %v", key, ok, pause.paused)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("keys that insert nothing wrote %q", out.String())
	}
	if _, _, ok := client.editorKey(editor, pause, "", 0, ':'); ok || !pause.typing {
		t.Fatalf("':' must pause for typing and be left to the terminal: ok %v typing %v", ok, pause.typing)
	}
	// The pause shows the hint once: not again for the next keys of the line.
	if strings.Count(out.String(), interactiveTypingHint) != 1 {
		t.Fatalf("hint shown %d times after the first key: %q", strings.Count(out.String(), interactiveTypingHint), out.String())
	}
	client.editorKey(editor, pause, ":", 1, 's')
	if strings.Count(out.String(), interactiveTypingHint) != 1 {
		t.Fatalf("hint repeated on the second key: %q", out.String())
	}
	line, pos, ok := client.editorKey(editor, pause, ":reload --gr", len(":reload --gr"), '\t')
	if !ok || line != ":reload --grep " || pos != len(line) {
		t.Fatalf("tab: %q %d %v", line, pos, ok)
	}
	line, _, ok = client.editorKey(editor, pause, ":reload --grep ", len(":reload --grep "), '\t')
	if !ok || line != ":reload --grep ERROR" {
		t.Fatalf("tab value: %q %v", line, ok)
	}
	if line, _, ok := client.editorKey(editor, pause, ":nope", 5, '\t'); !ok || line != ":nope" {
		t.Fatalf("tab without completion must keep the line and consume the key: %q %v", line, ok)
	}
}

// TestDispatchControlLineHostKeyAnswer checks that a line typed while the host
// key question is asked goes to the question, is not run as a command, and
// resumes the output its keys paused for typing; an explicit pause stays.
func TestDispatchControlLineHostKeyAnswer(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		logger := newPausingTestLogger()
		client := newPauseTestClient(logger)
		pause := client.newInteractivePause()
		router := &fakePromptRouter{}
		router.prompting.Store(true)
		answers := &promptAnswers{router: router, lines: make(chan []byte, 1)}
		var tty bytes.Buffer
		if explicit {
			pause.pause()
		} else {
			pause.pauseTyping()
		}
		quit, err := client.dispatchControlLine(&tty, "yes", pause, answers)
		if quit || err != nil || tty.Len() != 0 {
			t.Fatalf("explicit=%v: quit %v err %v tty %q", explicit, quit, err, tty.String())
		}
		if got := string(<-answers.lines); got != "yes\n" {
			t.Fatalf("answer = %q", got)
		}
		if pause.paused != explicit {
			t.Fatalf("explicit=%v: paused = %v after the answer", explicit, pause.paused)
		}
	}
	// Without a question the line is a command again.
	client := newPauseTestClient(newPausingTestLogger())
	var tty bytes.Buffer
	answers := &promptAnswers{router: &fakePromptRouter{}, lines: make(chan []byte, 1)}
	if _, err := client.dispatchControlLine(&tty, ":show", client.newInteractivePause(), answers); err != nil ||
		!strings.HasPrefix(tty.String(), "mode=") {
		t.Fatalf("command not run: %q %v", tty.String(), err)
	}
}

// TestControlHistoryRecallPauses checks that recalling a line (Up/Down, which
// x/term handles without the key callback) pauses the output for typing.
func TestControlHistoryRecallPauses(t *testing.T) {
	recalled := 0
	h := &controlHistory{onRecall: func() { recalled++ }}
	h.Add(":show")
	if h.At(0) != ":show" || recalled != 1 {
		t.Fatalf("At = %q, recalled %d", h.At(0), recalled)
	}
}

// TestInteractiveEditorRejectsLongLine checks that a line the Terminal may
// have cut at its 4096 rune limit is returned as errControlLineTooLong (not
// run, not kept in the history), that the rejection resumes the output its
// keys paused for typing, and that a line below the limit is returned.
func TestInteractiveEditorRejectsLongLine(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	history := &controlHistory{}
	input := strings.Repeat("x", editorMaxLine+500) + "\n:show\n"
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(input), io.Discard}, "")
	terminal.History = history
	editor := &interactiveEditor{terminal: terminal}
	terminal.AutoCompleteCallback = func(line string, pos int, key rune) (string, int, bool) {
		return client.editorKey(editor, pause, line, pos, key)
	}

	if _, err := editor.ReadLine(); !errors.Is(err, errControlLineTooLong) {
		t.Fatalf("long line: err %v, want errControlLineTooLong", err)
	}
	if !pause.typing || logger.pauseDepth() != 1 {
		t.Fatalf("typing the long line did not pause: typing %v depth %d", pause.typing, logger.pauseDepth())
	}
	var tty bytes.Buffer
	if err := client.rejectControlLine(&tty, errControlLineTooLong, pause); err != nil {
		t.Fatal(err)
	}
	if pause.paused || logger.pauseDepth() != 0 || !strings.Contains(tty.String(), "line too long") {
		t.Fatalf("rejection: paused %v depth %d tty %q", pause.paused, logger.pauseDepth(), tty.String())
	}
	if line, err := editor.ReadLine(); err != nil || line != ":show" {
		t.Fatalf("next line = %q, %v; want :show", line, err)
	}
	if history.Len() != 1 || history.entries[0] != ":show" {
		t.Fatalf("history = %q, want only :show", history.entries)
	}
	// An explicit pause stays when a long line is rejected (reading :show
	// above paused for typing: start over).
	pause.resume()
	pause.pause()
	if err := client.rejectControlLine(io.Discard, errControlLineTooLong, pause); err != nil || !pause.paused {
		t.Fatalf("explicit pause lost: %v %v", err, pause.paused)
	}
	short := &interactiveEditor{terminal: term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{strings.NewReader(strings.Repeat("y", editorMaxLine-1) + "\n"), io.Discard}, "")}
	if line, err := short.ReadLine(); err != nil || len(line) != editorMaxLine-1 {
		t.Fatalf("line below the limit: len %d err %v", len(line), err)
	}
}

// TestHandleInteractiveLineHelpKeepsPause checks that :help typed while the
// output was paused for typing keeps it paused (as an explicit pause, with
// the paused message after the help), so the help does not scroll away, and
// that the next empty line resumes it with the resumed message.
func TestHandleInteractiveLineHelpKeepsPause(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var tty bytes.Buffer

	pause.pauseTyping()
	if quit, err := client.handleInteractiveLine(&tty, ":help", pause); quit || err != nil {
		t.Fatalf("quit=%v err=%v", quit, err)
	}
	want := interactiveHelpText(client.Args) + "\n" + interactivePausedMessage + "\n"
	if tty.String() != want {
		t.Fatalf("tty = %q, want %q", tty.String(), want)
	}
	if !pause.paused || pause.typing || logger.pauseDepth() != 1 {
		t.Fatalf("after :help: paused %v typing %v depth %d", pause.paused, pause.typing, logger.pauseDepth())
	}
	// Another command keeps the explicit pause, as without the editor.
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":show", pause); err != nil || !pause.paused {
		t.Fatalf(":show after :help: err %v paused %v", err, pause.paused)
	}
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, "", pause); err != nil {
		t.Fatal(err)
	}
	if pause.paused || logger.pauseDepth() != 0 || tty.String() != interactiveResumedMessage+"\n" {
		t.Fatalf("empty line: paused %v depth %d tty %q", pause.paused, logger.pauseDepth(), tty.String())
	}
	// Not paused (no line editor, so typing did not pause): :help pauses
	// too, explicitly, with the same message.
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":help", pause); err != nil {
		t.Fatal(err)
	}
	if !pause.paused || pause.typing || logger.pauseDepth() != 1 || tty.String() != want {
		t.Fatalf(":help unpaused: paused %v typing %v depth %d tty %q",
			pause.paused, pause.typing, logger.pauseDepth(), tty.String())
	}
}
