Log Formats
===========

You may have looked at the [DTail Query Language](./querylanguage.md) and wondered how to make DTail understand your own log formats. Otherwise, DTail won't be able to extract information from your logs (e.g. extract fields and variables from your log lines to be used in the query language).

You could either make your application follow the DTail default log format, or you would need to implement a custom log format in Go.

## Current log formats

The following log formats are currently available out of the box:

* `default` - The default DTail log format.
* `generic` - A generic log format with a very simple set of fields.
* `generickv` - A simple log format expecting all log lines in form of `field1=value1|field2=value2|...`.

For details, have a look at the implementations at `./internal/mapr/logformat/`.

### Selecting a log format

By default, DTail will use the `default` log format. You can override the log format with the `logformat` keyword:

```shell
% dmap --files /var/log/example.log --query 'from EXAMPLE select ....queryhere.... logformat generickv'
```

Alternatively, you can override the default log format via `MapreduceLogFormat` in the Server section of `dtail.json`.

## Log format fields

TODO: Difference between field and variables.

## Log format variables

This is the list of pre-defined variables. Please note that these vary depending on the log format used. 

### Common variables:

The common variables may exist in all log formats.

* `$empty` - The empty string `""`
* `$hostname` - The server FQDN
* `$line` - The current log line
* `$server` - Alias for `$hostname`
* `$timeoffset` -  Offset of $timezone
* `$timezone` -  The current time zone
* `*` - Special placeholder 

### Default log format variables:

These variables may only exist when your logs are in the DTail default log format:

*Date and time:*

* `$hour` - The current hour in format HH
* `$minute` - The current minute in format MM
* `$second` - The current second in format SS.
* `$time` - The current time in format YYYYMMDD-HHMMSS

*Log level/severity:*

* `$loglevel` - Alias for `$severity`
* `$severity` - The log severity

*System and Go runtime:*

* `$caller` - DTail server caller of the logger
* `$cgocalls` - Num of DTail server CGo calls
* `$cpus` - Num of DTail server CPUs used
* `$goroutines` - Num of DTail server Goroutines used
* `$loadavg` - 1 min. average load average
* `$pid` - DTail server process ID
* `$uptime` - DTail server uptime
