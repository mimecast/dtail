package handlers

// Tests of the table-targeted probe (protocol.ProbeRequest.Table) and of the
// per-table $variables and table discovery of a plain probe.

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// A registered log format whose ProbeTabler panics on lines holding
// probeTestPanicLine, which no other test's sample holds.
const (
	probeTestPanicFormat = "probetest-panicking-tabler"
	probeTestPanicLine   = "PROBETEST-PANIC-THE-TABLER"
)

var registerPanicFormat sync.Once

// panickingTablerParser parses no line; its ProbeLineTable panics on
// probeTestPanicLine.
type panickingTablerParser struct{}

func (panickingTablerParser) MakeFields(string, string) (map[string]string, error) {
	return nil, logformat.ErrIgnoreFields
}

func (panickingTablerParser) ProbeLineTable(line string) (string, bool) {
	if strings.Contains(line, probeTestPanicLine) {
		panic("tabler exploded on purpose")
	}
	return "", false
}

// TestProbeReportsPanickingTabler: a ProbeTabler panicking while a plain
// probe scans or a table probe filters is recovered and reported like a
// failing log format probe; the other formats are still answered.
func TestProbeReportsPanickingTabler(t *testing.T) {
	registerPanicFormat.Do(func() {
		err := logformat.RegisterParser(probeTestPanicFormat,
			func(string, string, int) (logformat.Parser, error) { return panickingTablerParser{}, nil })
		if err != nil {
			t.Fatalf("register %s: %v", probeTestPanicFormat, err)
		}
	})
	// The last line names ORDERS but is claimed by no format the matcher
	// asks before the panicking one, so the table probe of ORDERS asks it.
	file := writeProbeTestFile(t, t.TempDir(), "app.log",
		probeTestStatsLine("marker", "1"), probeTestOrdersLine(1), "custom ORDERS "+probeTestPanicLine)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	want := []protocol.ProbeError{{File: probeTestPanicFormat, Error: protocol.ProbeErrorFormat}}
	for table, answered := range map[string]string{"": "STATS", "ORDERS": "ORDERS"} {
		answer := handler.newProber().run(context.Background(),
			protocol.ProbeRequest{ID: "t", Files: []string{file}, Table: table})
		if !slices.Equal(answer.Errors, want) {
			t.Fatalf("table %q: errors = %+v, want %+v", table, answer.Errors, want)
		}
		if _, ok := answer.Formats["default"].Tables[answered]; !ok {
			t.Fatalf("table %q: answer = %+v, want the default format's %s", table, answer, answered)
		}
	}
}

// probeTestOrdersLine is a default format line of the table ORDERS.
func probeTestOrdersLine(id int) string {
	return fmt.Sprintf("INFO|20211002-071209|1|orders.go:10|8|13|7|0.21|1s|MAPREDUCE:ORDERS|orderID=%d|amount=7", id)
}

// writeRareTableFile writes lines STATS lines with an ORDERS line at each of
// the line numbers in orders (1-based) and returns the file's path.
func writeRareTableFile(t *testing.T, dir string, lines int, orders ...int) string {
	t.Helper()
	content := make([]string, 0, lines)
	for i := 1; i <= lines; i++ {
		if slices.Contains(orders, i) {
			content = append(content, probeTestOrdersLine(i))
			continue
		}
		content = append(content, probeTestStatsLine("currentConnections", fmt.Sprint(i)))
	}
	return writeProbeTestFile(t, dir, "app.log", content...)
}

