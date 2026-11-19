package journaltest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	journalctlCommand = "journalctl"

	// TermSentinel is written to stderr when the mock receives SIGTERM.
	TermSentinel = "journalctl mock received SIGTERM"
)

// Invocation describes one journalctl mock execution scenario.
type Invocation struct {
	Lines          []string
	FollowLines    []string
	Stderr         []string
	ExitCode       int
	FailFirst      int
	FailExitCode   int
	NoEntries      bool
	PartialLine    string
	LongLineLength int
	InterLineDelay time.Duration
	HoldOpen       bool
	IgnoreSIGTERM  bool
}

// Scenario configures the journalctl mock. Unit-specific invocations are chosen
// from the parsed "-u UNIT" flag; Default is used when no unit matches.
type Scenario struct {
	Default Invocation
	Units   map[string]Invocation
}

// Mock describes an installed journalctl mock and its recorded state files.
type Mock struct {
	BinDir     string
	StateDir   string
	Path       string
	ArgsFile   string
	UnitFile   string
	FollowFile string
	CountFile  string
	OutputFile string
	TermFile   string
	PIDFile    string
}

// InstallMock installs a journalctl shell-script mock into PATH for the test.
func InstallMock(t testing.TB, scenario Scenario) *Mock {
	t.Helper()

	rootDir := t.TempDir()
	binDir := filepath.Join(rootDir, "bin")
	stateDir := filepath.Join(rootDir, "state")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatalf("create journalctl mock bin dir: %v", err)
	}
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("create journalctl mock state dir: %v", err)
	}

	paths := writeInvocations(t, rootDir, scenario)
	mock := &Mock{
		BinDir:     binDir,
		StateDir:   stateDir,
		Path:       binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		ArgsFile:   filepath.Join(stateDir, "journalctl.args"),
		UnitFile:   filepath.Join(stateDir, "journalctl.units"),
		FollowFile: filepath.Join(stateDir, "journalctl.follow"),
		CountFile:  filepath.Join(stateDir, "journalctl.counts"),
		OutputFile: filepath.Join(stateDir, "journalctl.outputs"),
		TermFile:   filepath.Join(stateDir, "journalctl.term"),
		PIDFile:    filepath.Join(stateDir, "journalctl.pid"),
	}

	scriptPath := filepath.Join(binDir, journalctlCommand)
	if err := os.WriteFile(scriptPath, []byte(mockScript(mock, paths)), 0o700); err != nil {
		t.Fatalf("write journalctl mock script: %v", err)
	}
	t.Setenv("PATH", mock.Path)

	return mock
}

// Env returns environment variables needed by child processes that override PATH.
func (m *Mock) Env() map[string]string {
	return map[string]string{
		"PATH": m.Path,
	}
}

// Args returns all recorded journalctl argument lines.
func (m *Mock) Args(t testing.TB) string {
	t.Helper()
	return readOptionalFile(t, m.ArgsFile)
}

// Terminated reports whether the mock observed SIGTERM.
func (m *Mock) Terminated(t testing.TB) bool {
	t.Helper()
	return strings.TrimSpace(readOptionalFile(t, m.TermFile)) != ""
}

