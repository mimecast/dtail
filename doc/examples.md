Examples
========

This page demonstrates the primary usage of DTail. Please also see `--help` for more available options.

## Table of contents

* How to use `dtail` to follow logs
* How to use `dtail` to aggregate logs
* How to use `dcat`
* How to use `dgrep`
* How to use `dmap`
* How to use the DTail serverless mode
* Exit statuses of `dcat` and `dgrep`
* More examples

## How to use `dtail`

### Following logs

The following example demonstrates how to follow logs of multiple servers at once. The server list is provided as a flat text file. The example filters all records containing the string `INFO`. Any other Go compatible regular expression can also be used instead of `INFO`.

```shell
% dtail --servers serverlist.txt --grep INFO --files "/var/log/dserver/*.log"
```

The server list file has one server (`HOST` or `HOST:PORT`) per line.
Whitespace around an entry is ignored, and so are blank lines and lines
starting with `#` (a `#` after an entry starts no comment), e.g.:

```
# web tier
web01.example.org
web02.example.org:2223

# db tier
db01.example.org
```

Hint: you can also provide a comma-separated server list, e.g.:
`--servers server1.example.org,server2.example.org:PORT,...`
Spaces around the commas and empty entries are ignored, so
`--servers 'server1.example.org, server2.example.org'` works as well.
A server list (file or comma-separated) that names no server at all, e.g.
a file of only comment lines or `--servers ' '`, is an error (`no server to
connect to`); `dtailhealth` reports it as `CRITICAL`. So is a `--discovery`
module, or a `/regex/` filter, that yields no server, and an empty
`--servers ''` together with `--discovery`. Whitespace around the path of a
server list file is ignored too, and so is a UTF-8 byte order mark at the
start of the file.

![dtail](dtail.gif "Tail example")

Hint: You can also use the shorthand version (omitting the `--files`)

```shell
% dtail --servers serverlist.txt --grep INFO "/var/log/dserver/*.log"
```

Hint: `--files` (and the shorthand arguments) take a comma-separated list of
files or globs, with blanks around each entry removed, so paths containing a
comma cannot be read. Paths containing spaces work when quoted for the shell,
e.g. `--files '/var/log/my app/*.log'`; this needs a dserver of the same
version or newer (capability `encoded-read-path-v1`), and the client fails
with `file paths containing spaces or starting with base64% require server
capability encoded-read-path-v1` against an older one.

### Aggregating logs

To run ad-hoc map-reduce aggregations on newly written log lines you must add a query. The following example follows all remote log lines and prints out every few seconds the result to standard output.

Hint: To run a map-reduce query across log lines written in the past, please use the `dmap` command instead.

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```

Beware: For map-reduce queries to work, you have to ensure that DTail supports your log format. Check out the [query language](./querylanguage.md) and [log formats](./logformats.md) for more information.

![dtail-map](dtail-map.gif "Tail mapreduce example")

Hint: You can also use the shorthand version:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    'from STATS select sum($goroutines),sum($cgocalls),last($time),max(lifetimeConnections)'
```
Here is another example:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

![dtail-map](dtail-map2.gif "Tail mapreduce example 2")

You can also continuously append the results to a CSV file by adding `outfile append filename.csv` to the query:

```shell
% dtail --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select ... outfile append result.csv'
```

## How to use `dcat`

The following example demonstrates how to cat files (display the full content of the files) of multiple servers at once.

The default client logger (`fout`) writes a daily diagnostics file to `~/log/YYYYMMDD.log` containing only the small connection/audit diagnostics (INFO/WARN/ERROR lines); the retrieved payload itself goes to stdout only. If you also want the payload teed into that daily log file, add the `--log-payload` flag (or set `Client.LogPayload` in the config). You can also use the `noColor` and `-plain` flags (this all also work with other DTail commands than `dcat`). To log to stdout only or to no log file at all, use `--logger stdout` or `--logger none`; `--logger file` writes to the log file only.

