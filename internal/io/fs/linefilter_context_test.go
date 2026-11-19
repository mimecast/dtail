package fs

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type contextFailureProcessor struct {
	retainingProcessor
	err    error
	panics bool
}

type rawContextFailureProcessor struct{ *contextFailureProcessor }

type contextOwnershipProcessor struct {
	counts map[*bytes.Buffer]int
	lines  []string
	nums   []uint64
	failAt int
	panics bool
}

func (p *contextFailureProcessor) ProcessLine(buf *bytes.Buffer, _ uint64, _ string) error {
	p.bufs = append(p.bufs, buf)
	if p.panics {
		panic("context sink")
	}
	return p.err
}

func (p *rawContextFailureProcessor) ProcessRawLine(_ []byte, _ uint64, _ string) error {
	if p.panics {
		panic("context sink")
	}
	return p.err
}

func (p *contextOwnershipProcessor) ProcessLine(buf *bytes.Buffer, num uint64, _ string) error {
	p.lines = append(p.lines, buf.String())
	p.nums = append(p.nums, num)
	p.counts[buf]++
	buf.Reset() // Deliberately do not return to the pool: keep identities distinct.
	if len(p.lines) == p.failAt {
		if p.panics {
			panic("ring sink")
		}
		return errors.New("ring sink")
	}
	return nil
}

func (*contextOwnershipProcessor) Flush() error { return nil }
func (*contextOwnershipProcessor) Close() error { return nil }

// contextReference computes the selected index ranges, independently of the
// streaming filter. Max+after keeps scanning until the next match, as before.
func contextReference(input []string, ltx lcontext.LContext, invert bool) ([]string, []uint64, int) {
	var matches []int
	end, stop := len(input), -1
	for i, s := range input {
		if strings.Contains(s, "HIT") == invert {
			continue
		}
		if ltx.MaxCount > 0 && len(matches) == ltx.MaxCount {
			end, stop = i, i
			break
		}
		matches = append(matches, i)
		if ltx.MaxCount > 0 && len(matches) == ltx.MaxCount && ltx.AfterContext == 0 {
			end, stop = i+1, i
			break
		}
	}
	selected := make([]bool, len(input))
	for _, i := range matches {
		for j := max(0, i-ltx.BeforeContext); j < min(end, i+ltx.AfterContext+1); j++ {
			selected[j] = true
		}
	}
	var lines []string
	var nums []uint64
	for i, yes := range selected {
		if yes {
			lines = append(lines, input[i])
			nums = append(nums, uint64(i+1))
		}
	}
	return lines, nums, stop
}

func TestContextFilterMatchesReference(t *testing.T) {
	contexts := []lcontext.LContext{{}, {MaxCount: 2}, {AfterContext: 2},
		{MaxCount: 2, AfterContext: 2}, {BeforeContext: 3},
		{BeforeContext: 3, AfterContext: 2}, {BeforeContext: 3, MaxCount: 2},
		{BeforeContext: 3, AfterContext: 2, MaxCount: 2}}
	for _, ltx := range contexts {
		for _, invert := range []bool{false, true} {
			t.Run(fmt.Sprintf("%+v/invert%v", ltx, invert), func(t *testing.T) {
				flag := regex.Default
				if invert {
					flag = regex.Invert
				}
				re := mustFlaggedRegex(t, "HIT", flag)
				// All match patterns across eight lines include overlapping
				// contexts, full ring wraps, empty input lines and final matches.
				for mask := 0; mask < 256; mask++ {
					input := make([]string, 8)
					for i := range input {
						if mask&(1<<i) != 0 {
							input[i] = fmt.Sprintf("HIT %d", i)
						}
					}
					want, nums, stop := contextReference(input, ltx, invert)
					for _, raw := range []bool{false, true} {
						verifyContextSequence(t, ltx, re, input, raw, want, nums, stop)
					}
				}
			})
		}
	}
}

func verifyContextSequence(t *testing.T, ltx lcontext.LContext, re regex.Regex, input []string,
	raw bool, want []string, nums []uint64, wantStop int) {
	t.Helper()
	owned := &captureProcessor{}
	borrowed := &filterCaptureProcessor{}
	var p line.Processor = owned
	if raw {
		p = borrowed
		owned = &borrowed.captureProcessor
	}
	f := NewLineFilter(ltx, p, re, "src")
	defer f.Close()
	stopAt := -1
	var buf []byte
	for i, s := range input {
		buf = append(buf[:0], s...)
		stop, err := f.ProcessLine(buf)
		if err != nil {
			t.Fatal(err)
		}
		// Reuse/overwrite the caller's input after every call.
		for j := range buf {
			buf[j] = 'X'
		}
		if stop {
			stopAt = i
			break
		}
	}
	if stopAt != wantStop || !reflect.DeepEqual(owned.lines, want) || !reflect.DeepEqual(owned.lineNums, nums) {
		t.Fatalf("raw=%v input=%q: got %q %v stop%d, want %q %v stop%d",
			raw, input, owned.lines, owned.lineNums, stopAt, want, nums, wantStop)
	}
}

func TestContextRawFailureOwnership(t *testing.T) {
	for _, raw := range []bool{false, true} {
		for _, after := range []bool{false, true} {
			for _, panics := range []bool{false, true} {
				t.Run(fmt.Sprintf("raw%v/after%v/panic%v", raw, after, panics), func(t *testing.T) {
					checkContextFailure(t, raw, after, panics)
				})
			}
		}
	}
}

