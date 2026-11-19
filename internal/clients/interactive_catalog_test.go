package clients

import (
	"reflect"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

// catalogTestFiles are the probed files of the catalog tests' answers.
var catalogTestFiles = []string{"/var/log/b.log", "/var/log/a.log"}

func catalogTestAnswer(defaultFormat string, fields map[string][]string) protocol.ProbeAnswer {
	return protocol.ProbeAnswer{
		DefaultLogFormat: defaultFormat,
		FilesSampled:     1,
		Formats: map[string]protocol.ProbeFormat{"default": {
			Tables:    map[string]map[string][]string{"STATS": fields},
			Variables: map[string][]string{"$hostname": {"host"}},
			Note:      "note",
		}},
	}
}

// TestFieldCatalogMergesServers: the snapshot is the union of the servers'
// latest answers: fields with their distinct samples (at most
// catalogMaxSamples), variables, distinct notes, errors with their server,
// the default log format most servers name, the source servers and the time
// of the latest answer.
func TestFieldCatalogMergesServers(t *testing.T) {
	catalog := newFieldCatalog()
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	catalog.store("srv2", catalogTestFiles, catalogTestAnswer("default", map[string][]string{
		"a": {"1", "2", "3"}, "b": {}}), start)
	srv1 := catalogTestAnswer("csv", map[string][]string{"a": {"3", "4", "5", "6", "7"}, "c": {"x"}})
	srv1.Truncated = true
	srv1.Errors = []protocol.ProbeError{{File: "/x", Error: protocol.CommandFailurePermission}}
	catalog.store("srv1", catalogTestFiles, srv1, start.Add(time.Second))
	catalog.store("srv3", catalogTestFiles, catalogTestAnswer("default", nil), start.Add(-time.Second))

	snap := catalog.snapshot(catalogTestFiles)
	if !reflect.DeepEqual(snap.Servers, []string{"srv1", "srv2", "srv3"}) {
		t.Fatalf("servers = %q", snap.Servers)
	}
	if snap.DefaultLogFormat != "default" || !snap.Updated.Equal(start.Add(time.Second)) || !snap.Truncated {
		t.Fatalf("snapshot = %+v", snap)
	}
	format := snap.Formats["default"]
	wantFields := map[string][]string{"a": {"3", "4", "5", "6", "7"}, "b": {}, "c": {"x"}}
	if !reflect.DeepEqual(format.Tables["STATS"], wantFields) {
		t.Fatalf("fields = %q, want %q", format.Tables["STATS"], wantFields)
	}
	if !reflect.DeepEqual(format.Variables, map[string][]string{"$hostname": {"host"}}) ||
		!reflect.DeepEqual(format.Notes, []string{"note"}) {
		t.Fatalf("variables %q, notes %q", format.Variables, format.Notes)
	}
	wantErrors := []catalogError{{Server: "srv1", File: "/x", Error: protocol.CommandFailurePermission}}
	if !reflect.DeepEqual(snap.Errors, wantErrors) {
		t.Fatalf("errors = %+v, want %+v", snap.Errors, wantErrors)
	}
}

// TestFieldCatalogDefaultLogFormatTie: on a tie the format that reached
// the top count first, going through the servers sorted by name, wins; that
// is not necessarily the first server's format.
func TestFieldCatalogDefaultLogFormatTie(t *testing.T) {
	for _, tt := range []struct {
		name    string
		formats map[string]string // server -> default log format
		want    string
	}{
		{name: "one each", formats: map[string]string{"b": "default", "a": "csv"}, want: "csv"},
		{name: "second format reaches two first",
			formats: map[string]string{"a": "csv", "b": "default", "c": "default", "d": "csv"}, want: "default"},
		{name: "majority", formats: map[string]string{"a": "csv", "b": "default", "c": "default"}, want: "default"},
	} {
		catalog := newFieldCatalog()
		for server, format := range tt.formats {
			catalog.store(server, catalogTestFiles, catalogTestAnswer(format, nil), time.Now())
		}
		if got := catalog.snapshot(catalogTestFiles).DefaultLogFormat; got != tt.want {
			t.Errorf("%s: default log format = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestFieldCatalogFilters: a snapshot merges only the answers about the
// files it is asked for, in any order; answers about other files (before a
// :reload) are kept for when the session reads those again. A snapshot for
// files no server answered about is empty.
func TestFieldCatalogFilters(t *testing.T) {
	catalog := newFieldCatalog()
	start := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	catalog.store("old", []string{"/var/log/a.log"},
		catalogTestAnswer("csv", map[string][]string{"stale": {"1"}}), start.Add(time.Minute))
	catalog.store("new", catalogTestFiles, catalogTestAnswer("default", map[string][]string{"x": {"2"}}), start)

	snap := catalog.snapshot([]string{"/var/log/a.log", "/var/log/b.log"})
	if !reflect.DeepEqual(snap.Servers, []string{"new"}) || snap.DefaultLogFormat != "default" ||
		!snap.Updated.Equal(start) || snap.Formats["default"].Tables["STATS"]["stale"] != nil {
		t.Fatalf("snapshot of the current files = %+v, want the answer of new only", snap)
	}
	if snap := catalog.snapshot([]string{"/var/log/a.log"}); !reflect.DeepEqual(snap.Servers, []string{"old"}) {
		t.Fatalf("snapshot of the old files: servers %q, want old", snap.Servers)
	}
	if snap := catalog.snapshot([]string{"/other.log"}); len(snap.Servers) != 0 || len(snap.Formats) != 0 ||
		!snap.Updated.IsZero() {
		t.Fatalf("snapshot of unprobed files = %+v, want it empty", snap)
	}
}

// TestFieldCatalogReplacesAnswers: a server's new answer replaces its old
// one (fields of files no longer read disappear), also one with errors
// only, except an answer that only says that another probe was running.
func TestFieldCatalogReplacesAnswers(t *testing.T) {
	catalog := newFieldCatalog()
	catalog.store("srv", catalogTestFiles, catalogTestAnswer("default", map[string][]string{"old": {"1"}}), time.Now())
	busy := protocol.ProbeAnswer{Errors: []protocol.ProbeError{{Error: protocol.ProbeErrorBusy}}}
	catalog.store("srv", catalogTestFiles, busy, time.Now())
	if fields := catalog.snapshot(catalogTestFiles).Formats["default"].Tables["STATS"]; fields["old"] == nil {
		t.Fatalf("busy answer replaced the old one: %q", fields)
	}

	catalog.store("srv", catalogTestFiles, catalogTestAnswer("default", map[string][]string{"new": {"2"}}), time.Now())
	fields := catalog.snapshot(catalogTestFiles).Formats["default"].Tables["STATS"]
	if !reflect.DeepEqual(fields, map[string][]string{"new": {"2"}}) {
		t.Fatalf("fields = %q, want the new answer's only", fields)
	}

	failed := protocol.ProbeAnswer{Errors: []protocol.ProbeError{
		{Error: protocol.ProbeErrorBusy}, {File: "/gone", Error: protocol.CommandFailureNoFile}}}
	catalog.store("srv", catalogTestFiles, failed, time.Now())
	snap := catalog.snapshot(catalogTestFiles)
	if len(snap.Formats) != 0 || len(snap.Errors) != 2 {
		t.Fatalf("snapshot after a failed probe = %+v, want its errors only", snap)
	}
}

// TestFieldCatalogSnapshotIsACopy: changing a snapshot changes neither the
// catalog nor later snapshots.
func TestFieldCatalogSnapshotIsACopy(t *testing.T) {
	catalog := newFieldCatalog()
	answer := catalogTestAnswer("default", map[string][]string{"a": {"1"}})
	catalog.store("srv", catalogTestFiles, answer, time.Now())
	snap := catalog.snapshot(catalogTestFiles)
	snap.Formats["default"].Tables["STATS"]["a"][0] = "changed"
	snap.Formats["default"].Tables["STATS"]["new"] = nil
	snap.Formats["default"].Variables["$hostname"][0] = "changed"

	again := catalog.snapshot(catalogTestFiles).Formats["default"]
	if again.Tables["STATS"]["a"][0] != "1" || len(again.Tables["STATS"]) != 1 ||
		again.Variables["$hostname"][0] != "host" || answer.Formats["default"].Tables["STATS"]["a"][0] != "1" {
		t.Fatalf("snapshot shares state with the catalog: %+v", again)
	}
}

func TestFieldCatalogEmpty(t *testing.T) {
	snap := newFieldCatalog().snapshot(catalogTestFiles)
	if len(snap.Servers) != 0 || len(snap.Formats) != 0 || !snap.Updated.IsZero() || snap.DefaultLogFormat != "" {
		t.Fatalf("empty catalog snapshot = %+v", snap)
	}
}
