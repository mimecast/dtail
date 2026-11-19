package clients

import (
	"bufio"
	"io"
	"sync/atomic"
	"testing"
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
