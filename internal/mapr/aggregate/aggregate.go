package aggregate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mimecast/dtail/internal"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
)

// Aggregate is a high-performance aggregator for MapReduce operations.
// It processes lines directly without channels for maximum throughput.
type Aggregate struct {
	logger logging.Logger
	done   *internal.Done
	// inputFinished is signaled via FinishInput once all one-shot input file
	// reads (cat/grep style) feeding this aggregate have drained. Start then
	// emits a final serialization and returns instead of blocking until
	// session teardown. Follow-mode (tail) inputs never signal it, so
	// interval-based streaming aggregation keeps running.
	inputFinished *internal.Done
	// The mapr query
	query *mapr.Query
	// The mapr log format parser
	parser logformat.Parser
	// parserFillsFields reports whether parser is a logformat.FieldsIntoParser
	// and thus parses straight into a line scratch's own map. The map any
	// other parser returns is copied, see parseLine.
	parserFillsFields bool
	serializer        *serializer
	// Stats
	// linesProcessed counts the lines handed to the aggregation; lines an
	// abort discards before they were parsed are not included.
	linesProcessed atomic.Uint64
	errors         atomic.Uint64
	filesProcessed atomic.Uint64
	// Synchronization for clean shutdown.
	processorMu sync.Mutex
	// processorsSealed prevents completion waiting from racing a late processor
	// registration. Shutdown/Abort seals registration before waiting or
	// returning; readers that reach processor creation after cancellation receive
	// an inert processor.
	processorsSealed bool
	processorCount   int
	// processorSeq numbers the processors so that each gets a source key of
	// its own, and each in-place truncation a new one, see Processor.sourceKey.
	processorSeq   atomic.Uint64
	processorsDone chan struct{}
	// Track active file processors
	activeProcessors atomic.Int32
	startOnce        sync.Once
	started          chan struct{}
	shutdownStarted  bool
	shutdownDone     chan struct{}
	terminalMu       sync.Mutex
	terminalState    aggregateTerminalState
	// finalizationCtx is fixed when finalization wins the terminal-state
	// transition. Start may subsequently be the goroutine that executes
	// finalization, but it must use the graceful caller's output context.
	finalizationCtx       context.Context
	finalizationOwnerHook func()
	serializationLoopHook func()
	serializationTickHook func()
}

type aggregateTerminalState uint8

const (
	aggregateRunning aggregateTerminalState = iota
	aggregateFinalizing
	aggregateAborted
)

// New returns an aggregator using dependencies resolved by the command layer.
func New(query *mapr.Query, parser logformat.Parser, hostname string,
	logger logging.Logger) (*Aggregate, error) {
	logger = logging.OrNop(logger)
	if query == nil {
		return nil, fmt.Errorf("create aggregate: query must not be nil")
	}
	if nilParser(parser) {
		return nil, fmt.Errorf("create aggregate: log format parser must not be nil")
	}
	a := &Aggregate{
		logger:        logger,
		done:          internal.NewDone(),
		inputFinished: internal.NewDone(),
		query:         query,
		parser:        parser,
		started:       make(chan struct{}),
		shutdownDone:  make(chan struct{}),
	}
	_, a.parserFillsFields = parser.(logformat.FieldsIntoParser)
	a.serializer = newSerializer(query, logger)
	logger.Debug("Created MapReduce aggregate", "hostname", hostname)
	return a, nil
}

func nilParser(parser logformat.Parser) bool {
	if parser == nil {
		return true
	}
	value := reflect.ValueOf(parser)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	case reflect.UnsafePointer:
		return value.IsZero()
	default:
		return false
	}
}

func (a *Aggregate) stopping() bool {
	select {
	case <-a.done.Done():
		return true
	default:
		return false
	}
}

func (a *Aggregate) stopSerializeTicker() {
	a.serializer.stopTicker()
}

// countGroups returns the current number of groups in the aggregation.
func (a *Aggregate) countGroups() int {
	return a.serializer.countGroups()
}

// Shutdown finalizes the aggregation while output remains writable.
// Canceling ctx abandons blocked final sends promptly; the unsent snapshot is
// re-merged before shutdown completes so cancellation never corrupts state.
func (a *Aggregate) Shutdown(ctx context.Context) {
	a.shutdown(ctx, false)
}

