package clients

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

type bytePayloadTeeLogger struct {
	payloadTeeLogger
	byteFile bytes.Buffer
}

type failedPayloadWriter struct{ err error }

type copyPayloadTeeLogger struct {
	clientlog.NopLogger
	data  []byte
	count int
}

func (l *bytePayloadTeeLogger) RawPayloadFileTeeBytes(message []byte) {
	_, _ = l.byteFile.Write(message)
}

func (w failedPayloadWriter) Write([]byte) (int, error) { return 0, w.err }

func (l *copyPayloadTeeLogger) RawPayloadFileTeeBytes(message []byte) {
	l.count += copy(l.data, message)
}

func (l *copyPayloadTeeLogger) RawPayloadFileTee(message string) {
	l.count += copy(l.data, message)
}

func TestServerlessByteTeeRoutingAndOwnership(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			var stdout bytes.Buffer
			logger := &bytePayloadTeeLogger{}
			r := newClientRuntimeBoundary(config.RuntimeConfig{
				Client: &config.ClientConfig{LogPayload: enabled},
			}, NewLoggerDependencies(logger, logging.NopLogger{}, logging.NopLogger{}))
			r.stdout = func() io.Writer { return &stdout }
			data := []byte("payload\n")
			n, err := r.serverlessOutputWriter().Write(data)
			clear(data)
			if err != nil || n != len(data) || stdout.String() != "payload\n" {
				t.Fatalf("stdout=%q n=%d err=%v", stdout.String(), n, err)
			}
			want := ""
			if enabled {
				want = "payload\n"
			}
			if logger.byteFile.String() != want || logger.file.Len() != 0 {
				t.Fatalf("byte tee=%q legacy tee=%q", logger.byteFile.String(), logger.file.String())
			}
		})
	}
}

func TestServerlessFailedStdoutDoesNotTee(t *testing.T) {
	logger := &bytePayloadTeeLogger{}
	r := newClientRuntimeBoundary(config.RuntimeConfig{
		Client: &config.ClientConfig{LogPayload: true},
	}, NewLoggerDependencies(logger, logging.NopLogger{}, logging.NopLogger{}))
	want := errors.New("stdout failed")
	r.stdout = func() io.Writer { return failedPayloadWriter{want} }
	n, err := r.serverlessOutputWriter().Write([]byte("payload\n"))
	if n != 0 || !errors.Is(err, want) || logger.byteFile.Len() != 0 || logger.file.Len() != 0 {
		t.Fatalf("n=%d err=%v byte tee=%q legacy=%q", n, err, logger.byteFile.String(), logger.file.String())
	}
}

func BenchmarkServerlessPayloadByteTee(b *testing.B) {
	for _, size := range []int{128, 4096, 65536, 131072} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			data := bytes.Repeat([]byte("x"), size)
			logger := &copyPayloadTeeLogger{data: make([]byte, size)}
			writer := payloadFileTeeWriter{logger: logger}
			b.ReportAllocs()
			b.SetBytes(int64(size))
			b.ResetTimer()
			for b.Loop() {
				if _, err := writer.Write(data); err != nil {
					b.Fatal(err)
				}
			}
			if logger.count != size*b.N || !bytes.Equal(logger.data, data) {
				b.Fatal("byte tee dropped or changed data")
			}
		})
	}
}
