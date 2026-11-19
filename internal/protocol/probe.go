package protocol

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The probe command asks a server which MapReduce tables, fields and
// $variables the lines of some of its files carry, for the interactive query
// completion, or (a request with List set) which files and directories
// beneath a path prefix the session user may read, for the path completion.
// The client sends the command frame
//
//	probe <base64 of the ProbeRequest JSON>
//
// and the server answers, once per request, with the hidden control frame
//
//	.syn probe <id> <base64 of the ProbeAnswer JSON>
//
// A probe runs next to the session's workload without replacing it, never
// ends the session and never reports a failed command
// (HiddenCommandFailedPrefix): every failure is an entry of
// ProbeAnswer.Errors. See docs/dev/probe.md.
const (
	// ProbeCommand is the name of the probe command.
	ProbeCommand = "probe"
	// HiddenProbePrefix starts the hidden control frame of a probe answer.
	HiddenProbePrefix = ".syn probe "
	// ProbeMaxFiles is the number of files a probe samples at most, over all
	// entries of ProbeRequest.Files and the files their globs match.
	ProbeMaxFiles = 8
	// ProbeMaxAnswerBytes bounds the JSON of a ProbeAnswer (before base64).
	// A server drops sample values, then fields, to stay within it and then
	// sets ProbeAnswer.Truncated.
	ProbeMaxAnswerBytes = 48 * 1024
	// ProbeMaxIDBytes is the length of a probe id at most.
	ProbeMaxIDBytes = 64
	// ProbeUnknownID is the id of the answer to a request whose id is
	// unknown, as the request did not decode or its id is invalid.
	ProbeUnknownID = "-"
	// ProbeListMaxEntries is the number of entries a path listing answers at
	// most (fewer when their names exceed ProbeMaxAnswerBytes); the answer
	// of a listing with more sets ProbeAnswer.Truncated.
	ProbeListMaxEntries = 500
	// ProbeListMaxPrefixBytes is the length of ProbeRequest.List at most.
	ProbeListMaxPrefixBytes = 4096
	// ProbeMaxTableBytes is the length of ProbeRequest.Table at most, the
	// length a probe cuts table names to (logformat.ProbeMaxNameBytes).
	ProbeMaxTableBytes = 64
)

// Fixed reasons of a ProbeError. A reason names no server-side path or
// error detail (those stay in the server log), like the reasons of a failed
// command; the read failures reuse those (CommandFailureNoFile etc.).
const (
	// ProbeErrorInvalidRequest: the request did not decode, its id is
	// invalid, it names no file, it is both a field probe and a path
	// listing (Files and List), its List prefix is invalid (too long, or
	// a NUL byte), it is a path listing with a Table, or its Table is
	// invalid (ValidProbeTable).
	ProbeErrorInvalidRequest = "probe: invalid request"
	// ProbeErrorBusy: another probe of the session is still running.
	ProbeErrorBusy = "probe: another probe is running"
	// ProbeErrorTooManyFiles: the entries and globs of the request name more
	// than ProbeMaxFiles files; only the first ones were sampled.
	ProbeErrorTooManyFiles = "probe: more files than a probe samples"
	// ProbeErrorJournalUnsupported: a journal target was asked for, but the
	// server lacks CapabilityJournalV1.
	ProbeErrorJournalUnsupported = "probe: journal targets need capability " + CapabilityJournalV1
	// ProbeErrorTimeout: the probe ran out of time (or the session ended)
	// before it sampled every file; the files sampled until then are
	// answered.
	ProbeErrorTimeout = "probe: timed out"
	// ProbeErrorFormat: a log format's probe failed (a custom Prober
	// returned an error or the parser panicked); File names the format.
	ProbeErrorFormat = "probe: log format probe failed"
	// ProbeErrorListDisabled: a path listing was asked for, but the server
	// sets Server.PathCompletionDisable.
	ProbeErrorListDisabled = "probe: path listing disabled"
	// ProbeErrorListFailed: the journal units could not be listed
	// (journalctl failed). A directory that can not be listed is answered
	// as an empty listing instead, like one the user may not see.
	ProbeErrorListFailed = "probe: unable to list"
)

// ErrInvalidProbe reports a probe request or answer that does not decode.
var ErrInvalidProbe = errors.New("invalid probe")