// shutdown finalizes the aggregate and waits for either completion or the
// caller's context. The Start goroutine retains ownership of its output channel
// until finalization has completely stopped, even when its command context has
// already been canceled.
func (a *Aggregate) shutdown(ctx context.Context, retainOutput bool) {
	finalizationCtx, owner, ok := a.claimFinalization(ctx, true)
	if !ok {
		return
	}

	if owner {
		if a.finalizationOwnerHook != nil {
			a.finalizationOwnerHook()
		}
		func() {
			defer close(a.shutdownDone)
			processorsDone := a.sealProcessors()
			a.done.Shutdown()
			a.stopSerializeTicker()
			finalizeCtx, cancel := context.WithTimeout(finalizationCtx, 10*time.Second)
			defer cancel()
			// Every processor drains its own batch in Close before it
			// releases its registration, so once processorsDone is closed all
			// accepted lines have reached the serializer.
			select {
			case <-processorsDone:
			case <-finalizeCtx.Done():
				return
			}
			a.doSerialize(finalizeCtx)
		}()
	}
	if retainOutput {
		// The finalizer observes finalizationCtx itself. The Start goroutine must
		// wait until that finalizer has completely stopped sending before its
		// caller can close the output channel.
		<-a.shutdownDone
		return
	}
	select {
	case <-a.shutdownDone:
	case <-ctx.Done():
	}
}

// PrepareShutdown claims final serialization and binds the output lifetime
// that every shutdown participant must use. This must happen before canceling
// command work so Start cannot wake first, choose an unrelated drain context,
// and let its caller close the result channel during final serialization. If
// Abort already owns termination, PrepareShutdown has no effect.
func (a *Aggregate) PrepareShutdown(ctx context.Context) {
	a.claimFinalization(ctx, false)
}

// Abort requests output-free termination and stops background processing
// without waiting. A finalization that already owns termination is allowed to
// finish; session generation replacement normally claims abort first and thus
// preempts old query work immediately.
func (a *Aggregate) Abort() {
	a.abort()
}

func (a *Aggregate) abort() bool {
	abortWon := a.claimAbort()
	a.sealProcessors()
	a.done.Shutdown()
	a.stopSerializeTicker()
	return abortWon
}

// AbortAndWait requests output-free termination and waits until every input
// processor has released its file and buffer resources.
func (a *Aggregate) AbortAndWait(ctx context.Context) {
	if ctx == nil {
		panic("aggregate: nil abort context")
	}
	a.Abort()
	select {
	case <-a.processorsDoneChannel():
	case <-ctx.Done():
	}
}

// FinishInput signals that all one-shot input (cat/grep style file reads)
// feeding this aggregate has been fully consumed and no further processors
// will register. Start reacts by emitting a final serialization and
// returning, which lets a server-side map command complete once its input
// line channels are exhausted.
// Follow-mode (tail) inputs must never call this so that interval-based
// streaming aggregation keeps running. Safe to call multiple times.
func (a *Aggregate) FinishInput() {
	a.inputFinished.Shutdown()
}

