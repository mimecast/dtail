package clients

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/protocol"
)

const (
	reloadTestQueryA = "select status,count(status) from stats group by status"
	reloadTestQueryB = "select status,count(status) from warnings group by status"
)

// reloadAnswer is how a reloadTestServer answers one SESSION UPDATE: with
// a rejection, or with its acknowledgement directly followed, in the same
// write, by the aggregate messages of the workload it switched to, as a
// dserver whose file is read at once sends them.
type reloadAnswer struct {
	reject     bool
	aggregates []string
}

// reloadTestServer plays the server of one real MaprHandler: it reads the
// client's command frames and acknowledges SESSION START with generation
// 1 and each SESSION UPDATE as its next answer says.
type reloadTestServer struct {
	handler *handlers.MaprHandler
	answers []reloadAnswer
	// after, if set, is waited for before the first update is answered.
	after <-chan struct{}
	// endsAfterFirst ends the session right after the first update's
	// answer, as a dmap server does whose file was read to its end.
	endsAfterFirst bool
	// firstAnswered is closed once the write of the first update's answer
	// returned: the handler then handled the acknowledgement and whatever
	// followed it.
	firstAnswered chan struct{}
	// answered gets a value once the write of an update's answer returned.
	answered chan struct{}
	stopped  chan struct{}
}

func startReloadTestServer(t *testing.T, handler *handlers.MaprHandler, after <-chan struct{},
	endsAfterFirst bool, answers ...reloadAnswer) *reloadTestServer {
	t.Helper()
	server := &reloadTestServer{handler: handler, answers: answers, after: after, endsAfterFirst: endsAfterFirst,
		firstAnswered: make(chan struct{}), answered: make(chan struct{}, 8), stopped: make(chan struct{})}
	go server.serve(t)
	t.Cleanup(func() {
		handler.Shutdown()
		<-server.stopped
	})
	return server
}

func (s *reloadTestServer) serve(t *testing.T) {
	defer close(s.stopped)
	p := make([]byte, 64*1024)
	updates := 0
	for {
		n, err := s.handler.Read(p)
		if err != nil {
			return
		}
		command := decodeReloadTestCommand(string(p[:n]))
		var stream bytes.Buffer
		switch {
		case strings.HasPrefix(command, "SESSION START "):
			protocol.EscapedControlFraming.EncodeControl(&stream, protocol.HiddenCapabilitiesPrefix)
			protocol.EscapedControlFraming.EncodeControl(&stream, protocol.HiddenSessionStartOKPrefix+" 1")
			_, _ = s.handler.Write(stream.Bytes())
		case strings.HasPrefix(command, "SESSION UPDATE ") && updates < len(s.answers):
			if updates == 0 && s.after != nil {
				<-s.after
			}
			s.answers[updates].encode(&stream, strings.Fields(command)[2])
			updates++
			_, _ = s.handler.Write(stream.Bytes())
			s.updateAnswered(updates)
		default:
			t.Errorf("unexpected command %q (frame %q)", command, p[:n])
			return
		}
	}
}

// updateAnswered is called once the write of the answer to update number
// update returned.
func (s *reloadTestServer) updateAnswered(update int) {
	if update == 1 {
		if s.endsAfterFirst {
			s.handler.Shutdown()
		}
		close(s.firstAnswered)
	}
	s.answered <- struct{}{}
}

// endReloadTestSessions ends the sessions of servers as a client's
// connections end before its final report: once the handler of servers[i]
// handled its server's answers to updates[i] updates (the frames behind an
// acknowledgement are handled after the acknowledgement was handed to the
// reload, which may return first), the handler is shut down, which
// flushes its aggregates.
func endReloadTestSessions(t *testing.T, servers []*reloadTestServer, updates ...int) {
	t.Helper()
	for i, server := range servers {
		for range updates[i] {
			select {
			case <-server.answered:
			case <-time.After(5 * time.Second):
				t.Fatalf("server %d did not answer %d updates", i+1, updates[i])
			}
		}
		if !server.endsAfterFirst {
			// A session that ended during the reload is not flushed again:
			// the reload's end must have merged what it held.
			server.handler.Shutdown()
		}
	}
}

