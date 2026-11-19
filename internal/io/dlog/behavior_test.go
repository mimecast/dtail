package dlog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/io/dlog/loggers"
	"github.com/mimecast/dtail/internal/source"
)

type behaviorLogger struct {
	mu            sync.Mutex
	logs          []string
	raws          []string
	fileOnly      []string
	coloredLogs   int
	coloredRaws   int
	coloredLog    string
	coloredRaw    string
	flushes       int
	pauses        int
	resumes       int
	supportsColor bool
}

func (l *behaviorLogger) Log(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logs = append(l.logs, message)
}

func (l *behaviorLogger) LogWithColors(message, colored string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.coloredLogs++
	l.coloredLog = colored
	l.logs = append(l.logs, message)
}

func (l *behaviorLogger) Raw(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.raws = append(l.raws, message)
}

func (l *behaviorLogger) RawWithColors(message, colored string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.coloredRaws++
	l.coloredRaw = colored
	l.raws = append(l.raws, message)
}

func (l *behaviorLogger) RawFileOnly(message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fileOnly = append(l.fileOnly, message)
}

func (l *behaviorLogger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.flushes++
}

func (l *behaviorLogger) SupportsColors() bool { return l.supportsColor }

func (l *behaviorLogger) Pause() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pauses++
}

func (l *behaviorLogger) Resume() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resumes++
}

var _ loggers.Logger = (*behaviorLogger)(nil)

func TestNewLevelRecognizesSupportedNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want level
	}{
		{name: "none", text: "none", want: None},
		{name: "fatal", text: "fatal", want: Fatal},
		{name: "error", text: "ERROR", want: Error},
		{name: "warn", text: "warn", want: Warn},
		{name: "info", text: "info", want: Info},
		{name: "empty default", text: "", want: Default},
		{name: "named default", text: "default", want: Default},
		{name: "verbose", text: "verbose", want: Verbose},
		{name: "debug", text: "debug", want: Debug},
		{name: "devel", text: "devel", want: Devel},
		{name: "trace", text: "trace", want: Trace},
		{name: "all", text: "all", want: All},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newLevel(tt.text)
			if err != nil {
				t.Fatalf("newLevel(%q) error = %v", tt.text, err)
			}
			if got != tt.want {
				t.Fatalf("newLevel(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestLevelString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level level
		want  string
	}{
		{None, "NONE"}, {Fatal, "FATAL"}, {Error, "ERROR"}, {Warn, "WARN"},
		{Info, "INFO"}, {Default, "DEFAULT"}, {Verbose, "VERBOSE"},
		{Debug, "DEBUG"}, {Devel, "DEVEL"}, {Trace, "TRACE"}, {All, "ALL"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := tt.level.String(); got != tt.want {
				t.Fatalf("level.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLevelStringPanicsForInvalidLevel(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("invalid level did not panic")
		}
	}()
	_ = level(99).String()
}

func TestDLogRoutesMessagesAndHonorsLevels(t *testing.T) {
	recorder := &behaviorLogger{}
	clientLog := &DLog{
		logger:        recorder,
		sourceProcess: source.Client,
		sourcePackage: source.Server,
		maxLevel:      All,
		hostname:      "host-a",
	}

	tests := []struct {
		name string
		log  func(...any) string
		want string
	}{
		{name: "fatal", log: clientLog.Fatal, want: "SERVER|host-a|FATAL|message|problem"},
		{name: "error", log: clientLog.Error, want: "SERVER|host-a|ERROR|message|problem"},
		{name: "warn", log: clientLog.Warn, want: "SERVER|host-a|WARN|message|problem"},
		{name: "info", log: clientLog.Info, want: "SERVER|host-a|INFO|message|problem"},
		{name: "verbose", log: clientLog.Verbose, want: "SERVER|host-a|VERBOSE|message|problem"},
		{name: "debug", log: clientLog.Debug, want: "SERVER|host-a|DEBUG|message|problem"},
	}
	wantLogs := make([]string, 0, len(tests)+2)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.log("message", errors.New("problem"))
			if got != tt.want {
				t.Fatalf("log result = %q, want %q", got, tt.want)
			}
			wantLogs = append(wantLogs, got)
		})
	}

	traceMessage := clientLog.Trace("trace message")
	if !strings.Contains(traceMessage, "SERVER|host-a|TRACE|trace message|at ") {
		t.Fatalf("Trace() = %q, want caller location", traceMessage)
	}
	wantLogs = append(wantLogs, traceMessage)
	develMessage := clientLog.Devel("development message")
	if !strings.Contains(develMessage, "SERVER|host-a|DEVEL|development message|at ") {
		t.Fatalf("Devel() = %q, want caller location", develMessage)
	}
	wantLogs = append(wantLogs, develMessage)

	if len(recorder.logs) != len(wantLogs) {
		t.Fatalf("sink received %d logs, want %d: %q", len(recorder.logs), len(wantLogs), recorder.logs)
	}
	for i, want := range wantLogs {
		if recorder.logs[i] != want {
			t.Fatalf("sink log %d = %q, want %q", i, recorder.logs[i], want)
		}
	}

	clientLog.maxLevel = Info
	recordedBeforeDisabled := len(recorder.logs)
	if got := clientLog.Debug("hidden"); got != "" {
		t.Fatalf("disabled Debug() = %q, want empty", got)
	}
	if got := clientLog.Trace("hidden"); got != "" {
		t.Fatalf("disabled Trace() = %q, want empty", got)
	}
	if got := clientLog.Devel("hidden"); got != "" {
		t.Fatalf("disabled Devel() = %q, want empty", got)
	}
	if len(recorder.logs) != recordedBeforeDisabled {
		t.Fatalf("disabled levels wrote to sink: log count %d, want %d", len(recorder.logs), recordedBeforeDisabled)
	}
}