// Start the aggregation. It blocks until the context is canceled, the
// aggregate is shut down, or — for one-shot inputs — FinishInput signals that
// all input has been consumed, in which case all remaining data is flushed
// and serialized before returning.
func (a *Aggregate) Start(ctx context.Context, maprMessages chan<- string) {
	if ctx == nil {
		panic("aggregate: nil start context")
	}
	// Publish before launching the serialization loop. ServerHandler may have
	// already published this same destination before exposing the aggregate to
	// graceful shutdown; repeating the atomic store is harmless.
	a.PrepareOutput(ctx, maprMessages)
	interval := a.query.Interval
	if interval <= 0 {
		interval = time.Second
	}
	// Publish the ticker before launching serializationLoop below. The store
	// happens-before that goroutine's Load via the go statement, and any later
	// stopSerializeTicker on the teardown goroutine observes it through the
	// serializer's atomic ticker pointer.
	a.serializer.startTicker(interval)
	a.startOnce.Do(func() {
		if a.started != nil {
			close(a.started)
		}
	})
	defer a.stopSerializeTicker()

	loopDone := make(chan struct{})
	loopPanic := make(chan any, 1)
	go func() {
		defer close(loopDone)
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("Recovered panic in aggregate serialization goroutine",
					"panic", recovered, "stack", string(debug.Stack()))
				a.abort()
				loopPanic <- recovered
			}
		}()
		a.serializationLoop(ctx)
	}()

	shouldFinalize := false
	var recovered any
	select {
	case <-ctx.Done():
		shouldFinalize = !a.abort()
	case <-a.done.Done():
		shouldFinalize = !a.abort()
	case <-a.inputFinished.Done():
		// All one-shot input is consumed: emit the final result and stop.
		// Shutdown waits for the processors, drains the batch and performs
		// the final serialization. Without this path a server-mode dmap
		// command would block here until session teardown while keeping the
		// session's active-command count nonzero — a circular wait that hung
		// the client forever even though all results had been transmitted.
		shouldFinalize = true
	case recovered = <-loopPanic:
		a.abort()
	}
	if shouldFinalize {
		// Shutdown owns the final processor join and serialization. Waiting here
		// keeps the producer channel valid until the final result has been sent,
		// including when command cancellation and external shutdown race.
		a.shutdown(ctx, true)
	}

	// Stop the serialization loop and wait for it to exit before returning,
	// so no serialization can send on maprMessages once the caller closes its
	// side of the channel right after Start returns.
	a.done.Shutdown()
	<-loopDone
	if recovered == nil {
		select {
		case recovered = <-loopPanic:
		default:
		}
	}
	if recovered != nil {
		panic(recovered)
	}
}

// PrepareOutput publishes the serialization destination before Start is
// scheduled. ServerHandler uses this barrier before exposing the aggregate to
// concurrent graceful shutdown; direct callers may continue to rely on Start
// publishing the same channel itself.
func (a *Aggregate) PrepareOutput(ctx context.Context, maprMessages chan<- string) {
	if ctx == nil {
		panic("aggregate: nil output context")
	}
	if maprMessages == nil {
		a.serializer.prepareOutput(nil)
		return
	}
	a.serializer.prepareOutput(maprMessages)
}

// claimFinalization makes graceful final output the aggregate's terminal
// outcome. Abort and finalization race through this single state transition,
// so only one can win. Repeated Shutdown calls join the same finalization.
func (a *Aggregate) claimFinalization(outputCtx context.Context, start bool) (context.Context, bool, bool) {
	if outputCtx == nil {
		panic("aggregate: nil finalization context")
	}

	a.terminalMu.Lock()
	defer a.terminalMu.Unlock()

	switch a.terminalState {
	case aggregateRunning:
		a.terminalState = aggregateFinalizing
		a.finalizationCtx = outputCtx
	case aggregateFinalizing:
	case aggregateAborted:
		return nil, false, false
	default:
		return nil, false, false
	}
	owner := start && !a.shutdownStarted
	if owner {
		a.shutdownStarted = true
	}
	return a.finalizationCtx, owner, true
}

// claimAbort makes abrupt, output-free termination the aggregate's terminal
// outcome. A finalization that already owns termination cannot be preempted.
func (a *Aggregate) claimAbort() bool {
	a.terminalMu.Lock()
	defer a.terminalMu.Unlock()

	switch a.terminalState {
	case aggregateRunning:
		a.terminalState = aggregateAborted
		return true
	case aggregateFinalizing:
		return false
	case aggregateAborted:
		return true
	default:
		return false
	}
}

// aborted reports whether abrupt, output-free termination won the terminal
// state transition. Lines still batched in a processor are then discarded
// instead of aggregated.
func (a *Aggregate) aborted() bool {
	a.terminalMu.Lock()
	defer a.terminalMu.Unlock()
	return a.terminalState == aggregateAborted
}

// processRawBatch aggregates batch and recycles its line buffers. It runs in
// two phases: every line is parsed into its own scratch without any lock, then
// the serializer's group lock is taken once to merge all accepted lines. Only
// the merge is serialized between concurrently running file processors, and a
// concurrent serialization observes either the whole batch or none of it.
func (a *Aggregate) processRawBatch(batch []rawLine) {
	if len(batch) == 0 {
		return
	}
	a.processRawBatchWith(batchScratchPool.Get().(*batchScratch), batch)
}

