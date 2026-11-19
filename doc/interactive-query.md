# Interactive Query Reload for DTail

## Overview

All DTail clients — `dtail`, `dgrep`, `dcat`, and `dmap` — accept the
`--interactive-query` flag. The session is held open after
the initial workload finishes, and the client listens for control commands on
the controlling TTY. This makes it possible to adjust a running query
without paying for a new SSH handshake and reconnect round-trip per change.

## Default: on for `dtail` in a terminal

`dtail` runs interactively by default: when neither `--interactive-query` nor
`--no-interactive` is given, interactive control is on if stdin and stdout are
both a terminal and the control TTY (`--control-tty`, default `/dev/tty`) can
be opened. A follow never ends by itself, so keeping the session open costs
nothing. Everything else keeps the old behaviour without any flag: a pipe
(`dtail ... | grep x`), a redirection (also `< /dev/null`), cron, CI, ssh
without a TTY and scripts.

* `--no-interactive` turns it off. It wins over `--interactive-query`.
* `--interactive-query` and `--interactive-query=false` are used as given,
  terminal or not.

`dcat`, `dgrep` and `dmap` stay one-shot: interactive control would keep
their session open after the workload finished, so they would no longer exit
by themselves. They still accept `--interactive-query` explicitly.
dserver's scheduled and continuous MapReduce jobs never run interactively,
whatever their arguments say (`NewMaprClient` forces it off for every job
mode).

On start the client prints `Interactive query control enabled. Press Enter to
pause the output. Type :help for examples of changing the filter or query on
the fly (Tab completes), :quit to end.`

## Control commands

The interactive session understands these control commands (`:query` only
in `dtail` and `dmap`, `:tail` only in `dtail`):

* `:reload <flags>` apply a new workload by reusing the current session
* `:query <query>` run the MapReduce query `<query>`: in `dtail` the session
  switches from following lines to the query (or a running query is
  replaced), in `dmap` the query is replaced, as `:reload --query` does. The
  query is the rest of the line as typed, without shell quoting, so
  `:query from STATS select count($line) where $hostname eq "web1"` needs no
  outer quotes; only the blanks around it are dropped. A query that does not
  parse is rejected and the running workload goes on. `:query` alone prints
  the running query (or that the session follows lines).
