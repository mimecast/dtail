package clients

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

// lateAckReload reloads a client of one real ServerConnection, committed at
// generation 4, whose SESSION UPDATE 5 is not acknowledged in time. Its
// acknowledgement arrives late, while the rollback waits for its own,
// followed by rollbackAck. It returns the connection, the commands it was
// sent and the reload's error.
func lateAckReload(t *testing.T, rollbackAck handlers.SessionAck) (*connectors.ServerConnection, []string, error) {
	t.Helper()
	oldArgs := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	oldSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	nextArgs := config.Args{Mode: omode.GrepClient, What: "/tmp/new.log", RegexStr: "WARN"}
	nextSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/tmp/new.log"}, Regex: "WARN"}

	handler := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5}, rollbackAck)
	conn := newInteractiveReloadServerConnection(t, "srv1", handler, oldSpec)
	handler.missAcks = 1
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{conn},
	}

	_, err := client.applyInteractiveReload(nextArgs, nextSpec)
	if !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("reload error = %v, want the acknowledgement timeout", err)
	}
	if !reflect.DeepEqual(client.Args, oldArgs) || !reflect.DeepEqual(client.sessionSpec, oldSpec) {
		t.Fatalf("client state changed by the failed reload: args=%#v spec=%#v", client.Args, client.sessionSpec)
	}
	return conn, handler.commands, err
}

// requireUpdateNumbers fails unless commands are SESSION UPDATEs numbered
// as in want.
func requireUpdateNumbers(t *testing.T, commands []string, want ...string) {
	t.Helper()
	if len(commands) != len(want) {
		t.Fatalf("commands = %q, want %d SESSION UPDATEs", commands, len(want))
	}
	for i, number := range want {
		if !strings.HasPrefix(commands[i], "SESSION UPDATE "+number+" ") {
			t.Fatalf("command %d = %q, want SESSION UPDATE %s", i, commands[i], number)
		}
	}
}

// TestReloadRollbackNotAnsweredByLateAck: the first (here the only)
// connection's update timed out and its acknowledgement arrives while the
// rollback waits. The rollback used to be numbered like the update (5), so
// the late "update ok 5" was taken for the rollback's acknowledgement and
// the rollback reported as done, although the server rejected it. It is
// numbered on top of the update now (6) and skips the late acknowledgement,
// so the rejection is reported.
func TestReloadRollbackNotAnsweredByLateAck(t *testing.T) {
	conn, commands, err := lateAckReload(t, handlers.SessionAck{Action: "error", Error: "rejected"})

	if !errors.Is(err, connectors.ErrSessionRejected) {
		t.Fatalf("reload error = %v, want the rejected rollback reported", err)
	}
	requireUpdateNumbers(t, commands, "5", "6")
	if _, generation, ok := conn.CommittedSession(); !ok || generation != 4 {
		t.Fatalf("committed generation = %d (%v), want 4: no transition was acknowledged", generation, ok)
	}
}

// TestReloadRollbackSkipsLateAck: as above, but the server acknowledges the
// rollback. The late acknowledgement of the update must neither commit the
// rollback under the update's number nor fail it as unexpected.
func TestReloadRollbackSkipsLateAck(t *testing.T) {
	conn, commands, err := lateAckReload(t, handlers.SessionAck{Action: "update", Generation: 6})

	if errors.Is(err, connectors.ErrUnexpectedSessionAck) || errors.Is(err, connectors.ErrSessionRejected) {
		t.Fatalf("reload error = %v, want only the update's timeout", err)
	}
	if strings.Contains(err.Error(), "rollback") {
		t.Fatalf("reload error = %v, want no rollback failure", err)
	}
	requireUpdateNumbers(t, commands, "5", "6")
	spec, generation, ok := conn.CommittedSession()
	if !ok || generation != 6 || spec.Regex != "ERROR" {
		t.Fatalf("committed = %#v at %d (%v), want the previous spec at 6", spec, generation, ok)
	}
}

// TestReloadNumbersAboveUnacknowledgedUpdates: a reload's update (5) and
// its rollback (6) both timed out, so the connection is still committed
// at generation 4. The next reload must not number its update 5 again: the
// late acknowledgement of the first update would answer it. It numbers it
// on top of the highest generation sent (7) and skips both late
// acknowledgements.
func TestReloadNumbersAboveUnacknowledgedUpdates(t *testing.T) {
	oldArgs := config.Args{Mode: omode.GrepClient, What: "/var/log/app.log", RegexStr: "ERROR"}
	oldSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/var/log/app.log"}, Regex: "ERROR"}
	nextArgs := config.Args{Mode: omode.GrepClient, What: "/tmp/new.log", RegexStr: "WARN"}
	nextSpec := SessionSpec{Mode: omode.GrepClient, Files: []string{"/tmp/new.log"}, Regex: "WARN"}

	handler := newInteractiveReloadSessionHandler(
		handlers.SessionAck{Action: "update", Generation: 5},
		handlers.SessionAck{Action: "update", Generation: 6},
		handlers.SessionAck{Action: "update", Generation: 7})
	conn := newInteractiveReloadServerConnection(t, "srv1", handler, oldSpec)
	handler.missAcks = 2
	client := &baseClient{
		mu:          newBaseClientMu(),
		Args:        oldArgs,
		sessionSpec: oldSpec,
		connections: []connectors.Connector{conn},
	}

	if _, err := client.applyInteractiveReload(nextArgs, nextSpec); !errors.Is(err, connectors.ErrSessionAckTimeout) {
		t.Fatalf("first reload error = %v, want the acknowledgement timeout", err)
	}
	if _, err := client.applyInteractiveReload(nextArgs, nextSpec); err != nil {
		t.Fatalf("second reload error = %v", err)
	}
	requireUpdateNumbers(t, handler.commands, "5", "6", "7")
	spec, generation, ok := conn.CommittedSession()
	if !ok || generation != 7 || spec.Regex != "WARN" {
		t.Fatalf("committed = %#v at %d (%v), want the reloaded spec at 7", spec, generation, ok)
	}
}
