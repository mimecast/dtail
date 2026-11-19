package client

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

func TestSessionStateCommitQueryResetsGenerationAndResults(t *testing.T) {
	query := mustSessionStateQuery(t, "select count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})

	initial := state.Snapshot()
	group := mapr.NewGroupSet(logging.NopLogger{})
	set := group.GetSet("ERROR")
	set.Samples = 1
	set.FValues[query.Select[0].FieldStorage] = 1
	if err := initial.GlobalGroup.Merge(query, group); err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if changed, ok := state.CommitRenderedResult(initial.Generation, "old-result"); !ok || !changed {
		t.Fatalf("CommitRenderedResult() = changed:%v ok:%v, want changed and ok", changed, ok)
	}

	rawQuery := "select count(status) from warnings group by status"
	updatedQuery, err := state.CommitQuery(rawQuery, 3)
	if err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}
	if updatedQuery == nil || updatedQuery.RawQuery != rawQuery {
		t.Fatalf("unexpected updated query: %#v", updatedQuery)
	}

	select {
	case <-state.Changes():
	default:
		t.Fatalf("expected change notification after CommitQuery")
	}

	updated := state.Snapshot()
	if updated.Generation != 3 {
		t.Fatalf("generation = %d, want 3", updated.Generation)
	}
	if updated.Query == nil || updated.Query.RawQuery != rawQuery {
		t.Fatalf("unexpected query after commit: %#v", updated.Query)
	}
	if !updated.GlobalGroup.IsEmpty() {
		t.Fatalf("expected committed global group to be reset")
	}
	if updated.LastResult != "" {
		t.Fatalf("last result = %q, want empty", updated.LastResult)
	}
}

func TestSessionStateCommitQueryRejectsInvalidQuery(t *testing.T) {
	query := mustSessionStateQuery(t, "select count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	before := state.Snapshot()

	if _, err := state.CommitQuery("select from", 5); err == nil {
		t.Fatalf("expected CommitQuery() to reject invalid query")
	}

	after := state.Snapshot()
	if after.Generation != before.Generation {
		t.Fatalf("generation changed on invalid query: got %d want %d", after.Generation, before.Generation)
	}
	if after.Query == nil || after.Query.RawQuery != before.Query.RawQuery {
		t.Fatalf("query changed on invalid query: before=%#v after=%#v", before.Query, after.Query)
	}
}

func TestSessionStateWithCurrentSnapshotOrdersPublicationAndCommit(t *testing.T) {
	query := mustSessionStateQuery(t, "select count(status) from stats group by status")
	state := NewSessionState(query, logging.NopLogger{})
	snapshot := state.Snapshot()

	publicationEntered := make(chan struct{})
	releasePublication := make(chan struct{})
	publicationDone := make(chan error, 1)
	go func() {
		run, err := state.WithCurrentSnapshot(snapshot, func() error {
			close(publicationEntered)
			<-releasePublication
			return nil
		})
		if err == nil && !run {
			err = fmt.Errorf("current snapshot callback was not run")
		}
		publicationDone <- err
	}()
	<-publicationEntered

	commitStarted := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		close(commitStarted)
		_, err := state.CommitQuery(query.RawQuery, 1)
		commitDone <- err
	}()
	<-commitStarted

	writerWaiting := false
	for range 10_000 {
		if state.mu.TryRLock() {
			state.mu.RUnlock()
			runtime.Gosched()
			continue
		}
		writerWaiting = true
		break
	}
	if !writerWaiting {
		close(releasePublication)
		t.Fatal("CommitQuery did not wait for the publication read lock")
	}
	select {
	case err := <-commitDone:
		close(releasePublication)
		t.Fatalf("CommitQuery completed during publication with error %v", err)
	default:
	}

	close(releasePublication)
	if err := <-publicationDone; err != nil {
		t.Fatalf("WithCurrentSnapshot() error = %v", err)
	}
	if err := <-commitDone; err != nil {
		t.Fatalf("CommitQuery() error = %v", err)
	}

	callbackCalled := false
	run, err := state.WithCurrentSnapshot(snapshot, func() error {
		callbackCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("stale WithCurrentSnapshot() error = %v", err)
	}
	if run || callbackCalled {
		t.Fatalf("stale WithCurrentSnapshot() = run:%v callbackCalled:%v, want both false", run, callbackCalled)
	}
}

func mustSessionStateQuery(t *testing.T, queryStr string) *mapr.Query {
	t.Helper()

	query, err := mapr.NewQuery(queryStr, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery(%q) error = %v", queryStr, err)
	}
	return query
}
