// Package readhub shares reads of a file between dserver sessions: one follow
// read between the sessions that tail the file (Hub.Follow), and one snapshot
// read between the members of a scheduled job group (Hub.ReadOnce). One reader
// per file reads, decompresses and splits the file into lines once and fans
// the lines out to every subscribed session. Each session keeps its own regex,
// local context, line numbering and processor, applied by an fs.LineFilter on
// the session's own goroutine, so a session sees the lines it would see from a
// private read, apart from where a follow session starts (see Hub.Follow).
package readhub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/regex"
)

// defaultQueueChunks bounds how many chunks, of about chunkSize bytes each, a
// subscriber may have waiting: a follow subscriber with a full queue is
// evicted to a private reader, a group member makes the group read wait.
const defaultQueueChunks = 64

const defaultRetryInterval = 2 * time.Second

// ErrStopped reports that the session's max-count limit ended its read. A
// private follow reader returns from Start at this point and the read command
// starts it again, which re-reads the file from its beginning; a shared read
// cannot do that for one session, so it hands the read back to the caller.
var ErrStopped = errors.New("shared read stopped by the session's max-count limit")

// ErrReaderFailed reports that the shared reader panicked, so the session's
// read ended; the error also wraps fs.ErrReaderWorkerPanic, as a private
// reader's worker panic does. After any other failure of the shared reader
// the session goes on with a private reader.
var ErrReaderFailed = errors.New("shared reader failed")

// Options configures a Hub.
type Options struct {
	// Logger receives the hub's diagnostics.
	Logger logging.Logger
	// MaxLineLength is the line length at which the reader splits a line.
	MaxLineLength int
	// RetryInterval is the pause before the reader opens the file again after
	// a read ended, e.g. because the file was rotated, as the read command's
	// retry interval is for a private reader. Zero selects 2 seconds.
	RetryInterval time.Duration
	// QueueChunks bounds each subscriber's queue; zero selects a default. A
	// follow subscriber whose queue is full is evicted to a private reader; a
	// group member's full queue makes the group read wait for it.
	QueueChunks int
	// GroupWait bounds how long a one-shot group read waits for its members
	// to join (see Hub.ReadOnce); zero selects DefaultGroupWait.
	GroupWait time.Duration
	// MaxGroupMembers bounds how many sessions share a one-shot group read;
	// dserver sets it to its cat slots, as every member holds one during the
	// read. Zero means no bound.
	MaxGroupMembers int
	// GroupMemory is how long the hub remembers a one-shot group read that
	// ended; zero selects DefaultGroupMemory.
	GroupMemory time.Duration
	// MaxEndedGroups bounds how many ended one-shot group reads the hub
	// remembers; zero selects DefaultMaxEndedGroups.
	MaxEndedGroups int
}

// Session describes one session's follow read of a file.
type Session struct {
	// Target is the session's own validated target for the file. The shared
	// reader opens the file only through the target of a subscribed session.
	Target fs.ValidatedReadTarget
	// FilePath is the path the session reads, as for a private reader.
	FilePath string
	// GlobID is the source ID the session's processor receives with a line.
	GlobID string
	// LContext, Regex: the session's local context and line regex.
	LContext lcontext.LContext
	Regex    regex.Regex
	// ServerMessages receives the reader's messages for the session, such as
	// the long line warning. It may be nil.
	ServerMessages chan<- string
	// Logger words the session's messages, as the logger of its private
	// reader would; nil selects the hub's logger.
	Logger logging.Logger
	// NewProcessor makes the processor for one read of the file, like the
	// read command does for every iteration of its retry loop. The hub
	// flushes and closes every processor it made.
	NewProcessor func() line.Processor
	// ReportFailure, if set, tells the session's client that its read failed
	// and its output is incomplete, as the read command does after a failed
	// iteration of a private read. The hub calls it, on the session's own
	// goroutine, for every failure it handles itself instead of returning it
	// (see sessionRead.reportFailure); the caller reports the failures Follow
	// returns. It may be called more than once for one session, so a caller
	// that reports only once keeps that to itself.
	ReportFailure func()
}

