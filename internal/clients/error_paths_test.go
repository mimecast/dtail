package clients

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/clients/handlers"
	"github.com/mimecast/dtail/internal/config"
	"github.com/mimecast/dtail/internal/omode"
)

func TestNewGrepClientReturnsInvalidRegexError(t *testing.T) {
	_, err := NewGrepClient(config.Args{
		ConnectionsPerCPU: 1,
		RegexStr:          "[",
		Serverless:        true,
	}, clientTestRuntimeConfig(), LoggerDependencies{})
	if err == nil || !strings.Contains(err.Error(), "compile regular expression") {
		t.Fatalf("NewGrepClient error = %v, want regex compilation error", err)
	}
}

func TestNewMaprClientReturnsInvalidQueryError(t *testing.T) {
	_, err := NewMaprClient(config.Args{QueryStr: "select from"}, config.RuntimeConfig{}, DefaultMode, LoggerDependencies{})
	if err == nil || !strings.Contains(err.Error(), "parse mapreduce query") {
		t.Fatalf("NewMaprClient error = %v, want query parse error", err)
	}
}

func TestMakeConnectionsReturnsSessionCommandError(t *testing.T) {
	client := &baseClient{
		profile: clientProfile{
			newHandler: func(string) handlers.Handler { return nil },
		},
		sessionSpec: SessionSpec{Mode: omode.Mode(255)},
	}
	err := client.makeConnections()
	if err == nil || !strings.Contains(err.Error(), "build session commands") {
		t.Fatalf("makeConnections error = %v, want wrapped session command error", err)
	}
}

func TestInitializeClosesAuthenticationResourcesOnConnectionSetupError(t *testing.T) {
	closer := &recordingCloser{}
	client := &baseClient{
		Args:       config.Args{Serverless: true},
		authCloser: closer,
		profile: clientProfile{
			newHandler: func(string) handlers.Handler { return nil },
		},
		sessionSpec: SessionSpec{Mode: omode.Mode(255)},
	}

	err := client.initialize()
	if err == nil || !strings.Contains(err.Error(), "build session commands") {
		t.Fatalf("initialize error = %v, want wrapped session command error", err)
	}
	if closer.calls != 1 {
		t.Fatalf("authentication closer calls = %d, want 1", closer.calls)
	}
	if client.authCloser != nil {
		t.Fatal("initialize retained a closed authentication resource")
	}
}

type recordingCloser struct{ calls int }

func (c *recordingCloser) Close() error {
	c.calls++
	return nil
}

func TestMakeConnectionsRejectsMissingHandlerFactory(t *testing.T) {
	client := &baseClient{sessionSpec: SessionSpec{Mode: omode.CatClient}}
	if err := client.makeConnections(); err == nil || !strings.Contains(err.Error(), "handler factory") {
		t.Fatalf("makeConnections error = %v, want missing handler factory error", err)
	}
}
