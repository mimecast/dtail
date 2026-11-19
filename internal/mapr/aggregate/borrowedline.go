package aggregate

import (
	"bytes"
	"sync"
	"unsafe"
)

const (
	scratchFieldsCapacity = 24
	scratchKeyCapacity    = 128
	// maxRetainedScratchKeyBytes and maxRetainedScratchFields bound what a
	// single line scratch keeps parked between batches. A single pathological
	// line -- a huge group key, or a query grouping by very many fields --
	// would otherwise inflate that scratch for as long as the pool holds it,
	// because neither a slice nor a map ever shrinks on its own. Both limits
	// are far above anything ordinary log lines reach, so the common case
	// still reuses its storage without allocating. Same policy as
	// maxRetainedLineBufBytes in internal/clients/handlers/basehandler.go.
	maxRetainedScratchKeyBytes = 64 * 1024
	maxRetainedScratchFields   = 1024
	// maxRetainedBatchScratchKeyBytes and maxRetainedBatchScratchFields bound
	// the idle storage summed over all line scratches of one pooled batch
	// scratch. A batch scratch holds up to processorBatchSize line scratches,
	// so the per-line limits alone would let it park 100 times as much storage
	// that nothing uses any more. Only idle headroom is charged: storage beyond
	// both the default size of a line scratch (scratchKeyCapacity,
	// scratchFieldsCapacity) and the watermark, the largest group key and the
	// largest field count of any line of the last retentionHistory batches
	// processed with this batch scratch, the current one included.
	//
	// The watermark is the reference for every scratch, not each scratch's own
	// line: which line lands in which scratch is arbitrary, so on a workload
	// whose line sizes vary, a scratch's own last line is a random draw that
	// says nothing about the next one, while the maximum over many lines stays
	// near the workload's maximum. It spans several batches, not only the
	// current one, because single smaller batches are ordinary: a follow-mode
	// Flush drains a partial batch of maybe one short line, and a workload may
	// interleave batches of short and long lines. Measured against the current
	// batch alone, each of those found the long lines' storage idle and shrank
	// it, and the next batch of long lines grew it back.
	//
	// Storage up to the watermark is never charged or shrunk. When the idle
	// total exceeds a budget, the scratches with the largest idle headroom are
	// shrunk to the watermark (at least the default size) until it fits. After
	// an outlier batch, the outlier storage stays until the outlier leaves the
	// history: the next retentionHistory-1 ordinary batches reuse it without
	// allocating, the retentionHistory-th trims the idle storage to the budget
	// (allocating for the scratches it shrinks), and later batches whose lines
	// are no larger than those of the recent history reuse all scratches
	// without allocating.
	//
	// Tradeoffs: a workload whose line sizes shift permanently downward keeps
	// the storage of its former large lines for up to retentionHistory
	// batches. A workload with at least one line of size L in any
	// retentionHistory consecutive batches lets every scratch keep up to L, so
	// the storage of a pooled batch scratch is then bounded only by the
	// per-line limits (processorBatchSize times maxRetainedScratchKeyBytes and
	// maxRetainedScratchFields), as it is right after a batch of maximal lines
	// anyway. And outliers rarer than that are shrunk and regrown: once more
	// than four line scratches of idle headroom at the per-line key limit, or
	// eight at the per-line fields limit, have gone unused for retentionHistory
	// batches, they are trimmed, and the next outlier batch grows them again.
	maxRetainedBatchScratchKeyBytes = 256 * 1024
	maxRetainedBatchScratchFields   = 8 * 1024
	// retentionHistory is the number of recent batches whose largest line sets
	// the watermark of a batch scratch, see maxRetainedBatchScratchKeyBytes.
	retentionHistory = 8
)

// lineScratch is the reusable working set of parseLine: the field map the line
// is parsed into and the buffer the group key is built in. Reusing them keeps
// the per-line path free of map, string and group-key allocations. Everything
// a scratch holds borrows the line buffer being processed, so a scratch is
// only valid while its own batch is being processed.
type lineScratch struct {
	// fields holds the parsed fields of the line. A logformat.FieldsIntoParser
	// fills it directly; the map any other parser returns is copied into it,
	// because the merge phase reads it only after the whole batch was parsed
	// and a parser is free to reuse its own map on the next call.
	fields map[string]string
	key    []byte
	// maxFields is the high-water mark of len(fields) since fields was
	// allocated, i.e. how many entries' worth of buckets the map retains: a
	// map keeps its buckets after clear(), and the length at recycle time is
	// only the last line's, so the peak has to be recorded while the scratch
	// is in use for the retention limits to spot an inflated map.
	maxFields int
}

