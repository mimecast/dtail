package aggregate

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

const testDefaultLogFormat = "default"

type aggregateTestParser struct{}

func (*aggregateTestParser) MakeFields(line, _ string) (map[string]string, error) {
	return map[string]string{"$line": line}, nil
}

func newAggregateFromTextForTest(queryText string, logger logging.Logger) (*Aggregate, error) {
	query, err := mapr.NewQuery(queryText, logger)
	if err != nil {
		return nil, err
	}
	parser, err := logformat.NewParserWithHostname(
		query.EffectiveLogFormat(testDefaultLogFormat), query, "aggregate-test")
	if err != nil {
		parser, err = logformat.NewParserWithHostname("generic", query, "aggregate-test")
		if err != nil {
			return nil, err
		}
	}
	return New(query, parser, "aggregate-test", logger)
}

type aggregatePanicLogger struct {
	logging.NopLogger
	mu       sync.Mutex
	messages []string
}

func (l *aggregatePanicLogger) Error(args ...any) string {
	message := fmt.Sprint(args...)
	l.mu.Lock()
	l.messages = append(l.messages, message)
	l.mu.Unlock()
	return message
}

func (l *aggregatePanicLogger) output() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.messages, "\n")
}

func TestNewAggregateValidatesDependencies(t *testing.T) {
	query, err := mapr.NewQuery(`select count($line)`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	parser := &aggregateTestParser{}
	var typedNilParser *aggregateTestParser

	tests := []struct {
		name     string
		query    *mapr.Query
		parser   logformat.Parser
		hostname string
		wantErr  string
	}{
		{name: "nil query", parser: parser, hostname: "server", wantErr: "query must not be nil"},
		{name: "nil parser", query: query, hostname: "server", wantErr: "parser must not be nil"},
		{name: "typed nil parser", query: query, parser: typedNilParser, hostname: "server", wantErr: "parser must not be nil"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, newErr := New(tc.query, tc.parser, tc.hostname, logging.NopLogger{})
			if newErr == nil || !strings.Contains(newErr.Error(), tc.wantErr) {
				t.Fatalf("New error = %v, want substring %q", newErr, tc.wantErr)
			}
		})
	}

	aggregate, err := New(query, parser, "server", nil)
	if err != nil {
		t.Fatalf("New with injected dependencies: %v", err)
	}
	if aggregate.query != query || aggregate.parser != parser {
		t.Fatal("New did not retain the injected query and parser")
	}
}

func TestAggregateSerializationChildPanicPropagatesToStart(t *testing.T) {
	logger := &aggregatePanicLogger{}
	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logger,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	aggregate.serializationLoopHook = func() { panic("serialize child failed") }

	result := make(chan any, 1)
	go func() {
		defer func() { result <- recover() }()
		aggregate.Start(context.Background(), make(chan string, 1))
	}()

	select {
	case recovered := <-result:
		if recovered != "serialize child failed" {
			t.Fatalf("Start panic = %v, want propagated child panic", recovered)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not stop after serialization child panic")
	}
	if !aggregate.stopping() {
		t.Fatal("aggregate was not aborted after serialization child panic")
	}
	logOutput := logger.output()
	for _, want := range []string{"Recovered panic", "serialize child failed", "goroutine"} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("panic log %q does not contain %q", logOutput, want)
		}
	}
}

func TestAggregateSerializationLoopTickProgressesWithPendingRequest(t *testing.T) {

	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	aggregate.PrepareOutput(context.Background(), make(chan string, 1))

	ctx, cancel := context.WithCancel(context.Background())
	ticker := time.NewTicker(time.Millisecond)
	aggregate.serializer.ticker.Store(ticker)
	firstTick := make(chan struct{})
	releaseFirstTick := make(chan struct{})
	secondTick := make(chan struct{})
	tickCount := 0
	aggregate.serializationTickHook = func() {
		tickCount++
		switch tickCount {
		case 1:
			close(firstTick)
			<-releaseFirstTick
		case 2:
			close(secondTick)
		}
	}

	loopDone := make(chan struct{})
	go func() {
		aggregate.serializationLoop(ctx)
		close(loopDone)
	}()

	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirstTick) }) }
	defer func() {
		release()
		cancel()
		ticker.Stop()
		select {
		case <-loopDone:
		case <-time.After(time.Second):
			t.Error("serialization loop goroutine did not exit")
		}
	}()

	select {
	case <-firstTick:
	case <-time.After(time.Second):
		t.Fatal("serialization loop did not select the first tick")
	}

	// Queue an external request after the loop has selected the ticker case but
	// before it performs the periodic serialization. The old loop called
	// Serialize here and blocked trying to send behind this pending token.
	aggregate.Serialize(ctx)
	if got := len(aggregate.serializer.requests); got != 1 {
		t.Fatalf("pending serialization requests = %d, want 1", got)
	}
	release()

	select {
	case <-secondTick:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("serialization loop did not progress past a tick with a pending request")
	}
}

