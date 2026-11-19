package fs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeSampleFile writes content to name in a temporary directory and
// returns its path and validated target.
func writeSampleFile(t *testing.T, name string, content []byte) (string, ValidatedReadTarget) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err = os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	target, err := NewValidatedReadTarget(path)
	if err != nil {
		t.Fatalf("validate %s: %v", path, err)
	}
	return path, target
}

// numberedLines returns the lines "line-<from>\n" to "line-<to>\n", 9 bytes
// each.
func numberedLines(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "line-%03d\n", i)
	}
	return b.String()
}

// numberedSample returns numberedLines as sampled lines.
func numberedSample(from, to int) []string {
	return strings.Split(strings.TrimSuffix(numberedLines(from, to), "\n"), "\n")
}

// sampleCase is a sample read of content and the sample it must yield.
type sampleCase struct {
	name    string
	content string
	options SampleOptions
	want    Sample
}

// assertSample compares got with want, treating nil and empty lines alike.
func assertSample(t *testing.T, got, want Sample) {
	t.Helper()
	if len(got.Lines) == 0 && len(want.Lines) == 0 {
		got.Lines, want.Lines = nil, nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sample = %#v, want %#v", got, want)
	}
}

func tailSampleCases() []sampleCase {
	return []sampleCase{
		{name: "empty file", content: ""},
		{
			name:    "file smaller than the window",
			content: "a\nb\r\nc\rx\nlast without newline",
			want:    Sample{Lines: []string{"a", "b", "c\rx", "last without newline"}},
		},
		{
			// 20 lines of 9 bytes; a window of 40 bytes starts 4 bytes into
			// line 16, so that partial line is dropped.
			name:    "partial first line dropped",
			content: numberedLines(1, 20),
			options: SampleOptions{WindowBytes: 40},
			want:    Sample{Lines: numberedSample(17, 20), FirstLine: "line-001", HasFirstLine: true},
		},
		{
			// A window of 36 bytes starts exactly at line 17.
			name:    "window starting at a line start keeps it",
			content: numberedLines(1, 20),
			options: SampleOptions{WindowBytes: 36},
			want:    Sample{Lines: numberedSample(17, 20), FirstLine: "line-001", HasFirstLine: true},
		},
		{
			name:    "window starting right after a line start drops it",
			content: numberedLines(1, 20),
			options: SampleOptions{WindowBytes: 35},
			want:    Sample{Lines: numberedSample(18, 20), FirstLine: "line-001", HasFirstLine: true},
		},
		{
			// The window holds the whole file, but MaxLines evicts its first
			// line, which is returned separately then (a csv header of a
			// file of many short lines).
			name:    "at most MaxLines, the last ones",
			content: numberedLines(1, 20),
			options: SampleOptions{MaxLines: 3},
			want:    Sample{Lines: numberedSample(18, 20), FirstLine: "line-001", HasFirstLine: true},
		},
		{
			// The window holds the whole file and every line is kept: the
			// first line is a sampled line, not a separate one.
			name:    "whole file within the limits",
			content: numberedLines(1, 3),
			options: SampleOptions{MaxLines: 3},
			want:    Sample{Lines: numberedSample(1, 3)},
		},
		{
			name:    "long lines cut at MaxLineLength",
			content: "short\n" + strings.Repeat("x", 25) + "\n",
			options: SampleOptions{MaxLineLength: 10},
			want:    Sample{Lines: []string{"short", strings.Repeat("x", 10), strings.Repeat("x", 10), "xxxxx"}},
		},
		{
			name:    "first line cut at MaxLineLength",
			content: "h,e,a,d,e,r\n" + numberedLines(1, 20),
			options: SampleOptions{WindowBytes: 18, MaxLineLength: 5},
			want: Sample{Lines: []string{"line-", "019", "line-", "020"},
				FirstLine: "h,e,a", HasFirstLine: true},
		},
		{
			name:    "line longer than the window yields nothing but the first line",
			content: strings.Repeat("y", 100),
			options: SampleOptions{WindowBytes: 50},
			want:    Sample{FirstLine: strings.Repeat("y", 50), HasFirstLine: true},
		},
		{
			// A line between WindowBytes and MaxLineLength is not cut; it
			// does not fit into the window, so the window starts inside it
			// and it is dropped as a partial line.
			name:    "line between WindowBytes and MaxLineLength dropped",
			content: "short\n" + strings.Repeat("x", 100) + "\nend\n",
			options: SampleOptions{WindowBytes: 64, MaxLineLength: 200},
			want:    Sample{Lines: []string{"end"}, FirstLine: "short", HasFirstLine: true},
		},
		{
			name:    "empty lines are lines",
			content: "\n\nz\n",
			want:    Sample{Lines: []string{"", "", "z"}},
		},
	}
}

