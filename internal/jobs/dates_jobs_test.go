package jobs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/config"
)

func TestJobCalendarDateExpansion(t *testing.T) {
	sofia, err := time.LoadLocation("Europe/Sofia")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                string
		now                 time.Time
		yesterday, tomorrow string
	}{
		{"spring", time.Date(2026, 3, 30, 0, 30, 0, 0, sofia), "20260329", "20260331"},
		{"autumn", time.Date(2026, 10, 25, 0, 30, 0, 0, sofia), "20261024", "20261026"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outdir := t.TempDir()
			b := newBackoffTestScheduler(t, filepath.Join(outdir, "$tomorrow.csv"), tt.now)
			job := &b.cfg.Server.Schedule[0]
			job.Files = "/logs/$yesterday.log"
			wantFiles := "/logs/" + tt.yesterday + ".log"
			wantQuery := job.Query + " outfile " + filepath.Join(outdir, tt.tomorrow+".csv")
			checkArgs := func(t *testing.T, args config.Args) {
				t.Helper()
				if args.What != wantFiles || args.QueryStr != wantQuery {
					t.Fatalf("job arguments = %q, %q; want %q, %q", args.What, args.QueryStr, wantFiles, wantQuery)
				}
			}
			t.Run("scheduled and frozen final retry", func(t *testing.T) {
				b.statuses = []int{1, 0}
				newClient := b.newMaprClient
				b.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
					checkArgs(t, args)
					return newClient(args, mode)
				}
				b.runJobs(context.Background())
				b.now = time.Date(tt.now.Year(), tt.now.Month(), tt.now.Day()+1, 0, 0, 0, 0, sofia)
				b.runFinalJobs(context.Background(), newGroupLimits())
				if len(b.modes) != 2 || b.modes[0] != clients.ScheduledMode || b.modes[1] != clients.ScheduledPartialMode {
					t.Fatalf("client modes = %v, want scheduled followed by final", b.modes)
				}
			})
			t.Run("continuous uses one clock reading", func(t *testing.T) {
				c := newContinuous(b.cfg, jobTestLoggers)
				calls := 0
				c.now = func() time.Time {
					calls++
					// Another reading would be on a different date.
					return tt.now.AddDate(0, 0, calls-1)
				}
				created := false
				c.newMaprClient = func(args config.Args, _ clients.MaprClientMode) (backgroundClient, error) {
					checkArgs(t, args)
					created = true
					return immediateBackgroundClient{}, nil
				}
				continuousJob := config.Continuous{}
				continuousJob.Files, continuousJob.Outfile, continuousJob.Query = job.Files, job.Outfile, job.Query
				c.runJob(context.Background(), &continuousJob)
				if !created || calls != 1 {
					t.Fatalf("created client = %v, clock calls = %d; want true, 1", created, calls)
				}
			})
		})
	}
}
