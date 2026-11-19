package integrationtests

import (
	"encoding/csv"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDMapCSVQuotedRoundTrip(t *testing.T) {
	logger := NewTestLogger("TestDMapCSVQuotedRoundTrip")
	defer writeLogFileIgnoringError(logger)
	runDualModeTest(t, DualModeTest{
		Name: "TestDMapCSVQuotedRoundTrip",
		ServerlessTest: func(t *testing.T) {
			testDMapCSVQuotedRoundTrip(t, logger, false)
		},
		ServerTest: func(t *testing.T) {
			testDMapCSVQuotedRoundTrip(t, logger, true)
		},
	})
}

func testDMapCSVQuotedRoundTrip(t *testing.T, logger *TestLogger, withServer bool) {
	dir := t.TempDir()
	first := writeSameBaseNameCSV(t, dir, "a", "name,part\n\"a,b\",c\na,\"b,c\"\n\"Jane \"\"JJ\"\"\",\n")
	second := writeSameBaseNameCSV(t, dir, "b", "part,name\nc,\"a,b\"\nplain,\nspaces,Jane \nspaces,Jane\n")
	ctxTimeout, cancel := createTestContextWithTimeout(t)
	defer cancel()
	ctx := WithTestLogger(ctxTimeout, logger)
	var server *TestServer
	if withServer {
		server = NewTestServer(t)
		if err := server.Start("error"); err != nil {
			t.Fatal(err)
		}
		ctx = WithTestLogger(server.ctx, logger)
	}
	exported := filepath.Join(dir, "export.csv")
	reread := filepath.Join(dir, "reread.csv")
	for i, outfile := range []string{exported, reread} {
		files := []string{first, second}
		if i == 1 {
			files = []string{exported}
		}
		// Keep name last so its trailing space is emitted unquoted by the
		// standard CSV writer and must survive the complete reread pipeline.
		query := fmt.Sprintf("select count($line),part,name group by name,part logformat csv outfile %s", outfile)
		args := append([]string{"--query", query, "--cfg", "none"}, files...)
		if server != nil {
			commandArgs := NewCommandArgs()
			commandArgs.Servers = []string{server.Address()}
			commandArgs.TrustAllHosts = true
			commandArgs.NoColor = true
			commandArgs.Files = files
			commandArgs.ExtraArgs = []string{"--query", query}
			args = commandArgs.ToSlice()
		}
		if _, err := runCommand(ctx, t, filepath.Join(dir, fmt.Sprintf("stdout%d.txt", i)), "../dmap", args...); err != nil {
			t.Fatal(err)
		}
		want := [][]string{{"2", "c", "a,b"}, {"1", "b,c", "a"}, {"1", "", `Jane "JJ"`}, {"1", "plain", ""}, {"1", "spaces", "Jane "}, {"1", "spaces", "Jane"}}
		if i == 1 {
			want[0][0] = "1"
		}
		assertCSVQuotedRecords(t, outfile, []string{"count($line)", "part", "name"}, want)
	}
}

func assertCSVQuotedRecords(t *testing.T, path string, header []string, want [][]string) {
	t.Helper()
	rows, err := csv.NewReader(strings.NewReader(readTestFile(t, path))).ReadAll()
	if err != nil {
		t.Fatalf("invalid CSV %s: %v", path, err)
	}
	if len(rows) == 0 || !reflect.DeepEqual(rows[0], header) {
		t.Fatalf("header = %v, want %v", rows, header)
	}
	slices.SortFunc(rows[1:], slices.Compare[[]string])
	slices.SortFunc(want, slices.Compare[[]string])
	if !reflect.DeepEqual(rows[1:], want) {
		t.Fatalf("rows = %q, want %q", rows[1:], want)
	}
}
