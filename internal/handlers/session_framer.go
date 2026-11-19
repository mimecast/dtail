package handlers

import (
	"bytes"
	"io"

	"github.com/mimecast/dtail/internal/protocol"
)

// sessionFraming is the framing of every frame a session sends, from its
// first frame (the capabilities frame) on. Since protocol 5 there is no
// negotiation: client and server both use protocol.EscapedControlFraming,
// and a client of another protocol version is rejected by the version check
// (commandDispatcher.rejectProtocolVersion).
const sessionFraming = protocol.EscapedControlFraming

// sessionFramer owns byte-stream boundaries in both directions. The SSH
// transport presents arbitrary chunks, so command frames and output messages
// must retain partial bytes across calls. Every output frame is encoded in
// sessionFraming.
type sessionFramer struct {
	handler  *baseHandler
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer

	maxCommandFrameSize int
	// maprTail is set while readBuf holds the rest of a coalesced mapr batch
	// (readMaprMessage) that did not fit into the Read buffer, so that the
	// next Read frames the mapr messages queued meanwhile behind it
	// (drainPending). Only the output goroutine (Read) touches it.
	maprTail bool
}

func newSessionFramer(handler *baseHandler, maxCommandFrameSize int) *sessionFramer {
	return &sessionFramer{
		handler:             handler,
		maxCommandFrameSize: maxCommandFrameSize,
	}
}

// queueFirstFrame makes message (a hidden control message) the first frame
// the session sends: it is encoded into readBuf, which Read drains before it
// takes anything from the output queues. It must be called before the
// output goroutine reads, i.e. while the handler is built.
func (f *sessionFramer) queueFirstFrame(message string) {
	sessionFraming.EncodeControl(&f.readBuf, message)
}

// Read emits one or more bytes from the next coordinated output item; a
// MapReduce message takes the mapr messages queued behind it along
// (readMaprMessage), and so does the rest of such a batch that did not fit
// into the previous Read (drainPending).
func (f *sessionFramer) Read(p []byte) (n int, err error) {
	c := f.handler.outputCoordinator
	c.ensureFlushChannels()
	c.readerSeen.Store(true)
	// This call means the bytes the previous one returned were delivered.
	c.deliveryPending.Store(false)
	c.readActive.Store(true)
	defer func() {
		if n > 0 {
			c.deliveryPending.Store(true)
		}
		c.readActive.Store(false)
	}()

	if f.readBuf.Len() > 0 {
		return f.drainPending(p), nil
	}

	for {
		result := c.next(p)
		if result.err != nil {
			return 0, result.err
		}
		switch result.kind {
		case outputReadBytes:
			return result.n, nil
		case outputReadServerMessage:
			n = f.readServerMessage(p, result.message)
		case outputReadMaprMessage:
			n = f.readMaprMessage(p, result.message)
		case outputReadRetry:
			continue
		}
		if n > 0 {
			return n, nil
		}
	}
}

// Write accumulates command bytes until a semicolon delimiter is received.
func (f *sessionFramer) Write(p []byte) (n int, err error) {
	h := f.handler
	for _, b := range p {
		switch b {
		case ';':
			h.handleCommand(f.writeBuf.String())
			f.writeBuf.Reset()
		default:
			f.writeBuf.WriteByte(b)
			if f.maxCommandFrameSize > 0 && f.writeBuf.Len() > f.maxCommandFrameSize {
				h.Logger().Error(h.user,
					"command frame exceeds maximum size, closing session",
					"frameSize", f.writeBuf.Len(),
					"limit", f.maxCommandFrameSize,
				)
				f.writeBuf.Reset()
				h.cancelCommands()
				h.done.Shutdown()
				return len(p), io.ErrClosedPipe
			}
		}
	}
	return len(p), nil
}

