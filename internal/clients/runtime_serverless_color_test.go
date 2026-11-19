package clients

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/color/brush"
	"github.com/mimecast/dtail/internal/config"
	sessionHandlers "github.com/mimecast/dtail/internal/handlers"
)

// TestServerlessColorizerHonorsTermColors writes a payload line the way the
// serverless handler does (colored line format, the runtime's colorizer) and
// checks that the line has no terminal colors unless the client config
// enables them: --noColor and --plain clear Client.TermColorsEnable. The
// serverless payload used to stay colored with --noColor.
func TestServerlessColorizerHonorsTermColors(t *testing.T) {
	const uncolored = "REMOTE|testhost|100|1|in.log|ERROR payload\n"
	theme := brush.New(config.DefaultTermColors())
	for _, test := range []struct {
		name      string
		clientCfg *config.ClientConfig
		brush     *brush.Brush
		wantColor bool
	}{
		{name: "colors off", clientCfg: &config.ClientConfig{}, brush: theme},
		{name: "no client config", brush: theme},
		{name: "colors on", clientCfg: &config.ClientConfig{TermColorsEnable: true},
			brush: theme, wantColor: true},
		{name: "colors on without brush", clientCfg: &config.ClientConfig{TermColorsEnable: true},
			wantColor: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := newClientRuntimeBoundary(config.RuntimeConfig{Client: test.clientCfg},
				LoggerDependencies{}, test.brush)
			got := writeServerlessLine(t, runtime.serverlessColorizer(), "ERROR payload")
			if !test.wantColor {
				if got != uncolored {
					t.Fatalf("output = %q, want uncolored %q", got, uncolored)
				}
				return
			}
			if !strings.Contains(got, "\x1b[") || !strings.Contains(got, "ERROR payload") {
				t.Fatalf("output = %q, want a colored line", got)
			}
		})
	}
}

// TestServerlessDependenciesCarryColorDecision checks the colorizer that
// NewServerlessHandler builds the handler with (serverlessDependencies), not
// just serverlessColorizer: handing over the raw brush instead would color
// the payload with --noColor again.
func TestServerlessDependenciesCarryColorDecision(t *testing.T) {
	const uncolored = "REMOTE|testhost|100|1|in.log|ERROR payload\n"
	theme := brush.New(config.DefaultTermColors())
	for _, colorsOn := range []bool{false, true} {
		runtime := newClientRuntimeBoundary(config.RuntimeConfig{
			Client: &config.ClientConfig{TermColorsEnable: colorsOn},
			Server: &config.ServerConfig{},
		}, LoggerDependencies{}, theme)
		dependencies := runtime.serverlessDependencies()
		if dependencies.ServerlessOutput == nil {
			t.Fatal("serverless dependencies have no serverless output")
		}
		got := writeServerlessLine(t, dependencies.Colorizer, "ERROR payload")
		if colored := strings.Contains(got, "\x1b["); colored != colorsOn {
			t.Fatalf("TermColorsEnable=%v: output = %q, colored = %v", colorsOn, got, colored)
		}
		if !colorsOn && got != uncolored {
			t.Fatalf("output = %q, want %q", got, uncolored)
		}
	}
}

// TestServerlessColorizerWithoutBrushIsNilInterface guards against handing
// the handler a typed nil *brush.Brush instead of a nil interface.
func TestServerlessColorizerWithoutBrushIsNilInterface(t *testing.T) {
	runtime := newClientRuntimeBoundary(config.RuntimeConfig{
		Client: &config.ClientConfig{TermColorsEnable: true},
	}, LoggerDependencies{})
	if colorizer := runtime.serverlessColorizer(); colorizer != nil {
		t.Fatalf("colorizer = %#v, want nil interface", colorizer)
	}
}

func writeServerlessLine(t *testing.T, colorizer sessionHandlers.Colorizer, line string) string {
	t.Helper()
	var output bytes.Buffer
	writer := sessionHandlers.NewDirectWriterWithColorizer(&output, "testhost", false, true, colorizer)
	if err := writer.WriteLineData([]byte(line), 1, "in.log"); err != nil {
		t.Fatalf("WriteLineData: %v", err)
	}
	if err := writer.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return output.String()
}
