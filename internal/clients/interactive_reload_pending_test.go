package clients

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/connectors"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

// requireRolledBack checks that conn was updated and rolled back to
// downOldSpec, and that client kept downOldSpec without a commit.
func requireRolledBack(t *testing.T, client *baseClient, recorder *interactiveReloadRecorder,
	conn *interactiveReloadConnector) {
	t.Helper()
	if conn.applyCount != 2 || !reflect.DeepEqual(conn.committedSpec, downOldSpec) {
		t.Fatalf("%s: applies=%d spec=%#v, want updated and rolled back", conn.server, conn.applyCount, conn.committedSpec)
	}
	if len(recorder.commits) != 0 || !reflect.DeepEqual(client.sessionSpec, downOldSpec) {
		t.Fatalf("failed reload changed state: commits=%#v spec=%#v", recorder.commits, client.sessionSpec)
	}
}

// TestReloadFailsOnPendingStart: a connecting server whose SESSION START
// still waits for its acknowledgement after the control loop's cap fails
// the reload as a whole (updateInactive's early return): the up server is
// rolled back, the reload is not committed, and the pending connection is
// not rolled back, as its start spec was not replaced.
func TestReloadFailsOnPendingStart(t *testing.T) {
	pending := &interactiveReloadConnector{server: "pending", replaceErr: connectors.ErrSessionStartPending}
	up := healthyReloadConnection("up")
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, up, pending)

	output := runReload(t, client)
	if !strings.Contains(output, "reload failed: pending: "+connectors.ErrSessionStartPending.Error()) {
		t.Fatalf("output = %q, want the reload failed by the pending start", output)
	}
	requireRolledBack(t, client, recorder, up)
	if pending.restoreCalls != 0 || len(pending.startSpecs) != 0 {
		t.Fatalf("pending: restores=%d start specs=%#v, want none", pending.restoreCalls, pending.startSpecs)
	}
}

// TestReloadRollbackOfStartSpecErrors: the rollback of a failed reload
// reports a connecting server whose start spec it could not restore, a
// deferred rollback (the start rolls itself back once acknowledged) as well
// as any other failure, and still restores the start spec of the others
// and rolls the up server back.
func TestReloadRollbackOfStartSpecErrors(t *testing.T) {
	deferred := &interactiveReloadConnector{server: "deferred", restoreErr: connectors.ErrSessionRollbackDeferred}
	broken := &interactiveReloadConnector{server: "broken", restoreErr: errors.New("restore broke")}
	connecting := &interactiveReloadConnector{server: "connecting"}
	up := healthyReloadConnection("up")
	recorder := &interactiveReloadRecorder{}
	client := newDownReloadClient(true, recorder, up, deferred, broken, connecting)
	client.profile.commit = func(SessionSpec, uint64) error { return errors.New("bad query") }

	output := runReload(t, client)
	for _, want := range []string{
		"reload failed: commit session state: bad query",
		"deferred: rollback start spec: " + connectors.ErrSessionRollbackDeferred.Error(),
		"broken: rollback start spec: restore broke",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if want := []sessionspec.Spec{downNewSpec, downOldSpec}; !reflect.DeepEqual(connecting.startSpecs, want) {
		t.Fatalf("connecting start specs = %#v, want %#v", connecting.startSpecs, want)
	}
	requireRolledBack(t, client, recorder, up)
}

// TestReloadRollbackUpdatesStartThatBegan: a connecting server whose start
// spec the reload replaced, and whose start then began and committed the
// reloaded spec before the rollback, is updated back to the previous spec
// on top of its own generation; a failure of that update is reported.
func TestReloadRollbackUpdatesStartThatBegan(t *testing.T) {
	for _, updateErr := range []error{nil, connectors.ErrSessionRejected} {
		connecting := &interactiveReloadConnector{server: "connecting"}
		up := healthyReloadConnection("up")
		recorder := &interactiveReloadRecorder{}
		client := newDownReloadClient(true, recorder, up, connecting)
		client.profile.commit = func(SessionSpec, uint64) error {
			// The start begins with the reloaded spec before the rollback.
			reloaded := downNewSpec
			connecting.startBegun, connecting.beganWith, connecting.applyErr = true, &reloaded, updateErr
			return errors.New("bad query")
		}

		output := runReload(t, client)
		if failed := strings.Contains(output, "connecting: rollback session spec: "); failed != (updateErr != nil) {
			t.Fatalf("updateErr=%v: output = %q, want the rollback failure reported only if it failed", updateErr, output)
		}
		if updateErr == nil && (connecting.generation != 2 || !reflect.DeepEqual(connecting.committedSpec, downOldSpec)) {
			t.Fatalf("connecting: generation=%d spec=%#v, want rolled back to the previous spec at 2",
				connecting.generation, connecting.committedSpec)
		}
		requireRolledBack(t, client, recorder, up)
	}
}
