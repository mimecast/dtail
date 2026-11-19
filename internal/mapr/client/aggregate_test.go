package client

import (
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

func TestAggregateResetsPendingLocalStateOnGenerationChange(t *testing.T) {
	query := mustSessionStateQuery(t, "select status,count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	aggregate := NewAggregate("srv1", state, logging.NopLogger{})
	countStorage := aggregateCountStorage(t, query)

	oldSet := aggregate.group.GetSet("ERROR")
	oldSet.Samples = 1
	oldSet.FValues[countStorage] = 1

	rawQuery := "select status,count(status) from warnings group by status"
	if _, err := state.CommitQuery(rawQuery, 2); err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}

	snapshot := state.Snapshot()
	message := strings.Join([]string{
		"WARN",
		"1",
		aggregateCountStorage(t, snapshot.Query) + protocol.AggregateKVDelimiter + "1",
		"",
	}, protocol.AggregateDelimiter)

	if err := aggregate.Aggregate(message); err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}

	result, numRows, err := snapshot.GlobalGroup.Result(snapshot.Query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 {
		t.Fatalf("numRows = %d, want 1", numRows)
	}
	if !strings.Contains(result, "1") {
		t.Fatalf("expected one new-generation aggregate row, got %q", result)
	}
}

func TestAggregateRejectsMalformedMessage(t *testing.T) {
	query := mustSessionStateQuery(t, "select count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	aggregate := NewAggregate("srv1", state, logging.NopLogger{})

	if err := aggregate.Aggregate("broken"); err == nil {
		t.Fatalf("expected Aggregate() to reject malformed messages")
	}
}

func TestAggregateFlushMergesPendingLocalState(t *testing.T) {
	query := mustSessionStateQuery(t, "select status,count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	aggregate := NewAggregate("srv1", state, logging.NopLogger{})
	countStorage := aggregateCountStorage(t, query)

	set := aggregate.group.GetSet("ERROR")
	set.Samples = 3
	set.FValues[countStorage] = 3

	if err := aggregate.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	result, numRows, err := state.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 {
		t.Fatalf("numRows = %d, want 1", numRows)
	}
	if !strings.Contains(result, "3") {
		t.Fatalf("expected flushed aggregate row, got %q", result)
	}
}

func TestAggregateAndFlushAreSafeConcurrently(t *testing.T) {
	query := mustSessionStateQuery(t, "select status,count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	aggregate := NewAggregate("srv1", state, logging.NopLogger{})
	countStorage := aggregateCountStorage(t, query)

	const messageCount = 1000
	message := strings.Join([]string{
		"ERROR",
		"1",
		countStorage + protocol.AggregateKVDelimiter + "1",
		"",
	}, protocol.AggregateDelimiter)

	start := make(chan struct{})
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range messageCount {
			if err := aggregate.Aggregate(message); err != nil {
				errors <- err
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range messageCount {
			if err := aggregate.Flush(); err != nil {
				errors <- err
				return
			}
		}
	}()

	close(start)
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Fatalf("concurrent aggregate operation failed: %v", err)
	}
	if err := aggregate.Flush(); err != nil {
		t.Fatalf("final Flush() error = %v", err)
	}

	result, numRows, err := state.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 {
		t.Fatalf("numRows = %d, want 1", numRows)
	}
	fields := strings.Fields(result)
	if got, want := fields[len(fields)-1], strconv.Itoa(messageCount); got != want {
		t.Fatalf("aggregate count = %s, want %s; result:\n%s", got, want, result)
	}
}

func aggregateCountStorage(t *testing.T, query *mapr.Query) string {
	t.Helper()

	for _, selectCondition := range query.Select {
		if selectCondition.Operation == mapr.Count {
			return selectCondition.FieldStorage
		}
	}
	t.Fatalf("query %q does not contain count() storage", query.RawQuery)
	return ""
}
