package readhub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
)

// entry is the shared follow read of one file.
type entry struct {
	key     entryKey
	path    string
	options Options
	logger  logging.Logger
	// onFailure lets the hub drop an entry whose reader failed for good, or
	// that lost its last subscriber to an eviction.
	onFailure func(*entry)
	failOnce  sync.Once
	stopOnce  sync.Once
	// seams holds the operations tests replace; see hubSeams.
	seams hubSeams

	reader *fs.ReadFile
	// messages receives the reader's long line warning; the fan-out
	// processor publishes it in order with the lines (see
	// fanoutProcessor.publishWarning).
	messages chan string
	cancel   context.CancelFunc
	ctx      context.Context
	// done is closed once the reader returned and the file is closed.
	done chan struct{}

	// mu guards the subscriber list and the owner; the hub's mutex is always
	// taken first when both are needed.
	mu          sync.Mutex
	subscribers []*subscriber
	// owner is the subscriber whose target the reader opens the file with.
	owner *subscriber
	// closed is set once the last subscriber left or was evicted: the entry
	// takes no new subscriber, a joining session starts a new entry.
	closed bool

	// publishMu keeps every subscriber's queue in the order items were
	// published, by the reader or by fail, and lets a session join between
	// two publications (see join).
	publishMu sync.Mutex
	// readPos is the file the reader has open and how far it has read it,
	// or an unknown position from the announcement of a new read until the
	// reader opened the file. Guarded by publishMu.
	readPos position
	// reading is set by the reader right before every read and cleared,
	// under publishMu, once readUpTo recorded how far it got: while it is
	// set, the reader may have read past readPos. It is atomic so that the
	// reader does not take publishMu before a read as well as after it.
	reading atomic.Bool
	// openedFile is the file the reader opened last, for which every
	// subscriber holds a descriptor (see heldFile), or nil from the
	// announcement of a new read until the reader opened the file. Guarded by
	// publishMu.
	openedFile os.FileInfo
	// published is where the read the subscribers were fed got to: the end
	// of the last line published, or where the reader started in the file
	// it opened before it published a line, or an unknown position from the
	// announcement of a new read until the reader opened the file. Every
	// line of that file ending past it is still to be published. An evicted
	// session rejoins from there on (see rejoin). Guarded by publishMu.
	published position
}

var (
	_ publisher     = (*entry)(nil)
	_ readTracker   = (*entry)(nil)
	_ warningSource = (*entry)(nil)
)

// newEntry makes the shared follow read of creator's file, which starts at
// start: the end of the file when creator joined, or where creator's private
// read got to when it rejoins. startFile, if not nil, is a descriptor of
// start's file, which the reader then reads first (see
// fs.ReadOptions.StartFile) and closes.
func newEntry(key entryKey, creator Session, start position, startFile *os.File, options Options,
	logger logging.Logger, seams hubSeams, onFailure func(*entry)) *entry {

	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{
		published: unknownPosition(),
		key:       key,
		path:      creator.FilePath,
		options:   options,
		logger:    logger,
		onFailure: onFailure,
		seams:     seams,
		messages:  make(chan string, 1),
		cancel:    cancel,
		ctx:       ctx,
		done:      make(chan struct{}),
		// The reader opens the file at start, unless it was rotated since.
		readPos: start,
	}
	target := creator.Target
	readOptions := fs.ReadOptions{
		Mode:           omode.TailClient,
		Target:         &target,
		FilePath:       creator.FilePath,
		GlobID:         creator.GlobID,
		ServerMessages: e.messages,
		MaxLineLength:  options.MaxLineLength,
		Logger:         logger,
	}
	start.startAt(&readOptions)
	readOptions.StartFile = startFile
	reader, err := fs.NewReadFile(readOptions)
	if err != nil {
		// NewReadFile only fails for an unsupported mode or target kind, or a
		// start position of a compressed file, and all of them are fixed or
		// validated before an entry is made.
		panic(fmt.Sprintf("readhub: shared follow reader: %v", err))
	}
	e.reader = reader
	return e
}

