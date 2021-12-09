DTail Query Language
====================

The query language allows you to run mapreduce queries on log files. This page intends to be a reference to the language.

## Prerequisites

For this to work, DTail needs to understand your log format. DTail already understands its own log format. You can have a look at all examples of the [examples](./examples.md) page using `-query` (these would be all examples of the `dmap` command, and some examples using the `dtail` command).

DTail also ships with a generic log format, which only allows you to run very basic queries. Check out the [log formats](./logformats.md) documentation for this.

To implement your own log format, please also check out the [log formats](./logformats.md) documentation.

## The complete language

```shell
QUERY :=
    select SELECT1[,SELECT2...]
    from TABLE
    [where COND1[,COND2...]]
    [group by GROUPFIELD1[,GROUPFIELD2...]]
    [order|rorder by ORDERFIELD]
    [interval SECONDS]
    [limit NUM]
    [outfile "FILENAME.csv"]
SELECT := FIELD|AGGREGATION(FIELD)
TABLE := The mapreduce table name, e.g. WRITE in MAPREDUCE:WRITE
AGGREGATION := count|sum|min|max|avg|last|len
COND := ARG1 OPERATOR ARG2
ARG := This is either
    a string: "foo bar"
    a float number: 3.14
    a bareword e.g.: responsecode
    or a $variable (see below).
OPERATOR := This is one of ...
    Floating point operators:
        == != < <= > >=
    String operators:
        eq ne contains lacks (lacks is the opposite of contains, e.g. 
                              "not contains")
GROUPFIELD := bareword|$variable       
ORDERFIELD := This must be a AGGREGATION(FIELD) or FIELD which was specified in
              select clause already.
```

## Predefined variables

This is the list of pre-defined variables. Please note that these vary depending on the log format used. 

### Common variables:

The common variables may exist in all log formats.

* `$empty` - The empty string `""`
* `$hostname` - The server FQDN
* `$line` - The current log line
* `$server` - Alias for `$hostname`
* `$timeoffset` -  Offset of $timezone
* `$timezone` -  The current time zone
* `* (special placeholder)

### DTail default log format:

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
