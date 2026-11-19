package connectors

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	sessionspec "github.com/mimecast/dtail/internal/session"
)

var (
	// ErrSessionAckTimeout indicates that no hidden SESSION acknowledgement arrived in time.
	ErrSessionAckTimeout = errors.New("timed out waiting for session acknowledgement")
	// ErrSessionRejected indicates that the server explicitly rejected a SESSION request.
	ErrSessionRejected = errors.New("session request rejected")
	// ErrUnexpectedSessionAck indicates that the client received a malformed or mismatched acknowledgement.
	ErrUnexpectedSessionAck = errors.New("unexpected session acknowledgement")
	// ErrJournalUnsupported indicates that the remote side did not advertise
	// journal support (protocol.CapabilityJournalV1).
	ErrJournalUnsupported = errors.New(journalUnsupportedMessage)
	// ErrSessionStartPending indicates that the connection's initial
	// SESSION START, or the rollback a failed reload deferred to right
	// after it (restoreStartSpec), still waited for its acknowledgement
	// after sessionUpdateAckMaxWait, so its session could not be changed.
	ErrSessionStartPending = errors.New("session start (or its deferred rollback) still waiting for its acknowledgement")
	// ErrSessionRollbackDeferred indicates that a rollback of the start
	// spec found the connection's initial SESSION START still waiting for
	// its acknowledgement (as ErrSessionStartPending): the start updates
	// its session to the rolled back spec right after it is acknowledged
	// (see committedSessionState.restoreStartSpec).
	ErrSessionRollbackDeferred = errors.New("session start still waiting for its acknowledgement, " +
		"rolled back once it is acknowledged")
)

// journalUnsupportedMessage is what the user is told when a spec reads a
// journal target from a server without journal support.
const journalUnsupportedMessage = "journal file targets require server capability " + protocol.CapabilityJournalV1

// defaultSessionAckTimeout is how long a SESSION acknowledgement is waited
// for, counting only the time the client handler was idle (see
// handlers.Handler.WaitForSessionAck): a busy handler, e.g. one whose
// payload write the paused output blocks, stretches it.
const defaultSessionAckTimeout = 2 * time.Second

// sessionUpdateAckMaxWait caps, in wall-clock time, how long the
// interactive control loop waits for a session transition: a SESSION
// UPDATE's acknowledgement (:reload, :query, :tail and their rollbacks)
// and the session lock a SESSION START in flight holds (replaceStartSpec).
// The control loop must not wait without bound: it alone answers a host
// key prompt, whose pause of the output can keep the client handler from
// reading the acknowledgement, and a flood of client-bound output keeps the
// handler busy, stretching the idle-time timeout. The initial SESSION
// START, waited for on the connection's own goroutine, has no such cap, so
// a long pause right after it only delays it.
// It is a variable only so that tests can shorten it.
var sessionUpdateAckMaxWait = 10 * time.Second

// committedSessionState is the session state of one connection: the spec
// and generation the server acknowledged last, and what its initial SESSION
// START sends. applyMu serializes the session transitions (the initial
// start, reloads, rollbacks and replacing the start spec); mu guards the
// committed fields for readers and the start's follow-up (see
// restoreStartSpec).
type committedSessionState struct {
	applyMu    applyLock
	mu         sync.RWMutex
	committed  bool
	generation uint64
	spec       sessionspec.Spec
	// started is set, under applyMu, once the initial SESSION START began,
	// whether it succeeded or not; the start spec is no longer replaced
	// then.
	started bool
	// startSpec, when hasStartSpec is set, replaces the connector's initial
	// spec in the initial SESSION START (see replaceStartSpec).
	startSpec    sessionspec.Spec
	hasStartSpec bool
	// startDone is set, under mu, once the initial SESSION START finished
	// (finishStart, which also returns followUp).
	startDone bool
	// followUp, when hasFollowUp is set (both under mu), is the spec the
	// initial SESSION START in flight updates its session to once it was
	// acknowledged (see restoreStartSpec). hasFollowUp stays set until that
	// update finished (endFollowUp), and the session counts as not
	// committed meanwhile (snapshot).
	followUp    sessionspec.Spec
	hasFollowUp bool
}

// replaceStartSpec makes spec the workload of the initial SESSION START if
// that has not begun yet, and reports true then. It reports false once it
// began, whether it succeeded or not: the caller then has to update a
// committed session, as a :reload does. A SESSION START in flight holds
// applyMu, so this waits for its outcome instead of racing with it, but
// for at most sessionUpdateAckMaxWait: it returns ErrSessionStartPending
// then, as the start's own wait has no wall-clock limit.
func (s *committedSessionState) replaceStartSpec(spec sessionspec.Spec) (bool, error) {
	if !s.applyMu.lockWithin(sessionUpdateAckMaxWait) {
		return false, ErrSessionStartPending
	}
	defer s.applyMu.Unlock()

	if s.started {
		return false, nil
	}
	s.startSpec = spec
	s.hasStartSpec = true
	return true, nil
}