// processRawBatchWith is processRawBatch with the batch scratch to parse into,
// which it recycles into batchScratchPool when done. Tests pass a fresh one.
func (a *Aggregate) processRawBatchWith(scratch *batchScratch, batch []rawLine) {
	// The deferred calls run in reverse order: the scratches, which borrow the
	// line buffers, are cleared before those buffers go back to the pool. The
	// batch scratch counts the line scratches it handed out, so recycling after
	// a panic part way through the batch leaves the original panic intact.
	defer recycleRawLines(batch)
	defer recycleBatchScratch(scratch)

	for i := range batch {
		lineScratch := scratch.line(i)
		accepted, err := a.parseLine(lineScratch, batch[i].content, batch[i].sourceID)
		if err != nil {
			a.errors.Add(1)
			// err can alias the line buffer recycled after the batch (a
			// *strconv.NumError keeps the offending value), so it is
			// formatted here instead of being handed to the logger.
			a.logger.Error("Error processing line:", err.Error(), "lineIndex", i)
			continue
		}
		if accepted {
			scratch.accepted = append(scratch.accepted, lineScratch)
		}
	}
	a.serializer.aggregateBatch(scratch.accepted)
}

// parseLine parses one line into scratch and reports whether it is to be
// aggregated, in which case scratch holds its fields and its group key.
//
// Everything the line yields — the parsed field values, the group key and the
// line itself — borrows lineContent, which processRawBatch recycles into the
// buffer pool once the batch has been merged. Nothing here may therefore
// outlive the batch: the scratch is cleared before the batch scratch is
// pooled again, a new group key is copied when the serializer inserts it, and
// mapr.AggregateSet copies the strings that last() and len() retain.
func (a *Aggregate) parseLine(scratch *lineScratch, lineContent *bytes.Buffer,
	sourceID string) (bool, error) {

	maprLine := borrowedLine(lineContent)
	parsedFields, err := logformat.MakeFieldsInto(a.parser, scratch.fields, maprLine, sourceID)
	if err == nil && !a.parserFillsFields {
		// Any other parser returns a map of its own, which it may clear and
		// reuse on its next call. The merge phase reads the fields only after
		// the whole batch was parsed, so they are copied into the scratch's
		// map, which stays valid until the batch has been merged. The values
		// are not cloned: Parser.MakeFields guarantees that they stay valid
		// and unchanged until maprLine is recycled.
		clear(scratch.fields)
		maps.Copy(scratch.fields, parsedFields)
		parsedFields = scratch.fields
	}
	// Record the peak field count so the retention limits can tell an
	// inflated map from a normal one; it costs one comparison per line.
	if n := len(scratch.fields); n > scratch.maxFields {
		scratch.maxFields = n
	}
	if err != nil {
		if !errors.Is(err, logformat.ErrIgnoreFields) {
			return false, err
		}
		return false, nil
	}

	// Apply where clause
	if !a.query.WhereClause(parsedFields) {
		return false, nil
	}

	// Apply set clause if needed
	if len(a.query.Set) > 0 {
		if err := a.query.SetClause(parsedFields); err != nil {
			return false, err
		}
	}

	// The group key is built in the scratch buffer and copied by the
	// serializer when it inserts a group of its own.
	scratch.key = buildGroupKey(scratch.key[:0], a.query.GroupBy, parsedFields)
	return true, nil
}

// serializationLoop handles periodic serialization.
func (a *Aggregate) serializationLoop(ctx context.Context) {
	if a.serializationLoopHook != nil {
		a.serializationLoopHook()
	}
	a.serializer.loop(ctx, a.done.Done(), a.serializationTickHook)
}

// Serialize requests serialization of all aggregated data. Requests coalesce
// while one is already pending; that pending pass will observe all data
// aggregated before it acquires the aggregate locks. Lines still batched in a
// Processor are aggregated on its next full batch or Flush.
func (a *Aggregate) Serialize(ctx context.Context) {
	if ctx == nil {
		panic("aggregate: nil serialize context")
	}
	a.serializer.request(ctx, a.done.Done())
}

