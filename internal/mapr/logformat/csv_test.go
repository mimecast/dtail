package logformat

import (
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/protocol"
)

func TestCSVLogFormat(t *testing.T) {
	parser, err := NewParser("csv", nil)
	if err != nil {
		t.Errorf("Unable to create parser: %s", err.Error())
	}

	headers := []string{"name", "last_name", "color", "profession", "employee_number"}
	dataLine1 := []string{"Paul", "Buetow", "Orange", "Site Reliability Engineer", "4242"}
	dataLine2 := []string{"Peter", "Bauer", "Black", "CEO", "1"}

	inputs := []string{
		strings.Join(headers, protocol.CSVDelimiter),
		strings.Join(dataLine1, protocol.CSVDelimiter),
		strings.Join(dataLine2, protocol.CSVDelimiter),
	}

	// First line is the header!
	if _, err := parser.MakeFields(inputs[0]); err != ErrIgnoreFields {
		t.Errorf("Unable to parse the CSV header")
	}

	// First data line
	fields, err := parser.MakeFields(inputs[1])
	if err != nil {
		t.Errorf("Unable to parse first CSV data line: %s", err.Error())
	}
	if val := fields["name"]; val != "Paul" {
		t.Errorf("Expected 'name' to be 'Paul' but got '%s'", val)
	}
	if val := fields["employee_number"]; val != "4242" {
		t.Errorf("Expected 'employee_number' to be '4242' but got '%s'", val)
	}

	// Second data line
	fields, err = parser.MakeFields(inputs[2])
	if err != nil {
		t.Errorf("Unable to parse first CSV data line: %s", err.Error())
	}
	if val := fields["last_name"]; val != "Bauer" {
		t.Errorf("Expected 'last_name' to be 'Bauer' but got '%s'", val)
	}
	if val := fields["color"]; val != "Black" {
		t.Errorf("Expected 'color' to be 'Black' but got '%s'", val)
	}
}
