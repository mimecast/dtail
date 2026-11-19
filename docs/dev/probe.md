# Probe command (field discovery and path listing)

The probe command lets a client ask a server which MapReduce tables, fields
and `$variables` the lines of some of its files carry, so that the
interactive query completion can offer them, or (a path listing, see "Path
listing" below) which files and directories beneath a path prefix the
session user may read, for the path completion. It is part of protocol 5;
there is no capability for it.

Code: `internal/protocol/probe.go` (wire types, limits, reasons),
`internal/handlers/probecommand.go` (command), `probe_targets.go` (path
resolution), `probe_answer.go` (sampling, log format probes, size cap),
`probe_list.go` (path listing; directory rule
`sessionuser.PathRules` in `internal/sessionuser/beneath.go`,
journal units `journal.ListUnits` in `internal/io/journal/units.go`,
shared by the sessions of a dserver through `journal.UnitCache` in
`internal/io/journal/unitcache.go`). The
sampler is `probe.Sample` (`internal/mapr/probe`, `internal/io/fs/sample.go`)
and the per-format field extraction `logformat.Probe`
(`internal/mapr/logformat/probe.go`, see `doc/logformats.md`, "Custom
probes").

## Request

The client sends the command

```
probe <base64 of the request JSON>
```

in an ordinary command frame (`protocol <version> base64 <...>`), built with
`protocol.EncodeProbeCommand`:

```json
{"id": "p1", "files": ["/var/log/app/*.log", "journal:app.service"]}
```

- `id`: 1 to 64 ASCII letters, digits, `.`, `_` or `-`, not `-` alone
  (`protocol.ValidProbeID`). It is echoed in the answer.
- `files`: read targets, each a path, a glob or a `journal:` target, written
  as the path word of a read command: a path containing a space, or starting
  with `base64%`, is encoded with `protocol.EncodeReadPath`.
- `table` (optional): makes the probe table-targeted, see "Table-targeted
  probe" below. 1 to 64 bytes of valid UTF-8 without blanks, control or
  format characters, `|` (the log field separator), `,`, quotes or
  backticks (`protocol.ValidProbeTable`), compared exactly (a
  query's `from` clause names a table in upper case). Not allowed with
  `list`. Servers that predate the field ignore it and answer a plain probe.

## Answer

The server answers every probe command exactly once with the hidden control
frame (a marked control frame, see `wire_framing.md`)

```
.syn probe <id> <base64 of the answer JSON>
```

`protocol.DecodeProbeAnswer` decodes it:

```json
{
  "id": "p1",
  "defaultLogFormat": "default",
  "formats": {
    "default": {
      "tables": {"STATS": {"currentConnections": ["0", "3"]},
                 "ORDERS": {"orderID": ["42"]}},
      "tableVariables": {"STATS": {"$caller": ["stats.go:56"]},
                         "ORDERS": {"$caller": ["orders.go:10"]}},
      "variables": {"$caller": ["stats.go:56", "orders.go:10"], "$hostname": ["web1"]}
    }
  },
  "filesSampled": 1,
  "linesSampled": 500,
  "truncated": false,
  "errors": [{"file": "/var/log/secret.log", "error": "read: no permission to read file"}]
}
```

- `defaultLogFormat` is the server's `MapreduceLogFormat` (`default` when
  unset), the format of a query with a table and without `logformat`.
- `formats` holds every registered log format (`logformat.Names`, built-ins,
  templates and proprietary ones) whose probe found something in the sample,
  as `logformat.ProbeResult` has it: tables (the default format's
  `MAPREDUCE:<TABLE>`, `""` for formats without tables) mapping fields to up
  to 5 distinct sample values of at most 64 bytes, `$variables` with sample
  values, and an optional note. Formats that parse no line are left out.
  `tableVariables` maps a table to the `$variables` the lines of that table
  filled (with samples), so that a query `from TABLE` can be offered only
  those; `variables` stays the format-wide view (of all lines, plus the
  format's known variables without values). `tableVariables` has an entry
  for every table of `tables`, also for a format of one table (formats
  without tables have the table `""`), empty for a table none of whose
  sampled lines filled a variable (a table only the window scan found); it
  never holds known variables without values. Only a format whose custom
  probe gives no per-table data leaves it out; its tables then have the
  format-wide `variables` (`logformat.ProbeResult.VariablesOf`, client
  `answerTableVariables`). Per table, the variables are capped like fields
  (256).
- `table` echoes the request's `table` of a table-targeted probe.
- `errors` lists `{file, error}` entries: `file` is the request entry (or,
  for `probe: log format probe failed`, the format name; empty for errors of
  the whole request), `error` a fixed reason naming no server-side path or
  error detail, which stay in the server log: the read reasons of failed
  commands (`read: no file to read`, `read: no permission to read file`,
  `read: unable to read file`, `read: more files than the server reads`,
  `read: unable to parse command`) and the `protocol.ProbeError*` reasons
  (`probe: invalid request`, `probe: another probe is running`,
  `probe: more files than a probe samples`, `probe: journal targets need
  capability journal-v1`, `probe: timed out`, `probe: log format probe
  failed`, and for path listings `probe: path listing disabled` and
  `probe: unable to list`). A request that does not decode, or has an
  invalid id, is answered with id `-` and `probe: invalid request`.

## What the server samples

Every entry goes the way of a read command's path, with the same checks:

1. `protocol.DecodeReadPath` (as `readCommand.parsePath`); an undecodable
   word is `read: unable to parse command`. `-` (stdin in serverless mode)
   and blank entries are invalid: a probe never reads stdin.
2. A `journal:` target needs the server's `journal-v1` capability; otherwise
   `filepath.Clean` and one `filepath.Glob` (no retries), capped at
   `MaxGlobTargets`.
3. Every match passes the session user's `readfiles` permission check
   (the one of `ServerHandler.PrepareReadTarget`, symlinks resolved first).
   Denied matches are not sampled; non-regular files are skipped silently
   unless nothing else matched.

Logging: a probe (field probe or path listing) checks up to thousands of
paths in every round of every interactive session, so its checks
(`ServerHandler.probeReadTarget`, `sessionuser.User.ResolveReadTargetQuietly`)
log about each path at debug level only, as does `Probe: unable to read
file` for a denied or unreadable match; the result is that of a read's
check. Each probe logs one info line, `Probe answered`, with the counts of
its answer. A field probe also logs one warning, `Probe: unable to read
entry <entry> <reason> matches <n>`, per request entry and reason it could
not read (a denied file, a file no allow rule covers, a glob matching only
denied files or none; at most 8 entries, the entry cut to 256 bytes), so
these stay visible at the default log level, with the number of matches
instead of one line per glob match. A read command's own check keeps its info lines (`User with OS
file system permissions to path`, `Permission test passed partially`, ...).
The permission regexes are compiled once per session user, not per check.
Listing a directory of 500 permitted files used to log 1000 info lines and
take about 13 ms and 10.8 MB of allocations with one rule (83-171 ms and
60 MB with five); it logs none per file and takes about 6 ms and 1.7 MB
with one or five rules (`BenchmarkProbeList500` in
`internal/handlers/probe_logging_test.go`).

At most `protocol.ProbeMaxFiles` (8) files are sampled per request, over all
entries and glob matches; more entries, or matches beyond, are dropped with
`probe: more files than a probe samples`. A file named twice (by a path and
a glob, or by a symlink and its target: same resolved path) is sampled once
and takes one slot. Each file is sampled with `probe.Sample`: an
uncompressed file's last 256 KiB (at most 500 lines), a compressed file's
first 500 lines, a journal target's `journalctl -n 500`. A tail sample's
separately returned first line is put in front of its lines, so header
formats (csv) name their fields; it is not counted in `linesSampled`.
The window is streamed (`fs.SampleTail` reads it through a 64 KiB buffer),
and every line of it, also those the 500-line limit evicts, is passed to
the formats' table scanners (`fs.SampleOptions.Observe`,
`logformat.ProbeTableScanner` over parsers implementing
`logformat.ProbeTabler`: the default format names the `MAPREDUCE:<TABLE>`
of a line without parsing its fields). The answer lists every table of the
window so (without fields, and with an empty `tableVariables` entry, when
none of its lines was kept), so the table list is as complete as the window
allows and the client can target a table probe at a table it found. A
`ProbeTabler` that panics is recovered, not used for the rest of the probe,
and reported as `probe: log format probe failed` for its format (logged at
WARN), like a failing log format probe.
A probe takes no cat or tail limiter slot. Every step is bounded by size
(8 entries, one glob each capped at `MaxGlobTargets`, 8 bounded samples);
the timeout `probeTimeout` (10 s) stops the sampling, `journalctl`
included, and the files not sampled by then are reported as `probe: timed
out`. The glob, the permission checks and the log format probes do not
check it.

The answer JSON is at most `protocol.ProbeMaxAnswerBytes` (48 KiB, before
base64). A larger one first loses all sample values (and notes), then as many
tables, fields and variables as needed, those of the default log format
last; `truncated` is set then. A table's `tableVariables` follow its fields
in that order, so a cut never keeps them without the table, and a kept table
of a format with per-table data keeps its (possibly emptied) entry.

## Table-targeted probe

A table that logs rarely is thin or missing in the plain sample (the last
256 KiB / 500 lines of a file). A request with `table` samples a larger
window for the lines of that table only and answers that table only
(`internal/handlers/probe_table.go`):

- The same checks and bounds as a plain probe: every entry goes the way
  described above (decode, glob, `readfiles` check of every match), at most
  8 files, the 10 s `probeTimeout`, one probe at a time per session (it is
  answered busy while another probe of the session runs, and holds the
  in-flight token like any probe), no cat slot.
- It scans `probeTableScanBytes` (4 MiB, 16 plain windows) of each
  uncompressed file's end, and as much decompressed input of a compressed
  file's beginning (`fs.SampleOptions.ScanBytes`), and asks `journalctl` for
  `probeTableScanLines` (5,000) entries of a journal target
  (`fs.SampleOptions.ScanLines`). At most 8 files, so at most 32 MiB read per
  probe; the window is streamed, so memory does not grow with it.
- It keeps the lines of the table (`fs.SampleOptions.Keep` =
  `logformat.ProbeTableScanner.Matcher`), at most 500 lines and 256 KiB of
  line data per file as a plain sample. A line some format's `ProbeTabler`
  assigns to a table (the default format's `MAPREDUCE:<TABLE>`) is kept if
  that is the table, so a line of another table that mentions the name is
  not; a line no `ProbeTabler` claims is kept if it names the table as a
  word of its own (not next to an ASCII letter, digit or `_`,
  `logformat.ProbeTableLineMatcher`), for formats that name tables without
  implementing `ProbeTabler`. Only lines containing the name are looked at
  closer. A panicking `ProbeTabler` is handled as in a plain probe.
- The answer holds, per log format, the requested table only
  (`logformat.ProbeResult.FilterTable`): its fields and its `tableVariables`
  (only what its lines filled; the format-wide ones only for a format
  without per-table data), no format-wide `variables`. A format without that table is left out; a table
  no line names gives no format, with the files still counted as sampled.
  `linesSampled` counts the kept lines.

## Place in the session

- A probe carries no session generation: it neither replaces the running
  `SESSION` workload nor is its answer dropped as stale output.
- One probe runs per session at a time; a probe arriving meanwhile is
  answered at once with `probe: another probe is running`. The next probe
  may be sent as soon as an answer arrived.
- At most one probe answer per session waits to be queued
  (`ServerHandler.probeInFlight`, `startProbe` in `probecommand.go`). A
  probe that arrives after the one before it finished sampling, while that
  one's answer is not queued yet, makes dserver's command input wait for
  it, at most `probeInFlightWait` (500 ms, well under the client's 2 s
  reload acknowledgement timeout), before it samples anything; if the
  answer is still not queued by then, the probe is answered busy. So a
  client that keeps sending probes without reading can not make dserver run
  every probe and keep a goroutine and an answer per probe, and the command
  input is never held for long: a `SESSION UPDATE` behind a probe is still
  applied in time for the client's reload.
- Answers can stay unqueued also while the client reads: the session output
  reads payload before server messages, so during a sustained follow flood
  the probe answers (10 server messages at most) wait until the payload
  lets up. The client's probes time out meanwhile; later ones are answered
  busy as above.
- The busy and invalid-request answers are queued from the command input
  without waiting (`trySendProbeAnswer`): when the server messages are full
  they are dropped (logged), and the client's probe times out. A client
  that reads gets the previous answer queued at once, so the next probe
  waits for nothing else and is not told it is busy.
- A probe never sends `.syn command failed`, so it changes no exit status.
- A probe never ends the session (`commandShutdownIfWanted` in
  `internal/handlers/serverhandler.go`): a session whose only command is a
  probe stays open. If the session's other work finished while a probe ran,
  and so left the idle shutdown to it, the probe starts that shutdown when it
  finishes, after its answer, so the close handshake still comes last.
- It works the same in serverless mode: the in-process server sends the
  answer frame over the in-process transport.

## Path listing

A request with a non-empty `list` (and no `files`) asks for the entries of a
directory instead of fields:

```json
{"id": "l1", "list": "/var/log/ap"}
```

The server splits the prefix after its last `/` into the directory part
(`/var/log/`; none means dserver's working directory, so send `./` to list
it) and the start of the names (`ap`), and answers with the same frame:

```json
{"id": "l1", "dir": "/var/log/", "entries": [{"name": "apache2", "dir": true}, {"name": "app.log"}], "filesSampled": 0, "linesSampled": 0, "truncated": false}
```

`dir` is the directory part as sent, `entries` the base names, sorted, with
`dir: true` for directories (also symlinks to one). `list` is a plain path
(not encoded like a `files` entry) of at most 4096 bytes
(`protocol.ProbeListMaxPrefixBytes`), valid UTF-8 without NUL; a request with
both `files` and `list`, or another `list`, is `probe: invalid request`.
`Server.PathCompletionDisable: true` answers every listing with
`probe: path listing disabled` (field probes still work).

What a listing offers is bounded by the session user's `readfiles`
permissions (`probe_list.go`). It offers no file the user may not read; it
may offer the names of some directories beneath which nothing is readable
(see the directory rule):

- A file is offered only when the read permission check of a read command
  (as `ServerHandler.PrepareReadTarget`, logging at debug level: symlinks
  resolved, the OS/ACL check, then the rules, deny wins) allows it. A symlink is offered as what it
  points to; dangling and looping symlinks, sockets, FIFOs and devices are
  never offered.
- A directory is offered unless the rules show that nothing beneath its
  resolved path is readable (`sessionuser.PathRules.MayReadBeneath`, the
  user's rules compiled once per listing): it is hidden when no allow rule
  can still match a path starting with `<dir>/`, or when one deny rule
  already matches `<dir>/` itself, with `$` counted as matched at its end.
  Each rule's regex runs as an NFA over the directory path
  (`regexp/syntax`). So `^/var/log/app/` offers `/`, `/var`, `/var/log`
  and `app`, but not `/var/log/secret`; `!^/var/log/private/` and
  `!^/var/log/secret/.*$` hide `private` and `secret` (and, erring towards
  hiding, `!^/x/$` hides `x`). This is an approximation, and directory names
  are revealed by it: a directory denied only by what a deny rule requires
  after its `/`, or only by several deny rules together, is offered
  although nothing beneath it is readable. For example `^/var/log/.*\.log$`
  with `!^/var/log/private/.*\.log$` offers `private`, and `^/data/` with
  `!^/data/x/[^/]` offers `x`. Such a name is always one an allow rule
  covers, and a glob read's `no permission` versus `no file` answers reveal
  as much; to hide a directory reliably, deny the directory path itself
  (`!^/var/log/private/`). An unanchored allow rule (e.g. `\.log$`) makes
  every directory listable. Like the read check, dserver's own scheduled and
  continuous jobs are not restricted.
- The listed directory itself must pass the same directory rule. A directory
  that fails it, does not exist, is no directory or can not be read is
  answered as an empty listing without error, so the answer tells none of
  these apart.
- The directory part of the prefix is made absolute and cleaned lexically
  (`filepath.Abs`) before its symlinks are resolved, so a `..` removes the
  component before it whatever that is: `/a/missing/../b/` and
  `/a/forbidden/../b/` both list `/a/b/`. Resolving the raw prefix would
  need every component before a `..` to exist and be a directory, an
  existence oracle of forbidden directories. Cleaning can not widen the
  listing: every check is made on the cleaned absolute path with its
  symlinks resolved (as a read's permission check resolves them), so
  relative prefixes and symlinked directories are just other paths under
  the same rules, and `link/..` removes the symlink, not its target. A
  read cleans its path lexically too (`filepath.Clean` before the glob), so
  listing and read agree: a name listed for a prefix with `..` is the file
  a read of the prefix as typed reads. Nothing is listed recursively.
- Names starting with `.` are offered only when the name prefix starts with
  `.`; names that are not valid UTF-8 are never offered.
- Bounds: at most 100,000 names are read from the directory, the
  permissions of at most 5,000 names matching the prefix are checked, at
  most 500 entries are answered (`protocol.ProbeListMaxEntries`, the first
  in sorted order), the answer JSON stays within 48 KiB (the last entries
  go), and the probe's 10 s timeout applies (`probe: timed out`). The
  scan keeps only the first 5,000 matching names in sorted order (those
  the checks reach, holding at most 10,000 at a time), so its memory does
  not grow with the directory. Reaching any bound sets `truncated`, also
  in an answer without entries: the client takes a listing without
  `truncated` as complete and narrows it locally, so when the first 5,000
  names are all denied and a permitted one sorts after them, `truncated`
  makes it fetch the narrower prefix that offers it. A scan cut off by the
  timeout answers `probe: timed out` with `truncated`, also before any name
  matched. Accepted oracle: `truncated`, with or without entries, can
  reveal that more than 5,000 names not offered share a prefix (or that a
  listable directory holds 100,000 names). The time a listing takes
  reveals their number more precisely anyway (some µs per checked name),
  and a user with local write access can probe for a single denied name
  by creating names around it; a read's "no permission" versus "no file"
  answers reveal single names as well.

A prefix starting with `journal:` lists systemd units instead: `dir` is
`journal:` and the entries are the units of `journalctl --field
_SYSTEMD_UNIT` (at most 10,000 read) whose names start with the rest of the
prefix and whose target `journal:<unit>` the permissions allow (the rule
must match the whole target, as for journal reads). When no allow rule of
the user can match a target starting with the prefix, the listing is empty
and `journalctl` is not run. It needs the server's
`journal-v1` capability (otherwise `probe: journal targets need capability
journal-v1`); a failing `journalctl` is `probe: unable to list`.

Listing the units reads the whole journal (about 2 s of CPU and 560 MB RSS
for a 3.4 GB journal with 2,743 units), so dserver shares the unit list
between all its sessions (`journal.UnitCache`, created once by
`handlers.NewJournalUnitCache` and passed as `Dependencies.JournalUnits`):
a successful list is reused for 60 s (`journal.DefaultUnitCacheTTL`), so a
unit that appears later is offered after at most a minute; a failed run is
not cached. Listings that arrive while `journalctl` runs wait for that run
instead of starting their own; the run belongs to none of them, so no
probe's 10 s timeout bounds it. It is stopped once every listing waiting
for it gave up (its probe timed out or its session ended), so one
session's timeout fails no other, and at the latest after 30 s
(`unitFlightTimeout`), so listings that keep arriving while earlier ones
time out can not keep a hung `journalctl` alive. A panic of the lister is
recovered into the run's error, which the waiting listings log with its
stack at WARN and answer `probe: unable to list`. The cache
holds the units only, at most 10,000 names; every listing filters them by
its own session user's permissions as above. Serverless mode has no shared
cache and runs `journalctl` per listing.

A listing runs like a field probe: one probe (of either kind) per session at
a time, no limiter slot, no session generation, never a failed command, and
it works the same in serverless mode.

## Client: background prober and field catalog

Only interactive sessions (`--interactive-query`) probe, in every mode
(fields are also useful before switching to `--query`). Code:
`internal/clients/interactive_probe.go` (`interactiveProber`),
`internal/clients/interactive_catalog.go` (`fieldCatalog`) and
`internal/clients/handlers/probe.go` (`handlers.Prober`).

- Routing: `baseHandler.handleHiddenMessage` hands a marked control frame
  starting with `protocol.HiddenProbePrefix` to `handleProbeMessage`, which
  decodes it and queues the answer (4 at most) without blocking the frame
  reader; an answer that does not decode or does not fit is dropped and only
  logged at debug level. An unmarked frame reading like an answer is payload.
- `handlers.Prober` (`ClientHandler`, `MaprHandler`): `Probe(ctx, request)`
  waits for the capabilities frame (the session is up), drops the answers
  still queued (late answers of requests given up on), sends the command and
  waits for the answer with the request's id, skipping an answer with
  another id still on its way. One call per handler at a time, matching the
  server's one probe per session: a concurrent call waits for the running
  one (the handler's probe slot) as long as its context allows. Every wait
  ends with the context or the session.
- Decision N1, on-demand requests share the slot: a path listing asked for
  by Tab while a background field probe runs (in server mode possibly held
  up by a paused output, up to the 30 s round timeout) waits for that probe
  rather than failing or cancelling it. Cancelling would free only the
  client's slot: dserver goes on sampling (up to 10 s) and would answer the
  listing with `probe: another probe is running`. Waiting costs at most the
  rest of the running probe, and as Tab lets the output run while it waits,
  the running probe's answer is read at once. Tab's own wait stays bounded
  (1 s, see below); the listing request goes on behind it.
- `interactiveProber` (started by `startInteractiveControl`, waited for when
  the session ends) runs one round at a time: right away (each probe waits
  for its session to come up), on `request()` (after every applied
  `:reload`; `:probe` via `probeNow`), and `probeInterval` (60 s) after the
  previous round while the output is not paused (`interactivePause` tells
  it). A round sends the session's current files (`protocol.EncodeReadPath`
  path words, stdin left out, at most `ProbeMaxFiles`) to three
  connections whose handler probes and whose session did not end, those
  with their session up first (see "Which connections"), read
  afresh each round so reconnected connections are used, concurrently and
  bounded by `probeRoundTimeout` (30 s). Failures are logged at debug level
  only; the prober prints nothing.
- Which connections (`probeTargets`): at most three, the connections whose
  session is up (the capabilities frame arrived, `WaitForCapabilities(0)`)
  first, then those still connecting, each in the client's order; those
  still connecting only fill the slots left by the ones up. A connection
  whose session ended (it waits for its reconnect, which replaces it) or
  whose handler cannot probe is never asked. A connection whose `Start`
  ended without a session (refused dial, failed handshake, a context that
  ended before a throttle slot was free) has its handler shut down
  (`ServerConnection.shutdownHandlerWithoutSession`), so it counts as
  ended: a server first in `--servers` that is down neither keeps a round
  (and `:probe`) waiting for the round timeout nor takes the path
  listings. A probe of a connection still connecting waits for its session
  to come up, up to the round timeout, or until its dial fails. A
  reconnect starts no round of its own: the reconnected connection is
  asked by the next round, at the latest the periodic one within 60 s
  (while the output is not paused).
- Every interactive session probes right away, the one-shot ones of `dcat`
  and `dgrep --interactive-query` included. The probe does not change when
  such a session ends: an interactive session is an active `SESSION`,
  which dserver never shuts down as idle (`claimIdleShutdown` returns false
  while `sessionCommandState.keepAlive()`), so it stays open after its read
  ended, with or without a probe, until the client ends it (`:quit`,
  Ctrl+C, end of the control TTY) or the idle session timeout. The probe's
  `commandShutdownIfWanted` (see "Place in the session") only matters while
  no `SESSION` is active.
- `fieldCatalog` keeps each server's latest answer with the files it was
  asked about (an answer reporting only `probe: another probe is running`
  is ignored) and merges, on `snapshot(files)`, the answers about the
  session's current files (in any order;
  `interactiveProber.catalogSnapshot`): after a `:reload` to other files
  the answers about the old ones no longer count, also those of servers
  not asked again (more than 3 servers, a reconnect, a failed probe), and
  count again once the session is back on those files. Per log format the union of tables, fields and `$variables`
  with at most 5 distinct sample values each, the distinct notes, the default
  log format most servers name (on a tie the one that reached the top count
  first, going through the servers sorted by name), the source servers, the time of the latest
  answer, `truncated` and the servers' errors. Tab completion and `:fields`
  (`fieldsText`, `internal/clients/interactive_fields.go`) read only this
  cache.
- `:probe` (`baseClient.runProbeCommand`) resumes the output first (see
  below), then `probeNow` requests a round and waits up to
  `probeNowWait` (5 s) for the first round that started after the
  request (`probeRounds` counts started and finished rounds and keeps the
  last round's `probeRoundResult`), so a round already running with the
  files of before does not count. It reports the servers that answered,
  answered busy (`ProbeErrorBusy`) or failed, or that later answers are
  still stored, and returns at once when the prober stops. Busy answers
  come from dserver only (another probe of the session still ran there):
  the client's own probes share the handler's slot (decision N1 above)
  and never fail busy.
- Per-table `$variables`: the merged catalog keeps them per table
  (`catalogFormat.TableVariables`), and `catalogFormat.variablesOf(table)`
  returns only what that table's lines filled, empty for a table no answer
  gave any for. The only fallback is explicit: an answer's format without
  per-table data (a dserver from before them, a custom probe that sets
  none) contributes its format-wide `variables` to each of its tables
  (`answerTableVariables`), and a catalog format without any per-table data
  returns its format-wide ones. The query completion offers these: once a
  `from` table is typed, only the `$variables` (and sample values) of that
  table's lines (`queryCandidateSource.variableNames`).
- Table probes (`interactiveProber.requestTable`,
  `internal/clients/interactive_probe_table.go`): a table-targeted probe of
  the session's current files is sent to the round's probe targets (at
  most 3) when Tab completes inside a `--query` value whose `from` table is
  typed (`completion.probeTable`, `completeWithTableProbe`; only the Tab
  that sends the request waits for it like for a path listing, at most 1 s
  with the output running, and completes again; a Tab while it is still on
  its way, or whose wait ran out, completes from the cache at once with the
  hint `tableProbeMissingHint`), and
  when `:fields TABLE` is run (`awaitTableProbe`: the table as the catalog
  spells it, else in upper case; waited for at most `tableProbeWait`, 2 s,
  with the output running, before `:fields` answers). It shares the
  on-demand request state of the path listings (one request per table on
  its way, ids `t1`, `t2`, ..., bounded by the round timeout, waited for when
  the prober stops). It is not sent while the catalog holds an answer to a
  table probe of that table and files younger than `tableProbeMaxAge` (60 s,
  `fieldCatalog.hasTableProbe`), also one that found nothing (so a table no
  line names is not probed on every Tab), nor within `tableProbeRetryAfter`
  (5 s) of a table probe of them no server answered (all failed, timed out
  or answered busy, `markTableProbeFailed`; e.g. a server that answers
  nothing while it floods follow output is not asked on every Tab), nor for
  a session reading only stdin or a name `protocol.ValidProbeTable`
  rejects. `requestTable` looks at the pending requests before the catalog,
  under one lock, so a request that just answered is never sent twice. The answers are kept
  per server and table (`fieldCatalog.storeTable`, at most 64, the oldest
  dropped; a busy answer is ignored, and the plain answer of a server that
  ignores `table` is kept for the table too) and merged into the snapshot of
  the same files: tables, fields and per-table `$variables`, the servers
  and times, and `catalogSnapshot.TableProbes` (table to lines sampled),
  but not their errors, which repeat those of the plain probes. `:fields
  TABLE` shows the table's `$variables` (`variablesOf`) under it and a line
  `table T probed on its own: N lines of it sampled`.


### Path completion

Tab inside a `:reload --files` value completes the base name of the last
comma-separated entry (`completeFilesValue`,
`internal/clients/interactive_complete_paths.go`) from path listings:

- Requests: the directory key of the entry (its part up to the last `/`;
  `./` without one, as an empty `list` is invalid; `journal:` for a journal
  target) followed by the start of the names: nothing for a first listing
  of the directory (`.` when the typed name starts with `.`, as hidden names
  are only listed then), the typed name to narrow a truncated listing.
- Cache: the `fieldCatalog` keeps each answer by its request
  (`storeListing`, `findListing` in `interactive_catalog_paths.go`), with
  the server that gave it, for 2 minutes, at most 64 listings. A listing of
  the names starting with `q` serves every typed name starting with `q`
  (not `q` empty for a name starting with `.`); a truncated one is used
  meanwhile and asked for narrower. Busy-only answers are not cached. An
  answer that failed for the moment (`probe: timed out`, `probe: unable to
  list`) is kept for 5 s only (`pathListingTransientMaxAge`, long enough for
  a Tab right after it to show the reason; a `timed out` answer arrives only
  after the server's probe timeout, well after the Tab's 1 s wait) and counts as truncated, so
  it never completes a narrower prefix (the server does not set `truncated`
  when the journal unit list times out or fails).
- Sender: `interactiveProber.listPaths` (`interactive_probe_paths.go`)
  sends the request (ids `l1`, `l2`, ...) to the first connection the
  probe rounds would ask (the first one up, see "Which connections"; while
  none is up, the first one still connecting), once per prefix while it is on its way, with the
  round timeout and not bound to the Tab that asked, so a late answer is
  cached for the next Tab. After every requested probe round (session
  start, `:reload`, `:probe`; once it counted as finished, so `:probe`
  does not wait for it) the prober lists the directories of the session's
  files in the background (`prefetchListings`, called by `run`; no
  globbed directories, no stdin, no `journal:` units (a `journalctl` run
  on the server, listed on Tab only), none whose whole listing is cached and
  younger than 2 minutes, except after `:probe`, which refreshes them so
  that a file created since shows up; neither the next round nor `:probe` waits for
  them), so the files next to the ones read complete at once, also after a
  `:reload` switched to other files. The periodic rounds list nothing: a
  listing makes dserver check every file of the directory, which every
  minute of every interactive session cost noticeable CPU time; once a
  listing expired, the next Tab that needs it fetches it.
- Waiting (`awaitPathListing`, `waitForListing` in
  `internal/clients/interactive_tty_paths.go`): with the listing not
  cached, Tab sends it and waits at most `pathListingWait` (1 s) with the
  output running (`interactivePause.runOutput`, the pause state kept), as
  the answer is only read then: with a remote dserver it is queued behind
  payload; in serverless mode the in-process server logs to the paused
  client output while it handles the request, so it waits the same way.
  Before the output runs the edit line is taken off the screen and
  afterwards as many empty rows are put below the output as the Terminal
  moves up to redraw the line (`editorScreen`). When no listing came in
  time (or the request failed), a hint says so and the request goes on.
  A listing answered with an error reason instead of entries shows it as
  a hint, Go-quoted unless plainly printable (`displayText`, as `:fields`
  shows server text), so the reason cannot write control sequences.
- Candidates: directories get a trailing `/`, names are escaped for the
  shell quote they are typed in (`escapeForQuote`, the common prefix taken
  on the decoded text); names with a comma, blanks at either end (which
  `--files` trims), control characters or format characters (such as
  the bidirectional override U+202E, which could disguise a name on the
  terminal) are not offered. With several
  connections, a listed set of candidates is headed by the server that
  listed it.

Answers while the output is paused (`TestProbeAnswerWhileOutputPaused` in
`internal/clients/interactive_probe_pause_test.go`): with a remote dserver
the payload is framed through the connection handler, and the paused writer
blocks the handler's `Write`, so an answer queued behind pending payload
arrives only after the output resumes. In serverless mode the in-process
server writes payload straight to the gated output, past the handler, and
the answer of a probe it is already running arrives while paused; a request
sent while paused is not handled before the output runs, though, as the
in-process server logs to the paused client output (found with the path
completion's serverless integration test). The query completion therefore
never waits for a server; it uses what the background rounds stored. The
path completion waits at most 1 s, with the output running.
