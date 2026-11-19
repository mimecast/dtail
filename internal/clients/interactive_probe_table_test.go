package clients

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mimecast/dtail/internal/protocol"
)

// tableTestAnswer is a table-targeted answer for table with one field and
// the $caller of the table's lines.
func tableTestAnswer(table, field, caller string, lines int) protocol.ProbeAnswer {
	return protocol.ProbeAnswer{
		DefaultLogFormat: "default", Table: table, FilesSampled: 1, LinesSampled: lines,
		Formats: map[string]protocol.ProbeFormat{"default": {
			Tables:         map[string]map[string][]string{table: {field: {"1"}}},
			TableVariables: map[string]map[string][]string{table: {"$caller": {caller}}},
		}},
	}
}

// TestFieldCatalogMergesTableProbes: a table-targeted answer adds its
// table, fields and per-table $variables to the snapshot of the same files,
// without touching the format-wide $variables, and the snapshot tells
// which tables were probed on their own with how many lines.
func TestFieldCatalogMergesTableProbes(t *testing.T) {
	catalog := newFieldCatalog()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	catalog.store("srv1", catalogTestFiles, catalogTestAnswer("default", map[string][]string{"a": {"1"}}), now)
	catalog.storeTable("srv1", catalogTestFiles, "ORDERS", tableTestAnswer("ORDERS", "orderID", "orders.go:10", 3), now)
	catalog.storeTable("srv2", catalogTestFiles, "ORDERS", tableTestAnswer("ORDERS", "amount", "orders.go:11", 2),
		now.Add(time.Second))
	catalog.storeTable("srv1", []string{"/other.log"}, "OTHER", tableTestAnswer("OTHER", "x", "o.go:1", 1), now)

	snap := catalog.snapshot(catalogTestFiles)
	format := snap.Formats["default"]
	if got := slices.Sorted(maps.Keys(format.Tables)); !reflect.DeepEqual(got, []string{"ORDERS", "STATS"}) {
		t.Fatalf("tables = %v, want ORDERS added to STATS (not OTHER of other files)", got)
	}
	if got := slices.Sorted(maps.Keys(format.Tables["ORDERS"])); !reflect.DeepEqual(got, []string{"amount", "orderID"}) {
		t.Errorf("ORDERS fields = %v, want both servers' fields", got)
	}
	if got := format.variablesOf("ORDERS")["$caller"]; !reflect.DeepEqual(got, []string{"orders.go:10", "orders.go:11"}) {
		t.Errorf("ORDERS $caller = %v", got)
	}
	if _, ok := format.Variables["$caller"]; ok {
		t.Errorf("format-wide variables got the table's: %v", format.Variables)
	}
	// STATS was answered by a plain probe of one table: its variables are
	// the format-wide ones.
	if got := format.variablesOf("STATS"); !reflect.DeepEqual(got, map[string][]string{"$hostname": {"host"}}) {
		t.Errorf("STATS variables = %v", got)
	}
	if !reflect.DeepEqual(snap.Servers, []string{"srv1", "srv2"}) || !snap.Updated.Equal(now.Add(time.Second)) {
		t.Errorf("servers %v updated %v", snap.Servers, snap.Updated)
	}
	if got := snap.TableProbes; !reflect.DeepEqual(got, map[string]int{"ORDERS": 5}) {
		t.Errorf("table probes = %v, want ORDERS with 5 lines", got)
	}
}

// TestFieldCatalogTableProbeFreshness: hasTableProbe is true for a table
// probed for the same files within tableProbeMaxAge, false for other files,
// a busy answer or an older one.
func TestFieldCatalogTableProbeFreshness(t *testing.T) {
	catalog := newFieldCatalog()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	busy := protocol.ProbeAnswer{Table: "BUSY", Errors: []protocol.ProbeError{{Error: protocol.ProbeErrorBusy}}}
	catalog.storeTable("srv1", catalogTestFiles, "BUSY", busy, now)
	catalog.storeTable("srv1", catalogTestFiles, "ORDERS", tableTestAnswer("ORDERS", "a", "c", 1), now)
	// A server that does not know table probes answers a plain probe: it
	// is stored for the table all the same, so it is not asked again.
	catalog.storeTable("srv1", catalogTestFiles, "OLD", catalogTestAnswer("default", nil), now)

	tests := []struct {
		files []string
		table string
		at    time.Time
		want  bool
	}{
		{catalogTestFiles, "ORDERS", now.Add(tableProbeMaxAge - time.Second), true},
		{catalogTestFiles, "OLD", now, true},
		{catalogTestFiles, "ORDERS", now.Add(tableProbeMaxAge + time.Second), false},
		{[]string{"/other.log"}, "ORDERS", now, false},
		{catalogTestFiles, "BUSY", now, false},
		{catalogTestFiles, "MISSING", now, false},
	}
	for _, tt := range tests {
		if got := catalog.hasTableProbe(tt.files, tt.table, tt.at); got != tt.want {
			t.Errorf("hasTableProbe(%v, %s, %v) = %v, want %v", tt.files, tt.table, tt.at, got, tt.want)
		}
	}
}

