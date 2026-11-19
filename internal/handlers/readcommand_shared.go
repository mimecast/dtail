package handlers

import (
	"context"
	"errors"

	"github.com/mimecast/dtail/internal/ctxutil"
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/fs/readhub"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/logging"
	"github.com/mimecast/dtail/internal/omode"
	"github.com/mimecast/dtail/internal/regex"
)

// followSharedFunc returns hub's Follow, or nil for no hub.
func followSharedFunc(hub *readhub.Hub) func(context.Context, readhub.Session) error {
	if hub == nil {
		return nil
	}
	return hub.Follow
}

// shouldShareRead reports whether the read may use the shared follow reader
// of its file instead of a private one. Only follow reads of a validated,
// uncompressed file in dserver share: a session that falls behind moves to a
// private reader at the byte offset where its shared read stopped, which a
// compressed file does not offer. A max-count limit keeps the read private:
// when the limit is reached, a private follow read starts over from the
// beginning of the file, which a shared read cannot do for one session.
func (r *readCommand) shouldShareRead(ltx lcontext.LContext, target *fs.ValidatedReadTarget, path string) bool {
	return r.followShared != nil &&
		!r.serverless &&
		r.mode == omode.TailClient &&
		target != nil &&
		target.Kind == fs.FileKind &&
		fs.CompressionFormat(path) == "" &&
		ltx.MaxCount == 0
}

// readShared follows the file through its shared reader. The hub moves the
// session to a private reader of its own when it falls behind or the shared
// reader fails, so the read only ends early like the private read loop's
// iteration would: with a processor error, a max-count stop or a panic.
func (r *readCommand) readShared(ctx context.Context, ltx lcontext.LContext, re regex.Regex,
	options readerFactoryOptions) {

	path, globID := options.path, options.globID
	r.logger.Info(r.logContext, "Using shared follow read", path, globID)
	writer := r.newLineWriter(ctx, r.generation)

	err := r.followShared(ctx, readhub.Session{
		Target:         *options.target,
		FilePath:       path,
		GlobID:         globID,
		LContext:       ltx,
		Regex:          re,
		ServerMessages: options.serverMessages,
		Logger:         options.logger,
		NewProcessor: func() line.Processor {
			return r.makeProcessor(path, globID, writer)
		},
		ReportFailure: r.reportHubFailure(ctx),
	})
	if err == nil || ctx.Err() != nil {
		return
	}

	r.logger.Error(r.logContext, path, globID, err)
	if errors.Is(err, fs.ErrReaderWorkerPanic) {
		// The private read loop panics on a reader worker panic, too.
		panic(err)
	}
	if !errors.Is(err, readhub.ErrStopped) {
		// The private read loop reports a failed read iteration to the client
		// before it reads the file again (see executeReadLoop); only a
		// max-count stop ends an iteration there without a failure.
		r.reportReadError(ctx, err)
	}
	r.restartPrivately(ctx, ltx, re, options, writer)
}

// reportHubFailure returns the readhub.Session.ReportFailure of the read:
// failures the hub handles itself, without ending the session's read, reach
// the client like a failed iteration of the private read loop. err, when the
// hub has it, is reported as that loop reports it (reportReadError), so that
// output the session's private reader after an eviction dropped as too large
// for the output buffer is reported as such, with its warning, and not as a
// failed read of the file; a failure without an error is a failed read.
func (r *readCommand) reportHubFailure(ctx context.Context) func(err error) {
	return func(err error) {
		if err == nil {
			r.reportFailure(ctx, readFailureReadingFile)
			return
		}
		r.reportReadError(ctx, err)
	}
}

// restartPrivately continues after the session's processor failed. The
// private read loop logs the error, waits, and starts the reader again, which
// reads the already opened file from its beginning; do the same with a
// private reader that starts at the beginning.
func (r *readCommand) restartPrivately(ctx context.Context, ltx lcontext.LContext, re regex.Regex,
	options readerFactoryOptions, writer LineWriter) {

	if !ctxutil.Sleep(ctx, r.timings.readRetryInterval) {
		return
	}
	r.logger.Info(options.path, options.globID, "Reading file again")
	reader, err := makeReader(options, omode.TailClient, false)
	if err != nil {
		r.sendServerMessage(ctx, logging.WarnMessage(r.logger, r.logContext, "Unable to create file reader", err))
		r.reportFailure(ctx, readFailureReader)
		return
	}
	r.executeReadLoop(ctx, ltx, options.path, options.globID, re, reader,
		r.readViaProcessor(options.path, options.globID, writer))
}
