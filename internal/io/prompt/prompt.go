package prompt

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/mimecast/dtail/internal/logging"
)

// Answer is a user input of a prompt question.
type Answer struct {
	// Long version of the expected user input
	Long string
	// Short version of the expected user input
	Short string
	// Runs when user input matches
	Callback func()
	// Runs after Callback and after logging resumes
	EndCallback func()
	// AskAgain can be used to not to ask again about the question.
	AskAgain bool
}

// Prompt used for interactive user input.
type Prompt struct {
	question string
	answers  []Answer
	logger   logging.Logger
}

type pausingLogger interface {
	Pause()
	Resume()
}

func (p *Prompt) askString() string {
	var sb strings.Builder
	sb.WriteString(p.question)
	sb.WriteString("? (")

	var ax []string
	for _, a := range p.answers {
		ax = append(ax, fmt.Sprintf("%s=%s", a.Short, a.Long))
	}

	sb.WriteString(strings.Join(ax, ","))
	sb.WriteString("): ")
	return sb.String()
}

// New returns a new prompt.
func New(question string, logger logging.Logger) *Prompt {
	return &Prompt{question: question, logger: logging.OrNop(logger)}
}

// Add an answer.
func (p *Prompt) Add(answer Answer) {
	p.answers = append(p.answers, answer)
}

// Ask a question using input for answers. A read error selects the "no" answer,
// when present, and returns so non-interactive callers fail closed.
func (p *Prompt) Ask(input io.Reader) {
	reader := bufio.NewReader(input)
	p.pauseLogging()

	for {
		_, _ = fmt.Print(p.askString())
		answerStr, err := reader.ReadString('\n')
		if err != nil {
			answer, _ := p.answer("no")
			p.handleAnswer(answer, false)
			return
		}

		if a, ok := p.answer(strings.TrimSpace(answerStr)); ok {
			if p.handleAnswer(a, true) {
				return
			}
		}
	}
}

func (p *Prompt) handleAnswer(answer *Answer, allowAskAgain bool) bool {
	if answer != nil && answer.Callback != nil {
		answer.Callback()
	}
	if answer != nil && allowAskAgain && answer.AskAgain {
		return false
	}

	p.resumeLogging()
	if answer != nil && answer.EndCallback != nil {
		answer.EndCallback()
	}
	return true
}

func (p *Prompt) pauseLogging() {
	if logger, ok := p.logger.(pausingLogger); ok {
		logger.Pause()
	}
}

func (p *Prompt) resumeLogging() {
	if logger, ok := p.logger.(pausingLogger); ok {
		logger.Resume()
	}
}

func (p *Prompt) answer(answerStr string) (*Answer, bool) {
	for _, a := range p.answers {
		switch answerStr {
		case a.Long:
			return &a, true
		case a.Short:
			return &a, true
		default:
		}
	}
	return nil, false
}
