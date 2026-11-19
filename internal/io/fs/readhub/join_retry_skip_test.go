package readhub

import (
	"os"
	"strings"
	"testing"

	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// A join that found its entry closed (the last subscriber left meanwhile) is
// retried with a new entry, whose reader starts at the end of the file as of
// the join. The join skip the failed attempt recorded against the dead
// reader must not survive: with it the session dropped the new lines up to
// that reader's offset.
func TestHubJoinRetryDropsTheSkipOfTheClosedEntry(t *testing.T) {
	hub := newTestHub()
	file := newTestFile(t)
	first := syncReader(t, hub, file)
	file.appendLines(strings.Repeat("old content ", 50))
	waitFor(t, "the long line", func() bool { return first.recorder.hasLine(strings.Repeat("old content ", 50)) })

	rec := &recorder{}
	session := Session{
		Target:       file.target(),
		FilePath:     file.path,
		GlobID:       "late",
		LContext:     lcontext.LContext{},
		Regex:        regex.NewNoop(),
		NewProcessor: rec.newProcessor,
	}
	sub := newSubscriber(session, hub.options.QueueChunks)

	// The first iteration of Hub.join finds the live entry ...
	e := hub.entryFor(file.path)
	if e == nil {
		t.Fatal("no entry for the followed file")
	}
	// ... the file is truncated below what the reader has read, and the
	// last subscriber leaves before the session is added.
	if err := os.WriteFile(file.path, []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := first.stop(t); err != nil {
		t.Fatalf("first session: %v", err)
	}
	if e.join(sub, func() position { return hub.endOfFile(session) }) {
		t.Fatal("the session joined an entry its last subscriber had left")
	}
	if !sub.skip.active || sub.skip.readThrough == 0 {
		t.Fatalf("the failed join recorded no skip against the old reader: %+v", sub.skip)
	}

	// The retry makes a new entry.
	sub.entry = hub.join(sub)
	defer func() {
		hub.leave(sub.entry, sub)
		sub.held.close()
	}()
	if sub.skip.active || sub.skip.readThrough != 0 || sub.skip.oldFile != nil {
		t.Fatalf("the session kept the skip of the closed entry: %+v", sub.skip)
	}
}
