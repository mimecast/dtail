package dlog

import (
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/io/dlog/loggers"
)

type byteFileTeeRecorder struct {
	behaviorLogger
	bytes []string
}

type coreOnlyLogger struct{ loggers.Logger }

type teeColorizer struct{ calls int }

func (c *teeColorizer) Colorfy(message string) string {
	c.calls++
	return "colored:" + message
}

func (l *byteFileTeeRecorder) RawFileOnlyBytes(message []byte) {
	l.bytes = append(l.bytes, string(message))
}

func TestDLogPayloadByteTeeCapabilityAndFallback(t *testing.T) {
	for _, mode := range []string{"bytes", "legacy", "unsupported"} {
		t.Run(mode, func(t *testing.T) {
			recorder := &byteFileTeeRecorder{behaviorLogger: behaviorLogger{supportsColor: true}}
			var sink loggers.Logger = recorder
			if mode == "legacy" {
				sink = &recorder.behaviorLogger
			}
			if mode == "unsupported" {
				sink = coreOnlyLogger{&recorder.behaviorLogger}
			}
			// Even with colors enabled, a file tee must keep plain payload.
			colorizer := &teeColorizer{}
			d := &DLog{logger: sink, colorsEnabled: true, colorizer: colorizer}
			payload := []byte("plain\x00payload\n")
			d.RawPayloadFileTeeBytes(payload)
			clear(payload)
			want := []string{"plain\x00payload\n"}
			switch mode {
			case "bytes":
				if !reflect.DeepEqual(recorder.bytes, want) || len(recorder.fileOnly) != 0 {
					t.Fatalf("bytes=%q legacy=%q", recorder.bytes, recorder.fileOnly)
				}
			case "legacy":
				if !reflect.DeepEqual(recorder.fileOnly, want) || len(recorder.bytes) != 0 {
					t.Fatalf("bytes=%q legacy=%q", recorder.bytes, recorder.fileOnly)
				}
			case "unsupported":
				if len(recorder.bytes)+len(recorder.fileOnly) != 0 {
					t.Fatal("unsupported file tee called")
				}
			}
			if len(recorder.raws)+len(recorder.logs)+recorder.coloredRaws+colorizer.calls != 0 {
				t.Fatal("file tee used stdout/diagnostic/color path")
			}
		})
	}
}
