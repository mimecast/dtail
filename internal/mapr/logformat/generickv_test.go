package logformat

import "testing"

func TestGenericKVParserSkipsMalformedFieldsAndContinues(t *testing.T) {
	t.Parallel()

	parser, err := newGenericKVParser("test-host", "UTC", 0)
	if err != nil {
		t.Fatalf("newGenericKVParser() error = %v", err)
	}
	fields, err := parser.MakeFields("first=1|malformed|second=2|also-malformed", "source")
	if err != nil {
		t.Fatalf("MakeFields() error = %v", err)
	}
	if fields["first"] != "1" || fields["second"] != "2" {
		t.Fatalf("MakeFields() = %#v, want valid fields surrounding malformed input", fields)
	}
	if _, found := fields["malformed"]; found {
		t.Fatalf("MakeFields() retained malformed field: %#v", fields)
	}
}