func TestSampleFileTail(t *testing.T) {
	for _, tt := range tailSampleCases() {
		t.Run(tt.name, func(t *testing.T) {
			path, target := writeSampleFile(t, "sample.log", []byte(tt.content))
			got, err := SampleFile(context.Background(), target, path, tt.options)
			if err != nil {
				t.Fatalf("SampleFile: %v", err)
			}
			assertSample(t, got, tt.want)
		})
	}
}

func TestSampleFileTailDefaultWindow(t *testing.T) {
	// 100,000 lines of 8 bytes: far more than 256 KiB and 500 lines.
	var b strings.Builder
	for i := 1; i <= 100000; i++ {
		fmt.Fprintf(&b, "n%06d\n", i)
	}
	path, target := writeSampleFile(t, "big.log", []byte(b.String()))
	got, err := SampleFile(context.Background(), target, path, SampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Lines) != DefaultSampleMaxLines {
		t.Fatalf("got %d lines, want %d", len(got.Lines), DefaultSampleMaxLines)
	}
	if got.Lines[len(got.Lines)-1] != "n100000" || got.Lines[0] != "n099501" {
		t.Fatalf("sample spans %q..%q, want n099501..n100000", got.Lines[0], got.Lines[len(got.Lines)-1])
	}
	if !got.HasFirstLine || got.FirstLine != "n000001" {
		t.Fatalf("first line = %q (%v), want n000001", got.FirstLine, got.HasFirstLine)
	}
}

// TestSampleFileTailDefaultWindowBytes checks the default byte window
// (DefaultSampleWindowBytes, 256 KiB), which TestSampleFileTailDefaultWindow
// cannot see as its 500 short lines fit far below it: 1,000 lines of 1 KiB
// each, so the last 256 KiB hold exactly the last 256 lines.
func TestSampleFileTailDefaultWindowBytes(t *testing.T) {
	const lineBytes, lines = 1024, 1000
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&b, "n%04d%s\n", i, strings.Repeat("x", lineBytes-len("n0000\n")))
	}
	path, target := writeSampleFile(t, "wide.log", []byte(b.String()))
	got, err := SampleFile(context.Background(), target, path, SampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := 256 * 1024 / lineBytes; len(got.Lines) != want {
		t.Fatalf("got %d lines, want the %d lines of the last 256 KiB", len(got.Lines), want)
	}
	if first := got.Lines[0][:len("n0000")]; first != "n0745" {
		t.Fatalf("sample starts at %q, want n0745", first)
	}
	if last := got.Lines[len(got.Lines)-1][:len("n0000")]; last != "n1000" {
		t.Fatalf("sample ends at %q, want n1000", last)
	}
}

func TestSampleTailKeepsFileOffset(t *testing.T) {
	_, target := writeSampleFile(t, "offset.log", []byte(numberedLines(1, 5)))
	fd, err := target.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()
	if _, err := SampleTail(context.Background(), fd, SampleOptions{WindowBytes: 10}); err != nil {
		t.Fatal(err)
	}
	if offset, err := fd.Seek(0, 1); err != nil || offset != 0 {
		t.Fatalf("offset after SampleTail = %d, %v; want 0", offset, err)
	}
}

