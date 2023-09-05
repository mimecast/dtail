Log Formats
===========

You may have looked at the [DTail Query Language](./querylanguage.md) and wondered how to make DTail understand your own log format(s). If DTail doesn't know your log format, it won't be able to extract much useful information from your logs. This information then can be used as fields (e.g. variables) by the Query Language.

You could either make your application follow the DTail default log format, or you would need to implement a custom log format. Have a look at `./integrationtests/mapr_testdata.log` for an example a log file in the DTail default format.

## Available log formats

The following log formats are currently available out of the box:

* `default` - The default DTail log format
* `generic` - A generic log format with a very simple set of fields
* `generickv` - A simple log format expecting all log lines in form of `field1=value1|field2=value2|...`

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
	defaultParser
}

func newGenericKVParser(hostname, timeZoneName string, timeZoneOffset int) (*genericKVParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &genericKVParser{}, err
	}
	return &genericKVParser{defaultParser: *defaultParser}, nil
}

func (p *genericKVParser) MakeFields(maprLine string) (map[string]string, error) {
	splitted := strings.Split(maprLine, protocol.FieldDelimiter)
	fields := make(map[string]string, len(splitted))

	fields["*"] = "*"
	fields["$line"] = maprLine
	fields["$empty"] = ""
	fields["$hostname"] = p.hostname
	fields["$server"] = p.hostname
	fields["$timezone"] = p.timeZoneName
	fields["$timeoffset"] = p.timeZoneOffset

	for _, kv := range splitted[0:] {
		keyAndValue := strings.SplitN(kv, "=", 2)
		if len(keyAndValue) != 2 {
			//dlog.Common.Debug("Unable to parse key-value token, ignoring it", kv)
			continue
		}
		fields[keyAndValue[0]] = keyAndValue[1]
	}

	return fields, nil
}
```

... whereas:

* `maprLine` is the whole raw log line to be parsed by the log format.
* `protocol.FieldDelimiter` is the field delimiter used by the log format, here: `|`.
* All field names starting with `$` are variables. They store some custom values.
* All other fields are bareword-fields and are extracted from the log lines directly, e.g. `field1=value1|field2=value2|...`

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

* `$hour` - The hour in format HH
* `$minute` - The minute in format MM
* `$second` - The second in format SS.
* `$time` - The time in format YYYYMMDD-HHMMSS

*Log level/severity:*

* `$loglevel` - Alias for `$severity`
* `$severity` - The log severity, one of `FATAL`, `ERROR`, `WARN`, `INFO`, `VERBOSE`, `DEBUG`, `DEVEL`, `TRACE`

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

... and replace `generic` ` with your format's name `foo`:

```go
package logformat

type fooParser struct {
	defaultParser
}

func newFooParser(hostname, timeZoneName string, timeZoneOffset int) (*fooParser, error) {
	defaultParser, err := newDefaultParser(hostname, timeZoneName, timeZoneOffset)
	if err != nil {
		return &fooParser{}, err
	}
	return &fooParser{defaultParser: *defaultParser}, nil
}

func (p *fooParser) MakeFields(maprLine string) (map[string]string, error) {
	fields := make(map[string]string, 3)

	..
	<YOUR CUSTOM CODE HERE>
	..

	return fields, nil
}
```

Once done, recompile DTail. DTail now understands `... logformat foo` (see "Seleting a log format" above).
