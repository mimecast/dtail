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
		"INFO|20210907-065632|SERVER|MAPREDUCE:TEST|foo=bar|baz=bay",
		"INFO|20210907-065732|CLIENT|MAPREDUCE:YOMAN|baz=bay|foo=bar",
	}

	for _, input := range inputs {
		fields, err := parser.MakeFields(input)

		if err != nil {
			t.Errorf("Parser unable to make fields: %s", err.Error())
		}

		if bar, ok := fields["foo"]; !ok {
			t.Errorf("Expected field 'foo', but no such field there in '%s'\n", input)
		} else if bar != "bar" {
			t.Errorf("Expected 'bar' stored in field 'foo', but got '%s' in '%s'\n", bar, input)
		}

		if bay, ok := fields["baz"]; !ok {
			t.Errorf("Expected field 'baz', but no such field there in '%s'\n", input)
		} else if bay != "bay" {
			t.Errorf("Expected 'bay' stored in field 'baz', but got '%s' in '%s'\n", bay, input)
		}

	}

	fields, err := parser.MakeFields("foozoo=bar|bazbay")
	if _, ok := fields["foo"]; ok {
		t.Errorf("Expected fiending field 'foo', but found it\n")
	}
}