func (a reloadAnswer) encode(stream *bytes.Buffer, generation string) {
	if a.reject {
		protocol.EscapedControlFraming.EncodeControl(stream, protocol.HiddenSessionErrorPrefix+"rejected")
		return
	}
	protocol.EscapedControlFraming.EncodeControl(stream, protocol.HiddenSessionUpdateOKPrefix+" "+generation)
	for _, aggregate := range a.aggregates {
		protocol.EscapedControlFraming.EncodePayload(stream, []byte(aggregate+"\n"))
	}
}

// decodeReloadTestCommand returns the command of a client command frame
// ("protocol <version> base64 <command>;").
func decodeReloadTestCommand(frame string) string {
	encoded, _ := strings.CutPrefix(frame, "protocol "+protocol.ProtocolCompat+" base64 ")
	command, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(encoded, ";"))
	if err != nil {
		return ""
	}
	return string(command)
}

func mustReloadTestQuery(t *testing.T, raw string) *mapr.Query {
	t.Helper()
	query, err := mapr.NewQuery(raw, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery(%q): %v", raw, err)
	}
	return query
}

func reloadTestCountStorage(t *testing.T, query *mapr.Query) string {
	t.Helper()
	for _, selectCondition := range query.Select {
		if selectCondition.Operation == mapr.Count {
			return selectCondition.FieldStorage
		}
	}
	t.Fatalf("query %q has no count()", query.RawQuery)
	return ""
}

// reloadTestAggregate returns the aggregate message of one group of query
// with the given count.
func reloadTestAggregate(t *testing.T, query *mapr.Query, key string, count int) string {
	t.Helper()
	serialized := strings.Join([]string{key, "1",
		reloadTestCountStorage(t, query) + protocol.AggregateKVDelimiter + strconv.Itoa(count), ""},
		protocol.AggregateDelimiter)
	return protocol.AggregateMessageID + protocol.FieldSeparator() + "host" + protocol.FieldSeparator() + serialized
}

// reloadTestCount returns the count of the group key in snapshot's result.
func reloadTestCount(t *testing.T, snapshot maprclient.SessionSnapshot, key string) float64 {
	t.Helper()
	if snapshot.Query == nil || snapshot.GlobalGroup == nil {
		return 0
	}
	storage := reloadTestCountStorage(t, snapshot.Query)
	var count float64
	snapshot.GlobalGroup.Update(func(group *mapr.GroupSet) {
		count = group.GetSet(key).FValues[storage]
	})
	return count
}

// requireReloadTestCount fails unless the session's current result counts
// want for key. It is read once, after the reload returned and the
// sessions ended (endReloadTestSessions), as a client's final report reads
// it: the reload's end merged what was held during it.
func requireReloadTestCount(t *testing.T, c *MaprClient, key string, want float64) {
	t.Helper()
	if got := reloadTestCount(t, c.session.Snapshot(), key); got != want {
		t.Fatalf("count of %q = %v, want %v", key, got, want)
	}
}

