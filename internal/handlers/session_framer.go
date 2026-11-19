package handlers

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/protocol"
)

// sessionFramer owns byte-stream boundaries in both directions. The SSH
// transport presents arbitrary chunks, so command frames and output messages
// must retain partial bytes across calls.
//
// It also owns the session's output framing (see protocol.Framing). A session
// starts in RawFraming and switches to EscapedFraming at most once, when the
// client asks for it with the session's first command (enableEscapedFraming,
// handlingCommandFrame). The switch waits until the
// capabilities frame, the first frame of the session, has been encoded, so
// that frame is always raw and the client, which decodes every frame after
// it as escaped, agrees with the server on every frame. An escaped session
// switches on to EscapedControlFraming at most once, when the client asks
// for it with the session's second command (enableControlFrames).
//
// Read encodes no frame after the capabilities frame until the session
// framing has settled (settleFraming, awaitFramingSettled), i.e. until no
// later command can switch it any more. A client asking for marked control
// frames decides so from the capabilities frame already, so a frame encoded
// before the second command was handled, such as the close handshake of a
// dserver shutting down in the session's first moment, would reach it in
// the wrong framing.
type sessionFramer struct {
	handler  *baseHandler
	readBuf  bytes.Buffer
	writeBuf bytes.Buffer

	maxCommandFrameSize int
	// commandFrames counts the command frames Write passed on so far. It is
	// only touched by Write, i.e. by the command input goroutine.
	commandFrames int

	// framingState holds the session's protocol.Framing: RawFraming until
	// the session switched to EscapedFraming, and EscapedControlFraming once
	// it switched on to that. It is written by the command input goroutine
	// and read by the output goroutine (Read) and by the command goroutines
	// creating line writers.
	framingState atomic.Uint32
	// capabilitiesFramed is closed once Read encoded the capabilities frame.
	capabilitiesFramed     chan struct{}
	capabilitiesFramedOnce sync.Once
	// framingSettled is closed once no command can switch the session
	// framing any more (settleFraming). settledObserved caches that for Read
	// and is only touched by the output goroutine.
	framingSettled     chan struct{}
	framingSettledOnce sync.Once
	settledObserved    bool
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
		capabilitiesFramed:  make(chan struct{}),
		framingSettled:      make(chan struct{}),
	}
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
	if f.readBuf.Len() == 0 {
		// Held back without taking anything, the read is not active yet:
		// a flush with nothing queued need not wait for it.
		f.awaitFramingSettled()
	}
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
			f.commandFrames++
			h.handleCommand(f.writeBuf.String())
			f.writeBuf.Reset()
			f.settleFramingAfterCommand()
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
// messages (hidden ones and AUTHKEY replies) become control frames
// (encodeControl); the AUTHKEY replies only in sessions that mark control
// frames, as older clients expect them as server messages (or untagged in
// plain mode). Server messages are dropped in serverless mode, and empty
// ones in every mode (isEmptyServerMessage).
func (f *sessionFramer) readServerMessage(p []byte, message string) int {
	h := f.handler
	generation, decodedMessage := decodeGeneratedMessage(message)
	if h.shouldDropGeneration(generation) {
		return 0
	}
	message = decodedMessage
	framing := f.framing()
	switch {
	case protocol.IsHiddenMessage(message):
		f.encodeControl(framing, message)
		return f.drainReadBuf(p)
	case h.serverless || isEmptyServerMessage(message):
		return 0
	case framing.MarksControl() && protocol.IsControlMessage(message):
		f.encodeControl(framing, message)
		return f.drainReadBuf(p)
	}
	kind := protocol.MessageServer
	// The command input goroutine may set the plain mode meanwhile
	// (commandDispatcher.handleOptions), hence the atomic read.
	if h.plainMode() {
		kind = protocol.MessagePlain
	}
	framing.EncodeMessage(&f.readBuf, protocol.Message{
		Kind:     kind,
		Hostname: h.hostname,
		Content:  message,
	})
	return f.drainReadBuf(p)
}

// encodeControl appends the control frame of message to readBuf and, for
// the capabilities frame, lets enableEscapedFraming go on.
func (f *sessionFramer) encodeControl(framing protocol.Framing, message string) {
	framing.EncodeControl(&f.readBuf, message)
	if strings.HasPrefix(message, protocol.HiddenCapabilitiesPrefix) {
		f.capabilitiesFramedOnce.Do(func() { close(f.capabilitiesFramed) })
	}
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
	framing := f.framing()
	f.encodeMaprMessage(framing, message)
	f.coalesceQueuedMapr(framing, len(p))
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
	f.coalesceQueuedMapr(f.framing(), len(p))
	return f.drainMaprBatch(p)
}