// TestTableProbeFindsARareTable: ORDERS logs 3 times in 20,000 lines (about
// 2.2 MB), last 5,000 lines (about 550 KB) before the end, so a plain probe
// (last 256 KiB) does not see it; a probe targeted at ORDERS scans 4 MiB
// and answers ORDERS only, with its fields and the $variables of its lines.
func TestTableProbeFindsARareTable(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 20000, 5000, 10000, 15000)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)

	plain := handler.newProber().run(context.Background(), protocol.ProbeRequest{ID: "p", Files: []string{file}})
	if _, ok := plain.Formats["default"].Tables["ORDERS"]; ok {
		t.Fatalf("plain probe found ORDERS, the test file must hide it from the default window")
	}

	answer := handler.newProber().run(context.Background(),
		protocol.ProbeRequest{ID: "t", Files: []string{file}, Table: "ORDERS"})
	if answer.Table != "ORDERS" || len(answer.Errors) != 0 || answer.FilesSampled != 1 {
		t.Fatalf("answer = %+v, want ORDERS answered without errors", answer)
	}
	if answer.LinesSampled != 3 {
		t.Errorf("lines sampled = %d, want the 3 ORDERS lines", answer.LinesSampled)
	}
	format := answer.Formats["default"]
	if got := sortedKeys(format.Tables); !slices.Equal(got, []string{"ORDERS"}) {
		t.Fatalf("tables = %v, want [ORDERS] only", got)
	}
	if got := format.Tables["ORDERS"]["orderID"]; !slices.Equal(got, []string{"5000", "10000", "15000"}) {
		t.Errorf("ORDERS orderID = %v", got)
	}
	if got := format.TableVariables["ORDERS"]["$caller"]; !slices.Equal(got, []string{"orders.go:10"}) {
		t.Errorf("ORDERS $caller = %v, want the caller of ORDERS lines only", got)
	}
	if format.Variables != nil {
		t.Errorf("format-wide variables = %v, want none in a targeted answer", format.Variables)
	}
	for name, other := range answer.Formats {
		if _, ok := other.Tables["ORDERS"]; !ok {
			t.Errorf("format %s answered without the table ORDERS: %+v", name, other)
		}
	}
}

// TestTableProbeOfAMissingTable: a table no line names gives no format, but
// the file counts as sampled (so the client knows it was looked for).
func TestTableProbeOfAMissingTable(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 100)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	answer := handler.newProber().run(context.Background(),
		protocol.ProbeRequest{ID: "t", Files: []string{file}, Table: "NOSUCHTABLE"})
	if answer.Table != "NOSUCHTABLE" || len(answer.Formats) != 0 || answer.FilesSampled != 1 ||
		answer.LinesSampled != 0 || len(answer.Errors) != 0 {
		t.Fatalf("answer = %+v, want one file sampled, no line, no format", answer)
	}
}

// TestTableProbeKeepsTheReadPermissionCheck: a targeted probe of a file the
// session user may not read samples nothing and answers the read reason,
// like a plain probe; neither the sampler nor the log formats run.
func TestTableProbeKeepsTheReadPermissionCheck(t *testing.T) {
	dir := realTempDir(t)
	allowed := writeProbeTestFile(t, filepath.Join(dir, "ok"), "a.log", probeTestOrdersLine(1))
	denied := writeProbeTestFile(t, filepath.Join(dir, "secret"), "b.log", probeTestOrdersLine(2))
	handler := newProbeTestHandler(t, []string{"readfiles:^" + filepath.Join(dir, "ok") + "/"}, nil)
	var sampled []string
	handler.probeOverrides.sampler = func(ctx context.Context, target fs.ValidatedReadTarget, path string,
		options fs.SampleOptions) (fs.Sample, error) {
		sampled = append(sampled, path)
		return fs.SampleFile(ctx, target, path, options)
	}

	answer := handler.newProber().run(context.Background(),
		protocol.ProbeRequest{ID: "t", Files: []string{allowed, denied}, Table: "ORDERS"})
	want := []protocol.ProbeError{{File: denied, Error: protocol.CommandFailurePermission}}
	if !slices.Equal(answer.Errors, want) {
		t.Fatalf("errors = %+v, want %+v", answer.Errors, want)
	}
	if !slices.Equal(sampled, []string{allowed}) {
		t.Fatalf("sampled %v, want only the permitted file", sampled)
	}
	if got := answer.Formats["default"].Tables["ORDERS"]["orderID"]; !slices.Equal(got, []string{"1"}) {
		t.Fatalf("ORDERS orderID = %v, want only the value of the permitted file", got)
	}
}

