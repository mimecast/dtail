package loggers

import (
	"context"
	"sync"
)

// Logger is there to plug in your own log implementation.
//
// The contract deliberately carries no timestamp: Raw and RawWithColors run once
// per retrieved payload line, and reading the clock there dominated client CPU
// on hosts whose clocksource the vDSO cannot read, such as hpet. Sinks that
// need wall time (the daily file sink) read and cache it themselves at a coarse
// granularity.
type Logger interface {
	Log(message string)
	LogWithColors(message, messageWithColors string)
	Raw(message string)
	RawWithColors(message, messageWithColors string)
	Flush()
	SupportsColors() bool
}

// RawBytesWriter is implemented by sinks that can write payload from a byte
// slice without first converting it to a string. RawBytes writes message
// verbatim, exactly like Raw. The slice is only valid during the call and must
// not be retained. Sinks without this capability receive a string via Raw.
type RawBytesWriter interface {
	RawBytes(message []byte)
}

// WriteRawBytes writes payload through logger's RawBytesWriter capability when
// it has one and through its string Raw method otherwise, so both paths produce
// identical output. It is the single dispatch point for sinks in this package
// and for DLog. message is not retained.
func WriteRawBytes(logger Logger, message []byte) {
	if writer, ok := logger.(RawBytesWriter); ok {
		writer.RawBytes(message)
		return
	}
	logger.Raw(string(message))
}

// Starter is implemented by loggers that own background work.
type Starter interface {
	Start(ctx context.Context, wg *sync.WaitGroup)
}

// Pauser is implemented by terminal loggers whose output must pause while an
// interactive prompt temporarily owns the terminal.
type Pauser interface {
	Pause()
	Resume()
}

// Rotator is implemented by loggers backed by rotatable resources.
type Rotator interface {
	Rotate()
}
