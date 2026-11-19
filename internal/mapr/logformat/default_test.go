package logformat

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

func TestDefaultLogFormat(t *testing.T) {
	parser, err := NewParser("default", nil)
	if err != nil {
		t.Errorf("Unable to create parser: %s", err.Error())
	}

	date := "20211002"
	hour := "07"
	minute := "23"
	second := "42"
	time := fmt.Sprintf("%s-%s%s%s", date, hour, minute, second)

	inputs := []string{
		fmt.Sprintf("INFO|%s|1|default_test.go:0|8|14|7|0.21|471h0m21s|MAPREDUCE:STATS|foo=bar|bar=foo", time),
		fmt.Sprintf("INFO|%s|1|default_test.go:0|8|14|7|0.21|471h0m21s|MAPREDUCE:STATS|bar=foo|foo=bar", time),
	}

	for _, input := range inputs {
		fields, parseErr := parser.MakeFields(input, "")

		if parseErr != nil {
			t.Errorf("Parser unable to make fields: %s", parseErr.Error())
		}

		if val, ok := fields["$severity"]; !ok {
			t.Errorf("Expected field '$severity', but no such field there in '%s'\n", input)
		} else if val != "INFO" {
			t.Errorf("Expected 'Info' stored in field '$severity', but got '%s' in '%s'\n",
				val, input)
		}

		if val, ok := fields["$time"]; !ok {
			t.Errorf("Expected field '$time', but no such field there in '%s'\n", input)
		} else if val != time {
			t.Errorf("Expected '%s' stored in field '$time', but got '%s' in '%s'\n",
				time, val, input)
		}

		if val, ok := fields["$date"]; !ok {
			t.Errorf("Expected field '$date', but no such field there in '%s'\n", input)
		} else if val != date {
			t.Errorf("Expected '%s' stored in field '$date', but got '%s' in '%s'\n",
				date, val, input)
		}

		if val, ok := fields["$hour"]; !ok {
			t.Errorf("Expected field '$hour', but no such field there in '%s'\n", input)
		} else if val != hour {
			t.Errorf("Expected '%s' stored in field '$hour', but got '%s' in '%s'\n",
				hour, val, input)
		}

		if val, ok := fields["$minute"]; !ok {
			t.Errorf("Expected field '$minute', but no such field there in '%s'\n", input)
		} else if val != minute {
			t.Errorf("Expected '%s' stored in field '$minute', but got '%s' in '%s'\n",
				minute, val, input)
		}

		if val, ok := fields["$second"]; !ok {
			t.Errorf("Expected field '$second', but no such field there in '%s'\n", input)
		} else if val != second {
			t.Errorf("Expected '%s' stored in field '$second', but got '%s' in '%s'\n",
				second, val, input)
		}

		if val, ok := fields["foo"]; !ok {
			t.Errorf("Expected field 'foo', but no such field there in '%s'\n", input)
		} else if val != "bar" {
			t.Errorf("Expected 'bar' stored in field 'foo', but got '%s' in '%s'\n",
				val, input)
		}

		if val, ok := fields["bar"]; !ok {
			t.Errorf("Expected field 'bar', but no such field there in '%s'\n", input)
		} else if val != "foo" {
			t.Errorf("Expected 'foo' stored in field 'bar', but got '%s' in '%s'\n",
				val, input)
		}
	}

	fields, err := parser.MakeFields("foozoo=bar|bazbay", "")
	if err != nil && !errors.Is(err, ErrIgnoreFields) {
		t.Errorf("%s", err.Error())
	}

	if _, ok := fields["foo"]; ok {
		t.Errorf("Expected fiending field 'foo', but found it\n")
	}
}

func TestDefaultLogFormatQuerySpecificFields(t *testing.T) {
	q, err := mapr.NewQuery(`select count(foo) from STATS where $hostname eq "testhost"`, logging.NopLogger{})
	if err != nil {
		t.Fatalf("Unable to create query: %s", err.Error())
	}

	parser, err := NewParser("default", q)
	if err != nil {
		t.Fatalf("Unable to create parser: %s", err.Error())
	}

	fields, err := parser.MakeFields(
		"INFO|20211002-072342|1|default_test.go:0|8|14|7|0.21|471h0m21s|MAPREDUCE:STATS|foo=bar|bar=baz",
		"",
	)
	if err != nil {
		t.Fatalf("Parser unable to make fields: %s", err.Error())
	}

	requiredFields := []string{"foo", "$hostname"}
	for _, field := range requiredFields {
		if _, ok := fields[field]; !ok {
			t.Errorf("Expected query-specific field '%s' to be present", field)
		}
	}

	omittedFields := []string{"bar", "$time", "$pid", "$line"}
	for _, field := range omittedFields {
		if _, ok := fields[field]; ok {
			t.Errorf("Expected query-specific field '%s' to be omitted", field)
		}
	}
}
