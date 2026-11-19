package handlers

import (
	"bytes"
	"context"
	"strconv"
	"testing"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

// benchmarkMaprQuery is the high-cardinality query of the dmap measurements
// (one group per user).
const benchmarkMaprQuery = "from STATS select count(user),sum(currentConnections),last(user) group by user"

// maprBenchmarkStream returns the server-to-client stream of a dserver that
// marks control frames and sends one aggregate frame per group of groups
// users, intervals times.
func maprBenchmarkStream(b *testing.B, query *mapr.Query, groups, intervals int) []byte {
	b.Helper()
	var stream bytes.Buffer
	protocol.RawFraming.EncodeControl(&stream, protocol.HiddenCapabilitiesPrefix+
		protocol.CapabilityEscapedFramingV1+" "+protocol.CapabilityControlFramesV1)
	framing := protocol.EscapedControlFraming
	for interval := range intervals {
		for group := range groups {
			user := "u" + strconv.Itoa(group)
			set := mapr.NewAggregateSet()
			set.Samples = 2
			for _, sc := range query.Select {
				switch sc.Operation {
				case mapr.Count:
					set.FValues[sc.FieldStorage] = 2
				case mapr.Sum:
					set.FValues[sc.FieldStorage] = float64(group%7) + 0.25*float64(interval)
				default:
					set.SValues[sc.FieldStorage] = user
				}
			}
			framing.EncodeMessage(&stream, protocol.Message{
				Kind: protocol.MessageAggregate, Hostname: "srv1", Content: serializeSet(b, set, user),
			})
		}
	}
	return stream.Bytes()
}

// BenchmarkMaprHandlerWrite feeds a high-cardinality dmap stream (20000
// groups, two intervals) through MaprHandler.Write in the 32 KiB chunks
// io.Copy hands it, and flushes it like a session end.
func BenchmarkMaprHandlerWrite(b *testing.B) {
	query, err := mapr.NewQuery(benchmarkMaprQuery, logging.NopLogger{})
	if err != nil {
		b.Fatalf("NewQuery() error = %v", err)
	}
	stream := maprBenchmarkStream(b, query, 20000, 2)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	for b.Loop() {
		session := maprclient.NewSessionState(query, logging.NopLogger{})
		handler := NewMaprHandler("srv1", session, clientlog.NopLogger{})
		for chunk := stream; len(chunk) > 0; {
			n := min(len(chunk), 32*1024)
			_, _ = handler.Write(chunk[:n])
			chunk = chunk[n:]
		}
		handler.Shutdown()
		if got := session.Snapshot().GlobalGroup.NumSets(); got != 20000 {
			b.Fatalf("NumSets() = %d, want 20000", got)
		}
	}
}

// serializeSet returns the content of the aggregate frame a dserver sends for
// set (AggregateSet.Serialize).
func serializeSet(tb testing.TB, set *mapr.AggregateSet, groupKey string) string {
	tb.Helper()
	ch := make(chan string, 1)
	if !set.Serialize(context.Background(), groupKey, ch, logging.NopLogger{}) {
		tb.Fatalf("Serialize() did not send the set")
	}
	return <-ch
}
