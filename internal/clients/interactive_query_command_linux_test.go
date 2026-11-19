//go:build linux

package clients

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestQueryAndTailCommandsOnPTY types :query and :tail on the pseudo
// terminal of an interactive dtail in serverless mode (the client of
// runSwitchE2E, framed and direct): the session goes from following lines
// to a query (its result printed), back to following lines with another
// regex, and to another query, each answered on the TTY, without a new
// session.
func TestQueryAndTailCommandsOnPTY(t *testing.T) {
	for _, framed := range []bool{false, true} {
		name := "direct"
		if framed {
			name = "framed"
		}
		t.Run(name, func(t *testing.T) {
			runQueryTailCommandsOnPTY(t, framed)
		})
	}
}

func runQueryTailCommandsOnPTY(t *testing.T, framed bool) {
	master, ttyPath := openTestPTY(t)
	file := filepath.Join(t.TempDir(), "commands.log")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c, logger, direct := newSwitchE2EClient(t, file, ttyPath, framed)
	payload := func() string {
		if framed {
			return strings.Join(logger.writtenLines(), "")
		}
		return direct.String()
	}
	results := func() string { return strings.Join(logger.writtenLines(), "") }
	stopFeeder := feedSwitchE2ELines(t, file)
	defer stopFeeder()

	done := make(chan int, 1)
	go func() { done <- c.Start(context.Background(), nil) }()
	waitForPTYOutput(t, master, "Interactive query control enabled", done)
	waitForSwitchE2E(t, "PLAIN line", payload)

	typeControlLine(t, master, ":query "+switchE2EQuery)
	waitForPTYOutput(t, master, "query applied", done)
	waitForSwitchE2E(t, "count($line)", results)
	typeControlLine(t, master, ":show")
	waitForPTYOutput(t, master, "workload=query", done)

	typeControlLine(t, master, ":tail WARN")
	waitForPTYOutput(t, master, "following lines", done)
	waitForSwitchE2E(t, "WARN line", payload)

	typeControlLine(t, master, ":query from STATS select max(currentConnections) group by $hostname interval 1")
	waitForPTYOutput(t, master, "query applied", done)
	waitForSwitchE2E(t, "max(currentConnections)", results)

	typeControlLine(t, master, ":quit")
	select {
	case status := <-done:
		if status != 0 {
			t.Fatalf("Start status = %d, want 0", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("interactive dtail did not end")
	}
}

// typeControlLine types line and Enter on the pseudo terminal master.
func typeControlLine(t *testing.T, master *os.File, line string) {
	t.Helper()
	if _, err := master.WriteString(line + "\n"); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}
}