// TestTableProbeSampleBounds checks the bounds a targeted probe samples
// with: the larger scan window and journal entry count, the default limits
// of kept lines, and a line filter of the table; a plain probe keeps the
// default window and observes every line for its tables instead.
func TestTableProbeSampleBounds(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 10, 5)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	var got []fs.SampleOptions
	handler.probeOverrides.sampler = func(ctx context.Context, target fs.ValidatedReadTarget, path string,
		options fs.SampleOptions) (fs.Sample, error) {
		got = append(got, options)
		return fs.SampleFile(ctx, target, path, options)
	}
	prober := handler.newProber()
	prober.run(context.Background(), protocol.ProbeRequest{ID: "t", Files: []string{file}, Table: "ORDERS"})
	prober.run(context.Background(), protocol.ProbeRequest{ID: "p", Files: []string{file}})
	if len(got) != 2 {
		t.Fatalf("%d samples, want 2", len(got))
	}
	targeted, plain := got[0], got[1]
	if targeted.ScanBytes != probeTableScanBytes || targeted.ScanLines != probeTableScanLines ||
		targeted.WindowBytes != 0 || targeted.MaxLines != 0 || targeted.Keep == nil {
		t.Errorf("targeted options = %+v, want ScanBytes %d, ScanLines %d, default kept limits and a filter",
			targeted, probeTableScanBytes, probeTableScanLines)
	}
	if targeted.Keep([]byte(probeTestStatsLine("a", "1"))) || !targeted.Keep([]byte(probeTestOrdersLine(1))) {
		t.Error("the targeted filter does not keep exactly the ORDERS lines")
	}
	if plain.ScanBytes != 0 || plain.ScanLines != 0 || plain.Keep != nil || plain.Observe == nil {
		t.Errorf("plain options = %+v, want the default window, no filter and a table observer", plain)
	}
	if probeTableScanBytes > 16*fs.DefaultSampleWindowBytes {
		t.Errorf("probeTableScanBytes = %d, more than 16 default windows", probeTableScanBytes)
	}
}

// TestTableProbeRunsOneAtATime: a targeted probe sent through the command
// dispatch while a plain probe runs is answered busy, like any probe (one
// probe per session), and runs once the other one answered.
func TestTableProbeRunsOneAtATime(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 10, 5)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	started, release := make(chan string, 4), make(chan struct{})
	handler.probeOverrides.sampler = blockingProbeSampler(started, release)

	handler.handleCommand(encodedTestCommand(probeCommandFor(t, protocol.ProbeRequest{ID: "plain", Files: []string{file}})))
	waitForStarted(t, started)
	targeted := protocol.ProbeRequest{ID: "targeted", Files: []string{file}, Table: "ORDERS"}
	busy, _ := runProbe(t, handler, probeCommandFor(t, targeted))
	if busy.ID != "targeted" || busy.FilesSampled != 0 || !hasProbeError(busy, "", protocol.ProbeErrorBusy) {
		t.Fatalf("concurrent targeted probe answer = %+v, want busy", busy)
	}
	close(release)
	if plain, _ := readProbeAnswer(t, handler.serverMessages); plain.ID != "plain" {
		t.Fatalf("answer = %+v, want the plain probe's", plain)
	}
	handler.probeOverrides.sampler = nil
	answer, _ := runProbe(t, handler, probeCommandFor(t, targeted))
	if answer.ID != "targeted" || answer.Table != "ORDERS" || answer.Formats["default"].Tables["ORDERS"] == nil {
		t.Fatalf("targeted probe answer = %+v, want ORDERS", answer)
	}
}

