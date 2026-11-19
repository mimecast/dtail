package readhub

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fanoutUnderTest returns a fan-out processor whose entry has one subscriber,
// whose queue the test reads.
func fanoutUnderTest(t *testing.T) (*fanoutProcessor, *subscriber) {
	t.Helper()
	e := &entry{messages: make(chan string, 1)}
	sub := newSubscriber(Session{}, 64)
	e.subscribers = []*subscriber{sub}
	return newFanoutProcessor(e), sub
}

func drain(sub *subscriber) []item {
	var items []item
	for {
		select {
		case it := <-sub.queue:
			items = append(items, it)
		default:
			return items
		}
	}
}

func chunkLines(c *chunk) []string {
	var lines []string
	for i := range c.ends {
		lines = append(lines, string(c.line(i)))
	}
	return lines
}

func TestFanoutPublishesOneChunkPerFlushWithItsPositions(t *testing.T) {
	fanout, sub := fanoutUnderTest(t)
	info := fileInfo(t)

	fanout.LineEndsAt(7, info) // no line added yet: ignored
	raw := []byte("first")
	if err := fanout.ProcessRawLine(raw, 1, "src"); err != nil {
		t.Fatal(err)
	}
	fanout.LineEndsAt(6, info)
	copy(raw, "XXXXX") // the reader reuses its buffer: the chunk must not alias it
	buf := bytes.NewBufferString("second")
	if err := fanout.ProcessLine(buf, 2, "src"); err != nil {
		t.Fatal(err)
	}
	fanout.LineEndsAt(42, info)
	if err := fanout.ProcessRawLine([]byte("fragment"), 3, "src"); err != nil {
		t.Fatal(err)
	}
	if err := fanout.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := fanout.Flush(); err != nil { // nothing pending: nothing published
		t.Fatal(err)
	}

	items := drain(sub)
	if len(items) != 1 || items[0].kind != chunkItem {
		t.Fatalf("items = %+v, want one chunk", items)
	}
	c := items[0].chunk
	if got, want := chunkLines(c), []string{"first", "second", "fragment"}; !reflect.DeepEqual(got, want) {
		t.Errorf("chunk lines = %q, want %q", got, want)
	}
	// A line without a reported end, like a fragment fed on cancellation,
	// has an unknown position.
	want := []position{{offset: 6, file: info}, {offset: 42, file: info}, unknownPosition()}
	for i, wantEnd := range want {
		if got := c.lineEnd(i); got.offset != wantEnd.offset || got.file != wantEnd.file {
			t.Errorf("line %d ends at %+v, want %+v", i, got, wantEnd)
		}
	}
}

func TestFanoutPublishesAFullChunkWithItsPositions(t *testing.T) {
	fanout, sub := fanoutUnderTest(t)
	info := fileInfo(t)
	line := strings.Repeat("a", chunkSize/2)
	var end int64
	for i := 0; i < 3; i++ {
		if err := fanout.ProcessRawLine([]byte(line), 0, ""); err != nil {
			t.Fatal(err)
		}
		end += int64(len(line)) + 1
		fanout.LineEndsAt(end, info)
	}
	huge := strings.Repeat("b", 2*chunkSize)
	if err := fanout.ProcessRawLine([]byte(huge), 0, ""); err != nil {
		t.Fatal(err)
	}
	end += int64(len(huge)) + 1
	fanout.LineEndsAt(end, info)
	if err := fanout.Flush(); err != nil {
		t.Fatal(err)
	}

	items := drain(sub)
	var sizes [][]int
	var offsets []int64
	for _, it := range items {
		var lengths []int
		for _, text := range chunkLines(it.chunk) {
			lengths = append(lengths, len(text))
		}
		sizes = append(sizes, lengths)
		offsets = append(offsets, it.chunk.lineEnd(len(it.chunk.ends)-1).offset)
	}
	// Two half-size lines fill a chunk, the third starts the next one, the
	// oversized line gets a chunk of its own, and every chunk, published full
	// or by Flush, knows where its last line ends.
	wantSizes := [][]int{{chunkSize / 2, chunkSize / 2}, {chunkSize / 2}, {2 * chunkSize}}
	if !reflect.DeepEqual(sizes, wantSizes) {
		t.Errorf("chunk line lengths = %v, want %v", sizes, wantSizes)
	}
	half := int64(chunkSize/2 + 1)
	if want := []int64{2 * half, 3 * half, end}; !reflect.DeepEqual(offsets, want) {
		t.Errorf("chunk end offsets = %v, want %v", offsets, want)
	}
}

func TestFanoutPublishesPendingLinesBeforeARestart(t *testing.T) {
	fanout, sub := fanoutUnderTest(t)
	if err := fanout.ProcessRawLine([]byte("old"), 0, ""); err != nil {
		t.Fatal(err)
	}
	fanout.SourceRestarted()
	if err := fanout.ProcessRawLine([]byte("new"), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := fanout.Flush(); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, it := range drain(sub) {
		switch it.kind {
		case chunkItem:
			got = append(got, strings.Join(chunkLines(it.chunk), ","))
		case restartItem:
			got = append(got, "restart")
		}
	}
	if want := []string{"old", "restart", "new"}; !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
}

func fileInfo(t *testing.T) os.FileInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// The follow-only hooks are optional: a one-shot group entry is neither a
// readTracker nor a warningSource, so the fan-out ignores reported read
// positions for it and never waits for a warning channel of its own.
func TestFanoutOfAGroupEntryHasNoFollowHooks(t *testing.T) {
	member := &groupMember{subscriber: newSubscriber(Session{}, 64)}
	e := &groupEntry{members: []*groupMember{member}}
	fanout := newFanoutProcessor(e)
	if fanout.tracker != nil || fanout.messages != nil {
		t.Fatalf("group fan-out has follow hooks: tracker %v, messages %v", fanout.tracker, fanout.messages)
	}
	fanout.ReadUpTo(3, nil)
	if err := fanout.ProcessRawLine([]byte("a\n"), 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := fanout.Flush(); err != nil {
		t.Fatal(err)
	}
	it := <-member.queue
	if it.kind != chunkItem || !reflect.DeepEqual(chunkLines(it.chunk), []string{"a\n"}) {
		t.Errorf("group member got %+v, want the chunk of line a", it)
	}

	follow := newFanoutProcessor(&entry{messages: make(chan string, 1)})
	if follow.tracker == nil || follow.messages == nil {
		t.Error("follow fan-out lacks its read tracker or warning channel")
	}
}
