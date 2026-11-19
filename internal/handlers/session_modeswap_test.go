package handlers

// Tests that one session switches between a plain tail read and a MapReduce
// query with SESSION UPDATE, both ways, in server mode (dserver) and in the
// serverless runtime. The interactive dtail client switches its running
// session this way, so dserver and the serverless runtime must
// accept a spec that gains or drops its query on update: session.Spec.Commands
// builds the read commands of a plain read (readCommands) or the map command
// and its reads (queryCommands) from the spec as it is, and
// sessionCommandState.update cancels the previous workload, whatever it was,
// and resets the aggregate (resetSessionAggregates).

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/session"
)

// modeSwapAggregate is the field of an aggregate frame of modeSwapQuery.
const (
	modeSwapQuery     = "from STATS select count($time),$time group by $time interval 1"
	modeSwapAggregate = "count($time)≔"
)

func TestSessionUpdateSwitchesTailToQueryAndBack(t *testing.T) {
	for _, serverless := range []bool{false, true} {
		name := "dserver"
		if serverless {
			name = "serverless"
		}
		t.Run(name, func(t *testing.T) {
			testSessionUpdateSwitchesTailToQueryAndBack(t, serverless)
		})
	}
}

func testSessionUpdateSwitchesTailToQueryAndBack(t *testing.T, serverless bool) {
	// Payload goes into the session stream in server mode and into the
	// runtime's own output in serverless mode; aggregates and
	// acknowledgements always go into the session stream.
	stream, payload := &testOutput{}, &testOutput{}
	handler := newMapTestHandlerWithOutput(t, handlerTestLogger, nil)
	options := "plain=true"
	if serverless {
		handler = newMapTestHandlerWithOutput(t, handlerTestLogger, payload)
		options += ":serverless=true"
	} else {
		payload = stream
	}
	path := writeTestStatsFile(t, 0)
	commandWg := wrapHandlerCommandsForJoin(handler)
	var writeMu sync.Mutex
	readerDone := startTestReader(handler, stream, &writeMu)
	stopFeeder := startModeSwapFeeder(t, path)

	tail := session.Spec{Mode: omode.TailClient, Files: []string{path}, Options: options, Regex: "PLAIN"}
	writeModeSwapCommand(t, handler, &writeMu, mustStartCommand(t, tail))
	stream.waitForContains(t, sessionAckStartOKPrefix+" 1", 5*time.Second)
	payload.waitForContains(t, "PLAIN line", 10*time.Second)

	query := session.Spec{Mode: omode.TailClient, Files: []string{path}, Options: options,
		Query: modeSwapQuery, Regex: `\|MAPREDUCE:STATS\|`}
	writeModeSwapCommand(t, handler, &writeMu, mustUpdateCommand(t, query, 2))
	stream.waitForContains(t, sessionAckUpdateOKPrefix+" 2", 5*time.Second)
	waitForAfter(t, stream, sessionAckUpdateOKPrefix+" 2", modeSwapAggregate, 15*time.Second)

	back := tail
	back.Regex = "WARN"
	writeModeSwapCommand(t, handler, &writeMu, mustUpdateCommand(t, back, 3))
	stream.waitForContains(t, sessionAckUpdateOKPrefix+" 3", 5*time.Second)
	waitForAfter(t, payload, "", "WARN line", 10*time.Second)
	// The query's aggregate is gone with its generation: its interval is 1 s,
	// so a still running aggregate would have sent another frame by now.
	time.Sleep(2500 * time.Millisecond)
	if after := textAfter(stream.String(), sessionAckUpdateOKPrefix+" 3"); strings.Contains(after, modeSwapAggregate) {
		t.Fatalf("aggregate frames after switching back to the tail read: %q", after)
	}

	stopFeeder()
	handler.Shutdown()
	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not observe EOF after handler shutdown")
	}
	waitForCommandJoin(t, commandWg, 10*time.Second)
}

// startModeSwapFeeder appends a PLAIN, a WARN and a STATS line to path every
// 50 ms until the returned stop is called, so that every workload of the
// session, whenever its tail read opened the file, finds lines to read.
func startModeSwapFeeder(t *testing.T, path string) (stop func()) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open feeder file: %v", err)
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		defer func() { _ = file.Close() }()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			case <-time.After(50 * time.Millisecond):
			}
			lines := fmt.Sprintf("PLAIN line %d\nWARN line %d\n%s\n", i, i, testStatsLine)
			if _, err := file.WriteString(lines); err != nil {
				return
			}
		}
	}()
	var once sync.Once
	stop = func() { once.Do(func() { close(done); <-stopped }) }
	t.Cleanup(stop)
	return stop
}

func writeModeSwapCommand(t *testing.T, handler *ServerHandler, writeMu *sync.Mutex, command string) {
	t.Helper()
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := handler.Write([]byte(encodeTestCommand(command))); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}
}

func mustStartCommand(t *testing.T, spec session.Spec) string {
	t.Helper()
	command, err := spec.StartCommand()
	if err != nil {
		t.Fatalf("build session start: %v", err)
	}
	return command
}

func mustUpdateCommand(t *testing.T, spec session.Spec, generation uint64) string {
	t.Helper()
	command, err := spec.UpdateCommand(generation)
	if err != nil {
		t.Fatalf("build session update: %v", err)
	}
	return command
}

// waitForAfter waits until output contains want after the first occurrence
// of marker ("" for anywhere).
func waitForAfter(t *testing.T, output *testOutput, marker, want string, timeout time.Duration) {
	t.Helper()
	waitForHandlerCondition(t, timeout, "timed out waiting for expected output", func() bool {
		return strings.Contains(textAfter(output.String(), marker), want)
	}, func() string {
		return fmt.Sprintf("expected %q after %q in %q", want, marker, output.String())
	})
}

// textAfter returns the part of text after the first occurrence of marker,
// "" if there is none, and all of text for an empty marker.
func textAfter(text, marker string) string {
	if marker == "" {
		return text
	}
	_, after, found := strings.Cut(text, marker)
	if !found {
		return ""
	}
	return after
}
