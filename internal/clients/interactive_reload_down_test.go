package clients

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	sessionspec "github.com/mimecast/dtail/internal/session"
	sshclient "github.com/mimecast/dtail/internal/ssh/client"

	gossh "golang.org/x/crypto/ssh"
)

var (
	downOldSpec = SessionSpec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	downOldArgs = config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	downNewSpec = SessionSpec{Mode: omode.TailClient, Files: []string{"/var/log/app.log"}, Regex: "WARN"}
	downNewArgs = config.Args{Mode: omode.TailClient, What: "/var/log/app.log", RegexStr: "WARN"}
)

// newDownReloadClient returns a client of connections in tail mode at
// downOldSpec, retrying like dtail when retry is set.
func newDownReloadClient(retry bool, recorder *interactiveReloadRecorder,
	connections ...connectors.Connector) *baseClient {
	return &baseClient{
		mu:          newBaseClientMu(),
		Args:        downOldArgs,
		sessionSpec: downOldSpec,
		connections: connections,
		profile:     clientProfile{retry: retry, commit: recorder.commitSessionSpec},
	}
}

// healthyReloadConnection is a connection whose session is up at
// generation 4 with downOldSpec.
func healthyReloadConnection(server string) *interactiveReloadConnector {
	return &interactiveReloadConnector{server: server, committedSpec: downOldSpec,
		liveSpec: downOldSpec, generation: 4}
}

// runReload runs :reload to downNewSpec through runInteractiveCommand and
// returns what it wrote to the TTY.
func runReload(t *testing.T, client *baseClient) string {
	t.Helper()
	output, err := reloadOutput(client)
	if err != nil {
		t.Fatalf("runInteractiveCommand() error = %v", err)
	}
	return output
}

// reloadOutput is runReload without the test's failure, for a goroutine
// other than the test's.
func reloadOutput(client *baseClient) (string, error) {
	var tty bytes.Buffer
	command := interactiveCommand{kind: "reload", next: downNewArgs, spec: downNewSpec}
	_, err := client.runInteractiveCommand(&tty, command)
	return tty.String(), err
}

// requireUpdatedOnce checks that conn was updated once, to downNewSpec at
// generation 5, and not rolled back.
func requireUpdatedOnce(t *testing.T, conn *interactiveReloadConnector) {
	t.Helper()
	if conn.applyCount != 1 || conn.generation != 5 || !reflect.DeepEqual(conn.committedSpec, downNewSpec) {
		t.Fatalf("%s: applies=%d generation=%d spec=%#v, want one update to generation 5 with %#v",
			conn.server, conn.applyCount, conn.generation, conn.committedSpec, downNewSpec)
	}
}

// TestReloadSkipsServerWithoutSession: a server that could not be connected
// to (a real ServerConnection whose dial was refused, listed first) no
// longer fails a :reload as a whole. The healthy server is updated once and
// not rolled back, the reload is committed, and the TTY names the server
// that was not updated. Before, the dead connection failed the reload (its
// SESSION START was never acknowledged), and the healthy one was updated
// and rolled back (generation 4 to 6).
func TestReloadSkipsServerWithoutSession(t *testing.T) {
	down, downHandler := unreachableServerConnection(t)
	if !handlerDone(downHandler) {
		t.Fatal("precondition: the unreachable connection's handler is not done")
	}
	up := healthyReloadConnection("up")
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, down, up)

	output := runReload(t, client)
	if !strings.HasPrefix(output, "reload applied successfully\n") {
		t.Fatalf("output = %q, want the reload applied", output)
	}
	if !strings.Contains(output, "without an active session were not updated: "+down.Server()) ||
		!strings.Contains(output, "when they reconnect") {
		t.Fatalf("output = %q, want the server without a session named", output)
	}
	requireUpdatedOnce(t, up)
	if !reflect.DeepEqual(client.sessionSpec, downNewSpec) {
		t.Fatalf("client spec = %#v, want %#v (a reconnect starts with it)", client.sessionSpec, downNewSpec)
	}
	if len(recorder.commits) != 1 || recorder.commits[0].generation != 5 {
		t.Fatalf("commits = %#v, want one at generation 5", recorder.commits)
	}
	if _, _, ok := down.CommittedSession(); ok {
		t.Fatal("dead connection has a committed session")
	}
}

// TestReloadWithoutRetryDoesNotPromiseReconnect: a client that does not
// reconnect (dcat, dgrep, dmap) reports the server without a session as
// not updated, without saying it starts with the workload later.
func TestReloadWithoutRetryDoesNotPromiseReconnect(t *testing.T) {
	down, _ := unreachableServerConnection(t)
	up := healthyReloadConnection("up")
	client := newDownReloadClient(false, &interactiveReloadRecorder{}, down, up)

	output := runReload(t, client)
	if !strings.Contains(output, "were not updated: "+down.Server()) || strings.Contains(output, "reconnect") {
		t.Fatalf("output = %q, want the server named without a reconnect promise", output)
	}
	requireUpdatedOnce(t, up)
}

