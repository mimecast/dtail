package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
	"github.com/mimecast/dtail/internal/regex"
	user "github.com/mimecast/dtail/internal/sessionuser"
)

// failureTestServer is a globCapTestServer whose read targets are resolved
// like those of a scheduled job's session, except for the denied paths and
// those with a target no reader can be created for (badKind).
type failureTestServer struct {
	*globCapTestServer
	denied  map[string]bool
	badKind map[string]bool
}

func newFailureTestServer(maxTargets int) *failureTestServer {
	return &failureTestServer{globCapTestServer: newGlobCapTestServer(maxTargets),
		denied: map[string]bool{}, badKind: map[string]bool{}}
}

func (s *failureTestServer) PrepareReadTarget(path string) (fs.ValidatedReadTarget, error) {
	switch {
	case s.denied[path]:
		return fs.ValidatedReadTarget{}, user.ErrReadPermissionDenied
	case s.badKind[path]:
		return fs.ValidatedReadTarget{Kind: fs.ReadTargetKind(99)}, nil
	}
	return (&user.User{Name: config.ScheduleUser}).ResolveReadTarget(path, "readfiles")
}

func (s *failureTestServer) readCommandDependencies() readCommandDependencies {
	dependencies := s.globCapTestServer.readCommandDependencies()
	dependencies.server = s
	return dependencies
}

// TestReadCommandReportsFailedReads runs one-shot read commands the way a
// scheduled mapreduce job's cat does and checks the hidden failed-command
// messages the client gets: exactly one, with the reason, for every way a
// read can fail, and none for a read that read all its files.
func TestReadCommandReportsFailedReads(t *testing.T) {
	tests := []struct {
		name string
		// setup creates the files in dir and returns the read's glob and the
		// paths PrepareReadTarget denies.
		setup      func(t *testing.T, dir string) (glob string, denied []string)
		args       func(glob string) []string
		maxTargets int
		// badKind are the files of dir that get a target no reader can be
		// created for.
		badKind []string
		want    string
	}{
		{
			name: "all files read",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				writeFailureTestFile(t, filepath.Join(dir, "b.log"), 0o600)
				return filepath.Join(dir, "*.log"), nil
			},
		},
		{
			name: "glob matches a directory besides the files",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				if err := os.Mkdir(filepath.Join(dir, "archive"), 0o700); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "*"), nil
			},
		},
		{
			name: "glob matches only directories",
			setup: func(t *testing.T, dir string) (string, []string) {
				for _, name := range []string{"archive", "old"} {
					if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				return filepath.Join(dir, "*"), nil
			},
			want: readFailureNoFile,
		},
		{
			name: "glob matches a dangling symlink",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				if err := os.Symlink(filepath.Join(dir, "gone.txt"), filepath.Join(dir, "current.log")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "*.log"), nil
			},
			want: readFailureNoFile,
		},
		{
			name: "no reader for a file",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				writeFailureTestFile(t, filepath.Join(dir, "b.log"), 0o600)
				return filepath.Join(dir, "*.log"), nil
			},
			badKind: []string{"b.log"},
			want:    readFailureReader,
		},
		{
			name: "no file matches",
			setup: func(_ *testing.T, dir string) (string, []string) {
				return filepath.Join(dir, "missing-*.log"), nil
			},
			want: readFailureNoFile,
		},
		{
			name: "no permission to read a file",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				writeFailureTestFile(t, filepath.Join(dir, "b.log"), 0o600)
				return filepath.Join(dir, "*.log"), []string{filepath.Join(dir, "b.log")}
			},
			want: readFailurePermission,
		},
		{
			name: "no permission to read two files reports once",
			setup: func(t *testing.T, dir string) (string, []string) {
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				writeFailureTestFile(t, filepath.Join(dir, "b.log"), 0o600)
				return filepath.Join(dir, "*.log"), []string{filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")}
			},
			want: readFailurePermission,
		},
		{
			name: "file can not be opened",
			setup: func(t *testing.T, dir string) (string, []string) {
				if os.Geteuid() == 0 {
					t.Skip("root opens files without read permission")
				}
				writeFailureTestFile(t, filepath.Join(dir, "a.log"), 0o600)
				writeFailureTestFile(t, filepath.Join(dir, "b.log"), 0o000)
				return filepath.Join(dir, "*.log"), nil
			},
			want: readFailureReadingFile,
		},
		{
			name: "more files than the server reads",
			setup: func(t *testing.T, dir string) (string, []string) {
				for _, name := range []string{"a.log", "b.log", "c.log"} {
					writeFailureTestFile(t, filepath.Join(dir, name), 0o600)
				}
				return filepath.Join(dir, "*.log"), nil
			},
			maxTargets: 2,
			want:       readFailureGlobCapped,
		},
		{
			name: "unparsable command",
			setup: func(_ *testing.T, dir string) (string, []string) {
				return filepath.Join(dir, "*.log"), nil
			},
			args: func(glob string) []string { return []string{"cat", glob} },
			want: readFailureCommand,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			glob, denied := tt.setup(t, dir)
			maxTargets := tt.maxTargets
			if maxTargets == 0 {
				maxTargets = 100
			}
			srv := newFailureTestServer(maxTargets)
			for _, path := range denied {
				srv.denied[path] = true
			}
			for _, name := range tt.badKind {
				srv.badKind[filepath.Join(dir, name)] = true
			}
			args := []string{"cat", glob, "."}
			if tt.args != nil {
				args = tt.args(glob)
			}

			command := newReadCommand(srv, omode.CatClient)
			command.Start(context.Background(), lcontext.LContext{}, len(args), args, 1)

			var want []string
			if tt.want != "" {
				want = []string{protocol.HiddenCommandFailedPrefix + tt.want + "\n"}
			}
			if got := failedCommandMessages(srv.serverMessage); !slices.Equal(got, want) {
				t.Fatalf("failed command messages = %q, want %q", got, want)
			}
		})
	}
}

