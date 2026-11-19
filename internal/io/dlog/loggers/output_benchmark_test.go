package loggers

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
)

type outputBenchmarkWriter struct {
	bytes  int64
	buffer []byte
}

func (w *outputBenchmarkWriter) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	copy(w.buffer, p)
	return len(p), nil
}

func BenchmarkLoggerOutput(b *testing.B) {
	for _, size := range []int{128, 4096, 128 * 1024} {
		for _, kind := range []string{"stdout", "file", "teeOff", "teeOn"} {
			b.Run(fmt.Sprintf("bytes%d/%s", size, kind), func(b *testing.B) {
				benchmarkLoggerOutput(b, size, kind, false)
			})
		}
	}
}

// BenchmarkLoggerOutputCopy also makes the underlying sink consume every
// payload byte. The counting-only variant above isolates pipeline overhead;
// for direct large writes it otherwise measures no data movement at all.
func BenchmarkLoggerOutputCopy(b *testing.B) {
	for _, size := range []int{128, 4096, 128 * 1024} {
		for _, kind := range []string{"stdout", "file", "teeOff", "teeOn"} {
			b.Run(fmt.Sprintf("bytes%d/%s", size, kind), func(b *testing.B) {
				benchmarkLoggerOutput(b, size, kind, true)
			})
		}
	}
}

func benchmarkLoggerOutput(b *testing.B, size int, kind string, copySink bool) {
	stdoutSink, fileSink := &outputBenchmarkWriter{}, &outputBenchmarkWriter{}
	if copySink {
		stdoutSink.buffer = make([]byte, max(size, stdoutWriterBufSize))
		fileSink.buffer = make([]byte, max(size, fileWriterBufSize))
	}
	s := newStdoutWriter(stdoutSink)
	f := newFile(Strategy{Rotation: SignalRotation, FileBase: "bench"}, "")
	f.lastFileName = "bench"
	f.writer = bufio.NewWriterSize(fileSink, fileWriterBufSize)
	var logger Logger = s
	switch kind {
	case "file":
		logger = f
	case "teeOff", "teeOn":
		logger = newFoutWithSinks(f, s, kind == "teeOn")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	startLogger(ctx, &wg, logger)
	raw := bytes.Repeat([]byte("x"), size)
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		WriteRawBytes(logger, raw)
	}
	logger.Flush()
	b.StopTimer()
	cancel()
	wg.Wait()
	want := int64(size) * int64(b.N)
	if kind == "file" {
		if fileSink.bytes != want || stdoutSink.bytes != 0 {
			b.Fatal("file-only output mismatch")
		}
		return
	}
	if stdoutSink.bytes != want {
		b.Fatal("stdout output mismatch")
	}
	if kind == "teeOn" && fileSink.bytes != want || kind != "teeOn" && fileSink.bytes != 0 {
		b.Fatal("payload tee output mismatch")
	}
}
