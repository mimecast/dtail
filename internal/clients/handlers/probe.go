package handlers

import (
	"context"
	"errors"
	"fmt"

	"github.com/mimecast/dtail/internal/protocol"
)

// probeAnswerQueueSize is how many received probe answers a handler queues
// for Probe. A probe client waits for one answer at a time, so a full queue
// only holds stale answers of requests it gave up on; more are dropped.
const probeAnswerQueueSize = 4

var (
	// ErrProbeUnsupported is returned by Probe of a handler that does not
	// probe: a health check handler, whose server session ends after its
	// first command (it has Probe only as it shares baseHandler).
	ErrProbeUnsupported = errors.New("handler does not probe")
	// ErrProbeSessionEnded is returned by Probe when the session ended
	// before the answer arrived.
	ErrProbeSessionEnded = errors.New("session ended before the probe answer arrived")
)

// Prober is implemented by the client handlers that can ask their server
// which MapReduce tables, fields and $variables some files carry (the probe
// command, see protocol.ProbeRequest and docs/dev/probe.md). The interactive
// query control's background prober (internal/clients/interactive_probe.go)
// type-asserts a connection's handler to it, so test handlers need not
// implement it.
type Prober interface {
	// Probe sends request once the session is up and returns the server's
	// answer to it, or an error when ctx ends or the session ends first.
	Probe(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error)
}

var (
	_ Prober = (*ClientHandler)(nil)
	_ Prober = (*MaprHandler)(nil)
)

// Probe implements Prober. It waits for the server's capabilities frame
// (the session is up then), drops the answers still queued (late answers of
// requests an earlier call gave up on, see dropQueuedProbeAnswers), sends
// the probe command and waits for the answer with request's id; an answer
// with another id that arrives meanwhile (one still on its way) is skipped.
// One call runs at a time per handler, as the server runs one probe at a
// time per session (a second one would be answered with
// protocol.ProbeErrorBusy): a concurrent call waits for the running one
// (acquireProbeSlot), as long as its ctx allows. So the path completion's
// on-demand listing shares the slot with the background field probe rather
// than failing or preempting it: cancelling the running call would free
// only the client's slot, while dserver went on sampling and answered the
// listing with protocol.ProbeErrorBusy. Waiting instead costs at most the
// rest of a running probe, which the server bounds (10 s of sampling), and
// with the output running the running probe's answer is read at once. Probe
// runs on the caller's goroutine and never blocks the handler's Write,
// which only queues answers (handleProbeMessage).
func (h *baseHandler) Probe(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	if h.probeAnswers == nil {
		return protocol.ProbeAnswer{}, ErrProbeUnsupported
	}
	command, err := protocol.EncodeProbeCommand(request)
	if err != nil {
		return protocol.ProbeAnswer{}, err
	}
	if err := h.acquireProbeSlot(ctx); err != nil {
		return protocol.ProbeAnswer{}, err
	}
	defer func() { <-h.probeSlot }()

	if err := h.waitForSessionUp(ctx); err != nil {
		return protocol.ProbeAnswer{}, err
	}
	h.dropQueuedProbeAnswers()
	if err := h.sendMessage(ctx, command); err != nil {
		return protocol.ProbeAnswer{}, fmt.Errorf("send probe: %w", err)
	}
	return h.waitForProbeAnswer(ctx, request.ID)
}

// acquireProbeSlot waits until no other Probe call of the handler runs and
// takes the slot, or returns why it gave up: ctx ended, or the session
// ended (no answer could arrive any more).
func (h *baseHandler) acquireProbeSlot(ctx context.Context) error {
	select {
	case h.probeSlot <- struct{}{}:
		return nil
	case <-h.Done():
		return ErrProbeSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dropQueuedProbeAnswers drops every answer queued before a new request is
// sent: they answer requests an earlier call gave up on, so none of them
// can be taken for the new request's answer, even should an id repeat.
func (h *baseHandler) dropQueuedProbeAnswers() {
	for {
		select {
		case answer := <-h.probeAnswers:
			h.log().Debug(h.server, "Dropping a late probe answer", answer.ID)
		default:
			return
		}
	}
}

// waitForSessionUp waits until the server's capabilities frame arrived. A
// frame that already arrived wins over an ended ctx or session (see
// WaitForCapabilities for why the non-blocking check comes first).
func (h *baseHandler) waitForSessionUp(ctx context.Context) error {
	if h.capabilitiesCh == nil {
		return ErrProbeUnsupported
	}
	select {
	case <-h.capabilitiesCh:
		return nil
	default:
	}
	select {
	case <-h.capabilitiesCh:
		return nil
	case <-h.Done():
		return ErrProbeSessionEnded
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitForProbeAnswer returns the next queued answer with the given id,
// skipping answers with another id. A queued answer wins over an ended ctx
// or session.
func (h *baseHandler) waitForProbeAnswer(ctx context.Context, id string) (protocol.ProbeAnswer, error) {
	for {
		var answer protocol.ProbeAnswer
		select {
		case answer = <-h.probeAnswers:
		default:
			select {
			case answer = <-h.probeAnswers:
			case <-h.Done():
				return protocol.ProbeAnswer{}, ErrProbeSessionEnded
			case <-ctx.Done():
				return protocol.ProbeAnswer{}, ctx.Err()
			}
		}
		if answer.ID == id {
			return answer, nil
		}
		h.log().Debug(h.server, "Skipping a stale probe answer", answer.ID, "waiting for", id)
	}
}

// handleProbeMessage queues the probe answer of a hidden message (starting
// with protocol.HiddenProbePrefix) for Probe. It never blocks the frame
// reader: an answer that does not decode, or that does not fit into the
// queue, is dropped and only logged at debug level (a handler that does not
// probe ignores it), as the probe is a
// background job whose failures must not reach the terminal.
func (h *baseHandler) handleProbeMessage(message string) {
	if h.probeAnswers == nil {
		return
	}
	answer, err := protocol.DecodeProbeAnswer(message)
	if err != nil {
		h.log().Debug(h.server, "Dropping a probe answer that does not decode", err)
		return
	}
	select {
	case h.probeAnswers <- answer:
	default:
		h.log().Debug(h.server, "Dropping a probe answer as the queue is full", answer.ID)
	}
}