// TestReloadReplacesStartSpecOfConnectingServer: a connection still
// connecting (no session committed, its handler not done, its SESSION START
// not begun) gets the reloaded spec as its start spec, so it comes up with
// the new workload instead of the replaced one, and is named on the TTY.
func TestReloadReplacesStartSpecOfConnectingServer(t *testing.T) {
	connecting := &interactiveReloadConnector{server: "connecting"}
	up := healthyReloadConnection("up")
	client := newDownReloadClient(true, &interactiveReloadRecorder{}, connecting, up)

	output := runReload(t, client)
	if !strings.Contains(output, "still connecting start with it: connecting") {
		t.Fatalf("output = %q, want the connecting server named", output)
	}
	requireUpdatedOnce(t, up)
	if !reflect.DeepEqual(connecting.startSpecs, []sessionspec.Spec{downNewSpec}) || connecting.applyCount != 0 {
		t.Fatalf("connecting: start specs %#v applies %d, want the new spec as start spec only",
			connecting.startSpecs, connecting.applyCount)
	}
}

// TestReloadUpdatesSessionStartedDuringReload: a connection whose SESSION
// START (with the replaced spec) began after the reload found it without a
// session is updated once that start committed, on top of the same base as
// the others, so it commits the same generation (5), and before the reload
// is committed: a dmap connection still sending results of the replaced
// query after the commit would have them merged into the new one.
func TestReloadUpdatesSessionStartedDuringReload(t *testing.T) {
	old := downOldSpec
	late := &interactiveReloadConnector{server: "late", startBegun: true, beganWith: &old}
	up := healthyReloadConnection("up")
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, up, late)
	var lateAtCommit sessionspec.Spec
	client.profile.commit = func(spec SessionSpec, generation uint64) error {
		lateAtCommit = late.committedSpec
		return recorder.commitSessionSpec(spec, generation)
	}

	output := runReload(t, client)
	if !strings.HasPrefix(output, "reload applied successfully\n") || strings.Contains(output, "late") {
		t.Fatalf("output = %q, want the reload applied with no server left out", output)
	}
	requireUpdatedOnce(t, up)
	if late.applyCount != 1 || late.generation != 5 || !reflect.DeepEqual(late.committedSpec, downNewSpec) {
		t.Fatalf("late: applies=%d generation=%d spec=%#v, want one update to generation 5",
			late.applyCount, late.generation, late.committedSpec)
	}
	if !reflect.DeepEqual(lateAtCommit, downNewSpec) {
		t.Fatalf("late had %#v at the commit, want it updated before", lateAtCommit)
	}
	if len(recorder.commits) != 1 || recorder.commits[0].generation != 5 {
		t.Fatalf("commits = %#v, want one at generation 5", recorder.commits)
	}
}

// TestReloadFailureOfLateSessionRollsBack: a connection whose session came
// up during the reload is held to the all-or-nothing rule like the others:
// its failed update fails the reload and rolls the up server back, and the
// reload is not committed.
func TestReloadFailureOfLateSessionRollsBack(t *testing.T) {
	old := downOldSpec
	late := &interactiveReloadConnector{server: "late", startBegun: true, beganWith: &old,
		applyErr: connectors.ErrSessionRejected}
	up := healthyReloadConnection("up")
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, up, late)

	output := runReload(t, client)
	if !strings.Contains(output, "reload failed: late:") {
		t.Fatalf("output = %q, want the reload failed by the late server", output)
	}
	if up.applyCount != 2 || !reflect.DeepEqual(up.committedSpec, downOldSpec) {
		t.Fatalf("up: applies=%d spec=%#v, want updated and rolled back", up.applyCount, up.committedSpec)
	}
	if len(recorder.commits) != 0 || !reflect.DeepEqual(client.sessionSpec, downOldSpec) {
		t.Fatalf("failed reload changed state: commits=%#v spec=%#v", recorder.commits, client.sessionSpec)
	}
}

// TestReloadCommitFailureRestoresStartSpec: a reload whose commit fails
// gives a connecting server the previous spec back as its start spec.
func TestReloadCommitFailureRestoresStartSpec(t *testing.T) {
	connecting := &interactiveReloadConnector{server: "connecting"}
	up := healthyReloadConnection("up")
	client := newDownReloadClient(true, &interactiveReloadRecorder{}, up, connecting)
	client.profile.commit = func(SessionSpec, uint64) error { return errors.New("bad query") }

	output := runReload(t, client)
	if !strings.Contains(output, "reload failed: commit session state: bad query") {
		t.Fatalf("output = %q, want the commit failure", output)
	}
	if want := []sessionspec.Spec{downNewSpec, downOldSpec}; !reflect.DeepEqual(connecting.startSpecs, want) {
		t.Fatalf("connecting start specs = %#v, want %#v", connecting.startSpecs, want)
	}
	if up.applyCount != 2 || !reflect.DeepEqual(up.committedSpec, downOldSpec) {
		t.Fatalf("up: applies=%d spec=%#v, want updated and rolled back", up.applyCount, up.committedSpec)
	}
}