func TestAggregateSerializeCoalescesAndHonorsTermination(t *testing.T) {

	newAggregate := func(t *testing.T) *Aggregate {
		t.Helper()
		aggregate, err := newAggregateFromTextForTest(
			`from STATS select count($time),$time group by $time interval 3600`,
			logging.NopLogger{},
		)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return aggregate
	}

	t.Run("pending request", func(t *testing.T) {
		aggregate := newAggregate(t)
		aggregate.serializer.requests <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		returned := make(chan struct{})
		go func() {
			aggregate.Serialize(ctx)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(250 * time.Millisecond):
			cancel()
			<-returned
			t.Fatal("Serialize blocked behind a pending request")
		}
		if got := len(aggregate.serializer.requests); got != 1 {
			t.Fatalf("coalesced serialization requests = %d, want 1", got)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		aggregate := newAggregate(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		aggregate.Serialize(ctx)
		if got := len(aggregate.serializer.requests); got != 0 {
			t.Fatalf("requests queued after context cancellation = %d, want 0", got)
		}
	})

	t.Run("aggregate stopped", func(t *testing.T) {
		aggregate := newAggregate(t)
		aggregate.serializer.requests <- struct{}{}
		aggregate.done.Shutdown()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		returned := make(chan struct{})
		go func() {
			aggregate.Serialize(ctx)
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(250 * time.Millisecond):
			cancel()
			<-returned
			t.Fatal("Serialize did not return promptly after aggregate shutdown")
		}
		if got := len(aggregate.serializer.requests); got != 1 {
			t.Fatalf("pending requests after aggregate shutdown = %d, want 1", got)
		}

		<-aggregate.serializer.requests
		aggregate.Serialize(context.Background())
		if got := len(aggregate.serializer.requests); got != 0 {
			t.Fatalf("requests queued after aggregate shutdown = %d, want 0", got)
		}
	})
}

func TestAggregateShutdownSerializesWithPendingRequest(t *testing.T) {

	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time from - group by $time`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	messages := make(chan string, 10)
	aggregate.PrepareOutput(context.Background(), messages)

	processor := NewProcessor(aggregate, "test")
	line := "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
	if err := processor.ProcessLine(bytes.NewBufferString(line), 1, "test"); err != nil {
		t.Fatalf("ProcessLine: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	aggregate.Serialize(context.Background())
	if got := len(aggregate.serializer.requests); got != 1 {
		t.Fatalf("pending serialization requests = %d, want 1", got)
	}
	aggregate.Shutdown(context.Background())

	select {
	case result := <-messages:
		if !strings.Contains(result, "count($time)≔1") {
			t.Fatalf("unexpected final aggregate result: %q", result)
		}
	default:
		t.Fatal("shutdown lost final serialization behind a pending request")
	}
}

// TestAggregateDoSerializeReMergesOnCtxCancel verifies that when a
// serialize is cancelled after the live map has already advanced, the
// canceled snapshot is merged back without overwriting newer overwrite-style
// values. This guards against stale last()/len() values clobbering more recent
// updates that arrived after swapGroupSets.
func TestAggregateDoSerializeReMergesOnCtxCancel(t *testing.T) {

	queryStr := `from STATS select count($time),last($message),len($message) from - group by $service`
	agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	countStorage := agg.query.Select[0].FieldStorage
	lastStorage := agg.query.Select[1].FieldStorage
	lenStorage := agg.query.Select[2].FieldStorage

	agg.serializer.groupMu.Lock()
	agg.serializer.groupSets["svc"] = &mapr.AggregateSet{
		Samples: 1,
		FValues: map[string]float64{
			countStorage: 1,
			lenStorage:   float64(len("old-len")),
		},
		SValues: map[string]string{
			lastStorage: "old-last",
			lenStorage:  "old-len",
		},
	}
	agg.serializer.groupMu.Unlock()

	if got := agg.countGroups(); got != 1 {
		t.Fatalf("precondition: expected 1 group, got %d", got)
	}

	// Block the first send so doSerialize captures a snapshot and then waits
	// in AggregateSet.Serialize. While it is blocked we advance the live state
	// for the same group, then cancel the serialize context.
	messages := make(chan string)
	agg.PrepareOutput(context.Background(), messages)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		agg.doSerialize(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		if got := agg.countGroups(); got == 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for aggregate to swap live state")
		case <-time.After(5 * time.Millisecond):
		}
	}

	agg.serializer.groupMu.Lock()
	agg.serializer.groupSets["svc"] = &mapr.AggregateSet{
		Samples: 2,
		FValues: map[string]float64{
			countStorage: 2,
			lenStorage:   float64(len("new-len")),
		},
		SValues: map[string]string{
			lastStorage: "new-last",
			lenStorage:  "new-len",
		},
	}
	agg.serializer.groupMu.Unlock()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("doSerialize did not return after ctx cancel")
	}

	agg.serializer.groupMu.Lock()
	set, ok := agg.serializer.groupSets["svc"]
	agg.serializer.groupMu.Unlock()
	if !ok {
		t.Fatal("expected svc group to be re-merged after ctx cancel")
	}
	if got := set.Samples; got != 3 {
		t.Fatalf("expected merged samples to be 3, got %d", got)
	}
	if got := set.FValues[countStorage]; got != 3 {
		t.Fatalf("expected merged count to be 3, got %v", got)
	}
	if got := set.SValues[lastStorage]; got != "new-last" {
		t.Fatalf("expected latest last() value to survive cancel, got %q", got)
	}
	if got := set.SValues[lenStorage]; got != "new-len" {
		t.Fatalf("expected latest len() string value to survive cancel, got %q", got)
	}
	if got := set.FValues[lenStorage]; got != float64(len("new-len")) {
		t.Fatalf("expected latest len() numeric value to survive cancel, got %v", got)
	}
}

func TestAggregateShutdownHonorsContextWhileSerializationOwnsPermit(t *testing.T) {

	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	messages := make(chan string)
	aggregate.PrepareOutput(context.Background(), messages)

	aggregate.serializer.groupMu.Lock()
	aggregate.serializer.groupSets["held"] = &mapr.AggregateSet{
		Samples: 1,
		FValues: map[string]float64{aggregate.query.Select[0].FieldStorage: 1},
		SValues: map[string]string{aggregate.query.Select[1].FieldStorage: "held"},
	}
	aggregate.serializer.groupMu.Unlock()

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		aggregate.doSerialize(firstCtx)
		close(firstDone)
	}()
	waitForAggregateSnapshot(t, aggregate)

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() {
		aggregate.Shutdown(secondCtx)
		close(secondDone)
	}()
	waitForAggregateCondition(t, time.Second, "Shutdown did not start finalization", func() bool {
		aggregate.terminalMu.Lock()
		defer aggregate.terminalMu.Unlock()
		return aggregate.terminalState == aggregateFinalizing
	})
	select {
	case <-secondDone:
		t.Fatal("Shutdown returned while another serialization retained ownership")
	default:
	}
	cancelSecond()
	select {
	case <-secondDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Shutdown ignored cancellation while waiting for serialization ownership")
	}
	select {
	case <-firstDone:
		t.Fatal("first serialization did not retain ownership while its send was blocked")
	default:
	}

	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first serialization did not stop after cancellation")
	}
}

func TestAggregateShutdownHonorsContextWhileProcessorIsActive(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time) interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	processor := NewProcessor(aggregate, "blocked")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		aggregate.Shutdown(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Shutdown ignored cancellation while waiting for an active processor")
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestAggregateShutdownJoiningCallerHonorsItsOwnContext(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time) interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	processor := NewProcessor(aggregate, "blocked")
	ownerClaimed := make(chan struct{})
	releaseOwner := make(chan struct{})
	aggregate.finalizationOwnerHook = func() {
		close(ownerClaimed)
		<-releaseOwner
	}

	ownerDone := make(chan struct{})
	go func() {
		aggregate.Shutdown(context.Background())
		close(ownerDone)
	}()
	select {
	case <-ownerClaimed:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not claim finalizer ownership")
	}

	joinCtx, cancelJoin := context.WithCancel(context.Background())
	joinDone := make(chan struct{})
	go func() {
		aggregate.Shutdown(joinCtx)
		close(joinDone)
	}()
	cancelJoin()
	select {
	case <-joinDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("joining Shutdown ignored its own canceled context")
	}
	select {
	case <-ownerDone:
		t.Fatal("joining caller cancellation stopped the owning finalization")
	default:
	}

	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(releaseOwner)
	select {
	case <-ownerDone:
	case <-time.After(time.Second):
		t.Fatal("owning Shutdown did not finish after processor close")
	}
}

func TestAggregatePrepareOutputNilDisablesSerialization(t *testing.T) {
	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time) interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	aggregate.serializer.groupMu.Lock()
	aggregate.serializer.groupSets[""] = &mapr.AggregateSet{
		Samples: 1,
		FValues: map[string]float64{aggregate.query.Select[0].FieldStorage: 1},
		SValues: make(map[string]string),
	}
	aggregate.serializer.groupMu.Unlock()

	aggregate.PrepareOutput(context.Background(), make(chan string, 1))
	aggregate.PrepareOutput(context.Background(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		aggregate.doSerialize(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("serialization blocked on a nil output channel")
	}
	if got := aggregate.countGroups(); got != 1 {
		t.Fatalf("serialization with nil output retained %d groups, want 1", got)
	}
}

func TestAggregateRejectsNilContexts(t *testing.T) {
	var nilContext context.Context

	tests := map[string]func(*Aggregate){
		"Start": func(aggregate *Aggregate) {
			aggregate.Start(nilContext, make(chan string, 1))
		},
		"PrepareOutput": func(aggregate *Aggregate) {
			aggregate.PrepareOutput(nilContext, make(chan string, 1))
		},
		"Serialize": func(aggregate *Aggregate) {
			aggregate.Serialize(nilContext)
		},
		"PrepareShutdown": func(aggregate *Aggregate) {
			aggregate.PrepareShutdown(nilContext)
		},
		"Shutdown": func(aggregate *Aggregate) {
			aggregate.Shutdown(nilContext)
		},
		"AbortAndWait": func(aggregate *Aggregate) {
			aggregate.AbortAndWait(nilContext)
		},
	}

	for name, invoke := range tests {
		t.Run(name, func(t *testing.T) {
			aggregate, err := newAggregateFromTextForTest(
				`from STATS select count($time) interval 3600`,
				logging.NopLogger{},
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer func() {
				if recover() == nil {
					t.Fatal("nil context did not panic")
				}
			}()
			invoke(aggregate)
		})
	}
}

// TestAggregateProducesResults verifies the aggregate processes all
// input lines and produces serialized results. It was formerly a
// two-aggregator comparison that also exercised the regular channel-based
// server.Aggregate; that regular aggregate was deleted once this aggregate
// became the only aggregate path (task hv0), so only this subtest remains.
func TestAggregateProducesResults(t *testing.T) {

	// Test query
	queryStr := `from STATS select count($time),$time,avg($goroutines) from - group by $time order by $time`

	// Test data - DTail MapReduce format
	testLines := []string{
		"INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071143|1|stats.go:56|8|16|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071143|1|stats.go:56|8|17|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071147|1|stats.go:56|8|10|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071147|1|stats.go:56|8|11|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
	}

	t.Run("Aggregate", func(t *testing.T) {
		// Create aggregate
		agg, aggregateErr := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
		if aggregateErr != nil {
			t.Fatalf("Failed to create aggregate: %v", aggregateErr)
		}

		// Channel to collect messages
		messages := make(chan string, 100)
		// Use a cancellable context
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		startDone := make(chan struct{})
		go func() {
			defer close(startDone)
			agg.Start(ctx, messages)
		}()
		waitForAggregateStart(t, agg)

		// Process lines
		processor := NewProcessor(agg, "test")
		for i, line := range testLines {
			buf := bytes.NewBufferString(line)
			processErr := processor.ProcessLine(buf, uint64(i+1), "test")
			if processErr != nil {
				t.Errorf("Failed to process line %d: %v", i+1, processErr)
			}
		}

		// Flush to ensure all data is processed
		flushErr := processor.Flush()
		if flushErr != nil {
			t.Errorf("Failed to flush: %v", flushErr)
		}

		// Close the processor to decrement activeProcessors
		closeErr := processor.Close()
		if closeErr != nil {
			t.Errorf("Failed to close processor: %v", closeErr)
		}

		// Shutdown and get results
		agg.Shutdown(context.Background())

		// Cancel context to stop background goroutines
		cancel()
		<-startDone

		// Collect results with timeout
		done := make(chan struct{})
		var results []string
		go func() {
			for msg := range messages {
				results = append(results, msg)
			}
			close(done)
		}()

		// Start has returned, so final serialization can no longer send.
		close(messages)

		// Wait for collection to complete with timeout
		select {
		case <-done:
			// Good, collected all messages
		case <-time.After(2 * time.Second):
			t.Error("Timeout collecting messages")
		}

		t.Logf("Aggregate processed %d lines", agg.linesProcessed.Load())
		t.Logf("Aggregate results: %d messages", len(results))
		for _, r := range results {
			t.Logf("Result: %s", r)
		}

		// Verify we got results
		if len(results) == 0 {
			t.Error("Aggregate produced no results")
		}

		// Check line count
		if agg.linesProcessed.Load() != uint64(len(testLines)) {
			t.Errorf("Expected %d lines processed, got %d", len(testLines), agg.linesProcessed.Load())
		}
	})
}

// TestAggregateConcurrency tests aggregate with concurrent file processing
func TestAggregateConcurrency(t *testing.T) {

	queryStr := `from STATS select count($time),$time from - group by $time`

	// Create aggregate
	agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Failed to create aggregate: %v", err)
	}

	// Channel to collect messages
	messages := make(chan string, 1000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		agg.Start(ctx, messages)
	}()
	waitForAggregateStart(t, agg)

	// Process multiple "files" concurrently
	var wg sync.WaitGroup
	numFiles := 10
	linesPerFile := 100

	for f := 0; f < numFiles; f++ {
		wg.Add(1)
		go func(fileNum int) {
			defer wg.Done()

			processor := NewProcessor(agg, "file"+string(rune(fileNum)))

			// Process lines
			for i := 0; i < linesPerFile; i++ {
				line := "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
				buf := bytes.NewBufferString(line)
				_ = processor.ProcessLine(buf, uint64(i+1), "file"+string(rune(fileNum)))
			}

			// Flush when file completes
			_ = processor.Flush()

			// Close the processor to decrement activeProcessors
			_ = processor.Close()
		}(f)
	}

	// Wait for all files to complete
	wg.Wait()

	// Shutdown and get results
	agg.Shutdown(context.Background())
	cancel()
	<-startDone

	// Start has returned, so final serialization can no longer send.
	close(messages)

	var results []string
	for msg := range messages {
		if strings.Contains(msg, "1002-071143") {
			results = append(results, msg)
		}
	}

	t.Logf("Processed %d lines total", agg.linesProcessed.Load())
	t.Logf("Processed %d files", agg.filesProcessed.Load())
	t.Logf("Got %d result messages", len(results))

	// Verify line count
	expectedLines := uint64(numFiles * linesPerFile)
	if agg.linesProcessed.Load() != expectedLines {
		t.Errorf("Expected %d lines processed, got %d", expectedLines, agg.linesProcessed.Load())
	}

	if agg.filesProcessed.Load() != uint64(numFiles) {
		t.Errorf("Expected %d files processed, got %d", numFiles, agg.filesProcessed.Load())
	}

	// Parse result to check count
	foundExpectedCount := false
	for _, result := range results {
		t.Logf("Result: %s", result)
		// The result should show count($time)≔1000 (10 files * 100 lines each)
		if strings.Contains(result, "count($time)≔1000") {
			t.Log("✓ Found expected count of 1000")
			foundExpectedCount = true
			break
		}
	}

	if !foundExpectedCount {
		t.Error("Did not find expected count of 1000 in results")
	}
}

func TestAggregateAbortReturnsPromptlyWithActiveProcessors(t *testing.T) {
	aggregate := &Aggregate{done: internal.NewDone(), serializer: &serializer{}}
	aggregate.activeProcessors.Store(1)

	done := make(chan struct{})
	go func() {
		aggregate.Abort()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Abort did not return promptly while processors were still active")
	}
}

func TestAggregateAbruptTerminationWinsBeforeProducerChannelCloses(t *testing.T) {
	for _, termination := range []string{"Abort", "ContextCancel"} {
		t.Run(termination, func(t *testing.T) {
			aggregate, messages, producerDone, cancel := newBufferedTestAggregate(t)
			defer cancel()
			// Abrupt termination owns the terminal transition before Start
			// returns and its caller closes messages. A later Shutdown must
			// respect that output-free outcome; serializing the buffered line
			// now would send on the closed channel.
			switch termination {
			case "Abort":
				aggregate.Abort()
			case "ContextCancel":
				cancel()
			}
			select {
			case <-producerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("Start did not return after abrupt termination")
			}

			shutdownDone := make(chan struct{})
			panicValue := make(chan any, 1)
			go func() {
				defer close(shutdownDone)
				defer func() {
					if recovered := recover(); recovered != nil {
						panicValue <- recovered
					}
				}()
				aggregate.Shutdown(context.Background())
			}()
			select {
			case <-shutdownDone:
			case <-time.After(2 * time.Second):
				t.Fatal("Shutdown did not honor abrupt termination")
			}
			select {
			case recovered := <-panicValue:
				t.Fatalf("Shutdown sent after the producer channel closed: %v", recovered)
			default:
			}
			if message, ok := <-messages; ok {
				t.Fatalf("abrupt termination emitted aggregate output: %q", message)
			}
		})
	}
}

func TestAggregateFinalizationWinsConcurrentAbort(t *testing.T) {
	aggregate, messages, producerDone, cancel := newBufferedTestAggregate(t)
	defer cancel()

	// Claim finalization before canceling work, as GracefulShutdown does. The
	// unbuffered output channel then holds Shutdown inside final serialization
	// while Abort races with it. Start must stay alive, keeping channel ownership
	// with the producer until the winning serialization completes.
	aggregate.PrepareShutdown(context.Background())
	shutdownDone := make(chan struct{})
	go func() {
		aggregate.Shutdown(context.Background())
		close(shutdownDone)
	}()
	waitForAggregateSnapshot(t, aggregate)

	abortDone := make(chan struct{})
	go func() {
		aggregate.Abort()
		close(abortDone)
	}()
	select {
	case <-abortDone:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Abort blocked behind final serialization")
	}
	select {
	case <-producerDone:
		t.Fatal("Start returned and closed the producer channel during final serialization")
	default:
	}

	select {
	case message := <-messages:
		if !strings.Contains(message, "count($time)≔1") {
			t.Fatalf("unexpected final aggregate result: %q", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for final aggregate result")
	}
	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not finish after final output was consumed")
	}
	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after final serialization")
	}
	if message, ok := <-messages; ok {
		t.Fatalf("unexpected extra aggregate output: %q", message)
	}
}

func TestAggregatePreparedContextControlsStartOwnedFinalization(t *testing.T) {

	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	messages := make(chan string, 1)
	commandCtx, cancelCommand := context.WithCancel(context.Background())
	producerDone := make(chan struct{})
	go func() {
		aggregate.Start(commandCtx, messages)
		close(messages)
		close(producerDone)
	}()
	waitForAggregateStart(t, aggregate)

	processor := NewProcessor(aggregate, "test")
	for i := 0; i < cap(messages)+2; i++ {
		line := strings.Replace(
			"INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
			"1002-071143", fmt.Sprintf("1002-%06d", i), 1,
		)
		if err := processor.ProcessLine(bytes.NewBufferString(line), uint64(i+1), "test"); err != nil {
			t.Fatalf("ProcessLine failed: %v", err)
		}
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Graceful shutdown claims and binds its output context before canceling
	// command work. Deliberately let Start wake and enter shutdown first: it
	// must still use drainCtx, otherwise the over-capacity result cannot be
	// canceled by the output consumer.
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	aggregate.PrepareShutdown(drainCtx)
	cancelCommand()

	waitForAggregateCondition(t, 2*time.Second, "Start did not enter final serialization", func() bool {
		return len(messages) == cap(messages)
	})
	select {
	case <-producerDone:
		t.Fatal("final serialization completed despite a full output channel")
	default:
	}

	cancelDrain()
	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start-owned finalization ignored the prepared output context")
	}

	// A later graceful participant joins the completed terminal result rather
	// than starting a second serialization with a different context.
	aggregate.Shutdown(context.Background())
	var emitted int
	for range messages {
		emitted++
	}
	if emitted != cap(messages) {
		t.Fatalf("emitted %d results, want the %d that fit before cancellation", emitted, cap(messages))
	}
	if remaining := aggregate.countGroups(); remaining == 0 {
		t.Fatal("expected canceled finalization to retain unsent groups")
	}
}

func TestAggregateProcessorCountsFlushOnce(t *testing.T) {
	lineBatcher, err := newBatcher(16)
	if err != nil {
		t.Fatalf("newBatcher: %v", err)
	}
	aggregate := &Aggregate{
		done:       internal.NewDone(),
		batcher:    lineBatcher,
		serializer: &serializer{},
	}

	processor := NewProcessor(aggregate, "test")
	if err := processor.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if got := aggregate.filesProcessed.Load(); got != 1 {
		t.Fatalf("expected filesProcessed to be 1, got %d", got)
	}
	if got := aggregate.activeProcessors.Load(); got != 0 {
		t.Fatalf("expected activeProcessors to be 0, got %d", got)
	}
}

func TestAggregateProcessorCloseReleasesAccountingWhenFlushPanics(t *testing.T) {
	aggregate := &Aggregate{
		done:       internal.NewDone(),
		serializer: &serializer{},
		// A nil parser makes real batch processing panic inside Flush.
		batcher: &batcher{
			maxSize: 1,
			pending: []rawLine{{content: bytes.NewBufferString("trigger flush panic")}},
		},
	}
	processor := NewProcessor(aggregate, "test")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = processor.Close()
	}()
	if recovered == nil {
		t.Fatal("Close did not propagate the injected Flush panic")
	}
	if got := aggregate.activeProcessors.Load(); got != 0 {
		t.Fatalf("active processors after Flush panic = %d, want 0", got)
	}

	done := make(chan struct{})
	go func() {
		aggregate.AbortAndWait(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AbortAndWait hung after Processor Flush panic")
	}
}

// TestAggregateFinishInputTerminatesStart is the regression test for the
// server-mode dmap deadlock: Start used to block until context cancel
// or session teardown even after all one-shot input had been consumed, which
// kept the server's map command active forever and hung the client after all
// results were delivered. With FinishInput, Start must emit the final
// serialization and return on its own.
func TestAggregateFinishInputTerminatesStart(t *testing.T) {

	queryStr := `from STATS select count($time),$time from - group by $time`
	agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	messages := make(chan string, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		agg.Start(ctx, messages)
	}()
	waitForAggregateStart(t, agg)

	processor := NewProcessor(agg, "test")
	testLines := []string{
		"INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071143|1|stats.go:56|8|16|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
		"INFO|1002-071147|1|stats.go:56|8|17|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1",
	}
	for i, lineStr := range testLines {
		if err := processor.ProcessLine(bytes.NewBufferString(lineStr), uint64(i+1), "test"); err != nil {
			t.Fatalf("ProcessLine failed: %v", err)
		}
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Signal input exhaustion; Start must finalize and return on its own,
	// without Shutdown or context cancellation.
	agg.FinishInput()

	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after FinishInput (server-mode dmap deadlock)")
	}

	// After Start returned, no goroutine may send on messages anymore, so
	// closing and draining is race-free.
	close(messages)
	var results []string
	for msg := range messages {
		results = append(results, msg)
	}
	if len(results) == 0 {
		t.Fatal("expected a final serialized result after FinishInput")
	}
	foundCount := false
	for _, result := range results {
		if strings.Contains(result, "count($time)≔2") {
			foundCount = true
		}
	}
	if !foundCount {
		t.Fatalf("expected final result to contain count($time)≔2, got: %v", results)
	}
}

// TestAggregateStreamingContinuesWithoutFinishInput is the negative
// counterpart of the FinishInput regression test: a follow-mode (tail) map
// query never exhausts its input, so the aggregate must keep emitting
// interval-based interim results and Start must NOT return while the stream
// is live. This guards against over-eager finalization breaking continuous
// map queries over tailed logs.
func TestAggregateStreamingContinuesWithoutFinishInput(t *testing.T) {

	queryStr := `from STATS select count($time),$time from - group by $time`
	agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	// Fast serialization interval so the test observes interim results quickly.
	agg.query.Interval = 50 * time.Millisecond

	messages := make(chan string, 100)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		agg.Start(ctx, messages)
	}()
	waitForAggregateStart(t, agg)

	// Keep the processor open for the whole test, simulating a followed file.
	processor := NewProcessor(agg, "test")
	feed := func(lineStr string) {
		t.Helper()
		if err := processor.ProcessLine(bytes.NewBufferString(lineStr), 1, "test"); err != nil {
			t.Fatalf("ProcessLine failed: %v", err)
		}
	}
	waitForResult := func(what string) string {
		t.Helper()
		select {
		case msg := <-messages:
			return msg
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s interval result", what)
			return ""
		}
	}

	feed("INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1")
	first := waitForResult("first")

	feed("INFO|1002-071147|1|stats.go:56|8|16|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1")
	second := waitForResult("second")

	if first == "" || second == "" {
		t.Fatal("expected two non-empty interval results")
	}

	// The stream is still live: Start must not have returned.
	select {
	case <-startDone:
		t.Fatal("Start returned although the follow-mode input never signaled FinishInput")
	default:
	}

	// Cleanup: close the processor before Shutdown (Shutdown waits for all
	// processors), then wait for Start to return.
	if err := processor.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	agg.Shutdown(context.Background())
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

// TestAggregateStartDoSerializeFieldRace exercises the concurrent access to
// the serializer output field. Start publishes the output while a separate
// goroutine runs doSerialize — the read site (aggregate.go ~355) reached
// in production via baseHandler.Shutdown -> Aggregate.Shutdown ->
// doSerialize, which runs on a different goroutine than the one executing Start.
// Before the fix the write in Start was unsynchronized while doSerialize read
// the field: a data race under the Go memory model even though the nil check
// prevented a crash. PrepareOutput now publishes an atomic output holder, so
// -race must stay clean across many tight iterations.
func TestAggregateStartDoSerializeFieldRace(t *testing.T) {

	queryStr := `from STATS select count($time),$time from - group by $time`
	const iterations = 500

	for i := 0; i < iterations; i++ {
		agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}

		messages := make(chan string, 8)
		ctx, cancel := context.WithCancel(context.Background())

		// Release both goroutines as close together as possible so the write to
		// the output at the top of Start overlaps the read inside
		// doSerialize. No lines are fed, so doSerialize takes the empty-snapshot
		// path and never sends on messages.
		release := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-release
			agg.Start(ctx, messages)
		}()
		go func() {
			defer wg.Done()
			<-release
			agg.doSerialize(ctx)
		}()
		close(release)

		// doSerialize returns quickly; cancel so Start unblocks its select and
		// its serialization loop exits before the next iteration.
		cancel()
		wg.Wait()

		close(messages)
		for range messages { //nolint:revive // drain any (unexpected) output
		}
	}
}

// TestAggregateStartStopTickerFieldRace exercises the concurrent access to
// the serializer ticker field. Start creates and publishes the ticker while
// a separate goroutine runs Abort -> stopSerializeTicker, which reads the field.
// In production stopSerializeTicker is reached from baseHandler.Shutdown ->
// Aggregate.Shutdown/Abort on the teardown goroutine, a different goroutine
// than the one executing Start. Before the fix the write in Start was a plain
// unsynchronized pointer store while stopSerializeTicker read the pointer with no
// happens-before edge: a data race under the Go memory model even though the nil
// check prevented a crash. Start now publishes the ticker with an atomic Store
// and stopSerializeTicker reads it with an atomic Load, so -race must stay clean
// across many tight iterations. This test deliberately omits
// waitForAggregateStart so the ticker write and read can actually overlap.
func TestAggregateStartStopTickerFieldRace(t *testing.T) {

	queryStr := `from STATS select count($time),$time from - group by $time`
	const iterations = 500

	for i := 0; i < iterations; i++ {
		agg, err := newAggregateFromTextForTest(queryStr, logging.NopLogger{})
		if err != nil {
			t.Fatalf("New failed: %v", err)
		}

		messages := make(chan string, 8)
		ctx, cancel := context.WithCancel(context.Background())

		// Release both goroutines as close together as possible so the ticker
		// Store near the top of Start overlaps the Load inside
		// stopSerializeTicker. Abort is used because it reaches
		// stopSerializeTicker without waiting for a final serialization, giving
		// the tightest overlap with Start's ticker publish.
		release := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-release
			agg.Start(ctx, messages)
		}()
		go func() {
			defer wg.Done()
			<-release
			agg.Abort()
		}()
		close(release)

		// Abort signals done, so Start unblocks its select and its serialization
		// loop exits. Cancel as a belt-and-suspenders in case Abort lost the race
		// and Start is still waiting on the ticker interval.
		cancel()
		wg.Wait()

		close(messages)
		for range messages { //nolint:revive // drain any (unexpected) output
		}
	}
}

func waitForAggregateStart(t *testing.T, aggregate *Aggregate) {
	t.Helper()

	if aggregate.started == nil {
		t.Fatal("aggregate missing start signal")
	}
	select {
	case <-aggregate.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("aggregate did not finish Start initialization")
	}
}

func newBufferedTestAggregate(t *testing.T) (*Aggregate, chan string, <-chan struct{}, context.CancelFunc) {
	t.Helper()

	aggregate, err := newAggregateFromTextForTest(
		`from STATS select count($time),$time group by $time interval 3600`,
		logging.NopLogger{},
	)
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	messages := make(chan string)
	producerDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		aggregate.Start(ctx, messages)
		close(messages)
		close(producerDone)
	}()
	waitForAggregateStart(t, aggregate)

	processor := NewProcessor(aggregate, "test")
	line := "INFO|1002-071143|1|stats.go:56|8|15|7|0.21|471h0m21s|MAPREDUCE:STATS|currentConnections=0|lifetimeConnections=1"
	if err := processor.ProcessLine(bytes.NewBufferString(line), 1, "test"); err != nil {
		t.Fatalf("ProcessLine failed: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if got := aggregate.countGroups(); got != 1 {
		t.Fatalf("precondition: expected one buffered aggregate group, got %d", got)
	}

	return aggregate, messages, producerDone, cancel
}

func waitForAggregateSnapshot(t *testing.T, aggregate *Aggregate) {
	t.Helper()

	waitForAggregateCondition(t, 2*time.Second, "aggregate did not snapshot data for final serialization", func() bool {
		return aggregate.countGroups() == 0
	})
}

func waitForAggregateCondition(t *testing.T, timeout time.Duration, failure string, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal(failure)
		}
	}
}