// newReloadAggregateClient returns an interactive MapReduce client (dmap,
// or with switchable interactive dtail) with args and two real server
// connections with real MaprHandlers, whose sessions the test servers
// (answering updates with answers[i]) committed at generation 1. srv2
// answers its first update only once srv1's answer was handled, so that
// what srv1 sends right after its acknowledgement always arrives before
// the client commits the reload. With srv1Ends the session of srv1 ends
// right after that answer.
func newReloadAggregateClient(t *testing.T, args config.Args, switchable, srv1Ends bool,
	answers [2][]reloadAnswer) (*MaprClient, []*reloadTestServer) {
	t.Helper()
	args.InteractiveQuery = true
	spec := NewSessionSpec(args)
	var query *mapr.Query
	if args.QueryStr != "" {
		query = mustReloadTestQuery(t, args.QueryStr)
	}
	c := &MaprClient{
		baseClient: baseClient{mu: newBaseClientMu(), Args: args, sessionSpec: spec},
		session:    maprclient.NewSessionState(query, logging.NopLogger{}),
	}
	c.loggers = LoggerDependencies{}.normalized()
	c.runtime = newClientRuntimeBoundary(clientTestRuntimeConfig(), c.loggers)
	c.profile = maprClientProfile(c, c.loggers, true, switchable)
	c.prober = c.newInteractiveProber()

	var after <-chan struct{}
	var servers []*reloadTestServer
	for i, name := range []string{"srv1", "srv2"} {
		handler := handlers.NewMaprHandler(name, c.session, c.loggers.Client)
		server := startReloadTestServer(t, handler, after, srv1Ends && i == 0, answers[i]...)
		after = server.firstAnswered
		servers = append(servers, server)
		conn, err := connectors.NewServerConnection(name, "user", nil, interactiveReloadHostKeyCallback{},
			handler, nil, spec, true, "", false, nil, logging.NopLogger{})
		if err != nil {
			t.Fatalf("NewServerConnection: %v", err)
		}
		if err := conn.ApplySessionSpec(spec, 0); err != nil {
			t.Fatalf("start the session of %s: %v", name, err)
		}
		c.connections = append(c.connections, conn)
	}
	return c, servers
}

// TestReloadedQueryKeepsAggregatesFromBeforeCommit: a :query of dmap
// replaces the running query on two servers. Each server acknowledges the
// update and sends the new query's result right away (a small file is read
// at once), srv1's so before srv2 was even updated and the client
// committed the new query. Those aggregates were merged into the replaced
// query's group set and so were missing in the new query's result (and
// showed in the replaced one's).
func TestReloadedQueryKeepsAggregatesFromBeforeCommit(t *testing.T) {
	queryB := mustReloadTestQuery(t, reloadTestQueryB)
	answer := []reloadAnswer{{aggregates: []string{reloadTestAggregate(t, queryB, "ERROR", 1)}}}
	c, servers := newReloadAggregateClient(t, config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: reloadTestQueryA}, false, false, [2][]reloadAnswer{answer, answer})
	replaced := c.session.Snapshot()

	if _, err := c.applyQueryCommand(reloadTestQueryB); err != nil {
		t.Fatalf("applyQueryCommand() error = %v", err)
	}
	if snapshot := c.session.Snapshot(); snapshot.Query == nil || snapshot.Query.RawQuery != reloadTestQueryB {
		t.Fatalf("query after the reload = %#v", snapshot.Query)
	}
	endReloadTestSessions(t, servers, 1, 1)
	requireReloadTestCount(t, c, "ERROR", 2)
	if got := reloadTestCount(t, replaced, "ERROR"); got != 0 {
		t.Fatalf("the replaced query's result counts %v of the new query's aggregates, want 0", got)
	}
}

// TestSwitchToQueryKeepsAggregatesFromBeforeCommit is the same for
// interactive dtail switching from following lines to a query: there is no
// query yet when srv1's first aggregates arrive, and they were dropped.
func TestSwitchToQueryKeepsAggregatesFromBeforeCommit(t *testing.T) {
	query := mustReloadTestQuery(t, switchTestQuery)
	answer := []reloadAnswer{{aggregates: []string{reloadTestAggregate(t, query, "host1", 3)}}}
	c, servers := newReloadAggregateClient(t, config.Args{Mode: omode.TailClient, What: "/var/log/app.log",
		RegexStr: "ERROR"}, true, false, [2][]reloadAnswer{answer, answer})

	if _, err := c.switchToQuery(switchTestQuery); err != nil {
		t.Fatalf("switchToQuery() error = %v", err)
	}
	endReloadTestSessions(t, servers, 1, 1)
	requireReloadTestCount(t, c, "host1", 6)
}

