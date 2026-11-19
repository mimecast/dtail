package fs

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// A snapshot read must match the regex on the line without its LF, so that
// end-of-line anchors and trailing negated classes work as in a follow read.
func TestSnapshotReadMatchesWithoutLineTerminator(t *testing.T) {
	const content = "foo\nfoobar\n\nbar foo"
	tests := []struct {
		pattern string
		want    []string
	}{
		{`foo$`, []string{"foo", "bar foo"}},
		{`foo[^x]`, []string{"foobar"}},
		{`^$`, []string{""}},
	}
	for _, tc := range tests {
		re, err := regex.New(tc.pattern, regex.Default)
		if err != nil {
			t.Fatal(err)
		}
		path := writeProcessorTestFile(t, content)
		cat := newSnapshotReadFile(path, "id", nil, defaultMaxLineLength, testLogger)
		p := &captureProcessor{}
		if err := cat.Start(context.Background(), lcontext.LContext{}, p, re); err != nil {
			t.Fatalf("%s: %v", tc.pattern, err)
		}
		var got []string
		for _, l := range p.lines {
			got = append(got, strings.TrimSuffix(l, "\n"))
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pattern %q: got %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
