package clients

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
)

// incompleteWarningPrefix starts the warning of an interactive dmap whose
// result may lack data (see MaprClient.finish).
const incompleteWarningPrefix = "The mapreduce result may be incomplete"

// newSessionEndTestClient returns an interactive (DefaultMode) dmap client
// writing to a temporary outfile, recording its warnings, whose connections
// srv1, srv2, ... run the given sessions. Every session ends without the
// server's close handshake (it is cut short unless the client ended it).
func newSessionEndTestClient(t *testing.T, runs ...func(context.Context)) (*MaprClient, *warnRecorder) {
	t.Helper()
	outfile := filepath.Join(t.TempDir(), "result.csv")
	c := newOutfileTestClient(outfileTestQuery(t, outfile, false), DefaultMode)
	logger := &warnRecorder{}
	c.loggers.Client = logger
	c.mu = newBaseClientMu()
	c.stats = newTailStats(len(runs), nil, 0, nil)
	for i, run := range runs {
		c.connections = append(c.connections, &finalTestConnector{
			retryTestConnector: retryTestConnector{server: fmt.Sprintf("srv%d", i+1),
				handler: &statusTestHandler{outcome: handlers.SessionOutcome{}}},
			run: run,
		})
	}
	return c, logger
}

// incompleteWarnings returns the warnings of logger about a result that may
// lack data.
func incompleteWarnings(logger *warnRecorder) []string {
	return slices.DeleteFunc(logger.warnings(), func(line string) bool {
		return !strings.HasPrefix(line, incompleteWarningPrefix)
	})
}

// TestMaprClientWarnsForSessionCutShortBeforeCancel covers a server that
// went away mid-run while another session went on, and the user pressing
// Ctrl-C (a cancel of Start's context) only later: the session cut short
// before the cancel is warned about, the one the cancel ended is not. The
// client decides per session, at the moment it ended, whether it ended it
// itself (baseClient.runSession), not from its context after all sessions.
func TestMaprClientWarnsForSessionCutShortBeforeCancel(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		c, logger := newSessionEndTestClient(t,
			func(context.Context) {},                   // srv1 goes away at once.
			func(ctx context.Context) { <-ctx.Done() }, // srv2 runs until the cancel.
		)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		go func() { done <- c.Start(ctx, nil) }()
		synctest.Wait() // srv1's session ended and was recorded.
		cancel()
		if status := <-done; status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
		want := []string{incompleteWarningPrefix + "|srv1: the session " + sessionCutShortReason}
		if got := incompleteWarnings(logger); !slices.Equal(got, want) {
			t.Fatalf("warnings = %q, want %q", got, want)
		}
	})
}

// TestSessionEndedByClientFollowsConnections covers the per-connection
// record of whether the client ended a session itself: it is kept for the
// connection whose session ended, missing entries (connections whose session
// did not end yet) read false, and a connection replaced for a retry starts
// with false again.
func TestSessionEndedByClientFollowsConnections(t *testing.T) {
	t.Parallel()

	c := &baseClient{mu: newBaseClientMu(), connections: []connectors.Connector{
		&retryTestConnector{server: "srv1"}, &retryTestConnector{server: "srv2"}}}
	c.recordSessionEnd(0, true)
	if got := endedByClientFlags(c); !slices.Equal(got, []bool{true, false}) {
		t.Fatalf("ended by client = %v, want [true false]", got)
	}
	c.recordSessionEnd(1, true)
	c.replaceConnection(0, &retryTestConnector{server: "srv1"})
	if got := endedByClientFlags(c); !slices.Equal(got, []bool{false, true}) {
		t.Fatalf("ended by client after replacing srv1 = %v, want [false true]", got)
	}
}

func endedByClientFlags(c *baseClient) []bool {
	var flags []bool
	for _, session := range c.snapshotSessions() {
		flags = append(flags, session.endedByClient)
	}
	return flags
}
