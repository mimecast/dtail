package handlers

// Tests of what a probe samples: the same path decoding, globbing, glob cap
// and readfiles permission check as read commands, the journal capability,
// the file limit, and requests that do not decode.

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/authkey"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/journal"
	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// probeTestStatsLine is a default format line of table STATS with a field
// whose value tests look for in the answer.
func probeTestStatsLine(field, value string) string {
	return "INFO|20211002-071209|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|" +
		field + "=" + value
}

// newProbeTestHandler returns a ServerHandler of a user with permissions,
// built like dserver (serverless nil) or the serverless runtime builds it.
func newProbeTestHandler(t *testing.T, permissions []string, serverless io.Writer,
	capabilities ...string) *ServerHandler {
	t.Helper()
	return newProbeTestHandlerWithUnits(t, permissions, serverless, nil, capabilities...)
}

// newProbeTestHandlerWithUnits is newProbeTestHandler with the journal unit
// cache of the dserver (Dependencies.JournalUnits; nil for none).
func newProbeTestHandlerWithUnits(t *testing.T, permissions []string, serverless io.Writer,
	units *journal.UnitCache, capabilities ...string) *ServerHandler {
	t.Helper()
	lookup := func(string) ([]string, error) { return permissions, nil }
	user, err := userserver.New("probe-user", "127.0.0.1", lookup, handlerTestLogger)
	if err != nil {
		t.Fatalf("new user: %v", err)
	}
	handler, err := NewServerHandler(context.Background(), user, Dependencies{
		CatLimiter:       make(chan struct{}, 4),
		TailLimiter:      make(chan struct{}, 4),
		ServerConfig:     &config.ServerConfig{MapreduceLogFormat: "default"},
		AuthKeyStore:     authkey.New(time.Hour, 5),
		ServerlessOutput: serverless,
		Loggers:          HandlerLoggers{Diagnostics: handlerTestLogger, Reader: handlerTestLogger},
		Capabilities:     capabilities,
		JournalUnits:     units,
		Hostname:         "probe-host.example.org",
	})
	if err != nil {
		t.Fatalf("NewServerHandler: %v", err)
	}
	return handler
}

// writeProbeTestFile writes lines into dir/name, creating dir.
func writeProbeTestFile(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// realTempDir is t.TempDir with its symlinks resolved, so that permission
// rules match the paths the permission check compares them with.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	return dir
}

// probeCommandFor returns the probe command of request.
func probeCommandFor(t *testing.T, request protocol.ProbeRequest) string {
	t.Helper()
	command, err := protocol.EncodeProbeCommand(request)
	if err != nil {
		t.Fatalf("encode probe: %v", err)
	}
	return command
}

// runProbe sends command through the handler's command dispatch and returns
// the answer and its raw frame. It fails the test on a failed-command report.
func runProbe(t *testing.T, handler *ServerHandler, command string) (protocol.ProbeAnswer, string) {
	t.Helper()
	handler.handleCommand(encodedTestCommand(command))
	return readProbeAnswer(t, handler.serverMessages)
}

// failOnGenerationBoundProbeAnswer fails the test at once when message is
// a probe answer bound to a session generation: the session output would
// drop it as stale (a probe carries no generation), so a test waiting for
// the answer would only run into its timeout.
func failOnGenerationBoundProbeAnswer(t *testing.T, message string) {
	t.Helper()
	if generation, decoded := decodeGeneratedMessage(message); generation != 0 &&
		strings.HasPrefix(decoded, protocol.HiddenProbePrefix) {
		t.Fatalf("probe answer bound to generation %d: %q", generation, message)
	}
}

// readProbeAnswer reads server messages until a probe answer arrives.
func readProbeAnswer(t *testing.T, messages <-chan string) (protocol.ProbeAnswer, string) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case message := <-messages:
			failOnGenerationBoundProbeAnswer(t, message)
			if strings.HasPrefix(message, protocol.HiddenCommandFailedPrefix) {
				t.Fatalf("probe reported a failed command: %q", message)
			}
			if !strings.HasPrefix(message, protocol.HiddenProbePrefix) {
				continue
			}
			answer, err := protocol.DecodeProbeAnswer(message)
			if err != nil {
				t.Fatalf("decode answer %q: %v", message, err)
			}
			return answer, message
		case <-timeout:
			t.Fatal("timed out waiting for the probe answer")
		}
	}
}