// gzipped returns the members compressed as gzip members, one after another.
func gzipped(t *testing.T, members ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, member := range members {
		w := gzip.NewWriter(&buf)
		if _, err := w.Write([]byte(member)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func gzipSampleCases(t *testing.T) []sampleCase {
	return []sampleCase{
		{
			name:    "first MaxLines lines",
			content: string(gzipped(t, numberedLines(1, 20))),
			options: SampleOptions{MaxLines: 4},
			want:    Sample{Lines: numberedSample(1, 4)},
		},
		{
			name:    "lines across gzip members",
			content: string(gzipped(t, numberedLines(1, 2), numberedLines(3, 4))),
			want:    Sample{Lines: numberedSample(1, 4)},
		},
		{
			// 4 lines of 8 bytes of line data fit into 35 bytes, a 5th not.
			name:    "at most WindowBytes of line data",
			content: string(gzipped(t, numberedLines(1, 20))),
			options: SampleOptions{WindowBytes: 35},
			want:    Sample{Lines: numberedSample(1, 4)},
		},
		{
			name:    "long lines cut",
			content: string(gzipped(t, strings.Repeat("z", 12)+"\n")),
			options: SampleOptions{MaxLineLength: 5},
			want:    Sample{Lines: []string{"zzzzz", "zzzzz", "zz"}},
		},
		{
			// The oversized first line is skipped, the short ones after it
			// are sampled.
			name:    "lines after an oversized first line",
			content: string(gzipped(t, strings.Repeat("o", 100)+"\n"+numberedLines(1, 3))),
			options: SampleOptions{WindowBytes: 64},
			want:    Sample{Lines: numberedSample(1, 3)},
		},
		{
			// Three oversized lines (303 bytes with their newlines) cross the
			// scan bound of 4 x 64 bytes: the short line after them is not
			// scanned any more.
			name:    "oversized lines up to the scan bound end the sample",
			content: string(gzipped(t, strings.Repeat(strings.Repeat("o", 100)+"\n", 3)+numberedLines(1, 3))),
			options: SampleOptions{WindowBytes: 64},
			want:    Sample{},
		},
		{
			// A line between WindowBytes and MaxLineLength is not cut, and
			// as it is larger than WindowBytes it is dropped.
			name:    "line between WindowBytes and MaxLineLength dropped",
			content: string(gzipped(t, "short\n"+strings.Repeat("x", 100)+"\nend\n")),
			options: SampleOptions{WindowBytes: 64, MaxLineLength: 200},
			want:    Sample{Lines: []string{"short", "end"}},
		},
		{
			name:    "default limits: the first 500 lines",
			content: string(gzipped(t, numberedLines(1, 2000))),
			want:    Sample{Lines: numberedSample(1, 500)},
		},
		{name: "empty file", content: ""},
	}
}

func TestSampleFileGzipHead(t *testing.T) {
	for _, tt := range gzipSampleCases(t) {
		t.Run(tt.name, func(t *testing.T) {
			path, target := writeSampleFile(t, "sample.log.gz", []byte(tt.content))
			got, err := SampleFile(context.Background(), target, path, tt.options)
			if err != nil {
				t.Fatalf("SampleFile: %v", err)
			}
			assertSample(t, got, tt.want)
		})
	}
}

func TestSampleFileFailures(t *testing.T) {
	t.Run("corrupt gzip", func(t *testing.T) {
		path, target := writeSampleFile(t, "corrupt.gz", []byte("not gzip at all\n"))
		if _, err := SampleFile(context.Background(), target, path, SampleOptions{}); err == nil {
			t.Fatal("expected an error for a corrupt gzip file")
		}
	})
	t.Run("journal target", func(t *testing.T) {
		target, err := NewValidatedJournalTarget("journal:ssh.service")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SampleFile(context.Background(), target, "journal:ssh.service", SampleOptions{}); err == nil {
			t.Fatal("expected an error for a journal target")
		}
	})
	t.Run("removed file", func(t *testing.T) {
		path, target := writeSampleFile(t, "gone.log", []byte("x\n"))
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if _, err := SampleFile(context.Background(), target, path, SampleOptions{}); err == nil {
			t.Fatal("expected an error for a removed file")
		}
	})
}

func TestSampleCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plainPath, plain := writeSampleFile(t, "plain.log", []byte(numberedLines(1, 5)))
	gzPath, gz := writeSampleFile(t, "plain.log.gz", gzipped(t, numberedLines(1, 5)))
	fd, err := plain.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fd.Close() }()

	for name, run := range map[string]func() (Sample, error){
		"SampleFile plain": func() (Sample, error) { return SampleFile(ctx, plain, plainPath, SampleOptions{}) },
		"SampleFile gzip":  func() (Sample, error) { return SampleFile(ctx, gz, gzPath, SampleOptions{}) },
		"SampleTail":       func() (Sample, error) { return SampleTail(ctx, fd, SampleOptions{}) },
		"SampleHead": func() (Sample, error) {
			return SampleHead(ctx, strings.NewReader("a\nb\n"), SampleOptions{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			sample, err := run()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if !reflect.DeepEqual(sample, Sample{}) {
				t.Fatalf("sample = %#v, want none", sample)
			}
		})
	}
}

// cancelingReader yields one line per Read and cancels its context after
// the first one, as if the sample was canceled while it read.
type cancelingReader struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 2 {
		r.cancel()
	}
	return copy(p, "line\n"), nil
}

func TestSampleHeadCanceledWhileReading(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &cancelingReader{cancel: cancel}
	// The reader never ends; only the cancellation ends the sample before
	// it is full.
	sample, err := SampleHead(ctx, reader, SampleOptions{MaxLines: 1000})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(sample, Sample{}) {
		t.Fatalf("sample = %#v, want none", sample)
	}
	if reader.reads > 3 {
		t.Fatalf("read %d times after the cancellation", reader.reads)
	}
}

// endlessReader repeats line forever, at most chunk bytes per Read, and
// counts the bytes it handed out.
type endlessReader struct {
	line  []byte
	chunk int
	pos   int
	read  int64
}

func (r *endlessReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) && n < r.chunk {
		c := copy(p[n:min(len(p), r.chunk)], r.line[r.pos:])
		n += c
		r.pos = (r.pos + c) % len(r.line)
	}
	r.read += int64(n)
	return n, nil
}