// doSerialize performs the actual serialization.
func (a *Aggregate) doSerialize(ctx context.Context) {
	if ctx == nil {
		panic("aggregate: nil serialization context")
	}
	a.serializer.serialize(ctx)
}

func mergeCancelledSnapshot(query *mapr.Query, live, snapshot *mapr.AggregateSet,
	logger logging.Logger) {
	live.Samples += snapshot.Samples
	for _, sc := range query.Select {
		storage := sc.FieldStorage
		switch sc.Operation {
		case mapr.Count, mapr.Sum, mapr.Avg, mapr.Percentage, mapr.Percentile:
			live.FValues[storage] += snapshot.FValues[storage]
		case mapr.Min:
			liveValue, ok := live.FValues[storage]
			if !ok {
				live.FValues[storage] = snapshot.FValues[storage]
				continue
			}
			if snapshotValue := snapshot.FValues[storage]; snapshotValue < liveValue {
				live.FValues[storage] = snapshotValue
			}
		case mapr.Max:
			liveValue, ok := live.FValues[storage]
			if !ok {
				live.FValues[storage] = snapshot.FValues[storage]
				continue
			}
			if snapshotValue := snapshot.FValues[storage]; snapshotValue > liveValue {
				live.FValues[storage] = snapshotValue
			}
		case mapr.Last:
			if _, ok := live.SValues[storage]; !ok {
				if snapshotValue, ok := snapshot.SValues[storage]; ok {
					live.SValues[storage] = snapshotValue
				}
			}
		case mapr.Len:
			if _, ok := live.SValues[storage]; !ok {
				if snapshotValue, ok := snapshot.SValues[storage]; ok {
					live.SValues[storage] = snapshotValue
					live.FValues[storage] = snapshot.FValues[storage]
				}
			}
		default:
			logger.Error("Aggregate re-merge encountered unsupported aggregation",
				"operation", sc.Operation, "storage", storage)
		}
	}
}

// Processor implements the line processor interface for aggregation. Each
// input file gets its own Processor, fed by a single reader goroutine, and the
// Processor batches that file's lines locally: no lock is taken per line, and
// the serializer's group lock is taken once per batch.
//
// A batch is aggregated when it is full and on every Flush, so a follow-mode
// reader, which flushes after each read, never holds lines back past a read
// boundary. A one-shot read flushes when the file is done; until then up to
// one batch of lines per file is not yet visible to periodic serialization.
//
// Every line of a Processor is parsed under the Processor's sourceKey, never
// under the sourceID the reader passes along. A stateful parser such as the
// CSV one installs a source's header from the first line it parses for that
// source, so each file read needs a key of its own whose lines reach the
// parser in file order. A Processor guarantees both: it is fed by one reader
// goroutine and parses its batches in order. The reader's sourceID (the glob
// ID) does not: two files with the same base name in different directories
// share it, and so do the reads before and after a follow-mode reader reopens
// a rotated file.
//
// A follow-mode reader that detects an in-place truncation keeps feeding the
// same Processor from the rewritten content. It then calls SourceRestarted,
// which moves the Processor to a fresh source key, so the rewritten content
// installs its own header as well.
type Processor struct {
	aggregate  *Aggregate
	globID     string
	sourceKey  string
	registered bool
	batch      lineBatch
	flushOnce  sync.Once
	closeOnce  sync.Once
}

// NewProcessor creates a new aggregate processor.
func NewProcessor(aggregate *Aggregate, globID string) *Processor {
	aggregate.processorMu.Lock()
	registered := !aggregate.processorsSealed
	if registered {
		aggregate.ensureProcessorsDoneLocked()
		aggregate.processorCount++
		aggregate.activeProcessors.Add(1)
	}
	aggregate.processorMu.Unlock()
	return &Processor{
		aggregate:  aggregate,
		globID:     globID,
		sourceKey:  aggregate.newSourceKey(globID),
		registered: registered,
	}
}

// newSourceKey returns a parser source key no other processor or source
// generation of this aggregate uses.
func (a *Aggregate) newSourceKey(globID string) string {
	return strconv.FormatUint(a.processorSeq.Add(1), 10) + ":" + globID
}

