package server

import "runtime/debug"

// recoverGoroutinePanic contains a panic within one connection or session
// goroutine and records enough context to diagnose it without stopping dserver.
func (s *Server) recoverGoroutinePanic(scope string, logContext any, onPanic func()) {
	recovered := recover()
	if recovered == nil {
		return
	}

	if logContext == nil {
		s.log().Error("Recovered panic", "scope", scope, "panic", recovered, "stack", string(debug.Stack()))
	} else {
		s.log().Error(logContext, "Recovered panic", "scope", scope, "panic", recovered, "stack", string(debug.Stack()))
	}
	if onPanic != nil {
		// A cleanup bug must not let a successfully contained connection or
		// session panic escape through this deferred recovery function.
		func() {
			defer func() {
				if cleanupPanic := recover(); cleanupPanic != nil {
					if logContext == nil {
						s.log().Error("Recovered panic while cleaning up goroutine", "scope", scope,
							"panic", cleanupPanic, "stack", string(debug.Stack()))
					} else {
						s.log().Error(logContext, "Recovered panic while cleaning up goroutine", "scope", scope,
							"panic", cleanupPanic, "stack", string(debug.Stack()))
					}
				}
			}()
			onPanic()
		}()
	}
}
