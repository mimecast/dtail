package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/omode"
)

type readCommand struct {
	server *ServerHandler
	mode   omode.Mode
}

func newReadCommand(server *ServerHandler, mode omode.Mode) *readCommand {
	return &readCommand{
		server: server,
		mode:   mode,
	}
}

func (r *readCommand) Start(ctx context.Context, argc int, args []string) {
	regex := "."
	if argc >= 4 {
		switch args[2] {
		}
		regex = strings.Join(args[3:], " ")
		logger.Debug("Joined regex", regex)
	}
	if argc < 3 {
		r.server.sendServerMessage(logger.Warn(r.server.user, commandParseWarning, args, argc))
		return
	}
	r.readGlob(ctx, args[1], regex)
}

func (r *readCommand) readGlob(ctx context.Context, glob string, regex string) {
	retryInterval := time.Second * 5
	glob = filepath.Clean(glob)

	maxRetries := 10
	for {
		maxRetries--
		if maxRetries < 0 {
			r.server.sendServerMessage(logger.Warn(r.server.user, "Giving up to read file(s)"))
			return
		}

		paths, err := filepath.Glob(glob)
		if err != nil {
			logger.Warn(r.server.user, glob, err)
			time.Sleep(retryInterval)
			continue
		}

		if numPaths := len(paths); numPaths == 0 {
			logger.Error(r.server.user, "No such file(s) to read", glob)
			r.server.sendServerMessage(logger.Warn(r.server.user, "Unable to read file(s), check server logs"))
			select {
			case <-ctx.Done():
				return
			default:
			}
			time.Sleep(retryInterval)
			continue
		}

		r.readFiles(ctx, paths, glob, regex, retryInterval)
		break
	}
}

func (r *readCommand) readFiles(ctx context.Context, paths []string, glob string, regex string, retryInterval time.Duration) {
	var wg sync.WaitGroup
	wg.Add(len(paths))

	for _, path := range paths {
		go r.readFileIfPermissions(ctx, &wg, path, glob, regex)
	}

	wg.Wait()
}

func (r *readCommand) readFileIfPermissions(ctx context.Context, wg *sync.WaitGroup, path, glob, regex string) {
	defer wg.Done()
	globID := r.makeGlobID(path, glob)

	if !r.server.user.HasFilePermission(path, "readfiles") {
		logger.Error(r.server.user, "No permission to read file", path, globID)
		r.server.sendServerMessage(logger.Warn(r.server.user, "Unable to read file(s), check server logs"))
		return
	}

	r.readFile(ctx, path, globID, regex)
}

func (r *readCommand) readFile(ctx context.Context, path, globID, regex string) {
	logger.Info(r.server.user, "Start reading file", path, globID)

	var reader fs.FileReader
	switch r.mode {
	case omode.TailClient:
		reader = fs.NewTailFile(path, globID, r.server.serverMessages, r.server.tailLimiter)
	case omode.GrepClient, omode.CatClient:
		reader = fs.NewCatFile(path, globID, r.server.serverMessages, r.server.catLimiter)
	default:
		reader = fs.NewTailFile(path, globID, r.server.serverMessages, r.server.tailLimiter)
	}

	lines := r.server.lines

	// Plug in mappreduce engine
	if r.server.aggregate != nil {
		lines = r.server.aggregate.Lines
	}

	for {
		if err := reader.Start(ctx, lines, regex); err != nil {
			logger.Error(r.server.user, path, globID, err)
		}

		select {
		case <-ctx.Done():
			return
		default:
			if !reader.Retry() {
				return
			}
		}

		time.Sleep(time.Second * 2)
		logger.Info(path, globID, "Reading file again")
	}
}

func (r *readCommand) makeGlobID(path, glob string) string {
	var idParts []string
	pathParts := strings.Split(path, "/")

	for i, globPart := range strings.Split(glob, "/") {
		if strings.Contains(globPart, "*") {
			idParts = append(idParts, pathParts[i])
		}
	}

	if len(idParts) > 0 {
		return strings.Join(idParts, "/")
	}

	if len(pathParts) > 0 {
		return pathParts[len(pathParts)-1]
	}

	r.server.sendServerMessage(logger.Error("Empty file path given?", path, glob))
	return ""
}
