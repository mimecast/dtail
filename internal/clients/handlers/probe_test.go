package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// probeAnswerFrame encodes answer as the marked control frame a server
// answers a probe with.
func probeAnswerFrame(t *testing.T, answer protocol.ProbeAnswer) []byte {
	t.Helper()
	data, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return controlFrames(protocol.EncodeProbeAnswer(answer.ID, data))
}

type probeResult struct {
	answer protocol.ProbeAnswer
	err    error
}

// startProbe runs handler.Probe in a goroutine and returns its result
// channel.
func startProbe(ctx context.Context, handler Prober, request protocol.ProbeRequest) <-chan probeResult {
	results := make(chan probeResult, 1)
	go func() {
		answer, err := handler.Probe(ctx, request)
		results <- probeResult{answer, err}
	}()
	return results
}

// readCommands passes every command the handler sends, decoded, to the
// returned channel until the handler is done.
func readCommands(handler io.Reader) <-chan string {
	commands := make(chan string, 8)
	go func() {
		defer close(commands)
		p := make([]byte, 64*1024)
		for {
			n, err := handler.Read(p)
			if err != nil {
				return
			}
			frame := strings.TrimSuffix(string(p[:n]), ";")
			_, encoded, _ := strings.Cut(frame, " base64 ")
			decoded, _ := base64.StdEncoding.DecodeString(encoded)
			commands <- string(decoded)
		}
	}()
	return commands
}

// receiveCommand returns the next command the handler sent and fails the
// test when none arrives within 5 s or the handler is done, instead of
// hanging until the test binary times out.
func receiveCommand(t *testing.T, commands <-chan string) string {
	t.Helper()
	select {
	case command, ok := <-commands:
		if !ok {
			t.Fatal("handler done before it sent a command")
		}
		return command
	case <-time.After(5 * time.Second):
		t.Fatal("handler sent no command")
		return ""
	}
}

func receiveResult(t *testing.T, results <-chan probeResult) probeResult {
	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("Probe did not return")
		return probeResult{}
	}
}

// TestProbeSendsRequestOnceSessionIsUp: Probe sends nothing before the
// server's capabilities frame, then the probe command with the request, and
// returns the answer with the request's id, skipping a stale one.
func TestProbeSendsRequestOnceSessionIsUp(t *testing.T) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	commands := readCommands(handler)
	request := protocol.ProbeRequest{ID: "p2", Files: []string{"/var/log/app.log"}}
	results := startProbe(t.Context(), handler, request)

	select {
	case command := <-commands:
		t.Fatalf("command %q sent before the session was up", command)
	case <-time.After(50 * time.Millisecond):
	}
	writeInChunks(t, handler, capabilitiesFrame(), 1024)
	command := receiveCommand(t, commands)
	want, err := protocol.EncodeProbeCommand(request)
	if err != nil {
		t.Fatal(err)
	}
	if command != want {
		t.Fatalf("command = %q, want %q", command, want)
	}

	stale := protocol.ProbeAnswer{ID: "p1", FilesSampled: 9}
	answer := protocol.ProbeAnswer{ID: "p2", DefaultLogFormat: "default", FilesSampled: 1,
		Formats: map[string]protocol.ProbeFormat{"default": {Tables: map[string]map[string][]string{
			"STATS": {"currentConnections": {"0"}}}}}}
	writeInChunks(t, handler, append(probeAnswerFrame(t, stale), probeAnswerFrame(t, answer)...), 7)
	result := receiveResult(t, results)
	if result.err != nil || !reflect.DeepEqual(result.answer, answer) {
		t.Fatalf("Probe() = %+v, %v; want %+v", result.answer, result.err, answer)
	}
}

