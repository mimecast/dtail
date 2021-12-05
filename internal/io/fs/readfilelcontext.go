package fs

import (
	"bytes"
	"context"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// The local context state.
type ltxState struct {
	// Max state
	maxCount        int
	processMaxCount bool
	maxReached      bool

	// Before state
	before        int
	processBefore bool
	beforeBuf     chan *bytes.Buffer

	// After state
	after        int
	processAfter bool
}

// We don't have any local grep context, which makes life much simpler and more efficient.
func (f *readFile) filterWithoutLContext(ctx context.Context, rawLines <-chan *bytes.Buffer,
	lines chan<- *line.Line, re regex.Regex) {

	for {
		select {
		case rawLine, ok := <-rawLines:
			f.updatePosition()
			if !ok {
				return
			}
			if newLine, ok := f.transmittable(rawLine, len(lines), cap(lines), re); ok {
				select {
				case lines <- newLine:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

// Filter log lines matching a given regular expression, however with local grep context.
func (f *readFile) filterWithLContext(ctx context.Context, ltx lcontext.LContext,
	rawLines <-chan *bytes.Buffer, lines chan<- *line.Line, re regex.Regex) {

	var ls ltxState

	// The following 3 scenarios may also be used at once/any combination together.

	// Scenario 1: Finish once maxCount hits found
	ls.maxCount = ltx.MaxCount
	ls.processMaxCount = ls.maxCount > 0
	ls.maxReached = false

	// Scenario 2: Print prev. N lines when current line matches.
	ls.before = ltx.BeforeContext
	ls.processBefore = ls.before > 0
	if ls.processBefore {
		ls.beforeBuf = make(chan *bytes.Buffer, ls.before)
	}

	// Screnario 3: Print next N lines when current line matches.
	ls.after = 0
	ls.processAfter = ltx.AfterContext > 0

	for rawLine := range rawLines {
		status := f.filterLineWithLContext(ctx, &ltx, &ls, rawLines, lines, &re, rawLine)
		if status == abortReading {
			return
		}
	}
}

// Filter log lines matching a given regular expression, however with local grep context.
func (f *readFile) filterLineWithLContext(ctx context.Context, ltx *lcontext.LContext,
	ls *ltxState, rawLines <-chan *bytes.Buffer, lines chan<- *line.Line, re *regex.Regex,
	rawLine *bytes.Buffer) readStatus {

	f.updatePosition()
	if !re.Match(rawLine.Bytes()) {
		f.updateLineNotMatched()

		if ls.processAfter && ls.after > 0 {
			ls.after--
			myLine := line.New(rawLine, f.totalLineCount(), 100, f.globID)

			select {
			case lines <- myLine:
			case <-ctx.Done():
				return abortReading
			}

		} else if ls.processBefore {
			// Keep last num BeforeContext raw messages.
			select {
			case ls.beforeBuf <- rawLine:
			default:
				pool.RecycleBytesBuffer(<-ls.beforeBuf)
				ls.beforeBuf <- rawLine
			}
		}
		return continueReading
	}

	f.updateLineMatched()

	if ls.processAfter {
		if ls.maxReached {
			return abortReading
		}
		ls.after = ltx.AfterContext
	}

	if ls.processBefore {
		i := uint64(len(ls.beforeBuf))
		for {
			select {
			case rawLine := <-ls.beforeBuf:
				myLine := line.New(rawLine, f.totalLineCount()-i, 100, f.globID)
				i--

				select {
				case lines <- myLine:
				case <-ctx.Done():
					return abortReading
				}
			default:
				// beforeBuf is now empty.
			}
			if len(ls.beforeBuf) == 0 {
				break
			}
		}
	}

	line := line.New(rawLine, f.totalLineCount(), 100, f.globID)

	select {
	case lines <- line:
		if ls.processMaxCount {
			ls.maxCount--
			if ls.maxCount == 0 {
				if !ls.processAfter || ls.after == 0 {
					return abortReading
				}
				// Unfortunatley we have to continue filter, as there might be more lines to print
				ls.maxReached = true
			}
		}
	case <-ctx.Done():
		return abortReading
	}

	return continueReading
}

/*
func (f *readFile) filterLineWithLContextNoMatch(ctx context.Context, ltx *lcontext.LContext,
	ls *ltxState, rawLines <-chan *bytes.Buffer, lines chan<- line.Line, re *regex.Regex,
	rawLine *bytes.Buffer) readStatus {
}
*/
