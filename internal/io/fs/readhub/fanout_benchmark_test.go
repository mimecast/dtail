package readhub

import (
	"bytes"
	"testing"
)

// Keeping the last chunk alive models a subscriber owning it past publish.
// These benchmarks isolate packing/allocation, not fan-out/network costs.
type benchmarkPublisher struct {
	last *chunk
}

func (p *benchmarkPublisher) publish(it item) { p.last = it.chunk }

func BenchmarkFanoutChunks(b *testing.B) {
	for _, tc := range []struct {
		name  string
		lines int
		size  int
	}{
		{"follow_sparse", 1, 128},
		{"follow_4KiB", 32, 128},
		{"follow_64KiB", 512, 128},
		{"follow_long", 1, 128 * 1024},
		{"snapshot_512KiB", 4096, 128},
		{"snapshot_short", 65536, 1},
	} {
		b.Run(tc.name, func(b *testing.B) {
			pub := &benchmarkPublisher{}
			p := newFanoutProcessor(pub)
			raw := bytes.Repeat([]byte("x"), tc.size)
			b.ReportAllocs()
			b.SetBytes(int64(tc.lines * tc.size))
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				for i := 0; i < tc.lines; i++ {
					if err := p.ProcessRawLine(raw, 0, ""); err != nil {
						b.Fatal(err)
					}
				}
				if err := p.Flush(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if pub.last == nil || len(pub.last.data) == 0 {
				b.Fatal("no payload published")
			}
		})
	}
}

func BenchmarkFanoutChangingReadSize(b *testing.B) {
	pub := &benchmarkPublisher{}
	p := newFanoutProcessor(pub)
	raw := bytes.Repeat([]byte("x"), 128)
	b.ReportAllocs()
	b.SetBytes(513 * 128)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for _, lines := range []int{512, 1} {
			for i := 0; i < lines; i++ {
				if err := p.ProcessRawLine(raw, 0, ""); err != nil {
					b.Fatal(err)
				}
			}
			if err := p.Flush(); err != nil {
				b.Fatal(err)
			}
		}
	}
}
