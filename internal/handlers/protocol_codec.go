package handlers

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/protocol"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// mismatchedClient is the kind of client whose command frame is of another
// protocol version (protocolCodec.handleProtocolVersion). The session
// handler decides by it whether the session stays open after the mismatch
// error (commandDispatcher.rejectProtocolVersion, versionRejectedCb).
type mismatchedClient int

const (
	// noMismatch is the zero value: the frame is of this server's protocol
	// version (handleProtocolVersion returned no error).
	noMismatch mismatchedClient = iota
	// otherVersionClient is a client of any other version not listed
	// below, e.g. a future one or another 5.x minor version. Its session
	// ends without the close handshake.
	otherVersionClient
	// protocol4Client is a client of protocol major version 4. Every such
	// client gets the mismatch error, possibly after unreadable output (it
	// does not decode the protocol 5 framing of the capabilities frame,
	// which dserver sends before it knows the client's version).
	// ServerHandler keeps its session open until the client exits or
	// IdleSessionTimeoutS, to prevent a reconnect loop; HealthHandler ends
	// it at once, as dtailhealth does not reconnect and a health check must
	// fail at once. Observed with 4.1 builds of this fork from before
	// protocol 5 (3ea4784) against a protocol 5 dserver that ended the
	// session: their dtail reconnected about every 3 s, each time with a
	// full SSH handshake, until it was stopped (their dcat and dmap exited
	// with status 1); with the session kept open, dtail stays on one
	// connection. Those builds also print the capabilities frame as payload
	// and report "unsupported DTail server: missing capability
	// escaped-framing-v1" before the mismatch error; released 4.x clients
	// (e.g. v4.3.4) predate that check.
	protocol4Client
	// legacyClient is a client of protocol major version 3 or lower, of
	// an unparsable version, or one sending no protocol prefix at all
	// (v1.x clients frame commands as "%s;"). Such clients do not check
	// the close handshake, and those that retry (dtail, dmap in tail mode
	// without outfile) reconnect 2 s after their session ended. Every
	// handler keeps the session open.
	legacyClient
)

type protocolCodec struct {
	user   *user.User
	logger logging.Logger
}

func newProtocolCodec(user *user.User, logger logging.Logger) protocolCodec {
	return protocolCodec{user: user, logger: logging.OrNop(logger)}
}

func (c protocolCodec) log() logging.Logger {
	return logging.OrNop(c.logger)
}

// handleProtocolVersion strips the "protocol <version>" prefix of a command
// frame. For a frame of another protocol version (or none) it returns an
// error, together with:
//   - add, the separator the client expects after that error message: a
//     newline for clients of protocol major version 3 or lower (including
//     an unparsable version, whose major parses as 0), otherwise nothing.
//     v1.x clients send no protocol prefix at all and never got a newline,
//     so a frame without the prefix gets none either.
//   - client, the kind of client that sent the frame (mismatchedClient).
//     It is reported explicitly, as add can not tell a prefix-less v1.x
//     frame from a frame of a future version.
func (c protocolCodec) handleProtocolVersion(args []string) (
	newArgs []string, argc int, add string, client mismatchedClient, err error) {
	argc = len(args)

	if argc <= 2 || args[0] != "protocol" {
		// No version to compare: a v1.x client (no prefix) or a malformed
		// frame. Treat it as legacy, so that a retrying client is not made
		// to reconnect in a loop.
		return args, argc, "", legacyClient, errors.New("unable to determine protocol version")
	}

	if args[1] != protocol.ProtocolCompat {
		clientMajor, clientMinor := parseProtocolCompat(args[1])
		serverMajor, serverMinor := parseProtocolCompat(protocol.ProtocolCompat)
		client = classifyMismatchedClient(args[1])
		if clientMajor <= 3 {
			// Protocol version 3 or lower expect a newline as message separator
			// One day (after 2 major versions) this exception may be removed!
			add = "\n"
		}

		toUpdate := "client"
		if clientMajor > serverMajor || (clientMajor == serverMajor && clientMinor > serverMinor) {
			toUpdate = "server"
		}
		err = fmt.Errorf("the DTail server protocol version '%s' does not match "+
			"client protocol version '%s', please update DTail %s",
			protocol.ProtocolCompat, args[1], toUpdate)
		return args, argc, add, client, err
	}

	return args[2:], argc - 2, "", noMismatch, nil
}

// classifyMismatchedClient returns the kind of client sending protocol
// version version, which is not this server's. A version whose major can not
// be parsed at all is a legacyClient: nothing identifies such a client as a
// newer one that would handle the session ending, so it is kept open.
func classifyMismatchedClient(version string) mismatchedClient {
	majorPart, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorPart)
	switch {
	case err != nil || major <= 3:
		return legacyClient
	case major == 4:
		return protocol4Client
	default:
		return otherVersionClient
	}
}

func parseProtocolCompat(version string) (int, int) {
	major := 0
	minor := 0

	parts := strings.SplitN(version, ".", 2)
	if len(parts) > 0 {
		major, _ = strconv.Atoi(parts[0])
	}
	if len(parts) == 2 {
		minor, _ = strconv.Atoi(parts[1])
	}

	return major, minor
}

func (c protocolCodec) handleBase64(args []string, argc int) ([]string, int, error) {
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
	argc = len(args)
	c.log().Trace(c.user, "Base64 decoded received command",
		redactReadShare(decodedStr), argc, argsForLog(args))

	return args, argc, nil
}