// TestTableProbeRejectsInvalidRequests:an invalid table, or a table with a
// path listing, is answered invalid without sampling or listing anything.
func TestTableProbeRejectsInvalidRequests(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 10, 5)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	handler.probeOverrides.sampler = func(context.Context, fs.ValidatedReadTarget, string, fs.SampleOptions) (fs.Sample, error) {
		t.Error("an invalid request was sampled")
		return fs.Sample{}, nil
	}
	tests := map[string]protocol.ProbeRequest{
		"blank":          {ID: "t", Files: []string{file}, Table: "A B"},
		"control":        {ID: "t", Files: []string{file}, Table: "A\x1b"},
		"too long":       {ID: "t", Files: []string{file}, Table: strings.Repeat("T", protocol.ProbeMaxTableBytes+1)},
		"with a listing": {ID: "t", List: filepath.Dir(file) + "/", Table: "STATS"},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			answer := handler.newProber().run(context.Background(), request)
			want := []protocol.ProbeError{{Error: protocol.ProbeErrorInvalidRequest}}
			if !slices.Equal(answer.Errors, want) || answer.FilesSampled != 0 || len(answer.Entries) != 0 {
				t.Fatalf("answer = %+v, want only %+v", answer, want)
			}
		})
	}
}

// TestPlainProbeListsTheTablesOfTheWholeWindow: an ORDERS line inside the
// default 256 KiB window but before the last 500 lines the sample keeps is
// listed as a table (without fields), so the client can target it; a line
// before the window is not.
func TestPlainProbeListsTheTablesOfTheWholeWindow(t *testing.T) {
	// About 110 bytes a line: the last 256 KiB hold about 2,400 lines.
	file := writeRareTableFile(t, t.TempDir(), 5000, 100, 4000)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	answer := handler.newProber().run(context.Background(), protocol.ProbeRequest{ID: "p", Files: []string{file}})
	if answer.LinesSampled != fs.DefaultSampleMaxLines {
		t.Fatalf("lines sampled = %d, want %d", answer.LinesSampled, fs.DefaultSampleMaxLines)
	}
	format := answer.Formats["default"]
	orders, ok := format.Tables["ORDERS"]
	if !ok {
		t.Fatalf("tables = %v, want ORDERS from the window", sortedKeys(format.Tables))
	}
	if len(orders) != 0 {
		t.Errorf("ORDERS fields = %v, want none (its line was not kept)", orders)
	}
	// Per-table data stays consistent: ORDERS has an entry, empty as no
	// kept line of it filled a variable, and STATS only what its lines did.
	if variables, ok := format.TableVariables["ORDERS"]; !ok || len(variables) != 0 {
		t.Errorf("ORDERS table variables = %v, %v: want an empty entry", variables, ok)
	}
	if got := format.TableVariables["STATS"]["$caller"]; !slices.Equal(got, []string{"stats.go:56"}) {
		t.Errorf("STATS $caller = %v", got)
	}
	if got := format.Tables["STATS"]["currentConnections"]; len(got) == 0 {
		t.Errorf("STATS lost its fields: %v", format.Tables["STATS"])
	}
}

// TestPlainProbeAnswersTableVariables: a plain probe of lines of two tables
// answers the $variables of each table's lines.
func TestPlainProbeAnswersTableVariables(t *testing.T) {
	file := writeRareTableFile(t, t.TempDir(), 10, 5)
	handler := newProbeTestHandler(t, []string{"readfiles:.*"}, nil)
	answer := handler.newProber().run(context.Background(), protocol.ProbeRequest{ID: "p", Files: []string{file}})
	format := answer.Formats["default"]
	if got := format.TableVariables["ORDERS"]["$caller"]; !slices.Equal(got, []string{"orders.go:10"}) {
		t.Errorf("ORDERS $caller = %v", got)
	}
	if got := format.TableVariables["STATS"]["$caller"]; !slices.Equal(got, []string{"stats.go:56"}) {
		t.Errorf("STATS $caller = %v", got)
	}
	if got := format.Variables["$caller"]; len(got) != 2 {
		t.Errorf("format-wide $caller = %v, want both callers", got)
	}
}
