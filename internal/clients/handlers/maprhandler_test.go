package handlers

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

type recordingClientLogger struct {
	clientlog.NopLogger
	errors []string
}

func (l *recordingClientLogger) Error(args ...any) string {
	message := fmt.Sprint(args...)
	l.errors = append(l.errors, message)
	return message
}

func TestMaprHandlerShutdownFlushesPendingAggregateState(t *testing.T) {
	query, queryErr := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("NewQuery() error = %v", queryErr)
	}

	session := maprclient.NewSessionState(query, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, clientlog.NopLogger{})
	countStorage := handlerCountStorage(t, query)

	message := strings.Join([]string{
		"ERROR",
		"2",
		countStorage + protocol.AggregateKVDelimiter + "2",
		"",
	}, protocol.AggregateDelimiter)
	if err := handler.aggregate.Aggregate(message); err != nil {
		t.Fatalf("Aggregate() error = %v", err)
	}

	handler.Shutdown()

	result, numRows, err := session.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 {
		t.Fatalf("numRows = %d, want 1", numRows)
	}
	if !strings.Contains(result, "2") {
		t.Fatalf("expected flushed aggregate row, got %q", result)
	}
}

func TestMaprHandlerWriteEmptyMessageBetweenDelimiters(t *testing.T) {
	query, queryErr := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if queryErr != nil {
		t.Fatalf("NewQuery() error = %v", queryErr)
	}

	session := maprclient.NewSessionState(query, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, clientlog.NopLogger{})

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MaprHandler.Write panicked on empty protocol message: %v", r)
		}
	}()

	// Two consecutive MessageDelimiter bytes produce an empty message
	// between them. A leading delimiter yields an empty message too.
	// Both must be tolerated without panicking.
	input := []byte{
		protocol.MessageDelimiter,
		protocol.MessageDelimiter,
	}
	if _, err := handler.Write(input); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

// TestMaprHandlerClassifiesAuthKeyAckAsControl is a regression test for the
// dmap client feeding the server's "AUTHKEY OK" acknowledgement into the
// aggregate parser. In plain output mode the ack arrives on the wire verbatim
// (no SERVER|host| prefix), so it begins with the letter 'A' just like a real
// AGGREGATE|host|data message. Classifying on the full AggregateMessageID
// field prefix, instead of only the first byte, keeps such acks (and any
// sibling control message that merely starts with 'A') out of the aggregate
// parser, which previously logged a spurious
// "Unable to aggregate data ... expected 3 parts" error.
func TestMaprHandlerClassifiesAuthKeyAckAsControl(t *testing.T) {
	aggregate := protocol.AggregateMessageID + protocol.FieldSeparator() +
		"host1" + protocol.FieldSeparator() + "payload"

	tests := []struct {
		name          string
		message       string
		wantAggregate bool
	}{
		{
			name:          "genuine aggregate data",
			message:       aggregate,
			wantAggregate: true,
		},
		{
			name:          "malformed aggregate data",
			message:       protocol.AggregateMessageID + protocol.FieldSeparator() + "host1",
			wantAggregate: true,
		},
		{
			name:          "plain-mode authkey ack",
			message:       "AUTHKEY OK",
			wantAggregate: false,
		},
		{
			name:          "server-prefixed authkey ack",
			message:       "SERVER" + protocol.FieldSeparator() + "host1" + protocol.FieldSeparator() + "AUTHKEY OK",
			wantAggregate: false,
		},
		{
			name:          "unrelated message starting with A",
			message:       "Application ready",
			wantAggregate: false,
		},
		{
			// Adversarial: the AGGREGATE| tag appears, but embedded in a
			// later field rather than as the leading field. Only the leading
			// tag may classify a message as aggregate data.
			message:       "SERVER" + protocol.FieldSeparator() + "host1" + protocol.FieldSeparator() + aggregate,
			name:          "embedded aggregate tag is not the leading field",
			wantAggregate: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAggregateMessage(tc.message); got != tc.wantAggregate {
				t.Fatalf("isAggregateMessage(%q) = %v, want %v",
					tc.message, got, tc.wantAggregate)
			}
		})
	}
}

