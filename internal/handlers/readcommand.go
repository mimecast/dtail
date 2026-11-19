package handlers

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	mapaggregate "github.com/mimecast/dtail/internal/mapr/aggregate"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

type readCommand struct {
	server               readCommandServer
	logger               logging.Logger
	readerLogger         logging.Logger
	logContext           any
	timings              readTimings
	newLineWriter        lineWriterFactory
	abort                func()
	serverless           bool
	aggregate            *mapaggregate.Aggregate
	mode                 omode.Mode
	generation           uint64
	pendingInputReserved bool
	shutdownCoordinator  *shutdownCoordinator
	inputBatch           *commandBatch
	inputBatchRead       commandBatchRead
}

type pendingInputReservationKeyType struct{}

var pendingInputReservationKey pendingInputReservationKeyType

// pendingInputReservation bridges synchronous protocol dispatch and the
// asynchronously started readCommand. claim and releaseIfUnclaimed are called
// by the dispatch goroutine before the handler returns; the read goroutine owns
// the pending counter only after claim succeeds.
type pendingInputReservation struct {
	lifecycle           readCommandLifecycle
	aggregate           *mapaggregate.Aggregate
	shutdownCoordinator *shutdownCoordinator
	ctx                 context.Context
	admission           *commandAdmissionResult
	inputBatch          *commandBatch
	inputBatchRead      commandBatchRead
	claimed             bool
	released            bool
}

func newPendingInputReservation(ctx context.Context, provider readCommandDependencyProvider, mode omode.Mode) *pendingInputReservation {
	if ctx == nil {
		panic("handlers: nil pending input context")
	}
	dependencies := provider.readCommandDependencies()
	dependencies.server.AddPendingFiles(1)
	aggregate := dependencies.aggregates.Aggregate()
	oneShotInput := mode == omode.CatClient || mode == omode.GrepClient
	return &pendingInputReservation{
		ctx:       ctx,
		lifecycle: dependencies.lifecycle,
		aggregate: aggregate,
		shutdownCoordinator: newShutdownCoordinator(dependencies.lifecycle, dependencies.aggregates,
			dependencies.timings, oneShotInput, aggregate),
	}
}

func withPendingInputReservation(ctx context.Context, reservation *pendingInputReservation) context.Context {
	return context.WithValue(ctx, pendingInputReservationKey, reservation)
}

func pendingInputReservationFromContext(ctx context.Context) *pendingInputReservation {
	reservation, _ := ctx.Value(pendingInputReservationKey).(*pendingInputReservation)
	return reservation
}

func (r *pendingInputReservation) claim() bool {
	if r == nil || r.claimed || r.released {
		return false
	}
	r.claimed = true
	return true
}

func (r *pendingInputReservation) releaseIfUnclaimed() {
	if r == nil || r.claimed || r.released {
		return
	}
	r.released = true
	if r.admission != nil && r.admission.admitted {
		// The admitted command already ran its commandFinished callback while
		// this reservation kept pending input non-zero. Complete through the
		// normal read lifecycle so the final zero transition can both finish a
		// one-shot aggregate and start idle shutdown.
		r.shutdownCoordinator.onFileProcessed(r.ctx, "unclaimed read command")
		r.completeInputBatch()
		return
	}

	remaining, _ := r.lifecycle.CompletePendingFile()
	if remaining == 0 {
		// Dispatch can reject the command before admission (for example during
		// graceful shutdown or option parsing). Preserve aggregate completion
		// for an older read without starting a competing shutdown sequence.
		r.shutdownCoordinator.maybeFinishAggregateInput(r.ctx)
	}
	r.completeInputBatch()
}

func (r *pendingInputReservation) completeInputBatch() {
	if r.inputBatch == nil || !r.inputBatchRead.tracked {
		return
	}
	if aggregate := r.inputBatch.completeRead(r.inputBatchRead); aggregate != nil {
		aggregate.FinishInput()
	}
	r.inputBatch = nil
	r.inputBatchRead = commandBatchRead{}
}

type readStrategy func(context.Context, lcontext.LContext, fs.FileReader, regex.Regex) error

func newReadCommand(provider readCommandDependencyProvider, mode omode.Mode) *readCommand {
	dependencies := provider.readCommandDependencies()
	return newReadCommandWithDependencies(dependencies, mode, dependencies.aggregates.Aggregate())
}

