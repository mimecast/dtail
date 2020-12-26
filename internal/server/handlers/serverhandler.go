package handlers

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/logger"
	"github.com/mimecast/dtail/internal/mapr/server"
	"github.com/mimecast/dtail/internal/omode"
	user "github.com/mimecast/dtail/internal/user/server"
	"github.com/mimecast/dtail/internal/version"
)

const (
	commandParseWarning string = "Unable to parse command"
)

// ServerHandler implements the Reader and Writer interfaces to handle
// the Bi-directional communication between SSH client and server.
// This handler implements the handler of the SSH server.
type ServerHandler struct {
	done               *internal.Done
	lines              chan line.Line
	regex              string
	aggregate          *server.Aggregate
	aggregatedMessages chan string
	serverMessages     chan string
	payload            []byte
	hostname           string
	user               *user.User
	catLimiter         chan struct{}
	tailLimiter        chan struct{}
	ackCloseReceived   chan struct{}
	activeCommands     int32
	activeReaders      int32
	quiet            bool
}

// NewServerHandler returns the server handler.
func NewServerHandler(user *user.User, catLimiter, tailLimiter chan struct{}) *ServerHandler {
	h := ServerHandler{
		done:               internal.NewDone(),
		lines:              make(chan line.Line, 100),
		serverMessages:     make(chan string, 10),
		aggregatedMessages: make(chan string, 10),
		ackCloseReceived:   make(chan struct{}),
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

// Shutdown the handler.
func (h *ServerHandler) Shutdown() {
	h.done.Shutdown()
}

// Done channel of the handler.
func (h *ServerHandler) Done() <-chan struct{} {
	return h.done.Done()
}

// Read is to send data to the dtail client via Reader interface.
func (h *ServerHandler) Read(p []byte) (n int, err error) {
	for {
		select {
		case message := <-h.serverMessages:
			if len(message) == 0 {
				logger.Warn(h.user, "Empty message recieved")
				return
			}
			if message[0] == '.' {
				// Handle hidden message (don't display to the user, interpreted by dtail client)
				wholePayload := []byte(fmt.Sprintf("%s\n", message))
				n = copy(p, wholePayload)
				return
			}

			// Handle normal server message (display to the user)
			wholePayload := []byte(fmt.Sprintf("SERVER|%s|%s\n", h.hostname, message))
			n = copy(p, wholePayload)
			return

		case message := <-h.aggregatedMessages:
			// Send mapreduce-aggregated data as a message.
			data := fmt.Sprintf("AGGREGATE➔%s➔%s\n", h.hostname, message)
			wholePayload := []byte(data)
			n = copy(p, wholePayload)
			return

		case line := <-h.lines:
			// Send normal file content data as a message.
			serverInfo := []byte(fmt.Sprintf("REMOTE|%s|%3d|%v|%s|",
				h.hostname, line.TransmittedPerc, line.Count, line.SourceID))
			wholePayload := append(serverInfo, line.Content[:]...)
			n = copy(p, wholePayload)
			return

		case <-time.After(time.Second):
			// Once in a while check whether we are done.
			select {
			case <-h.done.Done():
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

func (h *ServerHandler) handleCommand(commandStr string) {
	logger.Debug(h.user, commandStr)
	ctx := context.Background()

	args, argc, err := h.handleProtocolVersion(strings.Split(commandStr, " "))
	if err != nil {
		h.send(h.serverMessages, logger.Error(h.user, err))
		return
	}

	args, argc, err = h.handleBase64(args, argc)
	if err != nil {
		h.send(h.serverMessages, logger.Error(h.user, err))
		return
	}

	if h.user.Name == config.ControlUser {
		h.handleControlCommand(argc, args)
		return
	}

	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-h.done.Done()
		cancel()
	}()

	h.handleUserCommand(ctx, argc, args)
}

func (h *ServerHandler) handleProtocolVersion(args []string) ([]string, int, error) {
	argc := len(args)

	if argc <= 2 || args[0] != "protocol" {
		return args, argc, errors.New("unable to determine protocol version")
	}

	if args[1] != version.ProtocolCompat {
		err := fmt.Errorf("server with protocol version '%s' but client with '%s', please update DTail", version.ProtocolCompat, args[1])
		return args, argc, err
	}

	return args[2:], argc - 2, nil
}

func (h *ServerHandler) handleBase64(args []string, argc int) ([]string, int, error) {
	err := errors.New("Unable to decode client message, DTail server and client versions may not be compatible")

	if argc != 2 || args[0] != "base64" {
		return args, argc, err
	}

	decoded, err := base64.StdEncoding.DecodeString(args[1])
	if err != nil {
		return args, argc, err
	}
	decodedStr := string(decoded)

	args = strings.Split(decodedStr, " ")
	argc = len(decodedStr)
	logger.Trace(h.user, "Base64 decoded received command", decodedStr, argc, args)

	return args, argc, nil
}

func (h *ServerHandler) handleControlCommand(argc int, args []string) {
	switch args[0] {
	case "debug":
		h.send(h.serverMessages, logger.Debug(h.user, "Receiving debug command", argc, args))
	default:
		logger.Warn(h.user, "Received unknown control command", argc, args)
	}
}

func (h *ServerHandler) handleUserCommand(ctx context.Context, argc int, args []string) {
	logger.Debug(h.user, "handleUserCommand", argc, args)

	h.incrementActiveCommands()
	commandFinished := func() {
		if h.decrementActiveCommands() == 0 {
			h.shutdown()
		}
	}
	readerFinished := func() {
		if h.decrementActiveReaders() == 0 {
			if h.aggregate == nil {
				return
			}
			h.aggregate.Shutdown()
		}
	}

	splitted := strings.Split(args[0], ":")
	commandName := splitted[0]

	options, err := readOptions(splitted[1:])
	if err != nil {
		h.sendServerMessage(logger.Error(h.user, err))
		commandFinished()
		return
	}
	if quiet, ok := options["quiet"]; ok {
		if quiet == "true" {
			logger.Debug(h.user, "Enabling quiet mode")
			h.quiet = true
		}
	}

	switch commandName {
	case "grep", "cat":
		command := newReadCommand(h, omode.CatClient)
		go func() {
			h.incrementActiveReaders()
			command.Start(ctx, argc, args)
			readerFinished()
			commandFinished()
		}()

	case "tail":
		command := newReadCommand(h, omode.TailClient)
		go func() {
			h.incrementActiveReaders()
			command.Start(ctx, argc, args)
			readerFinished()
			commandFinished()
		}()

	case "map":
		command, aggregate, err := newMapCommand(h, argc, args)
		if err != nil {
			h.sendServerMessage(err.Error())
			logger.Error(h.user, err)
			commandFinished()
			return
		}

		h.aggregate = aggregate
		go func() {
			command.Start(ctx, h.aggregatedMessages)
			commandFinished()
		}()

	case "ack", ".ack":
		h.handleAckCommand(argc, args)
		commandFinished()

	default:
		h.sendServerMessage(logger.Error(h.user, "Received unknown user command", commandName, argc, args, options))
		commandFinished()
	}
}

func (h *ServerHandler) handleAckCommand(argc int, args []string) {
	if argc < 3 {
		h.sendServerMessage(logger.Warn(h.user, commandParseWarning, args, argc))
		return
	}
	if args[1] == "close" && args[2] == "connection" {
		close(h.ackCloseReceived)
	}
}

func (h *ServerHandler) send(ch chan<- string, message string) {
	select {
	case ch <- message:
	case <-h.done.Done():
	}
}

func (h *ServerHandler) sendServerMessage(message string) {
	h.send(h.serverMessageC(), message)
}

func (h *ServerHandler) serverMessageC() chan<- string {
	return h.serverMessages
}

func (h *ServerHandler) flush() {
	logger.Debug(h.user, "flush()")

	if h.aggregate != nil {
		h.aggregate.Flush()
	}

	unsentMessages := func() int {
		return len(h.lines) + len(h.serverMessages) + len(h.aggregatedMessages)
	}
	for i := 0; i < 3; i++ {
		if unsentMessages() == 0 {
			logger.Debug(h.user, "All lines sent")
			return
		}
		logger.Debug(h.user, "Still lines to be sent")
		time.Sleep(time.Second)
	}

	logger.Warn(h.user, "Some lines remain unsent", unsentMessages())
}

func (h *ServerHandler) shutdown() {
	logger.Debug(h.user, "shutdown()")
	h.flush()

	go func() {
		select {
		case h.serverMessageC() <- ".syn close connection":
		case <-h.done.Done():
		}
	}()

	select {
	case <-h.ackCloseReceived:
	case <-time.After(time.Second * 5):
		logger.Debug(h.user, "Shutdown timeout reached, enforcing shutdown")
	case <-h.done.Done():
	}

	h.done.Shutdown()
}

func (h *ServerHandler) incrementActiveCommands() {
	atomic.AddInt32(&h.activeCommands, 1)
}

func (h *ServerHandler) decrementActiveCommands() int32 {
	atomic.AddInt32(&h.activeCommands, -1)
	return atomic.LoadInt32(&h.activeCommands)
}

func (h *ServerHandler) incrementActiveReaders() {
	atomic.AddInt32(&h.activeReaders, 1)
}

func (h *ServerHandler) decrementActiveReaders() int32 {
	atomic.AddInt32(&h.activeReaders, -1)
	return atomic.LoadInt32(&h.activeReaders)
}

func readOptions(opts []string) (map[string]string, error) {
	options := make(map[string]string, len(opts))

	for _, o := range opts {
		kv := strings.SplitN(o, "=", 2)
		if len(kv) != 2 {
			return options, fmt.Errorf("Unable to parse options: %v", kv)
		}
		key := kv[0]
		val := kv[1]

		if strings.HasPrefix(val, "base64%") {
			s := strings.SplitN(val, "%", 2)
			decoded, err := base64.StdEncoding.DecodeString(s[1])
			if err != nil {
				return options, err
			}
			val = string(decoded)
		}

		options[key] = val
	}

	return options, nil
}
