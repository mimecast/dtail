package aggregate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

// finalTestGroups is the number of groups of the final serialization tests.
// At finalTestConsumerPause per message, a slow consumer takes 30 s (of the
// synctest bubble's fake clock) for them: three times the fixed 10 s deadline
// the final serialization used to have, which then dropped two thirds of the
// groups while the session still closed normally.
const (
	finalTestGroups        = 300
	finalTestConsumerPause = 100 * time.Millisecond
)

// newFinalTestAggregate returns an aggregate holding groups groups, one
// sample each, ready for a final serialization.
func newFinalTestAggregate(t *testing.T, groups int) *Aggregate {
	t.Helper()
	a, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	countField := a.query.Select[0].FieldStorage
	timeField := a.query.Select[1].FieldStorage
	a.serializer.groupMu.Lock()
	defer a.serializer.groupMu.Unlock()
	for i := range groups {
		key := fmt.Sprintf("group-%d", i)
		a.serializer.groupSets[key] = &mapr.AggregateSet{
			Samples: 1,
			FValues: map[string]float64{countField: 1},
			SValues: map[string]string{timeField: key},
		}
	}
	return a
}

// slowConsumer receives up to limit messages, pausing after each one like a
// client whose connection drains slowly, and reports how many it received.
// It stops early once stop is closed and no message is ready, so that a
// serialization that gave up fails the test instead of deadlocking it.
func slowConsumer(messages <-chan string, limit int, stop <-chan struct{}) <-chan int {
	received := make(chan int, 1)
	go func() {
		n := 0
		defer func() { received <- n }()
		for n < limit {
			select {
			case <-messages:
				n++
			case <-stop:
				return
			}
			time.Sleep(finalTestConsumerPause)
		}
	}()
	return received
}

// TestAggregateFinalSerializationHasNoDeadline sends a final result to a
// consumer far slower than the former 10 s bound, through both ways a final
// serialization starts (an explicit Shutdown and Start reacting to
// FinishInput). Every group must arrive and the result must be complete.
func TestAggregateFinalSerializationHasNoDeadline(t *testing.T) {
	tests := []struct {
		name     string
		finalize func(a *Aggregate, messages chan string)
	}{
		{name: "Shutdown", finalize: func(a *Aggregate, messages chan string) {
			a.PrepareOutput(context.Background(), messages)
			a.Shutdown(context.Background())
		}},
		{name: "Start after FinishInput", finalize: func(a *Aggregate, messages chan string) {
			a.FinishInput()
			a.Start(context.Background(), messages)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				a := newFinalTestAggregate(t, finalTestGroups)
				messages := make(chan string)
				finalized := make(chan struct{})
				received := slowConsumer(messages, finalTestGroups, finalized)
				start := time.Now()
				tt.finalize(a, messages)
				elapsed := time.Since(start)
				close(finalized)

				if got := <-received; got != finalTestGroups {
					t.Fatalf("consumer received %d groups, want %d", got, finalTestGroups)
				}
				if got := finalResultState(a.finalResult.Load()); got != finalResultComplete ||
					a.FinalResultInterrupted() {
					t.Fatalf("final result state = %d, FinalResultInterrupted() = %v after every group "+
						"was sent, want complete, false", got, a.FinalResultInterrupted())
				}
				if minimum := 10 * time.Second; elapsed <= minimum {
					t.Fatalf("final serialization took %v, want more than %v to cover the former deadline",
						elapsed, minimum)
				}
			})
		})
	}
}

// TestAggregateFinalSerializationCanceledIsIncomplete cancels the
// finalization context part way through the groups: the serialization stops
// and reports the result as interrupted, which the session handler then
// reports to the client as a failed command. The unsent groups are dropped,
// not re-merged (no serialization follows the final one), and the server
// logs the cut-off result once, with the number of unsent groups.
func TestAggregateFinalSerializationCanceledIsIncomplete(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const sent = 50
		a := newFinalTestAggregate(t, finalTestGroups)
		logger := &finalWarnRecorder{}
		a.logger, a.serializer.logger = logger, logger
		messages := make(chan string)
		a.PrepareOutput(context.Background(), messages)
		ctx, cancel := context.WithCancel(context.Background())
		received := slowConsumer(messages, sent, nil)
		go func() {
			<-received
			cancel()
		}()

		a.Shutdown(ctx)

		if !a.FinalResultInterrupted() {
			t.Fatal("FinalResultInterrupted() = false after a canceled final serialization")
		}
		if got := a.countGroups(); got != 0 {
			t.Fatalf("groups kept after the final serialization = %d, want 0 (dropped, not re-merged)", got)
		}
		want := fmt.Sprintf("Aggregate final serialization interrupted; final result incomplete|unsent|%d",
			finalTestGroups-sent)
		if got := logger.warnings(); !slices.Equal(got, []string{want}) {
			t.Fatalf("warnings = %q, want only %q", got, want)
		}
	})
}

// TestAggregateFinalResultWithoutFinalization pins that an aggregate whose
// terminal state is an abort, or that never finalized, has neither a
// complete nor an interrupted final result: there is nothing to report.
func TestAggregateFinalResultWithoutFinalization(t *testing.T) {
	a := newFinalTestAggregate(t, 1)
	if got := finalResultState(a.finalResult.Load()); got != finalResultNone || a.FinalResultInterrupted() {
		t.Fatalf("final result state = %d before finalization, want none", got)
	}
	a.Abort()
	a.PrepareOutput(context.Background(), make(chan string, 1))
	a.Shutdown(context.Background())
	if got := finalResultState(a.finalResult.Load()); got != finalResultNone || a.FinalResultInterrupted() {
		t.Fatalf("final result state = %d after Abort won the terminal state, want none", got)
	}
}

// finalWarnRecorder records the warnings logged, each with its arguments
// joined with "|".
type finalWarnRecorder struct {
	logging.NopLogger
	mu    sync.Mutex
	lines []string
}

func (l *finalWarnRecorder) Warn(args ...any) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = fmt.Sprint(arg)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.Join(parts, "|"))
	return ""
}

func (l *finalWarnRecorder) warnings() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

// TestWaitProcessorsPrefersFinishedInput pins that a processor join that is
// complete wins over an ended finalization context, so a session canceled
// right after its input ended is not reported as cut off before that.
func TestWaitProcessorsPrefersFinishedInput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	if waitProcessors(ctx, done) {
		t.Fatal("waitProcessors() = true with processors still running and ctx ended")
	}
	close(done)
	for range 1000 {
		if !waitProcessors(ctx, done) {
			t.Fatal("waitProcessors() = false with processors done and ctx ended")
		}
	}
}
