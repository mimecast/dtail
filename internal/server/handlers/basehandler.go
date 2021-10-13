package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/mapr/server"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/user/server"
)

type handleCommandCb func(context.Context, int, []string, string)

type baseHandler struct {
	done             *internal.Done
	handleCommandCb  handleCommandCb
	lines            chan line.Line
	aggregate        *server.Aggregate
	maprMessages     chan string
	serverMessages   chan string
	hostname         string
	user             *user.User
	ackCloseReceived chan struct{}
	activeCommands   int32
	readBuf          bytes.Buffer
	writeBuf         bytes.Buffer

	// Some global options + sync primitives required.
	once       sync.Once
	mutex      sync.Mutex
	quiet      bool
	spartan    bool
	serverless bool
}

// Shutdown the handler.
func (h *baseHandler) Shutdown() {
	h.done.Shutdown()
}

// Done channel of the handler.
func (h *baseHandler) Done() <-chan struct{} {
	return h.done.Done()
}

// Read is to send data to the dtail client via Reader interface.
func (h *baseHandler) Read(p []byte) (n int, err error) {
	defer h.readBuf.Reset()

	select {
	case message := <-h.serverMessages:
		if len(message) > 0 && message[0] == '.' {
			// Handle hidden message (don't display to the user)
			h.readBuf.WriteString(message)
			h.readBuf.WriteByte(protocol.MessageDelimiter)
			n = copy(p, h.readBuf.Bytes())
			return
		}

		if h.serverless {
			return
		}

		// Handle normal server message (display to the user)
		h.readBuf.WriteString("SERVER")
		h.readBuf.WriteString(protocol.FieldDelimiter)
		h.readBuf.WriteString(h.hostname)
		h.readBuf.WriteString(protocol.FieldDelimiter)
		h.readBuf.WriteString(message)
		h.readBuf.WriteByte(protocol.MessageDelimiter)
		n = copy(p, h.readBuf.Bytes())

	case message := <-h.maprMessages:
		// Send mapreduce-aggregated data as a message.
		h.readBuf.WriteString("AGGREGATE")
		h.readBuf.WriteString(protocol.FieldDelimiter)
		h.readBuf.WriteString(h.hostname)
		h.readBuf.WriteString(protocol.FieldDelimiter)
		h.readBuf.WriteString(message)
		h.readBuf.WriteByte(protocol.MessageDelimiter)
		n = copy(p, h.readBuf.Bytes())

	case line := <-h.lines:
		if !h.spartan {
			h.readBuf.WriteString("REMOTE")
			h.readBuf.WriteString(protocol.FieldDelimiter)
			h.readBuf.WriteString(h.hostname)
			h.readBuf.WriteString(protocol.FieldDelimiter)
			h.readBuf.WriteString(fmt.Sprintf("%3d", line.TransmittedPerc))
			h.readBuf.WriteString(protocol.FieldDelimiter)
			h.readBuf.WriteString(fmt.Sprintf("%v", line.Count))
			h.readBuf.WriteString(protocol.FieldDelimiter)
			h.readBuf.WriteString(line.SourceID)
			h.readBuf.WriteString(protocol.FieldDelimiter)
		}
		h.readBuf.WriteString(line.Content.String())
		h.readBuf.WriteByte(protocol.MessageDelimiter)
		n = copy(p, h.readBuf.Bytes())
		pool.RecycleBytesBuffer(line.Content)

	case <-time.After(time.Second):
		// Once in a while check whether we are done.
		select {
		case <-h.done.Done():
			err = io.EOF
			return
		default:
		}
	}
	return
}

// Write is to receive data from the dtail client via Writer interface.
func (h *baseHandler) Write(p []byte) (n int, err error) {
	for _, b := range p {
		switch b {
		case ';':
			h.handleCommand(string(h.writeBuf.Bytes()))
			h.writeBuf.Reset()
		default:
			h.writeBuf.WriteByte(b)
		}
	}
	n = len(p)
	return
}

func (h *baseHandler) handleCommand(commandStr string) {
	dlog.Server.Debug(h.user, commandStr)

	args, argc, add, err := h.handleProtocolVersion(strings.Split(commandStr, " "))
	if err != nil {
		h.send(h.serverMessages, dlog.Server.Error(h.user, err)+add)
		return
	}
	args, argc, err = h.handleBase64(args, argc)
	if err != nil {
		h.send(h.serverMessages, dlog.Server.Error(h.user, err))
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-h.done.Done()
		cancel()
	}()

	splitted := strings.Split(args[0], ":")
	commandName := splitted[0]
	options, err := config.DeserializeOptions(splitted[1:])
	if err != nil {
		h.send(h.serverMessages, dlog.Server.Error(h.user, err))
		return
	}
	h.setOptions(options)

	h.handleCommandCb(ctx, argc, args, commandName)
}

