package handlers

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// payloadRecordingLogger records the payload and the errors a handler logs.
type payloadRecordingLogger struct {
	clientlog.NopLogger
	mu      sync.Mutex
	payload strings.Builder
	errors  []string
}

func (l *payloadRecordingLogger) RawBytes(message []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.payload.Write(message)
}

func (l *payloadRecordingLogger) Raw(message string) string {
	l.RawBytes([]byte(message))
	return message
}

func (l *payloadRecordingLogger) Error(args ...any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	message := fmt.Sprint(args...)
	l.errors = append(l.errors, message)
	return message
}

// plainAggregateLookalikes are --plain log lines that start like an
// aggregate frame (protocol.CutAggregateContent): a malformed one, an empty
// one and one that parses as aggregate data.
var plainAggregateLookalikes = []string{
	"AGGREGATE|host|x",
	"AGGREGATE|",
	"AGGREGATE|h|k∥1∥f≔1∥",
}

// writeCapabilitiesAndLines writes the capabilities frame and lines, each a
// payload frame with its newline, to handler as a dserver sends them.
func writeCapabilitiesAndLines(t *testing.T, handler *MaprHandler, lines []string) {
	t.Helper()
	var input bytes.Buffer
	protocol.EscapedControlFraming.EncodeControl(&input, protocol.HiddenCapabilitiesPrefix)
	for _, line := range lines {
		protocol.EscapedControlFraming.EncodePayload(&input, []byte(line+"\n"))
	}
	if _, err := handler.Write(input.Bytes()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
}

// TestMaprHandlerPrintsAggregateLookalikesWithoutQuery is the handler of
// interactive dtail following lines (a session state without a query): a
// followed --plain line starting with AGGREGATE| is payload, printed as it
// is, never taken for aggregate data (dropped, or logged as an error).
func TestMaprHandlerPrintsAggregateLookalikesWithoutQuery(t *testing.T) {
	logger := &payloadRecordingLogger{}
	session := maprclient.NewSessionState(nil, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, logger)
	writeCapabilitiesAndLines(t, handler, plainAggregateLookalikes)

	want := strings.Join(plainAggregateLookalikes, "\n") + "\n"
	if got := logger.payload.String(); got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	if len(logger.errors) != 0 {
		t.Fatalf("errors = %q, want none", logger.errors)
	}
}

// TestMaprHandlerTakesAggregatesWhileExpected covers the states in which the
// session expects aggregates: a switch to a query is pending (the first
// aggregates may arrive before the commit) and a query is committed. A
// pending switch that failed expects none again.
func TestMaprHandlerTakesAggregatesWhileExpected(t *testing.T) {
	logger := &payloadRecordingLogger{}
	session := maprclient.NewSessionState(nil, logging.NopLogger{})
	handler := NewMaprHandler("srv1", session, logger)
	writeCapabilitiesAndLines(t, handler, nil)

	session.ExpectAggregates(true)
	writeCapabilitiesAndLines(t, handler, plainAggregateLookalikes[2:])
	if got := logger.payload.String(); got != "" {
		t.Fatalf("aggregate printed while a switch to a query is pending: %q", got)
	}
	session.ExpectAggregates(false)
	writeCapabilitiesAndLines(t, handler, plainAggregateLookalikes[:1])
	if got := logger.payload.String(); got != plainAggregateLookalikes[0]+"\n" {
		t.Fatalf("payload after a failed switch = %q", got)
	}

	query, err := mapr.NewQuery("select status,count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.CommitQuery(query.RawQuery, 2); err != nil {
		t.Fatal(err)
	}
	if !session.AggregatesExpected() {
		t.Fatal("a committed query expects no aggregates")
	}
	session.ClearQuery(3)
	if session.AggregatesExpected() {
		t.Fatal("a cleared query still expects aggregates")
	}
}
