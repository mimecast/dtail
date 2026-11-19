package handlers

import (
	"errors"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

type mapCommandTestParser struct{}

func (*mapCommandTestParser) MakeFields(string, string) (map[string]string, error) {
	return nil, nil
}

func TestResolveMapParserInjectsCommandDependencies(t *testing.T) {
	query, err := mapr.NewQuery(`from STATS select count($line)`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	wantParser := &mapCommandTestParser{}
	var gotName, gotHostname string
	var gotQuery *mapr.Query

	parser, err := resolveMapParser(query, "custom-default", "resolved-host",
		logging.NopLogger{}, func(name string, parsed *mapr.Query,
			hostname string) (logformat.Parser, error) {
			gotName = name
			gotQuery = parsed
			gotHostname = hostname
			return wantParser, nil
		})
	if err != nil {
		t.Fatalf("resolveMapParser: %v", err)
	}
	if parser != wantParser {
		t.Fatal("resolveMapParser did not return the parser from the injected factory")
	}
	if gotName != "custom-default" || gotQuery != query || gotHostname != "resolved-host" {
		t.Fatalf("factory inputs = (%q, %p, %q), want (%q, %p, %q)",
			gotName, gotQuery, gotHostname, "custom-default", query, "resolved-host")
	}
}

func TestResolveMapParserSelectsEffectiveLogFormat(t *testing.T) {
	tests := []struct {
		name       string
		queryText  string
		configured string
		want       string
	}{
		{
			name:      "explicit format wins",
			queryText: `from STATS select $line logformat generickv`,
			want:      "generickv",
		},
		{
			name:      "explicit format wins without table",
			queryText: `select service logformat generickv`,
			want:      "generickv",
		},
		{
			name:      "no table uses generic",
			queryText: `select service,sum(bytes) group by service`,
			want:      "generic",
		},
		{
			name:      "table uses built-in default",
			queryText: `from STATS select lifetimeConnections`,
			want:      "default",
		},
		{
			name:       "table uses configured default",
			queryText:  `from STATS select lifetimeConnections`,
			configured: "mimecast",
			want:       "mimecast",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			query, err := mapr.NewQuery(tc.queryText, logging.NopLogger{})
			if err != nil {
				t.Fatalf("NewQuery: %v", err)
			}
			var got string
			_, err = resolveMapParser(query, tc.configured, "host", logging.NopLogger{},
				func(name string, _ *mapr.Query, _ string) (logformat.Parser, error) {
					got = name
					return &mapCommandTestParser{}, nil
				})
			if err != nil {
				t.Fatalf("resolveMapParser: %v", err)
			}
			if got != tc.want {
				t.Fatalf("selected parser = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveMapParserFallback(t *testing.T) {
	query, err := mapr.NewQuery(
		`select count($line) logformat missing`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("NewQuery: %v", err)
	}
	wantParser := &mapCommandTestParser{}
	primaryErr := errors.New("primary parser unavailable")
	fallbackErr := errors.New("fallback parser unavailable")

	t.Run("uses generic parser", func(t *testing.T) {
		var names []string
		parser, err := resolveMapParser(query, "default", "host", logging.NopLogger{},
			func(name string, _ *mapr.Query, _ string) (logformat.Parser, error) {
				names = append(names, name)
				if name == "missing" {
					return nil, primaryErr
				}
				return wantParser, nil
			})
		if err != nil {
			t.Fatalf("resolveMapParser: %v", err)
		}
		if parser != wantParser || len(names) != 2 || names[0] != "missing" || names[1] != "generic" {
			t.Fatalf("fallback result = (%T, %v), want parser with [missing generic] attempts",
				parser, names)
		}
	})

	t.Run("returns fallback error", func(t *testing.T) {
		_, err := resolveMapParser(query, "default", "host", logging.NopLogger{},
			func(name string, _ *mapr.Query, _ string) (logformat.Parser, error) {
				if name == "missing" {
					return nil, primaryErr
				}
				return nil, fallbackErr
			})
		if !errors.Is(err, fallbackErr) || !strings.Contains(err.Error(), "fallback generic") {
			t.Fatalf("resolveMapParser error = %v, want wrapped fallback error", err)
		}
	})
}

func TestNewMapCommandReturnsQueryError(t *testing.T) {
	handler := newMapTestHandler(t)
	_, _, err := newMapCommand(handler, []string{"MAP", "invalid"})
	if err == nil || !strings.Contains(err.Error(), "parse map query") {
		t.Fatalf("newMapCommand error = %v, want parse context", err)
	}
}