// TestFieldCatalogBoundsTableProbes: the catalog keeps at most
// catalogMaxTableProbes table answers, dropping the oldest.
func TestFieldCatalogBoundsTableProbes(t *testing.T) {
	catalog := newFieldCatalog()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for i := range catalogMaxTableProbes + 5 {
		table := fmt.Sprintf("T%03d", i)
		catalog.storeTable("srv1", catalogTestFiles, table, tableTestAnswer(table, "f", "c", 1),
			now.Add(time.Duration(i)*time.Millisecond))
	}
	if got := len(catalog.tableAnswers); got != catalogMaxTableProbes {
		t.Fatalf("%d table answers kept, want %d", got, catalogMaxTableProbes)
	}
	last := now.Add(time.Duration(catalogMaxTableProbes+4) * time.Millisecond)
	if catalog.hasTableProbe(catalogTestFiles, "T000", last) {
		t.Error("the oldest table answer was kept")
	}
	if !catalog.hasTableProbe(catalogTestFiles, fmt.Sprintf("T%03d", catalogMaxTableProbes+4), last) {
		t.Error("the newest table answer was dropped")
	}
}

// tableRequest is requestTable without its started result.
func tableRequest(p *interactiveProber, table string) <-chan struct{} {
	done, _ := p.requestTable(table)
	return done
}

// TestMergeCatalogFormatTableVariables: per-table $variables are merged per
// table; a table with per-table data but no entry has none (never the
// format-wide ones); only an answer without per-table data contributes its
// Variables to each of its tables.
func TestMergeCatalogFormatTableVariables(t *testing.T) {
	merged := mergeCatalogFormat(catalogFormat{}, protocol.ProbeFormat{
		Tables: map[string]map[string][]string{"A": {}, "B": {}},
		TableVariables: map[string]map[string][]string{
			"A": {"$x": {"1"}},
		},
		Variables: map[string][]string{"$x": {"1"}, "$y": {"2"}},
	})
	merged = mergeCatalogFormat(merged, protocol.ProbeFormat{
		Tables:    map[string]map[string][]string{"C": {}},
		Variables: map[string][]string{"$z": {"3"}},
	})
	if got := merged.variablesOf("A"); !reflect.DeepEqual(got, map[string][]string{"$x": {"1"}}) {
		t.Errorf("A = %v", got)
	}
	if got := merged.variablesOf("C"); !reflect.DeepEqual(got, map[string][]string{"$z": {"3"}}) {
		t.Errorf("C = %v, want the Variables of its answer without per-table data", got)
	}
	if got := merged.variablesOf("B"); got == nil || len(got) != 0 {
		t.Errorf("B = %#v, want none (its answer gave per-table data without B)", got)
	}
	if got := (catalogFormat{Variables: map[string][]string{"$v": {}}}).variablesOf("X"); got["$v"] == nil {
		t.Errorf("format without any per-table data: %v, want the format-wide variables", got)
	}
}

// TestTableProbeOfSilentServer: with a server that never answers, only the
// Tab that sends the table probe waits (listWait); later Tabs while it is
// on its way complete at once with a hint, and once it failed (after the
// round timeout) the table is not asked for again within
// tableProbeRetryAfter, then it is.
func TestTableProbeOfSilentServer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		handler.block = true
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		c.Args = queryArgs
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		pause := c.newInteractivePause()
		line := ":reload --query 'from orders select "
		tab := func() (completion, time.Duration) {
			start := time.Now()
			result := c.completeEditorLine(nil, pause, line, len(line))
			return result, time.Since(start)
		}
		tableRequests := func() int {
			return len(handler.requestsWhere(func(r protocol.ProbeRequest) bool { return r.Table != "" }))
		}

		first, waited := tab()
		if waited != c.prober.listWait || !strings.Contains(first.hint, "probe of table ORDERS") {
			t.Fatalf("first Tab waited %v with hint %q, want %v and the hint", waited, first.hint, c.prober.listWait)
		}
		second, waited := tab()
		if waited != 0 || !strings.Contains(second.hint, "probe of table ORDERS") {
			t.Fatalf("Tab while pending waited %v with hint %q, want no wait and the hint", waited, second.hint)
		}
		time.Sleep(c.prober.roundTimeout)
		synctest.Wait()
		third, waited := tab()
		if waited != 0 || strings.Contains(third.hint, "probe of table") || tableRequests() != 1 {
			t.Fatalf("Tab after the failure waited %v, hint %q, %d requests; want none, no hint, 1 request",
				waited, third.hint, tableRequests())
		}
		time.Sleep(tableProbeRetryAfter + time.Second)
		if _, waited = tab(); waited != c.prober.listWait || tableRequests() != 2 {
			t.Fatalf("Tab after the retry delay waited %v with %d requests, want a new request",
				waited, tableRequests())
		}
	})
}

