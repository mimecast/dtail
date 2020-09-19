package handlers

import (
	"strings"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/client"
)

// MaprHandler is the handler used on the client side for running mapreduce aggregations.
type MaprHandler struct {
	baseHandler
	aggregate *client.Aggregate
	query     *mapr.Query
	count     uint64
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
		h.baseHandler.receiveBuf = append(h.baseHandler.receiveBuf, b)
		if b == '\n' {
			if len(h.baseHandler.receiveBuf) == 0 {
				continue
			}
			message := string(h.baseHandler.receiveBuf)

			if h.baseHandler.receiveBuf[0] == 'A' {
				h.handleAggregateMessage(strings.TrimSpace(message))
				h.baseHandler.receiveBuf = h.baseHandler.receiveBuf[:0]
				continue
			}
			h.baseHandler.handleMessageType(message)
		}
	}

	return len(p), nil
}

// Handle a message received from server including mapr aggregation
// related data.
func (h *MaprHandler) handleAggregateMessage(message string) {
	h.count++
	parts := strings.Split(message, "➔")

	// Index 0 contains 'AGGREGATE', 1 contains server host.
	// Aggregation data begins from index 2.
	logger.Debug("Received aggregate data", h.server, h.count, parts)
	h.aggregate.Aggregate(parts[2:])
	logger.Debug("Aggregated aggregate data", h.server, h.count)
}
