package handlers

import (
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// MaprHandler is the handler used on the client side for running mapreduce aggregations.
type MaprHandler struct {
	baseHandler
	aggregate *client.Aggregate
	query     *mapr.Query
}

// NewMaprHandler returns a new mapreduce client handler.
func NewMaprHandler(server string, query *mapr.Query, globalGroup *mapr.GlobalGroupSet) *MaprHandler {
	return &MaprHandler{
		baseHandler: baseHandler{
			server:       server,
			shellStarted: false,
			commands:     make(chan string),
			status:       -1,
			done:         internal.NewDone(),
		},
		query:     query,
		aggregate: client.NewAggregate(server, query, globalGroup),
	}
}

// Read data from the dtail server via Writer interface.
func (h *MaprHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case '\n':
			continue
		case protocol.MessageDelimiter:
			message := h.baseHandler.receiveBuf.String()
			dlog.Client.Debug(message)
			if message[0] == 'A' {
				h.handleAggregateMessage(message)
			} else {
				h.baseHandler.handleMessageType(message)
			}
			h.baseHandler.receiveBuf.Reset()
		default:
			h.baseHandler.receiveBuf.WriteByte(b)
		}
	}

	return len(p), nil
}

// Handle a message received from server including mapr aggregation
// related data.
func (h *MaprHandler) handleAggregateMessage(message string) {
	parts := strings.SplitN(message, protocol.FieldDelimiter, 3)
	if len(parts) != 3 {
		dlog.Client.Error("Unable to aggregate data", h.server, message, parts, len(parts), "expected 3 parts")
		return
	}
	if err := h.aggregate.Aggregate(parts[2]); err != nil {
		dlog.Client.Error("Unable to aggregate data", h.server, message, err)
	}
}
