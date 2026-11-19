package logformat

import (
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
)

func TestCSVExportRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.csv")
	q, err := mapr.NewQuery(fmt.Sprintf("select name,count(*) group by name outfile %q", path), logging.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	g := mapr.NewGroupSet(logging.NopLogger{})
	names := []string{"", "Smith, Jane", `a"b`, `"quoted, name"`}
	for i, name := range names {
		s := g.GetSet(fmt.Sprint(i))
		s.Samples, s.SValues["name"], s.FValues["count(*)"] = 1, name, 1
	}
	if writeErr := g.WriteResult(q, true); writeErr != nil {
		t.Fatal(writeErr)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	standardRows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	if err != nil {
		t.Fatalf("standard CSV reader: %v, input %q", err, data)
	}
	p, err := NewParser("csv", nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if _, err := p.MakeFields(lines[0], "export"); !errors.Is(err, ErrIgnoreFields) {
		t.Fatal(err)
	}
	for i, line := range lines[1:] {
		fields, err := p.MakeFields(line, "export")
		if err != nil || fields["name"] != names[i] || fields["count(*)"] != "1" {
			t.Fatalf("row %d fields=%v, err=%v", i, fields, err)
		}
		if !reflect.DeepEqual(standardRows[i+1], []string{names[i], "1"}) {
			t.Fatalf("standard row %d: %q", i, standardRows[i+1])
		}
	}
}

func TestCSVQuotedHeadersAndSourceOwnership(t *testing.T) {
	p, err := newCSVParser("host", "UTC", 0)
	if err != nil {
		t.Fatal(err)
	}
	buf := []byte(`"last,name","a""b",empty`)
	borrowed := unsafe.String(unsafe.SliceData(buf), len(buf))
	if _, parseErr := p.MakeFields(borrowed, "a"); !errors.Is(parseErr, ErrIgnoreFields) {
		t.Fatal(parseErr)
	}
	for i := range buf {
		buf[i] = 'x'
	}
	if _, parseErr := p.MakeFields(`empty,"a""b","last,name"`, "b"); !errors.Is(parseErr, ErrIgnoreFields) {
		t.Fatal(parseErr)
	}
	for source, line := range map[string]string{"a": `"Smith, Jane","a""b",`, "b": `,"a""b","Smith, Jane"`} {
		fields, parseErr := p.MakeFields(line, source)
		if parseErr != nil || fields["last,name"] != "Smith, Jane" || fields[`a"b`] != `a"b` || fields["empty"] != "" {
			t.Fatalf("source %s fields=%v err=%v", source, fields, parseErr)
		}
	}
	p.ReleaseSource("a")
	if _, parseErr := p.MakeFields(`"new,name"`, "a"); !errors.Is(parseErr, ErrIgnoreFields) {
		t.Fatal(parseErr)
	}
	fields, err := p.MakeFields(`""`, "a")
	if _, ok := fields["new,name"]; !ok || err != nil {
		t.Fatalf("released source fields=%v err=%v", fields, err)
	}
}

func TestCSVRejectsMalformedRecords(t *testing.T) {
	for _, line := range []string{`bare"quote,1`, `"unterminated,1`, `"closed"junk,1`, "\"multi\nline\",1", "\"multi\rline\",1"} {
		t.Run(fmt.Sprintf("%q", line), func(t *testing.T) {
			p, err := newCSVParser("host", "UTC", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.MakeFields(line, "source"); err == nil || errors.Is(err, ErrIgnoreFields) {
				t.Fatalf("malformed header accepted: %v", err)
			}
			// Invalid headers must not become persistent source state.
			if _, err := p.MakeFields("name,count", "source"); !errors.Is(err, ErrIgnoreFields) {
				t.Fatalf("valid header after rejection: %v", err)
			}
			if _, err := p.MakeFields(line, "source"); err == nil {
				t.Fatal("malformed data accepted")
			}
		})
	}
}

func TestCSVRejectsExcessFields(t *testing.T) {
	p, err := NewParser("csv", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.MakeFields("name,count", "source"); !errors.Is(err, ErrIgnoreFields) {
		t.Fatal(err)
	}
	for _, line := range []string{`plain,1,extra`, `"quoted, name",1,extra`, `"quoted, name",1,`} {
		if _, err := p.MakeFields(line, "source"); err == nil || !strings.Contains(err.Error(), "more fields than header") {
			t.Fatalf("excess fields %q: err=%v", line, err)
		}
	}
}