func TestDLogRoutesColoredRawAndDiagnosticMessages(t *testing.T) {
	recorder := &behaviorLogger{supportsColor: true}
	theme := config.DefaultTermColors()
	theme.Client.TextFg = color.FgMagenta
	theme.Client.TextBg = color.BgWhite
	theme.Client.TextAttr = color.AttrUnderline
	d := &DLog{
		logger:        recorder,
		colorizer:     brush.New(theme),
		colorsEnabled: true,
		sourceProcess: source.Server,
		maxLevel:      Info,
	}

	if got := d.Info("diagnostic"); !strings.HasSuffix(got, "|diagnostic") {
		t.Fatalf("Info() = %q, want diagnostic suffix", got)
	}
	if got := d.Raw("CLIENT|host|payload"); got != "CLIENT|host|payload" {
		t.Fatalf("Raw() = %q, want framed payload", got)
	}
	if got := d.RawLog("audit"); got != "audit" {
		t.Fatalf("RawLog() = %q, want audit", got)
	}
	if recorder.coloredLogs != 2 || recorder.coloredRaws != 1 {
		t.Fatalf("colored calls = logs:%d raws:%d, want 2 and 1", recorder.coloredLogs, recorder.coloredRaws)
	}
	wantSequence := string(color.FgMagenta) + string(color.BgWhite) +
		string(color.AttrUnderline) + "payload"
	if !strings.Contains(recorder.coloredRaw, wantSequence) {
		t.Fatalf("colored Raw() = %q, want injected sequence %q", recorder.coloredRaw, wantSequence)
	}
}

func TestDLogOptionalSinkCapabilities(t *testing.T) {
	recorder := &behaviorLogger{}
	d := &DLog{logger: recorder}

	d.RawPayloadFileTee("payload")
	d.Flush()
	d.Pause()
	d.Resume()

	if len(recorder.fileOnly) != 1 || recorder.fileOnly[0] != "payload" {
		t.Fatalf("file-only payloads = %v, want [payload]", recorder.fileOnly)
	}
	if recorder.flushes != 1 || recorder.pauses != 1 || recorder.resumes != 1 {
		t.Fatalf("lifecycle calls = flush:%d pause:%d resume:%d, want one each",
			recorder.flushes, recorder.pauses, recorder.resumes)
	}
}

