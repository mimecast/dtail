package fs

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type filterBenchmarkSink struct{ bytes int }
type rawFilterBenchmarkSink struct{ *filterBenchmarkSink }

func (s *filterBenchmarkSink) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	s.bytes += buf.Len()
	pool.RecycleBytesBuffer(buf)
	return nil
}
func (*filterBenchmarkSink) Flush() error { return nil }
func (*filterBenchmarkSink) Close() error { return nil }
func (s *rawFilterBenchmarkSink) ProcessRawLine(raw []byte, _ uint64, _ string) error {
	s.bytes += len(raw)
	return nil
}

func BenchmarkContextFilter(b *testing.B) {
	contexts := []struct {
		name string
		ltx  lcontext.LContext
	}{
		{"none", lcontext.LContext{}},
		{"max", lcontext.LContext{MaxCount: int(^uint(0) >> 1)}},
		{"after", lcontext.LContext{AfterContext: 5}},
		{"before", lcontext.LContext{BeforeContext: 5}},
	}
	for _, size := range []int{128, 4096} {
		for _, ctx := range contexts {
			for _, every := range []int{0, 100, 1} {
				for _, invert := range []bool{false, true} {
					for _, raw := range []bool{false, true} {
						name := fmt.Sprintf("bytes%d/%s/hitEvery%d/invert%v/raw%v", size, ctx.name, every, invert, raw)
						b.Run(name, func(b *testing.B) { benchmarkContextFilter(b, size, ctx.ltx, every, invert, raw) })
					}
				}
			}
		}
	}
}

func benchmarkContextFilter(b *testing.B, size int, ltx lcontext.LContext, every int, invert, raw bool) {
	owned := &filterBenchmarkSink{}
	var sink line.Processor = owned
	if raw {
		sink = &rawFilterBenchmarkSink{owned}
	}
	flag := regex.Default
	if invert {
		flag = regex.Invert
	}
	re, err := regex.New("ERROR", flag)
	if err != nil {
		b.Fatal(err)
	}
	f := NewLineFilter(ltx, sink, re, "bench")
	defer f.Close()
	miss := bytes.Repeat([]byte("x"), size)
	hit := bytes.Clone(miss)
	copy(hit, "ERROR")
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data := miss
		if every > 0 && i%every == 0 {
			data = hit
		}
		if stop, err := f.ProcessLine(data); stop || err != nil {
			b.Fatalf("stop=%v err=%v", stop, err)
		}
	}
	b.StopTimer()
	if owned.bytes%size != 0 {
		b.Fatal("partial line emitted")
	}
}
