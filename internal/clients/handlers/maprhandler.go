package handlers

import (
	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// MaprHandler is the handler used on the client side for running mapreduce
// aggregations.
type MaprHandler struct {
	baseHandler
	aggregate *client.Aggregate
	removedNl bool
}

// NewMaprHandler returns a new mapreduce client handler.
func NewMaprHandler(server string, session *client.SessionState, logger clientlog.Logger) *MaprHandler {
	logger = clientlog.OrNop(logger)
	return &MaprHandler{
		baseHandler: baseHandler{
			server:         server,
			shellStarted:   false,
			commands:       make(chan string),
			status:         -1,
			done:           internal.NewDone(),
			capabilities:   make(map[string]struct{}),
			capabilitiesCh: make(chan struct{}),
			sessionAcks:    make(chan SessionAck, 4),
			logger:         logger,
		},
		aggregate: client.NewAggregate(server, session, logger),
	}
}

// Read data from the dtail server via Writer interface.
func (h *MaprHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n':
			h.removedNl = true
		case protocol.MessageDelimiter:
			message := h.receiveBuf.String()
			if len(message) == 0 {
				h.receiveBuf.Reset()
				h.removedNl = false
				continue
			}
			h.log().Debug(message)
			if aggregateMessage, decodeErr, ok := decodeAggregateMessage(message); ok {
				h.handleAggregateMessage(message, aggregateMessage, decodeErr)
			} else {
				if h.removedNl {
					h.receiveBuf.WriteByte('\n')
				}
				h.handleMessage(h.receiveBuf.Bytes())
			}
			h.receiveBuf.Reset()
			h.removedNl = false
		default:
			h.receiveBuf.WriteByte(b)
		}
	}

	return len(p), nil
}

// isAggregateMessage reports whether a wire message starts with a mapreduce
// aggregate tag. Valid frames are handed to the aggregate parser; malformed
// tagged frames are reported as protocol errors. Matching the full
// AggregateMessageID field prefix, rather than just the first byte 'A', keeps
// plain-mode protocol acks such as "AUTHKEY OK" out of the parser; those would
// otherwise trigger a spurious "Unable to aggregate data ... expected 3
// parts" error. Non-aggregate messages are routed to the base handler, which
// recognises acks as control.
func isAggregateMessage(message string) bool {
	_, _, ok := decodeAggregateMessage(message)
	return ok
}

// Handle a message received from server including mapr aggregation related data.

func (h *MaprHandler) handleAggregateMessage(message string, decoded protocol.Message, decodeErr error) {
	if decodeErr != nil {
		h.log().Error("Unable to decode aggregate data", h.server, message, decodeErr)
		return
	}
	if err := h.aggregate.Aggregate(decoded.Content); err != nil {
		h.log().Error("Unable to aggregate data", h.server, message, err)
	}
}

func decodeAggregateMessage(message string) (protocol.Message, error, bool) {
	decoded, err := protocol.DecodeMessage(message)
	return decoded, err, decoded.Kind == protocol.MessageAggregate
}

// Shutdown flushes any pending aggregate state before marking the handler done.
func (h *MaprHandler) Shutdown() {
	if h.aggregate != nil {
		if err := h.aggregate.Flush(); err != nil {
			h.log().Error("Unable to flush aggregate data on shutdown", h.server, err)
		}
	}
	h.baseHandler.Shutdown()
}
