package handlers

// Tests for read commands whose path contains a space. Servers split every
// command on single spaces, so such a path used to be cut at its first space
// and the rest parsed as the regex ("Unable to parse command"). Clients now
// send it encoded (protocol.EncodeReadPath) and readCommand decodes it before
// globbing, the readfiles permission check and the journal check.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/session"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

const spacedPathTestLine = "spaced-path-payload"

// newSpacedPathTestHandler returns a ServerHandler of a user with the given
// permissions, built like dserver builds it (serverlessOutput nil) or like
// the in-process serverless runtime does (serverlessOutput set).
func newSpacedPathTestHandler(t *testing.T, permissions []string, serverlessOutput io.Writer) *ServerHandler {
	t.Helper()
	lookup := func(string) ([]string, error) { return permissions, nil }
	user, err := userserver.New("spaced-path-user", "127.0.0.1", lookup, handlerTestLogger)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:       make(chan struct{}, 4),
		TailLimiter:      make(chan struct{}, 4),
		ServerConfig:     &config.ServerConfig{MapreduceLogFormat: "default", AuthKeyEnabled: true},
		AuthKeyStore:     authkey.New(time.Hour, 5),
		ServerlessOutput: serverlessOutput,
		Loggers:          HandlerLoggers{Diagnostics: handlerTestLogger, Reader: handlerTestLogger},
	})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	// A path that matches no file gives up after one short retry.
	handler.readTimings.globRetryInterval = time.Millisecond
	return handler
}

// writeSpacedPathTestFile writes the test line into dir/name, creating dir.
func writeSpacedPathTestFile(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(spacedPathTestLine+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// readSpecOutput runs the legacy command stream of a one-shot spec reading
// files and returns what reached the transport and the serverless output.
func readSpecOutput(t *testing.T, handler *ServerHandler, serverless *testOutput,
	mode omode.Mode, files ...string) (string, string) {
	t.Helper()
	spec := session.Spec{Mode: mode, Files: files, Options: "plain=true", Regex: spacedPathTestLine}
	commands, err := spec.Commands()
	if err != nil {
		t.Fatalf("build commands: %v", err)
	}
	transport := runLegacyCommandsOn(t, handler, commands)
	if serverless == nil {
		return transport, ""
	}
	return transport, serverless.String()
}

// TestReadCommandReadsPathsWithSpaces reads a file, a glob and a file named
// with the encoded prefix through dcat's and dgrep's command stream, in
// server and in serverless mode.
func TestReadCommandReadsPathsWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with  spaces ")
	file := writeSpacedPathTestFile(t, dir, "a b.log")
	prefixed := writeSpacedPathTestFile(t, t.TempDir(), protocol.EncodedReadPathPrefix+"x.log")
	targets := map[string]string{
		"file":            file,
		"glob":            filepath.Join(dir, "* *.log"),
		"prefixed name":   prefixed,
		"space-free path": writeSpacedPathTestFile(t, t.TempDir(), "plain.log"),
	}
	for name, target := range targets {
		for _, mode := range []omode.Mode{omode.CatClient, omode.GrepClient} {
			for _, serverlessMode := range []bool{false, true} {
				t.Run(name+"/"+mode.String()+"/serverless="+strconv.FormatBool(serverlessMode), func(t *testing.T) {
					var serverless *testOutput
					var output io.Writer
					if serverlessMode {
						serverless = &testOutput{}
						output = serverless
					}
					handler := newSpacedPathTestHandler(t, []string{"readfiles:.*"}, output)
					transport, direct := readSpecOutput(t, handler, serverless, mode, target)
					got := transport
					if serverlessMode {
						got = direct
					}
					if !strings.Contains(got, spacedPathTestLine) {
						t.Fatalf("%s of %q did not read the file:\ntransport: %q\nserverless: %q",
							mode, target, transport, direct)
					}
					if strings.Contains(transport, "Unable to parse command") {
						t.Fatalf("%s of %q failed to parse: %q", mode, target, transport)
					}
				})
			}
		}
	}
}

// TestReadCommandChecksPermissionsOnTheDecodedPath checks that the readfiles
// permission regex is matched against the decoded path: a rule naming the
// directory with the space allows the read, and a rule allowing only another
// directory denies it, even though the command carries the encoded word.
func TestReadCommandChecksPermissionsOnTheDecodedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with space")
	file := writeSpacedPathTestFile(t, dir, "app.log")

	allowed := newSpacedPathTestHandler(t,
		[]string{"readfiles:^" + regexp.QuoteMeta(dir) + "/[^/]+\\.log$"}, nil)
	if output, _ := readSpecOutput(t, allowed, nil, omode.CatClient, file); !strings.Contains(output, spacedPathTestLine) {
		t.Fatalf("permission on the decoded path did not allow the read: %q", output)
	}

	denied := newSpacedPathTestHandler(t, []string{"readfiles:^/nonexistent/"}, nil)
	output, _ := readSpecOutput(t, denied, nil, omode.CatClient, file)
	if strings.Contains(output, spacedPathTestLine) {
		t.Fatalf("read a file no permission allows: %q", output)
	}
	if want := protocol.HiddenCommandFailedPrefix + protocol.CommandFailurePermission; !strings.Contains(output, want) {
		t.Fatalf("denied read did not report %q: %q", want, output)
	}
}

