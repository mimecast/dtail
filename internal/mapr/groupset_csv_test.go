package mapr

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/logging"
)

type failingCSVWriter struct {
	err   error
	short bool
}

var _ io.Writer = failingCSVWriter{}

func (w failingCSVWriter) Write(data []byte) (int, error) {
	if w.short {
		return len(data) - 1, nil
	}
	return 0, w.err
}

func TestCSVResultEncoding(t *testing.T) {
	for _, tt := range []struct {
		name   string
		header []string
		values []string
		want   string
	}{
		{"plain", []string{"name", "count(*)"}, []string{"Jane", "1"}, "name,count(*)\nJane,1\n"},
		{"quoted", []string{"last,name", `a"b`, "empty"}, []string{"Smith, Jane", `a"b`, ""}, "\"last,name\",\"a\"\"b\",empty\n\"Smith, Jane\",\"a\"\"b\",\n"},
		{"empty single field", []string{"name"}, []string{""}, "name\n\"\"\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := &Query{Limit: -1}
			for _, name := range tt.header {
				q.Select = append(q.Select, selectCondition{FieldStorage: name})
			}
			var out strings.Builder
			g := NewGroupSet(logging.NopLogger{})
			if err := g.resultWriteUnformatted(q, []result{{values: tt.values}}, &out, true); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Fatalf("CSV = %q, want %q", out.String(), tt.want)
			}
			rows, err := csv.NewReader(strings.NewReader(out.String())).ReadAll()
			if err != nil || !reflect.DeepEqual(rows, [][]string{tt.header, tt.values}) {
				t.Fatalf("decoded rows = %#v, err = %v", rows, err)
			}
		})
	}
}

func TestCSVResultWriteErrors(t *testing.T) {
	writeErr := errors.New("disk full")
	q := &Query{Limit: -1, Select: []selectCondition{{FieldStorage: "name"}}}
	for _, size := range []int{1, 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			// A small record fails on Flush; a large one fails in Write.
			g := NewGroupSet(logging.NopLogger{})
			err := g.resultWriteUnformatted(q, []result{{values: []string{strings.Repeat("x", size)}}}, failingCSVWriter{err: writeErr}, true)
			if !errors.Is(err, writeErr) {
				t.Fatalf("error = %v, want %v", err, writeErr)
			}
		})
	}
	for _, writeHeader := range []bool{false, true} {
		for _, short := range []bool{false, true} {
			wantErr := writeErr
			if short {
				wantErr = io.ErrShortWrite
			}
			g := NewGroupSet(logging.NopLogger{})
			err := g.resultWriteUnformatted(q, []result{{values: []string{""}}}, failingCSVWriter{err: writeErr, short: short}, writeHeader)
			if !errors.Is(err, wantErr) {
				t.Fatalf("single empty field: header=%v short=%v error=%v, want %v", writeHeader, short, err, wantErr)
			}
		}
	}
}

func TestCSVResultRejectsNewlinesBeforeWriting(t *testing.T) {
	g := NewGroupSet(logging.NopLogger{})
	for _, newline := range []string{"\n", "\r", "\r\n"} {
		for _, header := range []bool{false, true} {
			q := &Query{Limit: -1, Select: []selectCondition{{FieldStorage: "name"}}}
			rows := []result{{values: []string{"valid"}}, {values: []string{"bad" + newline + "value"}}}
			if header {
				q.Select[0].FieldStorage += newline
			}
			var out strings.Builder
			err := g.resultWriteUnformatted(q, rows, &out, true)
			if err == nil || !strings.Contains(err.Error(), "multiline CSV") || out.Len() != 0 {
				t.Fatalf("header=%v newline=%q: error=%v output=%q", header, newline, err, out.String())
			}
		}
	}
	// A limited-out row is not exported and must not block a valid result.
	q := &Query{Limit: 1, Select: []selectCondition{{FieldStorage: "name"}}}
	var out strings.Builder
	if err := g.resultWriteUnformatted(q, []result{{values: []string{"valid"}}, {values: []string{"bad\nvalue"}}}, &out, true); err != nil {
		t.Fatal(err)
	}
	if out.String() != "name\nvalid\n" {
		t.Fatalf("limited output = %q", out.String())
	}
}

func TestCSVResultAppendAndRejectedPublication(t *testing.T) {
	for _, appendMode := range []bool{false, true} {
		t.Run(fmt.Sprint(appendMode), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.csv")
			q, err := NewQuery("select name,count(*) group by name", logging.NopLogger{})
			if err != nil {
				t.Fatal(err)
			}
			q.Outfile = &Outfile{FilePath: path, AppendMode: appendMode}
			g := NewGroupSet(logging.NopLogger{})
			s := g.GetSet("row")
			s.Samples, s.SValues["name"], s.FValues["count(*)"] = 1, "Smith, Jane", 1
			for range 2 {
				if writeErr := g.WriteResult(q, true); writeErr != nil {
					t.Fatal(writeErr)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want := "name,count(*)\n\"Smith, Jane\",1\n"
			if appendMode {
				want += "\"Smith, Jane\",1\n"
			}
			if string(before) != want {
				t.Fatalf("output = %q, want %q", before, want)
			}
			s.SValues["name"] = "bad\nvalue"
			if writeErr := g.WriteResult(q, true); writeErr == nil || !strings.Contains(writeErr.Error(), "multiline CSV") {
				t.Fatalf("newline export error = %v", writeErr)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatalf("failed export changed destination: %q, err %v", after, err)
			}
			if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed export left temporary file: %v", err)
			}
		})
	}
}