func (h *baseHandler) handleProtocolVersion(args []string) ([]string, int, string, error) {
	argc := len(args)
	var add string

	if argc <= 2 || args[0] != "protocol" {
		return args, argc, add, errors.New("unable to determine protocol version")
	}

	if args[1] != protocol.ProtocolCompat {
		clientCompat, _ := strconv.Atoi(args[1])
		serverCompat, _ := strconv.Atoi(protocol.ProtocolCompat)
		if clientCompat <= 3 {
			// Protocol version 3 or lower expect a newline as message separator
			// One day (after 2 major versions) this exception may be removed!
			add = "\n"
		}

		toUpdate := "client"
		if clientCompat > serverCompat {
			toUpdate = "server"
		}
		err := fmt.Errorf("the DTail server protocol version '%s' does not match "+
			"client protocol version '%s', please update DTail %s",
			protocol.ProtocolCompat, args[1], toUpdate)
		return args, argc, add, err
	}

	return args[2:], argc - 2, add, nil
}

func (h *baseHandler) handleBase64(args []string, argc int) ([]string, int, error) {
	err := errors.New("unable to decode client message, DTail server and client " +
		"versions may not be compatible")
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
	dlog.Server.Trace(h.user, "Base64 decoded received command",
		decodedStr, argc, args)

	return args, argc, nil
}

func (h *baseHandler) handleAckCommand(argc int, args []string) {
	if argc < 3 {
		if !h.quiet {
			h.send(h.serverMessages, dlog.Server.Warn(h.user,
				"Unable to parse command", args, argc))
		}
		return
	}
	if args[1] == "close" && args[2] == "connection" {
		select {
		case <-h.ackCloseReceived:
		default:
			close(h.ackCloseReceived)
		}
	}
}

func (h *baseHandler) setOptions(options map[string]string) {
	// We have to make sure that this block is executed only once.
	h.mutex.Lock()
	defer h.mutex.Unlock()
	// We can read the options only once, will cause a data race otherwise if
	// changed multiple times for multiple incoming commands.
	h.once.Do(func() {
		if quiet, _ := options["quiet"]; quiet == "true" {
			dlog.Server.Debug(h.user, "Enabling quiet mode")
			h.quiet = true
		}
		if spartan, _ := options["spartan"]; spartan == "true" {
			dlog.Server.Debug(h.user, "Enabling spartan mode")
			h.spartan = true
		}
		if serverless, _ := options["serverless"]; serverless == "true" {
			dlog.Server.Debug(h.user, "Enabling serverless mode")
			h.serverless = true
		}
	})
}

func (h *baseHandler) send(ch chan<- string, message string) {
	select {
	case ch <- message:
	case <-h.done.Done():
	}
}

func (h *baseHandler) flush() {
	dlog.Server.Trace(h.user, "flush()")
	numUnsentMessages := func() int {
		return len(h.lines) + len(h.serverMessages) + len(h.maprMessages)
	}
	for i := 0; i < 10; i++ {
		if numUnsentMessages() == 0 {
			dlog.Server.Debug(h.user, "ALL lines sent", fmt.Sprintf("%p", h))
			return
		}
		dlog.Server.Debug(h.user, "Still lines to be sent")
		time.Sleep(time.Millisecond * 10)
	}
	dlog.Server.Warn(h.user, "Some lines remain unsent", numUnsentMessages())
}

func (h *baseHandler) shutdown() {
	dlog.Server.Debug(h.user, "shutdown()")
	h.flush()

	go func() {
		select {
		case h.serverMessages <- ".syn close connection":
		case <-h.done.Done():
		}
	}()

	select {
	case <-h.ackCloseReceived:
	case <-time.After(time.Second * 5):
		dlog.Server.Debug(h.user, "Shutdown timeout reached, enforcing shutdown")
	case <-h.done.Done():
	}
	h.done.Shutdown()
}

func (h *baseHandler) incrementActiveCommands() {
	atomic.AddInt32(&h.activeCommands, 1)
}

func (h *baseHandler) decrementActiveCommands() int32 {
	atomic.AddInt32(&h.activeCommands, -1)
	return atomic.LoadInt32(&h.activeCommands)
}