// TestReadCommandRejectsInvalidEncodedPath checks that a path word with the
// encoded prefix that does not decode reads nothing and fails the command.
func TestReadCommandRejectsInvalidEncodedPath(t *testing.T) {
	for _, word := range []string{protocol.EncodedReadPathPrefix + "!!!", protocol.EncodedReadPathPrefix} {
		t.Run(word, func(t *testing.T) {
			handler := newSpacedPathTestHandler(t, []string{"readfiles:.*"}, nil)
			// The test logger logs nothing, but the client still gets the
			// message text (see logging.ErrorMessage) along with the hidden
			// failed-command reason.
			output := runLegacyCommandsOn(t, handler, []string{"cat: " + word + " regex:noop "})
			if want := protocol.HiddenCommandFailedPrefix + protocol.CommandFailureReadCommand; !strings.Contains(output, want) {
				t.Fatalf("invalid encoded path %q did not report %q: %q", word, want, output)
			}
			if want := "Unable to parse command"; !strings.Contains(output, want) {
				t.Fatalf("invalid encoded path %q did not tell the client %q: %q", word, want, output)
			}
		})
	}
}

// TestSessionCommandsEncodeSpacedPaths checks SESSION START and UPDATE: the
// server builds the read commands from the spec's Files itself, and each
// read command, also the "timeout N" form of a query, carries one path word
// that decodes to the file.
func TestSessionCommandsEncodeSpacedPaths(t *testing.T) {
	const file = "/var/log/my app/a b.log"
	specs := map[string]session.Spec{
		"grep":          {Mode: omode.GrepClient, Files: []string{file}, Regex: "x y"},
		"query timeout": {Mode: omode.TailClient, Files: []string{file}, Query: "from STATS select count(*)", Timeout: 5},
	}
	for name, spec := range specs {
		for _, update := range []bool{false, true} {
			t.Run(name+"/update="+strconv.FormatBool(update), func(t *testing.T) {
				handler, dispatched := newArgsRecordingSessionHandler(t)
				commands := startSession(t, handler, dispatched, spec, update)
				read := commands[len(commands)-1]
				if got, err := protocol.DecodeReadPath(read[1]); err != nil || got != file {
					t.Fatalf("read command path word %q decodes to %q (%v), want %q", read[1], got, err, file)
				}
			})
		}
	}
}

// TestReadCommandParsePath covers parsePath directly: verbatim words, encoded
// words and invalid encoded words.
func TestReadCommandParsePath(t *testing.T) {
	tests := []struct {
		word   string
		want   string
		wantOK bool
	}{
		{word: "/var/log/app.log", want: "/var/log/app.log", wantOK: true},
		{word: "-", want: "-", wantOK: true},
		{word: "journal:ssh.service", want: "journal:ssh.service", wantOK: true},
		{word: protocol.EncodeReadPath("/a b/c.log"), want: "/a b/c.log", wantOK: true},
		{word: protocol.EncodeReadPath("journal:my unit.service"), want: "journal:my unit.service", wantOK: true},
		{word: protocol.EncodedReadPathPrefix + "%%%"},
	}
	for _, tc := range tests {
		t.Run(tc.word, func(t *testing.T) {
			server := newFailureTestServer(10)
			command := newReadCommand(server, omode.CatClient)
			got, ok := command.parsePath(context.Background(), tc.word)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("parsePath(%q) = %q, %v; want %q, %v", tc.word, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
