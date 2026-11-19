package clients

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mimecast/dtail/internal/clients/clientlog"
	"github.com/mimecast/dtail/internal/color"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/logging"
)

type payloadTeeLogger struct {
	clientlog.NopLogger
	file bytes.Buffer
}

type roleLogger struct {
	logging.NopLogger
	name string
}

func (l *payloadTeeLogger) RawPayloadFileTee(message string) {
	_, _ = l.file.WriteString(message)
}

func TestNewClientRuntimeBoundaryDefaults(t *testing.T) {
	runtime := newClientRuntimeBoundary(config.RuntimeConfig{}, LoggerDependencies{})

	if runtime.SSHPort() != 2222 {
		t.Fatalf("Expected default SSH port 2222, got %d", runtime.SSHPort())
	}
	if runtime.SSHConnectTimeout() != 2*time.Second {
		t.Fatalf("Expected default timeout 2s, got %v", runtime.SSHConnectTimeout())
	}
	if runtime.InterruptPause() != 3*time.Second {
		t.Fatalf("Expected default interrupt pause 3s, got %v", runtime.InterruptPause())
	}
	if got := runtime.output.PaintMaprRawQuery("select 1"); got != "select 1" {
		t.Fatalf("Expected plain raw query output, got %q", got)
	}
}

func TestNewClientRuntimeBoundaryUsesConfiguredSSHSettings(t *testing.T) {
	runtime := newClientRuntimeBoundary(config.RuntimeConfig{
		Common: &config.CommonConfig{
			SSHPort:             4022,
			SSHConnectTimeoutMs: 4500,
		},
	}, LoggerDependencies{})

	if runtime.SSHPort() != 4022 {
		t.Fatalf("Expected configured SSH port 4022, got %d", runtime.SSHPort())
	}
	if runtime.SSHConnectTimeout() != 4500*time.Millisecond {
		t.Fatalf("Expected configured timeout 4.5s, got %v", runtime.SSHConnectTimeout())
	}
}

func TestServerlessOutputWriterHonorsLogPayload(t *testing.T) {
	for _, test := range []struct {
		name        string
		logPayload  bool
		wantFileTee string
	}{
		{name: "disabled", logPayload: false},
		{name: "enabled", logPayload: true, wantFileTee: "payload\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			logger := &payloadTeeLogger{}
			runtime := newClientRuntimeBoundary(config.RuntimeConfig{
				Client: &config.ClientConfig{LogPayload: test.logPayload},
			}, NewLoggerDependencies(logger, logging.NopLogger{}, logging.NopLogger{}))
			runtime.stdout = func() io.Writer { return &stdout }

			payload := []byte("payload\n")
			n, err := runtime.serverlessOutputWriter().Write(payload)
			if err != nil {
				t.Fatalf("serverless output write: %v", err)
			}
			if n != len(payload) {
				t.Fatalf("serverless output bytes = %d, want %d", n, len(payload))
			}
			if got := stdout.String(); got != string(payload) {
				t.Fatalf("stdout payload = %q, want %q", got, payload)
			}
			if got := logger.file.String(); got != test.wantFileTee {
				t.Fatalf("file tee payload = %q, want %q", got, test.wantFileTee)
			}
		})
	}
}

func TestNewServerlessHandlerPreservesLoggerRoles(t *testing.T) {
	clientLogger := &payloadTeeLogger{}
	serverLogger := &roleLogger{name: "server"}
	commonLogger := &roleLogger{name: "common"}
	runtime := newClientRuntimeBoundary(config.RuntimeConfig{
		Client: &config.ClientConfig{},
		Server: &config.ServerConfig{
			Permissions: config.Permissions{Default: []string{"^/.*"}},
		},
	}, NewLoggerDependencies(clientLogger, serverLogger, commonLogger))

	handler, err := runtime.NewServerlessHandler(context.Background(), "alice")
	if err != nil {
		t.Fatalf("NewServerlessHandler: %v", err)
	}
	withLoggers, ok := handler.(interface {
		Logger() logging.Logger
		ReaderLogger() logging.Logger
	})
	if !ok {
		t.Fatalf("serverless handler type %T does not expose logger roles", handler)
	}
	if got := withLoggers.Logger(); got != serverLogger {
		t.Fatalf("diagnostics logger = %T(%p), want server logger %p", got, got, serverLogger)
	}
	if got := withLoggers.ReaderLogger(); got != commonLogger {
		t.Fatalf("reader logger = %T(%p), want common logger %p", got, got, commonLogger)
	}
}

func TestClientOutputFormatterColorModes(t *testing.T) {
	plain := newClientOutputFormatter(nil)
	if got := plain.FormatInterruptMessage(1, "hello"); got != " hello" {
		t.Fatalf("Expected plain interrupt message, got %q", got)
	}
	if got := plain.PaintMaprRawQuery("select 1"); got != "select 1" {
		t.Fatalf("Expected plain raw query, got %q", got)
	}

	cfg := &config.ClientConfig{TermColorsEnable: true}
	cfg.TermColors.Client.ClientFg = color.FgBlack
	cfg.TermColors.Client.ClientBg = color.BgYellow
	cfg.TermColors.Client.ClientAttr = color.AttrBold
	cfg.TermColors.MaprTable.RawQueryFg = color.FgCyan
	cfg.TermColors.MaprTable.RawQueryBg = color.BgBlack
	cfg.TermColors.MaprTable.RawQueryAttr = color.AttrUnderline
	cfg.TermColors.MaprTable.HeaderFg = color.FgWhite
	cfg.TermColors.MaprTable.HeaderBg = color.BgBlue
	cfg.TermColors.MaprTable.HeaderAttr = color.AttrBold
	cfg.TermColors.MaprTable.HeaderDelimiterFg = color.FgWhite
	cfg.TermColors.MaprTable.HeaderDelimiterBg = color.BgBlue
	cfg.TermColors.MaprTable.HeaderDelimiterAttr = color.AttrDim
	cfg.TermColors.MaprTable.HeaderSortKeyAttr = color.AttrUnderline
	cfg.TermColors.MaprTable.HeaderGroupKeyAttr = color.AttrReverse
	cfg.TermColors.MaprTable.DataFg = color.FgWhite
	cfg.TermColors.MaprTable.DataBg = color.BgBlue
	cfg.TermColors.MaprTable.DataAttr = color.AttrNone
	cfg.TermColors.MaprTable.DelimiterFg = color.FgWhite
	cfg.TermColors.MaprTable.DelimiterBg = color.BgBlue
	cfg.TermColors.MaprTable.DelimiterAttr = color.AttrDim

	colored := newClientOutputFormatter(cfg)
	if got := colored.FormatInterruptMessage(0, "hello"); got != " hello" {
		t.Fatalf("Expected first interrupt line to stay plain, got %q", got)
	}
	if got := colored.FormatInterruptMessage(1, "hello"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("Expected colored interrupt output, got %q", got)
	}
	if got := colored.PaintMaprRawQuery("select 1"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("Expected colored raw query output, got %q", got)
	}
	if colored.MaprResultRenderer() == nil {
		t.Fatal("Expected non-nil mapreduce result renderer")
	}
}