// ProbeRequest is the JSON a probe command carries.
type ProbeRequest struct {
	// ID is echoed in the answer, so the client can match answers to its
	// requests. It is 1 to ProbeMaxIDBytes of ASCII letters, digits, '.',
	// '_' and '-', but not ProbeUnknownID (ValidProbeID).
	ID string `json:"id"`
	// Files are the read targets to sample, each a path, a glob or a
	// journal target ("journal:unit"), written as the path word of a read
	// command: a path containing a space, or starting with
	// EncodedReadPathPrefix, is encoded with EncodeReadPath. The server
	// decodes it with DecodeReadPath and then resolves and checks it like a
	// read command's path.
	Files []string `json:"files"`
	// List, when not empty, makes the request a path listing instead of a
	// field probe (Files must then be empty): the server answers the
	// entries of the directory of the prefix List (its part up to and
	// including the last '/', the server's working directory if it has
	// none) whose names start with the rest of List and that the session
	// user may read (ProbeAnswer.Dir, Entries). A List starting with
	// "journal:" lists the systemd units instead (needs
	// CapabilityJournalV1). It is a plain path, not encoded like a Files
	// entry, of at most ProbeListMaxPrefixBytes. To list the working
	// directory, send "./".
	List string `json:"list,omitempty"`
	// Table, when not empty, makes a field probe table-targeted: the server
	// samples, from a larger window of each file than a plain probe reads,
	// only the lines of the table (those a log format's ProbeTabler assigns
	// to it, or that name it as a word of their own) and answers
	// only that table of every log format (ProbeAnswer.Table set): its
	// fields and the $variables its lines filled (TableVariables), without
	// the format-wide Variables. So a table that logs rarely, and is thin or
	// missing in a plain probe, gets fields and samples. It is compared
	// exactly, as a query's from clause names it (upper case), and must be
	// valid (ValidProbeTable). Servers before this field ignore it and
	// answer a plain probe, which has no ProbeAnswer.Table.
	Table string `json:"table,omitempty"`
}

// ProbeAnswer is the JSON of a probe answer frame.
type ProbeAnswer struct {
	// ID is the ProbeRequest.ID answered, or ProbeUnknownID.
	ID string `json:"id"`
	// DefaultLogFormat is the server's MapreduceLogFormat, the log format of
	// a query without a logformat clause.
	DefaultLogFormat string `json:"defaultLogFormat,omitempty"`
	// Table echoes ProbeRequest.Table of a table-targeted probe: Formats
	// then hold that table only (and no format-wide Variables).
	Table string `json:"table,omitempty"`
	// Formats maps every log format that found fields in the sample to them.
	Formats map[string]ProbeFormat `json:"formats,omitempty"`
	// FilesSampled and LinesSampled count the files sampled and their
	// sampled lines (at most 500 per file); a file's first line the server
	// puts in front of a tail sample for header formats is not counted.
	FilesSampled int `json:"filesSampled"`
	LinesSampled int `json:"linesSampled"`
	// Dir and Entries answer a path listing (ProbeRequest.List): Dir is the
	// directory part of the prefix as the client sent it ("" for the
	// working directory, "journal:" for journal units), Entries the names
	// in it, sorted, without Dir. A listing with no entry (also of a
	// directory that does not exist or the user may not see) has none.
	Dir     string       `json:"dir,omitempty"`
	Entries []ProbeEntry `json:"entries,omitempty"`
	// Truncated is set when the answer was cut to ProbeMaxAnswerBytes or,
	// for a path listing, when the server stopped before it checked every
	// matching name (ProbeListMaxEntries, the server's bounds on the
	// directory scan and on the permission checks, or the probe timeout),
	// so more entries may exist.
	Truncated bool `json:"truncated,omitempty"`
	// Errors lists what went wrong; a probe that failed altogether has
	// errors only.
	Errors []ProbeError `json:"errors,omitempty"`
}

// ProbeFormat is what a log format found in the sample: tables (the
// MAPREDUCE:<TABLE> of the default format, "" for formats without tables)
// mapping bareword fields to sample values, per table the $variables its
// lines filled (TableVariables: with an entry for every table of Tables,
// empty for a table none of whose sampled lines filled one, e.g. a table
// only found by scanning the window; never the format's known variables
// without values; nil only for a format whose custom probe gives no
// per-table data, whose tables then have the format-wide Variables), the
// format-wide $variables with sample values, and an optional note of a
// custom probe. It mirrors logformat.ProbeResult.
type ProbeFormat struct {
	Tables         map[string]map[string][]string `json:"tables,omitempty"`
	TableVariables map[string]map[string][]string `json:"tableVariables,omitempty"`
	Variables      map[string][]string            `json:"variables,omitempty"`
	Note           string                         `json:"note,omitempty"`
}

