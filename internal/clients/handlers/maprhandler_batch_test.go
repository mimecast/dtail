package handlers

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// batchTestQuery selects every client-merged operation, so a difference in
// how they are aggregated shows up in the result.
const batchTestQuery = "from STATS select count(user),sum(v),avg(v),min(v),max(v),last(user),len(user) group by g"

// batchTestStream is a server stream with escaped and marked control
// framing: the capabilities frame, the aggregate frames of sets (their
// group keys contain the delimiter and escape bytes), some with a trailing
// newline, and a marked control frame in between. It returns the stream and
// the sets in the order of their frames.
func batchTestStream(t *testing.T, query *mapr.Query) ([]byte, []batchTestSet) {
	t.Helper()
	var stream bytes.Buffer
	protocol.RawFraming.EncodeControl(&stream, protocol.HiddenCapabilitiesPrefix+
		protocol.CapabilityEscapedFramingV1+" "+protocol.CapabilityControlFramesV1)
	framing := protocol.EscapedControlFraming
	var sets []batchTestSet
	seed := uint64(1)
	for i := range 300 {
		seed = seed*6364136223846793005 + 1442695040888963407
		groupKey := "g€" + strconv.Itoa(int(seed>>33)%40) + "\xff"
		set := batchTestAggregateSet(query, seed, i)
		content := serializeSet(t, set, groupKey)
		if i%3 == 0 {
			content += "\n"
		}
		framing.EncodeMessage(&stream, protocol.Message{Kind: protocol.MessageAggregate, Hostname: "srv1", Content: content})
		if i == 150 {
			framing.EncodeControl(&stream, protocol.HiddenCommandFailedPrefix+"read: no file to read")
		}
		sets = append(sets, batchTestSet{groupKey: groupKey, set: set})
	}
	return stream.Bytes(), sets
}

// batchTestSet is a server's aggregate set of one group.
type batchTestSet struct {
	groupKey string
	set      *mapr.AggregateSet
}

// batchTestAggregateSet returns a server's set with values derived from
// seed: fractional sums whose total depends on the order of the additions,
// and a set every seventh time that lacks the v fields.
func batchTestAggregateSet(query *mapr.Query, seed uint64, i int) *mapr.AggregateSet {
	set := mapr.NewAggregateSet()
	set.Samples = 1 + int(seed>>40)%5
	value := float64(int64(seed>>20)%2000-1000) / 7
	for _, sc := range query.Select {
		switch sc.Operation {
		case mapr.Count:
			set.FValues[sc.FieldStorage] = float64(set.Samples)
		case mapr.Last, mapr.Len:
			user := "user" + strconv.Itoa(i)
			set.SValues[sc.FieldStorage] = user
			if sc.Operation == mapr.Len {
				set.FValues[sc.FieldStorage] = float64(len(user))
			}
		case mapr.Avg:
			if i%7 != 0 {
				_ = set.AggregateAvg(sc.FieldStorage, strconv.FormatFloat(value, 'f', -1, 64), set.Samples)
			}
		default:
			if i%7 != 0 {
				set.FValues[sc.FieldStorage] = value
			}
		}
	}
	return set
}

