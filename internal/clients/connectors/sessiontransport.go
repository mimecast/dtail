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
	// ErrSessionUnsupported indicates that the remote side did not advertise
	// runtime query update support.
	ErrSessionUnsupported = errors.New("runtime query updates unsupported by server")
	// ErrSessionAckTimeout indicates that no hidden SESSION acknowledgement arrived in time.
	ErrSessionAckTimeout = errors.New("timed out waiting for session acknowledgement")
	// ErrSessionRejected indicates that the server explicitly rejected a SESSION request.
	ErrSessionRejected = errors.New("session request rejected")
	// ErrUnexpectedSessionAck indicates that the client received a malformed or mismatched acknowledgement.
	ErrUnexpectedSessionAck = errors.New("unexpected session acknowledgement")
	// ErrJournalUnsupported indicates that the remote side did not advertise journal support.
	ErrJournalUnsupported = errors.New(capabilityMessage(journalTargetsWhat, protocol.CapabilityJournalV1))
	// ErrEncodedReadPathUnsupported indicates that the remote side did not
	// advertise support for encoded read paths, so it cannot read a path
	// containing a space or starting with base64% (protocol.ReadPathNeedsEncoding).
	ErrEncodedReadPathUnsupported = errors.New(capabilityMessage(encodedReadPathsWhat, protocol.CapabilityEncodedReadPathV1))
)

const (
	journalTargetsWhat   = "journal file targets"
	encodedReadPathsWhat = "file paths containing spaces or starting with " + protocol.EncodedReadPathPrefix
)

const defaultSessionAckTimeout = 2 * time.Second

type committedSessionState struct {
	applyMu    sync.Mutex
	mu         sync.RWMutex
	committed  bool
	generation uint64
	spec       sessionspec.Spec
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

func (s *committedSessionState) snapshot() (sessionspec.Spec, uint64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.spec, s.generation, s.committed
}

func dispatchInitialCommands(server string, handler handlers.Handler, commands []string,
	interactiveQuery bool, initialSpec sessionspec.Spec, state *committedSessionState,
	logger logging.Logger) error {
	logger = logging.OrNop(logger)

	if !interactiveQuery || initialSpec.Mode == omode.Unknown {
		if err := requireSpecCapabilities(server, handler, initialSpec, defaultCapabilityWait, true); err != nil {
			return err
		}
		return sendLegacyCommands(handler, commands)
	}

	if err := applySessionSpec(server, handler, state, initialSpec, defaultSessionAckTimeout, logger); err != nil {
		if !errors.Is(err, ErrSessionUnsupported) {
			state.clear()
			return err
		}

		logger.Warn(server, "Interactive session bootstrap unsupported, falling back to legacy commands", err)
		state.clear()
		return sendLegacyCommands(handler, commands)
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
	logger = logging.OrNop(logger)

	// Serialize session transitions so an interactive reload cannot race the
	// initial SESSION START bootstrap on the same connection.
	state.applyMu.Lock()
	defer state.applyMu.Unlock()

	if useCurrentGeneration {
		_, generation, _ = state.snapshot()
	}

	// Only a failed SESSION START (generation 0) is reported to the user and
	// sets the exit status: it fails the initial workload. A rejected UPDATE
	// is answered on the interactive TTY and the client keeps running.
	if err := requireSpecCapabilities(server, handler, spec, defaultCapabilityWait, generation == 0); err != nil {
		return err
	}
	if !supportsQueryUpdates(handler, defaultCapabilityWait) {
		return ErrSessionUnsupported
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

	ack, ok := handler.WaitForSessionAck(resolveSessionAckTimeout(timeout))
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

// specCapability is a server capability that some read targets of a session
// spec need. what names those targets in the message "<what> require server
// capability <capability>", which is also the text of sentinel.
type specCapability struct {
	capability string
	what       string
	sentinel   error
	neededBy   func(sessionspec.Spec) bool
}

// specCapabilities lists every capability a read target can need, in the
// order they are checked: journal targets (journal-v1), and paths that have
// to be encoded, i.e. containing a space or starting with base64%
// (encoded-read-path-v1, see sessionspec.Spec.HasEncodedFiles). An older
// server would split an encoded-needing path at its spaces and fail with
// "Unable to parse command", or look for a file named after the encoded
// word; the client fails clearly instead, before sending any read command.
// Space-free paths need no capability and keep working with every server.
var specCapabilities = []specCapability{
	{
		capability: protocol.CapabilityJournalV1,
		what:       journalTargetsWhat,
		sentinel:   ErrJournalUnsupported,
		neededBy:   sessionspec.Spec.HasJournalFiles,
	},
	{
		capability: protocol.CapabilityEncodedReadPathV1,
		what:       encodedReadPathsWhat,
		sentinel:   ErrEncodedReadPathUnsupported,
		neededBy:   sessionspec.Spec.HasEncodedFiles,
	},
}

// message returns what the user is told when the server lacks c.
func (c specCapability) message() string {
	return capabilityMessage(c.what, c.capability)
}

func capabilityMessage(what, capability string) string {
	return what + " require server capability " + capability
}

// missingSpecCapability waits up to timeout for the capabilities of
// handler's server and returns the first capability spec needs that the
// server does not advertise. A nil handler advertises nothing.
func missingSpecCapability(handler handlers.Handler, spec sessionspec.Spec,
	timeout time.Duration) (specCapability, bool) {
	if timeout <= 0 {
		timeout = defaultCapabilityWait
	}
	advertised := false
	for _, need := range specCapabilities {
		if !need.neededBy(spec) {
			continue
		}
		if handler == nil {
			return need, true
		}
		if !advertised && !handler.WaitForCapabilities(timeout) {
			return need, true
		}
		advertised = true
		if !handler.HasCapability(need.capability) {
			return need, true
		}
	}
	return specCapability{}, false
}

// requireSpecCapabilities checks, before any read command of spec is sent
// (legacy commands, SESSION START or a :reload's SESSION UPDATE), that the
// server advertises every capability the spec's read targets need, and
// returns the missing capability's sentinel wrapped with the server name
// otherwise. With report it also reports "<what> require server capability
// <capability>" to the user, which sets a non-zero exit status: the initial
// workload must fail the client, while a rejected :reload is answered on the
// TTY and leaves the running workload, and the exit status, as they were.
func requireSpecCapabilities(server string, handler handlers.Handler, spec sessionspec.Spec,
	timeout time.Duration, report bool) error {
	missing, ok := missingSpecCapability(handler, spec, timeout)
	if !ok {
		return nil
	}
	if report && handler != nil {
		handler.ReportServerError(missing.message())
	}
	return fmt.Errorf("%w: %s", missing.sentinel, server)
}

// CheckSpecCapabilities reports whether the server of conn advertises every
// capability the read targets of spec need, waiting up to timeout for its
// capabilities. It returns the sentinel of the first missing capability
// (ErrJournalUnsupported or ErrEncodedReadPathUnsupported) or nil, and
// reports nothing to the user. An interactive :reload checks every connection
// with it before it updates any, so that a server lacking a capability
// rejects the reload as a whole instead of after other servers already
// switched to the new workload.
func CheckSpecCapabilities(conn Connector, spec sessionspec.Spec, timeout time.Duration) error {
	missing, ok := missingSpecCapability(conn.Handler(), spec, timeout)
	if !ok {
		return nil
	}
	return missing.sentinel
}

func sendLegacyCommands(handler handlers.Handler, commands []string) error {
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
		if _, ok := handler.WaitForSessionAck(0); !ok {
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
