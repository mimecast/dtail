package handlers

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

func newMaprCoalesceTestHandler(activeGeneration uint64) *baseHandler {
	return newBaseHandler(context.Background(), baseHandlerConfig{
		serverMessages:   make(chan string, 4),
		maprMessages:     make(chan string, 64),
		hostname:         "testhost",
		activeGeneration: func() uint64 { return activeGeneration },
	})
}

// encodeAggregateFrames returns the raw aggregate frames of contents in order.
func encodeAggregateFrames(contents ...string) []byte {
	var want bytes.Buffer
	for _, content := range contents {
		protocol.RawFraming.EncodeMessage(&want, protocol.Message{
			Kind: protocol.MessageAggregate, Hostname: "testhost", Content: content})
	}
	return want.Bytes()
}

func TestReadCoalescesQueuedMaprMessages(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	var contents []string
	for i := 0; i < 8; i++ {
		content := fmt.Sprintf("group%d≔%d", i, i)
		contents = append(contents, content)
		handler.maprMessages <- content
	}

	p := make([]byte, 4096)
	n, err := handler.Read(p)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if want := encodeAggregateFrames(contents...); !bytes.Equal(p[:n], want) {
		t.Fatalf("one Read = %q, want all queued frames %q", p[:n], want)
	}
	if len(handler.maprMessages) != 0 {
		t.Fatalf("%d mapr messages left queued, want 0", len(handler.maprMessages))
	}
}

func TestReadCoalescingFiltersEachGeneration(t *testing.T) {
	handler := newMaprCoalesceTestHandler(2)
	handler.maprMessages <- encodeGeneratedMessage(2, "fresh1")
	handler.maprMessages <- encodeGeneratedMessage(1, "stale")
	handler.maprMessages <- encodeGeneratedMessage(2, "fresh2")
	handler.maprMessages <- "ungenerated"

	p := make([]byte, 4096)
	n, err := handler.Read(p)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if want := encodeAggregateFrames("fresh1", "fresh2", "ungenerated"); !bytes.Equal(p[:n], want) {
		t.Fatalf("Read = %q, want %q", p[:n], want)
	}
}

// TestReadCoalescingStopsAtReadSize checks that a Read takes no more queued
// mapr messages than fit into its buffer, keeps the rest of the last frame
// for the next Read, and that the stream equals the uncoalesced one.
func TestReadCoalescingStopsAtReadSize(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	contents := []string{"aaaaaaaaaa", "bbbbbbbbbb", "cccccccccc", "dddddddddd", "eeeeeeeeee"}
	for _, content := range contents {
		handler.maprMessages <- content
	}
	want := encodeAggregateFrames(contents...)
	frameLen := len(want) / len(contents)
	bufSize := frameLen*2 + frameLen/2

	p := make([]byte, bufSize)
	n, err := handler.Read(p)
	if err != nil || n != bufSize {
		t.Fatalf("first Read() = %d, %v, want %d, nil", n, err, bufSize)
	}
	if left := len(handler.maprMessages); left != 2 {
		t.Fatalf("%d mapr messages left queued after the first Read, want 2", left)
	}
	got := append([]byte(nil), p[:n]...)
	got = append(got, readExactly(t, handler, bufSize, len(want)-n)...)
	if !bytes.Equal(got, want) {
		t.Fatalf("coalesced stream = %q, want %q", got, want)
	}
}

func TestTryReadQueuedMaprDefersToServerMessage(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	handler.ensureFlushChannels()
	handler.maprMessages <- "group"
	handler.serverMessages <- "server\n"
	if message, ok := handler.tryReadQueuedMapr(); ok {
		t.Fatalf("tryReadQueuedMapr() = %q, true with a server message queued", message)
	}
	<-handler.serverMessages
	handler.flushErrors <- "flush error\n"
	if message, ok := handler.tryReadQueuedMapr(); ok {
		t.Fatalf("tryReadQueuedMapr() = %q, true with a flush error queued", message)
	}
	<-handler.flushErrors
	if message, ok := handler.tryReadQueuedMapr(); !ok || message != "group" {
		t.Fatalf("tryReadQueuedMapr() = %q, %v, want group, true", message, ok)
	}
}

func TestTryReadQueuedMaprDefersToPayload(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	handler.EnableDirectOutput()
	mustEnqueueOutput(t, &handler.output, 0, []byte("payload\n"))
	handler.maprMessages <- "group"
	if message, ok := handler.tryReadQueuedMapr(); ok {
		t.Fatalf("tryReadQueuedMapr() = %q, true with payload buffered", message)
	}
	if len(handler.maprMessages) != 1 {
		t.Fatal("tryReadQueuedMapr() took the mapr message although payload was buffered")
	}
}

// queueMaprContents queues count mapr messages of varying length and
// returns their contents in queue order.
func queueMaprContents(handler *baseHandler, count int) []string {
	contents := make([]string, 0, count)
	for i := 0; i < count; i++ {
		content := fmt.Sprintf("group%d≔%s", i, strings.Repeat("v", i%17))
		contents = append(contents, content)
		handler.maprMessages <- content
	}
	return contents
}