// TestRequestTableStarted: only the call that sends a table probe reports
// it as started; a call while it is on its way gets the same channel.
func TestRequestTableStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		handler.block = true
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		first, started := c.prober.requestTable("ORDERS")
		second, again := c.prober.requestTable("ORDERS")
		if first == nil || !started || second != first || again {
			t.Fatalf("requests = %v/%v, %v/%v; want one started request shared", first, started, second, again)
		}
	})
}

// TestInteractiveProberRequestTable: a table probe is sent with the
// session's files and the table to the probe targets, once while it is on
// its way, and not again while its answer is fresh; the answer is in the
// catalog snapshot.
func TestInteractiveProberRequestTable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		c, logger := newProbeTestClient([]string{"/var/log/a.log", "-"}, conn)
		if tableRequest(c.prober, "ORDERS") != nil {
			t.Fatal("a prober not started asked for a table")
		}
		ctx, cancel := context.WithCancel(t.Context())
		c.prober.start(ctx)
		synctest.Wait()

		first := tableRequest(c.prober, "ORDERS")
		if first == nil || tableRequest(c.prober, "ORDERS") != first {
			t.Fatal("a table probe on its way was asked for again")
		}
		<-first
		synctest.Wait()
		if tableRequest(c.prober, "ORDERS") != nil {
			t.Fatal("a fresh table probe was asked for again")
		}
		requests := handler.requestsWhere(func(r protocol.ProbeRequest) bool { return r.Table != "" })
		if len(requests) != 1 || requests[0].Table != "ORDERS" ||
			!reflect.DeepEqual(requests[0].Files, []string{"/var/log/a.log"}) {
			t.Fatalf("table requests = %+v, want one for ORDERS of /var/log/a.log", requests)
		}
		snap := c.prober.catalogSnapshot()
		if _, ok := snap.Formats["default"].Tables["ORDERS"]; !ok || snap.TableProbes["ORDERS"] == 0 {
			t.Fatalf("catalog = %+v, want the ORDERS answer", snap)
		}
		time.Sleep(tableProbeMaxAge + time.Second)
		if again := tableRequest(c.prober, "ORDERS"); again == nil {
			t.Fatal("an expired table probe was not asked for again")
		} else {
			<-again
		}
		cancel()
		c.prober.wait()
		if tableRequest(c.prober, "OTHER") != nil {
			t.Fatal("a stopped prober asked for a table")
		}
		logger.requireSilent(t)
	})
	var nilProber *interactiveProber
	if tableRequest(nilProber, "ORDERS") != nil {
		t.Fatal("a nil prober asked for a table")
	}
}

// TestInteractiveProberRequestTableWithoutFiles: a session reading stdin
// only has nothing to probe, and an invalid table name is not sent.
func TestInteractiveProberRequestTableWithoutFiles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"-"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		if tableRequest(c.prober, "ORDERS") != nil {
			t.Fatal("a table probe without files was sent")
		}
		c.storeReloadState(c.Args, SessionSpec{Files: []string{"/a.log"}})
		if tableRequest(c.prober, "A B") != nil || tableRequest(c.prober, strings.Repeat("T", 100)) != nil {
			t.Fatal("an invalid table was sent")
		}
		synctest.Wait()
		if got := handler.requestsWhere(func(r protocol.ProbeRequest) bool { return r.Table != "" }); len(got) != 0 {
			t.Fatalf("table requests = %+v, want none", got)
		}
	})
}

