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

A query is a sequence of clauses. Each clause starts with one of the clause
keywords below and extends up to the next clause keyword, so the clauses can be
written in **any order**. All of them are optional except `select`:

```shell
QUERY := CLAUSE1 [CLAUSE2 ...]
CLAUSE := select SELECT1[,SELECT2...]
          | from TABLE
          | where CONDITION1[,CONDITION2...]
          | group [by] FIELD1[,FIELD2...]
          | order [by] ORDERFIELD
          | rorder [by] ORDERFIELD
          | set SET1[,SET2...]
          | interval NUMBER
          | limit NUMBER
          | outfile [append] STRING
          | logformat LOGFORMAT
```

... whereas:

```shell
TABLE := The mapreduce table name, e.g. STATS in MAPREDUCE:STATS (see "Special
         table names" below)
SELECT := FIELD|AGGREGATION(FIELD)
CONDITION := ARG1 OPERATOR ARG2
ARG := FIELD|FLOAT|STRING
OPERATOR := FLOATOPERATOR|STRINGOPERATOR
FLOATOPERATOR := One of: == != < <= =< > >= =>
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
* The float operators `=<` and `=>` are accepted aliases for `<=` and `>=`.
* The `by` in `group by`, `order by` and `rorder by` is optional: `group $foo`
  and `group by $foo` mean the same.
* Where conditions are separated by commas or by the keyword `and`
  (`where $a eq 1 and $b ne 2` is the same as `where $a eq 1, $b ne 2`), and
  they are AND-ed together: a line must satisfy every condition to be counted.
* A bare number as an argument of a string operator is a literal:
  `where $a eq 1` compares the value of `$a` with the string `1`. A field
  that is named like a number is given in backticks (`` `1` ``); any other
  bareword is a field name and a quoted argument is a string.
* `limit` must not be negative, `interval` must be a non-negative number of
  seconds, and nothing may follow the closing parenthesis of an aggregation
  (`count(foo)bar` is rejected).
* Values that are not finite numbers (`NaN`, `Inf`) are not aggregated by
  `sum`, `avg`, `min`, `max`, `percentage` and `percentile`; they are logged
  as aggregation errors like any other non-number.
* The character `∥` (U+2225) separates the parts of a result on the wire. In
  group keys and in `last()`/`len()` values (plain select fields included) it
  is replaced by `||`.
* If no `group by` clause is given, DTail implicitly groups by the first select
  field. For `select count(*)` that field is `*`, i.e. everything collapses
  into a single group.
* Multiple group fields form a tuple: `("x,y", "z")` and `("x", "y,z")`
  are distinct groups. Compound keys escape commas and percent signs internally;
  selected values are unchanged. Old clients can consume these opaque keys,
  but queries grouping values containing commas or percent signs require all
  participating dservers to have this fix. Older servers can merge distinct
  tuples or emit different keys for the same tuple. Ordinary compound keys and
  single-field keys retain their previous spelling. Result ordering remains
  deterministic; escaped compound keys can change the order of tied rows.
* The field given to `order by`/`rorder by` must also appear in the `select`
  clause, otherwise the query fails to parse with an error.
* Each `select` field may appear only once: a query such as
  `select count($line),count($line)` (or `select $foo,$foo`) fails to parse
  with the error `'select' clause has 'count($line)' more than once`. Every
  select field is stored, sent over the wire and printed as a result (and CSV
  outfile) column under its text as written, so two equal fields would share
  one value, and each of them used to add every line to it again: for
  `count(lat),count(lat)` dmap printed 8 times the real count in both
  columns, `sum` was inflated the same way and `percentage` came out too low
  (`avg`, `min`, `max`, `last` and `len` were not affected). For the same
  reason a backtick-quoted field named like an aggregation
  (`` `count($foo)` ``, see "Escaping keywords") cannot be selected together
  with that aggregation (`count($foo)`). Different aggregations of one field
  (`count($foo),sum($foo),avg($foo)`) are fine. Both the client (before it
  connects to any server) and dserver reject such a query:
  * A current client rejects it whatever version the dservers have, so it
    never sends the query; a scheduled or continuous job with such a query
    logs the error instead of starting and writes no outfile.
  * An older client does not reject it and sends it to the dservers. An
    older dserver still aggregates the query with the wrong numbers
    described above. A current dserver answers with the parse error and
    sends no results and no log lines, so that server contributes no rows
    instead of wrong numbers:
    * With the plain command stream (`dmap`, `dtail --query`, continuous and
      scheduled jobs), the dserver reports the failed map command (hidden
      reason `map: invalid query`), rejects the session's read commands
      (each with an error line and the same reason) and ends the session
      with its normal close handshake. An older `dmap` writes an outfile
      without rows from that server (header-only when every server is a
      current dserver) and exits 0; an older `dtail --query` reconnects and
      shows the error again on every reconnect, without any log lines.
    * With `--interactive-query` (`SESSION START`), the dserver rejects the
      session with `.syn session err invalid session spec: <parse error>`,
      which the client shows as `session request rejected: invalid session
      spec: ...` (older dservers sent no parse error after `invalid session
      spec`). It does not fall back to the plain command stream and streams
      nothing; `dtail` reconnects and shows the error again. A `:reload` with
      such a query is rejected with the same error and the running query
      keeps running.
    * A scheduled job runs the client of the dserver scheduling it. If that
      dserver is older but has capability `command-failure-v1`, the
      `map: invalid query` failure is a transport failure (see "Failed
      scheduled jobs" in `AGENTS.md`): the job writes no outfile, is backed
      off, and its final runs after the TimeRange fail the same way, so the
      scheduler gives up on the outfile 24 hours after the TimeRange ended,
      for every date period. If the scheduling dserver predates
      `command-failure-v1`, it only sees a completed session and writes an
      outfile without rows from the current dservers (header-only when all
      of them are current, else only the wrong rows of older dservers).
* If no `interval` clause is given, results are aggregated every 5 seconds.
* An aggregation only sees the lines of a group that have its field (sparse
  `key=value` fields, a `where` clause not requiring it). The client merges
  the results it receives field by field, and results whose lines of the
  group all lack a field leave that field's value as it is. Which results
  are merged depends on whether the query is cumulative:
  * Cumulative (`dmap`, `dtail --query` with an `outfile`, scheduled jobs):
    the results of all servers and of all intervals so far are merged, so a
    server or interval without the field keeps the value earlier ones gave.
  * Non-cumulative (`dtail --query` without an `outfile`, continuous jobs):
    each result starts from zero and its rows show the lines received since
    the previous result, merged across the servers. A group whose lines
    received in that time all lack a field shows that field as if no line
    had it (e.g. `min` `0`), whatever earlier results had. Lines can arrive
    one result late, so a row is not strictly one interval's lines: a
    dserver puts the groups of an interrupted send back into its next
    interval, lines a dserver still batches are aggregated with a later
    interval, and a server's result the client could not merge at once
    (the global result was busy) joins the global result with that server's
    next message.

  This holds fully only when every dserver is up to date. An older dserver
  whose sending of results was interrupted (its session context cancelled
  while a send was blocked) puts the unsent results back with the old merge
  rules, so the results it sends after that can carry `0` for `min`/`max`
  and zero-valued fields no line had.

  Per function:
  * `count`, `sum`, `min` and `max` only use the lines that have the field;
    a field no line of the group had shows as `0`.
  * `last` and `len` keep the most recently received value (`len` its
    length); a field no line of the group had shows as empty (`last`) or `0`
    (`len`).
  * `avg` divides the sum of the field's values by the number of those
    values. Only the lines of the group that have the field with a numeric
    value count: a line lacking the field, or whose value is no number
    (logged as an aggregation error), does not lower the average. So
    `select avg(bytes),count(status)` over the three lines
    `bytes=10|status=ok`, `status=ok` and `bytes=abc|status=ok` of a group
    gives `10`; a group without any numeric value of the field shows `0`.
    A current dserver sends the number of values with its sum. An older
    dserver sends none, and its results are then averaged as before:
    divided by the number of lines of the group with at least one selected
    field whose value could be aggregated (the example above gives
    `3.333333` from such a server). An older client ignores the number and
    also divides that way.
  * `percentage` adds nothing to the total for a group without the field,
    and that group shows `0`.
  * `percentile` ranks only the groups that have the field; a group without
    it takes no part in the ranking (so it does not shift the other groups'
    ranks) and shows `0`.
* Available fields (variables and barewords) vary from the log format used. Check out the [log format](./logformats.md) documentation for more information.
* `percentage(field)` returns the selected group's share of the total for that field across all groups. For non-negative inputs, the result is between 0 and 100; with mixed positive and negative values, it can fall outside that range.
* `percentile(field)` returns the percentile rank of the selected group's value among the values of that field of all groups that have it, also expressed as a value between 0 and 100. Equal values share the same rank.

### Escaping keywords

Backticks around a token make it a literal field name instead of interpreting
it. For example, `select` with an escaped aggregation expression:

```shell
% dmap --files example.log --query 'select `count($foo)` group by `count($foo)`'
```

This selects the field literally named `count($foo)` (using the `last`
aggregation to pick its value) instead of counting the occurrences of `$foo`.
This also allows using a clause keyword itself as a field name. Such a field
cannot be selected together with the aggregation it is named like
(`` select `count($foo)`,count($foo) `` is rejected, see the notes above).

### Special table names

Table names are upper-cased (`from stats` reads `STATS`) and normally match
log lines of the form `|MAPREDUCE:<TABLENAME>|`. Two table names are special:

* `from .` — and likewise omitting the `from` clause entirely — matches every
  line.
* `from *` matches every `MAPREDUCE` line regardless of the table name.

Also note: a query using the `csv` log format without a `from` clause
automatically behaves as if `from .` was given, so every line of the CSV file
is processed.

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

2. **The `from TABLE` clause selects the rich parser.** Omitting `from TABLE`
   matches every line (see "Special table names" above), and — unless an
   explicit `logformat` is given — also downgrades the query to the `generic`
   log format, which
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