// TestSampleHeadBoundsScannedInput feeds SampleHead endless input of nothing
// but oversized lines (or MaxLineLength pieces), which it skips: it must
// still return, without cancellation, once it scanned 4 x WindowBytes.
func TestSampleHeadBoundsScannedInput(t *testing.T) {
	const window, chunk = 1024, 512
	tests := []struct {
		name          string
		line          string
		maxLineLength int
	}{
		{name: "endless line cut into pieces", line: "x", maxLineLength: 2048},
		{name: "oversized terminated lines", line: strings.Repeat("y", 1500) + "\n"},
		{name: "default MaxLineLength", line: "z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &endlessReader{line: []byte(tt.line), chunk: chunk}
			done := make(chan Sample, 1)
			go func() {
				sample, err := SampleHead(context.Background(), reader,
					SampleOptions{WindowBytes: window, MaxLineLength: tt.maxLineLength})
				if err != nil {
					t.Errorf("SampleHead: %v", err)
				}
				done <- sample
			}()
			var sample Sample
			select {
			case sample = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("SampleHead did not stop on input of oversized lines only")
			}
			if len(sample.Lines) != 0 {
				t.Fatalf("sample = %q, want no line", sample.Lines)
			}
			// It scans up to the bound, plus the rest of the token crossing
			// it and one read ahead.
			bound := int64(SampleHeadScanFactor * window)
			token := int64(LineLimit(tt.maxLineLength) + 1)
			if reader.read < bound || reader.read > bound+token+chunk {
				t.Fatalf("read %d bytes, want between %d and %d", reader.read, bound, bound+token+chunk)
			}
		})
	}
}