// TestMaprHandlerWriteResultIndependentOfChunking feeds one stream through
// MaprHandler.Write in chunks of many sizes, which split frames and escape
// sequences everywhere, and checks that the result is byte for byte the one
// of merging every server set into the global group set on its own, in
// order: the client result the handler gave before it batched the messages
// of a Write call.
func TestMaprHandlerWriteResultIndependentOfChunking(t *testing.T) {
	query, err := mapr.NewQuery(batchTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	stream, sets := batchTestStream(t, query)

	reference := mapr.NewGlobalGroupSet(logging.NopLogger{})
	for _, s := range sets {
		var err error
		reference.Update(func(group *mapr.GroupSet) {
			err = group.GetSet(s.groupKey).Merge(query, s.set)
		})
		if err != nil {
			t.Fatalf("Merge() error = %v", err)
		}
	}
	want := renderBatchResult(t, query, reference)

	for _, chunkSize := range []int{1, 2, 3, 5, 64, 4096, len(stream)} {
		session := maprclient.NewSessionState(query, logging.NopLogger{})
		logger := &recordingClientLogger{}
		handler := NewMaprHandler("srv1", session, logger)
		for chunk := stream; len(chunk) > 0; {
			n := min(len(chunk), chunkSize)
			_, _ = handler.Write(chunk[:n])
			chunk = chunk[n:]
		}
		handler.Shutdown()
		if got := renderBatchResult(t, query, session.Snapshot().GlobalGroup); got != want {
			t.Errorf("chunk size %d: result\n%s\nwant\n%s", chunkSize, got, want)
		}
		if len(logger.errors) > 0 {
			t.Errorf("chunk size %d: errors %q", chunkSize, logger.errors)
		}
		if failures := handler.Outcome().Failures; len(failures) != 1 {
			t.Errorf("chunk size %d: failures %q, want the marked control frame's one", chunkSize, failures)
		}
	}
}

func renderBatchResult(t *testing.T, query *mapr.Query, group *mapr.GlobalGroupSet) string {
	t.Helper()
	result, _, err := group.Result(query, -1, nil)
	if err != nil {
		t.Fatalf("Result() error = %v", err)
	}
	return result
}

// TestMaprHandlerWriteDropsNewlinesOfFrames verifies the newline handling of
// the mapr handler: newline bytes anywhere in a frame are dropped before it
// is classified, a frame of newlines only is ignored (it does not even decide
// the session framing), and a payload frame is written with one newline.
func TestMaprHandlerWriteDropsNewlinesOfFrames(t *testing.T) {
	query, err := mapr.NewQuery("select count(status) from stats group by status", logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	session := maprclient.NewSessionState(query, logging.NopLogger{})
	recorder := &payloadRecorder{}
	handler := NewMaprHandler("srv1", session, recorder)
	countStorage := handlerCountStorage(t, query)

	var stream bytes.Buffer
	stream.WriteString("\n\n")
	stream.WriteByte(protocol.MessageDelimiter)
	protocol.RawFraming.EncodeControl(&stream, protocol.HiddenCapabilitiesPrefix+protocol.CapabilityEscapedFramingV1)
	stream.WriteString("\nAGG\nREGATE|srv1|ERROR∥2∥" + countStorage + "≔\n2∥\n")
	stream.WriteByte(protocol.MessageDelimiter)
	stream.WriteString("pay\nload\n\n")
	stream.WriteByte(protocol.MessageDelimiter)
	if _, err := handler.Write(stream.Bytes()); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	handler.Shutdown()

	if got := handler.framing(); got != protocol.EscapedFraming {
		t.Fatalf("framing = %v, want %v (decided by the capabilities frame)", got, protocol.EscapedFraming)
	}
	if got, want := recorder.payload.String(), "payload\n"; got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
	set := session.Snapshot().GlobalGroup.GetSet("ERROR")
	if set.Samples != 2 || set.FValues[countStorage] != 2 {
		t.Fatalf("set = %v, want 2 samples and count 2", set)
	}
}

// TestMaprHandlerWriteDoesNotModifyInput verifies that Write never changes
// the bytes it is given (io.Writer), also when it drops newlines.
func TestMaprHandlerWriteDoesNotModifyInput(t *testing.T) {
	query, err := mapr.NewQuery(batchTestQuery, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery() error = %v", err)
	}
	stream, _ := batchTestStream(t, query)
	input := bytes.Clone(stream)
	handler := NewMaprHandler("srv1", maprclient.NewSessionState(query, logging.NopLogger{}), clientlog.NopLogger{})
	_, _ = handler.Write(input)
	if !bytes.Equal(input, stream) {
		t.Fatalf("Write() modified its input")
	}
}
