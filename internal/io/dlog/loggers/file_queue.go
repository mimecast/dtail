package loggers

import "sync"

const fileQueueChunks = 4

type fileChunk struct {
	data []byte
	end  bool // This chunk ends at a Log/Raw call boundary.
}

// fileQueue owns every enqueued byte. One producer serializes each complete
// Log/Raw call, including calls larger than the queue; the worker never takes
// producerMu. Slow sinks block producers once the bounded queue fills.
// At most limit queued chunks, one producer chunk and one worker chunk exist.
// Free chunks are reused locally, not retained in an unbounded/global pool.
type fileQueue struct {
	producerMu sync.Mutex
	mu         sync.Mutex
	changed    *sync.Cond
	ready      chan struct{}
	chunks     []fileChunk
	head       int
	size       int
	pending    []byte
	free       [][]byte
	active     bool
	closed     bool
}

func newFileQueue(limit int) *fileQueue {
	q := &fileQueue{ready: make(chan struct{}, 1), chunks: make([]fileChunk, limit)}
	q.changed = sync.NewCond(&q.mu)
	return q
}

// append copies strings and borrowed byte slices directly into owned chunks.
// The diagnostic newline belongs to the same serialized call as its message.
// false means shutdown already closed admission; accepted calls finish in full.
func (q *fileQueue) append(message string, raw []byte, newline bool) bool {
	q.producerMu.Lock()
	defer q.producerMu.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false
	}
	q.active = true
	defer func() { q.active = false; q.changed.Broadcast() }()
	total := len(message) + len(raw)
	if newline {
		total++
	}
	// Keep ordinary calls whole. Large calls get an exclusive run of chunks,
	// with an explicit end marker so rotation cannot split one message.
	if len(q.pending) > 0 && total > cap(q.pending)-len(q.pending) {
		q.publish(true)
	}
	large := total > fileWriterBufSize
	for len(message)+len(raw) > 0 || newline {
		if q.pending == nil {
			q.pending = q.getBuffer()
			// A partial first chunk must arm the worker's idle flush too.
			signal(q.ready)
		}
		start := len(q.pending)
		space := q.pending[start:cap(q.pending)]
		n := copy(space, message)
		message = message[n:]
		m := copy(space[n:], raw)
		raw = raw[m:]
		n += m
		if len(message)+len(raw) == 0 && newline && n < len(space) {
			space[n] = '\n'
			n++
			newline = false
		}
		q.pending = q.pending[:start+n]
		if len(q.pending) == cap(q.pending) {
			q.publish(len(message)+len(raw) == 0 && !newline)
		}
	}
	if large && len(q.pending) > 0 {
		q.publish(true)
	}
	return true
}

// publish requires mu; waits release it, but retain producerMu. take never
// steals pending while a producer is active, including during this wait.
func (q *fileQueue) publish(end bool) {
	for q.size == len(q.chunks) {
		q.changed.Wait()
	}
	q.chunks[(q.head+q.size)%len(q.chunks)] = fileChunk{q.pending, end}
	q.size++
	q.pending = nil
	q.changed.Broadcast()
	signal(q.ready)
}

// take transfers ownership to the single worker. Normal notifications consume
// full chunks only; timer/explicit/shutdown flushes also consume partial chunks.
func (q *fileQueue) take(partial bool) fileChunk {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.size > 0 {
		data := q.chunks[q.head]
		q.chunks[q.head] = fileChunk{}
		q.head = (q.head + 1) % len(q.chunks)
		q.size--
		q.changed.Broadcast()
		if q.size > 0 {
			signal(q.ready)
		}
		return data
	}
	if partial && !q.active {
		data := q.pending
		q.pending = nil
		return fileChunk{data, true}
	}
	return fileChunk{}
}

func (q *fileQueue) release(data []byte) {
	q.mu.Lock()
	q.free = append(q.free, data[:0])
	q.mu.Unlock()
}

// getBuffer requires mu. The worker returns a buffer before taking another,
// so growth stops at queue capacity + producer + worker, even under backpressure.
func (q *fileQueue) getBuffer() []byte {
	if n := len(q.free); n > 0 {
		data := q.free[n-1]
		q.free[n-1] = nil
		q.free = q.free[:n-1]
		return data
	}
	return make([]byte, 0, fileWriterBufSize)
}

// close stops new admissions, without cancelling an accepted large call that
// may currently be blocked by backpressure. The worker keeps draining it.
func (q *fileQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
}

// drained waits for an accepted producer to publish more bytes or finish.
// It is used only after close, and only by the worker after a drain attempt.
func (q *fileQueue) drained() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.active && q.size == 0 {
		q.changed.Wait()
	}
	return !q.active && q.size == 0 && len(q.pending) == 0
}
