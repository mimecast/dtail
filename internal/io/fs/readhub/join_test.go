package readhub

import (
	"testing"
)

func TestJoinSkip(t *testing.T) {
	joined, old, rotated := fileInfo(t), fileInfo(t), fileInfo(t)
	at := func(offset int64) position { return position{offset: offset, file: joined} }

	type step struct {
		// kind is "line", "restart" or "reopen"; skipped is the expected
		// result for a line, or whether the session ignores the item.
		kind    string
		end     position
		skipped bool
	}
	tests := []struct {
		name   string
		reader position
		steps  []step
	}{
		{"reader on the file, lines up to the join are skipped", at(50), []step{
			{"line", at(80), true}, {"line", at(100), true}, {"line", at(120), false},
			// Skipping ended: everything is delivered.
			{"line", at(90), false},
		}},
		{"unknown reader position", unknownPosition(), []step{
			{"line", at(100), true}, {"line", at(101), false},
		}},
		{"a line without a position ends skipping", at(50), []step{
			{"line", unknownPosition(), false}, {"line", at(60), false},
		}},
		{"truncated after the join: a private read starts over", at(50), []step{
			{"restart", position{}, false}, {"line", at(10), false},
		}},
		{"rotated after the join: the new file is read from its beginning", at(50), []step{
			{"line", at(100), true}, {"reopen", position{}, false}, {"line", position{offset: 5, file: rotated}, false},
		}},
		{"a line of a file rotated to after the join ends skipping", unknownPosition(), []step{
			{"line", position{offset: 5, file: rotated}, false},
		}},
		{"rotated before the join: the old file, then the new one up to the join", position{offset: 500, file: old}, []step{
			{"line", position{offset: 600, file: old}, true},
			// The old file's truncation is no concern of the session.
			{"restart", position{}, true},
			{"line", position{offset: 7, file: old}, true},
			{"reopen", position{}, true},
			{"line", at(40), true}, {"line", at(100), true}, {"line", at(130), false},
		}},
		{"rotated before the join, and again before the reader moved on", position{offset: 500, file: old}, []step{
			{"reopen", position{}, true}, {"line", position{offset: 5, file: rotated}, false},
		}},
		{"rotated before the join, then after the reader moved on", position{offset: 500, file: old}, []step{
			{"reopen", position{}, true}, {"line", at(100), true}, {"reopen", position{}, false},
		}},
		{"truncated before the join: old content up to the restart, then new content up to the join", at(300), []step{
			{"line", at(250), true}, {"line", at(300), true},
			{"restart", position{}, true},
			{"line", at(60), true}, {"line", at(100), true},
			// A second truncation happened after the join.
			{"restart", position{}, false},
			{"line", at(60), false},
		}},
		{"truncated before the join, grown back past the reader unnoticed", at(300), []step{
			{"line", at(250), true}, {"line", at(320), false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip := newJoinSkip(at(100), tt.reader)
			for i, s := range tt.steps {
				var got bool
				switch s.kind {
				case "line":
					got = skip.skips(s.end)
				case "restart":
					got = skip.restart()
				case "reopen":
					got = skip.reopen()
				}
				if got != s.skipped {
					t.Fatalf("step %d (%s ending at %d): skipped = %v, want %v", i, s.kind, s.end.offset, got, s.skipped)
				}
			}
		})
	}
}

func TestJoinSkipIsInactiveWithoutAJoinPosition(t *testing.T) {
	for _, joinedAt := range []position{unknownPosition(), {offset: 0}} {
		skip := newJoinSkip(joinedAt, unknownPosition())
		if skip.skips(position{offset: 3, file: fileInfo(t)}) || skip.restart() || skip.reopen() {
			t.Errorf("a session joined at %+v skipped something", joinedAt)
		}
	}
}