```shell
% dcat --servers serverlist.txt --files /etc/hostname
```

![dcat](dcat.gif "Cat example")

Hint: You can also use the shorthand version:

```shell
% dcat --servers serverlist.txt /etc/hostname
```

## How to use `dgrep`

The following example demonstrates how to grep files (display only the lines which match a given regular expression) of multiple servers at once. In this example, we look after some entries in `/etc/passwd`  This time, we don't provide the server list via an file but rather via a comma separated list directly on the command line. We also explore the `-before`, `-after` and `-max` flags (see animation).

```shell
% dgrep --servers server1.example.org:2223 \
    --files /etc/passwd \
    --regex nologin
```

Generally, `dgrep` is also a very useful way to search historic application logs for certain content.

![dgrep](dgrep.gif "Grep example")

Hint: `--grep` is an alias for `--regex`.

## How to use `dmap`

To run a map-reduce aggregation over logs written in the past, the `dmap` command can be used. The following example aggregates all map-reduce fields `dmap` will print interim results every few seconds. You can also write the result to an CSV file by adding `outfile result.csv` to the query.

```shell
% dmap --servers serverlist.txt \
    --files '/var/log/dserver/*.log' \
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

Remember: For that to work, you have to make sure that DTail supports your log format. You can either use the ones already defined in `internal/mapr/logformat` or add an extension to support a custom log format. The example here works out of the box though, as DTail understands its own log format already. 

![dmap](dmap.gif "DMap example")

## How to use the DTail serverless mode

Until now, all examples so far required to have remote server(s) to connect to. That makes sense, as after all DTail is a *distributed* tool. However, there are circumstances where you don't really need to connect to a server remotely. For example, you already have a login shell open to the server an all what you want is to run some queries directly on local log files.

The serverless mode does not require any `dserver` up and running and therefore there is no networking/SSH involved. 

All commands shown so far also work in a serverless mode. All what needs to be done is to omit a server list. The DTail client then starts in serverless mode. The explicit spelling `--servers serverless` works as well (in any case, and with whitespace around it ignored). Note that serverless mode defaults to log level `warn` unless you override it (e.g. with `--logLevel info`).

### Serverless map-reduce query

The following `dmap` example is the same as the previously shown one, but the difference is that it operates on a local log file directly:

```shell
% dmap --files /var/log/dserver/dserver.log \
    --query 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

As a shorthand version the following command can be used:

```shell
% dmap 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)' /var/log/dserver/dserver.log
```

You can also use a file input pipe as follows:

```shell
% cat /var/log/dserver/dserver.log | \
    dmap 'from STATS select $hostname,max($goroutines),max($cgocalls),$loadavg,lifetimeConnections group by $hostname order by max($cgocalls)'
```

### Aggregating CSV files

In essence, this works exactly like aggregating logs. All files operated on must be valid CSV files and the first line of the CSV must be the header. E.g.:

```shell
% cat example.csv
name,lastname,age,profession
Michael,Jordan,40,Basketball player
Michael,Jackson,100,Singer
Albert,Einstein,200,Physician
% dmap --query 'select lastname,name where age > 40 logformat csv outfile result.csv' example.csv
% cat result.csv
lastname,name
Jackson,Michael
Einstein,Albert
```

DMap can also be used to query and aggregate CSV files from remote servers.

### Other serverless commands

The serverless mode works transparently with all other DTail commands. Here are some examples:

```shell
dtail /var/log/dserver/dserver.log
```

```shell
dtail --logLevel trace /var/log/dserver/dserver.log
```

```shell
dcat /etc/passwd
```

```shell
dcat --plain /etc/passwd > /etc/test
# Should show no differences.
diff /etc/test /etc/passwd 
```

```shell
dgrep --regex ERROR --files /var/log/dserver/dserver.log
```

```shell
dgrep --before 10 --after 10 --max 10 --grep ERROR /var/log/dserver/dserver.log
```

### Server warnings and log levels

