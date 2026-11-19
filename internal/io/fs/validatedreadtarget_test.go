package fs

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

func TestValidatedCatFileStartReadsAllLines(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	target := mustValidatedReadTarget(t, filePath)
	re := regex.NewNoop()

	cat := mustNewReadFile(ReadOptions{
		Mode:           omode.CatClient,
		Target:         &target,
		FilePath:       filePath,
		GlobID:         "glob-id",
		ServerMessages: make(chan string, 1),
		MaxLineLength:  defaultMaxLineLength,
		Logger:         testLogger,
	})
	processor := &captureProcessor{}

	if err := cat.Start(
		context.Background(),
		lcontext.LContext{},
		processor,
		re,
	); err != nil {
		t.Fatalf("validated optimized reader start failed: %v", err)
	}

	want := []string{"alpha\n", "beta\n"}
	if !reflect.DeepEqual(processor.lines, want) {
		t.Fatalf("unexpected processed lines: got=%v want=%v", processor.lines, want)
	}
}

func TestValidatedReadTargetOpenRejectsEscapingSymlinkSwap(t *testing.T) {
	baseDir := t.TempDir()
	rootDir := filepath.Join(baseDir, "root")
	outsideDir := filepath.Join(baseDir, "outside")
	if err := os.MkdirAll(rootDir, 0755); err != nil {
		t.Fatalf("mkdir root dir: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0755); err != nil {
		t.Fatalf("mkdir outside dir: %v", err)
	}

	filePath := filepath.Join(rootDir, "app.log")
	if err := os.WriteFile(filePath, []byte("alpha\n"), 0600); err != nil {
		t.Fatalf("write app log: %v", err)
	}
	escapePath := filepath.Join(outsideDir, "secret.log")
	if err := os.WriteFile(escapePath, []byte("secret\n"), 0600); err != nil {
		t.Fatalf("write secret log: %v", err)
	}

	target := mustValidatedReadTarget(t, filePath)

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove app log: %v", err)
	}
	relativeEscape, err := filepath.Rel(rootDir, escapePath)
	if err != nil {
		t.Fatalf("relative escape path: %v", err)
	}
	if err := os.Symlink(relativeEscape, filePath); err != nil {
		t.Fatalf("symlink escape path: %v", err)
	}

	if _, err := target.Open(); err == nil {
		t.Fatal("expected rooted open to reject escaping symlink swap")
	}
}

func TestValidatedReadTargetOpenRejectsSameRootSymlinkSwap(t *testing.T) {
	baseDir := t.TempDir()
	filePath := filepath.Join(baseDir, "app.log")
	if err := os.WriteFile(filePath, []byte("alpha\n"), 0600); err != nil {
		t.Fatalf("write app log: %v", err)
	}
	otherPath := filepath.Join(baseDir, "other.log")
	if err := os.WriteFile(otherPath, []byte("other\n"), 0600); err != nil {
		t.Fatalf("write other log: %v", err)
	}

	target := mustValidatedReadTarget(t, filePath)

	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove app log: %v", err)
	}
	if err := os.Symlink(filepath.Base(otherPath), filePath); err != nil {
		t.Fatalf("symlink other log: %v", err)
	}

	_, err := target.Open()
	if err == nil {
		t.Fatal("expected rooted open to reject same-root symlink swap")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

func TestNewValidatedJournalTargetRejectsAmbiguousUnits(t *testing.T) {
	tests := []struct {
		name string
		spec string
	}{
		{
			name: "empty unit",
			spec: "journal:",
		},
		{
			name: "leading dash",
			spec: "journal:--output=json",
		},
		{
			name: "space",
			spec: "journal:ssh.service --output=json",
		},
		{
			name: "newline",
			spec: "journal:ssh.service\n--output=json",
		},
		{
			name: "tab",
			spec: "journal:ssh.service\t--output=json",
		},
		{
			name: "control character",
			spec: "journal:ssh.service\x00",
		},
		// journalctl -u treats a unit containing *, ? or [ as a glob over
		// the journal's units, so these read sshd.service while the
		// readfiles check saw another string.
		{name: "star glob", spec: "journal:ssh*.service"},
		{name: "question mark glob", spec: "journal:ssh?.service"},
		{name: "bracket glob", spec: "journal:ssh[d].service"},
		{name: "glob of every service", spec: "journal:*.service"},
		{name: "glob in unit type", spec: "journal:sshd.servi*"},
		// journalctl -u appends .service to a name without a unit type.
		{name: "implicit service suffix", spec: "journal:sshd"},
		{name: "unknown unit type", spec: "journal:sshd.log"},
		{name: "empty unit type", spec: "journal:sshd."},
		{name: "empty unit prefix", spec: "journal:.service"},
		// journalctl -u of a slice reads every unit in the slice.
		{name: "slice", spec: "journal:system.slice"},
		// A template names no unit of its own.
		{name: "template", spec: "journal:getty@.service"},
		{name: "leading at", spec: "journal:@tty1.service"},
		{name: "shell metacharacters", spec: "journal:ssh.service;touch$HOME`whoami`|cat"},
		{name: "slash", spec: "journal:../sshd.service"},
		{name: "non-ASCII letter", spec: "journal:sshé.service"},
		{name: "longer than systemd allows", spec: "journal:" + strings.Repeat("a", 248) + ".service"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewValidatedJournalTarget(tt.spec); err == nil {
				t.Fatalf("NewValidatedJournalTarget(%q) succeeded, want error", tt.spec)
			}
		})
	}
}

