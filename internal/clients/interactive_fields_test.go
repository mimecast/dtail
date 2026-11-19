package clients

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/protocol"
)

// fieldsCatalog is a catalog of two servers with two log formats, notes,
// errors and names and values that must be shown escaped.
var fieldsCatalog = catalogSnapshot{
	DefaultLogFormat: "default",
	Servers:          []string{"srv1", "srv2"},
	Updated:          time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC),
	Truncated:        true,
	Formats: map[string]catalogFormat{
		"generickv": {Tables: map[string]map[string][]string{"": {"user": {"alice"}}}},
		"default": {
			Tables: map[string]map[string][]string{
				"STATS": {"goroutines": {"12", "15"}, "noSamples": {}},
				"WEB":   {"status": {"200"}, "evil\x1b[2J": {"a\x1b[31mb"}},
				"SIZE":  {"größe": {"3"}, "len": {"4"}},
			},
			Variables: map[string][]string{"$hostname": {"web1"}},
			Notes:     []string{"grouped by MAPREDUCE:<TABLE>"},
		},
	},
	Errors: []catalogError{{Server: "srv2", File: "/var/log/x.log", Error: "read: no file to read"}},
}

var fieldsCatalogUpdatedLater = fieldsCatalog.Updated.Add(90 * time.Second)

// TestParseInteractiveCommandFieldsProbe: :fields takes at most one table
// name, :probe none; other words starting like them are unknown commands.
func TestParseInteractiveCommandFieldsProbe(t *testing.T) {
	tests := []struct {
		line      string
		wantKind  string
		wantTable string
		wantErr   string
	}{
		{line: ":fields", wantKind: "fields"},
		{line: "  :fields \r", wantKind: "fields"},
		{line: ":fields stats", wantKind: "fields", wantTable: "stats"},
		{line: ":fields\tSTATS  ", wantKind: "fields", wantTable: "STATS"},
		{line: ":probe", wantKind: "probe"},
		{line: " :probe\t", wantKind: "probe"},
		{line: ":fields a b", wantErr: ":fields takes at most one table name"},
		{line: ":fieldsx", wantErr: "unknown command"},
		{line: ":probe now", wantErr: ":probe takes no arguments"},
		{line: ":probes", wantErr: "unknown command"},
	}
	for _, tt := range tests {
		command, err := parseInteractiveCommand(completeGrepArgs, tt.line)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("%q: err %v, want %q", tt.line, err, tt.wantErr)
			}
			continue
		}
		if err != nil || command.kind != tt.wantKind || command.table != tt.wantTable {
			t.Fatalf("%q: kind %q table %q err %v, want %q %q", tt.line, command.kind, command.table, err,
				tt.wantKind, tt.wantTable)
		}
	}
}

// TestFieldsText checks the :fields listing: the header with servers, age
// and default format, the default format first, tables, samples, variables,
// notes, errors, the truncation note and escaped server text.
func TestFieldsText(t *testing.T) {
	want := strings.Join([]string{
		"fields found by the probe of srv1, srv2, updated 1m30s ago; default log format default",
		"log format default:",
		"  table SIZE:",
		`    größe  e.g. "3"`,
		`    len    e.g. "4"`,
		"  table STATS:",
		`    goroutines  e.g. "12", "15"`,
		"    noSamples",
		"  table WEB:",
		`    "evil\x1b[2J"  e.g. "a\x1b[31mb"`,
		`    status         e.g. "200"`,
		"  $variables:",
		`    $hostname  e.g. "web1"`,
		"  note: grouped by MAPREDUCE:<TABLE>",
		"log format generickv:",
		"  fields:",
		`    user  e.g. "alice"`,
		"probe error on srv2: read: no file to read (/var/log/x.log)",
		"(an answer was cut to the size limit: fields or sample values may be missing)",
	}, "\n")
	if got := fieldsText(fieldsCatalog, "", fieldsCatalogUpdatedLater); got != want {
		t.Fatalf("fields text:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(fieldsText(fieldsCatalog, "", fieldsCatalogUpdatedLater), "\x1b") {
		t.Fatal("an escape sequence from a server reached the terminal")
	}
}

// TestFieldsTextTable checks :fields with a table: only that table (any
// case) with the $variables of its format; an unknown table names the
// known ones; an empty catalog says that no probe answered yet.
func TestFieldsTextTable(t *testing.T) {
	got := fieldsText(fieldsCatalog, "stats", fieldsCatalogUpdatedLater)
	for _, want := range []string{"table STATS:", "goroutines", "$hostname", "probe error on srv2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fields stats %q lacks %q", got, want)
		}
	}
	for _, unwanted := range []string{"WEB", "generickv", "user"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("fields stats %q contains %q", got, unwanted)
		}
	}

	got = fieldsText(fieldsCatalog, "NOPE\x07", fieldsCatalogUpdatedLater)
	if !strings.Contains(got, `no table "NOPE\a" found; tables: SIZE, STATS, WEB`) || strings.Contains(got, "log format default:") {
		t.Fatalf("unknown table: %q", got)
	}
	if got := fieldsText(catalogSnapshot{}, "", fieldsCatalogUpdatedLater); got != fieldsEmptyMessage {
		t.Fatalf("empty catalog: %q", got)
	}
	answeredNothing := catalogSnapshot{Servers: []string{"srv1"}, Updated: fieldsCatalog.Updated,
		Formats: map[string]catalogFormat{}}
	if got := fieldsText(answeredNothing, "", fieldsCatalog.Updated); !strings.HasSuffix(got,
		"\nno log format found fields in the sampled lines") {
		t.Fatalf("answer without formats: %q", got)
	}
}