The warnings a server sends to the client (a missing file, a glob matching more files than the server reads, a split long line, ...) do not depend on `dserver`'s `--logLevel`: a `dserver` running at `--logLevel error` or `none` still sends them with their full text, and its level only decides what `dserver` writes to its own log.

In server mode the client prints these warnings as output lines, which its own `--logLevel` does not filter: framed as `SERVER|<host>|WARN|...` by default, and with `--plain` as the bare `WARN|...` line among the payload lines (so `dcat --plain` output redirected to a file can contain them). In serverless mode the client reads the files itself, and its warnings go through the client's logger, so its `--logLevel` filters them (serverless mode defaults to `warn`).

## Exit statuses of `dcat` and `dgrep`

Current clients require the server to advertise `escaped-framing-v1` in its
first frame. Older servers without it are unsupported: the client reports a
missing-capability error, asks you to update DTail server, and exits 1. Some
such servers share the same protocol label (`4.1`) but close on new control
commands before reading files, which previously looked like a successful
empty result. Servers with escaped framing but without control-frame markers
remain supported.

`dcat` and `dgrep` exit with one of these statuses, in server and in serverless mode alike:

| Status | Meaning |
|---|---|
| `0` | Every server read what was asked. `dgrep` exits `0` also when no line matched (unlike `grep`, which exits `1` then). |
| `1` | A server could not be connected to (refused or unreachable), the SSH handshake failed or no SSH session could be opened, the server rejected or did not acknowledge an `--interactive-query` session start or the commands could not be sent, a server does not support a requested journal target (capability `journal-v1`) or a path containing a space or starting with `base64%` (capability `encoded-read-path-v1`), a serverless session failed, a session was cut short in the middle of a read (see below), or the client could not start (e.g. a configuration error or no server to connect to). |
| `2` | A server reported that a command of the session failed, or the command line had an invalid flag. |

A server reports a failed command when a file is missing (also a glob that matched no file after its retries), denied by the read permissions, or could not be opened or read; when a glob matched more files than the server reads (`MaxGlobTargets`); when it could not parse the read command; and when a read produced more output than the session output buffer (`OutputBufferMaxBytes`) holds. The output of the other files and servers is still printed, and the reason is in the server's warnings and log. With several servers the client exits with the highest status of them all, so a read failure on one server (`2`) outweighs a connection failure on another (`1`). In an `--interactive-query` session a failure that reached the client counts for the whole session, also when it was reported for an earlier workload that a `:reload` has replaced since. A failure of a replaced workload that `dserver` still had queued when the reload took effect is dropped with that workload's other output and does not count, and a workload that a reload cancelled reports no further failures.

