package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"

	"github.com/mimecast/dtail/internal/io/fs"
	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/mapr/logformat"
	"github.com/mimecast/dtail/internal/protocol"
)

// run answers request: a path listing (request.List set, runList), or else
// it resolves and samples its files and probes the sample with every
// registered log format (logformat.Names), only for request.Table if set
// (a table-targeted probe, probe_table.go). It never fails: whatever goes
// wrong is an entry of the answer's Errors.
func (p *prober) run(ctx context.Context, request protocol.ProbeRequest) protocol.ProbeAnswer {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if request.List != "" {
		return p.runList(ctx, request)
	}
	if request.Table != "" && !protocol.ValidProbeTable(request.Table) {
		return probeErrorAnswer(request.ID, protocol.ProbeErrorInvalidRequest)
	}

	r := newProbeRun(p)
	answer := protocol.ProbeAnswer{
		ID:               request.ID,
		DefaultLogFormat: (*mapr.Query)(nil).EffectiveLogFormat(p.defaultLogFormat),
		Table:            request.Table,
	}
	if len(request.Files) == 0 {
		r.addError("", protocol.ProbeErrorInvalidRequest)
	}
	names := logformat.Names()
	tables := logformat.NewProbeTableScanner(names, p.hostname)
	sample, files, lines := r.sampleTargets(ctx, r.resolveTargets(request.Files),
		p.sampleOptions(request.Table, tables))
	answer.FilesSampled = files
	answer.LinesSampled = lines
	if files > 0 {
		answer.Formats = r.probeFormats(sample, names, request.Table, tables)
	}
	r.tablerFailures(tables)
	answer.Errors = r.errors
	return answer
}

// sampleTargets samples every target with options and returns the lines of
// all of them, each with its path as source id, the number of targets
// sampled and the number of sampled lines. A tail sample's FirstLine is put
// in front of its lines: it is a line of the same file, and header formats
// (csv) take a source's first line as its header. It is not counted as a
// sampled line, so the count is the sampler's (at most 500 lines per file).
func (r *probeRun) sampleTargets(ctx context.Context, targets []probeTarget,
	options fs.SampleOptions) (logformat.ProbeSample, int, int) {

	var sample logformat.ProbeSample
	sampled, lineCount := 0, 0
	for _, target := range targets {
		if ctx.Err() != nil {
			r.addError("", protocol.ProbeErrorTimeout)
			break
		}
		lines, err := r.sample(ctx, target.target, target.path, options)
		if err != nil {
			r.logger.Warn(r.logContext, "Probe: unable to sample file", target.path, err)
			r.addError(target.entry, probeSampleReason(ctx, err))
			continue
		}
		sampled++
		lineCount += len(lines.Lines)
		if lines.HasFirstLine {
			sample.Lines = append(sample.Lines, logformat.ProbeLine{Line: lines.FirstLine, SourceID: target.path})
		}
		for _, line := range lines.Lines {
			sample.Lines = append(sample.Lines, logformat.ProbeLine{Line: line, SourceID: target.path})
		}
	}
	return sample, sampled, lineCount
}

// probeSampleReason is the reason reported for a failed sample. Like every
// probe error it names the request entry, not the file a glob matched; the
// path is in the server log.
func probeSampleReason(ctx context.Context, err error) string {
	if ctx.Err() != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		return protocol.ProbeErrorTimeout
	}
	return protocol.CommandFailureReadingFile
}

// probeFormats probes sample with every log format of names and returns
// what the answer holds of each (formatResult): the non-empty results, with
// the tables scanned in the window added, or for a table-targeted probe
// (table set) the results listing that table, cut to it. A format whose
// probe failed is reported by name.
func (r *probeRun) probeFormats(sample logformat.ProbeSample, names []string, table string,
	tables *logformat.ProbeTableScanner) map[string]protocol.ProbeFormat {

	formats := make(map[string]protocol.ProbeFormat)
	for _, name := range names {
		result, err := logformat.Probe(name, r.hostname, sample)
		if err != nil {
			r.logger.Warn(r.logContext, "Probe: log format probe failed", name, err)
			r.addError(name, protocol.ProbeErrorFormat)
			continue
		}
		if format, ok := formatResult(name, table, result, tables); ok {
			formats[name] = format
		}
	}
	return formats
}

// marshalProbeAnswer returns the JSON of answer, at most maxBytes long when
// that can be had by leaving out sample values and fields: first every sample
// value goes, then as many tables, fields and variables as needed, those of
// the default log format last (probeItems). The answer of a path listing
// loses its last entries instead (marshalFittingEntries). Either sets
// answer.Truncated. Only the errors and counts are always kept; they are
// small (at most protocol.ProbeMaxFiles entries, cut to
// probeMaxErrorFileBytes).
func marshalProbeAnswer(answer protocol.ProbeAnswer, maxBytes int) ([]byte, error) {
	data, err := json.Marshal(answer)
	if err != nil || len(data) <= maxBytes {
		return data, err
	}
	answer.Truncated = true
	if len(answer.Entries) > 0 {
		return marshalFittingEntries(answer, maxBytes)
	}
	formats := withoutSampleValues(answer.Formats)
	answer.Formats = formats
	if data, err = json.Marshal(answer); err != nil || len(data) <= maxBytes {
		return data, err
	}
	return marshalFittingItems(answer, formats, maxBytes)
}