// start runs the reader.
func (e *entry) start() {
	e.logger.Info(e.path, "Shared follow read started", "subscribers=1")
	go e.run()
}

// stop ends the read once the last subscriber left or was evicted.
func (e *entry) stop() {
	e.stopOnce.Do(func() {
		e.logger.Info(e.path, "Shared follow read stopped", "subscribers=0")
		e.cancel()
	})
}

// add registers sub, unless the entry is closed, and returns how many
// subscribers the entry has now; the first subscriber owns the reader's
// target.
func (e *entry) add(sub *subscriber) (count int, added bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return len(e.subscribers), false
	}
	e.subscribers = append(e.subscribers, sub)
	if e.owner == nil {
		e.owner = sub
	}
	return len(e.subscribers), true
}

// markClosed makes the entry take no new subscriber: its reader failed.
func (e *entry) markClosed() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
}

// join adds sub to the running entry and records where its read starts: at
// the end of the file measured by measure, skipping the published lines that
// predate the join. Nothing is published between the measurement and the
// addition, so sub gets every item published after the measurement, and the
// reader's position tells which lines were read before it. Once sub is
// listed, its start is recorded. join returns false, and adds nothing, when
// the entry is closed.
func (e *entry) join(sub *subscriber, measure func() position) bool {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	sub.joinedAt = measure()
	sub.skip = newJoinSkip(sub.joinedAt, e.readPos)
	count, added := e.add(sub)
	if !added {
		return false
	}
	if count > 1 {
		e.logger.Info(e.path, "Shared follow read gained a subscriber", fmt.Sprintf("subscribers=%d", count))
	}
	sub.held.set(openSame(e.seams.openFile, sub.session.Target, e.openedFile))
	return true
}

// isClosed reports whether the entry takes no new subscriber.
func (e *entry) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// warnings returns the channel the reader sends its long line warning to.
func (e *entry) warnings() <-chan string {
	return e.messages
}

// readStarting records that the reader is about to read (see reading).
func (e *entry) readStarting() {
	e.reading.Store(true)
}

// readUpTo records how far the reader has read which file. When the reader
// opened a file, every subscriber opens a descriptor of it through its own
// target (see heldFile). A subscriber whose path was rotated away from the
// file in the moment since then holds none.
func (e *entry) readUpTo(p position) {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	e.readPos = p
	e.reading.Store(false)
	if p.file == e.openedFile {
		return
	}
	e.openedFile = p.file
	// The reader opened a file and starts reading it here: nothing of it is
	// published yet.
	e.published = p
	for _, sub := range e.snapshot() {
		sub.held.set(openSame(e.seams.openFile, sub.session.Target, p.file))
	}
}

// remove unregisters sub, stops deliveries to it and returns how many
// subscribers remain. When sub owned the reader's target, the reader is
// handed the target of the longest-subscribed remaining session.
func (e *entry) remove(sub *subscriber) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	remaining, found := e.detach(sub)
	close(sub.done)

	if found && remaining > 0 {
		e.logger.Info(e.path, "Shared follow read lost a subscriber", fmt.Sprintf("subscribers=%d", remaining))
	}
	if e.owner == sub {
		e.owner = nil
		if remaining > 0 {
			e.handOver(e.subscribers[0])
		}
	}
	return remaining
}

// handOver makes the reader use next's target. The caller holds e.mu.
func (e *entry) handOver(next *subscriber) {
	if err := e.seams.replaceTarget(e.reader, next.session.Target); err != nil {
		// Every subscriber's target resolves to the entry's path, so this
		// cannot fail; stop rather than keep reading through a departed
		// session's target. fail takes the hub's and the entry's locks,
		// which the caller holds.
		e.cancel()
		go func() {
			<-e.done
			e.fail(fmt.Errorf("hand over the shared read target: %w", err))
		}()
		return
	}
	e.owner = next
}