func checkContextFailure(t *testing.T, raw, after, panics bool) {
	t.Helper()
	p := &contextFailureProcessor{}
	defer func() {
		for _, buf := range p.bufs {
			pool.RecycleBytesBuffer(buf)
		}
	}()
	var sink line.Processor = p
	if raw {
		sink = &rawContextFailureProcessor{p}
	}
	f := NewLineFilter(lcontext.LContext{MaxCount: 2, AfterContext: 1}, sink, mustRegex(t, "HIT"), "src")
	defer f.Close()
	if after {
		if _, err := f.ProcessLine([]byte("HIT prime")); err != nil {
			t.Fatal(err)
		}
	}
	p.err, p.panics = errors.New("sink failed"), panics
	text := "HIT failing"
	if after {
		text = "tail failing"
	}
	func() {
		defer func() {
			if r := recover(); (r != nil) != panics || (panics && r != "context sink") {
				t.Fatalf("panic = %v, want panic %v", r, panics)
			}
		}()
		stop, err := f.ProcessLine([]byte(text))
		if panics {
			t.Fatal("expected panic")
		}
		if stop || !errors.Is(err, p.err) {
			t.Fatalf("got %v, %v", stop, err)
		}
	}()
	if !raw && p.bufs[len(p.bufs)-1].String() != text {
		t.Fatal("transferred input recycled by filter")
	}
	wantMatches := 0
	if after {
		wantMatches = 1
	}
	if f.filter.maxCount != wantMatches {
		t.Fatal("failed emission advanced max-count")
	}
}

func TestBeforeRingOwnershipOnDrainFailure(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			p := &contextOwnershipProcessor{counts: make(map[*bytes.Buffer]int)}
			if outcome != "success" {
				p.failAt = 2
				p.panics = outcome == "panic"
			}
			f := NewLineFilter(lcontext.LContext{BeforeContext: 3}, p, mustRegex(t, "HIT"), "src")
			f.filter.recycle = func(buf *bytes.Buffer) { p.counts[buf]++; buf.Reset() }
			for i := 1; i <= 8; i++ {
				if _, err := f.ProcessLine([]byte(fmt.Sprint(i))); err != nil {
					t.Fatal(err)
				}
			}
			func() {
				defer f.Close()
				defer func() {
					if r := recover(); (r != nil) != p.panics {
						t.Fatalf("panic = %v", r)
					}
				}()
				_, err := f.ProcessLine([]byte("HIT"))
				if (err != nil) != (outcome == "error") {
					t.Fatalf("error = %v", err)
				}
			}()
			f.Close() // Idempotent cleanup must not return any buffer twice.
			if len(p.counts) != 9 {
				t.Fatalf("released %d distinct buffers, want9", len(p.counts))
			}
			for _, n := range p.counts {
				if n != 1 {
					t.Fatalf("buffer released %d times", n)
				}
			}
			want := []string{"6", "7", "8", "HIT"}
			if outcome != "success" {
				want = want[:2]
			}
			if !reflect.DeepEqual(p.lines, want) {
				t.Fatalf("lines %q, want %q", p.lines, want)
			}
		})
	}
}

func TestMaxAfterRetainingProcessorGetsOwnedLongLines(t *testing.T) {
	p := &retainingProcessor{}
	f := NewLineFilter(lcontext.LContext{MaxCount: 2, AfterContext: 1}, p, mustRegex(t, "HIT"), "src")
	defer f.Close()
	defer func() {
		for _, buf := range p.bufs {
			pool.RecycleBytesBuffer(buf)
		}
	}()
	raw := bytes.Repeat([]byte("x"), 128*1024)
	copy(raw, "HIT")
	for i := 0; i < 2; i++ {
		if _, err := f.ProcessLine(raw); err != nil {
			t.Fatal(err)
		}
		copy(raw, "zzz")
	}
	if len(p.bufs) != 2 || !strings.HasPrefix(p.bufs[0].String(), "HIT") || !strings.HasPrefix(p.bufs[1].String(), "zzz") {
		t.Fatal("owned copies alias caller's storage")
	}
}

func TestMaxAfterBorrowsAndRejectsWithoutBuffers(t *testing.T) {
	p := &rawCaptureProcessor{}
	f := NewLineFilter(lcontext.LContext{MaxCount: 1, AfterContext: 2}, p, mustRegex(t, "HIT"), "src")
	defer f.Close()
	f.filter.recycle = func(buf *bytes.Buffer) {
		pool.RecycleBytesBuffer(buf)
		t.Error("max/after borrowed path acquired a discarded buffer")
	}
	for i, text := range []string{"skip", "HIT", "after1", "after2", "skip", "HIT stop"} {
		raw := []byte(text)
		stop, err := f.ProcessLine(raw)
		if err != nil || stop != (i == 5) {
			t.Fatalf("line%d: stop%v err%v", i, stop, err)
		}
		if i >= 1 && i <= 3 && &p.lastRaw[0] != &raw[0] {
			t.Fatal("emitted line was copied")
		}
	}
	if p.bufferCalls != 0 || p.rawCalls != 3 || !reflect.DeepEqual(p.lineNums, []uint64{2, 3, 4}) {
		t.Fatalf("owned/raw=%d/%d numbers=%v", p.bufferCalls, p.rawCalls, p.lineNums)
	}
}
