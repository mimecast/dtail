package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

const (
	sessionAckStartOKPrefix  = ".syn session start ok"
	sessionAckUpdateOKPrefix = ".syn session update ok"
	sessionAckErrorPrefix    = ".syn session err "
)

// sessionCommandState tracks the interactive session of one connection.
//
// mu guards active, spec and cancel, and serializes every write of
// generation. generation is additionally atomic so the per-line output gate
// (currentGeneration, reached through shouldWriteGeneration and
// outputCoordinator.shouldDropGeneration) reads it without taking mu. No reader
// needs generation to be consistent with the other fields: every reader of
// generation reads only generation, and the writers store it while holding mu,
// so the fields still change together for anyone who takes mu. update stores
// the new generation before cancelling the previous command context, so a
// goroutine that observes that cancellation also observes the new generation.
//
// generation is the server's own number of the running workload: it goes up
// by one with every started workload of the connection and is never reused,
// so that output still queued from any earlier workload is always stale
// (outputCoordinator.shouldDropGeneration compares for equality). The number
// the client knows a workload by (acked: 1 for a start, the client's own
// number for an update) is only acknowledged. The two used to be one number,
// and a client is free to number an update like the running workload (its
// rollback after an acknowledgement timeout does, when the server did apply
// the update) or like an earlier one, whose queued output was then delivered
// as output of the new workload.
type sessionCommandState struct {
	mu         sync.Mutex
	active     bool
	generation atomic.Uint64
	// workloads counts the workloads started on the connection; acked is
	// the client's number of the running one. Both are guarded by mu.
	workloads uint64
	acked     uint64
	spec      session.Spec
	cancel    context.CancelFunc
}

// nextGeneration returns the server's number for the workload about to
// start. The caller holds mu.
func (s *sessionCommandState) nextGeneration() uint64 {
	s.workloads++
	return s.workloads
}

// handleSessionCommand handles SESSION START and SESSION UPDATE. An error
// acknowledgement may carry the reason a query was rejected, which contains
// the client's query text; escaping (sessionFraming) carries every byte of
// it, so it is sent as it is.
//
// Every acknowledgement goes to the session's acknowledgement queue, not to
// the server messages: the output of the started workload is held back
// until its acknowledgement went out (sessionAckGate), so that a client on a
// slow link gets it in time rather than behind the session output buffer.
func (h *ServerHandler) handleSessionCommand(parentCtx context.Context, _ lcontext.LContext, argc int, args []string, commandFinished func()) {
	defer commandFinished()

	action, acked, spec, err := parseSessionCommand(args, argc, h.Logger())
	if err != nil {
		h.sendSessionError(err.Error())
		return
	}

	var prefix string
	var generation uint64
	switch action {
	case "START":
		prefix = sessionAckStartOKPrefix
		acked, generation, err = h.sessionState.start(parentCtx, h, spec)
	case "UPDATE":
		if !h.sessionState.activeSession() {
			h.sendSessionError("session not started")
			return
		}
		prefix = sessionAckUpdateOKPrefix
		acked, generation, err = h.sessionState.update(parentCtx, h, spec, acked)
	default:
		h.sendSessionError("unknown action")
		return
	}
	if err != nil {
		h.sendSessionError(err.Error())
		return
	}
	h.sendSessionAck(sessionAck{message: fmt.Sprintf("%s %d", prefix, acked), generation: generation})
}

// sendSessionError sends a session error acknowledgement. Clients take the
// whole rest of the acknowledgement as the error and show it, so it may say
// why a query was rejected. It releases no held output: a workload that
// failed to start released its own (sessionCommandState.dispatch).
func (h *ServerHandler) sendSessionError(reason string) {
	h.sendSessionAck(sessionAck{message: sessionAckErrorPrefix + reason})
}

