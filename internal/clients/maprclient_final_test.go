package clients

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
)

type finalTestConnector struct {
	retryTestConnector
	run func(context.Context)
}

type reportHookLogger struct {
	clientlog.NopLogger
	debug  func(string)
	errors atomic.Int32
}

func TestMaprClientStartPublishesDrainedFinalInterval(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("append=%v/canceled=%v", appendMode, canceled), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					outfile := filepath.Join(t.TempDir(), "result.csv")
					query := outfileTestQuery(t, outfile, appendMode)
					query.Interval = time.Hour
					c := newOutfileTestClient(query, NonCumulativeMode)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					end := make(chan struct{})
					configureFinalTestConnection(c, func(ctx context.Context) {
						select {
						case <-end:
						case <-ctx.Done():
						}
						// Model data already received that is drained before the
						// connection returns, even after cancellation.
						mergeOutfileTestRow(t, c.session.Snapshot(), 7)
					})
					done := make(chan int, 1)
					go func() { done <- c.Start(ctx, nil) }()
					synctest.Wait()
					if canceled {
						cancel()
					} else {
						close(end)
					}
					if status := <-done; status != 0 {
						t.Fatalf("Start status = %d", status)
					}
					assertScheduledTestFile(t, outfile, "count(foo)\n7\n")
					assertScheduledTestFile(t, outfile+".query", query.RawQuery)
					if !c.session.Snapshot().GlobalGroup.IsEmpty() {
						t.Fatal("final interval was not consumed")
					}
				})
			})
		}
	}
}

func TestMaprClientStartJoinsOverlappingReport(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("append=%v/canceled=%v", appendMode, canceled), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					outfile := filepath.Join(t.TempDir(), "result.csv")
					query := outfileTestQuery(t, outfile, appendMode)
					query.Interval = 2 * time.Second
					c := newOutfileTestClient(query, NonCumulativeMode)
					blocked, release := make(chan struct{}), make(chan struct{})
					var writes, reports atomic.Int32
					logger := &reportHookLogger{debug: func(message string) {
						if message == "Gathering interim mapreduce result" {
							reports.Add(1)
						}
						// The aggregate has been detached, but neither the query
						// file nor the outfile has been written yet.
						if message == "Writing query file" && writes.Add(1) == 1 {
							close(blocked)
							<-release
						}
					}}
					c.loggers.Client = logger
					c.session = maprclient.NewSessionState(query, logger)
					mergeOutfileTestRow(t, c.session.Snapshot(), 7)
					end := make(chan struct{})
					configureFinalTestConnection(c, func(ctx context.Context) {
						select {
						case <-end:
						case <-ctx.Done():
						}
					})
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					done := make(chan int, 1)
					go func() { done <- c.Start(ctx, nil) }()
					<-blocked // Fake time advances to the first report tick.
					mergeOutfileTestRow(t, c.session.Snapshot(), 9)
					if canceled {
						cancel()
					} else {
						close(end)
					}
					synctest.Wait()
					select {
					case status := <-done:
						t.Errorf("Start returned %d while the report was still writing", status)
					default:
					}
					close(release)
					synctest.Wait()
					select {
					case status := <-done:
						if status != 0 {
							t.Errorf("Start status = %d", status)
						}
					default:
						t.Fatal("Start did not return after report completion")
					}
					want := "count(foo)\n9\n"
					if appendMode {
						want = "count(foo)\n7\n9\n"
					}
					assertScheduledTestFile(t, outfile, want)
					if !c.session.Snapshot().GlobalGroup.IsEmpty() {
						t.Fatal("final interval was not consumed")
					}
					time.Sleep(10 * time.Second)
					synctest.Wait()
					if got := reports.Load(); got != 1 {
						t.Fatalf("interim reports = %d, want 1; reporter outlived Start", got)
					}
					assertScheduledTestFile(t, outfile, want)
				})
			})
		}
	}
}