// restoreStartSpec is replaceStartSpec for the rollback of a failed
// :reload, which gives a connecting connection the previous spec back. If
// the start already began with the reloaded spec and still waits for its
// acknowledgement after sessionUpdateAckMaxWait, the rollback can not wait
// for it; left alone, the connection would commit the reloaded spec while
// the client and the other connections are on the previous one (dmap would
// merge the reloaded query's results into the previous aggregation). So
// the start is told to update its session to spec right after its
// acknowledgement (finishStart), and ErrSessionRollbackDeferred is
// returned. Should the start have finished in the meantime, the lock is
// taken again: replaceStartSpec then reports false, and the caller updates
// the committed session as for any start that began.
func (s *committedSessionState) restoreStartSpec(spec sessionspec.Spec) (bool, error) {
	restored, err := s.replaceStartSpec(spec)
	if !errors.Is(err, ErrSessionStartPending) {
		return restored, err
	}
	if s.deferStartFollowUp(spec) {
		return false, ErrSessionRollbackDeferred
	}
	return s.replaceStartSpec(spec)
}

// deferStartFollowUp makes spec the follow-up of the initial SESSION START
// and reports true, unless the start finished already.
func (s *committedSessionState) deferStartFollowUp(spec sessionspec.Spec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.startDone {
		return false
	}
	s.followUp = spec
	s.hasFollowUp = true
	return true
}

// finishStart marks the initial SESSION START as finished and returns its
// follow-up spec, if a rollback deferred one (deferStartFollowUp). The
// caller holds applyMu and calls endFollowUp once the follow-up finished.
func (s *committedSessionState) finishStart() (sessionspec.Spec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.startDone = true
	return s.followUp, s.hasFollowUp
}

// endFollowUp marks the start's follow-up as finished, whether it
// succeeded or not, so the session counts as committed again if it is.
func (s *committedSessionState) endFollowUp() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.followUp, s.hasFollowUp = sessionspec.Spec{}, false
}

func (s *committedSessionState) commit(spec sessionspec.Spec, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.committed = true
	s.generation = generation
	s.spec = spec
}

func (s *committedSessionState) restore(spec sessionspec.Spec, generation uint64, committed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !committed {
		s.committed = false
		s.generation = 0
		s.spec = sessionspec.Spec{}
		return
	}

	s.committed = true
	s.generation = generation
	s.spec = spec
}

func (s *committedSessionState) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.committed = false
	s.generation = 0
	s.spec = sessionspec.Spec{}
}

// snapshot returns the committed spec and generation and whether a session
// is committed. A session whose start committed a reloaded spec that a
// failed reload deferred a rollback for (hasFollowUp) is reported as not
// committed until that rollback finished: committed, a reload arriving
// meanwhile would take it for an active session at the failed reload's
// spec and record that spec for its own rollback, outside applyMu, which
// the follow-up holds. Not committed, the reload takes the path of a
// connection still connecting (ReplaceStartSessionSpec), which waits for
// applyMu and then finds the rolled back spec committed. The generation is
// returned either way.
func (s *committedSessionState) snapshot() (sessionspec.Spec, uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.spec, s.generation, s.committed && !s.hasFollowUp
}

// dispatchInitialCommands starts the session's workload: the plain command
// stream of a non-interactive client, or SESSION START for an
// --interactive-query client with a known mode. Every protocol 5 server
// supports SESSION START, so a failed start fails the connection; there is
// no fallback to the command stream.
func dispatchInitialCommands(server string, handler handlers.Handler, commands []string,
	interactiveQuery bool, initialSpec sessionspec.Spec, state *committedSessionState,
	logger logging.Logger) error {
	if !interactiveQuery || initialSpec.Mode == omode.Unknown {
		if err := requireJournalCapability(server, handler, initialSpec, defaultCapabilityWait, true); err != nil {
			return err
		}
		return sendCommandStream(handler, commands)
	}

	if err := startInitialSession(server, handler, state, initialSpec, logger); err != nil {
		state.clear()
		return err
	}
	return nil
}

