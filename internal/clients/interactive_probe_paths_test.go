package clients

import (
	"bytes"
	"context"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestProbeNowDoesNotWaitForPrefetch: the path listings prefetched after a
// round run in the background, outside the round :probe waits for, so a
// listing that does not answer (here: never) neither delays the next round
// nor makes :probe time out although the field probes answered.
func TestProbeNowDoesNotWaitForPrefetch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		handler.listGate = func(string) bool { return false }
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait() // the first round ended; its prefetch hangs

		start := time.Now()
		result, status := c.prober.probeNow()
		if status != probeWaitDone || !reflect.DeepEqual(result.answered, []string{"srv1"}) ||
			time.Since(start) != 0 {
			t.Fatalf("probeNow = %+v %v after %v, want the answered round at once", result, status,
				time.Since(start))
		}
		if got := handler.listed(); len(got) == 0 || got[0] != "/var/log/" {
			t.Fatalf("listed %q, want the prefetch of /var/log/ on its way", got)
		}
	})
}

// TestInteractiveProberPrefetchesListings: a round lists the directories
// of the session's files on the first connection only, each once, the
// working directory for a file without one, and no directory with glob
// characters, no stdin and no journal units for a journal target (a
// journalctl run on the server, listed on Tab only); the listings land in
// the catalog and nothing is printed.
func TestInteractiveProberPrefetchesListings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, firstHandler := probeTestConnection("srv1")
		firstHandler.listings = map[string][]protocol.ProbeEntry{"/var/log/": {{Name: "a.log"}, {Name: "b.log"}}}
		second, secondHandler := probeTestConnection("srv2")
		files := []string{"/var/log/a.log", "-", "/var/log/b.log", "journal:x.service", "/x/*/y.log", "rel.log"}
		c, logger := newProbeTestClient(files, first, second)
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)
		synctest.Wait()
		cancel()
		c.prober.wait()

		got := firstHandler.listed()
		slices.Sort(got)
		if want := []string{"./", "/var/log/"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("first connection listed %q, want %q", got, want)
		}
		if got := secondHandler.listed(); len(got) != 0 {
			t.Fatalf("second connection listed %q", got)
		}
		match := c.prober.catalog.findListing("/var/log/", "", time.Now())
		if !match.complete || len(match.listing.entries) != 2 || match.listing.server != "srv1" {
			t.Fatalf("cached listing = %+v", match)
		}
		logger.requireSilent(t)
	})
}

// TestJournalUnitsListedOnTabOnly: a session reading a journal target
// prefetches no unit listing, not at the start, after a request or on
// :probe, and Tab after "journal:" then asks for the uncached listing of
// "journal:" (completeFilesValue's fetch, sent by completeEditorLine).
func TestJournalUnitsListedOnTabOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"journal:sshd.service"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		c.prober.request()
		synctest.Wait()
		if _, status := c.prober.probeNow(); status != probeWaitDone {
			t.Fatalf("probeNow status = %v", status)
		}
		synctest.Wait()
		if got := handler.listed(); len(got) != 0 {
			t.Fatalf("listed %q for a journal session, want no prefetch", got)
		}

		line := ":reload --files journal:ss"
		got, ok := completeFilesValue(line, len(line), completeGrepArgs, c.prober.pathListingSource())
		if !ok || got.fetch != fs.JournalSpecPrefix || got.line != line {
			t.Fatalf("Tab on %q = %+v (ok %v), want a fetch of %q", line, got, ok, fs.JournalSpecPrefix)
		}
	})
}

// TestPrefetchOnlyOnRequestedRounds: the periodic rounds prefetch no path
// listing (each would make the server check every file of the directory
// every minute); a requested round (session start, :reload, :probe) does,
// but not for a directory whose listing in the cache is still fresh, except
// after :probe, which refreshes it once.
func TestPrefetchOnlyOnRequestedRounds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		handler.listings = map[string][]protocol.ProbeEntry{"/var/log/": {{Name: "a.log"}}}
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		if got := handler.listed(); len(got) != 1 {
			t.Fatalf("listed %q at the start, want /var/log/ once", got)
		}

		c.prober.request() // e.g. :reload of the same files
		synctest.Wait()
		if got := handler.listed(); len(got) != 1 {
			t.Fatalf("listed %q after a request with a fresh listing cached, want no new listing", got)
		}

		time.Sleep(3*probeInterval + time.Second) // three periodic rounds
		synctest.Wait()
		if rounds := len(handler.requestsWhere(func(r protocol.ProbeRequest) bool {
			return len(r.Files) > 0
		})); rounds != 5 {
			t.Fatalf("%d field probes, want 5 (start, request, 3 periodic)", rounds)
		}
		if got := handler.listed(); len(got) != 1 {
			t.Fatalf("listed %q after periodic rounds, want no new listing", got)
		}

		c.prober.request() // the cached listing expired meanwhile
		synctest.Wait()
		if got := handler.listed(); len(got) != 2 {
			t.Fatalf("listed %q after a request with the listing expired, want a second listing", got)
		}

		// :probe refreshes the fresh listing, so a file created since shows up.
		if _, status := c.prober.probeNow(); status != probeWaitDone {
			t.Fatalf("probeNow status = %v", status)
		}
		synctest.Wait()
		if got := handler.listed(); len(got) != 3 {
			t.Fatalf("listed %q after :probe with a fresh listing cached, want a third listing", got)
		}
		c.prober.request() // the refresh was for that :probe only
		synctest.Wait()
		if got := handler.listed(); len(got) != 3 {
			t.Fatalf("listed %q after a request following :probe, want no new listing", got)
		}
	})
}