func TestMaprHandlerReportsMalformedAggregateFrame(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}

	logger := &recordingClientLogger{}
	handler := NewMaprHandler("srv1", maprclient.NewSessionState(query, logging.NopLogger{}), logger)
	malformed := protocol.AggregateMessageID + protocol.FieldSeparator() + "host1"
	input := append([]byte(malformed), protocol.MessageDelimiter)
	if _, err := handler.Write(input); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	logOutput := strings.Join(logger.errors, "\n")
	if !strings.Contains(logOutput, "Unable to decode aggregate data") ||
		!strings.Contains(logOutput, malformed) {
		t.Fatalf("malformed aggregate was not reported as a protocol error: %q", logOutput)
	}
}

// TestMaprHandlerWriteAuthKeyAckEmitsNoAggregateError feeds a plain-mode
// message stream (an "AUTHKEY OK" ack followed by a genuine aggregate message)
// through Write and asserts two things by inspecting the captured client log:
//
//  1. no spurious "Unable to aggregate data ... expected 3 parts" error is
//     emitted for the ack (the exact regression symptom); and
//  2. the genuine AGGREGATE|host|data message still produces its aggregate row.
//
// The log assertion is what makes this a real regression guard: with the old
// first-byte (message[0] == 'A') classifier the ack was fed to the aggregate
// parser and error-logged, so this test fails red against that code and green
// against the current prefix-based classifier.
func TestMaprHandlerWriteAuthKeyAckEmitsNoAggregateError(t *testing.T) {
	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}

	session := maprclient.NewSessionState(query, logging.NopLogger{})
	logger := &recordingClientLogger{}
	handler := NewMaprHandler("srv1", session, logger)
	countStorage := handlerCountStorage(t, query)

	// A genuine aggregate wire message: AGGREGATE|host|<serialized set>.
	serialized := strings.Join([]string{
		"ERROR",
		"2",
		countStorage + protocol.AggregateKVDelimiter + "2",
		"",
	}, protocol.AggregateDelimiter)
	aggregate := protocol.AggregateMessageID + protocol.FieldSeparator() +
		"host1" + protocol.FieldSeparator() + serialized

	// Plain-mode ack first, then the genuine aggregate message, each
	// terminated by the protocol message delimiter.
	var input []byte
	input = append(input, []byte("AUTHKEY OK")...)
	input = append(input, protocol.MessageDelimiter)
	input = append(input, []byte(aggregate)...)
	input = append(input, protocol.MessageDelimiter)

	if _, writeErr := handler.Write(input); writeErr != nil {
		t.Fatalf("Write() error = %v", writeErr)
	}
	handler.Shutdown()

	logOutput := strings.Join(logger.errors, "\n")
	if strings.Contains(logOutput, "Unable to aggregate data") ||
		strings.Contains(logOutput, "expected 3 parts") {
		t.Fatalf("AUTHKEY OK ack was fed to the aggregate parser; "+
			"captured client log:\n%s", logOutput)
	}

	result, numRows, err := session.Snapshot().GlobalGroup.Result(query, 10, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	if numRows != 1 {
		t.Fatalf("numRows = %d, want 1 (only the genuine aggregate message)", numRows)
	}
	if !strings.Contains(result, "2") {
		t.Fatalf("expected the genuine aggregate row, got %q", result)
	}
}

func handlerCountStorage(t *testing.T, query *mapr.Query) string {
	t.Helper()
	for _, selectCondition := range query.Select {
		if selectCondition.Operation == mapr.Count {
			return selectCondition.FieldStorage
		}
	}
	t.Fatalf("query %q does not contain count() storage", query.RawQuery)
	return ""
}
