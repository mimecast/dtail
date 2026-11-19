package loggers

import "time"

// nextFlushDelay keeps the original periodic schedule when a logger rearms
// after idle. Starting a fresh full interval instead would add up to an extra
// interval of latency to sparse output. Called only by the flush workers,
// never on the per-line producer path. interval is a positive constant.
func nextFlushDelay(start time.Time, interval time.Duration) time.Duration {
	return interval - time.Since(start)%interval
}