// batchScratch holds one lineScratch per line of a batch. processRawBatch
// parses every line of the batch into its own scratch first, without any lock,
// and only then takes the serializer's group lock once to merge the whole
// batch. The scratches of a batch therefore have to coexist, which is why one
// per line is needed instead of a single reused one.
type batchScratch struct {
	lines []*lineScratch
	// used is the number of line scratches handed out for the current batch.
	// It is tracked here rather than taken from the batch length so that
	// recycling after a panic part way through a batch clears exactly the
	// scratches that exist and were used, and the original panic survives.
	used int
	// accepted lists the scratches whose line passed the where clause and is
	// waiting to be merged into the serializer.
	accepted []*lineScratch
	// recentFields and recentKeyBytes are ring buffers of the largest field
	// count and group-key length of any line of each of the last
	// retentionHistory batches, zero for a batch that used no scratch or for
	// no batch yet; recentNext is the slot the next batch overwrites. They are
	// updated when the batch scratch is cleared.
	recentFields   [retentionHistory]int
	recentKeyBytes [retentionHistory]int
	recentNext     int
	// keepFields and keepKeyBytes are the watermark: the maxima over the
	// ring buffers, at least the default size of a line scratch. The
	// retention budget never charges or shrinks the storage of any line
	// scratch up to them, see maxRetainedBatchScratchKeyBytes.
	keepFields   int
	keepKeyBytes int
}

// batchScratchPool hands out one batch scratch per batch. The scratch cannot
// live on the Aggregate: several file processors call processRawBatch
// concurrently, and a shared fields map would let one file's line overwrite
// another's. It does not live on the Processor either, so that many idle
// follow-mode processors do not each pin a full batch worth of maps.
var batchScratchPool = sync.Pool{
	New: func() any { return &batchScratch{} },
}

func newLineScratch() *lineScratch {
	return &lineScratch{
		fields: make(map[string]string, scratchFieldsCapacity),
		key:    make([]byte, 0, scratchKeyCapacity),
	}
}

// line returns the scratch for the i-th line of the batch, growing the batch
// scratch on first use. Lines are handed out in order, so i is at most
// len(b.lines).
func (b *batchScratch) line(i int) *lineScratch {
	if i == len(b.lines) {
		b.lines = append(b.lines, newLineScratch())
	}
	if i >= b.used {
		b.used = i + 1
	}
	return b.lines[i]
}

// clear records the largest line of the current batch in the history, drops
// the borrowed views of the used line scratches, see clearLineScratch,
// enforces the batch retention budget and forgets the accepted lines.
func (b *batchScratch) clear() {
	var maxFields, maxKeyBytes int
	for _, scratch := range b.lines[:b.used] {
		maxFields = max(maxFields, len(scratch.fields))
		maxKeyBytes = max(maxKeyBytes, len(scratch.key))
		clearLineScratch(scratch)
	}
	b.used = 0
	b.recordBatch(maxFields, maxKeyBytes)
	b.enforceRetentionBudget()
	clear(b.accepted)
	b.accepted = b.accepted[:0]
}

// recordBatch stores the largest line of the batch just processed in the
// history, replacing the oldest entry, and recomputes the watermark over the
// last retentionHistory batches. It costs a walk over two fixed-size arrays
// per batch and never allocates.
func (b *batchScratch) recordBatch(maxFields, maxKeyBytes int) {
	b.recentFields[b.recentNext] = maxFields
	b.recentKeyBytes[b.recentNext] = maxKeyBytes
	b.recentNext = (b.recentNext + 1) % retentionHistory
	b.keepFields, b.keepKeyBytes = scratchFieldsCapacity, scratchKeyCapacity
	for i := range retentionHistory {
		b.keepFields = max(b.keepFields, b.recentFields[i])
		b.keepKeyBytes = max(b.keepKeyBytes, b.recentKeyBytes[i])
	}
}

// enforceRetentionBudget caps the idle headroom that all line scratches of the
// batch scratch retain together at maxRetainedBatchScratchFields and
// maxRetainedBatchScratchKeyBytes, see there. It runs once per batch; the
// common case, a batch within budget, is a single walk over at most
// processorBatchSize scratches without any change.
func (b *batchScratch) enforceRetentionBudget() {
	var fields, keyBytes int
	for _, scratch := range b.lines {
		fields += b.idleFields(scratch)
		keyBytes += b.idleKeyBytes(scratch)
	}
	// Shrink the scratch with the largest idle headroom first, so that as few
	// scratches as possible lose storage. A shrunk scratch keeps room for the
	// watermark, so its idle headroom drops to zero and a next line of up to
	// that size fits without allocating. Each round removes one scratch's
	// headroom, so the loops end after at most len(b.lines) rounds, and only a
	// batch over budget runs them at all. clearLineScratch has already
	// enforced the per-line limits, so a scratch is only ever shrunk to less
	// than those limits.
	for fields > maxRetainedBatchScratchFields {
		scratch := b.largest(b.idleFields)
		fields -= b.idleFields(scratch)
		scratch.fields = make(map[string]string, b.keepFields)
		// The fresh map is sized for keepFields entries, so that is the
		// storage it retains from now on.
		scratch.maxFields = b.keepFields
	}
	for keyBytes > maxRetainedBatchScratchKeyBytes {
		scratch := b.largest(b.idleKeyBytes)
		keyBytes -= b.idleKeyBytes(scratch)
		scratch.key = make([]byte, 0, b.keepKeyBytes)
	}
}

