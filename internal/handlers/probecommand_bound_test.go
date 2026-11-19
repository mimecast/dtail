package handlers

// Tests of the bounds of the probe command (startProbe): at most one probe
// answer per session waits to be queued, and the command input waits for it
// at most probeInFlightWait, so neither a client that stops reading nor a
// payload flood that leaves the answers unread stops the command input.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr/probe"
	"github.com/mimecast/dtail/internal/protocol"
)

// testProbeInFlightWait is the command input's wait for the token in the
// tests that lower it.
const testProbeInFlightWait = 100 * time.Millisecond

// countingProbeSampler counts its samples.
func countingProbeSampler(count *atomic.Int32) probeSampler {
	return func(ctx context.Context, target fs.ValidatedReadTarget, path string,
		options fs.SampleOptions) (fs.Sample, error) {
		count.Add(1)
		return probe.Sample(ctx, target, path, options)
	}
}

// fillServerMessages fills the session's server messages, as a client that
// does not read leaves them.
func fillServerMessages(handler *ServerHandler) {
	for len(handler.serverMessages) < cap(handler.serverMessages) {
		handler.serverMessages <- "filler\n"
	}
}

// handleCommandWithin dispatches command like the session's command input
// and fails the test if that takes longer than limit.
func handleCommandWithin(t *testing.T, handler *ServerHandler, command string, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		handler.handleCommand(encodedTestCommand(command))
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("command input held for more than %v by %.40q", limit, command)
	}
	t.Logf("command handled in %v", time.Since(start))
}

// readProbeAnswers reads the server messages until none arrived for a while
// and returns the probe answers per id.
func readProbeAnswers(t *testing.T, handler *ServerHandler) map[string][]protocol.ProbeAnswer {
	t.Helper()
	answers := map[string][]protocol.ProbeAnswer{}
	for {
		select {
		case message := <-handler.serverMessages:
			failOnGenerationBoundProbeAnswer(t, message)
			if !strings.HasPrefix(message, protocol.HiddenProbePrefix) {
				continue
			}
			answer, err := protocol.DecodeProbeAnswer(message)
			if err != nil {
				t.Fatalf("decode answer %q: %v", message, err)
			}
			answers[answer.ID] = append(answers[answer.ID], answer)
		case <-time.After(300 * time.Millisecond):
			return answers
		}
	}
}

// TestProbeAnswersUnreadBoundTheWork checks that a client that stops reading
// but keeps sending probes can not make dserver sample every probe and keep
// one goroutine per unsent answer: one probe ran, its answer waits, and every
// later probe is answered busy (here dropped, as the server messages are
// full) after at most probeInFlightWait, never sampled. Once the client reads
// again it gets the waiting answer, and the next probe runs.
func TestProbeAnswersUnreadBoundTheWork(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.inFlightWait = testProbeInFlightWait
	var samples atomic.Int32
	handler.probeOverrides.sampler = countingProbeSampler(&samples)
	fillServerMessages(handler)
	probeCommand := func(id string) string {
		return probeCommandFor(t, protocol.ProbeRequest{ID: id, Files: []string{file}})
	}

	const probes = 8
	for i := range probes {
		handleCommandWithin(t, handler, probeCommand(fmt.Sprintf("p%d", i)), time.Second)
		// A client sends the next probe a little later, when the previous
		// one finished sampling (and, without the token, freed its slot
		// while its answer still waits).
		waitForCondition(t, "the probe slot", func() bool { return !handler.probeRunning.Load() })
	}
	if got := samples.Load(); got != 1 {
		t.Fatalf("%d probes sampled while no answer could be queued, want 1", got)
	}
	if _, active := handler.PendingAndActive(); active != 1 {
		t.Fatalf("%d active commands while no answer could be queued, want 1", active)
	}

	answers := readProbeAnswers(t, handler)
	if got := answers["p0"]; len(got) != 1 || got[0].FilesSampled != 1 {
		t.Fatalf("answers of the probe that ran = %+v, want one sampled", got)
	}
	for i := 1; i < probes; i++ {
		for _, answer := range answers[fmt.Sprintf("p%d", i)] {
			t.Fatalf("probe p%d answered %+v while no answer could be queued", i, answer)
		}
	}
	next, _ := runProbe(t, handler, probeCommand("next"))
	if next.ID != "next" || next.FilesSampled != 1 {
		t.Fatalf("probe after the client read again = %+v", next)
	}
}

// TestProbeBusyAnswerDoesNotBlockTheCommandInput checks that a busy or
// invalid answer the full server messages can not take is dropped instead
// of holding the command input.
func TestProbeBusyAnswerDoesNotBlockTheCommandInput(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	started, release := make(chan string, 4), make(chan struct{})
	handler.probeOverrides.sampler = blockingProbeSampler(started, release)
	defer close(release)

	handleCommandWithin(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "running", Files: []string{file}}), time.Second)
	waitForStarted(t, started)
	fillServerMessages(handler)
	handleCommandWithin(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "busy", Files: []string{file}}), time.Second)
	handleCommandWithin(t, handler, "probe not-base64-json", time.Second)
}

