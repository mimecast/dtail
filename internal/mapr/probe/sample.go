// Package probe is to discover the MapReduce tables, fields and variables of
// the log lines a server holds, for the interactive query completion. It
// holds the sampler (this file), which returns a bounded sample of recent
// lines of one read target. The probe command
// (internal/handlers/probecommand.go) samples with it and parses the
// samples with logformat.Probe; see docs/dev/probe.md.
package probe

import (
	"bytes"
	"context"
	"strconv"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/io/journal"
	"github.com/mimecast/dtail/internal/io/line"
	"github.com/mimecast/dtail/internal/io/pool"
	"github.com/mimecast/dtail/internal/lcontext"
	"github.com/mimecast/dtail/internal/regex"
)

// collectorProcessor is the line.Processor of a journal sample: it adds every
// line to the collector and recycles its buffer.
type collectorProcessor struct {
	collector *fs.SampleCollector
}

var _ line.Processor = collectorProcessor{}

// Sample returns a bounded sample of recent lines of target, oldest first,
// without their LF or CRLF terminators (fs.Sample). It reads once and never
// follows, and it ends early with ctx's error when ctx is done.
//
//   - A file target is sampled with fs.SampleFile: an uncompressed file at its
//     end (lines starting within the last options.WindowBytes, 256 KiB by
//     default, or options.ScanBytes, the partial first line dropped), a
//     gzip or zstd file (chosen by
//     the suffix of path, as for a read command) at its beginning. A tail
//     sample also returns the file's first line (fs.Sample.FirstLine) when
//     its lines do not start with it, which the caller puts in front of the
//     lines (the probe command does so for every format), as the csv parser
//     and probe take a source's first line as its header.
//   - A journal target ("journal:unit") is sampled with journalctl -n
//     MaxLines (or ScanLines) through the journal reader; on systems without
//     journalctl (or not Linux) its error is returned.
//
// The lines of every sample are at most options.MaxLines (500 by default)
// and at most options.WindowBytes of line data (256 KiB), lines longer than
// options.MaxLineLength being cut into pieces as every read cuts them; a
// line (or piece) larger than WindowBytes on its own is skipped. A tail
// sample's FirstLine comes on top of these limits. With options.Keep only
// the lines it accepts are kept (a table probe); options.Observe sees every
// scanned line.
//
// Sample does no permission check: the caller resolves path to target with
// the session user's read permissions (sessionuser.User.ResolveReadTarget)
// first, as for a read command.
func Sample(ctx context.Context, target fs.ValidatedReadTarget, path string,
	options fs.SampleOptions) (fs.Sample, error) {

	if err := ctx.Err(); err != nil {
		return fs.Sample{}, err
	}
	if target.Kind == fs.JournalKind {
		return sampleJournal(ctx, target.ResolvedPath(), options)
	}
	return fs.SampleFile(ctx, target, path, options)
}

// ProcessLine adds line to the collector and takes ownership of its buffer.
func (p collectorProcessor) ProcessLine(line *bytes.Buffer, _ uint64, _ string) error {
	p.collector.Add(line.Bytes())
	pool.RecycleBytesBuffer(line)
	return nil
}

// Flush does nothing: the collector holds every line.
func (p collectorProcessor) Flush() error { return nil }

// Close does nothing.
func (p collectorProcessor) Close() error { return nil }

// sampleJournal samples the journal target spec with a one-shot journalctl
// run asking for its last MaxLines entries (ScanLines if set, so that Keep
// can pick from more entries than are kept). Entries are cut at
// MaxLineLength by the reader, so they can make more pieces than MaxLines:
// the collector keeps the most recent ones. Long-line warnings and
// journalctl's stderr go nowhere (no server messages), as a sample has no
// client output.
func sampleJournal(ctx context.Context, spec string, options fs.SampleOptions) (fs.Sample, error) {
	options = options.WithDefaults()
	entries := options.MaxLines
	if options.ScanLines > 0 {
		entries = options.ScanLines
	}
	reader, err := journal.NewReader(journal.ReaderOptions{
		Args:          append(journal.UnitArgs(spec), "-n", strconv.Itoa(entries)),
		SourceID:      spec,
		MaxLineLength: options.MaxLineLength,
	})
	if err != nil {
		return fs.Sample{}, err
	}
	collector := fs.NewSampleCollector(options, true)
	err = reader.Start(ctx, lcontext.LContext{}, collectorProcessor{collector}, regex.NewNoop())
	if err != nil {
		return fs.Sample{}, err
	}
	// The reader ends without an error when ctx is done.
	if err := ctx.Err(); err != nil {
		return fs.Sample{}, err
	}
	return fs.Sample{Lines: collector.Lines()}, nil
}
