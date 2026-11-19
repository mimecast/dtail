package readhub

import (
	"os"
	"sync"

	"github.com/mimecast/dtail/internal/io/fs"
)

// Every subscriber holds a descriptor of its own of the file the shared
// reader has open, so that when it is evicted, or the shared reader fails, its
// private reader goes on in that file even if the path was rotated away from
// it meanwhile: the shared reader may still be behind in the old file then,
// with the evicted session's last line in it. Each descriptor is a separate
// open of the file, never a dup, so that private readers do not share a file
// offset, and it is opened through the subscriber's own validated target and
// kept only if it is the file the reader has open, so a session only ever gets
// a descriptor of the file it legitimately reads. It costs every session one
// descriptor, as a private reader would.

// heldFile is a subscriber's descriptor of the file the shared reader has
// open, or nil. It is replaced whenever the reader opens a file, dropped when
// the reader announces a new read, and taken over by the session's private
// reader, or closed when the session's read ends.
type heldFile struct {
	mu   sync.Mutex
	file *os.File
	// ended is set once the descriptor was taken or closed for good; a
	// descriptor set afterwards is closed at once.
	ended bool
}

// set replaces the held descriptor with fd, which may be nil, and closes the
// one it replaces. After take or close, it closes fd instead.
func (h *heldFile) set(fd *os.File) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ended {
		closeHeld(fd)
		return
	}
	closeHeld(h.file)
	h.file = fd
}

// take hands the held descriptor, if any, to the caller, which closes it, and
// makes set close any later one.
func (h *heldFile) take() *os.File {
	h.mu.Lock()
	defer h.mu.Unlock()
	fd := h.file
	h.file = nil
	h.ended = true
	return fd
}

// resume makes fd, which may be nil, the held descriptor again after it was
// taken, when the session rejoined a shared reader, and closes one still
// held.
func (h *heldFile) resume(fd *os.File) {
	h.mu.Lock()
	defer h.mu.Unlock()
	closeHeld(h.file)
	h.file = fd
	h.ended = false
}

// close closes the held descriptor unless it was taken.
func (h *heldFile) close() {
	closeHeld(h.take())
}

func closeHeld(held *os.File) {
	if held != nil {
		_ = held.Close()
	}
}

// openSame opens the file at target's path through open, the target's own
// Open unless a test replaces it, and returns it if it is the file info
// describes, or nil otherwise, e.g. when the path was rotated since.
func openSame(open func(fs.ValidatedReadTarget) (*os.File, error), target fs.ValidatedReadTarget,
	info os.FileInfo) *os.File {

	if info == nil {
		return nil
	}
	fd, err := open(target)
	if err != nil {
		return nil
	}
	if !sameFile(fd, info) {
		closeHeld(fd)
		return nil
	}
	return fd
}

// sameFile reports whether fd is the file info describes.
func sameFile(fd *os.File, info os.FileInfo) bool {
	current, err := fd.Stat()
	return err == nil && info != nil && os.SameFile(current, info)
}