func TestDLogMapreduceFormatsClientAndServerRecords(t *testing.T) {
	tests := []struct {
		name          string
		process       source.Source
		wantSubstring string
	}{
		{name: "client stats", process: source.Client, wantSubstring: "INFO|STATS:REQUESTS|count=2"},
		{name: "server stats", process: source.Server, wantSubstring: "|MAPREDUCE:REQUESTS|count=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &behaviorLogger{}
			d := &DLog{
				logger:        recorder,
				sourceProcess: tt.process,
				sourcePackage: source.Client,
				maxLevel:      Info,
				hostname:      "host-a",
			}
			if got := d.Mapreduce("requests", map[string]any{"count": 2}); !strings.Contains(got, tt.wantSubstring) {
				t.Fatalf("Mapreduce() = %q, want substring %q", got, tt.wantSubstring)
			}
		})
	}
}

func TestDLogFatalPanicFlushesMessage(t *testing.T) {
	recorder := &behaviorLogger{}
	d := &DLog{logger: recorder, sourceProcess: source.Server, maxLevel: Fatal}
	defer func() {
		got := recover()
		if got != "fatal|problem" {
			t.Fatalf("panic = %#v, want %q", got, "fatal|problem")
		}
		if recorder.flushes != 1 || len(recorder.logs) != 1 {
			t.Fatalf("fatal path = flushes:%d logs:%v, want one of each", recorder.flushes, recorder.logs)
		}
	}()
	d.FatalPanic("fatal", errors.New("problem"))
}

func TestNewDLogValidatesConfiguration(t *testing.T) {
	if _, err := newDLog(source.Client, source.Client, config.RuntimeConfig{}, nil); err == nil || !strings.Contains(err.Error(), "configuration is unavailable") {
		t.Fatalf("newDLog() missing-config error = %v", err)
	}

	tests := []struct {
		name       string
		loggerName string
		levelName  string
		wantErr    string
	}{
		{name: "valid", loggerName: "none", levelName: "debug"},
		{name: "bad level", loggerName: "none", levelName: "chatty", wantErr: "unknown log level"},
		{name: "bad logger", loggerName: "network", levelName: "info", wantErr: "unsupported logger type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.RuntimeConfig{
				Common: &config.CommonConfig{
					HostnameOverride: "host-a",
					Logger:           tt.loggerName,
					LogLevel:         tt.levelName,
					LogRotation:      "signal",
				},
				Client: &config.ClientConfig{},
			}
			got, err := newDLog(source.Client, source.Server, cfg, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("newDLog() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("newDLog() error = %v", err)
			}
			if got.hostname != "host-a" || got.maxLevel != Debug || got.sourcePackage != source.Server {
				t.Fatalf("newDLog() = %#v, want configured logger", got)
			}
		})
	}
}

func TestStartInitializesAndStopsLoggers(t *testing.T) {
	previousClientLog, previousServerLog, previousCommonLog := Client, Server, Common
	mutex.Lock()
	previousStarted := started
	started = false
	mutex.Unlock()
	t.Cleanup(func() {
		mutex.Lock()
		started = previousStarted
		mutex.Unlock()
		Client, Server, Common = previousClientLog, previousServerLog, previousCommonLog
	})

	cfg := config.RuntimeConfig{
		Common: &config.CommonConfig{
			HostnameOverride: "host-a",
			Logger:           "none",
			LogLevel:         "info",
			LogRotation:      "signal",
		},
		Client: &config.ClientConfig{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	if err := Start(ctx, &wg, source.Client, cfg, nil); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if Client == nil || Server == nil || Common != Client {
		t.Fatalf("logger globals not initialized correctly: Client=%p Server=%p Common=%p", Client, Server, Common)
	}
	cancel()
	waitForWaitGroup(t, &wg)
}

func waitForWaitGroup(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for logger shutdown")
	}
}
