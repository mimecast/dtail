package clientlog

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

// stringOnlyLogger satisfies Logger without the byte-slice capability.
type stringOnlyLogger struct {
	logging.NopLogger
	raws []string
}

func (l *stringOnlyLogger) RawLog(message string) string { return message }

func (l *stringOnlyLogger) Raw(message string) string {
	l.raws = append(l.raws, message)
	return message
}

type bytesLogger struct {
	stringOnlyLogger
	rawBytes []string
}

func (l *bytesLogger) RawBytes(message []byte) {
	l.rawBytes = append(l.rawBytes, string(message))
}

func TestRawBytesUsesByteCapability(t *testing.T) {
	logger := &bytesLogger{}
	RawBytes(logger, []byte("payload\n"))

	if len(logger.rawBytes) != 1 || logger.rawBytes[0] != "payload\n" || len(logger.raws) != 0 {
		t.Fatalf("rawBytes=%q raws=%q, want one byte-path write", logger.rawBytes, logger.raws)
	}
}

func TestRawBytesFallsBackToRaw(t *testing.T) {
	logger := &stringOnlyLogger{}
	RawBytes(logger, []byte("payload\n"))

	if len(logger.raws) != 1 || logger.raws[0] != "payload\n" {
		t.Fatalf("raws=%q, want the payload through Raw", logger.raws)
	}
}