// TestProbeResultMessage covers every answer of :probe.
func TestProbeResultMessage(t *testing.T) {
	tests := []struct {
		name   string
		result probeRoundResult
		status probeWaitStatus
		want   string
	}{
		{name: "stopped", status: probeWaitStopped, want: "probe not run: the background prober is not running"},
		{name: "timeout", status: probeWaitTimeout, want: "probe requested, no answer within 5s"},
		{name: "no files", want: "nothing to probe: the session reads no files"},
		{name: "no connection", result: probeRoundResult{files: 1}, want: "nothing to probe: no server connection is up"},
		{name: "answered", result: probeRoundResult{files: 1, answered: []string{"a", "b"}},
			want: "probe answered by a, b; :fields shows what was found"},
		{name: "mixed", result: probeRoundResult{files: 1, answered: []string{"a"}, busy: []string{"b"},
			failed: []string{"c\n"}},
			want: `probe answered by a; busy with another probe: b; no answer from "c\n" (see the debug log); :fields`},
		{name: "all failed", result: probeRoundResult{files: 1, failed: []string{"a"}},
			want: "probe no answer from a (see the debug log)"},
	}
	for _, tt := range tests {
		got := probeResultMessage(tt.result, tt.status)
		if !strings.HasPrefix(got, tt.want) {
			t.Fatalf("%s: %q, want prefix %q", tt.name, got, tt.want)
		}
		if tt.name == "all failed" && strings.Contains(got, ":fields") {
			t.Fatalf("%s: %q points to :fields without an answer", tt.name, got)
		}
	}
}

// releaseProbeHandler answers a probe with answer once it receives a value
// from release (each probe takes one; a closed release lets every probe
// through), counting the probes. Path listings are answered at once with
// no entries and not counted.
type releaseProbeHandler struct {
	handlers.Handler
	done    chan struct{}
	release chan struct{}
	answer  protocol.ProbeAnswer
	probes  chan struct{}
}

func newReleaseProbeHandler(answer protocol.ProbeAnswer) *releaseProbeHandler {
	return &releaseProbeHandler{done: make(chan struct{}), release: make(chan struct{}), answer: answer,
		probes: make(chan struct{}, 16)}
}

func (h *releaseProbeHandler) Done() <-chan struct{} { return h.done }

// WaitForCapabilities reports the session up, as probeTargets asks.
func (h *releaseProbeHandler) WaitForCapabilities(time.Duration) bool { return true }

func (h *releaseProbeHandler) Probe(ctx context.Context, request protocol.ProbeRequest) (protocol.ProbeAnswer, error) {
	if request.List != "" {
		// The path listings prefetched after a round are answered at once
		// and not counted: only the field probes are held back.
		return protocol.ProbeAnswer{ID: request.ID}, nil
	}
	h.probes <- struct{}{}
	select {
	case <-h.release:
		return h.answer, nil
	case <-ctx.Done():
		return protocol.ProbeAnswer{}, ctx.Err()
	}
}