// Hub shares follow reads of the same file between sessions. It is safe for
// concurrent use.
type Hub struct {
	options Options
	logger  logging.Logger
	seams   hubSeams

	mu      sync.Mutex
	entries map[entryKey]*entry
	// oneshot holds the one-shot group reads; see oneshot.go.
	oneshot groupReads
}

// hubSeams are the reader operations an entry performs, replaceable in tests.
type hubSeams struct {
	startReader   func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error
	replaceTarget func(reader *fs.ReadFile, target fs.ValidatedReadTarget) error
	// openFile opens a subscriber's descriptor of the reader's file.
	openFile func(target fs.ValidatedReadTarget) (*os.File, error)
	// evicted, when set, is called after an eviction left remaining
	// subscribers, before an entry without any stops.
	evicted func(remaining int)
	// now, rejoinMinDelay and rejoinMaxDelay pace an evicted session's
	// attempts to rejoin (see rejoinPolicy); zero values select time.Now and
	// the defaults.
	now            func() time.Time
	rejoinMinDelay time.Duration
	rejoinMaxDelay time.Duration
}

func defaultSeams() hubSeams {
	return hubSeams{
		startReader: func(ctx context.Context, reader *fs.ReadFile, processor line.Processor) error {
			return reader.Start(ctx, lcontext.LContext{}, processor, regex.NewNoop())
		},
		replaceTarget: func(reader *fs.ReadFile, target fs.ValidatedReadTarget) error {
			return reader.ReplaceTarget(target)
		},
		openFile: openTarget,
	}
}

type entryKey struct {
	path        string
	compression string
}

// New returns an empty hub.
func New(options Options) *Hub {
	if options.RetryInterval <= 0 {
		options.RetryInterval = defaultRetryInterval
	}
	if options.QueueChunks <= 0 {
		options.QueueChunks = defaultQueueChunks
	}
	return &Hub{
		options: options,
		logger:  logging.OrNop(options.Logger),
		seams:   defaultSeams(),
		entries: make(map[entryKey]*entry),
	}
}

// Follow feeds session with the lines appended to its file from now on, from
// the file's shared reader, which it starts if no other session follows the
// file. It blocks like a private follow read and returns nil once ctx ends,
// ErrStopped when the session's max-count limit ended the read, and a
// processor error or ErrReaderFailed together with fs.ErrReaderWorkerPanic
// (the shared reader panicked) otherwise. After ErrStopped or an error, a
// private reader would start over; the caller decides how to continue.
//
// A failure Follow goes on from instead of returning it — a failed read of
// the shared reader, a failed read of the session's own reader after an
// eviction, or a shared reader that failed without panicking — is told to the
// session through Session.ReportFailure, so that its client hears of every
// failed read that a private read would report to it.
//
// Like a private follow read, which starts at the end of the file when it
// opens it, a session starts at the end of the file at the path when it
// joins: it skips published lines that were in the file already, also while
// the shared reader still reads a file the path was rotated away from before
// the join, or has not yet noticed that the file was truncated before it, and
// it keeps its processor until it gets a line. Two differences remain. When
// the join falls in the middle of a line, the private reader delivers the
// rest of that line, a shared session that whole line. And a trailing line
// the file's writer has not finished yet is not delivered when the session
// leaves, unlike the private reader, which passes such a fragment on when its
// read is cancelled.
//
// A session that falls behind by more than Options.QueueChunks chunks is
// evicted, so it never delays the other sessions: it handles what it has
// queued, including the rotation, truncation or failure that did not fit, and
// goes on with a private follow reader of its own target, just past the last
// line it handled, with the same filter and processor. A session whose shared
// reader failed without a panic goes on privately the same way. Every session
// holds a descriptor of its own of the file the shared reader has open (see
// heldFile), which that private reader reads first, so a rotation of the path
// costs the session no line, whether it came before the eviction, while the
// shared reader was still behind in the old file, or after it. Only when the
// path is rotated in the moment between the shared reader opening a file and
// the session opening its descriptor of it, does a session lack one; if it
// is then evicted after the rotation with lines of that file unread, its
// private reader reads the new file from its beginning with a new processor
// and warns that the rest of the old file is not read. An evicted session
// rejoins the shared reader once its private reader caught up with it: when
// the private reader reaches the end of the file at the start of a line
// that the shared reader has neither read nor published past yet, while that
// reader is not in the middle of a read, or it starts a new
// shared reader there when there is none, with the same filter and processor
// and without losing or repeating a line (see rejoin.go). A session evicted
// again soon after it rejoined waits longer before it rejoins once more.
func (h *Hub) Follow(ctx context.Context, session Session) error {
	if err := validateSession(session); err != nil {
		return err
	}
	if format := fs.CompressionFormat(session.FilePath); format != "" {
		return fmt.Errorf("shared follow read requires an uncompressed file, got %s", format)
	}
	sub := newSubscriber(session, h.options.QueueChunks)
	sub.entry = h.join(sub)
	defer func() {
		// The entry the session was subscribed to last, which it may have
		// rejoined after an eviction.
		h.leave(sub.entry, sub)
		// Unless the session's private reader took it over, the descriptor
		// is closed here, after leave: an eviction racing with the end of the
		// session's read may still have left it held.
		sub.held.close()
	}()
	return sub.run(ctx, h)
}

