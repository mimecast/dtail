package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/io/journal"
)

// TestStopSharedWorkStopsAbandonedUnitListing: the server's shutdown stops
// a journal unit listing no session waits for any more, and a server
// without a unit cache shuts down all the same.
func TestStopSharedWorkStopsAbandonedUnitListing(t *testing.T) {
	started := make(chan struct{})
	stopped := make(chan struct{})
	hung := func(ctx context.Context, _ int) ([]string, bool, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, false, ctx.Err()
	}
	s := &Server{journalUnits: journal.NewUnitCache(hung, time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.journalUnits.ListUnits(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListUnits: %v, want context.Canceled", err)
	}
	<-started
	select {
	case <-stopped:
		t.Fatal("the abandoned listing stopped before the shutdown")
	case <-time.After(50 * time.Millisecond):
	}
	s.stopSharedWork()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the shutdown did not stop the abandoned unit listing")
	}
	if _, _, err := s.journalUnits.ListUnits(context.Background(), 10); !errors.Is(err, journal.ErrUnitCacheClosed) {
		t.Fatalf("ListUnits after the shutdown: %v, want ErrUnitCacheClosed", err)
	}
	(&Server{}).stopSharedWork()
}
