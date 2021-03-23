package fs

import (
	"context"

	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

func (f readFile) filter(ctx context.Context, rawLines <-chan []byte, lines chan<- line.Line, re regex.Regex, lContext lcontext.LContext) {
	// Do we have any kind of local context settings? If so then run the more complex
	// filterWithLContext method.
	if lContext.Has() {
		// We can not skip transmitting any lines to the client with a local
		// grep context specified.
		f.canSkipLines = false
		f.filterWithLContext(ctx, rawLines, lines, re, lContext)
		return
	}

	f.filterWithoutLContext(ctx, rawLines, lines, re)
}

// Filter log lines matching a given regular expression, however with local grep context.
func (f readFile) filterWithLContext(ctx context.Context, rawLines <-chan []byte, lines chan<- line.Line, re regex.Regex, lContext lcontext.LContext) {
	// Scenario 1: Finish once maxCount hits found
	maxCount := lContext.MaxCount
	processMaxCount := maxCount > 0
	maxReached := false

	// Scenario 2: Print prev. N lines when current line matches.
	before := lContext.BeforeContext
	processBefore := before > 0
	var beforeBuf chan []byte
	if processBefore {
		beforeBuf = make(chan []byte, before)
	}

	// Screnario 3: Print next N lines when current line matches.
	after := 0
	processAfter := lContext.AfterContext > 0

	for rawLine := range rawLines {
		// logger.Debug("rawLine", string(rawLine))
		f.updatePosition()

		if !re.Match(rawLine) {
			f.updateLineNotMatched()

			if processAfter && after > 0 {
				after--
				myLine := line.Line{Content: rawLine, SourceID: f.globID, Count: f.totalLineCount(), TransmittedPerc: 100}
				select {
				case lines <- myLine:
				case <-ctx.Done():
					return
				}

			} else if processBefore {
				// Keep last num BeforeContext raw messages.
				select {
				case beforeBuf <- rawLine:
				default:
					<-beforeBuf
					beforeBuf <- rawLine
				}
			}
			continue
		}

		f.updateLineMatched()

		if processAfter {
			if maxReached {
				return
			}
			after = lContext.AfterContext
		}

		if processBefore {
			i := uint64(len(beforeBuf))
			for {
				select {
				case myRawLine := <-beforeBuf:
					myLine := line.Line{Content: myRawLine, SourceID: f.globID, Count: f.totalLineCount() - i, TransmittedPerc: 100}
					i--
					select {
					case lines <- myLine:
					case <-ctx.Done():
						return
					}
				default:
					// beforeBuf is now empty.
				}
				if len(beforeBuf) == 0 {
					break
				}
			}
		}

		line := line.Line{Content: rawLine, SourceID: f.globID, Count: f.totalLineCount(), TransmittedPerc: 100}

		select {
		case lines <- line:
			if processMaxCount {
				maxCount--
				if maxCount == 0 {
					if !processAfter || after == 0 {
						return
					}
					// Unfortunatley we have to continue filter, as there might be more lines to print
					maxReached = true
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// Filter log lines matching a given regular expression, there is no local grep context specified.
func (f readFile) filterWithoutLContext(ctx context.Context, rawLines <-chan []byte, lines chan<- line.Line, re regex.Regex) {
	for {
		select {
		case rawLine, ok := <-rawLines:
			f.updatePosition()
			if !ok {
				return
			}

			if f.lineUntransmittable(rawLine, len(lines), cap(lines), re) {
				continue
			}

			line := line.Line{Content: rawLine, SourceID: f.globID, Count: f.totalLineCount(), TransmittedPerc: f.transmittedPerc()}

			select {
			case lines <- line:
				continue
			case <-ctx.Done():
				return
			}
		}
	}
}

func (f readFile) lineUntransmittable(rawLine []byte, length, capacity int, re regex.Regex) bool {
	if !re.Match(rawLine) {
		f.updateLineNotMatched()
		f.updateLineNotTransmitted()
		// Regex dosn't match, so not interested in it.
		return true
	}
	f.updateLineMatched()

	// Can we actually send more messages, channel capacity reached?
	if f.canSkipLines && length >= capacity {
		f.updateLineNotTransmitted()
		// Matching, not transmittable
		return true
	}
	f.updateLineTransmitted()

	// Matching, transmittable
	return false
}