// marshalFittingItems searches for the largest number of items
// (probeItems) of formats whose answer fits maxBytes and returns that
// answer's JSON. Answers grow with the number of items, so a binary search
// needs about log2(items) encodings.
func marshalFittingItems(answer protocol.ProbeAnswer, formats map[string]protocol.ProbeFormat,
	maxBytes int) ([]byte, error) {

	items := probeItems(formats, answer.DefaultLogFormat)
	low, high := 0, len(items)-1 // the answer with all items does not fit
	answer.Formats = keepProbeItems(items, 0)
	best, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	for low < high {
		mid := (low + high + 1) / 2
		answer.Formats = keepProbeItems(items, mid)
		data, err := json.Marshal(answer)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxBytes {
			low, best = mid, data
		} else {
			high = mid - 1
		}
	}
	return best, nil
}

// marshalFittingEntries searches for the largest number of the first
// entries of a path listing answer that fits maxBytes and returns that
// answer's JSON, like marshalFittingItems.
func marshalFittingEntries(answer protocol.ProbeAnswer, maxBytes int) ([]byte, error) {
	entries := answer.Entries
	low, high := 0, len(entries)-1 // the answer with all entries does not fit
	answer.Entries = nil
	best, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	for low < high {
		mid := (low + high + 1) / 2
		answer.Entries = entries[:mid]
		data, err := json.Marshal(answer)
		if err != nil {
			return nil, err
		}
		if len(data) <= maxBytes {
			low, best = mid, data
		} else {
			high = mid - 1
		}
	}
	return best, nil
}

// probeItem is one table, field, table variable or variable of a format.
type probeItem struct {
	format, table, field, variable string
	kind                           probeItemKind
	// perTable marks the table item of a format with per-table data
	// (TableVariables not nil): a kept table then keeps an entry there,
	// empty if all its variables were cut.
	perTable bool
}

type probeItemKind int

const (
	probeItemTable probeItemKind = iota
	probeItemField
	probeItemTableVariable
	probeItemVariable
)

// probeItems lists the tables (each followed by its fields and then its
// TableVariables) and variables of formats, the default log format first,
// then the others by name, and tables, fields and variables sorted, so the
// items kept of a cut answer do not depend on map order. A table's
// variables follow the table, so no cut keeps them without it; those of a
// table Tables does not list are left out.
func probeItems(formats map[string]protocol.ProbeFormat, defaultFormat string) []probeItem {
	names := sortedKeys(formats)
	if i := slices.Index(names, defaultFormat); i > 0 {
		names = slices.Insert(slices.Delete(names, i, i+1), 0, defaultFormat)
	}
	var items []probeItem
	for _, name := range names {
		format := formats[name]
		for _, table := range sortedKeys(format.Tables) {
			items = append(items, probeItem{format: name, table: table, kind: probeItemTable,
				perTable: format.TableVariables != nil})
			for _, field := range sortedKeys(format.Tables[table]) {
				items = append(items, probeItem{format: name, table: table, field: field, kind: probeItemField})
			}
			for _, variable := range sortedKeys(format.TableVariables[table]) {
				items = append(items, probeItem{format: name, table: table, variable: variable,
					kind: probeItemTableVariable})
			}
		}
		for _, variable := range sortedKeys(format.Variables) {
			items = append(items, probeItem{format: name, variable: variable, kind: probeItemVariable})
		}
	}
	return items
}

// keepProbeItems builds the formats of the first n items, without sample
// values (and without notes, which are cut with the values).
func keepProbeItems(items []probeItem, n int) map[string]protocol.ProbeFormat {
	formats := make(map[string]protocol.ProbeFormat)
	for _, item := range items[:n] {
		format := formats[item.format]
		switch item.kind {
		case probeItemTable:
			if format.Tables == nil {
				format.Tables = make(map[string]map[string][]string)
			}
			format.Tables[item.table] = make(map[string][]string)
			if item.perTable {
				keptTableVariables(&format, item.table)
			}
		case probeItemField:
			format.Tables[item.table][item.field] = []string{}
		case probeItemTableVariable:
			keptTableVariables(&format, item.table)[item.variable] = []string{}
		case probeItemVariable:
			if format.Variables == nil {
				format.Variables = make(map[string][]string)
			}
			format.Variables[item.variable] = []string{}
		}
		formats[item.format] = format
	}
	return formats
}

// keptTableVariables returns the TableVariables entry of table in format,
// creating it (and the map) when missing.
func keptTableVariables(format *protocol.ProbeFormat, table string) map[string][]string {
	if format.TableVariables == nil {
		format.TableVariables = make(map[string]map[string][]string)
	}
	if format.TableVariables[table] == nil {
		format.TableVariables[table] = make(map[string][]string)
	}
	return format.TableVariables[table]
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// withoutSampleValues returns a copy of formats with every field and
// variable kept but no sample values and no notes.
func withoutSampleValues(formats map[string]protocol.ProbeFormat) map[string]protocol.ProbeFormat {
	items := probeItems(formats, "")
	return keepProbeItems(items, len(items))
}