// TestFieldsCommandAwaitsTableProbe: :fields orders asks for a table probe
// of ORDERS (upper case, as a query names it) with the output running while
// it waits, then shows its answer and leaves the output paused; a second
// :fields orders while the answer is fresh asks nothing.
func TestFieldsCommandAwaitsTableProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		pause := c.newInteractivePause()
		pause.pause()

		var tty strings.Builder
		for range 2 {
			if _, err := c.handleInteractiveLine(&tty, ":fields orders", pause); err != nil {
				t.Fatal(err)
			}
		}
		requests := handler.requestsWhere(func(r protocol.ProbeRequest) bool { return r.Table != "" })
		if len(requests) != 1 || requests[0].Table != "ORDERS" {
			t.Fatalf("table requests = %+v, want one for ORDERS", requests)
		}
		if !pause.paused {
			t.Error(":fields did not leave the output paused")
		}
		if out := tty.String(); !strings.Contains(out, "table ORDERS probed on its own") ||
			!strings.Contains(out, "caller.go:1") {
			t.Fatalf(":fields output = %q, want the table probe's answer", out)
		}
	})
}

// TestEditorTabAsksForTableProbe: Tab in a --query value naming a from
// table asks for a table probe of it in the background; Tab in a query
// without one asks nothing.
func TestEditorTabAsksForTableProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn, handler := probeTestConnection("srv1")
		c, _ := newProbeTestClient([]string{"/var/log/a.log"}, conn)
		c.Args = queryArgs
		ctx, cancel := context.WithCancel(t.Context())
		defer func() { cancel(); c.prober.wait() }()
		c.prober.start(ctx)
		synctest.Wait()
		pause := c.newInteractivePause()

		var result completion
		for _, line := range []string{":reload --query 'select ", ":reload --query 'from orders select "} {
			result = c.completeEditorLine(nil, pause, line, len(line))
		}
		synctest.Wait()
		requests := handler.requestsWhere(func(r protocol.ProbeRequest) bool { return r.Table != "" })
		if len(requests) != 1 || requests[0].Table != "ORDERS" {
			t.Fatalf("table requests = %+v, want one for ORDERS", requests)
		}
		// The Tab waited for the answer and completed with its field.
		if !slices.Contains(result.candidates, "field ") {
			t.Fatalf("candidates = %q, want the field of the ORDERS answer", result.candidates)
		}
	})
}

// TestCatalogTableName::fields matches a table of the catalog ignoring
// case and names an unknown one in upper case.
func TestCatalogTableName(t *testing.T) {
	if got := catalogTableName(queryCatalog, "stats"); got != "STATS" {
		t.Errorf("stats = %q", got)
	}
	mixed := catalogSnapshot{Formats: map[string]catalogFormat{"default": {
		Tables: map[string]map[string][]string{"MixedCase": {}}}}}
	if got := catalogTableName(mixed, "mixedcase"); got != "MixedCase" {
		t.Errorf("mixedcase = %q, want the catalog's spelling", got)
	}
	if got := catalogTableName(catalogSnapshot{}, "orders"); got != "ORDERS" {
		t.Errorf("orders = %q", got)
	}
}

// TestCompleteQueryValueNamesTheTable:a --query value naming a table
// reports it, so that the editor asks for a table probe of it.
func TestCompleteQueryValueNamesTheTable(t *testing.T) {
	tests := map[string]string{
		":reload --query 'from stats select ": "STATS",
		":reload --query 'select ":            "",
		":reload --query 'from ":              "",
	}
	for line, want := range tests {
		result, ok := completeQueryValue(line, len(line), queryArgs, catalogOf(queryCatalog))
		if !ok || result.probeTable != want {
			t.Errorf("completeQueryValue(%q) table = %q (%v), want %q", line, result.probeTable, ok, want)
		}
	}
}

// TestFieldsTextShowsTableProbes: :fields TABLE says when the table was
// probed on its own.
func TestFieldsTextShowsTableProbes(t *testing.T) {
	catalog := newFieldCatalog()
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	catalog.store("srv1", catalogTestFiles, catalogTestAnswer("default", map[string][]string{"a": {"1"}}), now)
	catalog.storeTable("srv1", catalogTestFiles, "ORDERS", tableTestAnswer("ORDERS", "orderID", "orders.go:10", 3), now)
	text := fieldsText(catalog.snapshot(catalogTestFiles), "orders", now)
	for _, want := range []string{"table ORDERS:", "orderID", "$caller", "orders.go:10",
		"table ORDERS probed on its own: 3 lines of it sampled"} {
		if !strings.Contains(text, want) {
			t.Errorf(":fields orders = %q, want %q in it", text, want)
		}
	}
	if strings.Contains(text, "$hostname") {
		t.Errorf(":fields orders = %q, want only the variables of ORDERS lines", text)
	}
}
