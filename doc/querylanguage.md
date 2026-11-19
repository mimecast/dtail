DTail Query Language
====================

The query language allows you to run mapreduce queries on log files. This page is the reference to the language.

## Prerequisites

For this to work, DTail needs to understand your log format. DTail already understands its own log format. You can have a look at all examples of the [examples](./examples.md) page using `-query` (these would be all examples of the `dmap` command, and some examples using the `dtail` command).

DTail also ships with a generic log format, which only allows you to run very basic queries. Check out the [log format](./logformats.md) documentation for this. That page also documents how to implement your own log format parser.

In an interactive session (`dtail` in a terminal, or `--interactive-query`), `:query <query>` runs a query on the running session, typed as is without shell quoting (in `dtail` it switches from following lines to the query, and `:tail` switches back), and `:reload --query '...'` changes the query on the fly. Tab inside the query completes the language (clause keywords, aggregations, operators, set functions) and the tables, fields, `$variables` and sample values a background probe of the servers found in the files being read, guided one step at a time (`from` and the table first, as the table narrows the fields, `$variables` and values to its lines); `:fields` lists them. With terminal colors, queries are shown highlighted by what the parser reads each word as. See [Interactive Query Reload](./interactive-query.md); log formats can describe their fields to the probe, see "Custom probes" in [log formats](./logformats.md).

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
written in **any order**. All of them are optional except `select`. A clause
keyword is a bare word: a quoted (`"select"`) or backticked (`` `select` ``)
keyword starts no clause, so a query starting with one is rejected
(`unexpected word "select"`):

