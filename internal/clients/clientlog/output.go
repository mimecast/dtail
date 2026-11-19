// Package clientlog adapts optional client-output capabilities implemented by
// the configured logger without widening the shared diagnostic Logger contract.
package clientlog

import "github.com/mimecast/dtail/internal/logging"

// Logger combines diagnostic logging with the raw output operations required
// by client handlers. Requiring both prevents a valid diagnostics-only logger
// from silently dropping retrieved user payload.
type Logger interface {
	logging.Logger
	Raw(string) string
	RawLog(string) string
}

// NopLogger discards both diagnostics and client payload.
type NopLogger struct {
	logging.NopLogger
}

func (NopLogger) Raw(string) string    { return "" }
func (NopLogger) RawLog(string) string { return "" }

// OrNop replaces nil and typed-nil client loggers with a no-op logger.
func OrNop(logger Logger) Logger {
	normalized := logging.OrNop(logger)
	if output, ok := normalized.(Logger); ok {
		return output
	}
	return NopLogger{}
}

type mapreduceLogger interface {
	Mapreduce(string, map[string]any) string
}

type pauser interface {
	Pause()
	Resume()
}

type payloadFileTeer interface {
	RawPayloadFileTee(string)
}

// Raw writes client payload through the logger's raw-output capability.
func Raw(logger Logger, message string) {
	logger.Raw(message)
}

// RawDiagnostic writes a preformatted diagnostic through the logger's required
// raw-log capability.
func RawDiagnostic(logger Logger, message string) {
	logger.RawLog(message)
}

// Mapreduce writes structured client statistics when the logger supports it.
func Mapreduce(logger logging.Logger, table string, data map[string]any) {
	if output, ok := logger.(mapreduceLogger); ok {
		output.Mapreduce(table, data)
	}
}

// Pause yields an interactive terminal owned by the logger.
func Pause(logger logging.Logger) {
	if output, ok := logger.(pauser); ok {
		output.Pause()
	}
}

// Resume returns an interactive terminal to the logger.
func Resume(logger logging.Logger) {
	if output, ok := logger.(pauser); ok {
		output.Resume()
	}
}

// TeePayloadToFile passes serverless direct-output bytes to the logger's
// optional file-only payload sink.
func TeePayloadToFile(logger logging.Logger, message string) {
	if output, ok := logger.(payloadFileTeer); ok {
		output.RawPayloadFileTee(message)
	}
}
