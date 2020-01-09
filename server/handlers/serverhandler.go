package handlers

import (
	"dtail/config"
	"dtail/fs"
	"dtail/logger"
	"dtail/mapr/server"
	"dtail/omode"
	"dtail/server/user"
	"dtail/version"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	commandParseWarning string = "Unable to parse command"
)

// ServerHandler implements the Reader and Writer interfaces to handle
// the Bi-directional communication between SSH client and server.
// This handler implements the handler of the SSH server.
type ServerHandler struct {
	// Local log file readers
	fileReaders    []fs.FileReader
	fileReadersMtx *sync.Mutex
	// Channel for read lines.
	lines chan fs.LineRead
	// Only process log lines matching this regex.
	regex string
	// Server side mapr log aggregation.
	aggregate *server.Aggregate
	// Channel of aggregated log lines.
	aggregatedMessages chan string
	// Channel for server messages to be sent to the client.
	serverMessages chan string
	// Channel for hidden messages to be sent to the client.
	hiddenMessages chan string
	// The current payload sent to the client.
	payload []byte
	// The current server hostname.
	hostname string
	// The user connecting to dtail.
	user *user.User
	// To limit the server wide max amount of concurrent cats
	catLimiter chan struct{}
	// To limit the server wide max amount of concurrent tails
	tailLimiter chan struct{}
	// Server can tell handler to stop the handler.
	stop chan struct{}
	// Indicate that client responded to server with "ack stop connection"
	ackStopReceived chan struct{}
	// Stop timeout.
	stopTimeout chan struct{}
}

// NewServerHandler returns the server handler.
func NewServerHandler(user *user.User, catLimiter chan struct{}, tailLimiter chan struct{}) *ServerHandler {
	logger.Debug(user, "Creating tail handler")
	h := ServerHandler{
		fileReadersMtx:     &sync.Mutex{},
		lines:              make(chan fs.LineRead, 100),
		serverMessages:     make(chan string, 10),
		aggregatedMessages: make(chan string, 10),
		hiddenMessages:     make(chan string, 10),
		ackStopReceived:    make(chan struct{}),
		stopTimeout:        make(chan struct{}),
		stop:               make(chan struct{}),
		catLimiter:         catLimiter,
		tailLimiter:        tailLimiter,
		regex:              ".",
		user:               user,
	}

	fqdn, err := os.Hostname()
	if err != nil {
		logger.FatalExit(err)
	}

	s := strings.Split(fqdn, ".")
	h.hostname = s[0]

	return &h
}

// Read is to send data to the dtail client via Reader interface.
func (h *ServerHandler) Read(p []byte) (n int, err error) {
	for {
		select {
		case message := <-h.serverMessages:
			wholePayload := []byte(fmt.Sprintf("SERVER|%s|%s\n", h.hostname, message))
			n = copy(p, wholePayload)
			return
		case message := <-h.aggregatedMessages:
			data := fmt.Sprintf("AGGREGATE|%s|%s\n", h.hostname, message)
			//logger.Debug("Sending aggregation data", data)
			wholePayload := []byte(data)
			n = copy(p, wholePayload)
			return
		case message := <-h.hiddenMessages:
			//logger.Debug(h.user, "Sending hidden message", message)
			wholePayload := []byte(fmt.Sprintf(".%s\n", message))
			n = copy(p, wholePayload)
			return
		case line := <-h.lines:
			serverInfo := []byte(fmt.Sprintf("REMOTE|%s|%3d|%v|%s|",
				h.hostname, line.TransmittedPerc, line.Count, *line.GlobID))
			wholePayload := append(serverInfo, line.Content[:]...)
			n = copy(p, wholePayload)
			return
		case <-time.After(time.Second):
			select {
			case <-h.stop:
				return 0, io.EOF
			default:
			}
		}
	}
}

// Write is to receive data from the dtail client via Writer interface.
func (h *ServerHandler) Write(p []byte) (n int, err error) {
	for _, c := range p {
		switch c {
		case ';':
			commandStr := strings.TrimSpace(string(h.payload))
			h.handleCommand(commandStr)
			h.payload = nil
		default:
			h.payload = append(h.payload, c)
		}
	}

	n = len(p)
	return
}

// Close the server handler.
func (h *ServerHandler) Close() {
	h.fileReadersMtx.Lock()
	defer h.fileReadersMtx.Unlock()

	for _, reader := range h.fileReaders {
		reader.Stop()
	}
	if h.aggregate != nil {
		h.aggregate.Close()
	}

	close(h.stop)
}