```shell
QUERY := CLAUSE1 [CLAUSE2 ...]
CLAUSE := select SELECT1[,SELECT2...]
          | from TABLE
          | where CONDITION1[,CONDITION2...]
          | group [by] FIELD1[,FIELD2...]
          | order [by] ORDERFIELD [desc]
          | rorder [by] ORDERFIELD [asc]
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

* The clause keywords, operators, aggregations and functions listed above are
  the parser's own tables (`Keywords`, `WhereOperators`, `AggregationNames` in
  `internal/mapr`, `Names` in `internal/mapr/funcs`); a unit test fails when
  this page and the tables differ. `AnalyzePartial` (`internal/mapr/complete.go`)
  reads a query typed so far with the same tokenizer and clause rules, for
  completion and highlighting of interactive queries; unit and fuzz tests
  compare its tokens with the parser's.
* Clause keywords and where operators are case insensitive (`SELECT`, `EQ`);
  aggregation and function names are lower case only (`COUNT(foo)` is
  rejected).
* `order` sorts the results by the order field in descending order, `rorder`
  (reverse order) in ascending order. The SQL direction word that matches
  is accepted right after the field and changes nothing: `order by FIELD
  desc` means `order by FIELD`, `rorder by FIELD asc` means `rorder by
  FIELD`. The other one is rejected (`order by FIELD asc` used to sort
  descending silently): use `rorder` for ascending order. Only one order
  field is allowed (`order by a,b` is rejected).
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
  seconds (`interval` without a number is rejected), and nothing may follow
  the closing parenthesis of an aggregation (`count(foo)bar` is rejected).
* The clauses `from`, `order`, `rorder`, `interval`, `limit`, `logformat` and
  `outfile` take one value (`outfile` an optional `append` before it,
  `order` and `rorder` an optional direction word after it); a further word
  up to the next clause keyword is rejected with an error naming it, e.g.
  `unexpected word "gro" after the value of the 'limit' clause` for `limit
  10 gro` or `order by x desc gro` (a half-typed or misspelled clause
  keyword used to be ignored there). See the upgrade note in
  [installation](./installation.md) for scheduled and continuous jobs.
* `outfile append PATH` appends the result to `PATH` (a CSV header only
  when the file is empty or new) instead of replacing it, and replaces
  `PATH.query` with the query. A path starting with `append:` is rejected
  (`outfile append:result.csv` read like an append but wrote a new file
  named `append:result.csv`); a file really named so is written as
  `outfile ./append:result.csv`. Only the bare, lower case word `append`
  before a path is the append mode: `outfile append` alone writes the file
  `append`, as do the quoted `outfile "append"` and the escaped ``outfile
  `append` ``, and `outfile append append` appends to it. A word after a
  quoted or escaped `append` is rejected like any word after the path
  (``outfile "append" x.csv`` and ``outfile `append` x.csv`` were read as
  `outfile append x.csv` before; write the bare word to append).
  What is appended depends on how the query is run:
  * `dmap`, and `dtail --query` with an outfile, aggregate cumulatively:
    every interim report holds the whole result so far. With `append` the
    interim reports therefore append no rows (and write no `PATH.query`);
    they only check that `PATH` can be opened for appending (creating it
    empty if it is missing) and log an error every interval if it can not.
    The final result is appended exactly once when the query ends: the
    client exits, or an interactive `:reload`, `:query` or `:tail` replaces
    the query, then with what it received so far. This also holds for a
    `:reload` that keeps the query text (e.g. `:reload --files ...`): the
    query starts over, so its result so far is appended and the reloaded
    query appends its own result at its end. Until then the result lives
    only in memory: a SIGKILL, a crash, the forced exit or a failed final
    write loses all of it, where a non-append outfile loses at most the last
    interval. Without `append` every interim report replaces `PATH`, so it
    shows the result so far.
  * Scheduled jobs write only their final result, appended once per run.
  * Continuous jobs are not cumulative: every interval appends the rows of
    that interval only, once.
* The fields of `select` and `group by` may also be separated by blanks
  (`select a b` is `select a,b`), so there every word is a field: in
  `select count(x) gro` the word `gro` selects the field `gro`. Only a bare
  `by` is rejected there (`select count(x) gropu by x`, `group by x gropu by
  y`), as it is almost certainly a misspelled `group by` or `order by`; a
  field named `by` is written `` `by` ``. Where and set conditions may be
  separated by blanks as well; a condition missing a part is rejected.
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
  selected values are unchanged. Ordinary compound keys and single-field keys
  retain their previous spelling. Result ordering remains
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
  * The client rejects it, so it never sends the query; a scheduled or
    continuous job with such a query logs the error instead of starting and
    writes no outfile. A `:reload` with such a query is rejected and the
    running query keeps running.
  * dserver rejects it as well, should a client send it anyway: it answers
    with the parse error and sends no results and no log lines, so that
    server contributes no rows instead of wrong numbers:
    * With the plain command stream of non-interactive clients (`dmap`,
      `dtail --query`, continuous and scheduled jobs), dserver reports the
      failed map command (hidden reason `map: invalid query`), rejects the
      session's read commands (each with an error line and the same reason)
      and ends the session with its normal close handshake.
    * With `--interactive-query` (`SESSION START`), dserver rejects the
      session with `.syn session err invalid session spec: <parse error>`,
      which the client shows as `session request rejected: invalid session
      spec: ...`, and streams nothing.
* `interval N` sets how often results are reported, in seconds; without an
  `interval` clause it is 5 seconds, and `interval 0` means 1 second. It
  governs both sides:
  * Every dserver sends what it aggregated since its last send once per
    interval, and the rest once its input ends (a `dmap` read reached the
    end of its files). A query stopped before that (its session torn
    down, or the query replaced by a `:reload`) does not send that rest.
  * The client merges what it received and reports the result (prints it,
    or writes the `outfile`) first half an interval after the query started
    (also after an interactive `:reload` or `:query`), then once per
    interval, and once more when the query ends. As the servers send their
    first aggregates only one interval after the start (or once a `dmap`
    read ended), the first aggregates of a query move the next report: it
    waits until every server still connected sent its first aggregates and
    comes one second after the last of them, but at most half an interval
    after the first server's (a server with nothing to send, or trailing
    by more, is in the next table; a connected server that sends nothing,
    e.g. no matching lines, so delays the first table to half an interval
    after the first server's, or until about a second after its connection
    ended), and both at most half an interval (so `interval 1` reports half a second after the sends, between
    them). The first table thus shows shortly after the data arrived instead
    of 1.5 intervals after the start; a server whose first aggregates trail
    by more is in the next table. The later reports follow once per
    interval from then on, each planned from the time the previous one was
    due, however long that one took. Continuous jobs keep reporting
    every interval after the first half interval. A printed table that has
    no rows or did not change since the last print is not printed again.
    Scheduled jobs report no interim results: they write their outfile once,
    when the query completed.

  The two run on their own clocks, so a reported result can lack up to
  about one interval of the servers' latest lines.
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
    (the global result was busy) joins the global result a moment later
    (the merge is tried again after 10 ms), which can be after the result
    was reported.

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
    dserver sends the number of values with each sum, and the client adds
    up both over all servers before dividing, so the average of several
    servers weighs every value equally.
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

Table names are upper-cased, quoted or not (`from stats` and `from "stats"`
read `STATS`), and normally match log lines of the form
`|MAPREDUCE:<TABLENAME>|` by that exact name. A line whose table name holds
lower-case letters (`|MAPREDUCE:stats|`) is therefore in no table a table
name selects; only `from *` (every `MAPREDUCE` line) and a query without a
table (no `from`, or `from .`: every line) read it. The interactive field catalog
(`:fields`, Tab completion) lists no such table, so that every table it
offers is one a query can name. It lists the fields and `$variables` of
those lines without a table instead (`fields:` in `:fields`, offered by Tab
only while the query names no table) and notes the first such table. A
table name is taken literally, also where it holds characters with a meaning
in a regular expression: `from "A+B"` reads the lines of
`|MAPREDUCE:A+B|` only, and `from A.B` not those of `|MAPREDUCE:AXB|`. Two
table names are special:

* `from .` — and likewise omitting the `from` clause entirely — matches every
  line.
* `from *` matches every `MAPREDUCE` line (every line holding
  `|MAPREDUCE:`) regardless of the table name, also those of a table without
  a name (`|MAPREDUCE:|`) and of a table whose name holds lower-case
  letters (`|MAPREDUCE:stats|`), which no table name selects.

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