// TestReadCoalescingFillsEveryWriteAfterSplitFrame checks that the rest of
// a batch whose last frame did not fit into the Read buffer goes out
// together with the groups queued behind it: every Read but the last
// returns a full buffer, so no tiny trailing write per batch remains, and
// the stream equals the uncoalesced one.
func TestReadCoalescingFillsEveryWriteAfterSplitFrame(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	want := encodeAggregateFrames(queueMaprContents(handler, 40)...)
	const bufSize = 100

	var got []byte
	p := make([]byte, bufSize)
	for len(got) < len(want) {
		n, err := handler.Read(p)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		if n != bufSize && len(got)+n != len(want) {
			t.Fatalf("Read() = %d bytes at offset %d of %d, want a full %d byte write",
				n, len(got), len(want), bufSize)
		}
		got = append(got, p[:n]...)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("coalesced stream = %q, want %q", got, want)
	}
	if left := len(handler.maprMessages); left != 0 {
		t.Fatalf("%d mapr messages left queued, want 0", left)
	}
}

// TestReadCoalescingEscapesEachFrame checks that every coalesced frame is
// escaped on its own under the escaped framings, also when a frame is split
// across Reads, so group content with MessageDelimiter (0xAC, e.g. in '€'
// and '¬') or FrameEscape (0xFF) bytes decodes back unchanged.
func TestReadCoalescingEscapesEachFrame(t *testing.T) {
	contents := []string{"€uro≔1", "not¬≔2", "esc\xff≔3", "both\xac\xff\xac≔4", "plain≔5",
		"\xff\xff\xac\xac≔6"}
	for _, framing := range []protocol.Framing{protocol.EscapedFraming,
		protocol.EscapedControlFraming} {
		handler := newMaprCoalesceTestHandler(0)
		handler.framingState.Store(uint32(framing))
		var want bytes.Buffer
		for _, content := range contents {
			handler.maprMessages <- content
			framing.EncodeMessage(&want, protocol.Message{
				Kind: protocol.MessageAggregate, Hostname: "testhost", Content: content})
		}
		stream := readExactly(t, handler, 13, want.Len())
		if !bytes.Equal(stream, want.Bytes()) {
			t.Fatalf("%v: stream = %q, want %q", framing, stream, want.Bytes())
		}
		assertAggregateFramesDecode(t, framing, stream, contents)
	}
}

// assertAggregateFramesDecode splits stream at MessageDelimiter and checks
// that it holds exactly one unmarked aggregate frame per content, in order.
func assertAggregateFramesDecode(t *testing.T, framing protocol.Framing, stream []byte,
	contents []string) {
	t.Helper()
	frames := bytes.Split(bytes.TrimSuffix(stream, []byte{protocol.MessageDelimiter}),
		[]byte{protocol.MessageDelimiter})
	if len(frames) != len(contents) {
		t.Fatalf("%v: %d frames, want %d: %q", framing, len(frames), len(contents), stream)
	}
	var scratch []byte
	for i, frame := range frames {
		body, control := framing.CutControl(frame)
		if control {
			t.Fatalf("%v: frame %d %q is marked as a control frame", framing, i, frame)
		}
		message, err := protocol.DecodeMessage(string(framing.DecodeFrame(body, &scratch)))
		if err != nil || message.Kind != protocol.MessageAggregate ||
			message.Content != contents[i] {
			t.Fatalf("%v: frame %d decodes to %+v, %v, want aggregate %q",
				framing, i, message, err, contents[i])
		}
	}
}

// TestReadCoalescedMaprPrecedesCloseSync checks that the close handshake
// requested while groups are queued follows all of their frames, also when
// the batches are split across Reads and their rests coalesced.
func TestReadCoalescedMaprPrecedesCloseSync(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	handler.AttachOutputReader()
	want := encodeAggregateFrames(queueMaprContents(handler, 20)...)
	handler.requestCloseSync(context.Background())
	var closeFrame bytes.Buffer
	protocol.RawFraming.EncodeControl(&closeFrame, ".syn close connection")
	want = append(want, closeFrame.Bytes()...)

	if got := readExactly(t, handler, 70, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("stream = %q, want all group frames, then the close handshake %q", got, want)
	}
}

// TestReadCoalescedMaprFlushAckAfterDelivery checks that a flush requested
// while groups are queued is acknowledged only by the Read after the one
// that returned the last coalesced bytes, i.e. once they were delivered.
func TestReadCoalescedMaprFlushAckAfterDelivery(t *testing.T) {
	handler := newMaprCoalesceTestHandler(0)
	handler.AttachOutputReader()
	want := encodeAggregateFrames(queueMaprContents(handler, 20)...)
	flushDone := make(chan error, 1)
	go func() { flushDone <- handler.flushContext(context.Background()) }()

	var got []byte
	p := make([]byte, 70)
	for len(got) < len(want) {
		n, err := handler.Read(p)
		if err != nil {
			t.Fatalf("Read() error = %v", err)
		}
		got = append(got, p[:n]...)
		select {
		case err := <-flushDone:
			t.Fatalf("flush returned %v after %d of %d bytes", err, len(got), len(want))
		default:
		}
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream = %q, want %q", got, want)
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = handler.Read(p)
	}()
	select {
	case err := <-flushDone:
		if err != nil {
			t.Fatalf("flush returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flush was not acknowledged after the coalesced bytes were delivered")
	}
	handler.done.Shutdown()
	<-readerDone
}