// TestReadCommandReportsNoFailureOnceCanceled checks that a read canceled
// with its session sends no failure: the session ends without its close
// handshake then, which the client already takes as incomplete.
func TestReadCommandReportsNoFailureOnceCanceled(t *testing.T) {
	srv := newFailureTestServer(100)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := newReadCommand(srv, omode.CatClient)
	command.reportFailure(ctx, readFailureReadingFile)
	if got := failedCommandMessages(srv.serverMessage); len(got) != 0 {
		t.Fatalf("failed command messages = %q, want none", got)
	}
}

// Repeatedly exercise the completion boundary with real directory targets.
// A worker must count its skipped target before signaling the wait group:
// otherwise readFiles can return without reporting that it read no files.
func TestDirectoryOnlyReadReportsFailureBeforeReturning(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "archive"), filepath.Join(dir, "old")}
	for _, path := range paths {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{protocol.HiddenCommandFailedPrefix + readFailureNoFile + "\n"}
	for i := 0; i < 1000; i++ {
		srv := newFailureTestServer(100)
		command := newReadCommand(srv, omode.CatClient)
		command.readFiles(context.Background(), lcontext.LContext{}, paths,
			filepath.Join(dir, "*"), regex.NewNoop())
		if got := failedCommandMessages(srv.serverMessage); !slices.Equal(got, want) {
			t.Fatalf("iteration %d: failure messages = %q, want %q", i, got, want)
		}
		if pending, _ := srv.PendingAndActive(); pending != 0 {
			t.Fatalf("iteration %d: pending files = %d, want 0", i, pending)
		}
	}
}

// failedCommandMessages drains messages and returns the failed-command ones.
func failedCommandMessages(messages chan string) []string {
	var failed []string
	for {
		select {
		case message := <-messages:
			_, message = decodeGeneratedMessage(message)
			if strings.HasPrefix(message, protocol.HiddenCommandFailedPrefix) {
				failed = append(failed, message)
			}
		default:
			return failed
		}
	}
}

func writeFailureTestFile(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("line one\nline two\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// TestGroupReadReportsFailedReads checks that a read through its group's
// one-shot read reports a failed group read, and nothing when the group read
// succeeded or had already started (the session then reads privately).
func TestGroupReadReportsFailedReads(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{name: "group read succeeded"},
		{name: "group read already started", err: readhub.ErrGroupReadStarted},
		{name: "group read failed", err: errors.New("read failed"),
			want: []string{protocol.HiddenCommandFailedPrefix + readFailureReadingFile + "\n"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.log")
			writeFailureTestFile(t, path, 0o600)
			srv := newFailureTestServer(100)
			command := newReadCommandWithDependencies(srv.readCommandDependencies(), omode.CatClient, nil)
			groupReads := 0
			command.readGroup = func(context.Context, omode.Mode, readhub.Session, readhub.Group,
				readhub.SlotAcquirer) error {
				groupReads++
				return tt.err
			}
			target, err := srv.PrepareReadTarget(path)
			if err != nil {
				t.Fatalf("test setup: no target for %s: %v", path, err)
			}

			command.read(contextWithReadShare(context.Background(), "g:2"), lcontext.LContext{}, path, &target,
				"glob", regex.NewNoop())

			if groupReads != 1 {
				t.Fatalf("group reads = %d, want 1", groupReads)
			}
			if got := failedCommandMessages(srv.serverMessage); !slices.Equal(got, tt.want) {
				t.Fatalf("failed command messages = %q, want %q", got, tt.want)
			}
		})
	}
}