// readServerMessage frames one message of the server itself. Control
// messages (hidden ones and AUTHKEY replies) become marked control frames
// (protocol.Framing.EncodeControl). Server messages other than hidden ones,
// AUTHKEY replies included, are dropped in serverless mode, and empty ones in
// every mode (isEmptyServerMessage).
func (f *sessionFramer) readServerMessage(p []byte, message string) int {
	h := f.handler
	generation, decodedMessage := decodeGeneratedMessage(message)
	if h.shouldDropGeneration(generation) {
		return 0
	}
	message = decodedMessage
	switch {
	case protocol.IsHiddenMessage(message):
		sessionFraming.EncodeControl(&f.readBuf, message)
		return f.drainReadBuf(p)
	case h.serverless || isEmptyServerMessage(message):
		return 0
	case protocol.IsControlMessage(message):
		sessionFraming.EncodeControl(&f.readBuf, message)
		return f.drainReadBuf(p)
	}
	kind := protocol.MessageServer
	// The command input goroutine may set the plain mode meanwhile
	// (commandDispatcher.handleOptions), hence the atomic read.
	if h.plainMode() {
		kind = protocol.MessagePlain
	}
	sessionFraming.EncodeMessage(&f.readBuf, protocol.Message{
		Kind:     kind,
		Hostname: h.hostname,
		Content:  message,
	})
	return f.drainReadBuf(p)
}

// readMaprMessage frames a MapReduce message together with the mapr
// messages queued behind it, up to len(p) bytes, so that one Read, and with
// it one SSH channel write (one packet, one write(2) and one client read),
// carries many of them. A GroupSet serializes every group as a message of
// its own, and a high-cardinality query (200k groups) otherwise spends about
// a third of dserver's CPU time in per-packet flush syscalls.
//
// Coalescing changes no order: it only takes a mapr message where the next
// Read would have taken it as well (outputCoordinator.tryReadQueuedMapr),
// and each message is still filtered by its generation on its own. The last
// frame may not fit into p; its rest stays in readBuf, which the next Read
// drains before anything else, as for any frame longer than p, and fills up
// with the mapr messages queued by then (drainPending, maprTail).
func (f *sessionFramer) readMaprMessage(p []byte, message string) int {
	f.encodeMaprMessage(message)
	f.coalesceQueuedMapr(len(p))
	return f.drainMaprBatch(p)
}

// drainPending returns the bytes a previous Read encoded but could not
// return. When they are the rest of a coalesced mapr batch (maprTail), the
// mapr messages queued since are framed behind them first, under the same
// rule as in readMaprMessage: the next Read would have taken those messages
// right after the rest anyway. Otherwise the rest of a full batch would go
// out alone, as an extra tiny SSH write per batch. The rest of any other
// frame (e.g. a long server message) is returned on its own, as before.
func (f *sessionFramer) drainPending(p []byte) int {
	if !f.maprTail {
		return f.drainReadBuf(p)
	}
	f.coalesceQueuedMapr(len(p))
	return f.drainMaprBatch(p)
}

// coalesceQueuedMapr frames queued mapr messages behind readBuf until it
// holds at least limit bytes or no message can be taken.
func (f *sessionFramer) coalesceQueuedMapr(limit int) {
	c := f.handler.outputCoordinator
	for f.readBuf.Len() < limit {
		queued, ok := c.tryReadQueuedMapr()
		if !ok {
			return
		}
		f.encodeMaprMessage(queued)
	}
}

// drainMaprBatch returns the first len(p) bytes of a coalesced mapr batch
// and records whether a rest of it stays in readBuf (maprTail).
func (f *sessionFramer) drainMaprBatch(p []byte) int {
	n := f.drainReadBuf(p)
	f.maprTail = f.readBuf.Len() > 0
	return n
}

// encodeMaprMessage appends the aggregate frame of message to readBuf,
// unless message belongs to a generation the session no longer delivers.
func (f *sessionFramer) encodeMaprMessage(message string) {
	h := f.handler
	generation, decodedMessage := decodeGeneratedMessage(message)
	if h.shouldDropGeneration(generation) {
		return
	}
	sessionFraming.EncodeMessage(&f.readBuf, protocol.Message{
		Kind:     protocol.MessageAggregate,
		Hostname: h.hostname,
		Content:  decodedMessage,
	})
}

func (f *sessionFramer) drainReadBuf(p []byte) int {
	n, _ := f.readBuf.Read(p)
	f.handler.readBufferedBytes.Store(int64(f.readBuf.Len()))
	return n
}

var _ io.ReadWriter = (*sessionFramer)(nil)
