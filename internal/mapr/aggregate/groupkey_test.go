package aggregate

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/protocol"
)

func TestCompoundGroupCountsAcrossFilesAndBatches(t *testing.T) {
	tests := []struct {
		name   string
		fields string
		lines  []string
	}{
		{"commas", "a,b", []string{"a=x,y|b=z", "a=x|b=y,z"}},
		{"empty", "a,b", []string{"a=,|b=", "a=|b=,", "a=|b="}},
		{"unicode", "a,b", []string{"a=日本,語|b=λ", "a=日本|b=語,λ"}},
		{"escape literals", "a,b", []string{"a=x,y|b=z", "a=x%2Cy|b=z", "a=x%252Cy|b=z"}},
		{"three columns", "a,b,c", []string{"a=x,y|b=|c=z", "a=x|b=y,|c=z", "a=x|b=y|c=,z"}},
		{"ordinary", "a,b", []string{"a=alpha|b=orange", "a=beta|b=orange"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newAggregateFromTextForTest("select "+tt.fields+",count(*) group by "+tt.fields+" logformat generickv", logging.NopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			var first map[string]*mapr.AggregateSet
			for file := 0; file < 2; file++ {
				p := NewProcessor(a, "file")
				for batch := 0; batch < 2; batch++ {
					feedLines(t, p, tt.lines...)
					if err := p.Flush(); err != nil {
						t.Fatal(err)
					}
					assertCompoundCounts(t, a.serializer.groupSets, len(tt.lines), file*2+batch+1)
					if file == 0 && batch == 0 {
						first = make(map[string]*mapr.AggregateSet)
						for key, set := range a.serializer.groupSets {
							first[key] = &mapr.AggregateSet{SValues: make(map[string]string)}
							for field, value := range set.SValues {
								first[key].SValues[field] = value
							}
						}
					}
				}
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
			for key, set := range first {
				if !reflect.DeepEqual(a.serializer.groupSets[key].SValues, set.SValues) {
					t.Fatalf("group %q changed selected values after scratch reuse", key)
				}
			}
		})
	}
}

func TestCompoundGroupTransportAndServerMerge(t *testing.T) {
	const queryText = "select a,b,count(*) group by a,b logformat generickv"
	query, err := mapr.NewQuery(queryText, logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	state := maprclient.NewSessionState(query, logging.NopLogger{})
	var previousResult string
	for _, server := range []string{"server-a", "server-b"} {
		a, err := newAggregateFromTextForTest(queryText, logging.NopLogger{})
		if err != nil {
			t.Fatal(err)
		}
		client := maprclient.NewAggregate(server, state, logging.NopLogger{})
		messages := make(chan string, 4)
		a.serializer.prepareOutput(messages)
		for range 2 {
			p := NewProcessor(a, "file")
			feedLines(t, p, "a=x,y|b=z", "a=x|b=y,z", "a=x%2Cy|b=z", "a=αβ|b=λ")
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			a.serializer.serialize(context.Background())
			for len(messages) > 0 {
				var wire bytes.Buffer
				// Sessions of current clients and servers use escaped framing.
				protocol.EscapedFraming.EncodeMessage(&wire, protocol.Message{Kind: protocol.MessageAggregate,
					Hostname: server, Content: <-messages})
				frame := wire.Bytes()
				if bytes.Count(frame, []byte{protocol.MessageDelimiter}) != 1 {
					t.Fatalf("group key corrupted stream framing: %q", frame)
				}
				var scratch []byte
				body := protocol.EscapedFraming.DecodeFrame(frame[:len(frame)-1], &scratch)
				message, err := protocol.DecodeMessage(string(body))
				if err != nil {
					t.Fatal(err)
				}
				if err := client.Add([]byte(message.Content)); err != nil {
					t.Fatalf("legacy opaque-key decoder rejected aggregate: %v", err)
				}
			}
		}
		if err := client.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	// Safe keys from an older server retain their spelling and must merge
	// with the corresponding new-server group. This fixture is the legacy
	// wire format, independent of the server's current key builder.
	legacy := maprclient.NewAggregate("legacy", state, logging.NopLogger{})
	if err := legacy.Add([]byte("αβ,λ∥1∥count(*)≔1∥a≔αβ∥b≔λ∥")); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Flush(); err != nil {
		t.Fatal(err)
	}
	global := state.Snapshot().GlobalGroup
	if got := global.NumSets(); got != 4 {
		t.Fatalf("merged groups = %d, want 4", got)
	}
	for key, tuple := range map[string][2]string{
		"x%2Cy,z": {"x,y", "z"}, "x,y%2Cz": {"x", "y,z"},
		"x%252Cy,z": {"x%2Cy", "z"}, "αβ,λ": {"αβ", "λ"},
	} {
		set := global.GetSet(key)
		count := 4
		if key == "αβ,λ" {
			count++
		}
		assertCompoundCounts(t, map[string]*mapr.AggregateSet{key: set}, 1, count)
		if set.SValues["a"] != tuple[0] || set.SValues["b"] != tuple[1] {
			t.Fatalf("group %q selected values = %v, want %v", key, set.SValues, tuple)
		}
	}
	for range 5 {
		result, rows, err := global.Result(query, 10, nil)
		if err != nil || rows != 4 {
			t.Fatalf("Result: rows=%d, err=%v", rows, err)
		}
		if previousResult != "" && previousResult != result {
			t.Fatalf("unstable result order: %q versus %q", previousResult, result)
		}
		previousResult = result
	}
}

func TestCompoundGroupLegacySafeKeysAndScratchReuse(t *testing.T) {
	fields := map[string]string{"a": "x,y%", "b": "z"}
	buffer := make([]byte, 0, 128)
	key := buildGroupKey(buffer, []string{"a", "b"}, fields)
	if string(key) != "x%2Cy%25,z" || &key[0] != &buffer[:cap(buffer)][0] {
		t.Fatalf("escaped key failed to reuse scratch: %q", key)
	}
	if allocs := testing.AllocsPerRun(100, func() {
		key = buildGroupKey(key[:0], []string{"a", "b"}, fields)
	}); allocs != 0 {
		t.Fatalf("reused compound key allocated %v times per line", allocs)
	}
	fields["a"] = "alpha"
	key = buildGroupKey(key[:0], []string{"a", "b"}, fields)
	if string(key) != "alpha,z" {
		t.Fatalf("ordinary key changed legacy spelling: %q", key)
	}
	fields["a"] = "x,y%"
	key = buildGroupKey(key[:0], []string{"a"}, fields)
	if string(key) != fields["a"] {
		t.Fatalf("single-field key changed legacy spelling: %q", key)
	}
}

func assertCompoundCounts(t *testing.T, sets map[string]*mapr.AggregateSet, groups, count int) {
	t.Helper()
	if len(sets) != groups {
		t.Fatalf("distinct tuples produced %d groups, want %d: %v", len(sets), groups, sets)
	}
	for key, set := range sets {
		if set.Samples != count {
			t.Fatalf("group %q Samples=%d, want %d", key, set.Samples, count)
		}
		if value := set.FValues["count(*)"]; value != float64(count) {
			t.Fatalf("group %q count(*)=%v, want %d", key, value, count)
		}
	}
}