// startProbeWithin calls startProbe as the command input does, after the
// caller took the probe slot, and fails the test if it does not return
// within limit. It returns the number of commandFinished calls.
func startProbeWithin(ctx context.Context, t *testing.T, handler *ServerHandler, id string,
	limit time.Duration) *atomic.Int32 {
	t.Helper()
	var finished atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.startProbe(ctx, protocol.ProbeRequest{ID: id, Files: []string{"/x"}},
			func() { finished.Add(1) })
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("startProbe of %s did not return within %v", id, limit)
	}
	return &finished
}

// TestProbeTokenWaitTimesOut checks startProbe's bounded wait for the token:
// a probe that gets no token within probeInFlightWait gives its slot back,
// finishes its command and is answered busy without sampling.
func TestProbeTokenWaitTimesOut(t *testing.T) {
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.inFlightWait = testProbeInFlightWait
	var samples atomic.Int32
	handler.probeOverrides.sampler = countingProbeSampler(&samples)
	handler.probeInFlight <- struct{}{} // an earlier probe's answer waits
	handler.probeRunning.Store(true)
	finished := startProbeWithin(context.Background(), t, handler, "late", 5*time.Second)
	if finished.Load() != 1 || handler.probeRunning.Load() || samples.Load() != 0 {
		t.Fatalf("after the timed out wait: finished=%d running=%v samples=%d",
			finished.Load(), handler.probeRunning.Load(), samples.Load())
	}
	answer, _ := readProbeAnswer(t, handler.serverMessages)
	if answer.ID != "late" || !hasProbeError(answer, "", protocol.ProbeErrorBusy) {
		t.Fatalf("answer of the probe without token = %+v, want busy", answer)
	}
}

// TestProbeTokenWaitEndsWithTheSession checks startProbe's wait for the
// token when the session ends meanwhile (ctx done): the probe gives its slot
// back, finishes its command, neither samples nor answers.
func TestProbeTokenWaitEndsWithTheSession(t *testing.T) {
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.inFlightWait = time.Minute
	var samples atomic.Int32
	handler.probeOverrides.sampler = countingProbeSampler(&samples)
	handler.probeInFlight <- struct{}{}
	handler.probeRunning.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	finished := startProbeWithin(ctx, t, handler, "ended", 5*time.Second)
	if finished.Load() != 1 || handler.probeRunning.Load() || samples.Load() != 0 {
		t.Fatalf("after the session ended: finished=%d running=%v samples=%d",
			finished.Load(), handler.probeRunning.Load(), samples.Load())
	}
	if answers := readProbeAnswers(t, handler); len(answers) != 0 {
		t.Fatalf("answers after the session ended: %+v", answers)
	}
}

// TestProbeSlotIsReleasedOnce checks that a probe whose answer send is held
// back by a slow client does not free the slot of a probe that took it
// meanwhile: a third probe is still busy. The second probe waits for the
// first answer to be queued before it samples (startProbe).
func TestProbeSlotIsReleasedOnce(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.inFlightWait = time.Minute
	started, release := make(chan string, 4), make(chan struct{})
	handler.probeOverrides.sampler = tokenProbeSampler(started, release)
	request := func(id string) string {
		return encodedTestCommand(probeCommandFor(t, protocol.ProbeRequest{ID: id, Files: []string{file}}))
	}

	handler.handleCommand(request("first"))
	waitForStarted(t, started)
	fillServerMessages(handler) // the first answer's send blocks
	release <- struct{}{}
	waitForCondition(t, "the first probe to release its slot", func() bool { return !handler.probeRunning.Load() })

	secondDispatched := make(chan struct{})
	go func() {
		defer close(secondDispatched)
		handler.handleCommand(request("second"))
	}()
	assertSecondProbeWaits(t, started, secondDispatched)
	readUntilProbeAnswer(t, handler, "first")
	waitForStarted(t, started)
	waitForClosed(t, secondDispatched, "the command input to take the second probe")
	waitForCondition(t, "the first probe to finish", func() bool {
		_, active := handler.PendingAndActive()
		return active == 1
	})

	third, _ := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "third", Files: []string{file}}))
	if third.ID != "third" || !hasProbeError(third, "", protocol.ProbeErrorBusy) {
		t.Fatalf("probe while the second runs = %+v, want busy", third)
	}
	release <- struct{}{}
	second, _ := readProbeAnswer(t, handler.serverMessages)
	if second.ID != "second" || second.FilesSampled != 1 {
		t.Fatalf("second probe answer = %+v", second)
	}
}

// assertSecondProbeWaits checks that the second probe neither samples nor
// leaves the command input while the first answer is unsent.
func assertSecondProbeWaits(t *testing.T, started <-chan string, dispatched <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
		t.Fatal("second probe sampled while the first answer was unsent")
	case <-dispatched:
		t.Fatal("command input took the second probe while the first answer was unsent")
	case <-time.After(200 * time.Millisecond):
	}
}