// startInitialSession sends the initial SESSION START of an interactive
// connection: with the spec a :reload set while the connection was still
// connecting (replaceStartSpec), or else initialSpec, the spec the
// connection was created with. The start is marked as begun and its spec
// read under applyMu, which it keeps holding, so a concurrent reload either
// replaces the spec before the start or finds the start begun and updates
// the session after it. A failed reload whose rollback could not wait for
// the start (restoreStartSpec) left a follow-up spec: the session is
// updated to it right after the start was acknowledged, still under
// applyMu, so no later transition sees the reloaded spec in between.
func startInitialSession(server string, handler handlers.Handler, state *committedSessionState,
	initialSpec sessionspec.Spec, logger logging.Logger) error {
	state.applyMu.Lock()
	defer state.applyMu.Unlock()

	state.started = true
	spec := initialSpec
	if state.hasStartSpec {
		spec = state.startSpec
	}
	// No wall-clock cap (maxWait 0): this runs on the connection's own
	// goroutine, and a paused output only delays the acknowledgement.
	err := applySessionSpecLocked(server, handler, state, spec, 0, defaultSessionAckTimeout, 0, logger)
	followUp, hasFollowUp := state.finishStart()
	if !hasFollowUp {
		return err
	}
	defer state.endFollowUp()
	if err != nil {
		return err
	}
	return applyStartFollowUp(server, handler, state, followUp, logger)
}

// applyStartFollowUp updates the session the initial SESSION START just
// committed to followUp, the spec a failed reload rolled it back to. Until
// the update is acknowledged the server runs the reloaded spec, and all it
// sends meanwhile reaches the client, which filters no output by
// generation: lines of the reloaded workload are printed, and with dmap
// its aggregates are merged into the client's aggregation (with dtail
// switched from following lines to a query, they can print as
// AGGREGATE|... lines). Only what dserver still queued when the update
// arrived is dropped. A failed update fails the start and so ends the
// connection (a retrying client reconnects with the client's current
// spec), rather than leaving it on the reloaded spec. The caller holds
// applyMu.
func applyStartFollowUp(server string, handler handlers.Handler, state *committedSessionState,
	followUp sessionspec.Spec, logger logging.Logger) error {
	_, generation, _ := state.snapshot()
	logging.OrNop(logger).Debug(server, "Rolling the session start back to the spec of before a failed reload",
		"generation", generation)
	if err := applySessionSpecLocked(server, handler, state, followUp, generation,
		defaultSessionAckTimeout, 0, logger); err != nil {
		return fmt.Errorf("roll back the reloaded spec of the session start: %w", err)
	}
	return nil
}

func applySessionSpec(server string, handler handlers.Handler,
	state *committedSessionState, spec sessionspec.Spec, timeout time.Duration,
	logger logging.Logger) error {
	return applySessionSpecWithGeneration(server, handler, state, spec, 0, true, timeout, logger)
}

func applySessionSpecWithGeneration(server string, handler handlers.Handler,
	state *committedSessionState, spec sessionspec.Spec, generation uint64, useCurrentGeneration bool,
	timeout time.Duration, logger logging.Logger) error {
	// Serialize session transitions so an interactive reload cannot race the
	// initial SESSION START bootstrap on the same connection. The interactive
	// control loop calls this, so neither the lock nor the acknowledgement
	// is waited for longer than sessionUpdateAckMaxWait.
	if !state.applyMu.lockWithin(sessionUpdateAckMaxWait) {
		return fmt.Errorf("%w: %s", ErrSessionStartPending, server)
	}
	defer state.applyMu.Unlock()

	if useCurrentGeneration {
		_, generation, _ = state.snapshot()
	}
	return applySessionSpecLocked(server, handler, state, spec, generation, timeout,
		sessionUpdateAckMaxWait, logger)
}

// applySessionSpecLocked sends SESSION START (generation 0) or SESSION
// UPDATE generation+1 for spec, and commits spec once the server
// acknowledged it, waiting for that as handlers.Handler.WaitForSessionAck
// with timeout and maxWait. The caller holds state.applyMu.
func applySessionSpecLocked(server string, handler handlers.Handler,
	state *committedSessionState, spec sessionspec.Spec, generation uint64,
	timeout, maxWait time.Duration, logger logging.Logger) error {
	logger = logging.OrNop(logger)

	// Only a failed SESSION START (generation 0) is reported to the user and
	// sets the exit status: it fails the initial workload. A rejected UPDATE
	// is answered on the interactive TTY and the client keeps running.
	if err := requireJournalCapability(server, handler, spec, defaultCapabilityWait, generation == 0); err != nil {
		return err
	}

	action := "start"
	nextGeneration := uint64(0)
	command, err := spec.StartCommand()
	if err != nil {
		return err
	}

	if generation != 0 {
		action = "update"
		nextGeneration = generation + 1
		command, err = spec.UpdateCommand(nextGeneration)
		if err != nil {
			return err
		}
	}

	drainSessionAcks(handler)
	if sendErr := handler.SendMessage(command); sendErr != nil {
		return sendErr
	}

	ack, ok := handler.WaitForSessionAck(resolveSessionAckTimeout(timeout), maxWait)
	if !ok {
		return ErrSessionAckTimeout
	}
	if ack.Error != "" {
		return fmt.Errorf("%w: %s", ErrSessionRejected, ack.Error)
	}
	if ack.Action != action {
		return fmt.Errorf("%w: got action %q want %q", ErrUnexpectedSessionAck, ack.Action, action)
	}
	if ack.Generation == 0 {
		return fmt.Errorf("%w: missing generation", ErrUnexpectedSessionAck)
	}
	if action == "update" && ack.Generation != nextGeneration {
		return fmt.Errorf("%w: got generation %d want %d", ErrUnexpectedSessionAck, ack.Generation, nextGeneration)
	}

	state.commit(spec, ack.Generation)
	logger.Debug(server, "Committed session spec", "action", action, "generation", ack.Generation)
	return nil
}