// fail ends the shared read for good: the hub forgets the entry, so the next
// session starts a new reader, and every subscriber's Follow returns cause
// wrapped in ErrReaderFailed. A cause that wraps fs.ErrReaderWorkerPanic, as
// a panic does, keeps doing so, so callers handle a shared reader's panic like
// a private reader's.
func (e *entry) fail(cause error, logArgs ...any) {
	e.failOnce.Do(func() {
		err := fmt.Errorf("%w: %w", ErrReaderFailed, cause)
		e.logger.Error(append([]any{e.path, err}, logArgs...)...)
		e.onFailure(e)
		e.cancel()
		e.publish(item{kind: failedItem, err: err})
	})
}

// recoverPanic turns a panic on one of the entry's goroutines into a failed
// shared read: the private reader runs on the session's goroutine, where a
// panic ends that session only, so a shared one must not crash dserver.
func (e *entry) recoverPanic(where string) {
	if recovered := recover(); recovered != nil {
		e.fail(fmt.Errorf("%w: shared %s: %v", fs.ErrReaderWorkerPanic, where, recovered),
			"stack", string(debug.Stack()))
	}
}

// run is the shared equivalent of the read command's retry loop for one
// file: it starts the reader again after a read ended, e.g. after the file
// was rotated, and announces each new read to the subscribers, who then make
// a new processor as a private read would.
func (e *entry) run() {
	defer close(e.done)
	defer e.recoverPanic("reader")
	fanout := newFanoutProcessor(e)

	for iteration := 0; ; iteration++ {
		if iteration > 0 {
			e.publish(item{kind: reopenItem})
		}
		err := e.seams.startReader(e.ctx, e.reader, fanout)
		fanout.publishWarning()
		fanout.publishPending()
		if errors.Is(err, fs.ErrReaderWorkerPanic) {
			e.fail(err)
			return
		}
		if errors.Is(err, fs.ErrStartOffsetFileChanged) && e.ctx.Err() == nil {
			// Rotated between the creator's join and the first open: read
			// the new file from its beginning with new processors, like a
			// private read after a rotation, without waiting.
			e.logger.Info(e.path, "File was rotated before the shared read opened it, reading the new file")
			continue
		}
		if err != nil {
			e.logger.Error(e.path, err)
			// The read command reports a failed read of a private reader to
			// its client before it reads the file again; every session of
			// this reader gets the same report (see sessionRead.handle).
			e.publish(item{kind: readErrorItem, err: err})
		}
		if e.ctx.Err() != nil || !ctxutil.Sleep(e.ctx, e.options.RetryInterval) {
			return
		}
		e.logger.Info(e.path, "Reading file again")
	}
}

// publish delivers it to every current subscriber, in publication order. A
// subscriber that joins later does not get it. publish never waits for a
// subscriber: one whose queue is full is evicted and reads privately.
func (e *entry) publish(it item) {
	e.publishMu.Lock()
	defer e.publishMu.Unlock()
	switch it.kind {
	case chunkItem:
		e.published = it.chunk.lineEnd(len(it.chunk.ends) - 1)
	case reopenItem:
		e.readPos = unknownPosition()
		e.published = unknownPosition()
		// A descriptor of the file read so far is of no use to a session
		// that goes on with the new read.
		e.openedFile = nil
		for _, sub := range e.snapshot() {
			sub.held.set(nil)
		}
	case restartItem:
		e.readPos.offset = 0
		e.published = position{offset: 0, file: e.readPos.file}
	case failedItem:
		e.published = unknownPosition()
		e.markClosed()
	}
	for _, sub := range e.snapshot() {
		if !sub.offer(it) {
			e.evict(sub, it)
		}
	}
}

func (e *entry) snapshot() []*subscriber {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*subscriber(nil), e.subscribers...)
}

// ownerSession returns the session whose target the reader uses, for tests.
func (e *entry) ownerSession() (Session, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.owner == nil {
		return Session{}, false
	}
	return e.owner.session, true
}