func (h *ServerHandler) makeGlobID(path, glob string) string {
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

	h.send(h.serverMessages, logger.Error("Empty file path given?", path, glob))
	return ""
}

func (h *ServerHandler) processFileGlob(mode omode.Mode, glob string, regex string) {
	retryInterval := time.Second * 5
	glob = filepath.Clean(glob)

	errors := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)

	go func() {
		for {
			select {
			case <-errors:
				h.send(h.serverMessages, logger.Warn(h.user, "Unable to read file(s), check server logs"))
			case <-stop:
				return
			case <-h.stop:
				return
			}
		}
	}()

	maxRetries := 10
	for {
		maxRetries--
		if maxRetries < 0 {
			h.send(h.serverMessages, logger.Warn(h.user, "Giving up to read file(s)"))
			h.internalClose()
			return
		}

		paths, err := filepath.Glob(glob)
		if err != nil {
			logger.Warn(h.user, glob, err)
			time.Sleep(retryInterval)
			continue
		}

		if numPaths := len(paths); numPaths == 0 {
			logger.Error(h.user, "No such file(s) to read", glob)
			select {
			case errors <- struct{}{}:
			case <-h.stop:
				return
			default:
			}
			time.Sleep(retryInterval)
			continue
		}

		h.startReadingFiles(mode, paths, glob, regex, retryInterval, errors)
		break
	}
}

func (h *ServerHandler) startReadingFiles(mode omode.Mode, paths []string, glob string, regex string, retryInterval time.Duration, errors chan<- struct{}) {
	var wg sync.WaitGroup
	wg.Add(len(paths))

	read := func(path string, wg *sync.WaitGroup) {
		defer wg.Done()
		globID := h.makeGlobID(path, glob)

		if !h.user.HasFilePermission(path) {
			logger.Error(h.user, "No permission to read file", path, globID)
			select {
			case errors <- struct{}{}:
			default:
			}
			return
		}

		h.startReadingFile(mode, path, globID, regex)
	}

	for _, path := range paths {
		go read(path, &wg)
	}

	wg.Wait()
}

