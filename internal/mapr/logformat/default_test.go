package logformat

import (
	"testing"
)

func TestDefaultLogFormat(t *testing.T) {
	parser, err := NewParser("default", nil)
	if err != nil {
		t.Errorf("Unable to create parser: %s", err.Error())
	}

	inputs := []string{
		"foo=bar|baz=bay",
		"INFO|20210907-065632|SERVER|MAPREDUCE:TEST|foo=bar|baz=bay",
	}

	for _, input := range inputs {
		fields, err := parser.MakeFields(input)

		if err != nil {
			t.Errorf("Parser unable to make fields: %s", err.Error())
		}

		if bar, ok := fields["foo"]; !ok {
			t.Errorf("Expected field 'foo', but no such field there\n")
		} else if bar != "bar" {
			t.Errorf("Expected 'bar' stored in field 'foo', but got '%s'\n", bar)
		}

		if bay, ok := fields["baz"]; !ok {
			t.Errorf("Expected field 'baz', but no such field there\n")
		} else if bay != "bay" {
			t.Errorf("Expected 'bay' stored in field 'baz', but got '%s'\n", bay)
		}

	}

	if _, err := parser.MakeFields("foo=bar|bazbay"); err == nil {
		t.Errorf("Expected error but didn't")
	}
}