func newReadCommandWithAggregate(provider readCommandDependencyProvider, mode omode.Mode, aggregate *mapaggregate.Aggregate) *readCommand {
	return newReadCommandWithDependencies(provider.readCommandDependencies(), mode, aggregate)
}

func newReadCommandWithDependencies(dependencies readCommandDependencies, mode omode.Mode,
	aggregate *mapaggregate.Aggregate) *readCommand {
	// cat/grep reads are one-shot: their input is exhausted once every file
	// has been read to EOF. tail follows its files indefinitely, so its input
	// never exhausts and must not finish an output aggregate.
	oneShotInput := mode == omode.CatClient || mode == omode.GrepClient
	return &readCommand{
		server:        dependencies.server,
		logger:        logging.OrNop(dependencies.logger),
		readerLogger:  logging.OrNop(dependencies.readerLogger),
		logContext:    dependencies.logContext,
		timings:       dependencies.timings.withDefaults(),
		newLineWriter: dependencies.newLineWriter,
		abort:         dependencies.abortAfterPanic,
		serverless:    dependencies.serverless,
		aggregate:     aggregate,
		mode:          mode,
		shutdownCoordinator: newShutdownCoordinator(dependencies.lifecycle, dependencies.aggregates,
			dependencies.timings, oneShotInput, aggregate),
	}
}

func (r *readCommand) Start(ctx context.Context, ltx lcontext.LContext,
	argc int, args []string, retries int) {
	// If parsing, glob expansion, retry cancellation, or another early-return
	// path prevents this command from resolving concrete inputs, release its
	// dispatch-time reservation here. Once an input is resolved, readPipe or
	// readFiles transfers the reservation and this becomes a no-op.
	defer func() { r.releasePendingInputReservation(ctx) }()
	r.generation = sessionGenerationFromContext(ctx)

	re := regex.NewNoop()
	if argc >= 4 {
		deserializedRegex, err := regex.Deserialize(strings.Join(args[2:], " "))
		if err != nil {
			r.sendServerMessage(ctx, r.logger.Error(r.logContext,
				"Unable to parse command", err))
			return
		}
		re = deserializedRegex
	}
	if argc < 3 {
		r.sendServerMessage(ctx, r.logger.Warn(r.logContext,
			"Unable to parse command", args, argc))
		return
	}

	// In serverless mode, can also read data from pipe
	// e.g.: grep foo bar.log | dmap 'from STATS select ...'
	// Only read from pipe if no file argument is provided
	isPipe := r.isInputFromPipe() && (argc < 2 || args[1] == "" || args[1] == "-")
	switch {
	case isPipe:
		r.logger.Debug("Reading data from stdin pipe")
		r.readPipe(ctx, ltx, re)
	case fs.IsJournalSpec(args[1]):
		r.logger.Debug("Reading data from journal")
		r.readJournal(ctx, ltx, args[1], re)
	default:
		r.logger.Debug("Reading data from file(s)")
		r.readGlob(ctx, ltx, args[1], re, retries)
	}
}

func (r *readCommand) adoptPendingInputReservation(reservation *pendingInputReservation) {
	if reservation == nil || !reservation.claim() {
		r.server.AddPendingFiles(1)
		r.pendingInputReserved = true
		return
	}
	r.shutdownCoordinator = reservation.shutdownCoordinator
	r.inputBatch = reservation.inputBatch
	r.inputBatchRead = reservation.inputBatchRead
	r.pendingInputReserved = true
}

func (r *readCommand) completeInputBatch() {
	if r.inputBatch == nil || !r.inputBatchRead.tracked {
		return
	}
	if aggregate := r.inputBatch.completeRead(r.inputBatchRead); aggregate != nil {
		aggregate.FinishInput()
	}
	r.inputBatch = nil
	r.inputBatchRead = commandBatchRead{}
}

