package handlers

import (
	"context"
	"strings"

	"github.com/mimecast/dtail/internal/mapr/server"
)

// Map command implements the mapreduce command server side.
type mapCommand struct {
	aggregate *server.Aggregate
	server    *ServerHandler
}

// NewMapCommand returns a new server side mapreduce command.
func newMapCommand(serverHandler *ServerHandler, argc int, args []string) (mapCommand, *server.Aggregate, error) {
	m := mapCommand{server: serverHandler}

	queryStr := strings.Join(args[1:], " ")
	aggregate, err := server.NewAggregate(queryStr)
	if err != nil {
		return m, nil, err
	}

	m.aggregate = aggregate
	return m, aggregate, nil

}

func (m mapCommand) Start(ctx context.Context, aggregatedMessages chan<- string) {
	m.aggregate.Start(ctx, aggregatedMessages)
}
