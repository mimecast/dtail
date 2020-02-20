package background

import (
	"context"
	"sync"
)

type command struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type Commands struct {
	mutex    sync.Mutex
	commands map[string]command
}

func NewCommands() *Commands {
	return &Commands{
		commands: make(map[string]command),
	}
}

func (b Commands) Add(argc int, args []string, cancel context.CancelFunc, done <-chan struct{}) {
}

func (h Commands) Stop(argc int, args []string) {
}