// readPipe reads the single stdin-pipe input in serverless mode (e.g.
// `grep foo bar.log | dmap 'from STATS select ...'`). Unlike file/glob/journal
// reads it does NOT go through readFiles/readFileIfPermissions, so it must
// reproduce their finalization here: the pipe is accounted as one pending input
// and onFileProcessed is invoked once it drains. That is what drives the
// shutdown coordinator's FinishInput signal to a output aggregate — without it a
// serverless dmap reading from a pipe would block in Aggregate.Start on its
// inputFinished signal forever (the client keeps re-gathering interim results
// and never terminates). Previously the pipe fed a regular Aggregate that
// finalized when its input line channel was closed; the output aggregate has no
// such channel-close, so the pending-file/onFileProcessed path is the
// equivalent "input exhausted" signal. registerPendingFiles transfers the
// dispatch-time reservation to the pipe (or creates one for direct unit use),
// balancing the unconditional decrement in onFileProcessed.
func (r *readCommand) readPipe(ctx context.Context, ltx lcontext.LContext, re regex.Regex) {
	r.registerPendingFiles(1)
	defer r.shutdownCoordinator.onFileProcessed(ctx, "-")
	// Empty file path and globID "-" represents reading from the stdin pipe.
	r.read(ctx, ltx, "", nil, "-", re)
}

func (r *readCommand) readJournal(ctx context.Context, ltx lcontext.LContext,
	spec string, re regex.Regex) {

	r.readFiles(ctx, ltx, []string{spec}, spec, re)
}

func (r *readCommand) readGlob(ctx context.Context, ltx lcontext.LContext,
	glob string, re regex.Regex, retries int) {

	retryInterval := r.timings.globRetryInterval
	glob = filepath.Clean(glob)

	for retryCount := 0; retryCount < retries; retryCount++ {
		paths, err := filepath.Glob(glob)
		if err != nil {
			r.logger.Warn(r.logContext, glob, err)
			if !ctxutil.Sleep(ctx, retryInterval) {
				return
			}
			continue
		}

		if numPaths := len(paths); numPaths == 0 {
			r.logger.Error(r.logContext, "No such file(s) to read", glob)
			r.sendServerMessage(ctx, r.logger.Warn(r.logContext,
				"Unable to read file(s), check server logs"))
			select {
			case <-ctx.Done():
				return
			default:
			}
			if !ctxutil.Sleep(ctx, retryInterval) {
				return
			}
			continue
		}

		// Cap the number of paths to prevent an authenticated user with a broad
		// read permission from spawning an unbounded number of goroutines and
		// exhausting server memory. Excess paths are dropped with a warning so
		// the partial result is still delivered rather than failing entirely.
		if maxTargets := r.timings.maxGlobTargets; len(paths) > maxTargets {
			r.logger.Warn(r.logContext, "Glob expansion exceeded cap, truncating",
				"glob", glob, "matched", len(paths), "cap", maxTargets)
			r.sendServerMessage(ctx, r.logger.Warn(r.logContext,
				"Glob expansion exceeded server limit, only first targets served",
				"limit", maxTargets, "matched", len(paths)))
			paths = paths[:maxTargets]
		}

		r.readFiles(ctx, ltx, paths, glob, re)
		return
	}

	r.sendServerMessage(ctx, r.logger.Warn(r.logContext,
		"Giving up to read file(s)"))
}

func (r *readCommand) readFiles(ctx context.Context, ltx lcontext.LContext,
	paths []string, glob string, re regex.Regex) {

	r.logger.Info(r.logContext, "Processing files", "count", len(paths), "glob", glob)

	// Transfer the dispatch-time reservation to the resolved paths before any
	// file goroutine can complete. Adding len(paths)-1 preserves a non-zero
	// count throughout the hand-off. Direct unit callers without a reservation
	// continue to register the full path count.
	totalPending := r.registerPendingFiles(len(paths))
	r.logger.Info(r.logContext, "Added pending files", "count", len(paths), "totalPending", totalPending)

	var wg sync.WaitGroup
	wg.Add(len(paths))
	for _, path := range paths {
		go r.readFileIfPermissions(ctx, ltx, &wg, path, glob, re)
	}
	wg.Wait()

	r.logger.Info(r.logContext, "All files processed", "count", len(paths))

	select {
	case <-ctx.Done():
		return
	default:
	}

	// The handler owns output draining and the epoch-protected EOF handshake.
	// readCommand only announces that this group of file reads has completed.
	r.server.FinishReadBatch(ctx, r.mode, r.generation)

	// In output mode with aggregate, we don't close the shared channel here
	// because it will be used across multiple invocations
	// The aggregate will handle channel closure when it's done
}