// probeFiles runs a probe of entries and returns the answer and its JSON.
func probeFiles(t *testing.T, handler *ServerHandler, entries ...string) (protocol.ProbeAnswer, string) {
	t.Helper()
	answer, frame := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "t1", Files: entries}))
	if answer.ID != "t1" {
		t.Fatalf("answer id = %q, want t1", answer.ID)
	}
	encoded := frame[strings.LastIndexByte(frame, ' ')+1:]
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	return answer, string(data)
}

// hasProbeError reports whether answer lists reason for entry.
func hasProbeError(answer protocol.ProbeAnswer, entry, reason string) bool {
	for _, probeErr := range answer.Errors {
		if probeErr.File == entry && probeErr.Error == reason {
			return true
		}
	}
	return false
}

func TestProbeAnswersFieldsOfTheDefaultFormat(t *testing.T) {
	file := writeProbeTestFile(t, t.TempDir(), "app.log",
		probeTestStatsLine("currentConnections", "42"), "a plain line")
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	answer, _ := probeFiles(t, handler, file)
	if len(answer.Errors) != 0 {
		t.Fatalf("errors = %+v", answer.Errors)
	}
	if answer.FilesSampled != 1 || answer.LinesSampled != 2 || answer.DefaultLogFormat != "default" {
		t.Fatalf("answer = %+v", answer)
	}
	stats := answer.Formats["default"].Tables["STATS"]
	if got := stats["currentConnections"]; len(got) != 1 || got[0] != "42" {
		t.Fatalf("STATS currentConnections = %v (formats %+v)", got, answer.Formats)
	}
	if got := answer.Formats["default"].Variables["$hostname"]; len(got) != 1 || got[0] != "probe-host" {
		t.Fatalf("$hostname = %v, want [probe-host]", got)
	}
}

// TestProbeKeepsTheCSVHeaderOfALargeFile checks that the header of a csv
// file whose first line is not among the sampled lines still names the csv
// fields: the sampler's FirstLine is put in front of the sample. That is a
// file larger than the sample window, or one of more lines than a sample
// keeps.
func TestProbeKeepsTheCSVHeaderOfALargeFile(t *testing.T) {
	for name, row := range map[string]string{
		"larger than the window": "1,2," + strings.Repeat("x", 100),
		"more lines than kept":   "1,2,3",
	} {
		t.Run(name, func(t *testing.T) {
			lines := []string{"alpha,beta,gamma"}
			for len(lines) < 4000 {
				lines = append(lines, row)
			}
			file := writeProbeTestFile(t, t.TempDir(), "big.csv", lines...)
			handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

			answer, _ := probeFiles(t, handler, file)
			// The header put in front is no sampled line.
			if answer.LinesSampled > fs.DefaultSampleMaxLines {
				t.Fatalf("lines sampled = %d, more than %d", answer.LinesSampled, fs.DefaultSampleMaxLines)
			}
			fields := answer.Formats["csv"].Tables[""]
			for _, column := range []string{"alpha", "beta", "gamma"} {
				if _, ok := fields[column]; !ok {
					t.Fatalf("csv fields %v lack header column %q", fields, column)
				}
			}
		})
	}
}

