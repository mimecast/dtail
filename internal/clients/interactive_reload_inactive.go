package clients

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/clients/connectors"
)

// reloadOutcome tells what an applied :reload did with the connections
// whose session was not up (see reloadRun.updateInactive); the connections
// that were up, or came up meanwhile, were all updated.
type reloadOutcome struct {
	// connecting are the servers still connecting: their SESSION START
	// sends the reloaded spec.
	connecting []string
	// inactive are the servers without an active session (it never came
	// up or it ended): they were not updated. A retrying client (dtail
	// follow mode) creates their next connection with the reloaded spec.
	inactive []string
	// notes are further hint lines of the answer, e.g. that a switch to a
	// query set --invert aside (switchToQuery).
	notes []string
}

// message is the part of the :reload answer that names the servers
// outcome lists, and its notes, one line each, starting with a line break;
// empty if it lists none. With retry (the client reconnects) the servers without an
// active session are told to start with the reloaded workload when they
// reconnect.
func (o reloadOutcome) message(style *interactiveStyle, retry bool) string {
	var sb strings.Builder
	if len(o.connecting) > 0 {
		sb.WriteString("\n" + style.hint("servers still connecting start with it: "+strings.Join(o.connecting, ", ")))
	}
	if len(o.inactive) > 0 {
		line := "servers without an active session were not updated: " + strings.Join(o.inactive, ", ")
		if retry {
			line += "; they start with it when they reconnect"
		}
		sb.WriteString("\n" + style.hint(line))
	}
	for _, note := range o.notes {
		sb.WriteString("\n" + style.hint(note))
	}
	return sb.String()
}

// partitionReloadConnections splits connections into the active ones, whose
// session is up (committed and its handler not done), which a :reload
// updates all or nothing, and the inactive ones: still connecting, or their
// session never came up (an unreachable server, whose Start shut its
// handler down) or ended. Updating an inactive one could only fail (no
// SESSION acknowledgement comes) and so failed the reload as a whole while
// any server was down. A connection without a handler (none in production)
// counts as not ended.
func partitionReloadConnections(connections []connectors.Connector) (active, inactive []connectors.Connector) {
	for _, conn := range connections {
		_, _, committed := conn.CommittedSession()
		if committed && !connectionEnded(conn) {
			active = append(active, conn)
			continue
		}
		inactive = append(inactive, conn)
	}
	return active, inactive
}

// connectionEnded reports whether conn's handler is done, i.e. its session
// ended or never came up and never will.
func connectionEnded(conn connectors.Connector) bool {
	handler := conn.Handler()
	return handler != nil && handlerDone(handler)
}

// updateInactive brings the inactive connections of a :reload whose active
// connections were all updated to nextSpec, before the reload is committed
// (see applyInteractiveReload). One that ended is left alone. One still
// connecting gets nextSpec as the spec of its SESSION START
// (ReplaceStartSessionSpec), so it does not come up with the replaced
// workload; a rollback gives it the previous spec back. One whose start
// already began (ReplaceStartSessionSpec waits for its outcome) and
// committed sent the replaced spec: it is updated like an active connection
// (reloadRun.update, on top of the same base, so it commits the same
// generation), and a failure fails the reload as a whole, as does a start
// still waiting for its acknowledgement after the wait's cap
// (connectors.ErrSessionStartPending, e.g. while a host key prompt pauses
// the output).
func (r *reloadRun) updateInactive(inactive []connectors.Connector) (reloadOutcome, error) {
	var outcome reloadOutcome
	for _, conn := range inactive {
		if connectionEnded(conn) {
			outcome.inactive = append(outcome.inactive, conn.Server())
			continue
		}
		replaced, err := conn.ReplaceStartSessionSpec(r.nextSpec)
		if err != nil {
			return reloadOutcome{}, fmt.Errorf("%s: %w", conn.Server(), err)
		}
		if replaced {
			r.replaced = append(r.replaced, conn)
			outcome.connecting = append(outcome.connecting, conn.Server())
			continue
		}
		if _, _, committed := conn.CommittedSession(); !committed {
			// Its start began and failed: the session never comes up.
			outcome.inactive = append(outcome.inactive, conn.Server())
			continue
		}
		if err := r.update(conn); err != nil {
			return reloadOutcome{}, err
		}
	}
	return outcome, nil
}

// restoreStartSpecs gives the connections whose start spec a failed reload
// replaced the previous spec back: as their start spec if their start has
// not begun yet, or else, if their session came up with the reloaded spec,
// by updating it back on top of its own generation. A start that began
// with the reloaded spec and still waits for its acknowledgement after the
// wait's cap is rolled back by the connection itself right after it is
// acknowledged (connectors.ErrSessionRollbackDeferred, which is reported so
// that the answer says so); before, it was only reported and the
// connection then ran the reloaded spec while the client was on the
// previous one.
func restoreStartSpecs(replaced []connectors.Connector, prevSpec SessionSpec) error {
	var restoreErr error
	for _, conn := range replaced {
		restored, err := conn.RestoreStartSessionSpec(prevSpec)
		if err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: rollback start spec: %w", conn.Server(), err))
			continue
		}
		if restored {
			continue
		}
		_, generation, committed := conn.CommittedSession()
		if !committed {
			continue
		}
		if err := conn.ApplySessionSpecWithGeneration(prevSpec, generation, interactiveControlTimeout); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("%s: rollback session spec: %w", conn.Server(), err))
		}
	}
	return restoreErr
}

// serverNames returns the servers of connections, comma-separated.
func serverNames(connections []connectors.Connector) string {
	names := make([]string, len(connections))
	for i, conn := range connections {
		names[i] = conn.Server()
	}
	return strings.Join(names, ", ")
}