// largest returns the line scratch with the largest idle headroom. The caller
// only asks while the summed headroom is positive, so the result has some.
func (b *batchScratch) largest(idle func(*lineScratch) int) *lineScratch {
	var largest *lineScratch
	largestIdle := 0
	for _, scratch := range b.lines {
		if n := idle(scratch); n > largestIdle {
			largest, largestIdle = scratch, n
		}
	}
	return largest
}

// idleFields is how many entries' worth of map buckets scratch retains beyond
// the watermark keepFields.
func (b *batchScratch) idleFields(scratch *lineScratch) int {
	return max(scratch.maxFields-b.keepFields, 0)
}

// idleKeyBytes is how many bytes of group-key buffer scratch retains beyond
// the watermark keepKeyBytes.
func (b *batchScratch) idleKeyBytes(scratch *lineScratch) int {
	return max(cap(scratch.key)-b.keepKeyBytes, 0)
}

// recycleBatchScratch clears the used line scratches and parks the batch
// scratch in the pool. The scratch must not be touched afterwards.
func recycleBatchScratch(scratch *batchScratch) {
	scratch.clear()
	batchScratchPool.Put(scratch)
}

// clearLineScratch drops the borrowed views a scratch holds and releases
// storage that one outlier line inflated beyond the per-line retention limits,
// so the scratch is safe and reasonably sized to reuse. It runs before the
// owning batch scratch goes back to the pool, so a pooled scratch can never
// hand a stale view of an already recycled line buffer to the next batch.
//
// Known tradeoff: the per-line limits apply to each of the up to
// processorBatchSize line scratches of a batch scratch, so a steady workload
// whose group keys exceed maxRetainedScratchKeyBytes, or whose lines exceed
// maxRetainedScratchFields fields, releases and regrows every scratch in every
// batch: about two allocations per line for such keys (the reset here and the
// regrow on the next line). Measured with the default parser, group by a
// 70,000 byte key, 100 identical lines per batch: 204 allocations per batch,
// against 3 at d5cae8f, before the per-line scratches, whose single scratch
// regrew once per batch; 60 KiB and 64 KiB keys make 0 (d5cae8f: 1). With a
// parser that is not a FieldsIntoParser, 1101 fields per line make 1912
// allocations per batch and 1001 fields make 0. Such lines are far outside
// ordinary logs, and raising the limits would let every pooled batch scratch
// park up to 100 times as much, so the limits stay as they are.
func clearLineScratch(scratch *lineScratch) {
	if scratch.maxFields > maxRetainedScratchFields {
		// clear() keeps the buckets a huge line grew, so the map itself has
		// to go; the next line refills a right-sized one.
		scratch.fields = make(map[string]string, scratchFieldsCapacity)
		scratch.maxFields = 0
	} else {
		clear(scratch.fields)
	}

	if cap(scratch.key) > maxRetainedScratchKeyBytes {
		scratch.key = make([]byte, 0, scratchKeyCapacity)
	} else {
		scratch.key = scratch.key[:0]
	}
}

// borrowedLine returns the trimmed content of buf as a string sharing buf's
// memory instead of copying it, which is worth one string allocation per
// processed line. The result is only valid until buf is written to again or
// recycled into the buffer pool, which the caller does as soon as the line's
// batch has been processed; see parseLine for the copy-on-retention rules that
// make that safe.
func borrowedLine(buf *bytes.Buffer) string {
	if buf == nil {
		// bytes.Buffer.String renders a nil receiver as "<nil>". Keep that
		// legacy rendering — the parsers reject it like any other
		// non-mapreduce line — instead of dereferencing nil in Bytes.
		return buf.String()
	}
	return borrowedString(bytes.TrimSpace(buf.Bytes()))
}

// borrowedString reinterprets b as a string without copying it. The caller
// must guarantee that b is neither modified nor recycled while the returned
// string is still reachable.
func borrowedString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}
