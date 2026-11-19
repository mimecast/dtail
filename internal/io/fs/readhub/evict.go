package readhub

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/omode"
)

// A subscriber whose queue is full when the shared reader publishes is
// evicted instead of making the reader, and so every other session of the
// file, wait for it. The evicted session handles what it has queued, then
// goes on with a private follow reader of its own that starts just past the
// last line it handled and feeds the session's filter and processor, so it
// loses no line and keeps its line numbering and local context. That reader
// first reads the session's descriptor of the file the shared reader had open
// at the eviction (see heldFile), so a rotation of the path, before the
// eviction while the shared reader was still behind in the old file or while
// the session handles its queue, does not cost it the rest of the old file.
// Once the private reader caught up, the session rejoins a shared reader (see
// rejoin.go).

// evict stops deliveries to sub, whose queue had no room for missed, and
// tells it to go on privately. The caller holds e.publishMu, so nothing is
// queued after the eviction. An entry left without subscribers is closed, so
// that no session joins it any more, and stops.
func (e *entry) evict(sub *subscriber, missed item) {
	e.mu.Lock()
	remaining, found := e.detach(sub)
	if !found {
		e.mu.Unlock()
		return
	}
	sub.missed = missed
	close(sub.evicted)
	e.logger.Info(e.path, "Shared follow read evicted a slow subscriber", fmt.Sprintf("subscribers=%d", remaining))
	if e.owner == sub && remaining > 0 {
		e.handOver(e.subscribers[0])
	}
	e.mu.Unlock()
	if e.seams.evicted != nil {
		e.seams.evicted(remaining)
	}

	if remaining == 0 {
		// The evicted session keeps the file's target valid until it
		// leaves, but nobody needs the shared reader any longer.
		e.onFailure(e)
		e.stop()
	}
}

// detach removes sub from the subscriber list and returns how many remain and
// whether sub was listed. An entry without subscribers is closed. The caller
// holds e.mu.
func (e *entry) detach(sub *subscriber) (remaining int, found bool) {
	for i, candidate := range e.subscribers {
		if candidate == sub {
			e.subscribers = append(e.subscribers[:i], e.subscribers[i+1:]...)
			found = true
			break
		}
	}
	if len(e.subscribers) == 0 {
		e.closed = true
	}
	return len(e.subscribers), found
}

// readPrivately goes on with a private follow reader of the session's own
// target, from where the session's read got to, until ctx ends. Like the read
// command's retry loop, it reads the file again after a read ended, e.g.
// after a rotation, with a new processor, and returns errRejoined when the
// session rejoined a shared reader at the end of the file. held, if not nil, is the session's
// descriptor of the file the shared reader had open when the session left it;
// readPrivately closes it.
func (r *sessionRead) readPrivately(ctx context.Context, held *os.File) error {
	reader, err := r.privateReader(held)
	if err != nil {
		return err
	}
	for {
		err := reader.StartFiltered(ctx, r.filter)
		if errors.Is(err, fs.ErrHandedOver) {
			return errRejoined
		}
		if errors.Is(err, fs.ErrStartOffsetFileChanged) {
			r.readRotatedFile()
			continue
		}
		if err != nil {
			r.logger.Error(r.session.FilePath, r.session.GlobID, err)
			if errors.Is(err, fs.ErrReaderWorkerPanic) {
				return err
			}
			// The read command's retry loop reports a failed read of a
			// private reader to the client before it reads the file again
			// (see executeReadLoop); this loop is that loop for the session.
			r.reportFailure()
		}
		if ctx.Err() != nil || !ctxutil.Sleep(ctx, r.options.RetryInterval) {
			return nil
		}
		r.logger.Info(r.session.FilePath, r.session.GlobID, "Reading file again")
		r.newProcessor()
	}
}

// readRotatedFile prepares the session for the private reader's next start,
// which reads the file now at the path from its beginning: the path was
// rotated after the session's read got to r.at, which the private reader
// found before reading anything. Like a private reader after a rotation, the
// session reads the new file with a new processor.
func (r *sessionRead) readRotatedFile() {
	r.logger.Warn(r.session.FilePath, r.session.GlobID,
		"File was rotated after the last line of the shared read, reading the new file from its beginning;"+
			" lines the old file had after that line, if any, are not read", "offset", r.at.offset)
	r.newProcessor()
	r.at = position{offset: 0}
	r.atLineEnd = false
}

// privateReader makes the session's private follow reader, positioned just
// past the last line the session handled, or where it joined. Its first read
// uses held when that is the file of that position, and closes it.
func (r *sessionRead) privateReader(held *os.File) (*fs.ReadFile, error) {
	target := r.session.Target
	options := fs.ReadOptions{
		Mode:           omode.TailClient,
		Target:         &target,
		FilePath:       r.session.FilePath,
		GlobID:         r.session.GlobID,
		ServerMessages: r.session.ServerMessages,
		MaxLineLength:  r.options.MaxLineLength,
		Logger:         r.messageLogger(),
	}
	if !r.at.known() {
		r.logger.Warn(r.session.FilePath, r.session.GlobID,
			"Position of the shared read unknown, following privately from the end of the file")
	}
	r.at.startAt(&options)
	if r.rejoin != nil {
		options.HandOverAtEOF = r.handOver
	}
	if held != nil && r.holds(held) {
		options.StartFile = held
	} else {
		closeHeld(held)
	}
	// The shared reader warned about the line it split there already.
	options.StartOffsetInSplitLine = r.atLineEnd && splitsLine(options.StartFile, target, r.at)
	reader, err := fs.NewReadFile(options)
	if err != nil {
		closeHeld(options.StartFile)
		return nil, fmt.Errorf("private follow reader after the shared read: %w", err)
	}
	return reader, nil
}

// holds reports whether held, the file the shared reader had open when the
// session left it, is the file the session's read goes on in: the file of its
// position, or, when the session is at the beginning of a file it has not
// seen a line of since the reader announced a new read, the file the reader
// opened for that read, which held then is.
func (r *sessionRead) holds(held *os.File) bool {
	switch {
	case r.at.offset > 0:
		return sameFile(held, r.at.file)
	case r.at.offset == 0:
		return r.at.file == nil || sameFile(held, r.at.file)
	}
	return false
}

// splitsLine reports whether at, the end of a line the shared reader fed, is
// in the middle of a line of the file: the reader split a line longer than
// the maximum line length there. A line ends at a newline otherwise. It reads
// the file through held, if given, or opens it through target.
func splitsLine(held *os.File, target fs.ValidatedReadTarget, at position) bool {
	if at.offset <= 0 || at.file == nil {
		return false
	}
	fd := held
	if fd == nil {
		opened, err := target.Open()
		if err != nil {
			return false
		}
		defer func() { _ = opened.Close() }()
		fd = opened
	}
	if !sameFile(fd, at.file) {
		return false
	}
	var last [1]byte
	if _, err := fd.ReadAt(last[:], at.offset-1); err != nil {
		return false
	}
	return last[0] != '\n'
}