// TestProbeFailures covers the ways Probe gives up: the context ending
// before the session is up or before the answer, the session ending, and a
// handler that does not probe.
func TestProbeFailures(t *testing.T) {
	request := protocol.ProbeRequest{ID: "p1", Files: []string{"/a"}}

	t.Run("context before session", func(t *testing.T) {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		defer handler.Shutdown()
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if _, err := handler.Probe(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Probe() error = %v, want %v", err, context.DeadlineExceeded)
		}
	})
	t.Run("context before answer", func(t *testing.T) {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		defer handler.Shutdown()
		commands := readCommands(handler)
		writeInChunks(t, handler, capabilitiesFrame(), 1024)
		ctx, cancel := context.WithCancel(t.Context())
		results := startProbe(ctx, handler, request)
		receiveCommand(t, commands)
		cancel()
		if result := receiveResult(t, results); !errors.Is(result.err, context.Canceled) {
			t.Fatalf("Probe() error = %v, want %v", result.err, context.Canceled)
		}
	})
	t.Run("session ended", func(t *testing.T) {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		results := startProbe(t.Context(), handler, request)
		handler.Shutdown()
		if result := receiveResult(t, results); !errors.Is(result.err, ErrProbeSessionEnded) {
			t.Fatalf("Probe() error = %v, want %v", result.err, ErrProbeSessionEnded)
		}
	})
	t.Run("health handler", func(t *testing.T) {
		handler := NewHealthHandler("srv1", clientlog.NopLogger{})
		defer handler.Shutdown()
		if _, err := handler.Probe(t.Context(), request); !errors.Is(err, ErrProbeUnsupported) {
			t.Fatalf("Probe() error = %v, want %v", err, ErrProbeUnsupported)
		}
	})
}

// writeInChunksWithin is writeInChunks failing the test when the writes do
// not return within limit, e.g. as a full probe answer queue blocks them.
func writeInChunksWithin(t *testing.T, handler *ClientHandler, stream []byte, size int,
	limit time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		for start := 0; start < len(stream); start += size {
			end := min(start+size, len(stream))
			n, err := handler.Write(stream[start:end])
			if err != nil {
				done <- fmt.Errorf("Write(): %w", err)
				return
			}
			if n != end-start {
				done <- fmt.Errorf("Write() = %d, want %d", n, end-start)
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(limit):
		t.Fatalf("Write blocked for more than %v", limit)
	}
}

// TestProbeAnswersAreNeverPrinted: probe answer frames, valid or not, are
// consumed silently (debug log only), never printed as payload, never
// warned about and never block Write, also when the queue is full.
func TestProbeAnswersAreNeverPrinted(t *testing.T) {
	recorder := &payloadRecorder{}
	handler := NewClientHandler("srv1", recorder)
	defer handler.Shutdown()

	stream := capabilitiesFrame()
	for i := range probeAnswerQueueSize + 2 {
		stream = append(stream, probeAnswerFrame(t, protocol.ProbeAnswer{ID: "p" + string(rune('a'+i))})...)
	}
	stream = append(stream, controlFrames(protocol.HiddenProbePrefix+"p1 !!!", protocol.HiddenProbePrefix+"p1")...)
	writeInChunksWithin(t, handler, stream, 3, 5*time.Second)

	if recorder.payload.Len() != 0 || len(recorder.warnings) != 0 {
		t.Fatalf("probe frames printed %q, warned %q", recorder.payload.String(), recorder.warnings)
	}
	if got := len(handler.probeAnswers); got != probeAnswerQueueSize {
		t.Fatalf("queued answers = %d, want %d", got, probeAnswerQueueSize)
	}
	if len(recorder.debugs) < 4 {
		t.Fatalf("dropped answers not logged at debug level: %q", recorder.debugs)
	}

	// A payload line that reads like a probe answer is payload.
	line := protocol.HiddenProbePrefix + "pa e30=\n"
	writeInChunks(t, handler, payloadFrames(protocol.EscapedControlFraming, line), 64)
	if recorder.payload.String() != line {
		t.Fatalf("payload = %q, want %q", recorder.payload.String(), line)
	}
}

// TestMaprHandlerProbes: the mapreduce handler (dmap, dtail --query) routes
// probe answers like the client handler.
func TestMaprHandlerProbes(t *testing.T) {
	query, err := mapr.NewQuery("from STATS select count($line) group by $hostname", logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewMaprHandler("srv1", maprclient.NewSessionState(query, logging.NopLogger{}), clientlog.NopLogger{})
	defer handler.Shutdown()
	commands := readCommands(handler)
	results := startProbe(t.Context(), handler, protocol.ProbeRequest{ID: "m1", Files: []string{"/a"}})
	if _, err := handler.Write(capabilitiesFrame()); err != nil {
		t.Fatal(err)
	}
	receiveCommand(t, commands)
	answer := protocol.ProbeAnswer{ID: "m1", FilesSampled: 1}
	if _, err := handler.Write(probeAnswerFrame(t, answer)); err != nil {
		t.Fatal(err)
	}
	if result := receiveResult(t, results); result.err != nil || result.answer.ID != "m1" {
		t.Fatalf("Probe() = %+v, %v", result.answer, result.err)
	}
}

// TestProbeDropsAnswersQueuedBeforeTheRequest: an answer queued before the
// request was sent, even one with the same id, is never taken for the
// request's answer.
func TestProbeDropsAnswersQueuedBeforeTheRequest(t *testing.T) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	commands := readCommands(handler)
	late := protocol.ProbeAnswer{ID: "p1", FilesSampled: 9}
	writeInChunks(t, handler, append(capabilitiesFrame(), probeAnswerFrame(t, late)...), 1024)

	results := startProbe(t.Context(), handler, protocol.ProbeRequest{ID: "p1", Files: []string{"/a"}})
	receiveCommand(t, commands)
	if len(handler.probeAnswers) != 0 {
		t.Fatalf("queued answers after the request = %d, want 0", len(handler.probeAnswers))
	}
	fresh := protocol.ProbeAnswer{ID: "p1", FilesSampled: 1}
	writeInChunks(t, handler, probeAnswerFrame(t, fresh), 1024)
	if result := receiveResult(t, results); result.err != nil || result.answer.FilesSampled != 1 {
		t.Fatalf("Probe() = %+v, %v; want the answer sent after the request", result.answer, result.err)
	}
}

// TestProbeGivesUpWaitingForTheSlot: a Probe call waiting for another one
// to finish gives up when its context or the session ends.
func TestProbeGivesUpWaitingForTheSlot(t *testing.T) {
	request := protocol.ProbeRequest{ID: "p1", Files: []string{"/a"}}
	t.Run("context while another probe runs", func(t *testing.T) {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		defer handler.Shutdown()
		handler.probeSlot <- struct{}{} // a running probe
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		if _, err := handler.Probe(ctx, request); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Probe() error = %v, want %v", err, context.DeadlineExceeded)
		}
	})
	t.Run("session ended while another probe runs", func(t *testing.T) {
		handler := NewClientHandler("srv1", clientlog.NopLogger{})
		handler.probeSlot <- struct{}{} // a running probe
		results := startProbe(t.Context(), handler, request)
		handler.Shutdown()
		if result := receiveResult(t, results); !errors.Is(result.err, ErrProbeSessionEnded) {
			t.Fatalf("Probe() error = %v, want %v", result.err, ErrProbeSessionEnded)
		}
	})
}

