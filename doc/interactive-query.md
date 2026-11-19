# Interactive Query Reload for DTail

## Overview

All DTail clients — `dtail`, `dgrep`, `dcat`, and `dmap` — accept the
`--interactive-query` flag. On capable servers the session is held open after
the initial workload finishes, and the client listens for control commands on
the controlling TTY. This makes it possible to adjust a running query
without paying for a new SSH handshake and reconnect round-trip per change.

## Control commands

The interactive session understands four control commands:

* `:reload <flags>` apply a new workload by reusing the current session when
  the active servers support it
* `:show` print the current interactive state (mode, files, query, regex,
  whether the regex is inverted, options, timeout), including capability
  counts
* `:help` print the interactive command help text
* `:quit` stop the interactive session

## Reload flags

The flags accepted by `:reload` depend on the mode the client runs in. Flags
not listed keep their current value; every reload requires at least one flag.

Shared flags (all clients):

* `--files` file(s) to read
* `--plain` plain output mode
* `--quiet` quiet output mode
* `--timeout` maximum time the `dserver` collects data until disconnection

Mode-specific flags:

* `dtail` (grep-driven) and `dgrep`: `--grep`/`--regex`, `--before`, `--after`,
  `--max`, `--invert`, plus the shared flags
* `dmap`, and query-driven `dtail` (started with `--query`): `--query`, plus
  the shared flags. `--invert` is rejected with a reason (a query selects
  the lines of its table itself, so its filter is never inverted);
  `--invert=false` is accepted and changes nothing
* `dcat`: the shared flags only

`--grep` is an alias of `--regex` and behaves as on the `dgrep` and `dtail`
command lines: a non-empty `--grep` wins over `--regex` whatever their order.
The command lines ignore `--grep ''`; a reload rejects it with an error
instead of restarting the unchanged workload. Use `--regex ''` to clear the
regex.

`--invert` carries over between reloads like the other flags: a reload that
does not give it keeps the current value (`:show` prints it as `invert=`).
Inverted, the cleared regex `''` matches no line, and `.` only the empty
lines; give `--invert=false` together with the new regex to match it
positively again, e.g. `:reload --regex '' --invert=false` for every line.

A reload must leave the session something to do: `--files` needs at least
one non-blank file (`--files ''`, `--files ' '` and `--files ,` are rejected),
and in `dmap` and query-driven `dtail` a blank `--query` is rejected. A
rejected reload keeps the current workload running unchanged.

## Quoting

The flags of a `:reload` line are split into arguments like a POSIX shell
splits a command line, without any variable, command or glob expansion
(`$line` and `*` stay as typed):

* Spaces and tabs separate arguments; quote a value to keep its spaces.
  A `--regex` (or `--grep`) value reaches the servers with its blanks
  exactly as typed (a leading or trailing blank, a run of blanks and a tab
  stay what they are), so `:reload --regex 'x  y'` matches `x  y` but not
  `x y`; so do the blanks inside a `--query`. Other values are trimmed: each
  comma-separated `--files` entry loses its leading and trailing blanks, and
  so does the whole `--query`. Older dservers that already accept reloads
  (capability `query-update-v1`) split the commands they receive at every run
  of blanks, so they collapse a run of blanks or a tab in a regex or query
  into one space and drop a regex's leading and trailing blanks.
* Single quotes keep everything up to the closing quote literally,
  backslashes included. Use them for regular expressions:
  `:reload --regex '\d+ ms'` matches the regex `\d+ ms`.
* Double quotes also keep backslashes, except that `\\`, `\"`, `\$` and
  ``\` `` stand for a backslash, a double quote, a dollar sign and a backtick,
  as in a POSIX shell: `:reload --regex "a\.b"` is the regex `a\.b`, and
  `"\$"` is `$`. (A shell's backslash-newline continuation does not apply,
  as each control line is read on its own.)
* Outside quotes, a backslash makes the next character literal, so `\d`
  becomes `d` there; `foo\ bar` is the one argument `foo bar`.
* An empty quoted string is an empty value: `:reload --regex ''` clears the
  regex, so every line matches again, unless `--invert` is in effect (also
  from an earlier reload or the command line): then no line matches. Add
  `--invert=false` to match every line.
* Quoted and unquoted parts next to each other form one argument, and an
  unterminated quote or a trailing backslash rejects the whole line with an
  error, leaving the current workload unchanged.
* Non-ASCII text is passed through byte for byte. A `:reload` line that is
  not valid UTF-8 is rejected with an error, as the update sent to the
  servers is JSON, which cannot carry invalid UTF-8 unchanged. The check
  covers `:reload` lines only: the initial workload from the command-line
  flags is not checked, and when it starts with `SESSION START` (see below)
  invalid bytes in its values reach the servers as U+FFFD.
* Only leading blanks and the line ending are removed before splitting, so
  an escaped trailing blank stays part of the value: `--regex foo\ ` (with
  a space after the backslash) is the regex `foo ` with the trailing space.
* A file path containing spaces is quoted or escaped like any other value:
  `:reload --files '/var/log/my app/a.log'`. The servers must advertise the
  capability `encoded-read-path-v1` (dserver of this version or newer);
  otherwise the reload is rejected with `file paths containing spaces or
  starting with base64% require server capability encoded-read-path-v1:
  <servers>` before any server switches, and the current workload keeps
  running. To read several files, list them comma-separated, e.g.
  `:reload --files /var/log/a.log,/var/log/b.log`; a comma therefore always
  separates two files, so paths containing a comma cannot be read, and
  blanks around each path are removed.

## Examples

Keep a `dtail` run open and switch the grep pattern in-flight:

```bash
dtail --servers app01 --files /var/log/app.log --grep ERROR --interactive-query
# then type:
:reload --grep WARN
```

Change the match and its context in a running `dgrep`:

```bash
dgrep --servers app01 --files /var/log/app.log --grep ERROR --interactive-query
# then type:
:reload --grep WARN --before 2 --after 3
```

Adapt a distributed MapReduce query without reconnecting:

```bash
dmap --servers app01 --files /var/log/app.log \
  --query 'from STATS select count($line) group by hostname' \
  --interactive-query
# then type:
:reload --query "from STATS select count($line),avg(latency) group by hostname"
```

## How it works

* On startup, an interactive client first tries a `SESSION START` handshake
  when the remote side advertises the `query-update-v1` capability.
* If a server is older or does not advertise that capability, startup falls
  back to the legacy command stream automatically, so mixed-version
  client/server combinations still run the original workload normally.
* Live `:reload` updates require every active server to advertise
  `query-update-v1`; otherwise the reload is rejected with an error message
  and the current workload keeps running unchanged.
* On capable servers, DTail reuses the existing SSH session and sends
  `SESSION UPDATE` messages instead of reconnecting.
* Every successful reload advances a generation boundary; late output from the
  previous workload is dropped so stale matches do not leak into the new result
  stream.

## See also

* The [DTail Query Language](./querylanguage.md) for `--query` syntax.
* The [Server Configuration Guide](./server-configuration.md) for the
  server-side settings.