func parseSessionCommand(args []string, argc int, logger logging.Logger) (action string, generation uint64, spec session.Spec, err error) {
	if argc < 3 {
		return "", 0, spec, fmt.Errorf("invalid SESSION command")
	}

	action = strings.ToUpper(strings.TrimSpace(args[1]))
	payloadIndex := 2
	if action == "UPDATE" && argc >= 4 {
		generation, err = strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return "", 0, spec, fmt.Errorf("invalid session generation")
		}
		payloadIndex = 3
	}

	payload, err := base64.StdEncoding.DecodeString(args[payloadIndex])
	if err != nil {
		return "", 0, spec, fmt.Errorf("invalid session payload")
	}
	if unmarshalErr := json.Unmarshal(payload, &spec); unmarshalErr != nil {
		return "", 0, spec, fmt.Errorf("invalid session spec")
	}
	if validationErr := validateSessionSpec(spec, logger); validationErr != nil {
		return "", 0, spec, validationErr
	}

	return action, generation, spec, nil
}

func validateSessionSpec(spec session.Spec, logger logging.Logger) error {
	switch spec.Mode {
	case omode.TailClient, omode.CatClient, omode.GrepClient, omode.MapClient, omode.HealthClient:
	default:
		return fmt.Errorf("unsupported session mode")
	}

	if spec.Query != "" && spec.Mode != omode.MapClient && spec.Mode != omode.TailClient {
		return fmt.Errorf("query sessions require map or tail mode")
	}

	if spec.Query == "" && spec.Mode == omode.MapClient {
		return fmt.Errorf("missing session query")
	}
	if spec.Query != "" {
		if _, err := mapr.NewQuery(spec.Query, logger); err != nil {
			return fmt.Errorf("invalid session spec: %w", err)
		}
	}

	if err := validateSessionOptions(spec.Options); err != nil {
		return err
	}

	if _, err := spec.Commands(); err != nil {
		return fmt.Errorf("invalid session spec")
	}

	return nil
}

// start starts the session with generation 1, the number the client knows
// it by (acked), and returns that and the server's number of the workload.
// Its map command, if any, is built before the session changes, so that one
// that can not be built rejects the request (see prepareSessionMapCommand).
func (s *sessionCommandState) start(parentCtx context.Context, handler *ServerHandler,
	spec session.Spec) (acked, generation uint64, err error) {

	commands, err := prepareSessionCommands(spec)
	if err != nil {
		return 0, 0, err
	}
	// Checked before building the map command too, so that a repeated START
	// gets this error rather than one of its query, without building it.
	if s.activeSession() {
		return 0, 0, fmt.Errorf("session already started")
	}
	ctx, err := handler.prepareSessionMapCommand(
		sessionCommandContext(handler.commandRootCtx, parentCtx), commands)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid session query: %w", err)
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("session already started")
	}
	ctx, cancel := context.WithCancel(ctx)
	generation = s.nextGeneration()
	s.active = true
	s.generation.Store(generation)
	s.acked = 1
	s.spec = spec
	s.cancel = cancel
	s.mu.Unlock()

	if err := s.dispatch(ctx, handler, commands, generation, cancel); err != nil {
		return 0, 0, err
	}
	return 1, generation, nil
}

// update replaces the running generation with a new one, which the client
// knows by its own number (clientGeneration) or else the next one, and
// returns that number (acked) and the server's number of the workload. Its
// map command, if any, is built before the previous generation is
// cancelled, so that one that can not be built rejects the update while the
// running generation keeps running (see prepareSessionMapCommand).
func (s *sessionCommandState) update(parentCtx context.Context, handler *ServerHandler,
	spec session.Spec, clientGeneration uint64) (acked, generation uint64, err error) {

	commands, err := prepareSessionCommands(spec)
	if err != nil {
		return 0, 0, err
	}
	ctx, err := handler.prepareSessionMapCommand(
		sessionCommandContext(handler.commandRootCtx, parentCtx), commands)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid session query: %w", err)
	}

	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("session not started")
	}
	oldCancel := s.cancel
	ctx, cancel := context.WithCancel(ctx)
	// acked is what the client knows the workload by: its own number, or
	// else the next one. generation is the server's (see the type comment).
	acked = clientGeneration
	if acked == 0 {
		acked = s.acked + 1
	}
	generation = s.nextGeneration()
	s.active = true
	s.generation.Store(generation)
	s.acked = acked
	s.spec = spec
	s.cancel = cancel
	s.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
	if err := s.dispatch(ctx, handler, commands, generation, cancel); err != nil {
		return 0, 0, err
	}
	return acked, generation, nil
}

