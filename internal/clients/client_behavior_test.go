package clients

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/clients/connectors"
	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	maprclient "github.com/mimecast/dtail/internal/mapr/client"
	"github.com/mimecast/dtail/internal/omode"
)

type serverlessStatsFactory struct {
	handler sessionHandlers.Handler
	created chan struct{}
}

type serverlessStatsHandler struct {
	done chan struct{}
	once sync.Once
}

func newServerlessStatsHandler() *serverlessStatsHandler {
	return &serverlessStatsHandler{done: make(chan struct{})}
}

func (f *serverlessStatsFactory) NewServerlessHandler(context.Context, string) (sessionHandlers.Handler, error) {
	close(f.created)
	return f.handler, nil
}

func (h *serverlessStatsHandler) Read([]byte) (int, error) {
	<-h.done
	return 0, io.EOF
}

func (*serverlessStatsHandler) Write(p []byte) (int, error) { return len(p), nil }
func (h *serverlessStatsHandler) Shutdown()                 { h.once.Do(func() { close(h.done) }) }
func (h *serverlessStatsHandler) Done() <-chan struct{}     { return h.done }

func TestClientConstructorsBuildServerlessWorkloads(t *testing.T) {
	runtimeCfg := clientTestRuntimeConfig()

	tests := []struct {
		name            string
		args            config.Args
		build           func(config.Args) (*baseClient, error)
		wantMode        omode.Mode
		wantRetry       bool
		wantUser        string
		wantHandlerType reflect.Type
	}{
		{
			name: "cat",
			args: config.Args{What: "app.log"},
			build: func(args config.Args) (*baseClient, error) {
				client, err := NewCatClient(args, runtimeCfg, LoggerDependencies{})
				if client == nil {
					return nil, err
				}
				return &client.baseClient, err
			},
			wantMode:        omode.CatClient,
			wantHandlerType: reflect.TypeOf((*handlers.ClientHandler)(nil)),
		},
		{
			name: "grep",
			args: config.Args{What: "app.log", RegexStr: "ERROR"},
			build: func(args config.Args) (*baseClient, error) {
				client, err := NewGrepClient(args, runtimeCfg, LoggerDependencies{})
				if client == nil {
					return nil, err
				}
				return &client.baseClient, err
			},
			wantMode:        omode.GrepClient,
			wantHandlerType: reflect.TypeOf((*handlers.ClientHandler)(nil)),
		},
		{
			name: "tail",
			args: config.Args{What: "app.log"},
			build: func(args config.Args) (*baseClient, error) {
				client, err := NewTailClient(args, runtimeCfg, LoggerDependencies{})
				if client == nil {
					return nil, err
				}
				return &client.baseClient, err
			},
			wantMode:        omode.TailClient,
			wantRetry:       true,
			wantHandlerType: reflect.TypeOf((*handlers.ClientHandler)(nil)),
		},
		{
			name: "health",
			build: func(args config.Args) (*baseClient, error) {
				client, err := NewHealthClient(args, runtimeCfg, LoggerDependencies{})
				if client == nil {
					return nil, err
				}
				return &client.baseClient, err
			},
			wantMode:        omode.HealthClient,
			wantUser:        config.HealthUser,
			wantHandlerType: reflect.TypeOf((*handlers.HealthHandler)(nil)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := tt.args
			args.Serverless = true
			args.ConnectionsPerCPU = 1
			client, err := tt.build(args)
			if err != nil {
				t.Fatalf("constructor error = %v", err)
			}
			if client.Mode != tt.wantMode || client.profile.retry != tt.wantRetry || client.UserName != tt.wantUser {
				t.Fatalf("client state = mode:%v retry:%v user:%q, want mode:%v retry:%v user:%q",
					client.Mode, client.profile.retry, client.UserName, tt.wantMode, tt.wantRetry, tt.wantUser)
			}
			if len(client.connections) != 1 {
				t.Fatalf("connections = %d, want one serverless connection", len(client.connections))
			}
			if got := reflect.TypeOf(client.connections[0].Handler()); got != tt.wantHandlerType {
				t.Fatalf("handler type = %v, want %v", got, tt.wantHandlerType)
			}
			if client.stats == nil || client.runtime == nil || client.profile.newHandler == nil {
				t.Fatalf("constructor left runtime dependencies uninitialized: %#v", client)
			}
			if client.profile.commit != nil {
				t.Fatal("non-map client profile has a session commit callback")
			}
			if client.sessionSpec.Mode != tt.wantMode {
				t.Fatalf("session mode = %v, want %v", client.sessionSpec.Mode, tt.wantMode)
			}
		})
	}
}

func TestClientConstructorsRejectInvalidInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func() error
		wantErr string
	}{
		{
			name: "cat rejects regex",
			build: func() error {
				_, err := NewCatClient(config.Args{RegexStr: "ERROR"}, config.RuntimeConfig{}, LoggerDependencies{})
				return err
			},
			wantErr: "can't use regex",
		},
		{
			name: "grep requires regex",
			build: func() error {
				_, err := NewGrepClient(config.Args{}, config.RuntimeConfig{}, LoggerDependencies{})
				return err
			},
			wantErr: "no regex specified",
		},
		{
			name: "map requires query",
			build: func() error {
				_, err := NewMaprClient(config.Args{}, config.RuntimeConfig{}, DefaultMode, LoggerDependencies{})
				return err
			},
			wantErr: "no mapreduce query specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := tt.build(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("constructor error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNewMaprClientDerivesModeAndRegex(t *testing.T) {
	runtimeCfg := clientTestRuntimeConfig()

	tests := []struct {
		name      string
		argsMode  omode.Mode
		query     string
		mode      MaprClientMode
		wantRetry bool
		wantRegex string
	}{
		{
			name:      "tail without outfile retries",
			argsMode:  omode.TailClient,
			query:     "from STATS select count(*)",
			mode:      NonCumulativeMode,
			wantRetry: true,
			wantRegex: `\|MAPREDUCE:STATS\|`,
		},
		{
			name:      "tail outfile is finite",
			argsMode:  omode.TailClient,
			query:     "from STATS select count(*) outfile result.txt",
			mode:      DefaultMode,
			wantRegex: `\|MAPREDUCE:STATS\|`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := NewMaprClient(config.Args{
				Mode:              tt.argsMode,
				QueryStr:          tt.query,
				Serverless:        true,
				ConnectionsPerCPU: 1,
			}, runtimeCfg, tt.mode, LoggerDependencies{})
			if err != nil {
				t.Fatalf("NewMaprClient() error = %v", err)
			}
			if client.profile.retry != tt.wantRetry || client.RegexStr != tt.wantRegex || client.mode != tt.mode {
				t.Fatalf("map client = retry:%v regex:%q mode:%v, want retry:%v regex:%q mode:%v",
					client.profile.retry, client.RegexStr, client.mode, tt.wantRetry, tt.wantRegex, tt.mode)
			}
			if client.sessionSpec.Query != tt.query {
				t.Fatalf("session query = %q, want %q", client.sessionSpec.Query, tt.query)
			}
			if client.profile.commit == nil {
				t.Fatal("map client profile has no session commit callback")
			}
			if _, ok := client.connections[0].Handler().(*handlers.MaprHandler); !ok {
				t.Fatalf("map handler type = %T, want *handlers.MaprHandler", client.connections[0].Handler())
			}
		})
	}
}

func TestMaprClientCumulativePolicy(t *testing.T) {
	t.Parallel()

	plainQuery := mustMaprClientQuery(t, "from STATS select count(*)")
	outfileQuery := mustMaprClientQuery(t, "from STATS select count(*) outfile result.txt")
	tests := []struct {
		name string
		mode MaprClientMode
		op   omode.Mode
		q    *mapr.Query
		want bool
	}{
		{name: "explicit cumulative", mode: CumulativeMode, q: plainQuery, want: true},
		{name: "scheduled is cumulative", mode: ScheduledMode, op: omode.TailClient, q: plainQuery, want: true},
		{name: "explicit non-cumulative", mode: NonCumulativeMode, op: omode.MapClient, q: outfileQuery},
		{name: "map defaults cumulative", mode: DefaultMode, op: omode.MapClient, q: plainQuery, want: true},
		{name: "outfile defaults cumulative", mode: DefaultMode, op: omode.TailClient, q: outfileQuery, want: true},
		{name: "tail stream defaults non-cumulative", mode: DefaultMode, op: omode.TailClient, q: plainQuery},
		{name: "nil query is safe", mode: DefaultMode, op: omode.TailClient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := &MaprClient{baseClient: baseClient{Args: config.Args{Mode: tt.op}}, mode: tt.mode}
			if got := client.isCumulative(tt.q); got != tt.want {
				t.Fatalf("isCumulative() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTailStatsDataAndPercentages(t *testing.T) {
	t.Parallel()

	stats := newTailStats(4, nil, 0, nil)
	if stats.interruptPause != 3*time.Second {
		t.Fatalf("default interrupt pause = %v, want 3s", stats.interruptPause)
	}
	data := stats.statsData(3, 2, 1)
	want := map[string]int{
		"connected":  3,
		"servers":    4,
		"connected%": 75,
		"new":        2,
		"throttle":   1,
	}
	for key, value := range want {
		if got := data[key]; got != value {
			t.Fatalf("statsData()[%q] = %#v, want %d", key, got, value)
		}
	}
	for _, key := range []string{"goroutines", "cgocalls", "cpu"} {
		if _, ok := data[key]; !ok {
			t.Fatalf("statsData() missing runtime key %q", key)
		}
	}

	line := stats.statsLine(3, 2, 1)
	for _, field := range []string{"connected=3", "servers=4", "connected%=75", "new=2", "throttle=1"} {
		if !strings.Contains(line, field) {
			t.Fatalf("statsLine() = %q, want field %q", line, field)
		}
	}

	percentTests := []struct {
		name         string
		total, value float64
		want         float64
	}{
		{name: "zero total", total: 0, value: 0, want: 100},
		{name: "all", total: 4, value: 4, want: 100},
		{name: "part", total: 4, value: 1, want: 25},
	}
	for _, tt := range percentTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := percentOf(tt.total, tt.value); got != tt.want {
				t.Fatalf("percentOf(%v, %v) = %v, want %v", tt.total, tt.value, got, tt.want)
			}
		})
	}
}

func TestTailStatsReportServerlessConnectionLifecycle(t *testing.T) {
	connectionStats := newTailStats(1, nil, time.Nanosecond, nil)
	throttleCh := make(chan struct{}, 1)
	serverHandler := newServerlessStatsHandler()
	factory := &serverlessStatsFactory{
		handler: serverHandler,
		created: make(chan struct{}),
	}
	connector := connectors.NewServerless(
		"test-user",
		handlers.NewClientHandler("local(serverless)", clientlog.NopLogger{}),
		nil,
		SessionSpec{},
		false,
		factory,
		logging.NopLogger{},
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		connector.Start(ctx, cancel, throttleCh, connectionStats.connectionsEstCh)
	}()

	select {
	case <-factory.created:
	case <-time.After(time.Second):
		t.Fatal("serverless handler was not created")
	}
	data := connectionStats.statsData(len(connectionStats.connectionsEstCh), 1, len(throttleCh))
	if got := data["connected"]; got != 1 {
		t.Fatalf("serverless connected stats = %#v, want 1", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serverless connection did not finish teardown")
	}
	data = connectionStats.statsData(len(connectionStats.connectionsEstCh), -1, len(throttleCh))
	if got := data["connected"]; got != 0 {
		t.Fatalf("serverless connected stats after teardown = %#v, want 0", got)
	}
}

func TestInteractiveControlWritersDescribeCurrentSession(t *testing.T) {
	t.Parallel()

	client := &baseClient{
		mu: newBaseClientMu(),
		Args: config.Args{
			Mode: omode.TailClient,
		},
		sessionSpec: SessionSpec{
			Files:   []string{"app.log", "audit.log"},
			Query:   "from STATS select count(*)",
			Regex:   "ERROR",
			Options: "plain=true",
			Timeout: 7,
		},
		connections: []connectors.Connector{
			&interactiveReloadConnector{supported: true},
			&interactiveReloadConnector{supported: false},
		},
	}

	var output bytes.Buffer
	if err := client.writeInteractiveHelp(&output); err != nil {
		t.Fatalf("writeInteractiveHelp() error = %v", err)
	}
	if !strings.Contains(output.String(), ":reload <flags>") {
		t.Fatalf("help output = %q, want reload syntax", output.String())
	}
	output.Reset()
	if err := client.writeInteractiveState(&output); err != nil {
		t.Fatalf("writeInteractiveState() error = %v", err)
	}
	for _, want := range []string{
		"mode=tail", "files=app.log,audit.log", `query="from STATS select count(*)"`,
		`regex="ERROR"`, `options="plain=true"`, "timeout=7", "capable=1/2",
	} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("state output = %q, want substring %q", output.String(), want)
		}
	}
}

func TestMaprTerminalRendererWritesEveryCellType(t *testing.T) {
	t.Parallel()

	cfg := &config.ClientConfig{TermColorsEnable: true}
	renderer := newClientOutputFormatter(cfg).MaprResultRenderer()
	var output strings.Builder
	renderer.WriteHeaderEntry(&output, "header", true, true)
	renderer.WriteHeaderDelimiter(&output, "|")
	renderer.WriteDataEntry(&output, "value")
	renderer.WriteDataDelimiter(&output, ";")
	for _, want := range []string{"header", "|", "value", ";"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("rendered output = %q, want substring %q", output.String(), want)
		}
	}
}

