DTail Query Language
====================

The query language allows you to run mapreduce queries on log files. This page is the reference to the language.

## Prerequisites

For this to work, DTail needs to understand your log format. DTail already understands its own log format. You can have a look at all examples of the [examples](./examples.md) page using `-query` (these would be all examples of the `dmap` command, and some examples using the `dtail` command).

DTail also ships with a generic log format, which only allows you to run very basic queries. Check out the [log format](./logformats.md) documentation for this. That page also documents how to implement your own log format parser.

## The language

This are the fundamental types of the query language:

```shell
NUMBER := A whole number (e.g. 42)
FLOAT := A float number, e.g. 3.14
STRING := A quoted string, e.g. "foo"
FIELD := BAREWORD|$VARIABLE
BAREWORD := A bare string without quotes, e.g. foo. This usually contains a value
            extracted from a log line.
$VARIABLE := Like a bareword, but with a $ prefix, e.g. $foo. This usually contains
            a special value set by DTail itself (not necessary from the log line).
```

This is the overall structure of a query:

```shell
QUERY := select SELECT1[,SELECT2...]
         [from TABLE]
         [where CONDITION1[,CONDITION2...]]
         [group by FIELD1[,FIELD2...]]
         [order|rorder by ORDERFIELD]
         [set SET1,[,SET2...]]
         [interval NUMBER]
         [limit NUMBER]
         [outfile [append] STRING]
         [logformat LOGFORMAT]
```

... whereas:

```shell
TABLE := The mapreduce table name, e.g. STATS in MAPREDUCE:STATS
SELECT := FIELD|AGGREGATION(FIELD)
CONDITION := ARG1 OPERATOR ARG2
ARG := FIELD|FLOAT|STRING
OPERATOR := FLOATOPERATOR|STRINGOPERATOR
FLOATOPERATOR := One of: == != < <= > >=
STRINGOPERATOR := eq|ne|contains|ncontains|lacks|hasprefix|nhasprefix|hassuffix|nhassuffix
ORDERFIELD := FIELD|AGGREGATION(FIELD)
SET := $VARIABLE = FLOAT|STRING|FIELD|FUNCTION(FIELD)
LOGFORMAT := default|generic|generickv|...
AGGREGATION := count|sum|min|max|avg|last|len|percentage|percentile
FUNCTION := md5sum|maskdigits
```

*Notes:*

* `rorder` stands for reverse order.
* `lacks` is an alias for `ncontains` (not contains).
* Available fields (variables and barewords) vary from the log format used. Check out the [log format](./logformats.md) documentation for more information.
* `percentage(field)` returns the selected group's share of the total for that field across all groups. For non-negative inputs, the result is between 0 and 100; with mixed positive and negative values, it can fall outside that range.
* `percentile(field)` returns the percentile rank of the selected group's value among all grouped values for that field, also expressed as a value between 0 and 100. Equal values share the same rank.

## Selecting the log format and dynamic fields

Two things commonly trip people up when a `$field`-style reference "does not
resolve" while positional/built-in fields work. Both are by design:

1. **Dynamic `key=value` fields are barewords, not `$`-variables.** A log line
   like `...|service=web|bytes=100` exposes `service` and `bytes` as *barewords*.
   Query them as `select service,sum(bytes)` — **not** `$service`/`$bytes`. The
   `$` prefix is reserved for values DTail sets itself (e.g. `$time`, `$hostname`,
   `$line`). A `$name` that is not one of those built-ins silently resolves to
   the empty string, which is exactly what "did not resolve" looks like:
   everything collapses into a single empty group. To catch this early, the
   client prints a plan-time warning to stderr for every `$`-variable the
   selected parser cannot populate, e.g.
   `warning: $service is not a known variable for log format "default"; did you
   mean bareword service?`. It is only a warning (resolution behaviour is
   unchanged), and it is never emitted for barewords, for built-ins like
   `$empty`, or for variables defined via a `set` clause.

2. **The `from TABLE` clause selects the rich parser.** Although `from TABLE` is
   written as optional in the grammar above, omitting it (and not passing an
   explicit `logformat`) downgrades the query to the `generic` log format, which
   exposes only the common variables (`$line`, `$hostname`, ...) and **no**
   dynamic `key=value` fields and **no** default-format `$`-variables such as
   `$time`. To query DTail's own default-format logs (lines containing
   `MAPREDUCE:STATS`), use `from STATS`, for example:

   ```shell
   % dmap --files /var/log/dserver/dserver.log \
       --query 'from STATS select $hostname,max($goroutines),lifetimeConnections group by $hostname'
   ```

   Alternatively, name the parser explicitly with the `logformat` keyword (e.g.
   `logformat generickv`), which works regardless of the `from` clause. See the
   [log formats](./logformats.md) documentation for details.
