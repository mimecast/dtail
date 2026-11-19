Log Formats
===========

You may have looked at the [DTail Query Language](./querylanguage.md) and wondered how to make DTail understand your own log format(s). If DTail doesn't know your log format, it won't be able to extract much useful information from your logs. This information then can be used as fields (e.g. variables) by the Query Language.

You could either make your application follow the DTail default log format, or you would need to implement a custom log format. Have a look at `./integrationtests/mapr_testdata.log` for an example of a log file in the DTail default format.

## Available log formats

The following log formats are currently available out of the box:

* `default` - The default DTail log format
* `generic` - A generic log format with a simple set of fields
* `generickv` - A simple log format expecting all log lines in the form of `field1=value1|field2=value2|...`
* `csv` - Comma separated CSV with a header on the first line of each file. Fields and header names containing commas or double quotes use standard CSV quoting: surround the field with double quotes and double any quotes inside it. Query outfiles use the same encoding and can be read again with `logformat csv`. Records must fit on one physical line: embedded CR/LF characters are unsupported, malformed quoted input is reported as a parse error, and exporting values with CR/LF fails instead of writing corrupt rows. Append mode writes the header only when the destination is missing or empty.
* `mimecast` and `mimecastgeneric` - Registered built-ins for Mimecast log formats. In the open source build these are stubs that return an error (`the mimecast logformat is not available in this build of DTail`); the real parsers are only compiled in with the proprietary build tag.
* `custom1` and `custom2` - Customizable log formats. Out of the box these are templates returning a "not implemented" error; register your own parsers under these names (see below) to use them.

Selecting a log format whose parser cannot be created — an unregistered name, or the `custom1`/`custom2`/`mimecast` stubs in a build without them — makes `dserver` log an error and fall back to the `generic` parser.

### Selecting a log format

By default, DTail will use the `default` log format. You can override the log format with the `logformat` keyword:

```shell
% dmap --files /var/log/example.log --query 'from EXAMPLE select ....queryhere.... logformat generickv'
```

You can override the default log format with `MapreduceLogFormat` in the Server section of `dtail.json`.

## Under the hood: generickv

As an example, let's have a look at the `generickv` log format's implementation. It's located at `internal/mapr/logformat/generickv.go`:

```go
type genericKVParser struct {
	base defaultParser
}

func newGenericKVParser(hostname, timeZoneName string, timeZoneOffset int) (*genericKVParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericKVParser{}, err
	}
	return &genericKVParser{base: *defaultParser}, nil
}

func (p *genericKVParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	fields := make(map[string]string, p.base.fieldsCapacity)
	if err := p.MakeFieldsInto(fields, maprLine, sourceID); err != nil {
		if errors.Is(err, ErrIgnoreFields) {
			return nil, err
		}
		return fields, err
	}
	return fields, nil
}

func (p *genericKVParser) MakeFieldsInto(dst map[string]string, maprLine, _ string) error {
	clear(dst)
	p.base.addDefaultFields(dst, maprLine)
	start := 0

	for {
		token, next, done := protocol.ScanField(maprLine, start)
		// Generic key-value logs may mix structured and unstructured fields.
		// Ignore malformed fields while continuing to parse later tokens.
		_ = p.base.addKeyValueField(dst, token)
		if done {
			break
		}
		start = next
	}

	return nil
}
```

... whereas:

* `maprLine` is the whole raw log line to be parsed by the log format.
* `sourceID` is the stable identifier of the log file / stream the line came from. Stateful parsers (e.g. CSV with a header row per file) should key their per-file state by this value; stateless parsers may ignore it.
* `protocol.ScanField` scans the next `|`-delimited field of the line. The delimiter itself is an implementation detail of the protocol package (`|` for log fields, `protocol.CSVDelimiter` `,` for CSV fields).
* All field names starting with `$` are variables. They store some custom values.
* All other fields are bareword-fields and are extracted from the log lines directly, e.g. `field1=value1|field2=value2|...`

Two interfaces matter here:

* `Parser` declares `MakeFields(maprLine, sourceID string) (map[string]string, error)` — allocate a field map and parse the line into it.
* `FieldsIntoParser` declares the optional, allocation-free hot-path form `MakeFieldsInto(dst map[string]string, maprLine, sourceID string) error` — fill a map owned by the caller (clear it first). The MapReduce aggregator prefers this form when a parser implements it and falls back to `MakeFields` otherwise.