// ProbeEntry is one entry of a path listing: a file (or journal unit) the
// session user may read, or a directory (Dir set) whose paths the user's
// rules do not rule out (it may still hold nothing readable, see
// docs/dev/probe.md). Name is the base name, without the listing's Dir.
type ProbeEntry struct {
	Name string `json:"name"`
	Dir  bool   `json:"dir,omitempty"`
}

// ProbeError is one failure of a probe: a fixed reason (a ProbeError* or a
// CommandFailure* read reason) and the request entry (or log format) it
// concerns, if any.
type ProbeError struct {
	File  string `json:"file,omitempty"`
	Error string `json:"error"`
}

// EncodeProbeCommand returns the probe command (without the protocol and
// base64 framing every command frame gets) for request.
func EncodeProbeCommand(request ProbeRequest) (string, error) {
	data, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode probe request: %w", err)
	}
	return ProbeCommand + " " + base64.StdEncoding.EncodeToString(data), nil
}

// DecodeProbeRequest decodes the argument of a probe command. It fails for
// invalid base64 or JSON and for an invalid id; the request may still name
// no file.
func DecodeProbeRequest(argument string) (ProbeRequest, error) {
	var request ProbeRequest
	if err := decodeProbeJSON(argument, &request); err != nil {
		return ProbeRequest{}, err
	}
	if !ValidProbeID(request.ID) {
		return ProbeRequest{}, fmt.Errorf("%w: invalid id", ErrInvalidProbe)
	}
	return request, nil
}

// EncodeProbeAnswer returns the hidden control frame body of the answer
// with the given id, data being the JSON of its ProbeAnswer.
func EncodeProbeAnswer(id string, data []byte) string {
	return HiddenProbePrefix + id + " " + base64.StdEncoding.EncodeToString(data)
}

// DecodeProbeAnswer decodes the hidden control frame body of a probe answer
// (starting with HiddenProbePrefix).
func DecodeProbeAnswer(message string) (ProbeAnswer, error) {
	rest, ok := strings.CutPrefix(strings.TrimRight(message, "\n"), HiddenProbePrefix)
	if !ok {
		return ProbeAnswer{}, fmt.Errorf("%w: missing %q prefix", ErrInvalidProbe, HiddenProbePrefix)
	}
	id, encoded, ok := strings.Cut(rest, " ")
	if !ok {
		return ProbeAnswer{}, fmt.Errorf("%w: missing answer", ErrInvalidProbe)
	}
	var answer ProbeAnswer
	if err := decodeProbeJSON(encoded, &answer); err != nil {
		return ProbeAnswer{}, err
	}
	if answer.ID != id {
		return ProbeAnswer{}, fmt.Errorf("%w: id %q of the frame differs from the answer's %q",
			ErrInvalidProbe, id, answer.ID)
	}
	return answer, nil
}

// ValidProbeID reports whether id may name a probe: 1 to ProbeMaxIDBytes
// ASCII letters, digits, '.', '_' or '-', but not ProbeUnknownID. It never
// contains a blank, so it is one word of the answer frame.
func ValidProbeID(id string) bool {
	if id == "" || id == ProbeUnknownID || len(id) > ProbeMaxIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// probeTableSeparators are characters no table name holds: the field
// separator of DTail log lines ('|', which ends the MAPREDUCE:<TABLE>
// token), the comma a query treats as a blank, and the quotes and
// backtick a query quotes words with.
const probeTableSeparators = "|,\"'`"

// ValidProbeTable reports whether table may be the Table of a probe request:
// 1 to ProbeMaxTableBytes bytes of valid UTF-8 without blanks, control or
// format characters and without probeTableSeparators (a table name is one
// word of a query and one field of a log line).
func ValidProbeTable(table string) bool {
	if table == "" || len(table) > ProbeMaxTableBytes || !utf8.ValidString(table) ||
		strings.ContainsAny(table, probeTableSeparators) {
		return false
	}
	for _, r := range table {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func decodeProbeJSON(encoded string, value any) error {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProbe, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidProbe, err)
	}
	return nil
}