func validateSession(session Session) error {
	switch {
	case session.Target.Kind != fs.FileKind:
		return fmt.Errorf("shared read requires a file target, got kind %d", session.Target.Kind)
	case session.Target.ResolvedPath() == "":
		return errors.New("shared read requires a validated target")
	case session.FilePath == "":
		return errors.New("shared read requires a file path")
	case session.NewProcessor == nil:
		return errors.New("shared read requires a processor factory")
	}
	return nil
}

// join adds sub to the entry for its file, starting one if needed, and
// records where sub starts: at the end of the file as of the join. A new
// entry's reader starts there; a subscriber joining a running entry skips the
// published lines that predate its join (see entry.join). A closed entry,
// whose last subscriber was just evicted, is replaced by a new one.
func (h *Hub) join(sub *subscriber) *entry {
	key := sessionKey(sub.session)
	measure := func() position { return h.endOfFile(sub.session) }

	for {
		h.mu.Lock()
		e := h.entries[key]
		if e == nil || e.isClosed() {
			// newEntry can panic if its reader cannot be constructed. Do not
			// leave the entire hub locked when the caller recovers that panic.
			defer h.mu.Unlock()
			sub.joinedAt = measure()
			e = newEntry(key, sub.session, sub.joinedAt, nil, h.options, h.logger, h.seams, h.forget)
			h.entries[key] = e
			// A new entry is open.
			_, _ = e.add(sub)
			e.start()
			return e
		}
		// Joining waits for a publication in progress, which may take the
		// hub's lock to drop an entry it closed, so the hub's lock is not
		// held meanwhile.
		h.mu.Unlock()
		if e.join(sub, measure) {
			return e
		}
	}
}

// sessionKey returns the key of the entry for session's file.
func sessionKey(session Session) entryKey {
	return entryKey{
		path:        session.Target.ResolvedPath(),
		compression: fs.CompressionFormat(session.FilePath),
	}
}

// endOfFile returns the end of session's file, or an unknown position, which
// makes the session take every line published from now on.
func (h *Hub) endOfFile(session Session) position {
	end, err := endOfFile(session.Target)
	if err != nil {
		h.logger.Warn(session.FilePath, "Unable to measure file for a shared follow read", err)
	}
	return end
}

// leave removes sub from e. The last subscriber stops e's reader; otherwise
// a reader using sub's target is handed another subscriber's target.
func (h *Hub) leave(e *entry, sub *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if remaining := e.remove(sub); remaining == 0 {
		if h.entries[e.key] == e {
			delete(h.entries, e.key)
		}
		e.stop()
	}
}

// forget drops e from the hub when its reader failed for good, so that the
// next session starts a fresh reader instead of joining a dead one.
func (h *Hub) forget(e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.entries[e.key] == e {
		delete(h.entries, e.key)
	}
}

// Subscribers returns how many sessions currently follow the file at the
// validated resolved path through a shared reader, across compression formats.
func (h *Hub) Subscribers(resolvedPath string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	count := 0
	for key, e := range h.entries {
		if key.path == resolvedPath {
			count += len(e.snapshot())
		}
	}
	return count
}

// entryFor returns the live entry for path, for tests.
func (h *Hub) entryFor(path string) *entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, e := range h.entries {
		if key.path == path {
			return e
		}
	}
	return nil
}