func (r *readCommand) registerPendingFiles(count int) int32 {
	if count <= 0 {
		pending, _ := r.server.PendingAndActive()
		return pending
	}
	delta := int32(count)
	if r.pendingInputReserved {
		delta--
		r.pendingInputReserved = false
	}
	if delta != 0 {
		return r.server.AddPendingFiles(delta)
	}
	pending, _ := r.server.PendingAndActive()
	return pending
}

func (r *readCommand) releasePendingInputReservation(ctx context.Context) {
	if !r.pendingInputReserved {
		return
	}
	r.pendingInputReserved = false
	r.shutdownCoordinator.onFileProcessed(ctx, "unresolved read command")
}

func (r *readCommand) readFileIfPermissions(ctx context.Context, ltx lcontext.LContext,
	wg *sync.WaitGroup, path, glob string, re regex.Regex) {

	defer recoverHandlerPanic(r.logger, r.logContext, "file read cleanup", r.abortAfterPanic)
	defer wg.Done()
	defer func() {
		r.shutdownCoordinator.onFileProcessed(ctx, path)
	}()
	defer recoverHandlerPanic(r.logger, r.logContext, "file read", r.abortAfterPanic)

	globID := r.makeGlobID(ctx, path, glob)
	target, ok := r.server.PrepareReadTarget(path)
	if !ok {
		r.logger.Error(r.logContext, "No permission to read file", path, globID)
		r.sendServerMessage(ctx, r.logger.Warn(r.logContext,
			"Unable to read file(s), check server logs"))
		return
	}
	r.read(ctx, ltx, path, &target, globID, re)
}

func (r *readCommand) read(ctx context.Context, ltx lcontext.LContext,
	path string, target *fs.ValidatedReadTarget, globID string, re regex.Regex) {

	r.logger.Info(r.logContext, "Start reading", path, globID)
	r.logRegexMode(re)

	serverMessages, closeServerMessages := r.server.NewReadMessages(ctx, r.generation)
	defer closeServerMessages()

	factory := readerFactoryFor(r.mode)
	reader, limiter, err := factory(readerFactoryOptions{
		slots:          r.server,
		target:         target,
		path:           path,
		globID:         globID,
		serverMessages: serverMessages,
		maxLineLength:  r.timings.maxLineLength,
		logger:         r.readerLogger,
	})
	if err != nil {
		message := "Unable to create file reader"
		if target != nil && target.Kind == fs.JournalKind {
			message = "Unable to read journal"
		}
		r.sendServerMessage(ctx, r.logger.Warn(r.logContext, message, err))
		return
	}

	release, acquired := limiter(ctx, path)
	if !acquired {
		return
	}
	defer release()

	// Output is the one and only read path. read() is only ever invoked for the
	// cat/grep/tail command handlers (see makeReadCommandHandler), and MapReduce
	// always builds a Aggregate for both server mode and serverless (see
	// mapcommand.go). So either the read runs in cat/grep mode with
	// Aggregate() non-nil and feeds it directly via Processor,
	// or it is genuine cat/grep/tail output using the output direct-output writer;
	// makeProcessor picks between the two. The former channel-based fallback
	// (readViaChannels feeding the former aggregate implementation) was removed once
	// serverless MapReduce migrated to the output aggregate (tasks sv0/hv0), so
	// there is no non-output path left here.
	r.logger.Debug(r.logContext, "Selecting read mode",
		"mode", r.mode, "hasAggregate", r.aggregate != nil)
	r.logger.Info(r.logContext, "Using turbo mode for reading", path, "mode", r.mode, "hasAggregate", r.aggregate != nil)
	r.readWithProcessor(ctx, ltx, path, globID, re, reader)
}

func (r *readCommand) readWithProcessor(ctx context.Context, ltx lcontext.LContext,
	path, globID string, re regex.Regex, reader fs.FileReader) {

	r.logger.Info(r.logContext, "Using output channel-less implementation", path, globID)
	r.logRegexMode(re)

	writer := r.newLineWriter(ctx, r.generation)

	r.executeReadLoop(ctx, ltx, path, globID, re, reader, r.readViaProcessor(path, globID, writer))
}

