// Package logging defines the logging contract used by DTail leaf packages.
package logging

import "reflect"

// Logger is the small consumer-side logging contract shared by packages that
// must not depend on the configured dlog implementation or its global loggers.
//
// Implementations must format args before returning and must not retain them
// past the call. Callers on borrowed-memory paths (the MapReduce line path,
// for example, where a value can alias a buffer that is recycled right after
// the call) pass values whose backing memory is reused immediately, so a
// logger that queued args for asynchronous formatting would render whatever
// overwrote them.
type Logger interface {
	Error(args ...any) string
	Warn(args ...any) string
	Info(args ...any) string
	Debug(args ...any) string
	Trace(args ...any) string
	TraceEnabled() bool
}

// NopLogger discards log entries. It is useful for callers and tests that do
// not need logging but still want to construct a leaf-package service.
type NopLogger struct{}

func (NopLogger) Error(...any) string { return "" }
func (NopLogger) Warn(...any) string  { return "" }
func (NopLogger) Info(...any) string  { return "" }
func (NopLogger) Debug(...any) string { return "" }
func (NopLogger) Trace(...any) string { return "" }

// TraceEnabled reports that trace logging is disabled.
func (NopLogger) TraceEnabled() bool { return false }

// DebugEnabled reports that debug logging is disabled.
func (NopLogger) DebugEnabled() bool { return false }

// debugEnabler is implemented by loggers that can tell whether they write
// debug messages (see DebugEnabled).
type debugEnabler interface {
	DebugEnabled() bool
}

// DebugEnabled reports whether logger writes debug messages, so that a
// per-message caller can skip building the arguments of a Debug call that
// would be dropped. It is an optional capability rather than part of Logger:
// a logger that cannot tell is assumed to write them.
func DebugEnabled(logger Logger) bool {
	enabler, ok := logger.(debugEnabler)
	return !ok || enabler.DebugEnabled()
}

// OrNop replaces a nil logger with a no-op implementation.
func OrNop(logger Logger) Logger {
	if logger == nil || isNil(logger) {
		return NopLogger{}
	}
	return logger
}

func isNil(logger Logger) bool {
	value := reflect.ValueOf(logger)
	return value.Kind() == reflect.Pointer && value.IsNil()
}