// TestSampleFileGzipOfOversizedLinesOnly samples a gzip file decompressing
// to 16 MiB without a newline: it stops after the scan bound and returns
// no line, instead of decompressing it to the end.
func TestSampleFileGzipOfOversizedLinesOnly(t *testing.T) {
	content := gzipped(t, strings.Repeat("b", 16<<20))
	path, target := writeSampleFile(t, "blob.gz", content)
	got, err := SampleFile(context.Background(), target, path,
		SampleOptions{WindowBytes: 1024, MaxLineLength: 2048})
	if err != nil {
		t.Fatalf("SampleFile: %v", err)
	}
	assertSample(t, got, Sample{})
}

func TestSampleCollector(t *testing.T) {
	t.Run("keep last evicts by count and bytes", func(t *testing.T) {
		c := NewSampleCollector(SampleOptions{MaxLines: 3, WindowBytes: 6}, true)
		for _, line := range []string{"a\n", "bb\n", "cc\n", "dd\n"} {
			if !c.Add([]byte(line)) {
				t.Fatalf("keep-last collector refused %q", line)
			}
		}
		if got, want := c.Lines(), []string{"bb", "cc", "dd"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
		c.Add([]byte("eeee\n"))
		if got, want := c.Lines(), []string{"dd", "eeee"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	})
	t.Run("keep last skips an oversized line and keeps the others", func(t *testing.T) {
		// 499 lines of 4 bytes fill 1996 of 2048 bytes; the oversized newest
		// line must neither evict them nor be kept.
		c := NewSampleCollector(SampleOptions{WindowBytes: 2048}, true)
		want := make([]string, 0, 499)
		for i := 0; i < 499; i++ {
			line := fmt.Sprintf("%04d", i)
			want = append(want, line)
			if !c.Add([]byte(line + "\n")) {
				t.Fatalf("keep-last collector refused %q", line)
			}
		}
		if !c.Add([]byte(strings.Repeat("j", 4096) + "\n")) {
			t.Fatal("keep-last collector refused the oversized line")
		}
		if got := c.Lines(); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %d lines after an oversized line, want the 499 before it", len(got))
		}
		c.Add([]byte("next\n"))
		if got := c.Lines(); len(got) != 500 || got[499] != "next" || got[0] != "0000" {
			t.Fatalf("lines after the oversized one: %d, last %q", len(got), got[len(got)-1])
		}
	})
	t.Run("keep first skips an oversized line and goes on", func(t *testing.T) {
		c := NewSampleCollector(SampleOptions{WindowBytes: 4}, false)
		if !c.Add([]byte("toolong\n")) {
			t.Fatal("keep-first collector stopped at an oversized line")
		}
		if !c.Add([]byte("ab\n")) || !c.Add([]byte("cd\n")) {
			t.Fatal("keep-first collector refused lines that fit")
		}
		if c.Add([]byte("e\n")) {
			t.Fatal("keep-first collector accepted a line past its byte limit")
		}
		if got, want := c.Lines(), []string{"ab", "cd"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	})
	t.Run("keep last stays bounded over many lines", func(t *testing.T) {
		c := NewSampleCollector(SampleOptions{MaxLines: 2}, true)
		for i := 0; i < 1000; i++ {
			c.Add([]byte(fmt.Sprintf("%d\n", i)))
		}
		if cap(c.lines) > 16 {
			t.Fatalf("backing array grew to %d entries for 2 kept lines", cap(c.lines))
		}
		if got, want := c.Lines(), []string{"998", "999"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	})
	t.Run("keep first reports full", func(t *testing.T) {
		c := NewSampleCollector(SampleOptions{MaxLines: 2}, false)
		if !c.Add([]byte("a\n")) || !c.Add([]byte("b\n")) {
			t.Fatal("collector refused a line it had room for")
		}
		if c.Add([]byte("c\n")) {
			t.Fatal("full collector accepted a line")
		}
		if got, want := c.Lines(), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("lines = %q, want %q", got, want)
		}
	})
	t.Run("defaults for unset limits", func(t *testing.T) {
		got := SampleOptions{WindowBytes: -1}.WithDefaults()
		if got.WindowBytes != DefaultSampleWindowBytes || got.MaxLines != DefaultSampleMaxLines {
			t.Fatalf("defaults = %+v", got)
		}
	})
}
