package handlers

import (
	"runtime/debug"

	"github.com/mimecast/dtail/internal/logging"
)

// recoverHandlerPanic contains a panic to one session-owned goroutine. The
// optional abort callback must only signal cancellation/shutdown; it must not
// wait for the goroutine whose defer is invoking it.
func recoverHandlerPanic(logger logging.Logger, logContext any, scope string, abort func()) {
	panicValue := recover()
	if panicValue == nil {
		return
	}

	logging.OrNop(logger).Error(logContext, "Recovered panic in session goroutine",
		"scope", scope, "panic", panicValue, "stack", string(debug.Stack()))
	if abort == nil {
		return
	}

	// A cleanup bug must not turn a successfully recovered work panic into a
	// process-wide panic.
	func() {
		defer func() {
			if cleanupPanic := recover(); cleanupPanic != nil {
				logging.OrNop(logger).Error(logContext, "Recovered panic while aborting session",
					"scope", scope, "panic", cleanupPanic, "stack", string(debug.Stack()))
			}
		}()
		abort()
	}()
}
