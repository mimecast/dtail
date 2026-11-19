package fs

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// rareTableContent returns lines "common-<i>" with a "rare-<i>" line every
// rareEvery lines, each line 16 bytes, numbered 1 to lines.
func rareTableContent(lines, rareEvery int) string {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		kind := "common"
		if i%rareEvery == 0 {
			kind = "rare__"
		}
		fmt.Fprintf(&b, "%s-%08d\n", kind, i)
	}
	return b.String()
}

func keepRare(line []byte) bool { return bytes.HasPrefix(line, []byte("rare__")) }

// TestSampleTailKeepAndScanBytes samples a rare kind of line from a window
// (ScanBytes) far larger than the line data kept (WindowBytes): only kept
// lines count towards MaxLines and WindowBytes, the window bounds the scan.
func TestSampleTailKeepAndScanBytes(t *testing.T) {
	// 100,000 lines of 16 bytes (1.6 MB), every 1,000th rare.
	path, target := writeSampleFile(t, "rare.log", []byte(rareTableContent(100000, 1000)))
	t.Run("default window misses the rare lines", func(t *testing.T) {
		got, err := SampleFile(context.Background(), target, path,
			SampleOptions{WindowBytes: 8 * 1024, Keep: keepRare})
		if err != nil {
			t.Fatal(err)
		}
		// 512 lines: only the last rare line is in the window.
		if len(got.Lines) != 1 || got.Lines[0] != "rare__-00100000" {
			t.Fatalf("lines = %q from a window of 512 lines, want the last rare line only", got.Lines)
		}
	})
	t.Run("larger scan window finds them", func(t *testing.T) {
		got, err := SampleFile(context.Background(), target, path,
			SampleOptions{WindowBytes: 8 * 1024, ScanBytes: 1 << 20, Keep: keepRare})
		if err != nil {
			t.Fatal(err)
		}
		// The last 1 MiB holds 65,536 lines: lines 34,465 to 100,000, so
		// the rare lines 35,000 to 100,000.
		if len(got.Lines) != 66 {
			t.Fatalf("got %d rare lines, want 66", len(got.Lines))
		}
		if got.Lines[0] != "rare__-00035000" || got.Lines[65] != "rare__-00100000" {
			t.Fatalf("sample spans %q..%q", got.Lines[0], got.Lines[65])
		}
		if !got.HasFirstLine || got.FirstLine != "common-00000001" {
			t.Fatalf("first line = %q (%v)", got.FirstLine, got.HasFirstLine)
		}
	})
	t.Run("kept lines stay within MaxLines", func(t *testing.T) {
		got, err := SampleFile(context.Background(), target, path,
			SampleOptions{ScanBytes: 1 << 30, MaxLines: 10, Keep: keepRare})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 10 || got.Lines[9] != "rare__-00100000" || got.Lines[0] != "rare__-00091000" {
			t.Fatalf("lines = %q, want the last 10 rare lines", got.Lines)
		}
		// The window holds the whole file: its first line is returned on
		// its own, as no kept line is it.
		if !got.HasFirstLine || got.FirstLine != "common-00000001" {
			t.Fatalf("first line = %q (%v)", got.FirstLine, got.HasFirstLine)
		}
	})
}

// TestSampleObserveSeesEveryScannedLine checks that Observe sees every line
// (piece) of the scanned window, also those Keep rejects or the limits
// evict, and none before the window.
func TestSampleObserveSeesEveryScannedLine(t *testing.T) {
	path, target := writeSampleFile(t, "observe.log", []byte(numberedLines(1, 20)))
	var observed []string
	got, err := SampleFile(context.Background(), target, path, SampleOptions{
		WindowBytes: 36, MaxLines: 2,
		Observe: func(line []byte) { observed = append(observed, string(line)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := numberedSample(17, 20); strings.Join(observed, ",") != strings.Join(want, ",") {
		t.Fatalf("observed %q, want %q", observed, want)
	}
	if strings.Join(got.Lines, ",") != "line-019,line-020" {
		t.Fatalf("lines = %q", got.Lines)
	}
}

// TestSampleHeadKeepAndScanBytes checks the scan bound of a head sample
// (a compressed file) with a Keep filter: ScanBytes replaces the default
// bound of SampleHeadScanFactor x WindowBytes.
func TestSampleHeadKeepAndScanBytes(t *testing.T) {
	content := gzipped(t, rareTableContent(100000, 1000))
	path, target := writeSampleFile(t, "rare.log.gz", content)
	tests := []struct {
		name    string
		options SampleOptions
		want    int
	}{
		// 4 x 8 KiB = 2,048 lines scanned: the rare lines 1,000 and 2,000.
		{"default bound", SampleOptions{WindowBytes: 8 * 1024, Keep: keepRare}, 2},
		// 1 MiB: 65,536 lines, 65 rare ones.
		{"scan bytes", SampleOptions{WindowBytes: 8 * 1024, ScanBytes: 1 << 20, Keep: keepRare}, 65},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SampleFile(context.Background(), target, path, tt.options)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Lines) != tt.want || got.Lines[0] != "rare__-00001000" {
				t.Fatalf("got %d lines starting %q, want %d from rare__-00001000",
					len(got.Lines), got.Lines, tt.want)
			}
		})
	}
}

// TestSampleTailLargeWindowStreams makes sure a large scan window does not
// read the whole window into memory: sampling 32 MiB of lines must allocate
// far less than the window.
func TestSampleTailLargeWindowStreams(t *testing.T) {
	path, target := writeSampleFile(t, "large.log", []byte(rareTableContent(2<<20, 100000)))
	options := SampleOptions{ScanBytes: 64 << 20, Keep: keepRare}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := SampleFile(context.Background(), target, path, options)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != 20 {
		t.Fatalf("got %d rare lines, want 20", len(got.Lines))
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 8<<20 {
		t.Fatalf("sampling a 32 MiB window allocated %d bytes, want it streamed", allocated)
	}
}
