package clients

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakePromptRouter struct {
	prompting atomic.Bool
	input     io.Reader
}

func (r *fakePromptRouter) SetPromptInput(input io.Reader) { r.input = input }
func (r *fakePromptRouter) Prompting() bool                { return r.prompting.Load() }

// Lines typed while the host key question is asked go to the prompt, all
// others stay with the control loop; the prompt gets io.EOF once the control
// loop ended.
func TestPromptAnswersRoutesLinesOnlyWhilePrompting(t *testing.T) {
	router := &fakePromptRouter{}
	answers := &promptAnswers{router: router, lines: make(chan []byte, 16)}

	if answers.take(":show") {
		t.Fatal("a line was taken for the prompt although no question is asked")
	}
	router.prompting.Store(true)
	if !answers.take("yes") {
		t.Fatal("the answer was not taken for the prompt")
	}
	router.prompting.Store(false)

	reader := bufio.NewReader(answers)
	line, err := reader.ReadString('\n')
	if err != nil || line != "yes\n" {
		t.Fatalf("prompt read %q, %v; want the answer line", line, err)
	}
	answers.close()
	answers.close()
	if _, err := reader.ReadString('\n'); err != io.EOF {
		t.Fatalf("prompt read after the control loop ended: %v, want io.EOF", err)
	}

	var none *promptAnswers
	if none.take("yes") {
		t.Fatal("nil answers took a line")
	}
	none.close()
}

// An answer to the host key question does not enter the history; lines
// entered while no question is asked do.
func TestControlHistorySkipsAnswers(t *testing.T) {
	var asking atomic.Bool
	h := &controlHistory{skip: asking.Load}
	h.Add(":show")
	asking.Store(true)
	h.Add("yes")
	asking.Store(false)
	h.Add(":help")
	if h.Len() != 2 || h.entries[0] != ":help" || h.entries[1] != ":show" {
		t.Fatalf("history = %q, want [:help :show]", h.entries)
	}
}

// The interrupt statistics go to the writer set with setInterruptOutput, and
// no longer once it is reset.
func TestStatsInterruptOutput(t *testing.T) {
	s := newTailStats(1, nil, time.Millisecond, nil)
	var out bytes.Buffer
	s.setInterruptOutput(&out)
	s.printStatsDueInterrupt([]string{"Hint: Hit Ctrl+C again to exit"})
	if got := out.String(); got != " Hint: Hit Ctrl+C again to exit\n" {
		t.Fatalf("interrupt output = %q", got)
	}
	s.setInterruptOutput(nil)
	if s.interruptOut.Load() != nil {
		t.Fatal("interrupt output not reset to stdout")
	}
	s.printStatsDueInterrupt([]string{"to stdout"})
	if strings.Contains(out.String(), "to stdout") {
		t.Fatal("statistics written to the reset writer")
	}
}