// dispatch dispatches the commands of the new workload generation. It first
// holds back the output of the generation until the workload's
// acknowledgement went out (sessionAckGate); no output of it exists before
// the dispatch. A workload that fails to start is cancelled, the session
// reset and the hold released, as no ok acknowledgement follows.
func (s *sessionCommandState) dispatch(ctx context.Context, handler *ServerHandler,
	commands []string, generation uint64, cancel context.CancelFunc) error {

	handler.holdSessionOutput(generation)
	ctx = withSessionGeneration(ctx, generation)
	handler.mapQueryFailures.clear(generation)
	handler.resetSessionAggregates()
	if err := handler.dispatchSessionCommands(ctx, commands); err != nil {
		cancel()
		s.reset()
		handler.releaseSessionOutput(generation)
		return err
	}
	return nil
}

func prepareSessionCommands(spec session.Spec) ([]string, error) {
	commands, err := spec.Commands()
	if err != nil {
		return nil, fmt.Errorf("invalid session spec")
	}

	return commands, nil
}

func sessionCommandContext(connCtx, parent context.Context) context.Context {
	if connCtx == nil || parent == nil {
		panic("handlers: nil session command context")
	}
	ctx := connCtx
	if hasSessionCommandAdmission(parent) {
		ctx = withSessionCommandAdmission(ctx)
	}
	return ctx
}

func validateSessionOptions(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}

	if _, _, err := config.DeserializeOptions(strings.Split(raw, ":")); err != nil {
		return fmt.Errorf("invalid session spec")
	}

	return nil
}

func (s *sessionCommandState) activeSession() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *sessionCommandState) keepAlive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// currentGeneration returns the active session generation, or 0 when no
// session is active. It takes no lock: it runs once per output line.
func (s *sessionCommandState) currentGeneration() uint64 {
	return s.generation.Load()
}

func (s *sessionCommandState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.active = false
	s.generation.Store(0)
	s.acked = 0
	s.spec = session.Spec{}
	s.cancel = nil
}

func (h *ServerHandler) dispatchSessionCommands(ctx context.Context, commands []string) error {
	batch := &commandBatch{}
	batch.begin()
	ctx = withCommandBatch(ctx, batch)
	defer h.finishCommandBatch(ctx, batch)

	for _, command := range commands {
		commandCtx := ctx
		var admission *commandAdmissionResult
		if hasSessionCommandAdmission(ctx) {
			admission = &commandAdmissionResult{}
			commandCtx = context.WithValue(commandCtx, commandAdmissionResultKey, admission)
		}
		if err := h.handleRawCommand(commandCtx, command); err != nil {
			return err
		}
		if admission != nil && !admission.admitted {
			return fmt.Errorf("server is shutting down")
		}
		// start and update built the map command before dispatching it (see
		// prepareSessionMapCommand), so it does not fail here as things
		// stand. Should it nevertheless, its reads are rejected (see
		// mapQueryFailures); stop dispatching them and let the caller reject
		// the request rather than acknowledge a workload without its
		// aggregate.
		if h.mapQueryFailures.failed(sessionGenerationFromContext(ctx)) {
			return fmt.Errorf("invalid session query")
		}
	}
	return nil
}

// resetSessionAggregates shuts down any active aggregates and clears the
// atomic pointers. This is called on session reload to ensure stale aggregates
// from the previous generation are not reused.
func (h *ServerHandler) resetSessionAggregates() {
	if ta := h.getAggregate(); ta != nil {
		ta.Abort()
		h.setAggregate(nil)
	}
}
