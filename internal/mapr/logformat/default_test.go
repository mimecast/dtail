package logformat

import (
	"fmt"
	"testing"
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
		fields, err := parser.MakeFields(input)

		if err != nil {
			t.Errorf("Parser unable to make fields: %s", err.Error())
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

	fields, err := parser.MakeFields("foozoo=bar|bazbay")
	if _, ok := fields["foo"]; ok {
		t.Errorf("Expected fiending field 'foo', but found it\n")
	}
}