// TestProbeNowWaitsForNewRound: :probe waits for a round that starts after
// its request, not for the round that is running (which asked for the files
// of before), and returns that round's result. Each probe is released on its
// own, so probeNow must still wait once the running round finished and the
// new round waits in its probe.
func TestProbeNowWaitsForNewRound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler := newReleaseProbeHandler(protocol.ProbeAnswer{DefaultLogFormat: "default", FilesSampled: 1})
		c, _ := newProbeTestClient([]string{"/a.log"}, &probeTestConnector{server: "srv", handler: handler})
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait() // the first round waits in its probe

		type outcome struct {
			result probeRoundResult
			status probeWaitStatus
		}
		done := make(chan outcome, 1)
		go func() {
			result, status := c.prober.probeNow()
			done <- outcome{result, status}
		}()
		synctest.Wait()
		handler.release <- struct{}{} // the running round finishes
		synctest.Wait()               // the new round waits in its probe
		select {
		case got := <-done:
			t.Fatalf("probeNow returned the result of the round running before its request: %+v", got)
		default:
		}
		if probes := len(handler.probes); probes != 2 {
			t.Fatalf("probes = %d, want the running round and a new one", probes)
		}
		handler.release <- struct{}{} // the new round finishes
		got := <-done
		if got.status != probeWaitDone || !reflect.DeepEqual(got.result.answered, []string{"srv"}) {
			t.Fatalf("probeNow = %+v", got)
		}
	})
}

// TestProbeNowOutcomes: busy and failed servers are reported apart from
// the answering one, a probe that does not answer within the wait times
// out, and a stopped or missing prober is reported as such.
func TestProbeNowOutcomes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		answering, _ := probeTestConnection("answering")
		failing, failingHandler := probeTestConnection("failing")
		failingHandler.err = errors.New("boom")
		busyHandler := newReleaseProbeHandler(protocol.ProbeAnswer{
			Errors: []protocol.ProbeError{{Error: protocol.ProbeErrorBusy}}})
		close(busyHandler.release)
		busy := &probeTestConnector{server: "busy", handler: busyHandler}
		c, logger := newProbeTestClient([]string{"/a.log"}, answering, failing, busy)
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)

		result, status := c.prober.probeNow()
		want := probeRoundResult{files: 1, answered: []string{"answering"}, busy: []string{"busy"},
			failed: []string{"failing"}}
		if status != probeWaitDone || !reflect.DeepEqual(result, want) {
			t.Fatalf("probeNow = %+v %v, want %+v", result, status, want)
		}
		if servers := c.prober.catalogSnapshot().Servers; !reflect.DeepEqual(servers, []string{"answering"}) {
			t.Fatalf("catalog servers = %q", servers)
		}
		cancel()
		c.prober.wait()
		if _, status := c.prober.probeNow(); status != probeWaitStopped {
			t.Fatalf("stopped prober: status %v", status)
		}
		logger.requireSilent(t)
	})

	synctest.Test(t, func(t *testing.T) {
		silent, silentHandler := probeTestConnection("silent")
		silentHandler.block = true
		c, _ := newProbeTestClient([]string{"/a.log"}, silent)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		start := time.Now()
		if _, status := c.prober.probeNow(); status != probeWaitTimeout ||
			time.Since(start) != probeNowWait {
			t.Fatalf("silent server: status %v after %s", status, time.Since(start))
		}
	})

	var nilProber *interactiveProber
	if _, status := nilProber.probeNow(); status != probeWaitStopped {
		t.Fatalf("nil prober: status %v", status)
	}
}