func TestBaseClientStartWithNoConnections(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &baseClient{
		mu:         newBaseClientMu(),
		stats:      newTailStats(0, nil, time.Nanosecond, nil),
		throttleCh: make(chan struct{}),
	}
	if status := client.Start(ctx, nil); status != 0 {
		t.Fatalf("Start() status = %d, want 0", status)
	}
	if got := client.snapshotConnections(); len(got) != 0 {
		t.Fatalf("snapshotConnections() = %v, want empty", got)
	}
}

func TestMaprReporterStopsOnCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &MaprClient{
		baseClient: baseClient{loggers: LoggerDependencies{}.normalized()},
		session:    maprclient.NewSessionState(nil, logging.NopLogger{}),
	}
	client.periodicReportResults(ctx)
	if err := client.reportResults(false); err != nil {
		t.Fatalf("reportResults() with no query error = %v", err)
	}
	if got := client.reportDelay(nil, true); got != 500*time.Millisecond {
		t.Fatalf("nil-query ramp-up delay = %v, want 500ms", got)
	}
}

func clientTestRuntimeConfig() config.RuntimeConfig {
	return config.RuntimeConfig{
		Server: config.NewDefaultServerConfigForTest(),
		Client: &config.ClientConfig{},
		Common: &config.CommonConfig{HostnameOverride: "test-host"},
	}
}