// SourceRestarted implements line.SourceRestarter. The reader calls it from
// the goroutine feeding the processor when the file was truncated in place and
// is read again from its beginning. The lines batched so far belong to the old
// content and are aggregated under the old source key first; then that key's
// parser state is released and the processor continues under a fresh key, so
// the first line of the new content installs its CSV header.
func (p *Processor) SourceRestarted() {
	if p.registered {
		p.drain()
	}
	p.releaseSource()
	p.sourceKey = p.aggregate.newSourceKey(p.globID)
}

// ProcessLine adds a line to the processor's batch and aggregates the batch
// once it is full. The processor takes ownership of lineContent. The line is
// parsed under the processor's own source key, see Processor.
func (p *Processor) ProcessLine(lineContent *bytes.Buffer, _ uint64, _ string) error {
	if !p.registered || p.aggregate.stopping() {
		pool.RecycleBytesBuffer(lineContent)
		return nil
	}
	if p.batch.add(rawLine{content: lineContent, sourceID: p.sourceKey}) {
		p.drain()
	}
	return nil
}

// Flush aggregates the lines batched so far. Lines accepted before a graceful
// shutdown began are still aggregated, so they reach the final serialization;
// after an abort they are discarded.
func (p *Processor) Flush() error {
	if !p.registered {
		return nil
	}
	p.drain()
	if p.aggregate.stopping() {
		return nil
	}
	p.flushOnce.Do(func() {
		p.aggregate.filesProcessed.Add(1)
	})
	return nil
}

// Close flushes any remaining data and releases the parser's state for the
// processor's source key.
func (p *Processor) Close() error {
	var err error
	p.closeOnce.Do(func() {
		// Deferred first, so it runs after the final drain below: no line
		// of this processor is parsed any more once its state is released.
		defer p.releaseSource()
		if p.registered {
			// Register accounting release before Flush. If batch processing
			// panics, aggregate abort/shutdown must still be able to join every
			// processor rather than waiting forever on a leaked reservation.
			defer p.aggregate.releaseProcessor()
			defer p.aggregate.activeProcessors.Add(-1)
		}
		err = p.Flush()
	})
	return err
}

// releaseSource drops the state a stateful parser keeps for the processor's
// current source key. The key is unique to the processor and source
// generation, so without this every file read, including every reopen of a
// rotated file and every in-place truncation in follow mode, would leave an
// entry behind for the rest of the session.
func (p *Processor) releaseSource() {
	if releaser, ok := p.aggregate.parser.(logformat.SourceReleaser); ok {
		releaser.ReleaseSource(p.sourceKey)
	}
}

// drain hands the batched lines to the aggregate, or discards them once the
// aggregate was aborted, and empties the batch.
func (p *Processor) drain() {
	lines := p.batch.pending()
	if len(lines) == 0 {
		return
	}
	defer p.batch.reset()

	if p.aggregate.stopping() && p.aggregate.aborted() {
		recycleRawLines(lines)
		return
	}
	// Only lines handed to the aggregation are counted, not those an abort
	// discards. Counted once per batch rather than per line, so that
	// processors of different files do not contend on the shared counter.
	p.aggregate.linesProcessed.Add(uint64(len(lines)))
	p.aggregate.processRawBatch(lines)
}

func (a *Aggregate) sealProcessors() <-chan struct{} {
	a.processorMu.Lock()
	defer a.processorMu.Unlock()
	a.ensureProcessorsDoneLocked()
	if a.processorsSealed {
		return a.processorsDone
	}
	a.processorsSealed = true
	if a.processorCount == 0 {
		close(a.processorsDone)
	}
	return a.processorsDone
}

func (a *Aggregate) processorsDoneChannel() <-chan struct{} {
	return a.sealProcessors()
}

func (a *Aggregate) ensureProcessorsDoneLocked() {
	if a.processorsDone == nil {
		a.processorsDone = make(chan struct{})
	}
}

func (a *Aggregate) releaseProcessor() {
	a.processorMu.Lock()
	defer a.processorMu.Unlock()
	a.processorCount--
	if a.processorsSealed && a.processorCount == 0 {
		close(a.processorsDone)
	}
}