* `:tail [regex]` (`dtail` only) switch the session from the query back to
  following lines, with the regex (and `--invert`, context lines, `--max`)
  followed before the query; `:tail <regex>` follows the lines matching
  `<regex>` instead (clearing `--invert`), also while already following
  lines. The regex is the rest of the line as typed, blanks around it
  dropped (write `foo[ ]` for a regex ending in a blank). `:tail` alone
  while the session already follows lines is rejected. When the session
  leaves the query, its last result is reported once more (see "How it
  works"): written to the query's outfile, if it has one, or else printed
  only if it has rows and differs from the table printed last.
* `:show` print the current interactive state (mode, the workload as
  `workload=query` while a query runs and the mode otherwise, files, query, regex,
  whether the regex is inverted, options, timeout), the number of servers
  (`servers=n`) and how many of them have an active session (`sessions=n`,
  the ones a `:reload` updates)
* `:help` print the commands and examples of changing the workload on the
  fly for the client's mode: the grep filter for `dtail` and `dgrep`
  (`--grep`, `--regex`, `--before`/`--after`, `--invert`, `--files`), the
  MapReduce query for `dmap` and `dtail --query`, the files for `dcat`. Every
  example is a valid `:reload` line of that mode. The output stays paused
  after `:help` until Enter is pressed on an empty line.
* `:fields [table]` print what the background field probe of the servers
  (see "How it works") found: per log format (the server's default format
  first) its tables with their fields and up to five sample values each,
  its `$variables` and the notes of the format's probe, together with the
  servers that answered, how old the data is, the default log format, the
  servers' probe errors (e.g. a file that could not be read) and whether an
  answer was cut to its size limit. With a table name (any case, as in a
  query's `from` clause) only that table is printed, with the `$variables`
  its lines carry, and the servers are first asked to probe that table on
  its own (a table probe: up to 4 MiB of the end of each file are searched
  for its lines, so a table that logs rarely gets fields and samples too;
  waited for up to 2 seconds with the output running, and asked again only
  after a minute); a line `table ORDERS probed on its own: 3 lines of it
  sampled` tells what it found. Names and values a server sent are printed in Go
  quoting where they are not plain printable text. Until a probe answered,
  `:fields` says so. A session that reads no files (started without
  `--files` or a path; serverless with stdin a terminal reads nothing
  either) can never get field data: there `:fields` answers `the session
  reads no files: :reload --files PATH (Tab lists remote paths)` (local
  paths in serverless mode), and the same line follows the banner at the
  start. The output
  stays paused after `:fields`, as after `:help`.
* `:probe` probe the servers now instead of waiting for the next round, and
  report which servers answered (`probe answered by srv1, srv2; :fields
  shows what was found`), answered busy or did not answer. It waits up to 5
  seconds; a probe still running after that is reported as such and its
  answers are stored once they arrive. A session reading only stdin has
  nothing to probe; one reading no files at all answers `nothing to probe:
  the session reads no files: :reload --files PATH`.
* `:quit` stop the interactive session

## Line editing and completion

When the control TTY is a terminal, control lines are edited in a small line
editor (golang.org/x/term, no other dependency):

* **Tab** completes the word before the cursor:
  * the command: `:re<Tab>` gives `:reload `, `:<Tab>` lists the commands
    of the client (with `:query` in `dtail` and `dmap`, `:tail` in `dtail`);
  * after `:query `, the query language and the probed fields exactly as
    inside a `--query` value (below), inserted as they are, without shell
    escaping: `:query from ST<Tab>` gives `:query from STATS `, and a sample
    value is inserted in the query's own double quotes (`eq "web1" `);
  * after `:fields`, the tables the probe found (`:fields st<Tab>` gives
    `:fields STATS`);
  * after `:reload`, the flags of the client's mode, taken from the same
    definition the `:reload` parser uses, so only accepted flags are offered
    (`--gr<Tab>` gives `--grep ` in `dtail`/`dgrep`, nothing in `dmap`);
    boolean flags also as `--invert=false`;
  * right after `--grep`, `--regex`, `--query` or `--files` (and a blank),
    their **current value**, quoted for the `:reload` tokenizer, so a long
    query is edited instead of typed again:
    `:reload --query <Tab>` inserts `'from STATS select ...'`;
  * inside a `--query` value (in `dmap` and `dtail --query`; quoted with
    single or double quotes, escaped, or as `--query=...`), the query
    language: clause keywords, aggregations (`count(`), where operators,
    `and`, `by`, `append`, set functions, log format names and the select
    items for `order by`, plus what the servers' background field probe
    found: tables, the fields of the query's table (of every table while
    none is named), `$variables` and, after a where operator, sample values
    (`"web1"` after a string operator, bare numbers after a numeric one).
    The completion is **guided**: Tab offers one step at a time, in the
    order `from` -> table -> `select` -> items -> `where` -> `group by` ->
    `order by` -> `limit`, and a single next step is inserted right away.
    A query begun with Tab (`:query <Tab>`, an empty `--query` value)
    always starts with `from`: with the table too when the servers
    reported only one table of the log format, else `from` with the tables
    listed in the same Tab, or, while no server has answered the field
    probe yet, `from` with a hint that a table name follows:

    ```
    :query <Tab>                         -> :query from STATS
    :query <Tab>                         -> :query from   (several tables)
      table: ORDERS  STATS
    :query from STATS <Tab>              -> :query from STATS select
    :query from STATS select <Tab>
      select item
      aggregation: count  sum  min  max  avg  last  len  percentage  percentile
      field:       currentConnections  lifetimeConnections
      variable:    $hostname  $time ...   (those seen in STATS lines)
    :query from STATS select count(<Tab>
      field to count
      field:    currentConnections  lifetimeConnections
      variable: $hostname  $time ...
    :query from STATS select count(currentConnections) <Tab>
      next: where  group by  order by  limit
      (, adds another item)
    ```

    Tab inserts the `)` after a field inside an aggregation without a
    blank, so that a comma (for another item) can follow it directly. A
    Tab right after the `)` of such a finished item (also of a set
    function, `md5sum($x)`, an order by item or a where value ending in
    `)`) lists the next step with the comma hint, as above, even a single
    next clause, and inserts the blank when the item ends the query; a
    comma typed right after that blank (the cursor not moved, no other key
    typed in between) replaces it, so `count(x)` Tab `,` gives
    `count(x),`. With text after the cursor (a blank, a comma, more of a
    word) it inserts nothing and only lists. Tab after a typed blank inserts a single next
    clause directly. A whole session built this way is under "Examples".

    A listing starts with a heading saying what is expected at the cursor
    (`select item`, `field to count`, `field to filter on`, `operator`,
    `value for currentConnections`, `to order by`, ...; left out where the
    group labels say it, e.g. `next:` or `table:`, before the first clause,
    whose group is `clause:`, and where the next clauses are offered beside
    other words), followed by the candidates in labelled groups, one per
    line (`and` and the next clauses share one `next:` line), the fields of
    the log before the `$variables`. Names are listed bare: the `(` after
    an aggregation, the `)` after a field inside it, the quotes around a
    value and the trailing blank are added only when Tab inserts the
    candidate; a field named like a keyword keeps the backticks it must be
    written in (`` `from` ``). A group longer
    than the terminal is wide wraps, its rows aligned, and is cut after
    three rows with `... (N more, type to narrow)`; hints (no field data
    yet, a comma adds another item, a table probe running) come on a line
    of their own.

    The table comes first because it narrows the fields, `$variables` and
    sample values to the lines of that table (`$variables` as the probe saw
    them in that table's lines); where the servers answered without a
    table of the log format (e.g. `csv`, or plain logs that only `generic`
    parses) the query starts at `select`, as a `from` clause would select
    nothing there; `from` with the hint is only for when no server has
    answered yet. Where `from` is the only clause left later in the query,
    Tab inserts it with the one known table too. A clause already in the
    query, before or after the cursor, is not offered again (`order` and `rorder` count as
    one). After a finished select item, where condition or `group by` field
    Tab offers only what continues the query (the next clauses, and `and`
    after a condition) with a hint that a comma adds another item; after a
    comma (or `and`) the items come again. A word typed before Tab
    completes to either: `... count(x) w<Tab>` gives `where `, `... count(x)
    lif<Tab>` gives `lifetimeConnections `. A where operator position offers
    only operators, a value position only sample values (no fields).
    `group` and `order` complete to `group by ` and `order by `, which then
    offers only the select items (without `by` when the text after the
    cursor starts with `by` already; `outfile` likewise with `append`). An
    item already selected is not offered again in `select` (the parser
    rejects duplicates), and after a where condition no field named `and`.
    Before the first probe answer the first step is `select `, with the
    hint that no field data arrived yet. The rare clauses `set`, `interval`,
    `outfile`, `logformat` and `rorder` complete once their first letters
    are typed (`int<Tab>` gives `interval `) but are not listed otherwise.
    Any clause order the parser accepts still completes when typed by hand:
    a query begun with `select` gets the fields of every table, with a hint
    that `from <table>` narrows them, and `from` among the next clauses. A
    `from TABLE` typed after the cursor scopes the completion too
    (`:query select count(<Tab>) from STATS`).
    What is inserted is escaped for the open quote, so the line still
    reads as the intended query: `:reload --query 'select cou<Tab>` gives
    `'select count(`, and `... where $hostname eq w<Tab>` gives
    `eq "web1" ` (inside double quotes `eq \"web1\" `). A field named like a
    keyword (or `by`) is inserted in backticks (`` `from` ``); fields whose
    names contain a blank (also a non-breaking one), comma, quote or
    parenthesis are not offered unquoted (inside a double quote in `select`
    and `group by` they are, see below, except names with a double quote), and
    no table, field, `$variable` or sample value holding a control or
    invisible format character (e.g. a coloured log value with escape
    sequences) is offered, as it would run on the terminal. The select
    items for `order by` are written as the query needs them: a keyword in
    backticks (`` order by `limit` ``), an item with a blank or comma in
    double quotes (`order by "foo bar"`). Inside a query string quote
    (`"...`) only words that can be written quoted are offered, and the
    quote is closed: values (`eq "web1" `), tables (`from "STATS" `), log
    format names, `order by` items, and fields in `select` and `group by`;
    elsewhere (a where operand or operator, a `set` condition) nothing is
    offered, as a quoted word would be a string. Until a probe answered, the query language
    still completes and a hint line says that no field data arrived yet
    (for a session that reads no files: how to give it files, as for
    `:fields`).
    Once a `from` table is typed, Tab also has the servers probe that
    table on its own (as `:fields TABLE` does) and waits up to a second for
    it with the output running, so the fields of a table that logs rarely
    are offered too. Only that one Tab waits: while the answer is still on
    its way, Tab completes from what arrived before, with a hint to press
    Tab again later, and after a probe no server answered the table is
    only asked for again after 5 seconds;
  * inside a `--files` value (started, quoted or not, or as `--files=...`),
    the **remote file paths**: the last comma-separated entry is completed
    from a listing of its directory on the first server that is up, with only the files
    the session user may read there and the directories beneath which
    something may be readable (see `docs/dev/probe.md`, "Path listing");
    directories get a trailing `/`, so a further Tab descends. A
    `journal:` prefix completes the systemd units. Names with a blank or a
    quote are inserted escaped for the quote they are typed in
    (`:reload --files /var/log/my<Tab>` gives `/var/log/my\ app.log`, inside
    single quotes `'/var/log/my app.log`); names with a comma, blanks at
    either end or control characters are not offered, as `--files` could
    not read them, nor names with invisible format characters (such as a
    bidirectional override), which could disguise them. Names starting with `.` are offered once the typed name
    starts with `.`. The directories of the files being read are listed in
    the background with the probe at the start, after a `:reload` and on
    `:probe`, so their siblings (rotated logs) complete at once (later, once
    that listing is older than two minutes, Tab asks for it again); the
    systemd units of a `journal:` target are not listed in the background,
    only when Tab asks for them, as listing them reads the whole journal on
    the server (which keeps the unit list for a minute for all sessions). For another directory Tab asks the server and lets
    the output run (the line being edited is taken off the screen and drawn
    again below it) until the listing arrives, at most one second, then
    pauses it again; if the listing did not come in time, a hint says so,
    and the next Tab uses it once it arrived. The answer of a remote server
    can only be read while the output runs; in serverless mode the
    in-process server logs to the same paused output, so it waits the same
    way. With several servers, a listed set of candidates is headed with the
    server it comes from, as the listing is that one server's view. Listings
    are cached for two minutes; one that timed out or failed on the server
    only for five seconds, so the next Tab asks again. `Server.PathCompletionDisable` on the server
    turns path listing off (a hint line says so).

  When several candidates remain, Tab extends their common prefix, and a
  further Tab lists them.
* **Up/Down** recall earlier control lines (the last 100, without blank
  lines and without repeating the latest line).
* Ctrl-A/Ctrl-E, Ctrl-U, Ctrl-W, Ctrl-K, Left/Right and the other usual
  editing keys work; Ctrl-D on an empty line ends the input like closing
  the terminal.
* **Alt-E** (or Alt-V, the keys of fish's `edit_command_buffer`) opens
  the line in an external editor: `$VISUAL`, else `$EDITOR`, else `vi`
  (run by `/bin/sh`, so `EDITOR="code -w"` works). Save and quit, and the
  edited text replaces the line, so you can check it, complete with Tab
  and run it with Enter; nothing runs on its own. Line breaks, tabs and
  other control characters in the text become blanks, which suits a long `:query` written over several lines.
  The output stays paused while the editor runs, and the editor gets the
  terminal in its normal mode. An editor that is not found or exits
  non-zero (vim's `:cq`) keeps the line as it was and says why. The
  terminal must send Alt as an ESC prefix (the default of most terminals;
  in macOS Terminal and iTerm2 set "Use Option as Meta key" / "Esc+").
* **vi mode**: with `--edit-mode vi` (or `"EditMode": "vi"` in the
  `Client` section of the config file; another value stops the client,
  not `dserver`) the line editor has a vi normal mode. Every line starts in insert mode, where the keys above work; Esc
  switches to normal mode (the cursor turns into a block, a bar again in
  insert mode, where the terminal supports cursor shapes). Normal mode has:
  * motions `h` `l` `w` `b` `e` `W` `B` `E` `0` `^` `$`, `f` `F` `t` `T`
    with a character and `;` `,` to repeat them, all with a count (`3w`);
  * the operators `d` `c` `y` with a motion (`dw`, `c2w`, `dt,`, `y$`) or
    doubled for the whole line (`dd` `cc` `yy`), and `x` `X` `D` `C` `s`
    `S` `Y`; `D` deletes from the cursor to the end of the line;
  * `i` `a` `I` `A` to insert, `r` to replace, `~` to switch case, `p` `P`
    to put what was deleted or yanked;
  * `u` undo and Ctrl-R redo (per line), `k` `j` earlier and later lines
    of the history.

  Enter runs the line in either mode. Keys the editor itself handles work
  in both modes as in insert mode: Backspace, the arrows, Home/End and the
  Ctrl keys (so Backspace in normal mode deletes rather than moves). A
  terminal sends Esc as a single byte; a key sequence split on a slow link
  (ssh over a bad line) may be taken for Esc plus keys, and an Esc that
  reaches `dtail` together with the next key (typed ahead while it was
  busy) is read as Alt plus that key, so Esc `e` opens the external editor.
* Tab always completes, so a literal tab cannot be typed into a value; use
  `\t` in a regex instead.
* A line is limited to 4095 characters. A longer one is rejected with an
  error, not run and not kept in the history (the editor would otherwise
  cut it silently); output its typing paused goes on.

The terminal is switched to "cbreak" mode for this: line buffering and echo
are off, but Ctrl-C and Ctrl-Z still send their signals and log output keeps
its line endings. After Ctrl-Z and `fg` the mode is set again (bash and zsh
leave the terminal in their own mode), and the editor follows window size
changes. The previous mode is restored when the interactive session ends,
also on a forced exit after a second Ctrl-C. While `dtail` runs in the
background (`bg`, `&`) it leaves the terminal mode alone; like any program
reading its terminal, an interactive `dtail` in the background is stopped
when it reads the terminal ("tty input"), so use `--no-interactive` for
background runs. Where the control TTY is
no terminal, or the mode cannot be set, lines are read as before, without
completion or history.

## Pausing the output

Press Enter on an empty line to pause the output, so that a command can be
typed without log lines scrolling over it. The client prints
`output paused, press Enter to resume or type a command` and the help text.
Press Enter on an empty line again to continue streaming; nothing else has
to be typed.

With the line editor, typing pauses the output by itself: the first key that
puts text on the line, Tab, or recalling a line with Up/Down pauses it, and
entering the line resumes it silently after its answer was printed. When
typing pauses the output, a hint line says so: `(output paused while you
type: Tab completes, :help shows commands and examples, Enter runs the
line)`. Keys that change nothing (PageUp, function keys, unbound Ctrl keys)
do not pause.

While the client asks whether to trust unknown hosts, the line typed is the
answer: its keys pause the output too, but no hint line is printed (it
would land behind the question), Tab completes nothing, and the answer is
not added to the history. Statistics printed with Ctrl-C go through the
line editor, which draws the line being typed again below them; so with
`--interactive-query` and stdout redirected to a file or pipe they go to
the control terminal, not into the redirected output.

`:help` and `:fields` keep the output paused, so their answer does not
scroll away, and the next empty line resumes the output. Typed while the
output runs or is paused only for typing (or without the line editor,
where nothing pauses it), they pause it as if Enter had, and the client
prints `output paused, press Enter to resume or type a command` after their
answer. Typed into an explicit pause (Enter on an empty line), they leave
it paused and print only their answer, as other commands do (below).
Clearing the line and pressing Enter resumes it as well.

While explicitly paused:

* `:show`, `:help`, `:fields` and rejected commands are answered and the
  output stays paused. This includes a `:reload`, `:probe`, `:query` or
  `:tail` line that is rejected (an unknown flag, an unterminated quote, a
  blank `--files`, a query that does not parse, a regex that does not
  compile, `:probe` with arguments, ...): the client
  checks the whole line before it resumes anything.
* `:reload <flags>` resumes the output first and then applies the reload:
  the servers acknowledge an update over the same connection that carries
  the output, which the client does not read while paused. A reload that
  passed the checks but then fails (no server with an active session, a
  server lacking `journal-v1` for a `journal:` target, no acknowledgement)
  has resumed the output already.
* `:query <query>` and `:tail` switch the session with an update as
  `:reload` does, so they resume the output first too.
* `:probe` resumes the output first too, for the same reason: a server's
  probe answer arrives over that connection. Typed into a pause for typing,
  all of them resume the output before they run, without the resumed message.
* `:quit`, closing the control terminal and Ctrl-C (twice) end the session
  as usual; the held back output is released for the shutdown.

With the line editor, a line started while the output is paused shows the
prompt `dtail> ` (after an explicit pause, and after `:help` and `:fields`
kept the output paused); a line started while the output runs has none, as
streaming lines and a prompt would overwrite each other. The prompt of a
line does not change while it is typed, so a pause for typing, which starts
with the line's first key, shows the hint line instead. With colors, the
prompt is painted unless the terminal is narrower than the painted prompt
has characters, escape sequences included (25 with the default theme): the
line editor would then count the cursor one column off, so such a terminal
shows the prompt uncolored.

The pause holds back everything the client writes to the terminal (payload,
`dmap` result tables and the client's own log lines) and, in serverless mode,
the payload written directly to stdout. Nothing is dropped and nothing is
buffered without bound: the client stops reading, so the servers wait (as
for a paused pager) and send the held back output once it resumes. A pause
right at the start of a session, before the client read a server's
acknowledgement of the session start (dserver sends it ahead of the
session's output, but output that is already on its way can come first), does
not fail that start: the client waits for 2 seconds of the
acknowledgement not being held back, and the time it is held back by the
pause does not count, so the session starts once the output resumes. A
`:reload` (and `:query`, `:tail`) waits like that too, but never longer than
10 seconds in all for one server (see below for several). A `dtail` follow session that is paused for long can fall behind the shared
follow read of its file on dserver and then reads privately until it caught
up. A dserver closes a connection without any network activity after its
`IdleSessionTimeoutS` (default 900 seconds), so keep pauses shorter than
that.

## Colors

With the line editor and terminal colors enabled (the default; `--noColor`,
`--plain` and `Client.TermColorsEnable: false` turn them off), the control
output is colored: messages (paused and resumed, success, errors, hints),
the headings, commands and examples of `:help`, the keys and values of
`:show`, the headings, fields and sample values of `:fields`, the prompt and
the Tab candidate lists, whose names are painted by kind (query keywords,
fields, `$variables`, functions, operators, values, tables, paths and
directories) and whose heading and group labels are painted as headings.
MapReduce queries are highlighted with the same styles, by what the query
parser reads each word as: the query of `:show`, the query echoed after an
applied `:reload` (`reload applied successfully`, then `query: ...`, also
without colors) and the query printed above every MapReduce result table
(also outside interactive mode), whose text between the words keeps the
`Client.TermColors.MaprTable.RawQuery*` colors. That query is highlighted
only when stdout is a terminal and no payload goes to a log file (not with
`--logger file` or `--log-payload`); piped output (`dmap ... > out`) and log
files get it as one run of text in the `RawQuery*` colors, so that
`grep 'from STATS select' out` finds it. A word the parser rejects
(e.g. a quoted value after the numeric `==`, where `eq` compares strings)
stays unpainted. A message of several lines (e.g. a `reload failed:` error
naming two problems) is painted line by line. Every style is a foreground color and an attribute (no background, so it
reads on dark and light terminals) in the client config section
`Client.TermColors.Interactive`, for example:

```json
{
  "Client": {
    "TermColors": {
      "Interactive": {
        "Prompt": { "Fg": "Green", "Attr": "Bold" },
        "CandidateField": { "Fg": "Cyan", "Attr": "None" }
      }
    }
  }
}
```

Keys left out keep their defaults; `examples/dtail.json.example` lists every
key with its default, `examples/dtail.schema.json` describes them. Where the
control TTY is no terminal (plain line reading) the control output is never
colored. The line being edited is not colored: x/term's line editor draws it
as typed.

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
  so does the whole `--query`.
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
  `:reload --files '/var/log/my app/a.log'`; every dserver of protocol 5
  reads such paths. A reload to a `journal:` target needs servers with
  journal support (capability `journal-v1`, Linux with `journalctl`);
  otherwise it is rejected with `journal file targets require server
  capability journal-v1: <servers>` before any server switches, and the
  current workload keeps running. To read several files, list them comma-separated, e.g.
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

Switch a running `dtail` to a MapReduce query and back to following lines,
on the same session:

```bash
dtail --servers app01 --files /var/log/app.log --grep ERROR
# then type:
:query from STATS select count($line) group by $hostname
:tail
:tail WARN
```

Build a query with Tab in a running `dtail` whose log has two tables,
`STATS` and `ORDERS` (`item=apple|price=3`, `item=pear|price=5`). Each
`<Tab>` below completes one step or lists what can come next (shown
indented); only the letters before a `<Tab>` are typed:

```
:query <Tab>                          -> :query from
  table: ORDERS  STATS
:query from OR<Tab>                   -> :query from ORDERS
:query from ORDERS <Tab>              -> :query from ORDERS select
:query from ORDERS select <Tab>
  select item
  aggregation: count  sum  min  max  avg  last  len  percentage  percentile
  field:       item  price
  variable:    $caller  $cgocalls  $cpus  $empty  $goroutines  $hostname  ...
:query from ORDERS select ma<Tab>     -> :query from ORDERS select max(
:query from ORDERS select max(pr<Tab> -> :query from ORDERS select max(price)
:query from ORDERS select max(price) <Tab>
  next: where  group by  order by  limit
  (, adds another item)
:query from ORDERS select max(price) gr<Tab>
                                      -> ... max(price) group by
:query from ORDERS select max(price) group by it<Tab>
                                      -> ... max(price) group by item
```

Only the fields and `$variables` of `ORDERS` lines are offered (no
`currentConnections`, and no `$date`, which only the `STATS` lines'
timestamps fill). Enter runs the query on the session: `query applied`,
and a result table with `max(price)` `3.000000` and `5.000000` about
a second after the last server's first aggregates (they send them 5 seconds,
the default `interval`, after the query started), then every 5 seconds
whenever it changed, while no followed line is printed. `:tail` goes back to
following lines (`following lines`, `regex: "..."`), and the next
`:query` starts another query, all on the same session. This session is
`TestInteractiveGuidedQuerySession` in `integrationtests/`.

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

* On startup, an interactive client starts its workload with a
  `SESSION START` handshake. Every dserver of protocol 5 supports it; a
  server of another protocol version is rejected, and a server that rejects
  or does not acknowledge the `SESSION START` fails that connection (exit
  status 1 for `dcat` and `dgrep`). There is no fallback to the plain
  command stream that non-interactive clients use.
* A live `:reload` reuses the existing SSH session of every server and sends
  `SESSION UPDATE` messages instead of reconnecting. If a server rejects the
  update, the servers already updated are rolled back and the current
  workload keeps running.
* This all-or-nothing rule covers the servers whose session is up. A server
  without an active session (down, unreachable, or its session ended) does
  not fail the reload: the reload is applied to the others, and its answer
  names the servers that were not updated (`servers without an active
  session were not updated: ...`). `dtail` reconnects such a server with the
  reloaded workload; `dcat`, `dgrep` and `dmap` do not reconnect. A server
  still connecting starts with the reloaded workload (`servers still
  connecting start with it: ...`), and one whose session came up with the
  previous workload while the reload ran is updated with the others (and
  can fail the reload like them). Only
  when no server has an active session is the reload rejected (`no server
  has an active session`). The `journal:` capability check covers the
  servers that are up; a server that connects later without journal support
  fails its session as at startup. A server whose session start already
  went out with the reloaded workload when the reload failed, and was not
  acknowledged within 10 seconds, is switched back to the current workload
  right after its start is acknowledged (the answer says `rolled back once
  it is acknowledged`); if that fails, its connection ends. Until the
  switch back is acknowledged (one round trip), what that server sends for
  the reloaded workload reaches the client: its lines are printed, and with
  `dmap` its results are merged into the current aggregation (an
  interactive `dtail` that the failed reload switched from following lines
  to a query can print them as `AGGREGATE|...` lines). A reload entered
  meanwhile treats that server as still connecting.
* A reload changes the servers one after the other and waits for each at
  most 10 seconds, and as long again for each rollback, so with servers
  that all stall a failed reload can take about 20 seconds per server
  before it is answered.
* Every successful reload advances a generation boundary; late output from the
  previous workload is dropped so stale matches do not leak into the new result
  stream.
* An interactive `dtail` runs, with or without `--query`, on one client that
  can hold both workloads on the same session: following lines and a
  MapReduce query. Its session switches from one to the other and back
  (`:query`, `:tail`) with a `SESSION UPDATE`, without a new connection, which every protocol 5
  dserver and the serverless runtime accept. Query results are printed (or
  written to the query's outfile) only while a query runs; when the session
  goes back to following lines, the query's last result is reported once
  more and its state is dropped, and a query started later begins with an
  empty result. With an outfile that last result (all received so far) is
  always written. With `outfile append` it is the only result of the query
  that is appended (interim reports append nothing), and so is the result
  of a query a `:reload` or `:query` replaces with another query. Without one it is printed only if the table has rows and
  differs from the table printed last (an empty or unchanged result is not
  printed again), and, as `dtail` prints each interval's result on its own,
  it covers only what arrived since the last printed report. Either way it
  can lack up to one `interval` of the query: the servers drop what they
  aggregated since their last periodic report when the query is
  replaced. The query reads the lines of
  its table only, so the `--grep`/`--regex`, `--invert`, `--before`,
  `--after` and `--max` of following lines do not apply to it: they are set
  aside and apply again once the session follows lines (a set-aside
  `--invert` is noted in one line). This holds for
  `dtail --query` started interactively too: its command line's
  `--grep`/`--regex`, `--invert`, context and `--max` are kept for following
  lines (a note instead of the `Ignoring --invert` warning) and not sent
  with the query. While the session follows lines, a `--plain` line that
  starts like a MapReduce aggregate frame (`AGGREGATE|...`) is printed like
  any other line; while a query runs (or a switch to one is under way) such
  a line is taken for aggregate data, as with `dtail --query` and `dmap`. A
  non-interactive `dtail` keeps the client it always had: the tail client,
  or the MapReduce client with `--query`. Exit statuses do not change.
* In the background the client asks up to three of the servers which
  MapReduce tables, fields and `$variables` the lines of the session's files
  carry (a probe: a bounded sample of the last lines of each file, see
  `docs/dev/probe.md`) and caches the answers for the query completion:
  right after the session started, after every applied `:reload`, on
  `:probe`, and every 60 seconds while the output is not paused. `:fields`
  prints what the answers hold. A probe neither changes the
  workload nor prints anything, nor does it affect the exit status. Its
  answer is read by the same connection that prints the output, so while the
  output is paused no answer of a server arrives (in serverless mode,
  whose output bypasses that connection, the answer of a probe sent before
  the pause does, but a probe sent while paused waits, as the in-process
  server logs to the same paused output); the query completion therefore
  reads only what arrived before. After a `:reload` to other files only
  answers about the new files count: `:fields` and the completion show
  nothing from the files read before. The rounds at the start, after a
  `:reload` and on `:probe` (not the periodic ones) also list the
  directories of the session's files on the first server that is up for the path completion of
  `--files` (see "Line editing and completion"). Every interactive session probes right away,
  `dcat` and `dgrep --interactive-query` included. The probe does not
  change how long their session lasts: an interactive session stays open
  after its read ended (dserver keeps an active `SESSION` alive) until
  `:quit`, Ctrl+C or dserver's idle session timeout, probe or not. A
  reconnected server is probed again by the next round, at the latest the
  periodic one.

## See also

* The [DTail Query Language](./querylanguage.md) for `--query` syntax.
* [Log formats](./logformats.md), "Custom probes", for how a log format
  describes its tables and fields to the probe behind `:fields` and the
  query completion.
* `docs/dev/probe.md` for the probe command, its answer frame and the
  client's background prober.
* The [Server Configuration Guide](./server-configuration.md) for the
  server-side settings.