// TestNewValidatedJournalTargetAcceptsUnitNames checks unit names systemd
// takes as one literal unit: every unit type but slices, instances (whose
// instance part may contain '@'), \x escapes and the longest name.
func TestNewValidatedJournalTargetAcceptsUnitNames(t *testing.T) {
	for _, unit := range []string{
		"ssh.service",
		"NetworkManager.service",
		"dbus-broker.service",
		"systemd-journald.socket",
		"logrotate.timer",
		"session-3.scope",
		"init.scope",
		"var-log.mount",
		"proc-sys-fs-binfmt_misc.automount",
		"dev-sda1.swap",
		"multi-user.target",
		"cups.path",
		"dev-ttyS0.device",
		"user@1000.service",
		"getty@tty1.service",
		"wg-quick@a@b.service",
		`systemd-fsck@dev-disk-by\x2duuid-0f2a.service`,
		"host:name.service",
		"app.v2.service",
		strings.Repeat("a", 247) + ".service",
	} {
		spec := JournalSpecPrefix + unit
		target, err := NewValidatedJournalTarget(spec)
		if err != nil {
			t.Errorf("NewValidatedJournalTarget(%q) error = %v", spec, err)
			continue
		}
		if target.Kind != JournalKind || target.ResolvedPath() != spec {
			t.Errorf("NewValidatedJournalTarget(%q) = %+v", spec, target)
		}
	}
}

func TestValidatedTailFileTruncatedReopenDetectsTruncation(t *testing.T) {
	filePath := writeProcessorTestFile(t, "alpha\nbeta\n")
	target := mustValidatedReadTarget(t, filePath)

	tail := mustNewReadFile(ReadOptions{
		Mode:           omode.TailClient,
		Target:         &target,
		FilePath:       filePath,
		GlobID:         "glob-id",
		ServerMessages: make(chan string, 1),
		SeekEOF:        true,
		MaxLineLength:  defaultMaxLineLength,
		Logger:         testLogger,
	})
	fd, openErr := target.Open()
	if openErr != nil {
		t.Fatalf("open validated target: %v", openErr)
	}
	t.Cleanup(func() {
		if closeErr := fd.Close(); closeErr != nil {
			t.Errorf("close validated target: %v", closeErr)
		}
	})

	if _, err := fd.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("seek end: %v", err)
	}
	if err := os.Truncate(filePath, 1); err != nil {
		t.Fatalf("truncate file: %v", err)
	}

	isTruncated, err := tail.truncated(fd)
	if !isTruncated {
		t.Fatal("expected truncation to be detected")
	}
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("expected truncation error, got %v", err)
	}
}

func mustValidatedReadTarget(t *testing.T, path string) ValidatedReadTarget {
	t.Helper()

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}

	target, err := NewValidatedReadTarget(absolutePath)
	if err != nil {
		t.Fatalf("create validated target: %v", err)
	}

	return target
}