func TestMaprClientFinalIntervalEmptyAndGenerationPolicy(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		for _, nextGeneration := range []bool{false, true} {
			t.Run(fmt.Sprintf("append=%v/new-generation=%v", appendMode, nextGeneration), func(t *testing.T) {
				outfile := filepath.Join(t.TempDir(), "result.csv")
				q := outfileTestQuery(t, outfile, appendMode)
				c := newOutfileTestClient(q, NonCumulativeMode)
				mergeOutfileTestRow(t, c.session.Snapshot(), 7)
				if err := c.reportResults(false); err != nil {
					t.Fatal(err)
				}
				want := "count(foo)\n7\n"
				if nextGeneration {
					retained := c.session.Snapshot()
					mergeOutfileTestRow(t, retained, 99)
					if _, err := c.session.CommitQuery(q.RawQuery, 1); err != nil {
						t.Fatal(err)
					}
					// Final publication must use this generation, including its
					// empty-interval policy, and reject a retained older snapshot.
					if !appendMode {
						want = "count(foo)\n"
					}
					if err := c.writeResultsToOutfile(retained, true); err != nil {
						t.Fatal(err)
					}
				}
				if status := c.finish(context.Canceled, 0); status != 0 {
					t.Fatalf("finish status = %d", status)
				}
				assertScheduledTestFile(t, outfile, want)
			})
		}
	}
}

func TestMaprClientStartFinalIntervalUsesCurrentGeneration(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("append=%v", appendMode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				outfile := filepath.Join(t.TempDir(), "result.csv")
				q := outfileTestQuery(t, outfile, appendMode)
				c := newOutfileTestClient(q, NonCumulativeMode)
				mergeOutfileTestRow(t, c.session.Snapshot(), 99)
				configureFinalTestConnection(c, func(ctx context.Context) { <-ctx.Done() })
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan int, 1)
				go func() { done <- c.Start(ctx, nil) }()
				synctest.Wait()
				if _, err := c.session.CommitQuery(q.RawQuery, 1); err != nil {
					t.Fatal(err)
				}
				mergeOutfileTestRow(t, c.session.Snapshot(), 7)
				synctest.Wait() // Let the reporter observe the query change.
				cancel()
				if status := <-done; status != 0 {
					t.Fatalf("Start status = %d", status)
				}
				assertScheduledTestFile(t, outfile, "count(foo)\n7\n")
			})
		})
	}
}

func TestMaprClientNonCumulativeFinalWriteFailureKeepsConnectionStatus(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		for _, status := range []int{0, 3} {
			t.Run(fmt.Sprintf("append=%v/status=%d", appendMode, status), func(t *testing.T) {
				q := outfileTestQuery(t, filepath.Join(t.TempDir(), "missing", "result.csv"), appendMode)
				c := newOutfileTestClient(q, NonCumulativeMode)
				logger := &reportHookLogger{}
				c.loggers.Client = logger
				mergeOutfileTestRow(t, c.session.Snapshot(), 7)
				if got := c.finish(context.Canceled, status); got != status {
					t.Fatalf("finish status = %d, want connection status %d", got, status)
				}
				if logger.errors.Load() != 1 {
					t.Fatal("final publication failure was not logged")
				}
			})
		}
	}
}

func TestMaprClientCanceledScheduledStartDoesNotPublishDrainedData(t *testing.T) {
	for _, mode := range []MaprClientMode{ScheduledMode, ScheduledPartialMode} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				outfile := filepath.Join(t.TempDir(), "result.csv")
				c := newOutfileTestClient(outfileTestQuery(t, outfile, false), mode)
				configureFinalTestConnection(c, func(ctx context.Context) {
					<-ctx.Done()
					mergeOutfileTestRow(t, c.session.Snapshot(), 7)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan int, 1)
				go func() { done <- c.Start(ctx, nil) }()
				synctest.Wait()
				cancel()
				if status := <-done; status != 1 {
					t.Fatalf("Start status = %d, want 1", status)
				}
				assertScheduledTestFile(t, outfile, "")
				assertScheduledTestFile(t, outfile+".query", "")
			})
		})
	}
}

func configureFinalTestConnection(c *MaprClient, run func(context.Context)) {
	c.mu = newBaseClientMu()
	c.stats = newTailStats(1, nil, 0, nil)
	c.connections = []connectors.Connector{&finalTestConnector{
		retryTestConnector: retryTestConnector{handler: &statusTestHandler{
			outcome: handlers.SessionOutcome{Completed: true},
		}},
		run: run,
	}}
}

func (c *finalTestConnector) Start(ctx context.Context, _ context.CancelFunc, _, _ chan struct{}) {
	c.run(ctx)
}

func (l *reportHookLogger) Debug(args ...any) string {
	if l.debug != nil && len(args) > 0 {
		l.debug(fmt.Sprint(args[0]))
	}
	return ""
}

func (l *reportHookLogger) Error(...any) string {
	l.errors.Add(1)
	return ""
}
