package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/regex"
)

// Spec captures the mutable, per-connection workload a DTail client wants to run.
type Spec struct {
	Mode        omode.Mode `json:"mode"`
	Files       []string   `json:"files"`
	Options     string     `json:"options,omitempty"`
	Query       string     `json:"query,omitempty"`
	Regex       string     `json:"regex,omitempty"`
	RegexInvert bool       `json:"regex_invert,omitempty"`
	Timeout     int        `json:"timeout,omitempty"`
}

// Parameters contains the client values needed to construct a session specification.
type Parameters struct {
	Mode              omode.Mode
	What              string
	SerializedOptions string
	Query             string
	Regex             string
	RegexInvert       bool
	Timeout           int
	Serverless        bool
}

// NewSpec returns a session specification from client args.
func NewSpec(parameters Parameters) Spec {
	files := splitFiles(parameters.What)
	if parameters.Serverless && len(files) == 0 && supportsServerlessPipe(parameters.Mode) {
		files = []string{"-"}
	}

	return Spec{
		Mode:        parameters.Mode,
		Files:       files,
		Options:     parameters.SerializedOptions,
		Query:       strings.TrimSpace(parameters.Query),
		Regex:       parameters.Regex,
		RegexInvert: parameters.RegexInvert,
		Timeout:     parameters.Timeout,
	}
}

// Commands returns the legacy command stream for this session specification.
func (s Spec) Commands() ([]string, error) {
	switch {
	case s.Mode == omode.HealthClient:
		return []string{"health"}, nil
	case s.Query != "":
		return s.queryCommands()
	default:
		return s.readCommands(s.Mode.String())
	}
}

// HasJournalFiles reports whether this session reads any systemd journal target.
func (s Spec) HasJournalFiles() bool {
	for _, file := range s.Files {
		if strings.HasPrefix(strings.TrimSpace(file), "journal:") {
			return true
		}
	}
	return false
}

// HasEncodedFiles reports whether a read command of this session carries an
// encoded path (protocol.ReadPathNeedsEncoding), which only servers
// advertising protocol.CapabilityEncodedReadPathV1 can read: a path that
// contains a space, or one that starts with protocol.EncodedReadPathPrefix.
func (s Spec) HasEncodedFiles() bool {
	for _, file := range s.Files {
		if protocol.ReadPathNeedsEncoding(file) {
			return true
		}
	}
	return false
}

// StartCommand returns the SESSION START command for this specification.
func (s Spec) StartCommand() (string, error) {
	payload, err := s.encodedPayload()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("SESSION START %s", payload), nil
}

// UpdateCommand returns the SESSION UPDATE command for this specification.
func (s Spec) UpdateCommand(generation uint64) (string, error) {
	payload, err := s.encodedPayload()
	if err != nil {
		return "", err
	}

	if generation == 0 {
		return fmt.Sprintf("SESSION UPDATE %s", payload), nil
	}

	return fmt.Sprintf("SESSION UPDATE %d %s", generation, payload), nil
}

// queryCommands returns the map command and one read command per file of a
// query session. The read commands select the lines of the query's table with
// s.Regex, which the client derives from the query, and never invert it, so
// RegexInvert is ignored here: inverted, the filter would feed the other
// tables' lines (or, for every-line tables, only the empty lines) into the
// aggregation. Ignoring it in this one place covers both sides, as the client
// builds its legacy command stream and dserver its SESSION START/UPDATE
// commands with it, so a dserver also reads the right lines for an older
// client that sends regex_invert with a query.
func (s Spec) queryCommands() ([]string, error) {
	if s.Mode != omode.MapClient && s.Mode != omode.TailClient {
		return nil, fmt.Errorf("session spec query mode requires map or tail mode, got %s", s.Mode)
	}

	regexValue, err := serializedRegex(s.Regex, false)
	if err != nil {
		return nil, err
	}

	commands := []string{fmt.Sprintf("map:%s %s", s.Options, s.Query)}
	readMode := "cat"
	if s.Mode == omode.TailClient {
		readMode = "tail"
	}

	for _, file := range s.Files {
		path := protocol.EncodeReadPath(file)
		if s.Timeout > 0 {
			commands = append(commands, fmt.Sprintf("timeout %d %s %s %s", s.Timeout, readMode, path, regexValue))
			continue
		}
		commands = append(commands, fmt.Sprintf("%s:%s %s %s", readMode, s.Options, path, regexValue))
	}

	return commands, nil
}

// readCommands returns one "<mode>:<options> <path> <regex>" command per
// file. Servers split commands on single spaces and take the second word as
// the path, so each path goes through protocol.EncodeReadPath (verbatim
// unless it contains a space or starts with the encoded prefix);
// queryCommands does the same for its read commands, including the
// "timeout N <mode> <path> <regex>" form.
func (s Spec) readCommands(mode string) ([]string, error) {
	switch s.Mode {
	case omode.TailClient, omode.CatClient, omode.GrepClient:
	default:
		return nil, fmt.Errorf("unsupported session mode %s", s.Mode)
	}

	regexValue, err := serializedRegex(s.Regex, s.RegexInvert)
	if err != nil {
		return nil, err
	}

	var commands []string
	for _, file := range s.Files {
		commands = append(commands, fmt.Sprintf("%s:%s %s %s", mode, s.Options,
			protocol.EncodeReadPath(file), regexValue))
	}

	return commands, nil
}

// serializedRegex returns the wire form of the read commands' regex.
func serializedRegex(pattern string, invert bool) (string, error) {
	flag := regex.Default
	if invert {
		flag = regex.Invert
	}

	re, err := regex.New(pattern, flag)
	if err != nil {
		return "", err
	}

	return re.Serialize()
}

// splitFiles splits a --files value (or a job's Files) at its commas, trims
// whitespace around every entry and drops empty entries. A comma therefore
// always separates two files, so paths containing a comma cannot be read,
// and neither can paths with leading or trailing whitespace. Inner spaces
// are kept (see protocol.EncodeReadPath).
func splitFiles(what string) []string {
	if strings.TrimSpace(what) == "" {
		return nil
	}

	rawFiles := strings.Split(what, ",")
	files := make([]string, 0, len(rawFiles))
	for _, file := range rawFiles {
		file = strings.TrimSpace(file)
		if file == "" {
			continue
		}
		files = append(files, file)
	}
	return files
}

func supportsServerlessPipe(mode omode.Mode) bool {
	switch mode {
	case omode.TailClient, omode.CatClient, omode.GrepClient, omode.MapClient:
		return true
	default:
		return false
	}
}

func (s Spec) encodedPayload() (string, error) {
	payload, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("marshal session spec: %w", err)
	}

	return base64.StdEncoding.EncodeToString(payload), nil
}