// TestHandleInteractiveLineFieldsAndProbePause: :fields keeps the output
// paused like :help (typed, or without the line editor); :probe resumes an
// explicit pause first, like :reload, and answers without a prober that it
// did not run; a rejected :probe line leaves the pause alone.
func TestHandleInteractiveLineFieldsAndProbePause(t *testing.T) {
	logger := newPausingTestLogger()
	client := newPauseTestClient(logger)
	pause := client.newInteractivePause()
	var tty bytes.Buffer

	pause.pauseTyping()
	if _, err := client.handleInteractiveLine(&tty, ":fields", pause); err != nil {
		t.Fatal(err)
	}
	want := fieldsEmptyMessage + "\n" + interactivePausedMessage + "\n"
	if tty.String() != want || !pause.paused || pause.typing || logger.pauseDepth() != 1 {
		t.Fatalf(":fields typed: tty %q paused %v typing %v depth %d", tty.String(), pause.paused,
			pause.typing, logger.pauseDepth())
	}

	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":probe x", pause); err != nil || !pause.paused {
		t.Fatalf("rejected :probe: err %v paused %v", err, pause.paused)
	}
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":probe", pause); err != nil {
		t.Fatal(err)
	}
	want = interactiveResumedMessage + "\nprobe not run: the background prober is not running\n"
	if tty.String() != want || pause.paused || logger.pauseDepth() != 0 {
		t.Fatalf(":probe paused: tty %q paused %v depth %d", tty.String(), pause.paused, logger.pauseDepth())
	}

	// Without the line editor nothing paused: :fields pauses explicitly.
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":fields STATS", pause); err != nil {
		t.Fatal(err)
	}
	if !pause.paused || pause.typing || !strings.HasSuffix(tty.String(), interactivePausedMessage+"\n") {
		t.Fatalf(":fields unpaused: paused %v tty %q", pause.paused, tty.String())
	}
	// A typed :probe resumes before it waits and prints no resume message.
	if !pause.resume() {
		t.Fatal("not paused")
	}
	pause.pauseTyping()
	tty.Reset()
	if _, err := client.handleInteractiveLine(&tty, ":probe", pause); err != nil {
		t.Fatal(err)
	}
	if pause.paused || strings.Contains(tty.String(), interactiveResumedMessage) {
		t.Fatalf(":probe typed: paused %v tty %q", pause.paused, tty.String())
	}
}

// TestRunProbeCommandAnswers runs :probe end to end against a started
// prober: the answer names the server and the catalog holds its fields,
// which :fields then lists.
func TestRunProbeCommandAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, _ := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"/a.log"}, []connectors.Connector{conn}...)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)

		var tty bytes.Buffer
		if err := c.runProbeCommand(&tty); err != nil {
			t.Fatal(err)
		}
		if tty.String() != "probe answered by srv1; :fields shows what was found\n" {
			t.Fatalf(":probe: %q", tty.String())
		}
		tty.Reset()
		if _, err := c.runInteractiveCommand(&tty, interactiveCommand{kind: "fields", table: "STATS"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(tty.String(), "table STATS:\n    files  e.g. \"/a.log\"") ||
			!strings.Contains(tty.String(), "updated 0s ago") {
			t.Fatalf(":fields: %q", tty.String())
		}
	})
}

// TestCompleteFieldsTable: Tab after ":fields " completes the catalog's
// tables ignoring case, lists several, hints on an empty catalog, and
// leaves other words and lines alone.
func TestCompleteFieldsTable(t *testing.T) {
	tests := []struct {
		name           string
		line           string
		catalog        catalogSnapshot
		wantLine       string
		wantCandidates []string
		wantHint       string
	}{
		{name: "command", line: ":fi", catalog: fieldsCatalog, wantLine: ":fields "},
		{name: "probe command", line: ":pr", catalog: fieldsCatalog, wantLine: ":probe"},
		{name: "table any case", line: ":fields st", catalog: fieldsCatalog, wantLine: ":fields STATS"},
		{name: "tables listed", line: ":fields ", catalog: fieldsCatalog, wantLine: ":fields ",
			wantCandidates: []string{"SIZE", "STATS", "WEB"}},
		{name: "empty catalog", line: ":fields S", catalog: catalogSnapshot{}, wantLine: ":fields S",
			wantHint: fieldsTableHint},
		{name: "no match", line: ":fields X", catalog: fieldsCatalog, wantLine: ":fields X"},
		{name: "second word", line: ":fields STATS s", catalog: fieldsCatalog, wantLine: ":fields STATS s"},
		{name: "fieldsx", line: ":fieldsx s", catalog: fieldsCatalog, wantLine: ":fieldsx s"},
		{name: "quote", line: ":fields 'S", catalog: fieldsCatalog, wantLine: ":fields 'S"},
		{name: "after probe", line: ":probe S", catalog: fieldsCatalog, wantLine: ":probe S"},
	}
	for _, tt := range tests {
		got := completeInteractive(tt.line, len(tt.line), completeGrepArgs, catalogOf(tt.catalog))
		if got.line != tt.wantLine || !reflect.DeepEqual(got.candidates, tt.wantCandidates) ||
			got.hint != tt.wantHint {
			t.Fatalf("%s: line %q candidates %q hint %q, want %q %q %q", tt.name, got.line, got.candidates,
				got.hint, tt.wantLine, tt.wantCandidates, tt.wantHint)
		}
	}
}
