//go:build linux

package clients

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

// listingPTYHandler is the connection handler of the path completion PTY
// test: a session that never ends and answers a path listing only once a
// payload line was written after the request, as a remote server's answer
// frame queued behind payload is read only once the output takes that
// payload (its field probes get an empty answer).
type listingPTYHandler struct {
	*statusTestHandler
	open     chan struct{}
	logger   *pausingTestLogger
	listings map[string][]protocol.ProbeEntry

	mu     sync.Mutex
	listed []string
}

func (h *listingPTYHandler) Done() <-chan struct{} { return h.open }

func (h *listingPTYHandler) Probe(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	if request.List == "" {
		return protocol.ProbeAnswer{ID: request.ID}, nil
	}
	written := len(h.logger.writtenLines())
	for len(h.logger.writtenLines()) == written {
		select {
		case <-ctx.Done():
			return protocol.ProbeAnswer{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	h.mu.Lock()
	h.listed = append(h.listed, request.List)
	h.mu.Unlock()
	return protocol.ProbeAnswer{ID: request.ID, Entries: h.listings[request.List]}, nil
}

func (h *listingPTYHandler) listedPrefixes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.listed)
}

// TestInteractiveEditorCompletesPathsOnPTY runs the line editor on a real
// pseudo terminal while payload streams: Tab in a --files value completes a
// file of the prefetched directory at once, with the output paused (no
// payload line written meanwhile), and a file of a directory not listed yet
// after letting the output run until the listing arrived (payload lines
// written meanwhile), with the output paused again afterwards, also in a
// line with the prompt.
func TestInteractiveEditorCompletesPathsOnPTY(t *testing.T) {
	t.Parallel()
	master, ttyPath := openTestPTY(t)
	logger := newPausingTestLogger()
	stream := func(ctx context.Context) {
		for ctx.Err() == nil {
			logger.Raw("payload line\n")
			time.Sleep(2 * time.Millisecond)
		}
	}
	c, _ := newSessionEndTestClient(t, stream)
	handler := &listingPTYHandler{statusTestHandler: &statusTestHandler{}, open: make(chan struct{}),
		logger: logger, listings: map[string][]protocol.ProbeEntry{
			"/var/log/": {{Name: "app.log"}}, "/srv/": {{Name: "data.log"}}, "/opt/": {{Name: "other.log"}}}}
	c.connections[0].(*finalTestConnector).handler = handler
	c.loggers.Client = logger
	c.InteractiveQuery = true
	c.ControlTTYPath = ttyPath
	c.Mode = omode.GrepClient
	c.What = "/var/log/app.log"
	c.RegexStr = "ERROR"
	c.sessionSpec = NewSessionSpec(c.Args) // the prober reads the files from it

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Tab completes", done)
	waitFor(t, "prefetched listing", func() bool { return slices.Contains(handler.listedPrefixes(), "/var/log/") })

	writePTY(t, master, ":")
	waitForPTYOutput(t, master, interactiveTypingHint, done)
	waitForPauseDepth(t, logger, 1)
	paused := len(logger.writtenLines())
	writePTY(t, master, "reload --files /var/log/a\t")
	waitForPTYOutput(t, master, "/var/log/app.log", done)
	if written := len(logger.writtenLines()); written != paused {
		t.Fatalf("cached completion let %d payload lines through", written-paused)
	}

	writePTY(t, master, "\x15:reload --files /srv/d\t")
	waitForPTYOutput(t, master, "/srv/data.log", done)
	if written := len(logger.writtenLines()); written == paused {
		t.Fatal("no payload line written while waiting for the listing")
	}
	if depth := logger.pauseDepth(); depth != 1 {
		t.Fatalf("pause depth after the listing = %d, want 1", depth)
	}

	// The same in a line with the prompt (started in an explicit pause):
	// the line is taken off the screen, prompt included, for the wait, and
	// the Terminal draws prompt and line again below it before the
	// completed line, so its cursor bookkeeping stays right.
	writePTY(t, master, "\x15\n") // An empty line ends the typing pause.
	waitForPauseDepth(t, logger, 0)
	writePTY(t, master, "\n") // Explicit pause: the next line has the prompt.
	waitForPTYOutput(t, master, "interactive-query.md).\r\r\n"+interactivePrompt, done)
	// The wait helper drops what it read past its match, and the redraw and
	// the completed line arrive together, so one wait reads all of it and
	// the order is checked in what it returns.
	writePTY(t, master, ":reload --files /opt/o\t")
	out := waitForPTYOutput(t, master, "/opt/other.log", done)
	hide := strings.Index(out, "\r\x1b[J")
	redraw := strings.LastIndex(out, interactivePrompt+":reload --files /opt/o")
	if hide < 0 || redraw < hide || redraw > strings.Index(out, "/opt/other.log") {
		t.Fatalf("want the line hidden, then prompt and line redrawn, then the completion; got %q", out)
	}
	writePTY(t, master, "\x15:quit\n")
	requireStartStatus(t, done, 0)
}

// requireStartStatus waits up to 10 s for the status of the client's Start.
func requireStartStatus(t *testing.T, done <-chan int, want int) {
	t.Helper()
	select {
	case status := <-done:
		if status != want {
			t.Fatalf("Start status = %d, want %d", status, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive client did not end")
	}
}

func writePTY(t *testing.T, master *os.File, s string) {
	t.Helper()
	if _, err := master.WriteString(s); err != nil {
		t.Fatalf("write %q: %v", s, err)
	}
}

// waitFor polls cond for up to 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
