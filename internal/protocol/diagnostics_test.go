package protocol

import (
	"errors"
	"reflect"
	"testing"
)

func TestDiagnosticCodecsPreserveWireFormat(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "host tagged",
			got: EncodeDiagnostic(Diagnostic{
				Source: "CLIENT", Hostname: "host-a", Level: "WARN",
				Details: []any{"message", "detail"},
			}),
			want: "CLIENT|host-a|WARN|message|detail",
		},
		{
			name: "host tagged without details",
			got: EncodeDiagnostic(Diagnostic{
				Source: "CLIENT", Hostname: "host-a", Level: "INFO",
			}),
			want: "CLIENT|host-a|INFO|",
		},
		{
			name: "timed",
			got: EncodeTimedDiagnostic(TimedDiagnostic{
				Level: "ERROR", Timestamp: "0914-120000", Details: []any{"failed"},
			}),
			want: "ERROR|0914-120000|failed",
		},
		{
			name: "server error retains separators in message",
			got:  EncodeServerError("server-a", "failed|with detail"),
			want: "SERVER|server-a|ERROR|failed|with detail",
		},
		{
			name: "detail fields",
			got:  EncodeDiagnosticDetails([]any{"first", errors.New("problem"), 42}),
			want: "first|problem|42",
		},
		{
			name: "stats",
			got:  EncodeStatsLine(map[string]any{"connected": 2}),
			want: "connected=2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("encoded diagnostic = %q, want %q", test.got, test.want)
			}
		})
	}
}

func TestDecodeDiagnosticPreservesContentFields(t *testing.T) {
	got, err := DecodeDiagnostic("CLIENT|host-a|ERROR|message|detail")
	if err != nil {
		t.Fatalf("DecodeDiagnostic returned error: %v", err)
	}
	want := DecodedDiagnostic{
		Source: "CLIENT", Hostname: "host-a", Content: "ERROR|message|detail",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded diagnostic = %#v, want %#v", got, want)
	}

	if _, err := DecodeDiagnostic("CLIENT|host-a"); err == nil {
		t.Fatal("DecodeDiagnostic accepted a short frame")
	}
}

func TestScanFieldPreservesEmptyFields(t *testing.T) {
	payload := "first||third|"
	var fields []string
	for start := 0; ; {
		field, next, done := ScanField(payload, start)
		fields = append(fields, field)
		if done {
			break
		}
		start = next
	}

	want := []string{"first", "", "third", ""}
	if !reflect.DeepEqual(fields, want) {
		t.Fatalf("fields = %#v, want %#v", fields, want)
	}
	if got := FieldSeparator(); got != "|" {
		t.Fatalf("FieldSeparator() = %q, want %q", got, "|")
	}
}
