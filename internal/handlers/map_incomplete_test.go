package handlers

import (
	"context"
	"testing"

	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/protocol"
)

// incompleteMapTestGroups is the number of groups of the aggregates the
// incomplete map result tests shut down.
const incompleteMapTestGroups = 3

// TestShutdownReportsIncompleteMapResult checks the session's graceful
// shutdown: a map result whose final serialization ran but could not hand
// every group to the output is reported as a failed command before the close
// handshake (a scheduled job then writes no outfile, see
// protocol.CommandFailureMapIncomplete). A complete result closes without a
// report, and so does an aggregate that never finalized because an abort won
// its terminal state: one aborted directly, and one whose command context
// was canceled (a canceled command or a replaced session generation), which
// has no final result to report on.
func TestShutdownReportsIncompleteMapResult(t *testing.T) {
	tests := []struct {
		name string
		// prepare binds the aggregate's output and may end it before the
		// shutdown; it returns the output channel.
		prepare func(t *testing.T, aggregate *mapaggregate.Aggregate) chan string
		// cancelShutdown cancels the shutdown context before the shutdown.
		cancelShutdown bool
		wantReport     bool
		// wantSent is the number of groups the final result must send.
		wantSent int
	}{
		{name: "complete result closes normally", prepare: prepareBufferedOutput,
			wantSent: incompleteMapTestGroups},
		// An unbuffered channel nobody reads keeps the final sends blocked.
		{name: "canceled final serialization is reported", prepare: prepareBlockedOutput,
			cancelShutdown: true, wantReport: true},
		{name: "aborted aggregate is not reported", prepare: prepareAbortedOutput},
		{name: "aggregate of a canceled command is not reported", prepare: prepareCanceledStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newReadTestHandler()
			// No client acknowledges the close here; do not wait for one.
			handler.acknowledgeClose()
			aggregate := newPopulatedTestAggregate(t, incompleteMapTestGroups)
			output := tt.prepare(t, aggregate)
			handler.setAggregate(aggregate)

			if reported := shutdownReportsIncompleteMap(handler, tt.cancelShutdown); reported != tt.wantReport {
				t.Fatalf("incomplete map result reported = %v, want %v", reported, tt.wantReport)
			}
			if len(output) != tt.wantSent {
				t.Fatalf("final result sent %d groups, want %d", len(output), tt.wantSent)
			}
		})
	}
}

// shutdownReportsIncompleteMap shuts handler down, with a canceled context
// if cancel is set, and reports whether it reported an incomplete map result.
//
// The report is queued synchronously, before shutdown requests the close
// handshake; the test handler has no attached reader, so the close message
// is sent asynchronously and may be dropped once the handler is done. Only
// the report is checked.
func shutdownReportsIncompleteMap(handler *baseHandler, cancel bool) bool {
	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	if cancel {
		cancelCtx()
	}
	handler.shutdown(ctx)

	incompleteReport := protocol.HiddenCommandFailedPrefix + protocol.CommandFailureMapIncomplete + "\n"
	reported := false
	for len(handler.serverMessages) > 0 {
		if <-handler.serverMessages == incompleteReport {
			reported = true
		}
	}
	return reported
}

// prepareBufferedOutput binds an output with room for every group.
func prepareBufferedOutput(_ *testing.T, aggregate *mapaggregate.Aggregate) chan string {
	output := make(chan string, incompleteMapTestGroups)
	aggregate.PrepareOutput(context.Background(), output)
	return output
}

// prepareBlockedOutput binds an unbuffered output nobody reads.
func prepareBlockedOutput(_ *testing.T, aggregate *mapaggregate.Aggregate) chan string {
	output := make(chan string)
	aggregate.PrepareOutput(context.Background(), output)
	return output
}

// prepareAbortedOutput binds an output and aborts the aggregate.
func prepareAbortedOutput(t *testing.T, aggregate *mapaggregate.Aggregate) chan string {
	output := prepareBufferedOutput(t, aggregate)
	aggregate.Abort()
	return output
}

// prepareCanceledStart runs the aggregate's Start with a canceled command
// context, as a canceled command or a replaced session generation ends it:
// the abort wins the terminal state and Start returns without a final
// result.
func prepareCanceledStart(t *testing.T, aggregate *mapaggregate.Aggregate) chan string {
	output := make(chan string, incompleteMapTestGroups)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		aggregate.Start(ctx, output)
	}()
	waitForGracefulSignal(t, returned, "Start of a canceled map command to return")
	return output
}
