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
type sessionCommandState struct {
	mu         sync.Mutex
	active     bool
	generation atomic.Uint64
	spec       session.Spec
	cancel     context.CancelFunc
}

func (h *ServerHandler) handleSessionCommand(parentCtx context.Context, _ lcontext.LContext, argc int, args []string, commandFinished func()) {
	defer commandFinished()

	action, generation, spec, err := parseSessionCommand(args, argc, h.Logger())
	if err != nil {
		h.send(h.serverMessages, sessionAckErrorPrefix+err.Error())
		return
	}

	switch action {
	case "START":
		generation, err = h.sessionState.start(parentCtx, h, spec)
		if err != nil {
			h.send(h.serverMessages, sessionAckErrorPrefix+err.Error())
			return
		}
		h.send(h.serverMessages, fmt.Sprintf("%s %d", sessionAckStartOKPrefix, generation))
	case "UPDATE":
		if !h.sessionState.activeSession() {
			h.send(h.serverMessages, sessionAckErrorPrefix+"session not started")
			return
		}
		generation, err = h.sessionState.update(parentCtx, h, spec, generation)
		if err != nil {
			h.send(h.serverMessages, sessionAckErrorPrefix+err.Error())
			return
		}
		h.send(h.serverMessages, fmt.Sprintf("%s %d", sessionAckUpdateOKPrefix, generation))
	default:
		h.send(h.serverMessages, sessionAckErrorPrefix+"unknown action")
	}
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
			return fmt.Errorf("invalid session spec")
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

func (s *sessionCommandState) start(parentCtx context.Context, handler *ServerHandler, spec session.Spec) (uint64, error) {
	commands, err := prepareSessionCommands(spec)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		return 0, fmt.Errorf("session already started")
	}
	ctx, cancel := context.WithCancel(sessionCommandContext(handler.commandRootCtx, parentCtx))
	s.active = true
	s.generation.Store(1)
	s.spec = spec
	s.cancel = cancel
	s.mu.Unlock()
	ctx = withSessionGeneration(ctx, 1)

	handler.resetSessionAggregates()
	if dispatchErr := handler.dispatchSessionCommands(ctx, commands); dispatchErr != nil {
		cancel()
		s.reset()
		return 0, dispatchErr
	}

	return 1, nil
}

func (s *sessionCommandState) update(parentCtx context.Context, handler *ServerHandler, spec session.Spec, generation uint64) (uint64, error) {
	commands, err := prepareSessionCommands(spec)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	if !s.active {
		s.mu.Unlock()
		return 0, fmt.Errorf("session not started")
	}
	oldCancel := s.cancel
	ctx, cancel := context.WithCancel(sessionCommandContext(handler.commandRootCtx, parentCtx))
	if generation == 0 {
		generation = s.generation.Load() + 1
	}
	s.active = true
	s.generation.Store(generation)
	s.spec = spec
	s.cancel = cancel
	s.mu.Unlock()
	ctx = withSessionGeneration(ctx, generation)

	if oldCancel != nil {
		oldCancel()
	}

	handler.resetSessionAggregates()
	if dispatchErr := handler.dispatchSessionCommands(ctx, commands); dispatchErr != nil {
		cancel()
		s.reset()
		return 0, dispatchErr
	}

	return generation, nil
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
