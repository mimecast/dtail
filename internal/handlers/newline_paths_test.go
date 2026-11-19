package handlers

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
	userserver "github.com/mimecast/dtail/internal/sessionuser"
)

// TestNewlinePathsAreDeniedOnEveryRoute checks the newline denial of the
// read permission check (sessionuser.errNewlineInPath) at the handler: for
// rule sets that would otherwise permit them ("." and "$" do not see a
// newline, "(?s)" and "(?m)" rules match across one), no path holding a
// newline is readable, whether named literally, through a glob, base64
// encoded, through a symlink to a file or to a directory, or with a "..";
// a probe samples nothing of them and a path listing offers neither such
// a file, nor a directory with such a name, nor a symlink leading to one.
// A symlink whose own name holds a newline but leads to a readable file
// is offered: the check is made on the resolved path.
func TestNewlinePathsAreDeniedOnEveryRoute(t *testing.T) {
	dir := realTempDir(t)
	secret := filepath.Join(dir, "secret")
	line := probeTestStatsLine("foo", "SECRETVALUE")
	writeProbeTestFile(t, secret, "a\nb.log", line)
	writeProbeTestFile(t, secret, "a\nb c.log", line)
	writeProbeTestFile(t, filepath.Join(secret, "nl\ndir"), "c.log", line)
	writeProbeTestFile(t, filepath.Join(dir, "pub\ndir"), "d.log", line)
	writeProbeTestFile(t, dir, "app.log", probeTestStatsLine("foo", "PUBLIC"))
	if err := os.Symlink(filepath.Join(secret, "a\nb.log"), filepath.Join(dir, "link.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(secret, "nl\ndir"), filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "app.log"), filepath.Join(dir, "nl\nlink.log")); err != nil {
		t.Fatal(err)
	}
	quoted := "^" + regexp.QuoteMeta(dir) + "/"
	for name, perms := range map[string][]string{
		"denyform":  {"readfiles:" + quoted, "readfiles:!" + quoted + "secret/.*$"},
		"allowonly": {"readfiles:" + quoted},
		"dotall":    {"readfiles:(?s)" + quoted + ".*$"},
		"multiline": {"readfiles:(?m)^b\\.log$", "readfiles:(?m)^dir/c\\.log$", "readfiles:(?m)^b c\\.log$"},
	} {
		t.Run(name, func(t *testing.T) {
			handler := newProbeTestHandler(t, perms, nil)
			for _, path := range []string{
				filepath.Join(secret, "a\nb.log"), filepath.Join(secret, "a\nb c.log"),
				filepath.Join(secret, "nl\ndir", "c.log"), filepath.Join(dir, "pub\ndir", "d.log"),
				filepath.Join(dir, "link.log"), filepath.Join(dir, "linkdir", "c.log"),
				filepath.Join(dir, "secret/../secret/a\nb.log"),
			} {
				if _, err := handler.PrepareReadTarget(path); !errors.Is(err, userserver.ErrReadPermissionDenied) {
					t.Errorf("PrepareReadTarget(%q) = %v, want denied", path, err)
				}
				if _, err := handler.probeReadTarget(path); !errors.Is(err, userserver.ErrReadPermissionDenied) {
					t.Errorf("probeReadTarget(%q) = %v, want denied", path, err)
				}
			}
			// probes: literal, glob, base64-encoded, symlink
			for _, files := range [][]string{
				{filepath.Join(secret, "a\nb.log")},
				{filepath.Join(secret, "*")},
				{filepath.Join(secret, "*", "*")},
				{protocol.EncodeReadPath(filepath.Join(secret, "a\nb c.log"))},
				{filepath.Join(dir, "link.log")},
				{filepath.Join(dir, "linkdir", "*")},
				{filepath.Join(dir, "pub*", "*")},
			} {
				answer, raw := runProbe(t, handler, probeCommandFor(t, protocol.ProbeRequest{ID: "p", Files: files}))
				if answer.FilesSampled != 0 || answer.LinesSampled != 0 || strings.Contains(raw, "SECRET") || len(answer.Formats) != 0 {
					t.Errorf("probe %q sampled: %+v", files, answer)
				}
			}
			// listings
			for _, prefix := range []string{dir + "/", secret + "/", filepath.Join(secret, "nl\ndir") + "/",
				filepath.Join(dir, "pub\ndir") + "/", filepath.Join(dir, "linkdir") + "/", dir + "/pub", dir + "/l", dir + "/n"} {
				answer := probeList(t, handler, prefix)
				for _, e := range answer.Entries {
					if strings.Contains(e.Name, "\n") && e.Name != "nl\nlink.log" {
						t.Errorf("list %q offers %q", prefix, e.Name)
					}
					if e.Name == "linkdir" || e.Name == "link.log" || e.Name == "c.log" || e.Name == "d.log" {
						t.Errorf("list %q offers %q", prefix, e.Name)
					}
				}
			}
		})
	}
}