// missingJournalCapability reports whether spec reads a journal target and
// the server of handler, waited for up to timeout, does not advertise
// protocol.CapabilityJournalV1. A nil handler advertises nothing. Journal
// support is the only capability a read target can need: it is a runtime
// property of the server (Linux with journalctl), while everything else a
// spec can ask for, e.g. a path that has to be encoded
// (protocol.EncodeReadPath), every protocol 5 server supports.
func missingJournalCapability(handler handlers.Handler, spec sessionspec.Spec,
	timeout time.Duration) bool {
	if !spec.HasJournalFiles() {
		return false
	}
	if handler == nil {
		return true
	}
	if timeout <= 0 {
		timeout = defaultCapabilityWait
	}
	return !handler.WaitForCapabilities(timeout) || !handler.HasCapability(protocol.CapabilityJournalV1)
}

// requireJournalCapability checks, before any read command of spec is sent
// (plain commands, SESSION START or a :reload's SESSION UPDATE), that the
// server supports journal targets if spec reads one, and returns
// ErrJournalUnsupported wrapped with the server name otherwise. With report
// it also reports journalUnsupportedMessage to the user, which sets a
// non-zero exit status: the initial workload must fail the client, while a
// rejected :reload is answered on the TTY and leaves the running workload,
// and the exit status, as they were.
func requireJournalCapability(server string, handler handlers.Handler, spec sessionspec.Spec,
	timeout time.Duration, report bool) error {
	if !missingJournalCapability(handler, spec, timeout) {
		return nil
	}
	if report && handler != nil {
		handler.ReportServerError(journalUnsupportedMessage)
	}
	return fmt.Errorf("%w: %s", ErrJournalUnsupported, server)
}

// CheckJournalCapability returns ErrJournalUnsupported if spec reads a
// journal target and the server of conn, waited for up to timeout, does not
// support journal targets, and nil otherwise. It reports nothing to the
// user. An interactive :reload checks every connection with it before it
// updates any, so that a server without journal support rejects the reload
// as a whole instead of after other servers already switched to the new
// workload.
func CheckJournalCapability(conn Connector, spec sessionspec.Spec, timeout time.Duration) error {
	if missingJournalCapability(conn.Handler(), spec, timeout) {
		return ErrJournalUnsupported
	}
	return nil
}

// sendCommandStream sends commands, the plain command stream of a
// non-interactive client (session.Spec.Commands), one command frame each. A
// stream holding a read command is enclosed in an input batch
// (protocol.InputBatchBeginCommand ... InputBatchCompleteCommand), so that
// dserver neither ends the session nor finishes a MapReduce aggregate's
// input before every command of the stream was admitted.
func sendCommandStream(handler handlers.Handler, commands []string) error {
	batched := containsReadCommand(commands)
	if batched {
		if err := handler.SendMessage(protocol.InputBatchBeginCommand); err != nil {
			return err
		}
	}
	for _, command := range commands {
		if err := handler.SendMessage(command); err != nil {
			return err
		}
	}
	if batched {
		if err := handler.SendMessage(protocol.InputBatchCompleteCommand); err != nil {
			return err
		}
	}
	return nil
}

// containsReadCommand reports whether commands hold a cat, grep or tail read
// command, also in the "timeout N <mode> <path> <regex>" form. It looks at
// the command name only, which comes before the path, and the path of a read
// command is a single word anyway (protocol.EncodeReadPath encodes a path
// containing a space).
func containsReadCommand(commands []string) bool {
	for _, command := range commands {
		fields := strings.Fields(command)
		if len(fields) == 0 {
			continue
		}
		commandIndex := 0
		if fields[0] == "timeout" && len(fields) >= 3 {
			commandIndex = 2
		}
		commandName := strings.SplitN(fields[commandIndex], ":", 2)[0]
		switch commandName {
		case "cat", "grep", "tail":
			return true
		}
	}
	return false
}

func drainSessionAcks(handler handlers.Handler) {
	for {
		if _, ok := handler.WaitForSessionAck(0, 0); !ok {
			return
		}
	}
}

func resolveSessionAckTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return defaultSessionAckTimeout
	}
	return timeout
}
