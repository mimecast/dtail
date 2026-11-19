package handlers

// Tests of the probe command's place in a session: it answers once, runs one
// at a time, never ends the session and reports no failed command, in server
// and in serverless mode.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr/probe"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
)

// blockingProbeSampler holds every sample until release is closed and tells
// started about each. Every wait also ends with the probe's context.
func blockingProbeSampler(started chan<- string, release <-chan struct{}) probeSampler {
	return func(ctx context.Context, target fs.ValidatedReadTarget, path string,
		options fs.SampleOptions) (fs.Sample, error) {
		select {
		case started <- path:
		case <-ctx.Done(): // nobody takes the start, e.g. too many samples
			return fs.Sample{}, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return fs.Sample{}, ctx.Err()
		}
		return probe.Sample(ctx, target, path, options)
	}
}

// waitForStarted waits until the blocking sampler was called.
func waitForStarted(t *testing.T, started <-chan string) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("probe did not start sampling")
	}
}

// waitForClosed waits until done is closed and fails the test when that
// takes longer than 10 s, instead of hanging until the test binary times out.
func waitForClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestProbeRunsOneAtATime checks that a probe sent while another runs is
// answered at once as busy, and that the next probe after the answer runs.
func TestProbeRunsOneAtATime(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	started, release := make(chan string, 4), make(chan struct{})
	handler.probeOverrides.sampler = blockingProbeSampler(started, release)

	handler.handleCommand(encodedTestCommand(probeCommandFor(t, protocol.ProbeRequest{ID: "first", Files: []string{file}})))
	waitForStarted(t, started)

	busy, _ := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "second", Files: []string{file}}))
	if busy.ID != "second" || busy.FilesSampled != 0 || !hasProbeError(busy, "", protocol.ProbeErrorBusy) {
		t.Fatalf("concurrent probe answer = %+v, want busy", busy)
	}

	close(release)
	first, _ := readProbeAnswer(t, handler.serverMessages)
	if first.ID != "first" || first.FilesSampled != 1 || len(first.Errors) != 0 {
		t.Fatalf("first probe answer = %+v", first)
	}

	third, _ := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "third", Files: []string{file}}))
	if third.ID != "third" || third.FilesSampled != 1 || len(third.Errors) != 0 {
		t.Fatalf("probe after the first one's answer = %+v", third)
	}
}

// probeSession drives a handler like a client: it writes frames and reads
// the output until the session ends.
type probeSession struct {
	handler    *ServerHandler
	output     *testOutput
	writeMu    sync.Mutex
	readerDone <-chan struct{}
}

func startProbeSession(handler *ServerHandler) *probeSession {
	s := &probeSession{handler: handler, output: &testOutput{}}
	s.readerDone = startTestReader(handler, s.output, &s.writeMu)
	return s
}

func (s *probeSession) send(t *testing.T, commands ...string) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.handler.Write([]byte(encodeTestCommands(commands...))); err != nil {
		t.Fatalf("write commands: %v", err)
	}
}

// waitForOutput waits until the output contains text.
func (s *probeSession) waitForOutput(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(s.output.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("output lacks %q: %q", text, s.output.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertRunning checks that the session is still open after a while.
func (s *probeSession) assertRunning(t *testing.T) {
	t.Helper()
	select {
	case <-s.handler.Done():
		t.Fatalf("session ended: %q", s.output.String())
	case <-time.After(300 * time.Millisecond):
	}
}

// waitForEnd waits until the session ended and the reader saw its EOF.
func (s *probeSession) waitForEnd(t *testing.T) string {
	t.Helper()
	for _, done := range []<-chan struct{}{s.handler.Done(), s.readerDone} {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("session did not end: %q", s.output.String())
		}
	}
	return s.output.String()
}

// catCommands returns the command stream of a cat of file.
func catCommands(t *testing.T, file string) []string {
	t.Helper()
	commands, err := session.Spec{Mode: omode.CatClient, Files: []string{file}, Options: "plain=true"}.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	return commands
}

// TestProbeKeepsTheSessionRunning checks that a probe answers with a hidden
// frame and leaves the session open, in server and in serverless mode, and
// that the session's work after it ends the session as usual.
func TestProbeKeepsTheSessionRunning(t *testing.T) {
	for _, serverless := range []bool{false, true} {
		t.Run(map[bool]string{false: "server", true: "serverless"}[serverless], func(t *testing.T) {
			file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("probed", "1"))
			var serverlessOutput io.Writer
			if serverless {
				serverlessOutput = &testOutput{}
			}
			s := startProbeSession(newProbeTestHandler(t, []string{"readfiles:.*"}, serverlessOutput))

			s.send(t, probeCommandFor(t, protocol.ProbeRequest{ID: "keep", Files: []string{file}}))
			s.waitForOutput(t, protocol.HiddenProbePrefix+"keep ")
			s.assertRunning(t)

			s.send(t, catCommands(t, file)...)
			output := s.waitForEnd(t)
			if strings.Contains(output, protocol.HiddenCommandFailedPrefix) {
				t.Fatalf("probe session reported a failed command: %q", output)
			}
			if !strings.Contains(output, ".syn close connection") {
				t.Fatalf("session after the probe did not close normally: %q", output)
			}
		})
	}
}

// TestProbeEndsTheSessionWhenOtherWorkFinishedMeanwhile checks that the
// session's work finishing while a probe runs does not leave the session
// open: the probe starts the idle shutdown when it finishes, after its
// answer.
func TestProbeEndsTheSessionWhenOtherWorkFinishedMeanwhile(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("probed", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	started, release := make(chan string, 4), make(chan struct{})
	handler.probeOverrides.sampler = blockingProbeSampler(started, release)
	s := startProbeSession(handler)

	s.send(t, probeCommandFor(t, protocol.ProbeRequest{ID: "slow", Files: []string{file}}))
	waitForStarted(t, started)
	s.send(t, catCommands(t, file)...)
	s.waitForOutput(t, "probed=1")
	s.assertRunning(t)

	close(release)
	output := s.waitForEnd(t)
	answer := strings.Index(output, protocol.HiddenProbePrefix+"slow ")
	closing := strings.Index(output, ".syn close connection")
	if answer < 0 || closing < answer {
		t.Fatalf("want the probe answer before the close handshake: %q", output)
	}
}

// tokenProbeSampler holds every sample until it gets a token from release
// and tells started about each. Every wait also ends with the probe's context.
func tokenProbeSampler(started chan<- string, release <-chan struct{}) probeSampler {
	return func(ctx context.Context, target fs.ValidatedReadTarget, path string,
		options fs.SampleOptions) (fs.Sample, error) {
		select {
		case started <- path:
		case <-ctx.Done(): // nobody takes the start, e.g. too many samples
			return fs.Sample{}, ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
			return fs.Sample{}, ctx.Err()
		}
		return probe.Sample(ctx, target, path, options)
	}
}

// waitForCondition polls condition until it holds.
func waitForCondition(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestProbeTakesNoLimiterSlot checks that a probe answers while every cat
// and tail slot of the server is taken.
func TestProbeTakesNoLimiterSlot(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log", probeTestStatsLine("n", "1"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	for _, limiter := range []chan struct{}{handler.catLimiter, handler.tailLimiter} {
		for len(limiter) < cap(limiter) {
			limiter <- struct{}{}
		}
	}
	answer, _ := probeFiles(t, handler, file)
	if answer.FilesSampled != 1 || len(answer.Errors) != 0 {
		t.Fatalf("probe with all limiter slots taken = %+v", answer)
	}
}