// readUntilProbeAnswer reads the server messages up to the answer of id.
func readUntilProbeAnswer(t *testing.T, handler *ServerHandler, id string) {
	t.Helper()
	for {
		message := readServerMessage(t, handler.serverMessages)
		failOnGenerationBoundProbeAnswer(t, message)
		if !strings.HasPrefix(message, protocol.HiddenProbePrefix) {
			continue
		}
		if answer, _ := protocol.DecodeProbeAnswer(message); answer.ID == id {
			return
		}
	}
}

// writeFloodFile writes a file of about size bytes of log lines.
func writeFloodFile(t *testing.T, dir string, size int) string {
	t.Helper()
	line := strings.Repeat("flood payload line ", 10) + "\n"
	path := filepath.Join(dir, "flood.log")
	if err := os.WriteFile(path, []byte(strings.Repeat(line, size/len(line))), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// startSlowReader reads the session output in 4 KiB pieces, one every 2 ms,
// like a client on a slow link, and counts the bytes. It stops at EOF.
func startSlowReader(handler *ServerHandler, read *atomic.Int64) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		p := make([]byte, 4096)
		for {
			n, err := handler.Read(p)
			read.Add(int64(n))
			if err != nil {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return done
}

// TestProbeFloodDoesNotStopTheCommandInput checks the command input while
// payload outpaces a client that reads: the session output reads payload
// before server messages, so the probe answers stay unread although the
// client reads. Repeated probes then fill the server messages and leave one
// answer waiting; the next probes and a command behind them must still be
// taken within about probeInFlightWait each, as a SESSION UPDATE behind
// them has to be applied for the client's reload.
func TestProbeFloodDoesNotStopTheCommandInput(t *testing.T) {
	dir := t.TempDir()
	probed := writeProbeTestFile(t, dir, "app.log", probeTestStatsLine("n", "1"))
	flood := writeFloodFile(t, dir, 32<<20)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.inFlightWait = testProbeInFlightWait
	var read atomic.Int64
	readerDone := startSlowReader(handler, &read)
	defer func() {
		handler.Shutdown()
		waitForClosed(t, readerDone, "the session reader to end")
	}()

	for _, command := range catCommands(t, flood) {
		handleCommandWithin(t, handler, command, time.Second)
	}
	waitForCondition(t, "the client to read payload", func() bool { return read.Load() > 64<<10 })
	for i := range cap(handler.serverMessages) + 4 {
		handleCommandWithin(t, handler, probeCommandFor(t,
			protocol.ProbeRequest{ID: fmt.Sprintf("f%d", i), Files: []string{probed}}), 2*time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	handleCommandWithin(t, handler, ".ack noop now", 2*time.Second)
	if len(handler.serverMessages) < cap(handler.serverMessages) {
		t.Fatalf("server messages %d of %d: the flood did not leave the answers unread",
			len(handler.serverMessages), cap(handler.serverMessages))
	}
}

// TestProbeOverridesZeroValueIsProduction checks that a handler nothing was
// overridden on, as every handler outside the tests, probes with the
// production values, and that a set field replaces only its own value.
func TestProbeOverridesZeroValueIsProduction(t *testing.T) {
	var none probeOverrides
	if got := none.inFlightWaitOrDefault(); got != probeInFlightWait {
		t.Fatalf("in-flight wait = %v, want %v", got, probeInFlightWait)
	}
	if none.samplerOrDefault() == nil {
		t.Fatal("no default sampler")
	}
	if got, want := none.listLimits.withDefaults(), (probeListLimits{}).withDefaults(); got != want {
		t.Fatalf("list limits = %+v, want the defaults %+v", got, want)
	}
	if got := newSessionTestHandler("probe-overrides-user").probeOverrides; got.inFlightWait != 0 ||
		got.sampler != nil || got.listLimits != (probeListLimits{}) {
		t.Fatalf("a new handler has overrides set: %+v", got)
	}

	var samples atomic.Int32
	set := probeOverrides{inFlightWait: time.Minute, sampler: func(context.Context, fs.ValidatedReadTarget,
		string, fs.SampleOptions) (fs.Sample, error) {
		samples.Add(1)
		return fs.Sample{}, nil
	}}
	if got := set.inFlightWaitOrDefault(); got != time.Minute {
		t.Fatalf("overridden in-flight wait = %v, want 1m", got)
	}
	_, err := set.samplerOrDefault()(context.Background(), fs.ValidatedReadTarget{}, "", fs.SampleOptions{})
	if err != nil || samples.Load() != 1 {
		t.Fatalf("overridden sampler not used: err %v, samples %d", err, samples.Load())
	}
	if (probeOverrides{inFlightWait: -time.Second}).inFlightWaitOrDefault() != probeInFlightWait {
		t.Fatal("a negative in-flight wait is not replaced by the default")
	}
}
