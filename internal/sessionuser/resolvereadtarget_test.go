package sessionuser

import (
	"errors"
	"os"
	osuser "os/user"
	"path/filepath"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
)

// TestResolveReadTargetErrors checks the errors ResolveReadTarget tells a read
// command why it can not read a path with: a path the permissions deny, one
// that does not exist and one that is no regular file are told apart, and the
// permissions are checked first.
func TestResolveReadTargetErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "app.log")
	if err := os.WriteFile(file, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "archive")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "current.log")
	if err := os.Symlink(filepath.Join(dir, "missing.log"), dangling); err != nil {
		t.Fatal(err)
	}
	current, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}

	scheduled := newTestUser(nil)
	scheduled.Name = config.ScheduleUser
	permitted := newTestUser([]string{"readfiles:^" + dir})
	permitted.Name = current.Username
	denied := newTestUser([]string{"readfiles:^/nothing-here/"})
	denied.Name = current.Username

	tests := []struct {
		name string
		user *User
		path string
		want error
	}{
		{name: "scheduled job reads a file", user: scheduled, path: file},
		{name: "scheduled job and a directory", user: scheduled, path: subdir, want: fs.ErrNotRegularFile},
		{name: "scheduled job and a missing file", user: scheduled, path: filepath.Join(dir, "missing.log"),
			want: os.ErrNotExist},
		{name: "scheduled job and a dangling symlink", user: scheduled, path: dangling, want: os.ErrNotExist},
		{name: "permitted user reads a file", user: permitted, path: file},
		{name: "permitted user and a directory", user: permitted, path: subdir, want: fs.ErrNotRegularFile},
		{name: "permitted user and a missing file", user: permitted, path: filepath.Join(dir, "missing.log"),
			want: ErrReadPermissionDenied},
		{name: "denied user and a file", user: denied, path: file, want: ErrReadPermissionDenied},
		{name: "denied user and a directory", user: denied, path: subdir, want: ErrReadPermissionDenied},
		{name: "denied journal", user: denied, path: "journal:ssh.service", want: ErrReadPermissionDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, err := tt.user.ResolveReadTarget(tt.path, "readfiles")
			if tt.want == nil {
				if err != nil || target.Kind != fs.FileKind {
					t.Fatalf("ResolveReadTarget(%s) = %+v, %v, want a file target", tt.path, target, err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ResolveReadTarget(%s) error = %v, want %v", tt.path, err, tt.want)
			}
			if !errors.Is(tt.want, ErrReadPermissionDenied) && errors.Is(err, ErrReadPermissionDenied) {
				t.Fatalf("ResolveReadTarget(%s) error = %v, must not be a denied permission", tt.path, err)
			}
		})
	}
}
