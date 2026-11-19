package loggers

import (
	"context"
	"sync"
)

// fout logs to both a file and stdout. It is the default client logger.
//
// The two things a client emits are deliberately split at this seam:
//   - Diagnostics (connection INFO/WARN/ERROR/etc.) arrive via Log/LogWithColors
//     and are ALWAYS written to both stdout and the file — they are the small,
//     useful audit trail the daily log file is meant to keep.
//   - Retrieved payload (the bulk dcat/dgrep/dtail output) arrives via
//     Raw/RawWithColors. It always reaches stdout/terminal, but it is teed to
//     the file only when logPayload is set (opt-in via --log-payload /
//     Client.LogPayload). By default the file receives no payload, so a bulk
//     dcat no longer silently grows the daily log file by the full payload size.
type fout struct {
	file       Logger
	stdout     Logger
	logPayload bool
}

var _ Logger = (*fout)(nil)
var _ Starter = (*fout)(nil)
var _ Pauser = (*fout)(nil)
var _ Rotator = (*fout)(nil)
var _ RawBytesWriter = (*fout)(nil)

// newFout builds the default client logger from injected process options.
func newFout(strategy Strategy, options Options) *fout {
	return newFoutWithSinks(newFile(strategy, options.LogDir), newStdout(), options.LogPayload)
}

// newFoutWithSinks builds a fout over injectable sinks and an explicit payload
// switch. Production uses newFout (concrete file+stdout, injected switch);
// tests inject fakes to assert that diagnostics always reach the file while
// payload reaches it only when opted in.
func newFoutWithSinks(file, stdout Logger, logPayload bool) *fout {
	return &fout{file: file, stdout: stdout, logPayload: logPayload}
}

func (f *fout) Start(ctx context.Context, wg *sync.WaitGroup) {
	go func() {
		defer wg.Done()

		var wg2 sync.WaitGroup
		startLogger(ctx, &wg2, f.file)
		startLogger(ctx, &wg2, f.stdout)
		wg2.Wait()
	}()
}

func startLogger(ctx context.Context, wg *sync.WaitGroup, logger Logger) {
	starter, ok := logger.(Starter)
	if !ok {
		return
	}
	wg.Add(1)
	starter.Start(ctx, wg)
}

func (f *fout) Log(message string) {
	f.stdout.Log(message)
	f.file.Log(message)
}

func (f *fout) LogWithColors(message, coloredMessage string) {
	f.stdout.LogWithColors("", coloredMessage)
	f.file.LogWithColors(message, coloredMessage)
}

// Raw writes retrieved payload. It always reaches stdout/terminal; it is teed
// to the file sink only when the client opted in via --log-payload /
// Client.LogPayload. By default the file is left payload-free.
func (f *fout) Raw(message string) {
	f.stdout.Raw(message)
	if f.logPayload {
		f.file.Raw(message)
	}
}

// RawBytes is the byte-slice form of Raw with the same --log-payload gate. Only
// the file tee converts to a string, and only when payload teeing is enabled.
func (f *fout) RawBytes(message []byte) {
	WriteRawBytes(f.stdout, message)
	if f.logPayload {
		WriteRawBytes(f.file, message)
	}
}

func (f *fout) RawWithColors(message, coloredMessage string) {
	f.stdout.RawWithColors("", coloredMessage)
	// Same opt-in gate as Raw; the file gets the plain (uncolored) payload.
	if f.logPayload {
		f.file.RawWithColors(message, coloredMessage)
	}
}

// RawFileOnly tees retrieved payload into the daily log FILE sink only, never to
// stdout, honoring the same --log-payload / Client.LogPayload opt-in as Raw.
//
// It exists for the serverless direct-output path: that path writes
// payload straight to its own stdout sink and bypasses Raw entirely, so without
// this hook --log-payload would silently no longer tee payload to the file in
// serverless mode. The caller (the serverless output writer) already emits the
// payload bytes to stdout itself, so this method deliberately writes ONLY to the
// file to keep stdout byte-identical whether or not --log-payload is set.
func (f *fout) RawFileOnly(message string) {
	if f.logPayload {
		f.file.Raw(message)
	}
}

func (f *fout) Flush() { f.stdout.Flush(); f.file.Flush() }

func (f *fout) Pause() {
	for _, logger := range []Logger{f.stdout, f.file} {
		if pauser, ok := logger.(Pauser); ok {
			pauser.Pause()
		}
	}
}

func (f *fout) Resume() {
	for _, logger := range []Logger{f.stdout, f.file} {
		if pauser, ok := logger.(Pauser); ok {
			pauser.Resume()
		}
	}
}

func (f *fout) Rotate() {
	if rotator, ok := f.file.(Rotator); ok {
		rotator.Rotate()
	}
}

func (*fout) SupportsColors() bool { return true }