// TestRolledBackReloadDropsItsAggregates: srv1 switched to the new query
// and sent its first aggregates, then srv2 rejected the update and the
// reload was rolled back. The aggregates of the query that never became
// the client's must not show in the result of the query that stays, while
// what srv1 sends after its rollback (the previous query, run again) does.
func TestRolledBackReloadDropsItsAggregates(t *testing.T) {
	queryA := mustReloadTestQuery(t, reloadTestQueryA)
	queryB := mustReloadTestQuery(t, reloadTestQueryB)
	c, servers := newReloadAggregateClient(t, config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: reloadTestQueryA}, false, false, [2][]reloadAnswer{
		{
			{aggregates: []string{reloadTestAggregate(t, queryB, "NEW", 5)}},
			{aggregates: []string{reloadTestAggregate(t, queryA, "OLD", 7)}},
		},
		{{reject: true}},
	})

	if _, err := c.applyQueryCommand(reloadTestQueryB); err == nil {
		t.Fatal("applyQueryCommand() succeeded although srv2 rejected the update")
	}
	snapshot := c.session.Snapshot()
	if snapshot.Query == nil || snapshot.Query.RawQuery != reloadTestQueryA {
		t.Fatalf("query after the failed reload = %#v, want the previous one", snapshot.Query)
	}
	endReloadTestSessions(t, servers, 2, 1)
	requireReloadTestCount(t, c, "OLD", 7)
	if got := reloadTestCount(t, c.session.Snapshot(), "NEW"); got != 0 {
		t.Fatalf("the rolled back query's aggregates count %v in the result, want 0", got)
	}
}

// TestReloadedQueryKeepsAggregatesOfSessionEndedMeanwhile: srv1 sends the
// new query's whole result behind its acknowledgement and ends its
// session (its file was read) while the reload still updates srv2. The
// handler's shutdown flush keeps what is held for the reload, and the
// reload's end merges it before it returns, so the final result, read
// right after, has srv1's rows.
func TestReloadedQueryKeepsAggregatesOfSessionEndedMeanwhile(t *testing.T) {
	queryB := mustReloadTestQuery(t, reloadTestQueryB)
	answer := []reloadAnswer{{aggregates: []string{reloadTestAggregate(t, queryB, "ERROR", 1)}}}
	c, servers := newReloadAggregateClient(t, config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: reloadTestQueryA}, false, true, [2][]reloadAnswer{answer, answer})

	if _, err := c.applyQueryCommand(reloadTestQueryB); err != nil {
		t.Fatalf("applyQueryCommand() error = %v", err)
	}
	endReloadTestSessions(t, servers, 1, 1)
	requireReloadTestCount(t, c, "ERROR", 2)
}

// TestRolledBackReloadDropsAggregatesOfEndedSession: srv1 switched to the
// new query, sent its result and ended its session; then srv2 rejected the
// update. The rollback can not reach srv1 any more, so what its connection
// holds is the abandoned query's: the end of the failed reload must drop
// it instead of merging it into the result of the query that stays.
func TestRolledBackReloadDropsAggregatesOfEndedSession(t *testing.T) {
	queryB := mustReloadTestQuery(t, reloadTestQueryB)
	c, servers := newReloadAggregateClient(t, config.Args{Mode: omode.MapClient, What: "/var/log/app.log",
		QueryStr: reloadTestQueryA}, false, true, [2][]reloadAnswer{
		{{aggregates: []string{reloadTestAggregate(t, queryB, "NEW", 5)}}},
		{{reject: true}},
	})

	if _, err := c.applyQueryCommand(reloadTestQueryB); err == nil {
		t.Fatal("applyQueryCommand() succeeded although srv2 rejected the update")
	}
	endReloadTestSessions(t, servers, 1, 1)
	// Also a later flush of the ended session must merge nothing.
	servers[0].handler.Shutdown()
	if snapshot := c.session.Snapshot(); snapshot.Query == nil || snapshot.Query.RawQuery != reloadTestQueryA {
		t.Fatalf("query after the failed reload = %#v, want the previous one", snapshot.Query)
	}
	requireReloadTestCount(t, c, "NEW", 0)
}
