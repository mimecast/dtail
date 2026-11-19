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
//   - legacy, whether the frame is of a legacy client whose session must
//     stay open (commandDispatcher.rejectProtocolVersion): protocol major
//     version 3 or lower, an unparsable version, or no protocol prefix at
//     all (v1.x clients frame commands as "%s;"). legacy is reported
//     explicitly, as add can not tell a prefix-less v1.x frame from a
//     frame of a future version.
func (c protocolCodec) handleProtocolVersion(args []string) (
	newArgs []string, argc int, add string, legacy bool, err error) {
	argc = len(args)

	if argc <= 2 || args[0] != "protocol" {
		// No version to compare: a v1.x client (no prefix) or a malformed
		// frame. Treat it as legacy, so that a retrying client is not made
		// to reconnect in a loop.
		return args, argc, "", true, errors.New("unable to determine protocol version")
	}

	if args[1] != protocol.ProtocolCompat {
		clientMajor, clientMinor := parseProtocolCompat(args[1])
		serverMajor, serverMinor := parseProtocolCompat(protocol.ProtocolCompat)
		legacy = isLegacyProtocolVersion(args[1])
		if isLegacyProtocolMajor(clientMajor) {
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
		return args, argc, add, legacy, err
	}

	return args[2:], argc - 2, "", false, nil
}

// isLegacyProtocolMajor reports whether a client of protocol major version
// major is one of the older clients (protocol 3 or lower): they expect a
// newline after each message, do not check for the close handshake, and
// some of them (dtail, dmap in tail mode without outfile) reconnect 2 s
// after their session ended.
func isLegacyProtocolMajor(major int) bool {
	return major <= 3
}

// isLegacyProtocolVersion reports whether a client sending protocol version
// version is a legacy client (isLegacyProtocolMajor), also when its major
// version can not be parsed at all: nothing identifies such a client as a
// newer one that would handle the session ending, so it is kept open.
func isLegacyProtocolVersion(version string) bool {
	majorPart, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(majorPart)
	return err != nil || isLegacyProtocolMajor(major)
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
