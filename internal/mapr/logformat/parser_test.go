package logformat

import (
	"errors"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/mapr"
)

type testParser struct{}

func (p *testParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	return map[string]string{"line": maprLine}, nil
}

type queryAwareTestParser struct {
	query *mapr.Query
}

func (p *queryAwareTestParser) MakeFields(maprLine, _ string) (map[string]string, error) {
	return map[string]string{"line": maprLine}, nil
}

func (p *queryAwareTestParser) setQuery(query *mapr.Query) {
	p.query = query
}

func TestRegisterParserValidation(t *testing.T) {
	if err := RegisterParser("", wrapParserFactory(newDefaultParser)); err == nil {
		t.Errorf("Expected error when registering parser with empty name")
	}

	if err := RegisterParser("test-nil-factory", nil); err == nil {
		t.Errorf("Expected error when registering parser with nil factory")
	}
}

func TestNewParserUsesRegistry(t *testing.T) {
	const parserName = "unit-test-registry-parser"
	registerParserFactoryForTest(t, parserName, func(string, string, int) (Parser, error) {
		return &testParser{}, nil
	})

	parser, err := NewParser(parserName, nil)
	if err != nil {
		t.Fatalf("Unable to create parser from registry: %s", err.Error())
	}

	fields, err := parser.MakeFields("hello", "")
	if err != nil {
		t.Fatalf("Unable to parse line: %s", err.Error())
	}
	if fields["line"] != "hello" {
		t.Errorf("Expected custom parser output, got '%s'", fields["line"])
	}
}

func TestNewParserWithHostnameInjectsHostname(t *testing.T) {
	const parserName = "unit-test-hostname-parser"
	var gotHostname string
	registerParserFactoryForTest(t, parserName, func(hostname, _ string, _ int) (Parser, error) {
		gotHostname = hostname
		return &testParser{}, nil
	})

	if _, err := NewParserWithHostname(parserName, nil, "configured-host"); err != nil {
		t.Fatalf("NewParserWithHostname: %v", err)
	}
	if gotHostname != "configured-host" {
		t.Fatalf("factory hostname = %q, want configured-host", gotHostname)
	}
}

func TestNewParserRejectsUnknownFormatWithoutReturningParser(t *testing.T) {
	parser, err := NewParser("missing-parser-format", nil)
	if err == nil {
		t.Fatal("NewParser succeeded for missing parser format")
	}
	if !strings.Contains(err.Error(), "no 'missing-parser-format' mapr log format") {
		t.Errorf("NewParser error = %q, want missing-format context", err)
	}
	if parser != nil {
		t.Fatalf("NewParser returned parser %T together with error %v", parser, err)
	}
}

func TestNewParserWrapsFactoryErrorWithoutReturningParser(t *testing.T) {
	const parserName = "unit-test-error-parser"
	wantErr := errors.New("factory failed")
	registerParserFactoryForTest(t, parserName, func(string, string, int) (Parser, error) {
		return &testParser{}, wantErr
	})

	parser, err := NewParser(parserName, nil)
	if parser != nil {
		t.Fatalf("NewParser returned parser %T together with error %v", parser, err)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("NewParser error = %v, want wrapped factory error %v", err, wantErr)
	}
}

func TestNewParserRejectsNilParserFromFactory(t *testing.T) {
	const parserName = "unit-test-nil-parser"
	registerParserFactoryForTest(t, parserName, func(string, string, int) (Parser, error) {
		return nil, nil
	})

	parser, err := NewParser(parserName, nil)
	if parser != nil {
		t.Fatalf("NewParser returned unexpected parser %T", parser)
	}
	if err == nil || !strings.Contains(err.Error(), "factory returned nil parser") {
		t.Fatalf("NewParser error = %v, want nil-parser contract error", err)
	}
}

func TestNewParserRejectsTypedNilQueryAwareParser(t *testing.T) {
	const parserName = "unit-test-typed-nil-parser"
	query, err := mapr.NewQuery("select $line", nil)
	if err != nil {
		t.Fatalf("NewQuery failed: %v", err)
	}
	registerParserFactoryForTest(t, parserName, func(string, string, int) (Parser, error) {
		var parser *queryAwareTestParser
		return parser, nil
	})

	parser, err := NewParser(parserName, query)
	if parser != nil {
		t.Fatalf("NewParser returned unexpected parser %T", parser)
	}
	if err == nil || !strings.Contains(err.Error(), "factory returned nil parser") {
		t.Fatalf("NewParser error = %v, want typed-nil parser contract error", err)
	}
}

func registerParserFactoryForTest(t *testing.T, parserName string, factory ParserFactory) {
	t.Helper()

	parserFactoriesMu.Lock()
	original, existed := parserFactories[parserName]
	parserFactoriesMu.Unlock()
	if err := RegisterParser(parserName, factory); err != nil {
		t.Fatalf("RegisterParser(%q) failed: %v", parserName, err)
	}

	t.Cleanup(func() {
		parserFactoriesMu.Lock()
		defer parserFactoriesMu.Unlock()
		if existed {
			parserFactories[parserName] = original
			return
		}
		delete(parserFactories, parserName)
	})
}
