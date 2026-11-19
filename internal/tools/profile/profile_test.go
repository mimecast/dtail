package profile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesByNewestModTimeReturnsStatError(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	brokenLink := filepath.Join(dir, "dcat_cpu_broken.prof")
	if err := os.Symlink(missing, brokenLink); err != nil {
		t.Fatalf("create broken profile symlink: %v", err)
	}

	_, err := filesByNewestModTime(filepath.Join(dir, "dcat_cpu_*.prof"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("filesByNewestModTime error = %v, want not-exist error", err)
	}
}

func TestProfileDirFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "explicit profile dir",
			args: []string{"-profile", "-profiledir", "custom-profiles", "-plain"},
			want: "custom-profiles",
		},
		{
			name: "missing profile dir falls back to default",
			args: []string{"-profile", "-plain"},
			want: "profiles",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profileDirFromArgs(tt.args); got != tt.want {
				t.Fatalf("profileDirFromArgs(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