// TestListPaths: a listing asked for again while on its way is sent once;
// it goes on after the caller stopped waiting, so a late answer is cached;
// a nil, not started or stopped prober sends none.
func TestListPaths(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		var open atomic.Bool
		handler.listings = map[string][]protocol.ProbeEntry{"/srv/": {{Name: "data.log"}}}
		handler.listGate = func(string) bool { return open.Load() }
		c, _ := newProbeTestClient([]string{"-"}, conn)
		if c.prober.listPaths("/srv/") != nil {
			t.Fatal("a prober not started listed")
		}
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)

		first := c.prober.listPaths("/srv/")
		if second := c.prober.listPaths("/srv/"); second != first {
			t.Fatal("a listing on its way was asked for again")
		}
		time.Sleep(pathListingWait) // the caller gives up; the request goes on
		synctest.Wait()
		open.Store(true)
		<-first
		if got := handler.listed(); !reflect.DeepEqual(got, []string{"/srv/"}) {
			t.Fatalf("listed %q, want one request", got)
		}
		if match := c.prober.catalog.findListing("/srv/", "d", time.Now()); !match.complete {
			t.Fatalf("late answer not cached: %+v", match)
		}
		cancel()
		c.prober.wait()
		if c.prober.listPaths("/other/") != nil {
			t.Fatal("a stopped prober listed")
		}
	})
	var nilProber *interactiveProber
	if nilProber.listPaths("/srv/") != nil {
		t.Fatal("a nil prober listed")
	}
}

// pathWaitTest is a client with a started prober over one connection whose
// listings are held back by the test, and the pause of its control loop
// paused for typing.
type pathWaitTest struct {
	c       *baseClient
	logger  *pausingTestLogger
	handler *probeTestHandler
	pause   *interactivePause
	screen  *bytes.Buffer
	editor  *interactiveEditor
	// depths records the logger's pause depth whenever a listing went
	// through the gate.
	depths []int
}

// newPathWaitTest returns the test client; gate decides, with the
// client's logger, whether a listing of prefix is answered now.
func newPathWaitTest(t *testing.T, serverless bool, gate func(logger *pausingTestLogger, prefix string) bool) *pathWaitTest {
	w := &pathWaitTest{logger: newPausingTestLogger(), screen: &bytes.Buffer{}}
	w.c = newPauseTestClient(w.logger)
	w.c.Serverless = serverless
	conn, handler := probeTestConnection("srv1")
	w.handler = handler
	handler.listings = map[string][]protocol.ProbeEntry{
		"/var/log/": {{Name: "app.log"}}, "/srv/": {{Name: "data.log"}}, "/late/": {{Name: "late.log"}}}
	handler.listGate = func(prefix string) bool {
		if !gate(w.logger, prefix) {
			return false
		}
		w.depths = append(w.depths, w.logger.pauseDepth())
		return true
	}
	w.c.connections = []connectors.Connector{conn}
	w.c.prober = w.c.newInteractiveProber()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() { cancel(); w.c.prober.wait() })
	w.c.prober.start(ctx)
	synctest.Wait() // the first round prefetches /var/log/
	w.pause = w.c.newInteractivePause()
	w.pause.pauseTyping()
	w.editor = &interactiveEditor{screen: newEditorScreen(w.screen)}
	return w
}

func (w *pathWaitTest) complete(line string) completion {
	return w.c.completeEditorLine(w.editor, w.pause, line, len(line))
}

// requireTypingPause requires the output paused for typing, as before the
// Tab.
func (w *pathWaitTest) requireTypingPause(t *testing.T) {
	t.Helper()
	if !w.pause.paused || !w.pause.typing || w.logger.pauseDepth() != 1 {
		t.Fatalf("pause after Tab: paused %v typing %v depth %d", w.pause.paused, w.pause.typing,
			w.logger.pauseDepth())
	}
}

