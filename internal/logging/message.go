package logging

import "github.com/mimecast/dtail/internal/protocol"

// MessageLogger is the optional Logger capability for diagnostics that a
// server also sends to its client. Each method logs args at its level exactly
// like the Logger method of the same level, but returns the formatted message
// even when the logger's own level filters it out (a Logger method returns ""
// then). A client-facing message thus does not depend on the server's log
// level: with dserver --logLevel error a client still gets its warnings.
type MessageLogger interface {
	ErrorMessage(args ...any) string
	WarnMessage(args ...any) string
}

// ErrorMessage logs args at error level through logger and returns the
// client-facing text of the diagnostic, which is never empty (see
// MessageLogger).
func ErrorMessage(logger Logger, args ...any) string {
	logger = OrNop(logger)
	if messages, ok := logger.(MessageLogger); ok {
		return messages.ErrorMessage(args...)
	}
	return orFallbackMessage(logger.Error(args...), "ERROR", args)
}

// WarnMessage logs args at warn level through logger and returns the
// client-facing text of the diagnostic, which is never empty (see
// MessageLogger).
func WarnMessage(logger Logger, args ...any) string {
	logger = OrNop(logger)
	if messages, ok := logger.(MessageLogger); ok {
		return messages.WarnMessage(args...)
	}
	return orFallbackMessage(logger.Warn(args...), "WARN", args)
}

// orFallbackMessage returns message, what a logger without the MessageLogger
// capability returned, or, when it returned nothing (its level filtered the
// diagnostic out, or it discards everything like NopLogger), the level and
// args as protocol fields.
func orFallbackMessage(message, level string, args []any) string {
	if message != "" {
		return message
	}
	return level + protocol.FieldSeparator() + protocol.EncodeDiagnosticDetails(args)
}