// TestProbeAppliesReadPermissions checks that a probe samples nothing a read
// command could not read: a denied file, a glob escaping the permitted
// directory with "..", and a symlink in it pointing outside.
func TestProbeAppliesReadPermissions(t *testing.T) {
	root := realTempDir(t)
	allowed := filepath.Join(root, "allowed")
	secretDir := filepath.Join(root, "secret")
	writeProbeTestFile(t, allowed, "a.log", probeTestStatsLine("visibleField", "visible"))
	secret := writeProbeTestFile(t, secretDir, "s.log", probeTestStatsLine("secretField", "topsecret"))
	if err := os.Symlink(secret, filepath.Join(allowed, "link.log")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	permissions := []string{"readfiles:^" + regexp.QuoteMeta(allowed) + "/"}

	tests := []struct {
		name    string
		entry   string
		sampled int
	}{
		{name: "denied file", entry: secret},
		{name: "glob escaping with ..", entry: filepath.Join(allowed, "..", "secret", "*.log")},
		{name: "symlink out of the permitted directory", entry: filepath.Join(allowed, "*.log"), sampled: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newProbeTestHandler(t, permissions, nil)
			answer, data := probeFiles(t, handler, tt.entry)
			if strings.Contains(data, "secretField") || strings.Contains(data, "topsecret") {
				t.Fatalf("answer reveals the denied file: %s", data)
			}
			if !hasProbeError(answer, tt.entry, protocol.CommandFailurePermission) {
				t.Fatalf("errors = %+v, want %q for %q", answer.Errors, protocol.CommandFailurePermission, tt.entry)
			}
			if answer.FilesSampled != tt.sampled {
				t.Fatalf("files sampled = %d, want %d", answer.FilesSampled, tt.sampled)
			}
			if tt.sampled > 0 && !strings.Contains(data, "visibleField") {
				t.Fatalf("permitted file not sampled: %s", data)
			}
		})
	}
}

// TestProbeDecodesEncodedPaths checks that entries are read command path
// words: an encoded path with blanks is sampled, an undecodable one is not.
func TestProbeDecodesEncodedPaths(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dir with space")
	file := writeProbeTestFile(t, dir, "a b.log", probeTestStatsLine("spaced", "yes"))
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	answer, _ := probeFiles(t, handler, protocol.EncodeReadPath(file))
	if answer.FilesSampled != 1 || len(answer.Formats["default"].Tables["STATS"]["spaced"]) != 1 {
		t.Fatalf("encoded path not sampled: %+v", answer)
	}

	invalid := protocol.EncodedReadPathPrefix + "!!!"
	answer, _ = probeFiles(t, handler, invalid)
	if answer.FilesSampled != 0 || !hasProbeError(answer, invalid, protocol.CommandFailureReadCommand) {
		t.Fatalf("undecodable entry: %+v", answer)
	}
}

// TestProbeJournalTargets checks that a journal target needs the journal
// capability and the readfiles permission before journalctl would run.
func TestProbeJournalTargets(t *testing.T) {
	const entry = "journal:probe-test.service"
	withoutCapability := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	answer, _ := probeFiles(t, withoutCapability, entry)
	if answer.FilesSampled != 0 || !hasProbeError(answer, entry, protocol.ProbeErrorJournalUnsupported) {
		t.Fatalf("journal target without capability: %+v", answer)
	}

	denied := newProbeTestHandler(t, []string{"readfiles:^/nothing/"}, nil, protocol.CapabilityJournalV1)
	denied.probeOverrides.sampler = failingProbeSampler(t)
	answer, _ = probeFiles(t, denied, entry)
	if answer.FilesSampled != 0 || !hasProbeError(answer, entry, protocol.CommandFailurePermission) {
		t.Fatalf("denied journal target: %+v", answer)
	}
}

// TestProbeJournalTargetsNoDenyBypass checks that a journal target naming
// no single literal unit (a glob journalctl -u expands, a name it completes
// with .service, a slice) never reaches the sampler, so a deny rule on the
// unit can not be bypassed by the probe.
func TestProbeJournalTargetsNoDenyBypass(t *testing.T) {
	permissions := []string{`readfiles:!^journal:sshd\.service$`, `readfiles:^journal:.*`}
	handler := newProbeTestHandler(t, permissions, nil, protocol.CapabilityJournalV1)
	handler.probeOverrides.sampler = failingProbeSampler(t)
	for _, entry := range []string{
		"journal:ssh*.service", "journal:ssh[d].service", "journal:*.service",
		"journal:sshd", "journal:system.slice",
	} {
		answer, _ := probeFiles(t, handler, entry)
		if answer.FilesSampled != 0 || !hasProbeError(answer, entry, protocol.CommandFailureReadingFile) {
			t.Errorf("entry %q: %+v", entry, answer)
		}
	}
}