// TestTabWaitsForPathListing: with a remote server (whose answer comes only
// while the output runs) a cached directory completes at once with the
// output paused; an uncached one lets the output run (the line taken off
// the screen and its rows given back) until the answer came and pauses it
// again; a listing that does not come within the wait gives a hint, and
// the next Tab uses its late answer from the cache.
func TestTabWaitsForPathListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var lateOpen atomic.Bool
		w := newPathWaitTest(t, false, func(logger *pausingTestLogger, prefix string) bool {
			running := logger.pauseDepth() == 0
			return running && (prefix != "/late/" || lateOpen.Load())
		})
		requests := len(w.handler.listed())

		if got := w.complete(":reload --files /var/log/a"); got.line != ":reload --files /var/log/app.log" {
			t.Fatalf("cached: %+v", got)
		}
		if len(w.handler.listed()) != requests || w.screen.Len() != 0 {
			t.Fatalf("cached directory listed again (%q) or touched the screen (%q)", w.handler.listed(), w.screen)
		}
		w.requireTypingPause(t)

		got := w.complete(":reload --files /srv/d")
		if got.line != ":reload --files /srv/data.log" || got.hint != "" {
			t.Fatalf("uncached: %+v", got)
		}
		if w.depths[len(w.depths)-1] != 0 || w.screen.String() != "\r\x1b[J" {
			t.Fatalf("output not running during the wait (depth %v) or screen %q", w.depths, w.screen)
		}
		w.requireTypingPause(t)

		start := time.Now()
		got = w.complete(":reload --files /late/l")
		if waited := time.Since(start); waited != pathListingWait || got.line != ":reload --files /late/l" ||
			!strings.Contains(got.hint, "no listing of /late/") {
			t.Fatalf("timeout after %v: %+v", waited, got)
		}
		w.requireTypingPause(t)
		lateOpen.Store(true)
		// The output runs again (the user ran the line), and the answer
		// arrives.
		w.pause.runOutput(func() {
			time.Sleep(100 * time.Millisecond)
			synctest.Wait()
		})
		w.screen.Reset()
		if got := w.complete(":reload --files /late/l"); got.line != ":reload --files /late/late.log" ||
			w.screen.Len() != 0 {
			t.Fatalf("late answer not used: %+v, screen %q", got, w.screen)
		}
	})
}

// TestTabWaitsForServerlessListing: in serverless mode Tab lets the output
// run while it waits too, as the in-process server logs to the paused
// client output while it handles the request.
func TestTabWaitsForServerlessListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newPathWaitTest(t, true, func(logger *pausingTestLogger, _ string) bool {
			return logger.pauseDepth() == 0
		})
		if got := w.complete(":reload --files /srv/d"); got.line != ":reload --files /srv/data.log" {
			t.Fatalf("serverless: %+v", got)
		}
		if w.screen.String() != "\r\x1b[J" {
			t.Fatalf("screen %q, want the line taken off it", w.screen)
		}
		w.requireTypingPause(t)
	})
}

// TestTabStopsWaitingForFailedListing: a listing request that ends without
// a listing (here the server is busy with another probe) ends the wait at
// once with the hint instead of waiting the whole time.
func TestTabStopsWaitingForFailedListing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := newPathWaitTest(t, false, func(*pausingTestLogger, string) bool { return true })
		w.handler.err = context.Canceled
		start := time.Now()
		got := w.complete(":reload --files /srv/d")
		if time.Since(start) != 0 || !strings.Contains(got.hint, "no listing of /srv/") {
			t.Fatalf("failed listing after %v: %+v", time.Since(start), got)
		}
		w.requireTypingPause(t)
	})
}

// TestEditorScreen: hiding the edited line moves up to its first row and
// clears to the end of the screen; restoring gives back as many rows as the
// Terminal moves up for the cursor's position.
func TestEditorScreen(t *testing.T) {
	var out bytes.Buffer
	s := newEditorScreen(&out)
	s.setWidth(10)
	line := strings.Repeat("x", 25)
	s.hideLine(line, 25)
	if out.String() != "\x1b[2A\r\x1b[J" {
		t.Fatalf("hide = %q", out.String())
	}
	out.Reset()
	s.restoreLine(line, 25)
	if out.String() != "\r\n\r\n" {
		t.Fatalf("restore = %q", out.String())
	}
	out.Reset()
	s.setWidth(0) // ignored
	s.hideLine("ab", 1)
	s.restoreLine("ab", 1)
	if out.String() != "\r\x1b[J" {
		t.Fatalf("one row: %q", out.String())
	}
	var nilScreen *editorScreen
	nilScreen.hideLine(line, 25)
	nilScreen.restoreLine(line, 25)
	nilScreen.setWidth(5)
}
