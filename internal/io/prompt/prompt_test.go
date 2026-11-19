package prompt

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

type singleReadReader struct {
	data string
	err  error
	read bool
}

func (r *singleReadReader) Read(buf []byte) (int, error) {
	if r.read {
		panic("prompt read input again after a read error")
	}
	r.read = true
	return copy(buf, r.data), r.err
}

type recordingLogger struct {
	logging.NopLogger
	events *[]string
}

func (l *recordingLogger) Pause() {
	*l.events = append(*l.events, "pause")
}

func (l *recordingLogger) Resume() {
	*l.events = append(*l.events, "resume")
}

func TestAskHandlesInput(t *testing.T) {
	readErr := errors.New("input unavailable")
	tests := []struct {
		name       string
		input      io.Reader
		wantEvents []string
	}{
		{
			name:       "accepts valid answer",
			input:      strings.NewReader("yes\n"),
			wantEvents: []string{"pause", "yes", "resume", "yes end"},
		},
		{
			name:       "retries invalid answer",
			input:      strings.NewReader("maybe\nno\n"),
			wantEvents: []string{"pause", "no", "resume", "no end"},
		},
		{
			name:       "rejects empty EOF",
			input:      &singleReadReader{err: io.EOF},
			wantEvents: []string{"pause", "no", "resume", "no end"},
		},
		{
			name:       "rejects partial answer at EOF",
			input:      &singleReadReader{data: "yes", err: io.EOF},
			wantEvents: []string{"pause", "no", "resume", "no end"},
		},
		{
			name:       "rejects read error",
			input:      &singleReadReader{err: readErr},
			wantEvents: []string{"pause", "no", "resume", "no end"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			p := testPrompt(&events)

			p.Ask(tt.input)

			if !reflect.DeepEqual(events, tt.wantEvents) {
				t.Fatalf("Ask events = %v, want %v", events, tt.wantEvents)
			}
		})
	}
}

func TestAskContinuesAfterAskAgainAnswer(t *testing.T) {
	var events []string
	p := testPrompt(&events)
	p.Add(Answer{
		Long:     "details",
		Short:    "d",
		AskAgain: true,
		Callback: func() { events = append(events, "details") },
	})

	p.Ask(strings.NewReader("details\nyes\n"))

	wantEvents := []string{"pause", "details", "yes", "resume", "yes end"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("Ask events = %v, want %v", events, wantEvents)
	}
}

func TestAskWithoutNoAnswerReturnsOnReadError(t *testing.T) {
	var events []string
	p := New("Continue", &recordingLogger{events: &events})
	p.Add(Answer{
		Long:     "yes",
		Short:    "y",
		Callback: func() { events = append(events, "yes") },
	})

	p.Ask(strings.NewReader(""))

	wantEvents := []string{"pause", "resume"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Fatalf("Ask events = %v, want %v", events, wantEvents)
	}
}

func testPrompt(events *[]string) *Prompt {
	p := New("Continue", &recordingLogger{events: events})
	p.Add(Answer{
		Long:        "yes",
		Short:       "y",
		Callback:    func() { *events = append(*events, "yes") },
		EndCallback: func() { *events = append(*events, "yes end") },
	})
	p.Add(Answer{
		Long:        "no",
		Short:       "n",
		Callback:    func() { *events = append(*events, "no") },
		EndCallback: func() { *events = append(*events, "no end") },
	})
	return p
}
