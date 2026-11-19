package handlers

import (
	"slices"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/protocol"
)

// TestStatusConcurrentAccess: the status of a handler is written by the
// goroutine copying the server's output into Write (checkFirstFrame, the
// health check's OK) and by the dial goroutine (ReportServerError, e.g. from
// requireJournalCapability), and read by the client (Status). Run with
// -race, which reports a data race on an unguarded status.
func TestStatusConcurrentAccess(t *testing.T) {
	// The first frame of a server of an older protocol version is no
	// capabilities frame, so checkFirstFrame reports it.
	oldServerFrame := payloadFrames(protocol.EscapedControlFraming, "hello\n")
	okFrame := payloadFrames(protocol.EscapedControlFraming, "OK\n")
	tests := []struct {
		name    string
		handler func() Handler
		stream  []byte
		want    []int // the acceptable final statuses
	}{
		{name: "client handler", stream: oldServerFrame, want: []int{1},
			handler: func() Handler { return NewClientHandler("srv1", clientlog.NopLogger{}) }},
		{name: "mapr handler", stream: oldServerFrame, want: []int{1},
			handler: func() Handler { return NewMaprHandler("srv1", nil, clientlog.NopLogger{}) }},
		// The health handler sets OK (0) on the server's OK, and the report
		// raises only a status of 0 or less (not the initial CRITICAL 2) to
		// 1, so the result depends on the order: either is fine, the race
		// is not.
		{name: "health handler", stream: okFrame, want: []int{0, 1},
			handler: func() Handler { return NewHealthHandler("srv1", clientlog.NopLogger{}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := tc.handler()
			defer handler.Shutdown()
			var wg sync.WaitGroup
			wg.Add(3)
			go func() {
				defer wg.Done()
				_, _ = handler.Write(tc.stream)
			}()
			go func() {
				defer wg.Done()
				handler.ReportServerError("missing capability")
			}()
			go func() {
				defer wg.Done()
				_ = handler.Status()
			}()
			wg.Wait()
			if got := handler.Status(); !slices.Contains(tc.want, got) {
				t.Fatalf("Status() = %d, want one of %v", got, tc.want)
			}
		})
	}
}

// TestReportServerErrorKeepsHigherStatus: a reported server error raises a
// status of 0 or less (handlers start at -1, health checks at 2) to 1 and
// keeps a higher one.
func TestReportServerErrorKeepsHigherStatus(t *testing.T) {
	tests := []struct {
		name  string
		start int32
		want  int
	}{
		{name: "initial", start: -1, want: 1},
		{name: "ok", start: 0, want: 1},
		{name: "already failed", start: 1, want: 1},
		{name: "critical", start: 2, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewClientHandler("srv1", clientlog.NopLogger{})
			defer handler.Shutdown()
			handler.status.Store(tc.start)
			handler.ReportServerError("boom")
			if got := handler.Status(); got != tc.want {
				t.Fatalf("Status() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestHandlerInitialStatus: client and mapreduce handlers start at -1 (no
// status yet), health checks at 2 (CRITICAL until the server says OK).
func TestHandlerInitialStatus(t *testing.T) {
	tests := []struct {
		name    string
		handler Handler
		want    int
	}{
		{name: "client", handler: NewClientHandler("srv1", clientlog.NopLogger{}), want: -1},
		{name: "mapr", handler: NewMaprHandler("srv1", nil, clientlog.NopLogger{}), want: -1},
		{name: "health", handler: NewHealthHandler("srv1", clientlog.NopLogger{}), want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer tc.handler.Shutdown()
			if got := tc.handler.Status(); got != tc.want {
				t.Fatalf("Status() = %d, want %d", got, tc.want)
			}
		})
	}
}
