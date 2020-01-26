package prompt

import (
	"bufio"
	"fmt"
	"github.com/mimecast/dtail/internal/io/logger"
	"os"
	"strings"
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

	AskAgain bool
}

// Prompt used for interactive user input.
type Prompt struct {
	question string
	answers  []Answer
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
func New(question string) *Prompt {
	return &Prompt{question: question}
}

// Add an answer.
func (p *Prompt) Add(answer Answer) {
	p.answers = append(p.answers, answer)
}

// Ask a question.
func (p *Prompt) Ask() {
	reader := bufio.NewReader(os.Stdin)
	logger.Pause()

	for {
		fmt.Print(p.askString())
		answerStr, _ := reader.ReadString('\n')

		if a, ok := p.answer(strings.TrimSpace(answerStr)); ok {
			if a.Callback != nil {
				a.Callback()
			}

			if !a.AskAgain {
				logger.Resume()
				if a.EndCallback != nil {
					a.EndCallback()
				}
				return
			}
		}
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