A parser that keeps per-source state (like the CSV parser keeps header rows) should also implement `SourceReleaser` (`ReleaseSource(sourceID string)`), which the aggregator calls once it has parsed the last line of a source.

## Log format variables

### Common variables:

The common variables may exist in all log formats:

* `$empty` - The empty string `""`
* `$hostname` - The server FQDN
* `$line` - The whole log line
* `$server` - Alias for `$hostname`
* `$timeoffset` -  Offset of $timezone
* `$timezone` -  The current time zone
* `*` - Special placeholder. E.g. sometimes used by the query language to group by everything.

### Default log format variables:

These variables may only exist in the DTail default log format (see `internal/mapr/logformat/default.go` more details):

*Date and time:*

* `$date` - The date in format YYYYMMDD. Only populated for timestamps carrying a year, i.e. the 15 character `YYYYMMDD-HHMMSS` form below.
* `$hour` - The hour in format HH
* `$minute` - The minute in format MM
* `$second` - The second in format SS
* `$time` - The raw timestamp token as it appears in the log line. The default format accepts two timestamp forms: the 15 character `YYYYMMDD-HHMMSS` form (e.g. `20211002-071209`), which populates `$date`, `$hour`, `$minute` and `$second`, and the 11 character `MMDD-HHMMSS` form (e.g. `1002-071143`) that `dserver` stamps onto its own diagnostics lines (see `internal/io/dlog/dlog.go`), which carries no year and therefore populates only `$hour`, `$minute` and `$second` but not `$date`.

*Log level/severity:*

* `$loglevel` - Alias for `$severity`
* `$severity` - The log severity. Note that in practice the default parser only accepts lines whose first field starts with `INFO` (all other lines are ignored with `ErrIgnoreFields`), so on the lines the default parser actually processes, `$severity`/`$loglevel` is always `INFO`.

*System and Go runtime:*

* `$caller` - DTail server caller of the logger
* `$cgocalls` - Num of DTail server CGo calls
* `$cpus` - Num of DTail server CPUs used
* `$goroutines` - Num of DTail server Goroutines used
* `$loadavg` - 1 min. load average
* `$pid` - DTail server process ID
* `$uptime` - DTail server uptime

## Implementing your own log format `Foo`

What needs to be done is to place your own implementation into the `logformat` source directory. As a template, you can copy an existing format ...

```shell
% cp internal/mapr/logformat/generic.go internal/mapr/logformat/foo.go
```

... and replace `generic` with your format's name `foo`:

```go
package logformat

import (
	"errors"

	"github.com/mimecast/dtail/internal/mapr"
	"github.com/mimecast/dtail/internal/protocol"
)

type fooParser struct {
	// Keep the defaultParser in a named field, NOT as an embedded field:
	// embedding it would promote defaultParser.MakeFieldsInto onto
	// fooParser, so the allocation-free path would silently parse every
	// line in DTail's own MAPREDUCE layout instead of yours (see the
	// FieldsIntoParser doc comment in parser.go). With a named field the
	// compiler insists on a MakeFieldsInto of your own.
	base defaultParser
}

func newFooParser(hostname, timeZoneName string, timeZoneOffset int) (*fooParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &fooParser{}, err
	}
	return &fooParser{base: *defaultParser}, nil
}

func (p *fooParser) setQuery(query *mapr.Query) {
	// Lets the base parser populate only the fields the query needs.
	p.base.setQuery(query)
}

func (p *fooParser) MakeFields(maprLine, sourceID string) (map[string]string, error) {
	fields := make(map[string]string, p.base.fieldsCapacity)
	if err := p.MakeFieldsInto(fields, maprLine, sourceID); err != nil {
		if errors.Is(err, ErrIgnoreFields) {
			return nil, err
		}
		return fields, err
	}
	return fields, nil
}

func (p *fooParser) MakeFieldsInto(dst map[string]string, maprLine, _ string) error {
	clear(dst)
	p.base.addDefaultFields(dst, maprLine)
	start := 0

	for {
		token, next, done := protocol.ScanField(maprLine, start)
		..
		<YOUR CUSTOM CODE HERE>
		..
		if done {
			break
		}
		start = next
	}

	return nil
}
```

