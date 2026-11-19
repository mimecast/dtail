package clients

import (
	"context"
	"testing"
	"time"
)

// TestSessionStartWhileOutputPaused pauses the interactive output on the
// first payload write of a session, before the client handler may have
// read the server's SESSION START acknowledgement, and holds the pause for
// longer than the acknowledgement timeout (defaultSessionAckTimeout, 2 s).
// dserver used to queue the acknowledgement behind the first payload
// frames, which a paused handler does not read (it now sends it ahead of
// them, see TestSessionStartCommitsOverSlowLink, but payload already on its
// way can still come first); the session start must not fail for that:
// once the output resumes, the acknowledgement is read and the session is
// committed. Whether the acknowledgement comes behind payload
// depends on scheduling (before the fix about 1 run in 10 failed with
// -test.cpu 1); the deterministic checks are in the handlers package
// (TestWaitForSessionAckBehindBlockedPayload) and the connectors package
// (TestSessionStartAckBehindPausedPayload). Both serverless variants of
// TestProbeAnswerWhileOutputPaused are run: payload framed through the
// client handler (as from a remote dserver), and the direct serverless
// output with an in-process server logging through the paused output.
func TestSessionStartWhileOutputPaused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		direct bool
	}{
		{name: "framed", direct: false},
		{name: "direct", direct: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runPausedSessionStart(t, tt.direct)
		})
	}
}

// runPausedSessionStart runs a session of TestSessionStartWhileOutputPaused.
func runPausedSessionStart(t *testing.T, direct bool) {
	t.Helper()
	file := writeProbeTestLog(t, 20000)
	session := newPausedProbeSession(t, file, direct, false)
	session.pauseBeforeAck = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	go func() {
		defer close(started)
		session.connector.Start(ctx, cancel, nil, nil)
	}()
	defer func() { cancel(); session.resume(); <-started }()

	select {
	case <-session.paused:
	case <-time.After(10 * time.Second):
		t.Fatal("no payload arrived")
	}
	time.Sleep(3 * time.Second)
	if ctx.Err() != nil {
		t.Fatalf("the connection ended while the output was paused: status %d", session.handler.Status())
	}
	session.resume()
	requireSessionCommitted(t, ctx.Done(), session)
}

// requireSessionCommitted waits for the session start of session to be
// committed at generation 1, failing if the connection ends first.
func requireSessionCommitted(t *testing.T, ended <-chan struct{}, session *pausedProbeSession) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, generation, committed := session.connector.CommittedSession(); committed {
			if generation != 1 {
				t.Fatalf("committed generation = %d, want 1", generation)
			}
			return
		}
		select {
		case <-ended:
			t.Fatalf("the session start failed: status %d", session.handler.Status())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("the session start was not committed after the output resumed")
}