// coalesceQueuedMapr frames queued mapr messages behind readBuf until it
// holds at least limit bytes or no message can be taken.
func (f *sessionFramer) coalesceQueuedMapr(framing protocol.Framing, limit int) {
	c := f.handler.outputCoordinator
	for f.readBuf.Len() < limit {
		queued, ok := c.tryReadQueuedMapr()
		if !ok {
			return
		}
		f.encodeMaprMessage(framing, queued)
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
func (f *sessionFramer) encodeMaprMessage(framing protocol.Framing, message string) {
	h := f.handler
	generation, decodedMessage := decodeGeneratedMessage(message)
	if h.shouldDropGeneration(generation) {
		return
	}
	framing.EncodeMessage(&f.readBuf, protocol.Message{
		Kind:     protocol.MessageAggregate,
		Hostname: h.hostname,
		Content:  decodedMessage,
	})
}

// framing returns the framing of the frames the session encodes from now on.
func (f *sessionFramer) framing() protocol.Framing {
	return protocol.Framing(f.framingState.Load())
}

// handlingCommandFrame reports whether the command being handled is the
// session's n-th command frame (counting from 1). It is only meaningful
// while a command is handled on the command input goroutine (Write).
func (f *sessionFramer) handlingCommandFrame(n int) bool {
	return f.commandFrames == n
}

// enableEscapedFraming switches the session to EscapedFraming once the
// capabilities frame has been encoded. It runs on the command input
// goroutine, so no later command of the session is handled, and none of
// its frames encoded, before the switch. It gives up without switching when
// ctx or the session ends first. In practice it does not wait at all: the
// capabilities frame is queued when the session handler is built, before the
// client can send anything.
func (f *sessionFramer) enableEscapedFraming(ctx context.Context) bool {
	select {
	case <-f.capabilitiesFramed:
	case <-ctx.Done():
		return false
	case <-f.handler.done.Done():
		return false
	}
	f.framingState.Store(uint32(protocol.EscapedFraming))
	return true
}

// enableControlFrames switches an escaped session to EscapedControlFraming
// and reports whether it did; a session in any other framing keeps it. It
// runs on the command input goroutine like enableEscapedFraming, and needs
// not wait: the capabilities frame was encoded before the session became
// escaped.
func (f *sessionFramer) enableControlFrames() bool {
	return f.framingState.CompareAndSwap(uint32(protocol.EscapedFraming),
		uint32(protocol.EscapedControlFraming))
}

// settleFraming marks the session framing as final, so that Read goes on
// encoding the frames after the capabilities frame. The command input
// goroutine calls it as soon as no command can switch the framing any more:
// before it handles a command that can not switch it while the framing
// could still change (settleFramingBeforeCommand), when an escaped framing
// request leaves the session without a possible control frames switch
// (ServerHandler.handleEscapedFramingRequest), when the control frames
// request was handled, and after any command that leaves no switch possible
// (settleFramingAfterCommand). It is idempotent.
func (f *sessionFramer) settleFraming() {
	f.framingSettledOnce.Do(func() { close(f.framingSettled) })
}

// settleFramingBeforeCommand settles the framing before the command being
// handled (the commandFrames-th) when it is no command that could still
// switch the framing: a first command that is not the escaped framing
// request leaves the session raw for good, and a second one that is not the
// control frames request leaves it without control frames. decoded is false
// when the command frame could not be decoded at all, which never switches
// anything either. Later commands find the framing settled already.
//
// Settling before such a command, not only after it, matters because the
// command input goroutine may wait for output while it handles the command:
// a SESSION START queues its ack after it started its reads, whose messages
// (e.g. one "Unable to read file(s)" per missing file) can fill the server
// message channel first, so the ack waits for room, and Read must be able
// to drain meanwhile. That is the one case left that blocks: an unknown
// command or failed map query of an idle session starts the session's
// shutdown, whose flush waits for Read, on a goroutine of its own
// (ServerHandler.finishUserCommand), so the command input goroutine returns
// and stays free to handle the client's close acknowledgement.
// The cost is that the replies such a command queues may follow a frame
// queued before it on its own, e.g. the close handshake of a dserver
// shutting down in the session's first moment; that frame is still encoded
// in the settled framing, and clients ack it either way.
func (f *sessionFramer) settleFramingBeforeCommand(decoded bool, argc int, args []string) {
	switch {
	case !decoded:
		f.settleFraming()
	case f.commandFrames == 1 && !isEscapedFramingRequest(argc, args):
		f.settleFraming()
	case f.commandFrames == 2 && !isControlFramesRequest(argc, args):
		f.settleFraming()
	}
}

// settleFramingAfterCommand settles the framing after the command just
// handled, unless that was the first one and switched the session to
// EscapedFraming: then the second command may still be the control frames
// request. Any other framing after the first command is final (a raw session
// never switches later, as only the first command can make it escaped), as
// is every framing after the second one. It covers the framing requests
// that switched nothing, e.g. an escaped framing request a session did not
// advertise; settleFramingBeforeCommand covered every other command.
func (f *sessionFramer) settleFramingAfterCommand() {
	if f.commandFrames >= 2 || f.framing() != protocol.EscapedFraming {
		f.settleFraming()
	}
}

// awaitFramingSettled blocks Read after the capabilities frame was encoded
// until the session framing settled or the session ended, so every later
// frame is encoded in the framing the client decodes it with. Before the
// capabilities frame it does not wait: that frame is always raw, and
// enableEscapedFraming waits for it. Sessions without a capabilities frame
// (the health handler) never wait. It never waits for long: clients send
// the framing requests together with, or right before, their first commands,
// older clients send no requests, so the framing settles before their first
// command is handled, and a session whose input ends is shut down.
func (f *sessionFramer) awaitFramingSettled() {
	if f.settledObserved {
		return
	}
	select {
	case <-f.capabilitiesFramed:
	default:
		return
	}
	select {
	case <-f.framingSettled:
		f.settledObserved = true
	case <-f.handler.done.Done():
	case <-f.handler.connCtx.Done():
	}
}

func (f *sessionFramer) drainReadBuf(p []byte) int {
	n, _ := f.readBuf.Read(p)
	f.handler.readBufferedBytes.Store(int64(f.readBuf.Len()))
	return n
}

var _ io.ReadWriter = (*sessionFramer)(nil)