func (h *ServerHandler) startReadingFile(mode omode.Mode, path, globID, regex string) {
	defer h.stopReadingFile(path)
	logger.Info(h.user, "Start reading file", path, globID)

	var reader fs.FileReader
	switch mode {
	case omode.TailClient:
		reader = fs.NewTailFile(path, globID, h.serverMessages, h.tailLimiter)
	case omode.GrepClient:
		fallthrough
	case omode.CatClient:
		reader = fs.NewCatFile(path, globID, h.serverMessages, h.catLimiter)
	default:
		reader = fs.NewTailFile(path, globID, h.serverMessages, h.tailLimiter)
	}

	h.fileReadersMtx.Lock()
	h.fileReaders = append(h.fileReaders, reader)
	h.fileReadersMtx.Unlock()

	lines := h.lines
	// Plugin mappreduce engine
	if h.aggregate != nil {
		lines = h.aggregate.Lines
	}

	for {
		if err := reader.Start(lines, regex); err != nil {
			logger.Error(h.user, path, globID, err)
		}

		select {
		case <-h.stop:
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

func (h *ServerHandler) stopReadingFile(path string) {
	logger.Info(h.user, "Stop reading file", path)

	h.fileReadersMtx.Lock()
	defer h.fileReadersMtx.Unlock()

	path = filepath.Clean(path)
	var fileReaders []fs.FileReader

	for _, reader := range h.fileReaders {
		if reader.FilePath() == path {
			reader.Stop()
			continue
		}
		fileReaders = append(fileReaders, reader)
	}

	if len(fileReaders) == len(h.fileReaders) {
		logger.Warn(h.user, "Didn't read file path", path)
		return
	}

	h.fileReaders = fileReaders

	if len(fileReaders) == 0 {
		if h.aggregate != nil {
			h.aggregate.Serialize()
		}
		h.allLinesSent()
	}
}

func (h *ServerHandler) numUnsentMessages() int {
	return len(h.lines) + len(h.serverMessages) + len(h.hiddenMessages) + len(h.aggregatedMessages)
}

func (h *ServerHandler) allLinesSent() {
	defer h.internalClose()

	for i := 0; i < 3; i++ {
		if h.numUnsentMessages() == 0 {
			logger.Debug(h.user, "All lines sent")
			return
		}
		logger.Debug(h.user, "Still lines to be sent")
		time.Sleep(time.Second)
	}

	logger.Warn(h.user, "Some lines remain unsent", h.numUnsentMessages())
}

// Handler decides to shutdown the connection, not the server itself.
func (h *ServerHandler) internalClose() {
	select {
	case h.hiddenMessages <- "syn close connection":
	case <-time.After(time.Second * 5):
		logger.Debug(h.user, "Not waiting for ack close connection")
		close(h.stopTimeout)
		return
	}

	select {
	case <-h.Wait():
	case <-time.After(time.Second * 5):
		logger.Debug(h.user, "Not waiting for ack close connection")
		close(h.stopTimeout)
	}
}

func (h *ServerHandler) handleCommand(commandStr string) {
	logger.Info(h.user, commandStr)

	args := strings.Split(commandStr, " ")
	argc := len(args)

	logger.Debug(h.user, "Received command", commandStr, argc, args)

	if h.user.Name == config.ControlUser {
		h.handleControlCommand(argc, args)
		return
	}

	h.handleUserCommand(argc, args)
}

// Special (restricted) set of commands for anonymous ControlUser access.
func (h *ServerHandler) handleControlCommand(argc int, args []string) {
	switch args[0] {
	case "ping":
		h.send(h.hiddenMessages, "pong")
	case "debug":
		h.send(h.serverMessages, logger.Debug(h.user, "Receiving debug command", argc, args))
	default:
		logger.Warn(h.user, "Received unknown command", argc, args)
	}
}

// Commands for authed users.
func (h *ServerHandler) handleUserCommand(argc int, args []string) {
	switch args[0] {
	case "grep":
		fallthrough
	case "cat":
		h.handleReadCommand(argc, args, omode.CatClient)
	case "tail":
		h.handleReadCommand(argc, args, omode.TailClient)
	case "map":
		h.handleMapCommand(argc, args)
	case "ack":
		h.handleAckCommand(argc, args)
	case "ping":
		h.send(h.hiddenMessages, "pong")
	case "version":
		h.send(h.serverMessages, fmt.Sprintf("Server version is "+version.String()))
	case "debug":
		h.send(h.serverMessages, logger.Debug(h.user, "Received debug command", argc, args))
	default:
		h.send(h.serverMessages, logger.Warn(h.user, "Received unknown command", argc, args))
	}
}

func (h *ServerHandler) handleReadCommand(argc int, args []string, mode omode.Mode) {
	regex := "."
	if argc >= 4 {
		regex = args[3]
	}
	if argc < 3 {
		h.send(h.serverMessages, logger.Warn(h.user, commandParseWarning, args, argc))
		return
	}
	go h.processFileGlob(mode, args[1], regex)
}

func (h *ServerHandler) handleMapCommand(argc int, args []string) {
	if argc < 2 {
		h.send(h.serverMessages, logger.Warn(h.user, commandParseWarning, args, argc))
		return
	}

	queryStr := strings.Join(args[1:], " ")
	logger.Info(h.user, "Creating new mapr aggregator", queryStr)
	aggregate, err := server.NewAggregate(h.aggregatedMessages, queryStr)

	if err != nil {
		h.send(h.serverMessages, logger.Error(h.user, err))
		return
	}

	h.aggregate = aggregate
}

func (h *ServerHandler) handleAckCommand(argc int, args []string) {
	if argc < 3 {
		h.send(h.serverMessages, logger.Warn(h.user, commandParseWarning, args, argc))
		return
	}
	if args[1] == "close" && args[2] == "connection" {
		close(h.ackStopReceived)
	}
}

func (h *ServerHandler) send(ch chan<- string, message string) {
	select {
	case ch <- message:
	case <-h.stop:
	}
}

// Wait (block) until server handler is closed or a timeout has exceeded.
func (h *ServerHandler) Wait() <-chan struct{} {
	wait := make(chan struct{})

	go func() {
		select {
		case <-h.ackStopReceived:
			logger.Debug(h.user, "Closing wait channel due to ACK stop received")
			close(wait)
		case <-h.stopTimeout:
			logger.Debug(h.user, "Closing wait channel due to wait timeout")
			close(wait)
		case <-h.stop:
			logger.Debug(h.user, "Closing wait channel due to stop")
		}
	}()

	return wait
}
