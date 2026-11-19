//go:build linux

package journal

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
)

func TestListUnits(t *testing.T) {
	long := strings.Repeat("x", 3*maxUnitLineBytes)
	tests := []struct {
		name          string
		lines         []string
		max           int
		want          []string
		wantTruncated bool
	}{
		{name: "all units", lines: []string{"ssh.service", "", "cron.service"}, max: 10,
			want: []string{"ssh.service", "cron.service"}},
		{name: "capped", lines: []string{"a.service", "b.service", "c.service"}, max: 2,
			want: []string{"a.service", "b.service"}, wantTruncated: true},
		{name: "exactly the cap", lines: []string{"a.service", "b.service"}, max: 2,
			want: []string{"a.service", "b.service"}},
		{name: "overlong line skipped", lines: []string{long, "a.service"}, max: 10,
			want: []string{"a.service"}},
		{name: "no units", max: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := journaltest.InstallMock(t, journaltest.Scenario{
				Default: journaltest.Invocation{Lines: tt.lines},
			})
			units, truncated, err := ListUnits(context.Background(), tt.max)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(units, tt.want) || truncated != tt.wantTruncated {
				t.Fatalf("ListUnits = %q, %v, want %q, %v", units, truncated, tt.want, tt.wantTruncated)
			}
			if args := strings.TrimSpace(mock.Args(t)); args != "--field _SYSTEMD_UNIT" {
				t.Fatalf("journalctl args = %q", args)
			}
		})
	}
}

func TestListUnitsFailures(t *testing.T) {
	t.Run("journalctl missing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, _, err := ListUnits(context.Background(), 10); !errors.Is(err, ErrJournalctlNotFound) {
			t.Fatalf("err = %v, want ErrJournalctlNotFound", err)
		}
	})
	t.Run("journalctl fails", func(t *testing.T) {
		journaltest.InstallMock(t, journaltest.Scenario{
			Default: journaltest.Invocation{Lines: []string{"a.service"}, ExitCode: 1},
		})
		if _, _, err := ListUnits(context.Background(), 10); err == nil {
			t.Fatal("expected an error from a failing journalctl")
		}
	})
	t.Run("context ends a hanging journalctl", func(t *testing.T) {
		journaltest.InstallMock(t, journaltest.Scenario{
			Default: journaltest.Invocation{Lines: []string{"a.service"}, HoldOpen: true},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, _, err := ListUnits(ctx, 10); err == nil {
			t.Fatal("expected an error once the context ended")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ListUnits returned after %v", elapsed)
		}
	})
}