Populate fields through `p.base.addDefaultFields(...)` (the common `$`-variables), `p.base.addDynamicField(...)`/`p.base.addKeyValueField(...)` (bareword fields, honoring the query's field plan) and `protocol.ScanField` — not by writing raw keys into the map by hand. If your parser ignores a line, return `ErrIgnoreFields`.

Next, the new log format needs to be registered. There is no switch statement to extend: `NewParser` looks parsers up in a registry of parser factories (see `internal/mapr/logformat/parser.go`). The built-in formats are registered there via `registerBuiltInParsers`, but since your file lives in the same package you can simply register yours from an `init` function in `foo.go`:

```go
func init() {
	mustRegisterParser("foo", wrapParserFactory(newFooParser))
}
```

`logformat.RegisterParser("name", factory)` is exported, too, so code outside the `logformat` package can register (or replace) a parser without touching the package at all. That is the intended way to implement the `custom1` and `custom2` template formats in your own code:

```go
func init() {
	logformat.RegisterParser("custom1", func(hostname, timeZoneName string, timeZoneOffset int) (logformat.Parser, error) {
		return newMyCustom1Parser(hostname, timeZoneName, timeZoneOffset)
	})
}
```

Once done, recompile DTail. DTail now understands `... logformat foo` (see "Selecting a log format" above).

## Custom probes

DTail can probe a sample of log lines to find out which tables, fields and `$`-variables a log format extracts from them, e.g. to complete MapReduce queries. A format needs no probe of its own: `logformat.Probe(name, hostname, sample)` builds the format's parser with no query (so it fills all fields) and runs the generic probe `logformat.ProbeWithParser(parser, sample)`, which parses every sampled line with your `MakeFields`/`MakeFieldsInto`, skips lines it ignores (`ErrIgnoreFields`) or fails on, and collects the field names and a few sample values. A newly registered format is therefore discoverable at once. `logformat.Names()` lists every registered format and `logformat.KnownVariables(name)` the `$`-variables a built-in format can populate.

The types involved (`internal/mapr/logformat/probe.go`):

* `ProbeSample` holds the sampled lines (`ProbeLine`: `Line` and `SourceID`), the lines of each source in the order of the source. A stateful format reads it as its parser reads a file: the `csv` probe takes the first line of every source as its header row.
* `ProbeResult` holds `Tables` (table name -> bareword field -> sample values), `Variables` (`$`-variable -> sample values) and an optional `Note` shown with the fields. Formats without tables put their fields under the table `logformat.ProbeAnyTable` (the empty name). `Probe` adds the format's `KnownVariables` the sample did not show, without values, to every non-empty result.
* `ProbeResult.TableVariables` (table -> `$`-variable -> sample values) holds the `$`-variables the lines of each table filled, so that a query `from TABLE` can be offered only those, never the `KnownVariables` the lines did not show. The generic probe fills it for every table of its result, also for a result of one table; once it is not nil, `Probe` gives every table of `Tables` an entry (empty if your `Prober` set none: no variables seen) and drops the entries of tables `Tables` does not list. A `Prober` may leave it nil: the format then has no per-table data and `ProbeResult.VariablesOf(table)` falls back to `Variables`, so results built before the field existed stay valid. `ProbeResult.FilterTable(table)` returns the part about one table, which a table-targeted probe answers.
* `ProbeTabler` is the optional interface `ProbeLineTable(line string) (table string, ok bool)`: it tells a line's table without parsing its fields, cheaply enough to run over every line a probe scans, also those its sample does not keep. A probe adds the tables so found, without fields, to the result (`ProbeTableScanner`), so the table list covers the whole scanned window. A table-targeted probe keeps exactly the lines a `ProbeTabler` assigns to the table (`ProbeTableScanner.Matcher`, names compared cut to `ProbeMaxNameBytes`, as they are listed); lines no `ProbeTabler` claims are kept when they hold the table name as a word of its own (or as the first `ProbeMaxNameBytes` bytes of a longer word, the name such a table is listed under), so a format that names tables only in its `Prober` still gets its table probed, but less precisely: implement `ProbeTabler` if your format has tables. A panic in `ProbeLineTable` is recovered: the format's tabler is not used for the rest of the probe, which reports `probe: log format probe failed` for the format. The `default` format implements it.
* `Prober` is the optional interface `Probe(sample ProbeSample) (ProbeResult, error)`. When the parser your factory returns implements it, `Probe` calls it instead of the generic probe, like the aggregator uses `FieldsIntoParser` and `SourceReleaser` when a parser implements them. There is no second registry: the format's one registration covers its probe.

A custom probe can call `ProbeWithParser(p, sample)` and adjust the result, or replace it, e.g. to name tables, to list a fixed schema without sampling, to hide fields, or to supply better sample values. The built-in `default` format's probe groups the fields by the `MAPREDUCE:<TABLE>` of each line (the table of `from TABLE`; the fields of a line whose table name holds lower-case letters are listed under `ProbeAnyTable` and the table is named in the `Note`, as a query's table name is upper-cased, so no table name selects the line and only a query without a table reads it, `mapr.SelectableTable`; list only tables a query can name in a probe of your own too), and the `csv` probe adds the columns of the header rows, also when no data row was sampled. `internal/mapr/logformat/custom1.go` holds a commented example:

