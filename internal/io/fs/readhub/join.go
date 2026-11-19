package readhub

import "os"

// joinSkip tells a session that joined a running shared read which published
// lines were in the file before it joined, so that it skips them like a
// private follow read opened at the join would, which starts at the end of
// the file. Such lines reach the session when the shared reader lags behind
// the file: it has not published everything it read yet, it still reads a
// file the path was rotated away from, or it has not noticed yet that the
// file was truncated. Skipping ends with the first line the session gets.
type joinSkip struct {
	active bool
	// at is the end of the file at the path when the session joined.
	at position
	// oldFile is the file the reader had open at the join when that was not
	// the file at the path any more: the path was rotated before the join.
	// Its lines, up to the reopen that moves the reader on, predate the join.
	oldFile os.FileInfo
	// readThrough, when beyond at.offset, is how far the reader had read the
	// file at the join, which had been truncated below that: lines ending up
	// to readThrough are the old content, up to the restart that rewinds the
	// reader.
	readThrough int64
}

// newJoinSkip returns the skip state of a session that joined when the file
// at the path ended at joinedAt and the reader had read reader, the file it
// had open and how far, or an unknown position between two reads.
func newJoinSkip(joinedAt, reader position) joinSkip {
	skip := joinSkip{active: joinedAt.offset >= 0 && joinedAt.file != nil, at: joinedAt}
	if !skip.active || reader.offset < 0 || reader.file == nil {
		return skip
	}
	switch {
	case !os.SameFile(reader.file, joinedAt.file):
		skip.oldFile = reader.file
	case reader.offset > joinedAt.offset:
		skip.readThrough = reader.offset
	}
	return skip
}

// skips reports whether the published line that ends at end predates the
// join. The first line that does not ends skipping.
func (s *joinSkip) skips(end position) bool {
	if !s.active {
		return false
	}
	switch {
	case !end.known() || end.file == nil:
		// Without a position there is nothing to compare; deliver.
	case s.oldFile != nil && os.SameFile(end.file, s.oldFile):
		return true
	case os.SameFile(end.file, s.at.file):
		if end.offset <= max(s.at.offset, s.readThrough) {
			return true
		}
	}
	// A line appended after the join, or a line of a file the path was
	// rotated to after the join, which a private read reads from its
	// beginning.
	s.active = false
	return false
}

// restart reports whether the truncation the reader announced happened before
// the join, so that the session, which has not seen a line yet, ignores it
// and keeps skipping: a private read opened at the join would not notice it.
func (s *joinSkip) restart() bool {
	switch {
	case !s.active:
		return false
	case s.oldFile != nil:
		// The rotated-away file was truncated; its lines are skipped anyway.
		return true
	case s.readThrough > s.at.offset:
		// The truncation seen at the join: skip the rewritten content up to
		// the join position.
		s.readThrough = 0
		return true
	}
	// The file was truncated after the join; a private read starts over.
	s.active = false
	return false
}

// reopen reports whether the reader moving on to the file at the path is the
// one after a rotation before the join, which the session ignores.
func (s *joinSkip) reopen() bool {
	if !s.active {
		return false
	}
	if s.oldFile != nil {
		s.oldFile = nil
		s.readThrough = 0
		return true
	}
	// The path was rotated after the join; a private read reads the new
	// file from its beginning with a new processor.
	s.active = false
	return false
}
