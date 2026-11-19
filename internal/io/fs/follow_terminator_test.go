package fs

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

type terminatorCaptureProcessor struct {
	captureProcessor
	preserve bool
}

func (p *terminatorCaptureProcessor) PreserveLineTerminators() bool { return p.preserve }

func TestFollowLineTerminatorProvenance(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, before := range []int{0, 1} {
			t.Run(fmt.Sprintf("preserve=%v/before=%d", preserve, before), func(t *testing.T) {
				processor := &terminatorCaptureProcessor{preserve: preserve}
				filter := newFilteringProcessor(lcontext.LContext{BeforeContext: before}, processor, regex.NewNoop(), &stats{}, "source")
				defer filter.resetGeneration()
				p := followLineProcessor{
					file:   newFollowReadFile("unused", "source", nil, defaultMaxLineLength, testLogger),
					filter: filter, processor: processor, partialLine: new(bytes.Buffer),
				}
				// CRLF straddles read calls; the last bare CR is an unfinished
				// record, and must not be disguised as a CRLF terminator.
				for _, chunk := range []string{"first\r", "\nsecond\n\n", "unfinished\r"} {
					if stop, err := p.processChunk(context.Background(), []byte(chunk)); stop || err != nil {
						t.Fatalf("processChunk: stop=%v err=%v", stop, err)
					}
				}
				if err := p.finish(); err != nil {
					t.Fatal(err)
				}
				want := []string{"first\r", "second", "unfinished\r"}
				if preserve {
					want = []string{"first\r\n", "second\n", "unfinished\r"}
				}
				if !reflect.DeepEqual(processor.lines, want) {
					t.Fatalf("lines=%q, want=%q", processor.lines, want)
				}
			})
		}
	}
}

func TestFollowLengthSplitHasNoSyntheticTerminator(t *testing.T) {
	processor := &terminatorCaptureProcessor{preserve: true}
	p := followLineProcessor{
		file:      newFollowReadFile("unused", "source", nil, 4, testLogger),
		filter:    newFilteringProcessor(lcontext.LContext{}, processor, regex.NewNoop(), &stats{}, "source"),
		processor: processor, partialLine: new(bytes.Buffer),
	}
	// "12345" is one byte above the limit, so "1234" is cut off without a
	// terminator once the next byte shows that the line goes on (a pending
	// line of exactly the limit would wait for its newline instead), and the
	// rest keeps its real CRLF.
	for _, chunk := range []string{"12345", "x\r", "\n"} {
		if stop, err := p.processChunk(context.Background(), []byte(chunk)); stop || err != nil {
			t.Fatalf("processChunk: stop=%v err=%v", stop, err)
		}
	}
	if want := []string{"1234", "5x\r\n"}; !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("lines=%q, want=%q", processor.lines, want)
	}
}

func TestFollowTerminatorKeepsAnchoredRegexAndContext(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, before := range []int{0, 1} {
			t.Run(fmt.Sprintf("preserve=%v/before=%d", preserve, before), func(t *testing.T) {
				re, err := regex.New("hit$", regex.Default)
				if err != nil {
					t.Fatal(err)
				}
				processor := &terminatorCaptureProcessor{preserve: preserve}
				filter := NewLineFilter(lcontext.LContext{BeforeContext: before}, processor, re, "source")
				defer filter.Close()
				for _, text := range []string{"before\n", "hit\n", "after\n"} {
					if stop, err := filter.ProcessFollowLine([]byte(text)); stop || err != nil {
						t.Fatalf("follow filter: stop=%v err=%v", stop, err)
					}
				}
				want := []string{"hit"}
				if before > 0 {
					want = []string{"before", "hit"}
				}
				if preserve {
					for i := range want {
						want[i] += "\n"
					}
				}
				if !reflect.DeepEqual(processor.lines, want) {
					t.Fatalf("lines=%q, want=%q", processor.lines, want)
				}
			})
		}
	}
}
