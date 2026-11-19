Log Formats
===========

You may have looked at the [DTail Query Language](./querylanguage.md) and wondered how to make DTail understand your own log format(s). If DTail doesn't know your log format, it won't be able to extract much useful information from your logs. This information then can be used as fields (e.g. variables) by the Query Language.

You could either make your application follow the DTail default log format, or you would need to implement a custom log format. Have a look at `./integrationtests/mapr_testdata.log` for an example of a log file in the DTail default format.

## Available log formats

The following log formats are currently available out of the box:

* `default` - The default DTail log format
* `generic` - A generic log format with a simple set of fields
* `generickv` - A simple log format expecting all log lines in the form of `field1=value1|field2=value2|...`
* `csv` - A simple CSV format expecting all files to be comma separated CSV files. The first line of the file must be the CSV header.
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