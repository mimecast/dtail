package protocol

const (
	// HiddenSessionStartOKPrefix acknowledges a successful SESSION START request.
	HiddenSessionStartOKPrefix = ".syn session start ok"
	// HiddenSessionUpdateOKPrefix acknowledges a successful SESSION UPDATE request.
	HiddenSessionUpdateOKPrefix = ".syn session update ok"
	// HiddenSessionErrorPrefix reports a rejected SESSION request.
	HiddenSessionErrorPrefix = ".syn session err "
)

// HiddenCommandFailedPrefix reports that a command of the session failed on
// the server, for example a read whose file(s) did not exist or could not be
// opened, so that the session's output is incomplete. It is followed by a
// short, fixed reason naming no path or server-side error detail (those stay
// in the server log). Servers advertising CapabilityCommandFailureV1 send it
// once per failure, before the session's close handshake; clients that do
// not know it ignore it like any other unknown hidden message. dcat and dgrep
// exit with status 2 for any reported reason; scheduled dmap jobs tell file
// read failures (IsFileReadFailure) from the others.
const HiddenCommandFailedPrefix = ".syn command failed "

// Reasons a server sends after HiddenCommandFailedPrefix.
const (
	// CommandFailureDecode: a command could not be decoded.
	CommandFailureDecode = "command: unable to decode command"
	// CommandFailureRejected: a command was rejected, e.g. for invalid options.
	CommandFailureRejected = "command: rejected"
	// CommandFailureUnknown: a command is unknown.
	CommandFailureUnknown = "command: unknown command"
	// CommandFailureMapQuery: a map query is invalid. It is also reported
	// for each read command following the failed map command of a command
	// stream, as dserver does not run those reads.
	CommandFailureMapQuery = "map: invalid query"
	// CommandFailureMapIncomplete: a map command's final result did not
	// reach the client in full before the session ended normally, e.g. its
	// final serialization was canceled part way through its groups. It is no
	// file read failure: what the client got is missing groups, so a
	// scheduled job writes no outfile.
	CommandFailureMapIncomplete = "map: incomplete result"
	// CommandFailureReadCommand: a read command could not be parsed.
	CommandFailureReadCommand = "read: unable to parse command"
	// CommandFailureOutputTooLarge: a read produced output (a batch of line
	// frames) larger than the session's output buffer cap, so that output
	// was dropped. It is no file read failure: the file was read, but the
	// command could not deliver what it read.
	CommandFailureOutputTooLarge = "output: too large for the session output buffer"

	// The file read failures (see IsFileReadFailure).

	// CommandFailureNoFile: a read found no file (to read) after its glob
	// retries, a file vanished before it was read, or every file the glob
	// matched is not a regular file.
	CommandFailureNoFile = "read: no file to read"
	// CommandFailureGlobCapped: a glob matched more files than the server's
	// MaxGlobTargets, so only part of them was read.
	CommandFailureGlobCapped = "read: more files than the server reads"
	// CommandFailurePermission: the read permissions deny a file.
	CommandFailurePermission = "read: no permission to read file"
	// CommandFailureReader: the reader of a file or journal can not be created.
	CommandFailureReader = "read: unable to create file reader"
	// CommandFailureReadingFile: a file could not be opened or read.
	CommandFailureReadingFile = "read: unable to read file"
)

// IsFileReadFailure reports whether reason is one of the failures of a read
// whose files are missing, not permitted or unreadable: the rest of the
// session's output is still correct, only that data is missing. The other
// reasons (and unknown ones) mean that a command did not run as asked.
func IsFileReadFailure(reason string) bool {
	switch reason {
	case CommandFailureNoFile, CommandFailureGlobCapped, CommandFailurePermission,
		CommandFailureReader, CommandFailureReadingFile:
		return true
	default:
		return false
	}
}
