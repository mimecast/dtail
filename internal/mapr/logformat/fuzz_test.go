package logformat

import (
	"errors"
	"testing"
)

func FuzzDefaultParse(f *testing.F) {
	for _, seed := range []string{
		"INFO|20211002-071209|1|main.go:1|8|14|7|0.21|1h0m0s|MAPREDUCE:STATS|status=ok",
		"INFO|||||||||MAPREDUCE:STATS|empty=",
		"ERROR|not-a-mapreduce-line",
		"\x00|\xff|key=value",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		parser, err := newDefaultParser("fuzz-host", "UTC", 0)
		if err != nil {
			t.Fatalf("newDefaultParser() error = %v", err)
		}
		fields, parseErr := parser.MakeFields(line, "fuzz-source")
		if parseErr == nil && fields == nil {
			t.Fatalf("MakeFields(%q) succeeded with nil fields", line)
		}
		if errors.Is(parseErr, ErrIgnoreFields) && fields != nil {
			t.Fatalf("MakeFields(%q) ignored input but returned fields %#v", line, fields)
		}
	})
}

func FuzzGenericKVParse(f *testing.F) {
	for _, seed := range []string{
		"status=ok|latency=12",
		"empty=|=value|missing-separator",
		"\x00=\xff",
		"",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		parser, err := newGenericKVParser("fuzz-host", "UTC", 0)
		if err != nil {
			t.Fatalf("newGenericKVParser() error = %v", err)
		}
		fields, parseErr := parser.MakeFields(line, "fuzz-source")
		if parseErr != nil {
			t.Fatalf("MakeFields(%q) error = %v", line, parseErr)
		}
		if fields == nil {
			t.Fatalf("MakeFields(%q) returned nil fields", line)
		}
	})
}

func FuzzCSVParse(f *testing.F) {
	f.Add("name;value", "alpha;1")
	f.Add("single", "value")
	f.Add("empty;", ";")
	f.Add("\x00;\xff", "\xff;\x00")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, header, line string) {
		parser, err := newCSVParser("fuzz-host", "UTC", 0)
		if err != nil {
			t.Fatalf("newCSVParser() error = %v", err)
		}
		if fields, headerErr := parser.MakeFields(header, "fuzz-source"); !errors.Is(headerErr, ErrIgnoreFields) || fields != nil {
			t.Fatalf("header parse = fields:%#v error:%v, want ErrIgnoreFields", fields, headerErr)
		}
		fields, parseErr := parser.MakeFields(line, "fuzz-source")
		if parseErr == nil && fields == nil {
			t.Fatalf("data parse of %q succeeded with nil fields", line)
		}
	})
}

func FuzzGenericParse(f *testing.F) {
	for _, seed := range []string{"plain text", "\x00\xff", "", "a|b|c"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		parser, err := newGenericParser("fuzz-host", "UTC", 0)
		if err != nil {
			t.Fatalf("newGenericParser() error = %v", err)
		}
		fields, parseErr := parser.MakeFields(line, "fuzz-source")
		if parseErr != nil {
			t.Fatalf("MakeFields(%q) error = %v", line, parseErr)
		}
		if fields == nil {
			t.Fatalf("MakeFields(%q) returned nil fields", line)
		}
	})
}
