//go:build linux

package handlers

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	journaltest "github.com/mimecast/dtail/internal/io/journal/testhelper"
	"github.com/mimecast/dtail/internal/protocol"
)

// journalctlUnitRuns returns how often the journalctl mock listed the
// journal units (journalctl --field _SYSTEMD_UNIT).
func journalctlUnitRuns(t *testing.T, mock *journaltest.Mock) int {
	t.Helper()
	return strings.Count(mock.Args(t), "--field _SYSTEMD_UNIT")
}

// TestProbeListJournalUnitsCachedAcrossSessions checks that the journal:
// listings of several sessions of one dserver (one Dependencies.JournalUnits)
// run journalctl once, while each session still filters the units by its
// own user's read permissions; without the cache every listing ran it.
func TestProbeListJournalUnitsCachedAcrossSessions(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{Default: journaltest.Invocation{
		Lines: []string{"sshd.service", "app-web.service", "cron.service"},
	}})
	units := NewJournalUnitCache()
	for _, tt := range []struct {
		permissions []string
		want        []string
	}{
		{permissions: []string{`readfiles:^journal:app-`}, want: []string{"app-web.service"}},
		{permissions: []string{`readfiles:^journal:(sshd|cron)\.`}, want: []string{"cron.service", "sshd.service"}},
		{permissions: []string{`readfiles:^journal:`, `readfiles:!^journal:sshd`},
			want: []string{"app-web.service", "cron.service"}},
	} {
		handler := newProbeTestHandlerWithUnits(t, tt.permissions, nil, units, protocol.CapabilityJournalV1)
		answer := probeList(t, handler, "journal:")
		if got := entryNames(answer); !slices.Equal(got, tt.want) || len(answer.Errors) != 0 {
			t.Fatalf("permissions %q: listing %q (%+v), want %q", tt.permissions, got, answer, tt.want)
		}
	}
	if runs := journalctlUnitRuns(t, mock); runs != 1 {
		t.Fatalf("journalctl listed the units %d times for three sessions, want once", runs)
	}
}

// TestProbeListJournalUnitsUncachedWithoutCache checks the negative case:
// handlers without a shared cache (e.g. serverless mode) run journalctl
// for every listing, so the count above is the cache's doing.
func TestProbeListJournalUnitsUncachedWithoutCache(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{Default: journaltest.Invocation{
		Lines: []string{"app-web.service"},
	}})
	for range 2 {
		handler := newProbeTestHandler(t, []string{`readfiles:^journal:`}, nil, protocol.CapabilityJournalV1)
		if got := entryNames(probeList(t, handler, "journal:")); !slices.Equal(got, []string{"app-web.service"}) {
			t.Fatalf("listing %q", got)
		}
	}
	if runs := journalctlUnitRuns(t, mock); runs != 2 {
		t.Fatalf("journalctl listed the units %d times, want twice", runs)
	}
}

// TestProbeListJournalUnitsConcurrentSessions checks that sessions listing
// journal: at the same time share one journalctl run instead of starting
// one each (the mock is slow, so all requests arrive during the run).
func TestProbeListJournalUnitsConcurrentSessions(t *testing.T) {
	mock := journaltest.InstallMock(t, journaltest.Scenario{Default: journaltest.Invocation{
		Lines:          []string{"a.service", "b.service", "c.service"},
		InterLineDelay: 100 * time.Millisecond,
	}})
	units := NewJournalUnitCache()
	const sessions = 6
	var wg sync.WaitGroup
	results := make([][]string, sessions)
	for i := range sessions {
		handler := newProbeTestHandlerWithUnits(t, []string{`readfiles:^journal:`}, nil, units,
			protocol.CapabilityJournalV1)
		wg.Go(func() { results[i] = entryNames(probeList(t, handler, "journal:")) })
	}
	wg.Wait()
	for i, got := range results {
		if !slices.Equal(got, []string{"a.service", "b.service", "c.service"}) {
			t.Fatalf("session %d listed %q", i, got)
		}
	}
	if runs := journalctlUnitRuns(t, mock); runs != 1 {
		t.Fatalf("journalctl listed the units %d times for %d concurrent sessions, want once", runs, sessions)
	}
}