// TestProbeLimitsFiles checks protocol.ProbeMaxFiles over entries and glob
// matches, and the MaxGlobTargets cap of a glob.
func TestProbeLimitsFiles(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := range 12 {
		files = append(files, writeProbeTestFile(t, dir, fmt.Sprintf("f%02d.log", i), probeTestStatsLine("n", "1")))
	}
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	for name, entries := range map[string][]string{
		"entries": files,
		"glob":    {filepath.Join(dir, "*.log")},
	} {
		t.Run(name, func(t *testing.T) {
			answer, _ := probeFiles(t, handler, entries...)
			if answer.FilesSampled != protocol.ProbeMaxFiles || !hasProbeError(answer, "", protocol.ProbeErrorTooManyFiles) {
				t.Fatalf("answer = %+v, want %d files and %q", answer, protocol.ProbeMaxFiles, protocol.ProbeErrorTooManyFiles)
			}
		})
	}

	handler.readTimings.maxGlobTargets = 3
	glob := filepath.Join(dir, "*.log")
	answer, _ := probeFiles(t, handler, glob)
	if answer.FilesSampled != 3 || !hasProbeError(answer, glob, protocol.CommandFailureGlobCapped) {
		t.Fatalf("glob cap: %+v", answer)
	}
}

// TestProbeReportsUnreadableEntries checks entries that name nothing to read.
func TestProbeReportsUnreadableEntries(t *testing.T) {
	dir := t.TempDir()
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.sampler = failingProbeSampler(t)
	tests := map[string]string{
		filepath.Join(dir, "missing.log"): protocol.CommandFailureNoFile,
		dir:                               protocol.CommandFailureNoFile, // a directory only
		filepath.Join(dir, "[.log"):       protocol.CommandFailureReadCommand,
		"-":                               protocol.ProbeErrorInvalidRequest, // never stdin
		" ":                               protocol.ProbeErrorInvalidRequest,
	}
	for entry, reason := range tests {
		t.Run(entry, func(t *testing.T) {
			answer, _ := probeFiles(t, handler, entry)
			if answer.FilesSampled != 0 || len(answer.Formats) != 0 || !hasProbeError(answer, entry, reason) {
				t.Fatalf("answer = %+v, want error %q for %q", answer, reason, entry)
			}
		})
	}
}

// TestProbeRejectsInvalidRequests checks requests that do not decode: they
// are answered with protocol.ProbeUnknownID, never as a failed command.
func TestProbeRejectsInvalidRequests(t *testing.T) {
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	tests := map[string]string{
		"no argument":   "probe",
		"two arguments": "probe " + encode(`{"id":"a","files":["x"]}`) + " x",
		"not base64":    "probe !!!",
		"invalid id":    "probe " + encode(`{"id":"a b","files":["x"]}`),
	}
	for name, command := range tests {
		t.Run(name, func(t *testing.T) {
			handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
			answer, _ := runProbe(t, handler, command)
			if answer.ID != protocol.ProbeUnknownID || !hasProbeError(answer, "", protocol.ProbeErrorInvalidRequest) {
				t.Fatalf("answer = %+v", answer)
			}
		})
	}

	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	answer, _ := probeFiles(t, handler)
	if !hasProbeError(answer, "", protocol.ProbeErrorInvalidRequest) {
		t.Fatalf("request without files: %+v", answer)
	}
}

// TestProbeSamplesAFileOnce checks that a file named by a path, a glob and a
// symlink is sampled once, takes one slot, and its lines count once.
func TestProbeSamplesAFileOnce(t *testing.T) {
	dir := realTempDir(t)
	file := writeProbeTestFile(t, dir, "a.log", probeTestStatsLine("n", "1"), probeTestStatsLine("n", "2"))
	if err := os.Symlink(file, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	answer, _ := probeFiles(t, handler, file, filepath.Join(dir, "*.log"), filepath.Join(dir, "link.txt"))
	if answer.FilesSampled != 1 || answer.LinesSampled != 2 || len(answer.Errors) != 0 {
		t.Fatalf("answer = %+v, want one file of 2 lines", answer)
	}
}

// failingProbeSampler fails the test when a probe samples anything.
func failingProbeSampler(t *testing.T) probeSampler {
	return func(_ context.Context, _ fs.ValidatedReadTarget, path string, _ fs.SampleOptions) (fs.Sample, error) {
		t.Errorf("probe sampled %q", path)
		return fs.Sample{}, fmt.Errorf("unexpected sample")
	}
}
