package handlers

import (
	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// Bounds of a table-targeted probe (protocol.ProbeRequest.Table), which any
// authenticated session user can send. It goes through the same read
// permission check, protocol.ProbeMaxFiles cap, probeTimeout and one probe
// per session (probeRunning, probeInFlight) as a plain probe, takes no cat
// slot either, and keeps as few lines (fs.DefaultSampleMaxLines, 256 KiB of
// line data per file). What it reads is larger, so that a table logging
// rarely is found:
//   - probeTableScanBytes (4 MiB, 16 plain windows) of the end of every
//     uncompressed file, and as much decompressed input of the beginning of
//     a gzip or zstd file. At most protocol.ProbeMaxFiles files, so 32 MiB of
//     reads per probe at most; the window is streamed (64 KiB read buffer
//     plus the line scanner's), so memory does not grow with it. A table
//     that logs one line per 20,000 lines of about 200 bytes still shows up
//     about once per file. A sample that runs out of probeTimeout is
//     stopped and reported like a plain one's.
//   - probeTableScanLines (5,000, 10 times a plain probe) journal entries of
//     a journal target, of which the lines naming the table are kept.
//
// The lines kept are those of the table (logformat.ProbeTableScanner.Matcher):
// a line a log format's ProbeTabler (the default format's MAPREDUCE:<TABLE>)
// assigns to the table, or, for a line no ProbeTabler claims, one naming the
// table as a word of its own (formats that name tables without ProbeTabler);
// the answer holds the requested table only (logformat.ProbeResult.FilterTable).
const (
	probeTableScanBytes = 4 << 20
	probeTableScanLines = 5000
)

// sampleOptions returns the sample bounds of a probe: those of a
// table-targeted one, keeping the lines of the table (tables.Matcher), for
// a table, else the plain defaults with tables observing every scanned line
// (logformat.ProbeTableScanner.Scan), so that the table list of the answer
// is as complete as the plain window allows.
func (p *prober) sampleOptions(table string, tables *logformat.ProbeTableScanner) fs.SampleOptions {
	options := fs.SampleOptions{MaxLineLength: p.maxLineLength}
	if table == "" {
		options.Observe = tables.Scan
		return options
	}
	options.ScanBytes = probeTableScanBytes
	options.ScanLines = probeTableScanLines
	options.Keep = tables.Matcher(table)
	return options
}

// tablerFailures reports every log format whose ProbeTabler panicked while
// the probe scanned or filtered lines like a failed log format probe
// (protocol.ProbeErrorFormat, logged at warn level): the panic is recovered
// and the format's tabler is not used for the rest of the probe, so a
// broken custom format neither ends the session nor hides its failure.
func (r *probeRun) tablerFailures(tables *logformat.ProbeTableScanner) {
	failed := tables.Failed()
	for _, name := range sortedKeys(failed) {
		r.logger.Warn(r.logContext, "Probe: log format probe failed", name, failed[name])
		r.addError(name, protocol.ProbeErrorFormat)
	}
}

// formatResult returns what the answer holds of a format's probe result:
// for a table-targeted probe that table only (false when the result does
// not list it), else the result with the tables scanned in the window
// added (false when it is empty).
func formatResult(name, table string, result logformat.ProbeResult,
	tables *logformat.ProbeTableScanner) (protocol.ProbeFormat, bool) {

	if table != "" {
		var ok bool
		if result, ok = result.FilterTable(table); !ok {
			return protocol.ProbeFormat{}, false
		}
	} else if result = tables.AddTables(name, result); result.Empty() {
		return protocol.ProbeFormat{}, false
	}
	return protocol.ProbeFormat{
		Tables:         result.Tables,
		TableVariables: result.TableVariables,
		Variables:      result.Variables,
		Note:           result.Note,
	}, true
}