// TestProbeSharesTheSlot: a second Probe call while one waits for its
// answer (an on-demand path listing during a background field probe) is
// not refused but waits for the slot, sends its request only after the
// first answer arrived, and then gets its own answer, so the server never
// sees two probes of the session at once.
func TestProbeSharesTheSlot(t *testing.T) {
	handler := NewClientHandler("srv1", clientlog.NopLogger{})
	defer handler.Shutdown()
	commands := readCommands(handler)
	writeInChunks(t, handler, capabilitiesFrame(), 1024)

	fields := protocol.ProbeRequest{ID: "p1", Files: []string{"/a"}}
	first := startProbe(t.Context(), handler, fields)
	receiveCommand(t, commands)
	listing := protocol.ProbeRequest{ID: "l1", List: "/var/log/"}
	second := startProbe(t.Context(), handler, listing)
	select {
	case command := <-commands:
		t.Fatalf("second request %q sent while the first probe ran", command)
	case result := <-second:
		t.Fatalf("second Probe returned while the first ran: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	writeInChunks(t, handler, probeAnswerFrame(t, protocol.ProbeAnswer{ID: "p1", FilesSampled: 1}), 1024)
	if result := receiveResult(t, first); result.err != nil || result.answer.ID != "p1" {
		t.Fatalf("first Probe() = %+v, %v", result.answer, result.err)
	}
	want, err := protocol.EncodeProbeCommand(listing)
	if err != nil {
		t.Fatal(err)
	}
	if command := receiveCommand(t, commands); command != want {
		t.Fatalf("second command = %q, want %q", command, want)
	}
	answer := protocol.ProbeAnswer{ID: "l1", Dir: "/var/log/", Entries: []protocol.ProbeEntry{{Name: "app.log"}}}
	writeInChunks(t, handler, probeAnswerFrame(t, answer), 1024)
	if result := receiveResult(t, second); result.err != nil || !reflect.DeepEqual(result.answer, answer) {
		t.Fatalf("second Probe() = %+v, %v; want %+v", result.answer, result.err, answer)
	}
}
