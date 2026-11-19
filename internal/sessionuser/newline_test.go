package sessionuser

import (
	"errors"
	"os"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/fs"
)

// TestResolveReadTargetDeniesNewlinePaths checks the real read check with
// paths holding a newline. "." matches no newline and "$" only the end of
// the path, so the documented deny form "!^<dir>/.*$" does not match such a
// path, while an allow rule "^<dir>/" does: the deny rule was bypassed for
// every file whose name, or the name of a directory above it, held a
// newline. Such a path is denied to every user the permissions apply to,
// whatever the rules say, also when a symlink leads to it.
func TestResolveReadTargetDeniesNewlinePaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	secret := filepath.Join(dir, "secret")
	inNewlineDir := filepath.Join(secret, "sub\ndir", "c.log")
	if err := os.MkdirAll(filepath.Dir(inNewlineDir), 0o700); err != nil {
		t.Fatal(err)
	}
	newlineName := filepath.Join(secret, "a\nb.log")
	trailingNewline := filepath.Join(secret, "t.log\n")
	plain := filepath.Join(dir, "app.log")
	for _, file := range []string{newlineName, trailingNewline, inNewlineDir, plain} {
		if err := os.WriteFile(file, []byte("line\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(newlineName, link); err != nil {
		t.Fatal(err)
	}
	current, err := osuser.Current()
	if err != nil {
		t.Fatal(err)
	}
	quoted := "^" + regexp.QuoteMeta(dir) + "/"

	denyForm := newTestUser([]string{"readfiles:" + quoted, "readfiles:!" + quoted + "secret/.*$"})
	denyForm.Name = current.Username
	allowOnly := newTestUser([]string{"readfiles:" + quoted})
	allowOnly.Name = current.Username
	scheduled := newTestUser(nil)
	scheduled.Name = config.ScheduleUser

	tests := []struct {
		name string
		user *User
		path string
		want error
	}{
		{name: "deny form, newline in the file name", user: denyForm, path: newlineName,
			want: ErrReadPermissionDenied},
		{name: "deny form, trailing newline", user: denyForm, path: trailingNewline, want: ErrReadPermissionDenied},
		{name: "deny form, newline in a directory name", user: denyForm, path: inNewlineDir,
			want: ErrReadPermissionDenied},
		{name: "deny form, symlink to a newline path", user: denyForm, path: link, want: ErrReadPermissionDenied},
		{name: "deny form, plain file beside", user: denyForm, path: plain},
		{name: "allow rule alone, newline in the file name", user: allowOnly, path: newlineName,
			want: ErrReadPermissionDenied},
		{name: "allow rule alone, plain file", user: allowOnly, path: plain},
		{name: "scheduled job reads a newline path", user: scheduled, path: newlineName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			target, err := tt.user.ResolveReadTarget(tt.path, "readfiles")
			if tt.want == nil {
				if err != nil || target.Kind != fs.FileKind {
					t.Fatalf("ResolveReadTarget(%q) = %+v, %v, want a file target", tt.path, target, err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("ResolveReadTarget(%q) error = %v, want %v", tt.path, err, tt.want)
			}
		})
	}
}

// TestNewlinePathsAreNeverPermitted checks the rule check itself and the
// path listing's directory rule: no rule permits a path with a newline, not
// one that matches it either ("(?s)", a class, "(?m)" anchors), a journal
// target with one is denied, and a directory with one is not offered.
func TestNewlinePathsAreNeverPermitted(t *testing.T) {
	t.Parallel()
	rules := [][]string{
		{"readfiles:^/var/log/"},
		{"readfiles:(?s)^/var/log/.*$"},
		{"readfiles:^/var/log/[^/]*$"},
		{"readfiles:(?m)^/var/log/app\\.log$"},
		{"readfiles:^journal:", "readfiles:/var/log/"},
	}
	paths := []string{"/var/log/a\nb", "/var/log/a\n", "/etc/x\n/var/log/app.log", "journal:a\n/var/log/.service"}
	for _, perms := range rules {
		u := newTestUser(perms)
		for _, path := range paths {
			permitted, err := u.iteratePaths(path, "readfiles", u.checkLog(false))
			if permitted || !errors.Is(err, errNewlineInPath) {
				t.Errorf("rules %q: iteratePaths(%q) = %v, %v, want false and errNewlineInPath",
					perms, path, permitted, err)
			}
		}
		if u.PathRules("readfiles").MayReadBeneath("/var/log/a\nb") {
			t.Errorf("rules %q: a directory with a newline in its path is offered", perms)
		}
	}
	u := newTestUser(rules[0])
	permitted, err := u.iteratePaths("/var/log/a b\tc\r.log", "readfiles", u.checkLog(false))
	if !permitted || err != nil {
		t.Errorf("a path with other blanks = %v, %v, want permitted", permitted, err)
	}
	if !u.PathRules("readfiles").MayReadBeneath("/var/log/a b") {
		t.Error("a directory without a newline is not offered")
	}
}
