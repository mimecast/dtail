package clientlog

import (
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

type stringFileTeer struct {
	logging.NopLogger
	file string
}

type byteFileTeer struct {
	stringFileTeer
	byteFile string
}

func (l *stringFileTeer) RawPayloadFileTee(message string) { l.file = message }

func (l *byteFileTeer) RawPayloadFileTeeBytes(message []byte) { l.byteFile = string(message) }

func TestTeePayloadBytesToFileCapabilities(t *testing.T) {
	data := []byte("payload\x00\n")
	byteSink, stringSink := &byteFileTeer{}, &stringFileTeer{}
	TeePayloadBytesToFile(byteSink, data)
	TeePayloadBytesToFile(stringSink, data)
	TeePayloadBytesToFile(logging.NopLogger{}, data)
	TeePayloadBytesToFile(nil, data)
	clear(data)
	if byteSink.byteFile != "payload\x00\n" || byteSink.file != "" || stringSink.file != "payload\x00\n" {
		t.Fatalf("bytes=%q legacy=%q fallback=%q", byteSink.byteFile, byteSink.file, stringSink.file)
	}
}