func (r *readCommand) executeReadLoop(ctx context.Context, ltx lcontext.LContext,
	path, globID string, re regex.Regex, reader fs.FileReader, strategy readStrategy) {

	for {
		if err := strategy(ctx, ltx, reader, re); err != nil {
			r.logger.Error(r.logContext, path, globID, err)
			if errors.Is(err, fs.ErrReaderWorkerPanic) {
				panic(err)
			}
		}

		select {
		case <-ctx.Done():
			return
		default:
			if !reader.Retry() {
				return
			}
		}

		if !ctxutil.Sleep(ctx, r.timings.readRetryInterval) {
			return
		}
		r.logger.Info(path, globID, "Reading file again")
	}
}

func (r *readCommand) readViaProcessor(path, globID string, writer LineWriter) readStrategy {
	return func(ctx context.Context, ltx lcontext.LContext, reader fs.FileReader, re regex.Regex) error {
		r.logger.Trace(r.logContext, path, globID, "readWithProcessor -> starting read loop iteration")

		processor := r.makeProcessor(path, globID, writer)

		startErr := func() error {
			// Register cleanup before entering reader code. A panic in either the
			// reader or Flush must still close an Processor and release
			// its processorsWg reservation. LIFO order preserves Flush-before-Close.
			defer func() {
				closeErr := processor.Close()
				if closeErr != nil {
					r.logger.Error(r.logContext, path, globID, "close error", closeErr)
				}
				r.logger.Trace(r.logContext, path, globID, "readWithProcessor -> processor closed")
			}()
			defer func() {
				r.logger.Trace(r.logContext, path, globID, "readWithProcessor -> flushing processor")
				if flushErr := processor.Flush(); flushErr != nil {
					r.logger.Error(r.logContext, path, globID, "flush error", flushErr)
				}
			}()

			r.logger.Trace(r.logContext, path, globID, "readWithProcessor -> reader.Start -> about to start")
			err := reader.Start(ctx, ltx, processor, re)
			r.logger.Trace(r.logContext, path, globID, "readWithProcessor -> reader.Start -> completed")
			return err
		}()

		return startErr
	}
}

func (r *readCommand) makeProcessor(path, globID string, writer LineWriter) line.Processor {
	if aggregator := r.aggregate; aggregator != nil {
		r.logger.Info(r.logContext, "Using turbo aggregate processor for MapReduce", path, globID)
		return mapaggregate.NewProcessor(aggregator, globID)
	}

	return NewDirectLineProcessor(writer, globID, r.logger)
}

func (r *readCommand) logRegexMode(re regex.Regex) {
	if r.mode != omode.GrepClient {
		return
	}
	switch {
	case re.IsLiteral():
		r.logger.Info(r.logContext, "Using optimized literal string matching for pattern:", re.Pattern())
	default:
		r.logger.Info(r.logContext, "Using regex matching for pattern:", re.Pattern())
	}
}

func (r *readCommand) makeGlobID(ctx context.Context, path, glob string) string {
	var idParts []string
	pathParts := strings.Split(path, "/")

	for i, globPart := range strings.Split(glob, "/") {
		if strings.Contains(globPart, "*") {
			idParts = append(idParts, pathParts[i])
		}
	}

	if len(idParts) > 0 {
		return strings.Join(idParts, "/")
	}
	if len(pathParts) > 0 {
		return pathParts[len(pathParts)-1]
	}

	r.sendServerMessage(ctx, r.logger.Warn("Empty file path given?", path, glob))
	return ""
}

// sendServerMessage forwards a user-visible message to the session's shared
// serverMessages channel (capacity 10, drained only by baseHandler.Read). A
// bare send here would block forever once the client disconnects and Read
// stops draining — pinning one goroutine per stuck send (e.g. one per
// permission-denied file of a glob expansion, up to MaxGlobTargets). The
// select on ctx.Done() makes the send abandonable: the per-command context is
// cancelled on command completion and on handler shutdown (see
// baseHandler.newCommandContext), mirroring the done-guarded baseHandler.send.
func (r *readCommand) sendServerMessage(ctx context.Context, message string) {
	r.server.SendReadMessage(ctx, r.generation, message)
}

func (r *readCommand) abortAfterPanic() {
	if r.abort != nil {
		r.abort()
	}
}

func (r *readCommand) isInputFromPipe() bool {
	if !r.serverless {
		// Can read from pipe only in serverless mode.
		return false
	}
	fileInfo, _ := os.Stdin.Stat()
	return fileInfo.Mode()&os.ModeCharDevice == 0
}
