package sessionuser

import "github.com/mimecast/dtail/internal/logging"

// checkLog logs the lines of one read permission check. A verbose check (a
// read command's, ResolveReadTarget) logs every line at its own level, as
// documented for reads. A quiet check (ResolveReadTargetQuietly) logs the
// lines about the single path at debug level: the probe command checks up
// to thousands of paths in every round of every interactive session and
// logs one summary line of its own instead.
type checkLog struct {
	logger logging.Logger
	quiet  bool
	// debugOn tells whether the logger writes debug lines, so that the
	// per-rule debug line of iteratePaths boxes no arguments when it does
	// not.
	debugOn bool
}

// checkLog returns the log of a permission check of u.
func (u *User) checkLog(quiet bool) checkLog {
	logger := u.log()
	return checkLog{logger: logger, quiet: quiet, debugOn: logging.DebugEnabled(logger)}
}

// error logs at error level, at debug level when quiet.
func (l checkLog) error(args ...any) {
	if l.quiet {
		l.logger.Debug(args...)
		return
	}
	l.logger.Error(args...)
}

// warn logs at warn level, at debug level when quiet.
func (l checkLog) warn(args ...any) {
	if l.quiet {
		l.logger.Debug(args...)
		return
	}
	l.logger.Warn(args...)
}

// info logs at info level, at debug level when quiet.
func (l checkLog) info(args ...any) {
	if l.quiet {
		l.logger.Debug(args...)
		return
	}
	l.logger.Info(args...)
}

// debug logs at debug level.
func (l checkLog) debug(args ...any) {
	l.logger.Debug(args...)
}

// infoWritten reports whether info writes anything, so that a caller in a
// loop over many paths can skip boxing the arguments of a line a quiet
// check would drop.
func (l checkLog) infoWritten() bool {
	return !l.quiet || l.debugOn
}