// TestReloadWithoutAnyActiveSessionFails: with no session up at all the
// reload is rejected and changes nothing, not even the start spec of a
// connecting server: there is no generation to commit the new workload at.
func TestReloadWithoutAnyActiveSessionFails(t *testing.T) {
	down, _ := unreachableServerConnection(t)
	connecting := &interactiveReloadConnector{server: "connecting"}
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, down, connecting)

	output := runReload(t, client)
	if !strings.Contains(output, "reload failed: no server has an active session") {
		t.Fatalf("output = %q, want the reload rejected", output)
	}
	if !reflect.DeepEqual(client.sessionSpec, downOldSpec) || len(recorder.commits) != 0 ||
		len(connecting.startSpecs) != 0 {
		t.Fatalf("reload changed state: spec=%#v commits=%#v start specs=%#v",
			client.sessionSpec, recorder.commits, connecting.startSpecs)
	}
}

// TestReloadFailureOfActiveSessionStillRollsBack: the all-or-nothing rule
// stays for connections that are up: one failing update rolls the others
// back, and a connecting server keeps its start spec.
func TestReloadFailureOfActiveSessionStillRollsBack(t *testing.T) {
	up := healthyReloadConnection("up")
	failing := healthyReloadConnection("failing")
	failing.applyErr = connectors.ErrSessionRejected
	connecting := &interactiveReloadConnector{server: "connecting"}
	client := newDownReloadClient(true, &interactiveReloadRecorder{}, up, failing, connecting)

	output := runReload(t, client)
	if !strings.Contains(output, "reload failed: failing:") {
		t.Fatalf("output = %q, want the reload failed by the up server", output)
	}
	if up.applyCount != 2 || !reflect.DeepEqual(up.committedSpec, downOldSpec) {
		t.Fatalf("up: applies=%d spec=%#v, want updated and rolled back", up.applyCount, up.committedSpec)
	}
	if len(connecting.startSpecs) != 0 || !reflect.DeepEqual(client.sessionSpec, downOldSpec) {
		t.Fatalf("failed reload changed state: start specs=%#v spec=%#v", connecting.startSpecs, client.sessionSpec)
	}
}

// blockingReloadConnector is a connection whose session is up and whose
// update blocks until release is closed.
type blockingReloadConnector struct {
	interactiveReloadConnector
	entered chan struct{}
	release chan struct{}
}

func (c *blockingReloadConnector) ApplySessionSpecWithGeneration(spec sessionspec.Spec,
	generation uint64, timeout time.Duration) error {
	if c.applyCount == 0 {
		close(c.entered)
		<-c.release
	}
	return c.interactiveReloadConnector.ApplySessionSpecWithGeneration(spec, generation, timeout)
}

func (c *blockingReloadConnector) ApplySessionSpec(spec sessionspec.Spec, timeout time.Duration) error {
	return c.ApplySessionSpecWithGeneration(spec, c.generation, timeout)
}

// TestReconnectDuringReloadGetsNewSpec: a dtail connection that reconnects
// while a :reload runs is not in the reload's list of connections, so it is
// created only once the reload ended, with the reloaded spec. Before, the
// reconnect could be created with the replaced spec in the middle of the
// reload, which then left it out, and it ran the replaced workload.
func TestReconnectDuringReloadGetsNewSpec(t *testing.T) {
	up := &blockingReloadConnector{interactiveReloadConnector: *healthyReloadConnection("up"),
		entered: make(chan struct{}), release: make(chan struct{})}
	dead := &retryTestConnector{server: "dead", handler: &retryTestHandler{}}
	client := newDownReloadClient(true, &interactiveReloadRecorder{}, up, dead)
	client.stats = &stats{connectionsEstCh: make(chan struct{}, 1)}
	var reconnectSpec atomic.Pointer[SessionSpec]
	client.connectionFactory = func(server string, _ []gossh.AuthMethod,
		_ sshclient.HostKeyCallback, spec SessionSpec, _ bool) connectors.Connector {
		reconnectSpec.Store(&spec)
		return &retryTestConnector{server: server, handler: &retryTestHandler{}}
	}
	var sleeps atomic.Int32
	client.sleepFn = func(ctx context.Context, _ time.Duration) bool {
		if sleeps.Add(1) == 1 {
			// The reconnect comes while the reload runs.
			select {
			case <-up.entered:
			case <-time.After(5 * time.Second):
			}
			return true
		}
		return false
	}

	type reloadResult struct {
		output string
		err    error
	}
	reloadDone := make(chan reloadResult, 1)
	go func() {
		output, err := reloadOutput(client)
		reloadDone <- reloadResult{output, err}
	}()
	connDone := make(chan int, 1)
	go func() { connDone <- client.startConnection(t.Context(), 1, dead) }()

	select {
	case <-up.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not update the up server")
	}
	time.Sleep(50 * time.Millisecond) // time for a reconnect not held back
	close(up.release)
	if res := <-reloadDone; res.err != nil || !strings.HasPrefix(res.output, "reload applied successfully") {
		t.Fatalf("output = %q error = %v, want the reload applied", res.output, res.err)
	}
	<-connDone
	if got := reconnectSpec.Load(); got == nil || !reflect.DeepEqual(*got, downNewSpec) {
		t.Fatalf("reconnect spec = %#v, want %#v", got, downNewSpec)
	}
}