```go
var _ Prober = (*custom1Parser)(nil)

func (p *custom1Parser) Probe(sample ProbeSample) (ProbeResult, error) {
	result, err := ProbeWithParser(p, sample)
	if err != nil {
		return result, err
	}
	if fields, ok := result.Tables[ProbeAnyTable]; ok {
		delete(fields, "secret")
		delete(result.Tables, ProbeAnyTable)
		result.Tables["ORDERS"] = fields
		result.Note = "custom1: order log, one line per order"
	}
	return result, nil
}
```

Rules every probe result follows:

* A format whose parser cannot be built (the `custom1`/`custom2` templates, the `mimecast` stubs of the open source build, unknown names) or, with the generic probe, that parses no line of the sample yields an empty result and no error, so it is simply left out. A format's own probe decides itself: the `csv` probe lists the header columns also when no data line parses, and a result with only a `Note` is not empty. The `csv` probe only probes sources that look like csv files: the first line must parse as one csv record with at least two columns that are neither empty nor longer than `ProbeMaxNameBytes` (only those columns are listed; the others, e.g. after a trailing comma, are left out), at least half of the sampled data rows must have between two columns and the header's number of columns (the parser takes rows with fewer columns than the header), and, once three data rows were sampled (one or two rows repeating a column name are no evidence), no more than half of the sampled data rows may echo the header, i.e. hold, in some column whose name has a letter, a value equal to that name but for the digits of its numbers (a header row names its columns and so differs from its data, while the first line of a free-text log is a line like the others: `request 1 done, ok` before `request 2 done, ok`). Other sources are left out, and a sample without such a source yields an empty result, so a log of another format (e.g. dserver's own `|`-separated log) gets no `csv` section in `:fields` and no csv field names in the Tab completion. This is a heuristic: a csv file of a single column is never probed (`dmap` still queries it), neither is a csv file without a header row most of whose rows repeat a value of the first one (its columns would be named after that row's values), and a free-text log most of whose lines hold a comma, no more commas than its first line, is probed as csv all the same when in at least half of its lines every text between the commas differs from the first line's by more than numbers (`user bob logged in, from berlin` before `user alice logged out, from paris`).
* `Probe` cuts every result, also one your `Prober` built, to the caps: 64 tables (`ProbeMaxTables`), 256 fields per table and 256 variables (`ProbeMaxFieldsPerTable`), 5 distinct sample values per field (`ProbeMaxValuesPerField`), 64 bytes per value at a UTF-8 character boundary (`ProbeMaxValueBytes`), 64 bytes per table, field and variable name, cut the same way (`ProbeMaxNameBytes`; names equal after the cut share one entry) and 256 bytes of note (`ProbeMaxNoteBytes`). The generic probe keeps the first tables and values it sees and, of a line's fields that no longer fit, the first in sorted order, so the same sample always yields the same result; of a `Prober`'s result the first tables, fields and variables in sorted order are kept.
* An error is returned only when a `Prober` returns one or the parser panics during the probe (the panic is recovered).
* Your parser factory must return a new parser on every call, as the built-in ones do: `Probe` builds a parser for the probe alone and releases the sample's sources on it (`SourceReleaser`) afterwards, which would hit the sources of a parser shared with a running query.
* Embedding another parser promotes its `Probe` (e.g. the default format's grouping by table) and `ProbeLineTable` onto your type, the same hazard as for `MakeFieldsInto`: hold a reused parser in a named field.
* The variables of a table (`TableVariables`) are capped like its fields (256), and only kept for tables the capped `Tables` list.
