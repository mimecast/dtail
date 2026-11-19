package jobs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients"
	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

type continuousFinalClient struct {
	backgroundClient
	started chan<- context.Context
}

type continuousDrainLogger struct {
	clientlog.NopLogger
	once    sync.Once
	drained chan struct{}
	release chan struct{}
}

// TestContinuousCancellationPublishesFinalInterval uses the job's factory and
// day-change hooks with a real MapReduce client. A finite in-process read supplies
// a known aggregate, then pauses the connection after it drained its output so
// cancellation happens before Start can publish the final interval.
func TestContinuousCancellationPublishesFinalInterval(t *testing.T) {
	for _, dayChange := range []bool{false, true} {
		name := "shutdown"
		if dayChange {
			name = "day change"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			logFile := filepath.Join(dir, "app.log")
			line := "INFO|1002-071143|1|stats.go:56|8|13|7|0.21|471h0m21s|MAPREDUCE:STATS|foo=1\n"
			if err := os.WriteFile(logFile, []byte(strings.Repeat(line, 7)), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := config.RuntimeConfig{
				Server: config.NewDefaultServerConfigForTest(),
				Client: &config.ClientConfig{},
				Common: &config.CommonConfig{HostnameOverride: "test-host"},
			}
			logger := &continuousDrainLogger{drained: make(chan struct{}), release: make(chan struct{})}
			loggers := clients.NewLoggerDependencies(logger, jobTestLoggers.Server, jobTestLoggers.Common)
			c := newContinuous(cfg, loggers)
			started := make(chan context.Context, 1)
			c.newMaprClient = func(args config.Args, mode clients.MaprClientMode) (backgroundClient, error) {
				if mode != clients.NonCumulativeMode || args.Mode != omode.TailClient {
					t.Errorf("continuous client mode = %v, operation = %v", mode, args.Mode)
				}
				args.Serverless = true
				args.Mode = omode.MapClient
				client, err := clients.NewMaprClient(args, cfg, mode, loggers)
				return continuousFinalClient{backgroundClient: client, started: started}, err
			}
			changed := make(chan struct{})
			c.dayChangeWatcher = func(ctx context.Context, _ time.Time) bool {
				select {
				case <-changed:
					return true
				case <-ctx.Done():
					return false
				}
			}
			job := config.Continuous{RestartOnDayChange: dayChange}
			job.Name = "final-interval"
			job.Files = logFile
			job.Query = "from STATS select count(foo) interval 3600"
			job.Outfile = filepath.Join(dir, "result.csv")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c.runJob(ctx, &job)
			}()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(logger.release) }) }
			defer release()
			var jobCtx context.Context
			select {
			case jobCtx = <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("continuous client did not start")
			}
			waitContinuousFinal(t, logger.drained, "connection did not drain its aggregate")
			if _, err := os.Stat(job.Outfile); !os.IsNotExist(err) {
				t.Fatalf("outfile exists before cancellation: %v", err)
			}
			if dayChange {
				close(changed)
			} else {
				cancel()
			}
			waitContinuousFinal(t, jobCtx.Done(), "job context was not canceled")
			release()
			waitContinuousFinal(t, done, "job did not finish after cancellation")
			assertJobFile(t, job.Outfile, "count(foo)\n7\n")
			assertJobFile(t, job.Outfile+".query", job.Query+" outfile "+job.Outfile)
		})
	}
}

func (c continuousFinalClient) Start(ctx context.Context, statsCh <-chan string) int {
	c.started <- ctx
	return c.backgroundClient.Start(ctx, statsCh)
}

func (l *continuousDrainLogger) Debug(args ...any) string {
	if len(args) > 1 && args[1] == "Decrementing connection stats" {
		l.once.Do(func() {
			close(l.drained)
			<-l.release
		})
	}
	return ""
}

func waitContinuousFinal(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}