// WaitForTerm waits until the mock records SIGTERM.
func (m *Mock) WaitForTerm(t testing.TB, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for {
		if m.Terminated(t) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("journalctl mock did not observe SIGTERM before timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

type invocationPaths struct {
	defaultDir string
	unitDirs   map[string]string
}

func writeInvocations(t testing.TB, rootDir string, scenario Scenario) invocationPaths {
	t.Helper()

	paths := invocationPaths{
		defaultDir: filepath.Join(rootDir, "scenario-default"),
		unitDirs:   make(map[string]string, len(scenario.Units)),
	}
	writeInvocation(t, paths.defaultDir, scenario.Default)

	i := 0
	for unit, invocation := range scenario.Units {
		dir := filepath.Join(rootDir, fmt.Sprintf("scenario-unit-%d", i))
		writeInvocation(t, dir, invocation)
		paths.unitDirs[unit] = dir
		i++
	}

	return paths
}

func writeInvocation(t testing.TB, dir string, invocation Invocation) {
	t.Helper()

	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create journalctl mock scenario dir: %v", err)
	}
	writeLinesFile(t, filepath.Join(dir, "stdout"), invocation.Lines, true)
	writeLinesFile(t, filepath.Join(dir, "follow"), invocation.FollowLines, true)
	writeLinesFile(t, filepath.Join(dir, "stderr"), stderrLines(invocation), true)
	writeLinesFile(t, filepath.Join(dir, "partial"), []string{invocation.PartialLine}, false)

	if invocation.LongLineLength > 0 {
		longLine := strings.Repeat("x", invocation.LongLineLength)
		writeLinesFile(t, filepath.Join(dir, "long"), []string{longLine}, true)
	}

	config := []string{
		fmt.Sprintf("exit_code=%d", invocation.ExitCode),
		fmt.Sprintf("fail_first=%d", invocation.FailFirst),
		fmt.Sprintf("fail_exit_code=%d", positiveExitCode(invocation.FailExitCode)),
		fmt.Sprintf("delay=%s", shellDelay(invocation.InterLineDelay)),
		fmt.Sprintf("hold_open=%d", boolInt(invocation.HoldOpen)),
		fmt.Sprintf("ignore_term=%d", boolInt(invocation.IgnoreSIGTERM)),
	}
	if err := os.WriteFile(filepath.Join(dir, "config"), []byte(strings.Join(config, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write journalctl mock scenario config: %v", err)
	}
}

func stderrLines(invocation Invocation) []string {
	lines := append([]string(nil), invocation.Stderr...)
	if invocation.NoEntries {
		lines = append(lines, "-- No entries --")
	}
	return lines
}

func writeLinesFile(t testing.TB, path string, lines []string, newline bool) {
	t.Helper()

	var b strings.Builder
	for _, line := range lines {
		b.WriteString(strings.TrimSuffix(line, "\n"))
		if newline {
			b.WriteByte('\n')
		}
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write journalctl mock fixture %s: %v", path, err)
	}
}

func shellDelay(delay time.Duration) string {
	if delay <= 0 {
		return ""
	}
	return fmt.Sprintf("%.3f", delay.Seconds())
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func positiveExitCode(value int) int {
	if value > 0 {
		return value
	}
	return 1
}

func mockScript(mock *Mock, paths invocationPaths) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("args_file=" + shellQuote(mock.ArgsFile) + "\n")
	b.WriteString("unit_file=" + shellQuote(mock.UnitFile) + "\n")
	b.WriteString("follow_file=" + shellQuote(mock.FollowFile) + "\n")
	b.WriteString("count_file=" + shellQuote(mock.CountFile) + "\n")
	b.WriteString("output_file=" + shellQuote(mock.OutputFile) + "\n")
	b.WriteString("term_file=" + shellQuote(mock.TermFile) + "\n")
	b.WriteString("pid_file=" + shellQuote(mock.PIDFile) + "\n")
	b.WriteString("term_sentinel=" + shellQuote(TermSentinel) + "\n")
	b.WriteString(scriptBody(paths))
	return b.String()
}

func scriptBody(paths invocationPaths) string {
	var b strings.Builder
	b.WriteString(`
original_args="$*"
unit=""
follow=0
count=""
output=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    -u)
      shift
      unit="$1"
      ;;
    -f)
      follow=1
      ;;
    -n)
      shift
      count="$1"
      ;;
    --output=*)
      output=${1#--output=}
      ;;
    --output)
      shift
      output="$1"
      ;;
  esac
  [ "$#" -gt 0 ] && shift
done

printf '%s\n' "$$" > "$pid_file"
printf '%s\n' "$original_args" >> "$args_file"
printf '%s\n' "$unit" >> "$unit_file"
printf '%s\n' "$follow" >> "$follow_file"
printf '%s\n' "$count" >> "$count_file"
printf '%s\n' "$output" >> "$output_file"

scenario_dir=`)
	b.WriteString(shellQuote(paths.defaultDir))
	b.WriteString(`
case "$unit" in
`)
	for unit, dir := range paths.unitDirs {
		b.WriteString("  ")
		b.WriteString(shellQuote(unit))
		b.WriteString(") scenario_dir=")
		b.WriteString(shellQuote(dir))
		b.WriteString(" ;;\n")
	}
	b.WriteString(`esac

. "$scenario_dir/config"

invocation_file="$scenario_dir/invocations"
invocations=0
if [ -f "$invocation_file" ]; then
  invocations=$(cat "$invocation_file")
fi
invocations=$((invocations + 1))
printf '%s\n' "$invocations" > "$invocation_file"

sleep_pid=""

on_term() {
  printf '%s\n' "$term_sentinel" >&2
  printf 'term' > "$term_file"
  if [ -n "$sleep_pid" ]; then
    kill "$sleep_pid" 2>/dev/null || true
  fi
  if [ "$ignore_term" != "1" ]; then
    exit 0
  fi
}

trap on_term TERM

if [ "$fail_first" -gt 0 ] && [ "$invocations" -le "$fail_first" ]; then
  exit "$fail_exit_code"
fi

interruptible_sleep() {
  sleep "$1" &
  sleep_pid=$!
  wait "$sleep_pid" 2>/dev/null || true
  sleep_pid=""
}

sleep_delay() {
  if [ -n "$delay" ]; then
    interruptible_sleep "$delay"
  fi
}

emit_lines() {
  file=$1
  [ -s "$file" ] || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    printf '%s\n' "$line"
    sleep_delay
  done < "$file"
}

emit_raw() {
  file=$1
  [ -s "$file" ] || return 0
  cat "$file"
  sleep_delay
}

emit_lines "$scenario_dir/stderr" >&2
emit_lines "$scenario_dir/stdout"
emit_lines "$scenario_dir/long"
emit_raw "$scenario_dir/partial"

if [ "$follow" = "1" ]; then
  while :; do
    emit_lines "$scenario_dir/follow"
    if [ ! -s "$scenario_dir/follow" ]; then
      if [ -n "$delay" ]; then
        sleep_delay
      else
        interruptible_sleep 0.05
      fi
    fi
  done
fi

if [ "$hold_open" = "1" ]; then
  while :; do
    interruptible_sleep 0.05
  done
fi

exit "$exit_code"
`)
	return b.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func readOptionalFile(t testing.TB, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