A session cut short in the middle of a read (e.g. `dserver` killed, crashed or shut down, or a network failure, so that the session ended without the server's close handshake) exits `1`, as the output may be incomplete, and the client logs `Session ended without the server's close handshake, the output may be incomplete` for that server. A failed command the server reported before (`2`) still outweighs it. Every `dserver` version ends a session it completed with that handshake, so this works with older servers too. A read the client stopped itself keeps its status: Ctrl+C twice, `SIGTERM`, or `:quit` in an `--interactive-query` session.

In `--plain` server mode a log line that starts with `.syn command failed`, or a log line `.syn close connection`, is printed like any other line and does not change the exit status or end the read. This needs a `dcat`/`dgrep` and a `dserver` that both support control frames (capability `control-frames-v1`); with an older client or server the first is still taken as a failure report (exit `2`), the second as the server's close handshake (the output is truncated at that line and the client exits `0`), and the other lines starting with `.` are dropped. The default (non-plain) output and serverless mode are not affected either way.

Only servers advertising capability `command-failure-v1` report failed commands. With an older `dserver` a missing or unreadable file still exits `0`, as before.

The other clients keep their exit statuses: `dtail` (follow mode) is not affected by failed reads or sessions cut short (it reconnects), `dmap` handles them in its scheduled modes (see the [Server Configuration Guide](./server-configuration.md)), and `dtailhealth` has its own statuses.

```shell
% dcat --servers serverlist.txt --files /var/log/missing.log; echo $?
...
2
```

## More examples

Follow the systemd journal of a unit on all servers (Linux servers advertising the `journal-v1` capability, i.e. with `journalctl` on `PATH`):

```shell
% dtail --servers serverlist.txt journal:myapp.service
```

Keep a `dgrep` run open and reload the query in-flight (see the [Interactive Query Reload documentation](interactive-query.md)):

```shell
% dgrep --servers serverlist.txt --files '/var/log/dserver/*.log' --grep ERROR --interactive-query
:reload --grep WARN
```

A few more one-liners:

```shell
% dgrep --servers serverlist.txt --invert --grep INFO --files '/var/log/dserver/*.log'
# Prints all lines NOT matching the regex
```

The patterns `.` (the default of `dtail` and `dgrep`), `.*` and the empty pattern select every line, empty lines included. With `--invert` they are matched like any other regex, as `grep -v` does: `--invert --grep '.*'` prints no line, and `--invert --grep .` (also `--invert` without `--grep` or `--regex`) prints only the empty lines. A line ending in `\r\n` is not empty.

The empty pattern can only be given in some places: on the command line `--grep ''` is ignored, which leaves the default `.`, and `dgrep --regex ''` is rejected (`no regex specified`). `dtail --regex ''` and an interactive `:reload --regex ''` (or `:reload --regex=''`) do use it, and inverted it prints no line, like `.*`. `:reload --grep ''` is rejected with a hint to use `--regex ''` instead. A `:reload` keeps an earlier `--invert` (from the command line or a previous reload) unless it gives `--invert=false`, so after `--invert` a `:reload --regex ''` prints no line; `:show` prints the current `invert=` value.

`dtail` can follow a gzip file (`.gz`, `.gzip`): it waits at the end of the file and prints the lines of the gzip members appended from then on (the writer must append whole members).

An option given on the command line overrides the config file also when it names the default value (`--port 2222`, `--logLevel info`).

A regex is matched against the line without its line terminator, for every kind of read (`dtail`, `dcat`, `dgrep`, `dmap`, journal targets): `--regex 'foo$'` selects the lines ending in `foo`, and `--regex '^$'` the empty lines of a `dgrep`.

With `--query`, `dtail` reads the lines of the query's table (every line for `from .` or no table), whatever `--regex` or `--grep` say, and it ignores `--invert` with a warning: inverting the table filter would feed the other tables' lines, or only the empty lines, into the aggregation. In an interactive query session (`dmap`, or `dtail --query`, with `--interactive-query`) `:reload --invert` is rejected with the same reason on the terminal, and the current workload keeps running.

Mixed versions: the inverted `.` and `.*` (and `''`) are honoured where the regex is built by a current DTail. In the default command mode the client builds it, so a current client gets the behaviour above from an older `dserver` too. With `--interactive-query` and a server advertising `query-update-v1` (`SESSION START`), `dserver` builds it from the session spec, so a current client talking to an older `dserver` still prints every line. An older client is the other way round: a current `dserver` honours its `--invert` in an interactive session, but not in the default command mode. The same holds for `--query`: a current `dserver` ignores the `--invert` of an older client's interactive `--query` session, while in the default command mode the older client sends the inverted table filter itself. For a named table or `*` the inversion then applies (the query reads every line except those of its table), but for `from .` or no table the filter is `.`, which the older client's regex code turns into a match of every line even when inverted, so such a query still reads every line. Serverless runs always behave as above.

```shell
% dcat --servers serverlist.txt --trustAllHosts /etc/hostname
# Skips the unknown-host trust prompt (use with care)
```

```shell
% dtail --servers serverlist.txt --user paul --files '/var/log/dserver/*.log'
# Connects as SSH user "paul" instead of the local system user
```

```shell
% dcat --servers serverlist.txt --log-payload /etc/hostname
# Also tees the retrieved payload into the daily client log file ~/log/YYYYMMDD.log
```
