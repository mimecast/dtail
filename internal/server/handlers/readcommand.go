package handlers

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
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

func (r *readCommand) Start(ctx context.Context, lContext lcontext.LContext, argc int, args []string, retries int) {
	re := regex.NewNoop()

	if argc >= 4 {
		deserializedRegex, err := regex.Deserialize(strings.Join(args[2:], " "))
		if err != nil {
			r.server.sendServerMessage(logger.Error(r.server.user, commandParseWarning, err))
			return
		}
		re = deserializedRegex
	}
	if argc < 3 {
		r.server.sendServerWarnMessage(logger.Warn(r.server.user, commandParseWarning, args, argc))
		return
	}
	r.readGlob(ctx, lContext, args[1], re, retries)
}

func (r *readCommand) readGlob(ctx context.Context, lContext lcontext.LContext, glob string, re regex.Regex, retries int) {
	retryInterval := time.Second * 5
	glob = filepath.Clean(glob)

	for retryCount := 0; retryCount < retries; retryCount++ {
		paths, err := filepath.Glob(glob)
		if err != nil {
			logger.Warn(r.server.user, glob, err)
			time.Sleep(retryInterval)
			continue
		}

		if numPaths := len(paths); numPaths == 0 {
			logger.Error(r.server.user, "No such file(s) to read", glob)
			r.server.sendServerWarnMessage(logger.Warn(r.server.user, "Unable to read file(s), check server logs"))
			select {
			case <-ctx.Done():
				return
			default:
			}
			time.Sleep(retryInterval)
			continue
		}

		r.readFiles(ctx, lContext, paths, glob, re, retryInterval)
		return
	}

	r.server.sendServerWarnMessage(logger.Warn(r.server.user, "Giving up to read file(s)"))
	return
}

func (r *readCommand) readFiles(ctx context.Context, lContext lcontext.LContext, paths []string, glob string, re regex.Regex, retryInterval time.Duration) {
	var wg sync.WaitGroup
	wg.Add(len(paths))

	for _, path := range paths {
		go r.readFileIfPermissions(ctx, lContext, &wg, path, glob, re)
	}

	wg.Wait()
}

func (r *readCommand) readFileIfPermissions(ctx context.Context, lContext lcontext.LContext, wg *sync.WaitGroup, path, glob string, re regex.Regex) {
	defer wg.Done()
	globID := r.makeGlobID(path, glob)

	if !r.server.user.HasFilePermission(path, "readfiles") {
		logger.Error(r.server.user, "No permission to read file", path, globID)
		r.server.sendServerWarnMessage(logger.Warn(r.server.user, "Unable to read file(s), check server logs"))
		return
	}

	r.readFile(ctx, lContext, path, globID, re)
}

func (r *readCommand) readFile(ctx context.Context, lContext lcontext.LContext, path, globID string, re regex.Regex) {
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
		if err := reader.Start(ctx, lContext, lines, re); err != nil {
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

	r.server.sendServerWarnMessage(logger.Warn("Empty file path given?", path, glob))
	return ""
}
