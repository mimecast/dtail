# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

DTail (Distributed Tail) is a DevOps tool written in Go for distributed log operations across multiple servers. It provides secure, concurrent access to logs on many machines using SSH protocol, supporting tail, cat, grep, MapReduce operations, and auth-key fast reconnect optimization for repeated SSH connections.

## Build Commands

```bash
# Build all binaries
make build

# Build individual components
make dtail      # Client for tailing log files
make dserver    # Server component (required on target machines)
make dcat       # Client for displaying files
make dgrep      # Client for searching files
make dmap       # Client for MapReduce queries
make dtailhealth # Health check client

# Clean build artifacts
make clean

# Enable ACL support (requires libacl-devel)
DTAIL_USE_ACL=yes make build

# Build without zstd (CGO-free cross-compiles; .zst logs unsupported)
DTAIL_NO_ZSTD=yes make build

# Enable proprietary features
DTAIL_USE_PROPRIETARY=yes make build

# Build PGO-optimized binaries (requires existing profiles)
make build-pgo

# Generate PGO profiles and build optimized binaries
make pgo
```

## Testing & Development

```bash
# Run all tests (unit tests only)
make test

# Run all tests including integration tests
# IMPORTANT: Always rebuild binaries before running integration tests
make clean && make build
DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test

# Quick integration test workflow (recommended)
make build && DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test

# Run linting
make lint

# Run go vet
make vet

# Run integration tests individually (requires binaries built first)
cd integrationtests && go test
```

## Benchmarking

```bash
# Run all benchmarks
make benchmark

# Quick benchmarks (subset of tests)
make benchmark-quick

# Full benchmarks with longer runs
make benchmark-full

# Create a baseline for comparison
make benchmark-baseline

# Create a quick baseline (small files only, will prompt for name)
make benchmark-baseline-quick

# Compare current performance against a baseline
make benchmark-compare BASELINE=benchmarks/baselines/baseline_TIMESTAMP.txt

# Compare the local fork with upstream (sibling checkout of github.com/mimecast/dtail)
make benchmark-upstream-smoke   # Compile both trees, tiny correctness checks, no timings
make benchmark-upstream         # Paired end-to-end scenarios; run only on an idle host

# Drop system caches before a benchmark run
make drop-caches
```

## Profile-Guided Optimization (PGO)

```bash
# Full PGO workflow: generate profiles and build optimized binaries
make pgo

# Quick PGO with smaller datasets (faster)
make pgo-quick

# PGO for specific commands only
make pgo-commands COMMANDS='dcat dgrep'

# Generate PGO profiles only (without building)
make pgo-generate

# Build PGO-optimized binaries using existing profiles
make build-pgo

# Install PGO-optimized binaries to system
make install-pgo

# Clean PGO artifacts
make pgo-clean

# Show PGO help
make pgo-help
```

### PGO Notes

- PGO provides additional performance improvements on top of DTail's default optimized read/output path
- Measured improvements are workload-dependent and modest: on a 100 MB
  serverless run, DGrep ~7-8%, DCat ~3%, DMap within measurement noise. PGO
  output is byte-identical to non-PGO output (verified with `cmp`).
- Profiles are saved in `pgo-profiles/` directory
- `pgo-profiles/` and `pgo-build/` are **gitignored**: profiles are regenerated
  locally with `make pgo` / `make pgo-generate`, not committed. They are
  workload-specific; regenerate them after significant hot-path changes.
- Optimized binaries are built in `pgo-build/` directory
- Use `make build-pgo` to rebuild optimized binaries without regenerating profiles
- The tooling (`verifyProfileNonEmpty` in `internal/tools/pgo`) fails loudly
  on missing, 0-byte, or zero-sample profiles, so an idle-server or
  I/O-bound capture can no longer silently produce an empty/zero-sample profile.
- **All five commands are profiled by default.** The `dtail-tools pgo` default
  command set covers dcat, dgrep, dmap, dtail and dserver. dtail used to be
  excluded because its follow client never returned from `client.Start` under
  `-shutdownAfter`, SIGINT or SIGTERM (the "auto shutdown does not work" bug
  noted in `cmd/dtail/main.go`), producing a 0-byte `dtail.pprof`; that follow
  shutdown is now fixed. PGO profiles dtail with a dedicated live-server
  follow workload (`runDtailWorkload` in `internal/tools/pgo`: live dserver,
  deterministic keypair auth, `-shutdownAfter 6`, plus a handshake-failure
  representativeness guard). The separate `internal/tools/profile` profiling
  harness still covers dcat, dgrep and dmap only.

## Profiling

```bash
# Profile all commands (dcat, dgrep, dmap)
make profile-all

# Quick profiling with smaller datasets
make profile-quick

# Profile dmap MapReduce queries
make profile-dmap

# List available profiles
make profile-list

# Analyze a specific profile interactively
make profile-analyze PROFILE=profiles/dcat_cpu_*.prof

# Open the web interface for a profile (go tool pprof -web)
make profile-web PROFILE=profiles/dcat_cpu_*.prof

# Clean all profile data
make profile-clean

# Show all profiling options
make profile-help
```

### Profiling Notes

- Profiles are saved in the `profiles/` directory by default
- Each command generates CPU, memory, and allocation profiles
- Use `go tool pprof` for detailed analysis of profile files

## Test Execution Details

- Integration tests require binaries to be built before execution
- **IMPORTANT:** Always recompile binaries after code changes before running integration tests:
  ```bash
  make clean && make build
  DTAIL_INTEGRATION_TEST_RUN_MODE=yes make test
  ```
- Integration tests are run by setting DTAIL_INTEGRATION_TEST_RUN_MODE to yes, and by running 'make test'
- `DTAIL_INTEGRATION_TEST_RUN_MODE=yes` is a compatibility shortcut for the
  unchanged test harness. During config initialization it supplies
  `HostnameOverride=integrationtest`, `KnownHostsPath=./known_hosts`,
  `AuthorizedKeysPath=<DTAIL_AUTH_KEY_PATH>.pub` (or `./id_rsa.pub` when that
  key variable is unset), and
  `HostKeyPath=./ssh_host_key` only when those values were not set explicitly.
  Runtime handlers and SSH packages consume these fields and do not inspect the
  integration-mode environment variable themselves.
- Integration tests verify: DCat, DGrep, DMap (MapReduce), DServer, DTail, DTailHealth, and auth-key fast reconnect functionality
- All tests run with race detection enabled (`--race` flag)
- Interactive integration tests (`integrationtests/interactive_*_test.go`,
  `interactive_e2e_test.go` for query completion through to the result
  table, control colors on/off and permission-filtered path listings, the
  latter serverless with `Server.Permissions` from the client's `--cfg`) drive
  `dtail`/`dmap` on a PTY with `runInteractivePTYCommand`
  (`integrationtests/interactive_pty_harness_test.go`, python3 `pty`;
  `interactiveStep.Raw` sends keys without Enter, `Delay` sleeps before the
  input). A `WaitFor`/`WaitForFile` is an assertion: if it does not appear
  within 30 s, or the process exits first, the harness kills the process and
  the run fails naming the step (`ptyStepFailure`); the end of the test
  context kills the process too. A `WaitFor` matches anywhere in the output
  so far, so wait for text only the step produces (e.g. `invert`, not
  `servers=`, which client STATS lines carry too; a data line rather than a
  file name a listing or log line may also carry), and keep edited lines
  under the PTY's 80 columns: a wrapped line is not drawn as one piece.
  `interactive_default_test.go` checks that `dtail` is interactive on a
  terminal without `--interactive-query` and not with `--no-interactive`;
  `pty_harness_test.go` tests the harness itself (no binaries needed).

## Known Limitations

### Interactive Query Reload
Interactive query control is opt-in on the client with `--interactive-query`,
except for `dtail`, where it is the default when stdin and stdout are a
terminal and the control TTY opens (`cli.InteractiveDefault` in
`internal/cli/interactive_default.go`; `--no-interactive` disables it and wins,
an explicit `--interactive-query[=false]` is kept). The logic lives in
`internal/cli` because the build compiles each command from its single
`main.go`. Scheduled and continuous jobs are never interactive: `NewMaprClient`
forces it off for every mode but `DefaultMode`. `:help` prints per-mode
`:reload`, `:query` and `:tail` examples (grep filter, MapReduce query,
switching between them, files), each checked against
the real parser by `TestInteractiveHelpExamplesParse`.
On a terminal the control loop edits lines with `golang.org/x/term`'s
`Terminal` (`interactiveEditor`, `internal/clients/interactive_tty.go`) in a
cbreak mode that keeps ISIG/OPOST/ICRNL (`enterCbreakMode`,
`ttymode_*.go`); `term.MakeRaw` is not used, as it would disable Ctrl-C and
staircase the log output. Tab completion (`completeInteractive`,
`interactive_complete.go`) offers commands, the mode's flags from
`newInteractiveReloadFlagSet` (shared with the parser), the current
`--grep/--regex/--query/--files` value (not one holding a control or format
character, which the `:reload` tokenizer can not escape and x/term would
write raw: `currentValueCandidate` then gives `unprintableValueHint`
instead), and inside a `--query` value the
query language and field catalog (`completeQueryValue`,
`interactive_complete_query.go`: `splitInteractiveArgsPartial` maps the
decoded value back to raw offsets, `mapr.AnalyzePartial` says what is
expected, one candidate function per kind from the vocabulary tables and
`interactiveProber.catalogSnapshot`, candidates escaped for the open quote
with `escapeForQuote`; candidates holding a control or format character,
i.e. log content with escape sequences, are dropped for every kind at once
in `queryCandidateSource.groups` with `hasHiddenRune`, the path completion's rune
test; names are written as the tokenizer and parser need them,
`interactive_complete_query_words.go`: a keyword or `by` in backticks,
`order by` items with a blank in double quotes, and inside an open double
quote only the kinds `mapr.AnalyzePartial` expects there (`quotedExpect`),
closed by the quote; an empty catalog adds `queryCatalogHint`, or for a
session without files (`catalogSnapshot.NoFilesHint`) the no-files hint,
`catalogEmptyHint`). A session that reads no files at all
(`interactiveProber.readsNoFiles`, `internal/clients/interactive_nofiles.go`:
no file, and the `-` a serverless session without files gets counts as
reading only in serverless mode with stdin not a terminal, as
`readCommand.isInputFromPipe` reads it only then, while interactive `dtail`
is the default on a terminal) says `the session reads no files: :reload
--files PATH (Tab lists remote paths)` (`local paths` in serverless mode;
Tab note only with the line editor) after the banner (`writeControlBanner`),
on `:fields`, `:probe` and Tab after `:fields `, instead of waiting for
field data; no default files are made up. In such a session (`--files`
value empty or only `-`, `namesNoFile`) Tab right after `:reload --files `
has no current value to insert and completes the value not yet started
like an empty one, i.e. lists the working directory (`filesValueAt` in
`interactive_complete_paths.go`). The query
completion is guided (`interactive_complete_query_guide.go`): one step per
Tab in the order from -> table -> select -> items -> where -> group by ->
order by -> limit (`queryOutline.nextClauses`; a query begun with Tab
always starts with from: `from TABLE ` in one Tab when the catalog knows
one table of the format, `from ` with the tables listed in the same Tab
when it knows several, `completeFromFirst`, `queryCompletion.lead`, and
`from ` with `tableNameHint` while the catalog is empty; select first
when the servers answered without a table of the format, `tableless`,
e.g. csv or plain logs only generic parses; from as the only clause left
later is inserted with its one table too), a single next step inserted at
once;
clauses present on either side of the cursor are not offered again
(`newQueryOutline` runs `mapr.AnalyzeQuery` over the whole query: a word
left of the cursor plus the value's rest, `queryValueTail` for `--query`,
the line's rest for `:query`); after a finished item only the continuation
plus `commaItemHint`/`commaConditionHint` (also right after the `)` of a
finished item, `count(x)|`, or a where value ending in `)`: Tab lists
that, even one clause, and inserts the blank only at the query's end, never
before a blank, comma or word, `completeAfterItem`,
`queryCompletion.afterItem`/`lead`; a comma typed as the very next
key with the cursor right after that blank replaces it, `completion.itemBlank`,
`interactiveEditor.itemBlank`, `commaAfterItemBlank`, not in vi normal
mode), after a comma
(`mapr.Partial.AfterComma`) the items again; value positions offer no
fields; rare clauses (set, interval, outfile, logformat, rorder) only for a
typed prefix; fields, `$variables` (`catalogFormat.variablesOf`) and values
only of the query's table, wherever it is typed (`tableFirstHint` while
none is). Candidates come in labelled `candidateGroup`s
(`queryCompletion.groups`, `completion.groups`) with a heading of what is
expected (`queryHeading`: "field to count", "value for x", "next", ...);
no heading before the first clause (group `clause:`) or where the next
clauses stand beside other words; the editor prints them with
`interactiveStyle.listing` (`interactive_listing.go`): heading (dropped
when every group's label equals it), one line per label (groups of one
label merged, so `and` and the clauses are one `next:` line;
`aggregation:`, `field:`, `variable:`, fields before `$variables`), names
bare (`bareCandidateName` drops the `(`, `)`, quotes, protective
backticks inside quotes and blank Tab inserts, but keeps the backticks of
a keyword-named field such as `` `from` ``), wrapped to
`editorScreen.columns()` (`displayWidth`: East Asian wide runes count two
columns, from a small range table) and cut after
`listingMaxRows` rows with `... (N more, type to narrow)`, hints on their
own line; heading and labels in the theme's Heading style, names per kind,
plain text without colors. Commands, flags and paths have no groups and
are listed on one wrapped line. The query
completion reads the cache only, except that a typed `from` table not probed
on its own within 60 s has `completeEditorLine` ask for a table probe and
wait up to 1 s with the output running (see "Probe Command"). Inside a `--files`
value the editor completes remote paths (`completeEditorLine` in
`interactive_tty_paths.go` calls `completeFilesValue`,
`interactive_complete_paths.go`, before `completeInteractive`): the last
comma-separated entry's base name from the path listing of its directory
cached in the `fieldCatalog` (`storeListing`/`findListing`,
`interactive_catalog_paths.go`, 2 min, a listing that timed out or
failed 5 s and never for a narrower prefix, at most 64 listings, one server's
view), escaped for the open quote; names with a comma, edge blanks,
control or format characters (e.g. U+202E) are not offered; a listing's
error reason is hinted through `displayText`. After every
requested round (session start, `:reload`, `:probe`; not the periodic
ones, outside what `:probe` waits for) `run` prefetches the directories of
the session's files whose listing is not cached fresh (`:probe` refreshes
fresh ones too; never `journal:`, whose units are listed on Tab only, as
each listing runs `journalctl --field _SYSTEMD_UNIT`), without waiting for them (`prefetchListings`, `interactive_probe_paths.go`). An uncached listing is asked of the first
connection up (`interactiveProber.listPaths`, deduplicated, not bound to the
Tab's wait, so a late answer is cached for the next Tab) and waited for at
most `pathListingWait` (1 s) with the output running
(`interactivePause.runOutput`; the edit line is taken off the screen and its
rows restored by `editorScreen`, through `interactiveEditor.hideLine`/
`showLine`: while it is `hidden` nothing may have the Terminal draw, whose
cursor bookkeeping still describes the drawn line, so a SIGWINCH resize is
deferred until `showLine` (`resizePending`) and control output of another
goroutine, the Ctrl-C statistics, is written to the TTY as it is
(`editorOutput`); `TestInteractiveEditorHiddenLineIsNotRedrawn`. The
output is always paused at such a wait, as `editorKey` calls `startTyping`
before it completes), also in serverless mode, whose in-process
server logs to the paused output; on a timeout a hint is shown. A key that inserts text, Tab or a
history recall pauses the output (`interactivePause.pauseTyping`; x/term only
passes inserting and unbound keys to the callback, recalls are seen through
`controlHistory.onRecall`), the entered line resumes it silently; an explicit
Enter pause stays explicit. Starting a typing pause shows
`interactiveTypingHint` (Tab, `:help`, Enter) above the line (`startTyping`).
While the host key question is asked (`promptAnswers.prompting`, passed to
the editor as `interactiveEditor.prompting`) the line is its answer: keys
still pause, but no hint, Tab consumed without completing, and
`controlHistory.skip` keeps it out of the history. While the editor runs,
the Ctrl-C statistics go through its writer (`stats.setInterruptOutput`,
set by `openControlLines`), which redraws the edit line below them (to the
control TTY, also when stdout is redirected).
`:help` and `:fields` always leave the output paused (`keepPausedAfter`)
until the next empty line. A line started while the output is paused gets
the prompt `dtail> ` (`interactiveEditor.setPrompt`, set only between lines:
x/term draws a prompt at a line's start and counts it in every cursor move,
so changing it mid-line would misplace the cursor; `editorScreen` counts it
too, and after a Tab wait that let the output run `redrawLine` has the
Terminal draw prompt and line again, as `hideLine` erased both). With terminal colors on, the editor's control output is painted by
`interactiveStyle` (`internal/clients/interactive_style.go`, theme
`Client.TermColors.Interactive`, `config.InteractiveTermColors`): messages,
`:help`, `:show`, `:fields`, the prompt and the Tab candidates, grouped and
styled by `candidateKind` (`completion.kind`/`kinds`; query kinds come from
`queryCandidateKinds`). The style travels with the writer
(`editorWriter`, `styleOf`): a nil style (plain line reading, `--noColor`,
`--plain`, any other writer) returns every text unchanged. MapReduce
queries are highlighted by `queryHighlighter`
(`internal/clients/query_highlight.go`, token kinds of
`mapr.AnalyzePartial`, styles of the matching candidate kinds; stripped of
its SGR sequences the output equals the query, `quoted` equals
`strconv.Quote`): the query of `:show`, the echo `query: ...` after an
applied `:reload` (`reloadAppliedMessage`) and the raw query above the
result table (`clientOutputFormatter.PaintMaprRawQuery`, on the
`MaprTable.RawQuery*` colors as the base between tokens; only when stdout
is a terminal and no payload goes to a log file, `rawQueryTokenColors`,
else one single-color run so `grep` finds the query in piped output).
`paint` paints a text of several lines line by line;
`interactiveStyle.failure` first escapes the message's control and format
characters but the line feed (`terminalText`,
`internal/clients/interactive_terminal_text.go`), with and without colors,
as it can end in server text (the reason of a rejected update after `reload
failed:`/`query failed:`/`tail failed:`); those three pass `errorLines(err)`,
which gives every error of an `errors.Join` (also nested) a line of its own
and escapes the line feeds inside each (`terminalLine`), so a server's
reason can not forge a line of control output; `:fields` is built as typed lines
(`fieldsLines`, `fieldsLineKind`) and painted from them
(`interactiveStyle.fields`), never by reading its text again, while `:help`
is painted by its text, which is all constants of dtail
(`TestInteractiveHelpIsPaintedByKind`); the prompt is
unpainted on a terminal narrower than the painted prompt has runes
(`interactiveStyle.editorPrompt`: x/term cuts the prompt into rows by
runes, escape sequences included, and miscounts the cursor). Alt-E and Alt-V (fish's `edit_command_buffer` keys) open the line in
`$VISUAL`/`$EDITOR`/`vi` (`editExternally`,
`internal/clients/interactive_tty_external.go`): x/term reads ESC+letter as
one anonymous key, so `editInput` in front of the Terminal translates the
two sequences (outside bracketed paste) into `externalEditKey`
(`utf8.MaxRune`), which `editorKey` consumes; the editor runs via
`/bin/sh` on a temp file with a fresh descriptor of the TTY in the mode
dtail found it in (cbreak and the window size again after it; while
it runs, `editMu`/`editing`, the SIGCONT watcher sets no mode, a resize
redraws nothing, and a `close` (session end) leaves mode and cursor shape
to the editor, `endEditing` then setting nothing), output paused for
typing; its text (line breaks and control characters as blanks,
`editedLine`) replaces the line, which runs only on Enter; a
failing editor keeps the line. The editor's process dependencies (the
forced-exit hook and the environment naming the external editor) are an
`editorDeps` argument of `newInteractiveEditor`, passed down from
`runInteractiveControl`: `processEditorDeps` for a client run, tests pass
their own (`testEditorDeps`); `editorDeps.watchSignals` is the SIGCONT/
SIGWINCH watcher (`watchTTYSignals`), which tests leave out unless they
test the signals, as both go to the whole test process and would have
every other editor write to its PTY; a nil dependency is the process's
(`withDefaults`). The editor asks the terminal for bracketed
paste (`interactiveEditor.setPasteMode`, DECSET 2004: on at the start and
wherever cbreak mode is set again, i.e. after the external editor and
SIGCONT; off for the external editor, on close and, best effort in a
goroutine as a TTY write blocks on stopped output, on the forced exit;
never while dtail is in the background, `ttyMode.foreground`). x/term then inserts pasted keys without the key callback
(no Tab completion, a line break enters the line), so `editInput` does
what the callback and x/term do not: control and format characters of a
paste (`hiddenRune`, rune by rune, one cut by a read held back) but line
breaks become blanks (`pasteBlank`; x/term would put a tab on the
line raw and an ESC sequence as a private key code), a `Read` ends after
the first line break (`chunk`) so every pasted line gets a `Read` of its
own, and `onPaste` (`startTyping`) pauses the output before pasted text
that stays on the line is returned (`announcePaste`; a pasted line ending
in its line break runs at once and needs no pause). A paste marker cut by
a read is held back (`holdsTail`: always inside a paste and when more
than its ESC was read, else only when the read filled its buffer); a
start marker cut right after its ESC by a short read is put together by
x/term alone, without these three, and in vi mode that ESC is the Esc key,
so the marker's rest and the paste are normal mode keys (accepted); pasted
text holding the end marker ends the paste early. A blank line that was
pasted as a whole (`term.ErrPasteIndicator`) is skipped by
`interactiveEditor.readPastedOrTyped`, so it does not toggle the output
pause. `TestEditInputBracketedPaste`,
`TestInteractiveEditorBracketedPasteOnPTY`. `Client.EditMode` (`--edit-mode`, `emacs` default
or `vi`; checked by the clients only, `initializer.applyEditMode` in
`transformClient`, so a typo in a shared config stops no dserver) adds a vi layer
(`viEditor`, `internal/clients/interactive_vi.go`, motions in
`interactive_vi_motion.go`): x/term has no vi mode, so every line starts in
insert mode (x/term edits), `editInput` turns a lone ESC (ending the input read or
followed by ESC) into `viEscapeKey` (x/term would keep it as a sequence
start; reads are the Terminal's size, and a read that filled its buffer
holds a trailing ESC or partial paste marker for the next, so a backlog
cut by a read is not misread), and in normal mode `editorKey` hands every key but Tab to
`viEditor.key`, which returns the changed line (motions with counts,
d/c/y operators, x X D C s S Y, i a I A, r ~ p P, u/Ctrl-R per line, k/j
history via `controlHistory`). In vi mode `editInput` also turns Up and
Down (`ESC [ A`/`B`, and Ctrl-P/Ctrl-N, which x/term reads as these
arrows) into `viUpKey`/`viDownKey`, which `viEditor.arrowKey`
handles in both modes, so the arrows and k/j move one history position
(`viEditor.histIndex`; x/term's own never moves) and Down or j past the
newest entry restores the line being typed
(`TestViHistoryOnePosition`, through a real `term.Terminal`). The other
keys x/term handles itself (Enter, Backspace, Left/Right, Ctrl keys) never
reach it. Mode changes set the cursor
shape (DECSCUSR, `viCursorShapes`; default shape back on close). SIGCONT re-applies cbreak (bash/zsh leave the TTY
cooked after `fg`), SIGWINCH resizes; termios is only set while dtail is the
terminal's foreground process group (a background `tcsetattr` would stop it
with SIGTTOU). Lines of 4096+ runes (x/term's silent cut) are rejected
(`errControlLineTooLong`) and resume a typing pause. The mode is restored on every loop
exit and, via `signal.SetBeforeForceExit`, on the 5 s forced exit, whose
hook (`restoreForForcedExit`) only restores the mode: it waits neither for
the normal close nor for the SIGCONT/SIGWINCH watcher, which a resize can
block behind a `Terminal.Write` stuck on stopped output (Ctrl-S). Non-terminal
control TTYs keep the plain `bufio.Scanner` path.
The controlling TTY accepts `:reload <flags>`, `:query [query]` (dtail and
dmap), `:tail [regex]` (dtail), `:show`, `:fields [table]`,
`:probe`, `:help`, and `:quit`. `:query <query>` and `:tail [regex]`
(`internal/clients/interactive_query_command.go`) take the rest of the line
as typed, only the blanks around it dropped, with no shell quoting (the
query language has its own double quotes); the query must parse and the
regex compile before anything runs (rejected while still paused). In dtail
`:query` switches the running session to the query or replaces it
(`switchToQuery`), `:tail` switches back to the regex followed before and
`:tail <regex>` to that regex (`switchToTail`; `:tail` alone while
following lines is rejected); in dmap `:query` replaces the query like
`:reload --query` (`applyQueryCommand`); dgrep and dcat reject both, and
Tab offers them only where they exist (`interactiveCommandsFor`). `:query`
alone shows the running query. Both resume paused output first
(`resumesBeforeRun`), answer `query applied` (query echoed) or `following
lines` (regex), or `query failed:`/`tail failed:` leaving the workload as it
was. Tab after `:query ` runs the `--query` completer on the rest of the
line and inserts candidates without shell escaping (`completeQueryCommand`).
`:show` prints `workload=` (`query` while a query runs, else the mode).
`:fields` prints the field catalog of the
background prober (`fieldsText` in `internal/clients/interactive_fields.go`:
per log format its tables, fields with sample values, `$variables` and probe
notes, the answering servers, the data's age and the probe errors; a table
argument, matched ignoring case, narrows it to that table and its lines'
`$variables`, after a table probe of it, `awaitTableProbe`; server text is
Go-quoted unless plainly printable); Tab after `:fields ` completes the
catalog's tables.
`:probe` resumes the output like `:reload` (an answer is read by the handlers
the pause blocks), requests a round and waits up to 5 s for a round started
after the request (`interactiveProber.probeNow`, `probeRounds`), then reports
which servers answered, answered busy or failed, or that answers arriving
later are stored.
An empty line pauses the terminal output (client logger and serverless direct
output, `interactivePause`/`outputGate` in
`internal/clients/interactive_pause.go`) and prints the help; the next empty
line resumes it. The pause blocks the writers (backpressure to the servers, no
buffering); a `:reload` (or `:probe`, `:query`, `:tail`) that parsed resumes first, as its
acknowledgement is read by the blocked handlers, and the control loop resumes
when it ends; a rejected line (flags, quoting, query parse) is answered with
the output still paused (`resumesBeforeRun` is asked after
`parseInteractiveCommand`, which never logs). The periodic
client STATS are logged by a goroutine of their own (`stats.logStats`), so the
stats loop keeps taking Ctrl-C from `statsCh` while the logger is paused.
`:reload` flags are tokenized POSIX-shell-like without expansion
(`internal/clients/interactive_args.go`): single quotes are literal, double
quotes keep backslashes except `\\`, `\"`, `\$` and ``\` ``, and `''`/`""` is
an empty value (`--regex ''` clears the regex, so every line matches unless
`--invert` is in effect, which carries over between reloads and then matches
no line; `--invert=false` resets it, `:show` prints `invert=`). `--grep`
follows the CLI: a non-empty `--grep` wins over `--regex`; `--grep ''`
(ignored by the CLI) is rejected with a hint to use `--regex ''`. Blank `--files`, blank `--query`
values and lines that are not valid UTF-8 are rejected. Full rules:
`doc/interactive-query.md`.

**Compatibility and session semantics:**
- The interactive bootstrap is always `SESSION START` (every protocol 5
  dserver supports it; a peer of another protocol version is rejected, see
  "Wire Framing"). A rejected or unacknowledged `SESSION START` fails the
  connection (exit status 1 for `dcat` and `dgrep`; `dtail` follow mode and
  `dmap` keep their statuses, see "Client Exit Statuses"); there is no
  fallback to the command stream
  (`dispatchInitialCommands` in
  `internal/clients/connectors/sessiontransport.go`).
- A `SESSION START`/`UPDATE` acknowledgement is waited for for 2 s of idle
  handler time (`defaultSessionAckTimeout`; a busy handler stretches it),
  counting only time the client handler was free to read frames
  (`baseHandler.WaitForSessionAck`, `busyClock` in
  `internal/clients/handlers/busyclock.go`): a payload write blocked by the
  paused output (or a host key prompt, a stalled pipe) can hold back an ack
  that arrived behind payload frames already on their way, so pausing right
  after the start no longer fails it. dserver sends every ack (ok or error)
  ahead of queued payload as soon as no frame is left half sent, and an ok
  ack ahead of all output of the workload it acknowledges (`sessionAckGate`,
  `internal/handlers/session_ack_gate.go`): before the dispatch the new
  generation is held, its payload stays queued and its server and MapReduce
  messages and a flush error of its read batch
  (`outputCoordinator.takeFlushError`) are deferred until the ack, which has a queue of its own
  (`sessionAcks`), went out; a workload that fails to start releases its
  hold on the input goroutine. The rest of a replaced generation is stale
  and dropped. So a slow link no longer makes an ack wait behind the
  session output buffer, also not the error ack of a rejected update while
  the running workload floods it; failure reports of the workload follow
  the ack. Non-`SESSION` command streams hold nothing, and payload still
  goes before other server messages. The initial `SESSION START`
  (connection goroutine) has no wall-clock cap; the session's end ends its wait, and so does the
  end of the connection's context (`:quit`, Ctrl-C;
  `baseHandler.WaitForSessionAckContext`, without which a quit during an
  unacknowledged start took 2 s of idle handler time). Everything the
  interactive control loop waits for is capped at 10 s wall clock
  (`sessionUpdateAckMaxWait` in `sessiontransport.go`): a `SESSION UPDATE`
  ack (`:reload`, `:query`, `:tail`, rollbacks) and the session lock a
  start in flight holds (`ReplaceStartSessionSpec`, which then returns
  `ErrSessionStartPending` and fails the reload), as the control loop alone
  answers a host key prompt whose output pause could otherwise deadlock it.
  Connections are updated and rolled back sequentially, so a failed reload
  with every server stalling takes up to about 20 s per connection
  (documented bound; no concurrent apply or total budget, which would break
  the all-or-nothing rollback). `applyLock.Unlock` of an unlocked lock
  panics like `sync.Mutex`. The rollback of an update that was sent but not
  acknowledged is numbered above it (base+2, `reloadRun.update`), and a
  wait skips ok acks of updates numbered below its own
  (`sessionRequest.awaitAck`), so a late ack of the timed-out update is not
  taken for the rollback's. Error acks carry no generation: a late
  rejection fails the request waiting then.
- A live `:reload` updates every active connection (session committed, its
  handler not done; `partitionReloadConnections` in
  `internal/clients/interactive_reload_inactive.go`) with `SESSION UPDATE`,
  all or nothing (a failure rolls the updated ones back). The only
  capability it checks up front, on the active connections, is `journal-v1`,
  for a reload to a `journal:` target (`checkReloadJournalCapability` in
  `internal/clients/interactive_control.go`): with one of them lacking it the
  reload is rejected on the TTY as a whole while the current workload keeps
  running. Connections without an active session (a server down, a session
  that ended) no longer fail the reload; `reloadRun.updateInactive` skips
  them and the TTY names them, a connection still connecting gets the
  reloaded spec for its SESSION START (`Connector.ReplaceStartSessionSpec`,
  under the connection's `applyMu`; a rollback gives the previous one back
  (`RestoreStartSessionSpec`; if that start already began with the
  reloaded spec and is still unacknowledged after 10 s, the rollback is
  deferred, `ErrSessionRollbackDeferred`: the start sends a `SESSION
  UPDATE` back to the previous spec right after its ack, under `applyMu`,
  and a failure of it fails the connection, `startInitialSession`; until
  that update finished `CommittedSession` reports the session as not
  committed, so a reload meanwhile takes the connecting path and waits for
  `applyMu` instead of recording the failed reload's spec for its own
  rollback; lines of the reloaded spec until the update's ack reach the
  client, as the client filters no output by generation, while its
  aggregates, which arrive outside the reload's switch and so are not
  held, are dropped up to that ack: the connection tells its handler
  before it defers the rollback, `Aggregate.AbandonStartWorkload`);
  once the start began, a committed one is updated like an active one, on
  the same base generation and all or nothing). Every connection is changed
  before the profile commit (for dmap, `CommitQuery`), so no connection
  sends results of the replaced query into the new generation. The other
  way round, a server starts the reloaded query right after its ack, so its
  first aggregates (a small file's whole result) can arrive before the
  commit: during a reload (`clientProfile.reloading`,
  `SessionState.Switching`) a connection's `Aggregate` merges what was
  queued before the ack frame into the current result and holds what
  follows (`Aggregate.SessionAcked`, called by `MaprHandler` in frame
  order) until the reload ended; a second ack within the reload (the
  rollback's) drops what was held. The reload's end
  (`Switching(false)`) merges what every connection holds into the result
  current by then itself, one holder after the other, each waiting for the
  group set's lock (`Aggregate.endHold`), and returns only then; a session
  that ended during the reload keeps its held messages through its
  shutdown `Flush`, and `MaprClient.finish` waits for a reload in flight
  (`awaitReload`), so the final result has them. After a failed reload,
  what a connection holds without a second ack (its session ended, or its
  rollback failed) is the abandoned query's and is dropped instead
  (`Aggregate.settleHold`, `SessionState.switchOutcome`). The hold is not
  capped: it is bounded by what a server sends during one reload, which
  the client keeps in memory anyway once merged (dropping beyond 4 MiB
  lost most rows of a large result). `Aggregate` logs nothing under its
  lock (`withLock`), for which the reload's end waits. A `SESSION START` ack during the reload holds too (a connecting
  server starts with the reloaded spec). Reloads number their updates on
  top of the highest generation committed or ever sent by a reload or
  rollback (`baseClient.reloadBase`), so unacknowledged numbers are not
  reused. Before, those
  aggregates went into the replaced query's result, or were dropped when
  `dtail` switched from following lines, and were missing in the new
  query's. With no
  active connection the reload is rejected (`no server has an active
  session`). A reload holds `baseClient.reloadMu` throughout and a `dtail`
  reconnect takes it to create and install its connection
  (`recreateConnection`), so a reconnect gets the spec as of before or after
  a reload, never one the reload left out. `:show` prints the number of
  servers (`servers=n`) and of active sessions (`sessions=n`).
- Successful reloads reuse the existing SSH session and advance a generation
  boundary so stale output from older workloads is dropped. dserver numbers
  the workloads of a connection itself (`sessionCommandState.generation`,
  never reused) and only acknowledges the client's generation number, so an
  update numbered like the running or an earlier workload still makes the
  replaced workload's queued output stale. A reload updates every connection
  on top of the highest committed generation of all of them
  (`applyReloadSpec`), so one that reconnected (and started over at 1) does
  not make later reloads fail with mismatched generations.
- A connection takes one session channel and a channel one shell request;
  further ones are refused (`internal/server/server.go`), so `MaxConnections`
  bounds the sessions.
- Interactive `dtail` (with or without `--query`) is built by
  `clients.NewInteractiveTailClient` (`cmd/dtail/main.go`
  `buildTailClient`): a `MaprClient` in tail mode whose query is optional
  (`clientProfile.switchWorkload`), with `MaprHandler`s, which print payload
  and aggregate the aggregate frames. Its running session switches between
  following lines and a query with `SESSION UPDATE`
  (`baseClient.switchToQuery`/`switchToTail` in
  `internal/clients/interactive_workload.go`, on top of
  `applyInteractiveReload`; driven by the `:query`/`:tail`
  commands). dserver and the serverless runtime need no change for it
  (`TestSessionUpdateSwitchesTailToQueryAndBack` in
  `internal/handlers/session_modeswap_test.go`). The query reads without the
  tail's regex, `--invert`, context and `--max` (`tailFilter`, set aside and
  restored on the way back, also for a `--query` given at start; an explicit
  regex for the way back clears `--invert`). `MaprClient.commitSessionSpec`
  takes a spec without query only for this client: `dropQuery` reports the
  result once more (`reportResults(true)`: with an outfile it is always
  written; without one `printResults` prints it only if the rendered table
  has rows and changed, and for `dtail` without outfile, non-cumulative, it
  covers only what arrived since the last report; dserver aborts the
  replaced aggregate, so up to one interval is missing), then `SessionState.ClearQuery` (no
  query, no global group set; `Aggregate.merge` drops aggregates silently
  while there is none). `MaprHandler` takes payload frames for aggregate
  data only while `SessionState.AggregatesExpected` (a committed query, or
  a pending switch to one, set by `clientProfile.expectQuery` before the
  update and reset on failure), so followed `--plain` lines starting with
  `AGGREGATE|` are printed; frames are otherwise handled as by a
  `ClientHandler`. The reporter idles without a query
  (`awaitQueryChange`). `finish` reports nothing while the session follows lines, and
  the incomplete-result warnings skip failures reported before the switch
  to the query (`queryFailureMarks`). Exit statuses are those of dtail's
  follow mode; non-interactive `dtail`, `dmap`, `dcat`, `dgrep` and the
  jobs are unchanged.
- **`outfile append`** (`MaprClient.writeCumulativeResult`): a cumulative
  query (`dmap`, `dtail --query` with an outfile, see `isCumulative`)
  appends no rows on interim reports, as each holds the whole result so
  far; they only open the outfile for appending (`checkAppendOutfile`,
  creating it empty) so an unwritable path is logged every interval. The
  result lives only in memory until its final result is appended, once (`finish`, `dropQuery`, or
  `appendReplacedResult` when a reload commits another query; a failed
  commit appends nothing). Continuous jobs (`NonCumulativeMode`) append each
  interval's swapped-out rows once; scheduled jobs have no interim reports.
  Semantics per mode: `doc/querylanguage.md`.
- **Interim report timing** (`MaprClient.reportGeneration`): a query
  generation's first report is due half an interval after it started, but
  each server's first aggregates merged for it
  (`SessionSnapshot.AggregatesArrived`, signaled by `Aggregate.merge` once
  per server and generation) move it: once every live server
  (`MaprClient.allLiveArrived`: the `Server()` of each connection whose
  handler is not done, matched by name with
  `SessionSnapshot.ArrivedFrom`, so one that sent and left does not count
  for a missing one; liveness sampled at each arrival and, while servers
  are missing, every `firstReportSettle`, `firstReportWindow.postpone`, so
  a missing server whose connection ends no longer holds the table until
  the cap) sent its first
  aggregates, to
  `firstReportSettle` (1 s) after the last of them; while some are
  missing it waits for them, at most half an interval after the first
  server's (servers trailing by seconds, e.g. slower hosts or sequential
  session updates, were seen live; a server with no rows to send only
  costs that wait), and every wait capped at half the interval (`firstReportWindow`,
  `maprclient_report.go`; at `interval 1` a 1 s wait would land on the
  next sends and mix two intervals in one table). A due report that finds
  an arrival signaled but not yet taken (timer and arrival ready together)
  takes it as an arrival and is put off by the settle time (`postpone`);
  reporting at once put the later reports on the servers' send instants.
  The later reports are planned once per interval from the instant the
  previous one was planned for (`reportSchedule`, `nextReportDue`), not
  from its end, so the time reports take does not add up; that normally
  gives each table one send of every server, which is no guarantee, as the
  servers send by their own clocks. dservers send their first aggregates one interval after the
  start, so the first table used to appear only after 1.5 intervals. Continuous jobs (`NonCumulativeMode`) keep the fixed
  cadence; scheduled jobs have no interim reports.
- **Logging under the MapReduce state's locks** (`deferredLogger`,
  `internal/clients/maprclient_log.go`): the client's `SessionState`, its
  group sets and what the reporter logs inside
  `SessionState.WithCurrentSnapshot` (`MaprClient.lockedLogger`) log through
  a logger that queues (at most 256 diagnostics, the rest dropped and
  counted in one WARN) and never blocks; a goroutine that runs only while
  something is queued passes them on in order, and `MaprClient.finish`
  flushes it. The client logger blocks while the output is paused, and the
  one pause that can coincide with a `:reload` is the host key question of a
  server still in its handshake, which only the control loop answers: the
  reporter then waited in the paused logger (`Writing to outfile`) with the
  session state's read lock and the global group set's lock held, the
  reload's commit and end (`CommitQuery`, `Switching(false)`) waited for
  those locks, bounded by none of the 10 s caps, and the question was never
  answered (`TestMaprReportUnderPausedLoggerDoesNotBlockReload`).
  The control loop itself must not wait for the paused logger either, lock
  or not. Covered: what `commitSessionSpec` prints and logs on it goes
  through the same logger (`MaprClient.controlLogger`), i.e. the last table
  of a query that `:tail` or a reload ends (`dropQuery` ->
  `reportResultsTo`: rendered at once, its `Raw` payload queued and never
  dropped, printed in order once the logger runs again, so it can come
  after followed lines that were printed directly meanwhile), the errors
  of writing it, and of appending a replaced query's result
  (`appendReplacedResult`); and the aggregation failures of what a
  connection held through the reload, which `Aggregate.endHold` logs to
  the session state's logger instead of the connection's
  (`TestMaprTailSwitchUnderPausedLoggerDoesNotBlockControlLoop`,
  `TestEndHoldLogsFailuresToTheSessionLogger`). Not deferred: the periodic
  reporter and the final result of `finish` (synchronous, on goroutines
  that hold no lock while printing; `finish` flushes the queue first),
  `client.Aggregate`'s failures on the frame-reading goroutines (logged
  after releasing its lock, `withLock`), and trace diagnostics. The reload
  code outside the MapReduce client (`internal/clients/interactive_*.go`)
  answers on the control TTY, not through the client logger.

### Auth-Key Fast Reconnect
Auth-key fast reconnect is enabled by default. The client can register a public key with `dserver` over an already-authenticated session, and subsequent connections can use this in-memory key before falling back to normal SSH auth.

**Technical Details:**
- Client sends `AUTHKEY <base64-pubkey>` command during session setup
- Server stores keys in memory, per user, with TTL and max-keys limits
- SSH `PublicKeyCallback` checks in-memory auth-key store before `authorized_keys`
- If fast-path auth misses (restart/expiry/mismatch), normal SSH auth is used automatically
- `dtailhealth` never registers a key (`clients.NewHealthClient` sets
  `NoAuthKey`; its `--no-auth-key` flag is accepted without effect), and the
  server's health handler ignores `AUTHKEY` (`HealthHandler.ignoreAuthKey`)
  instead of ending the session as an unknown command, which reported a
  healthy server CRITICAL whenever the auth key's `.pub` existed. Either side
  alone fixes it, so mixed old/new protocol 5 versions work

**Config and Flags:**
- Client flag: `--auth-key-path` (default `~/.ssh/id_rsa`)
- Client flag: `--no-auth-key` (disable feature)
- Client env: `DTAIL_AUTH_KEY_PATH` (primary env alias for auth key path; takes precedence over `DTAIL_SSH_PRIVATE_KEYFILE_PATH`)
- Client env: `DTAIL_SSH_PRIVATE_KEYFILE_PATH` (legacy alias; used only when `DTAIL_AUTH_KEY_PATH` is not set)
- Env var precedence (highest to lowest): CLI flag → `DTAIL_AUTH_KEY_PATH` → `DTAIL_SSH_PRIVATE_KEYFILE_PATH`
- Server config: `AuthKeyEnabled` (default `true`)
- Server config: `AuthKeyTTLSeconds` (default `86400`)
- Server config: `AuthKeyMaxPerUser` (default `5`)

### Host Key Verification
The client checks server host keys against `known_hosts`
(`internal/ssh/client/knownhostscallback.go`). An unknown host is asked about
on the terminal, or trusted and added without asking with `--trustAllHosts`.
A host whose key differs from the one in `known_hosts` is not unknown: it is
refused with a `HOST KEY CHANGED` error in every mode, unless the client was
started with `--trustChangedHostKeys` (`Args.TrustChangedHostKeys`, all
clients and `dtailhealth`, which otherwise trusts all hosts). With that flag
the prompt names the hosts whose key changed, and trust-all mode replaces
their entries with a WARN. A key marked `@revoked` is never trusted. The
parsed file is cached until it changes (file identity, size, modification
time). Scheduled and continuous jobs authenticate with their own auth methods
and do not use `known_hosts`.

**Connect and handshake timeouts:** `Common.SSHConnectTimeoutMs` (default
2000) bounds only the TCP connect (`net.Dialer.Timeout` in
`ServerConnection.dial`). `Common.SSHHandshakeTimeoutMs` (default 30000)
bounds the SSH handshake after it (`ssh.NewClientConn`, which has no timeout
of its own) with a deadline on the TCP connection (`handshakeDeadline`,
`internal/clients/connectors/handshake_deadline.go`), so a host that accepts
TCP but never completes the handshake fails the connection (`no handshake
within 30s`) instead of leaving it connecting forever. The host key callback
pauses the deadline and resumes it with the time left, so the unknown host
question may wait for the user's answer as long as it takes; the deadline is
cleared once the handshake ended, and re-keys later leave it alone. Every
connection of every client, `dtailhealth` and the scheduler's jobs goes
through `ServerConnection`, so it applies to all of them. Agent signing is
not paused: a touch- or confirm-per-signature agent key (hardware key,
`ssh-add -c`) shared by many connections can run later ones into the
deadline.

### File Paths With Spaces
dserver splits every command on single spaces and a read command is
`<mode>:<options> <path> <regex>`, so a path is one word. `session.Spec.Commands`
(the one builder of read commands, for the plain command stream of
non-interactive clients and for the commands dserver builds from
`SESSION START`/`UPDATE` Files, including the `timeout N <mode> <path>
<regex>` form) passes every path through
`protocol.EncodeReadPath` (`internal/protocol/readpath.go`): a path containing a
space, or starting with the prefix `base64%`, is sent as `base64%` + standard
base64 of the path (the prefix config option values already use); every other
path is sent verbatim. `readCommand.parsePath`
decodes the word before anything else sees it, so globbing, the `readfiles`
permission check (which must match the decoded path), `journal:` targets and
shared reads all use the real path; a prefixed word that does not decode fails
the command with `read: unable to parse command` and reads nothing.

**Compatibility:** every protocol 5 dserver (and the serverless runtime)
decodes encoded paths, so there is no capability for it and clients send such
paths to every server (the former `encoded-read-path-v1` capability is gone).
Via `--interactive-query` (`SESSION START`/`UPDATE`, Files as JSON) dserver
builds and encodes the commands itself.

**Commas:** `--files`, positional arguments (joined with commas) and job `Files`
are split at commas with blanks around each entry trimmed (`splitFiles` in
`internal/session/spec.go`), so paths containing a comma, or with leading or
trailing blanks, cannot be read. The scheduler's job grouping parses the same
comma lists, so this is not changed.

**Newlines:** the read permission check denies every path holding a newline
(`errNewlineInPath` in `sessionuser.User.iteratePaths`, the resolved path, so
also through a symlink; `PathRules.MayReadBeneath` offers no such directory):
`.` matches no newline and `$` only the end, so `!^/dir/.*$` did not match
`/dir/a\nb` while an allow `^/dir/` did, a bypass of the documented deny
form (`TestResolveReadTargetDeniesNewlinePaths`). The read reports `read: no
permission to read file`. dserver's own jobs are not checked and read them.

### Journal Source Reads
Journal targets use `journal:unit.service` syntax. Permission rules should match the full target, for example `Server.Permissions.Users[user]: ["readfiles:^journal:.*\\.service$"]`.

**Technical Details:**
- Server capability `journal-v1` is required for any journal-backed read target; clients reject journal sources when the server does not advertise it.
- The capability is advertised only on Linux when `journalctl` is available on `PATH`.
- Journal reads use `journalctl` via `os/exec` only; there is no cgo or `libsystemd` path, and the feature is Linux-only at runtime.
- Non-follow reads the current journal snapshot once. Follow mode adds `-f -n 0` and keeps restarting `journalctl` until canceled.
- Entries are cut at `Server.MaxLineLength` exactly like file lines (shared `fs.SplitLine`/`fs.ScannerTokenLimit`, same long-line warning, passed via `journal.ReaderOptions.MaxLineLength`), so an entry over 1 MiB no longer fails with `bufio.Scanner: token too long`. An unterminated last entry of a `journalctl` run is still dropped (pieces cut off before it are kept).
- The unit must be one literal systemd unit name (`validateJournalSpec`,
  `internal/io/fs/journalunit.go`, used by reads, probes and the unit
  listing through `fs.NewValidatedJournalTarget`), as the `readfiles` check
  matches the target string while `journalctl -u` would widen other names:
  ASCII letters, digits and `:-_.\@` only (no glob `*?[`, which `journalctl
  -u` expands over the journal's units, so `journal:ssh*.service` read
  `sshd.service` past a deny rule), at most 255 bytes, no leading `-`, a
  non-empty name before `@` and the type, a non-empty instance (templates
  such as `getty@.service` are refused), and an explicit unit type suffix
  (`.service`, `.socket`, `.timer`, `.scope`, `.mount`, `.automount`,
  `.swap`, `.target`, `.path`, `.device`): `journal:sshd` is refused, as
  `journalctl` appends `.service` to it, and so is `.slice`, as a slice
  reads all units in it. A refused target fails like an unreadable file
  (`read: unable to read file`) after the permission check, and journalctl
  never runs.

### Probe Command (field discovery, path listing)
`probe <base64 JSON {id, files}>` asks dserver (and the serverless runtime)
which MapReduce tables, fields and `$variables` the lines of some files carry,
for the interactive query completion; it is answered once with the hidden
control frame `.syn probe <id> <base64 JSON>` (`protocol.ProbeAnswer`:
`defaultLogFormat`, `formats` of every registered log format that found
fields, `filesSampled`, `linesSampled`, `truncated`, `errors`). Wire types
and limits: `internal/protocol/probe.go`; command:
`internal/handlers/probecommand.go`, `probe_targets.go`, `probe_answer.go`;
full description: `docs/dev/probe.md`.
- Entries are read command path words and get exactly a read's checks:
  `protocol.DecodeReadPath`, `journal:` targets only with `journal-v1`, one
  `filepath.Glob` capped at `MaxGlobTargets`, the `readfiles` permission check
  of every match (`ServerHandler.PrepareReadTarget`); never stdin (`-`).
- At most 8 distinct files per request (`protocol.ProbeMaxFiles`; a file
  named twice, e.g. by a path and a glob, is sampled once), each a bounded
  sample (`probe.Sample`: last 256 KiB / 500 lines, compressed files' first
  500 lines, `journalctl -n 500`, the tail sample's first line put in front
  for csv headers but not counted in `linesSampled`), no cat/tail limiter
  slot. Every step is bounded by size; a 10 s timeout (`probeTimeout`) also
  stops the sampling (and `journalctl`), not the glob, permission checks or
  log format probes. The answer JSON is
  capped at 48 KiB: sample values go first, then fields (the default log
  format's last), with `truncated` set.
- Per table: a format's `tableVariables` maps every table to the
  `$variables` its lines filled, also for one table, an empty entry for a
  table none of whose sampled lines filled one, never known variables
  without values; only a custom probe without per-table data leaves it
  out, and only then does `logformat.ProbeResult.VariablesOf` fall back to
  the format-wide `variables`. The sample window is streamed and every line
  of it, also those beyond the 500 kept, is passed to the table scanners
  (`fs.SampleOptions.Observe`, `logformat.ProbeTableScanner`,
  `ProbeTabler`), so the answer lists every table of the window (without
  fields, with an empty `tableVariables` entry, where none of its lines was
  kept). A panicking `ProbeTabler` is recovered and reported as `probe: log
  format probe failed` for its format. Only tables a query can name are
  listed: `from stats` reads `STATS` (`mapr.TableName`) and lines are
  matched by the exact name, so the default format's probe and
  `ProbeLineTable` put a line whose table name holds lower-case letters
  (`mapr.SelectableTable`; `MAPREDUCE:stats`) under the table-less entry
  `""` (`ProbeAnyTable`): a query without a table (no `from`, `from .`)
  reads it, so Tab offers its fields only while no table is typed and
  `:fields` shows them as `fields:`; the probe's `note` names the first
  such table of the kept lines and how to query them.
- The `csv` format answers only for sources that look like csv files
  (`csvProbeSources`, `internal/mapr/logformat/csv_probe.go`: a first line of
  at least two non-empty columns of at most 64 bytes, only those listed, at
  least half of the sampled rows of 2 up to the header's columns, and no
  more than half of them, once 3 rows were sampled (`csvProbeMinEchoRows`),
  echoing the header, `csvProbeSource.echoesHeader`:
  a value equal, but for the digits of its numbers, `csvSameShape`, to the
  name of its column, for names with a letter), so other logs get no bogus
  csv field named after their first line, also not a free-text log of lines
  like `request 1 done, ok`. Heuristic: single-column csv files and
  headerless csv files whose rows mostly repeat a value of the first are
  never probed; a free-text log whose lines hold a comma still is when most
  lines differ from the first by more than numbers on both sides of it.
  All 21 multi-column csv fixtures of the integration tests stay probed
  (`TestCSVProbeKeepsFixtureCorpus`).
- Table-targeted probe (`{id, files, table}`, `internal/handlers/probe_table.go`):
  same checks, file cap, timeout, one-probe-per-session and no limiter slot
  as above, but it scans 4 MiB of each file (`probeTableScanBytes`;
  `ScanBytes` of a head sample, 5,000 `journalctl` entries), streamed, and
  keeps (500 lines / 256 KiB) only the lines of the table
  (`ProbeTableScanner.Matcher`: the table a format's `ProbeTabler` assigns,
  else, for lines no `ProbeTabler` claims, the name as a word, or as the
  first 64 bytes, `ProbeMaxNameBytes`, of a longer word, the name such a
  table is listed under); the answer
  (`table` echoed) holds that table only, with its fields and
  `tableVariables`, no format-wide `variables`. An invalid table
  (`protocol.ValidProbeTable`: no blanks, control/format characters, `|`,
  `,`, quotes or backticks) or a table with `list` is `probe: invalid
  request`.
- Failures are `errors` entries with fixed reasons, never `.syn command
  failed`, so a probe changes no exit status. One probe at a time per session
  (another is answered `probe: another probe is running`), and at most one
  answer waiting to be queued (`probeInFlight`, `startProbe`): the next
  probe waits for it on the command input at most 500 ms
  (`probeInFlightWait`) and is answered busy then, so neither a client that
  stops reading nor a payload flood (payload is read before server
  messages, so answers can stay unread while the client reads) holds the
  command input or makes dserver keep a goroutine and answer per probe.
  Busy/invalid answers never wait: dropped when the server messages are
  full (`trySendProbeAnswer`). A probe carries no
  session generation, so it runs next to a `SESSION` workload without
  replacing it, and it never ends the session; it only starts an idle
  shutdown that other work asked for while it ran
  (`commandShutdownIfWanted`).
- Logging: a probe's permission checks (`ServerHandler.probeReadTarget`,
  `sessionuser.User.ResolveReadTargetQuietly`, same result as
  `PrepareReadTarget`) and its per-file `Probe: unable to read file` lines
  log at DEBUG only; each probe or listing logs one INFO line, `Probe
  answered` (entries, errors), and a field probe one WARN `Probe: unable to
  read entry <entry> <reason> matches <n>` per request entry and reason
  that failed (`probeRun.entryFailed`; at most 8 entries, entry cut to
  256 bytes), so denied or missing targets stay visible at the default log
  level without one line per glob match. A read command's check keeps its
  INFO lines.
  A listing's directory that can not be opened or read (`Probe: unable to
  list directory`) logs at DEBUG too, and a rule that does not compile is
  logged at WARN by a user's first listing only (`User.PathRules`), as any
  user starts listings with Tab.
  A user's permission regexes are compiled once per `User`
  (`compiledPermissions`), not per check or listing. Measured listing of a 500-file
  directory: 1000 INFO lines and 13 ms / 10.8 MB (1 rule), 83-171 ms /
  60 MB (5 rules) before; 0 per-file lines and about 6 ms / 1.7 MB after
  (`BenchmarkProbeList500`).
- Path listing (`internal/handlers/probe_list.go`): a request `{id, list:
  "<prefix>"}` (no `files`) is answered `{id, dir, entries: [{name, dir}],
  truncated}` for the directory part of the prefix (names starting with the
  rest, sorted, at most 500, answer within 48 KiB). Files only if
  the read permission check allows them. Directories unless no allow rule can
  still match a path beneath their resolved path or one deny rule already
  matches `<dir>/` itself: within it, or only at its end through an
  assertion about what follows (`$`, `\b`) and then also the sample paths
  `<dir>/x`, `<dir>/x/x`, `<dir>/.x`, `<dir>/y/y/y/y.y`, `<dir>/-` and
  `<dir>/-/-` (`denySamples`), so `!^/var/log/secret/.*$` hides `secret` while
  `!^/var/log/[^/]*$`, `!^/var/log/(old/.*)?$` and a rule of one and two
  levels only (`!^/home/[^/]*/?[^/]*$`) keep their directory, whose
  subdirectories hold readable files, offered (a rule that matches `<dir>/`
  itself and everything beneath, `!^<dir>/` or `!^<dir>/.*$`, matches every
  sample and always hides; `!^<dir>/.+$` does not match `<dir>/` and hides
  nothing; `sessionuser.PathRules.MayReadBeneath`, rules compiled once per
  `User`, an NFA run of each rule over the directory path; at its end the
  assertions are tried per kind of next rune, word, non-word, end of text,
  `endOfPrefix`, so contradictory ones such as `\b\B` keep no allow rule
  alive; an unanchored allow rule that may start a match after the prefix,
  e.g. at the end of a longer path, `$`, `\b$`, `(?m)$`, counts as alive
  everywhere, `matchesLater`). This is an
  approximation that reveals some directory names: a directory denied only
  by what a deny rule requires after its `/` (`!^/var/log/private/.*\.log$`,
  `!^/data/x/[^/]`) or by several deny rules together is offered (only names
  an allow rule covers; a glob read's no-permission/no-file answers reveal
  as much). The listed directory must pass the same rule, and a
  missing, unreadable or forbidden one is the same empty listing. Checks use
  resolved absolute paths (symlinks, relative prefixes; a `..` is cleaned
  lexically first, as reads do, so it is no existence oracle of the component before
  it); hidden names only for a `.` prefix; bounded scan (100,000 names,
  5,000 checks, at most 10,000 names held, the probe timeout; a bound
  sets `truncated` also without entries, so the client fetches a narrower
  prefix, an accepted oracle that many names not offered share a prefix,
  as is the listing's timing; a timeout is `probe: timed out`). `journal:` prefixes list `journalctl --field
  _SYSTEMD_UNIT` units the user may read as `journal:<unit>`, only with
  `journal-v1` and only when an allow rule can match a target with the
  prefix (else no `journalctl` run). The unit list is shared by all
  sessions of a dserver (`journal.UnitCache`, `internal/io/journal/unitcache.go`,
  `handlers.NewJournalUnitCache`, `Dependencies.JournalUnits`): a
  successful list is reused for 60 s (`journal.DefaultUnitCacheTTL`;
  failures are not cached), concurrent listings share one `journalctl` run,
  which belongs to none of them, so the probe timeout does not bound it:
  it goes on when every listing waiting for it gave up (until dserver
  shuts down, `UnitCache.Close` in `Server.stopSharedWork`) and its result
  is cached (a journal slower to list than a probe's 10 s was never cached
  before), and it stops at the latest after 30 s (`unitFlightTimeout`),
  which alone bounds a hung `journalctl`; a panic of the lister is recovered into
  the run's error (logged with its stack at WARN). The permission
  filtering stays per user and request. Serverless mode does not cache. `Server.PathCompletionDisable` answers `probe: path listing
  disabled`.
- Client (`docs/dev/probe.md`, "Client"): only interactive sessions probe,
  in every mode. `baseHandler.handleHiddenMessage` queues answers for
  `handlers.Prober.Probe` (`internal/clients/handlers/probe.go`; ctx-aware,
  stale ids skipped, never blocks the frame reader; one call per handler at
  a time: a concurrent call waits for the slot while its ctx allows, so an
  on-demand path listing from Tab shares it with the background field probe
  instead of failing or preempting it, decision N1: preempting would free
  only the client's slot while dserver went on sampling and answered
  `probe: another probe is running`). The
  background `interactiveProber` (`internal/clients/interactive_probe.go`,
  started by `startInteractiveControl`, stops with the session) asks at most
  3 connections (re-read each round, so reconnects are fine; `probeTargets`
  takes those whose session is up first, those still connecting only fill
  the slots left, and never one whose session ended; a connection whose
  `Start` ended without a session, e.g. a server down, has its handler
  shut down by `ServerConnection.Start`, so it neither holds a round or
  `:probe` for the 30 s round timeout nor takes the path listings): at start,
  after every applied `:reload` (`request()`), every 60 s while the output is
  not paused; debug logging only; after each requested round (not the
  periodic ones), once it counted as finished, the path listings of the
  files' directories not cached fresh (after `:probe`: all of them) are
  requested in the background. Answers go into the mutex-guarded
  `fieldCatalog` (`interactive_catalog.go`: latest answer per server with
  its probed files, merged on `snapshot(files)` over the answers about the
  session's current files, so answers about files of before a `:reload`
  no longer count), which Tab completion and `:fields` read. Constraint, settled by
  `TestProbeAnswerWhileOutputPaused`: with a remote dserver no answer
  arrives while the output is paused (the paused writer blocks the handler
  reading the frames, the answer is queued behind payload); in serverless
  mode a field probe sent before the pause does (payload bypasses the
  handler), but a request sent while paused is not handled until the output
  runs, as the in-process server logs to the paused output. The query
  completion waits for a server only for a table probe (below); the path
  completion waits at most 1 s with the output running.
- Table probes (`interactiveProber.requestTable`,
  `interactive_probe_table.go`): when Tab completes in a `--query` value
  with a typed `from` table (`completeWithTableProbe`; only the Tab that
  sends it waits, like for a path listing, 1 s with the output running; a
  Tab while it is on its way completes from the cache with a hint) and on
  `:fields TABLE` (`awaitTableProbe`, 2 s), a table-targeted probe of the
  current files is sent to the round's targets, unless one is on its way
  (checked first, under the same lock), the catalog holds a table answer of
  that table and files younger than 60 s (`hasTableProbe`, also one that
  found nothing) or one no server answered (failed, timed out, busy) within
  5 s (`markTableProbeFailed`, `tableProbeRetryAfter`). Answers are kept per
  server and table (`storeTable`, at most 64) and merged into the snapshot
  of the same files (`catalogSnapshot.TableProbes`);
  `catalogFormat.variablesOf(table)` gives only the `$variables` a table's
  lines filled (format-wide only for answers without per-table data,
  `answerTableVariables`), which is what the query completion offers
  once a `from` table is typed.
- End to end (`integrationtests/interactive_guided_test.go`, dserver and
  serverless): `TestInteractiveGuidedQuerySession` types the target
  session of the guided completion with Tab on a PTY (`--noColor`, no SGR
  in the output) in a `dtail` following a log of two tables, checks that
  the listings hold only the chosen table's fields and `$variables`, runs
  the query (result table, no followed line meanwhile), `:tail` back and
  `:query` again on one session; `TestInteractiveQueryTableScope` checks
  the tables Tab lists after `from`, where values per table, and a table
  that only appears before the probe's window, found by the table probe
  of the Tab after `from RARE` and listed by `:fields RARE`.

### Wire Framing (server-to-client stream)
Every server-to-client message (frame) ends with `protocol.MessageDelimiter`,
the single byte 0xAC. That byte is also a UTF-8 continuation byte (`€` is
E2 82 AC, `Ь` D0 AC, `¬` C2 AC, about 1/64 of all CJK characters), so raw
framing cut such lines in two and dropped the byte. Since protocol 5
(`protocol.ProtocolCompat` = `5`) every session uses
`protocol.EscapedControlFraming` for every frame, from its first frame (the
capabilities frame, held as the framer's first pending output,
`sessionFramer.queueFirstFrame`, so payload queued meanwhile can not overtake
it) on, in server and serverless mode and for health checks;
nothing is negotiated (`internal/protocol/framing.go`, `control_frames.go`;
design in `docs/dev/wire_framing.md`):
- Every frame body has 0xAC replaced by `FF 01` and 0xFF by `FF 02`, in time
  linear in the body however many bytes are escaped and with the escaped body
  sized exactly (counted first), so a dense 1 MiB body costs one allocation of
  its escaped size. Valid UTF-8 never contains 0xFF, so frames without 0xAC
  are written byte for byte.
- Every control frame (hidden `.syn ...` messages, the capabilities frame
  included, and `AUTHKEY` replies) starts with the marker `FF 03`, which
  escaping makes unforgeable; every unmarked frame is payload. So `--plain`
  log lines starting with `.` or reading `AUTHKEY OK` are printed and never
  acted upon (`.syn close connection`, `.syn command failed x`).
- The server (`sessionFraming` in `internal/handlers/session_framer.go`)
  never waits for a client request before it sends a frame; the client
  handlers (`internal/clients/handlers`) send no framing request. The former
  `.ack framing escaped-v1` / `control-v1` requests are unknown `.ack`
  commands now and ignored.
- `NewClientHandler` and `NewMaprHandler` check the session's first frame
  (`baseHandler.checkFirstFrame`): unless it is a marked capabilities frame
  (which may list no capability), they report `unsupported DTail server: not
  a protocol 5 server (no capabilities frame first); please update DTail
  server` and set status
  1, even after a normal close handshake (protocol-4.1 servers can finish a
  session on unknown `.ack` commands before reading files, which must never
  look like a successful empty query). The health handler does not check (the
  server's health handler sends no capabilities frame).
- The capabilities frame lists only runtime properties of a server, today
  just `journal-v1` (Linux with `journalctl`), so it may be empty
  (`handlers.ServerCapabilities`). What every protocol 5 peer supports is not
  advertised: the former `query-update-v1`, `command-failure-v1`,
  `escaped-framing-v1`, `encoded-read-path-v1` and `control-frames-v1` are
  gone, and so are the client checks of them.
- The server rejects command frames of another protocol version
  (`commandDispatcher.rejectProtocolVersion`, see "Client Exit Statuses").
  `protocol.TerminalFraming` (verbatim, unmarked) only lends the frame layout
  to serverless terminal output and is never sent to a client.
- Escaping can grow a frame to almost twice its raw size, so the output buffer
  cap of every client session (`ServerHandler`) is at least
  `2*MaxLineLength + 128 KiB` (see the output limits below); health sessions
  send only a few short frames and keep the default. Output that still does not fit is reported, never dropped
  silently: a visible warning plus the failed-command reason `output: too
  large for the session output buffer`.

### Diagnostic Escaping (dserver and client logs)
Every diagnostic detail (the args of `Info`/`Warn`/`Error`/..., strings,
errors and `%v` values alike) is written through
`protocol.writeEscapedDetail` (`internal/protocol/diagnostics_escape.go`,
called by `writeDiagnosticDetails`): C0 controls (tab included), DEL, C1
controls, Cf format characters (e.g. U+202E) and U+2028/U+2029 become Go
escapes (`\n`, `\r`, `\t`, `\xNN`, `\uNNNN`), so a path, glob or probe entry
with a newline can not forge log lines and an ESC can not drive the terminal.
Printable text, non-ASCII text, invalid UTF-8 bytes (e.g. Latin-1 names)
and backslashes are kept byte for byte (normal paths are unchanged; escaping
is idempotent), except a lone invalid byte 0x80-0x9f, which an 8-bit
terminal takes for a C1 control and which becomes `\xNN`. Header
fields (source, hostname, level, timestamp) are not escaped. Stack traces
logged as a detail are one line with `\n\t` escapes. Payload (`Raw`) is never
escaped, and nothing the client parses changes: control frames (`.syn ...`,
`AUTHKEY`) are built without the diagnostic writer, and the server warnings a
client receives are payload frames it only prints.

### Client Log Contents: Diagnostics vs Payload
The default client logger is `fout` (stdout + a daily file at
`<LogDir>/YYYYMMDD.log`, `LogDir` defaults to `~/log`). The logger and log
directory resolve as: explicit `--logger`/`--logDir` flag > `Common.Logger`/
`Common.LogDir` from the config file > per-command default (clients `fout` and
`~/log`, dserver `file` and `log`). The flags default to empty so an unset flag
never masks the config file. dtailhealth is the exception: it has no `--cfg`
flag and reads the user's client config, so it ignores `Common.Logger` and
`Common.LogDir` and uses `none` and `log` unless its own `--logger` flag is
given (it has no `--logDir` flag). By default the
`fout` daily file records DIAGNOSTICS ONLY — the small connection/audit lines (INFO/WARN/ERROR).
The full retrieved PAYLOAD (the bulk `dcat`/`dgrep`/`dtail` output) is NOT written
to the file by default, so a large read no longer silently grows the daily log by
the full payload size. Payload always still goes to STDOUT/terminal, unchanged.

**Restoring the legacy full-payload tee (opt-in):**
- Client flag: `--log-payload` (all client commands: dcat, dgrep, dtail, dmap)
- Client config: `Client.LogPayload` (default `false`)
- When enabled, retrieved payload is teed into the daily log file as before.

**Seam and scope:**
- The split happens in the `fout` logger: diagnostics arrive via `Log`/`LogWithColors`
  (always written to both stdout and file); payload arrives via `Raw`/`RawWithColors`
  (always to stdout, to the file only when `LogPayload` is set).
- Only the default `fout` logger is affected. `--logger stdout` (no file sink) and
  `--logger none` are unaffected. `--logger file` is a pure file sink with no stdout
  tee; it deliberately still writes payload to the file, because otherwise its output
  file would be empty — payload teeing there is not gated by `--log-payload`.
- Note: the serverless direct-output path can bypass the logger and write payload
  directly to stdout; that path never wrote payload to the file and is unaffected.
  The disk-fill footgun lives on the `fout` file path (the server-mode receive
  path), which is what this setting gates. As that path also bypasses the
  logger's color switch, the client runtime passes the color decision itself
  (`clientRuntimeBoundary.serverlessColorizer` in
  `internal/clients/runtime_boundary.go`): with `--noColor` (or
  `Client.TermColorsEnable` false) serverless lines are uncolored
  `REMOTE|...` lines, byte-identical to server mode with `--noColor`; with
  `--plain` they are the raw lines, as in server mode with `--plain`.
- Serverless mode is a property of the runtime that built a session handler, not
  of the session: it holds exactly when the in-process client runtime hands over
  an output of its own (`handlers.Dependencies.ServerlessOutput`, the single
  source; there is no separate flag, and no stdout default). dserver never hands
  one over and ignores the `serverless=true` option a client sends, so a remote
  session can neither route its payload into the dserver process output instead
  of its SSH channel, nor read dserver's own stdin with the target `-`, which
  takes `readPipe` and so is checked by no `readfiles` permission.
- Daily file rotation uses a day name cached by the file sink and refreshed on its
  100 ms flush tick while active. Idle loggers stop their flush timers; the first
  file write after idle refreshes the day, without reading the clock per bulk
  payload line. Stdout arms its flush timer when buffered output first arrives;
  both sinks retain their original 100 ms tick schedule when rearming after idle.
  The file is chosen at
  write time: around midnight a line (including a timestamped dserver diagnostic)
  can land in the neighbouring day's file. This is intended.
  File sinks copy payload and diagnostics into owned 64 KiB batches rather
  than allocate/enqueue a message per line. Four queued batches, one producer
  batch and one worker batch bound payload backing memory to 384 KiB (plus the
  64 KiB output buffer). Buffers are reused locally; an unused sink allocates
  no batch storage. Partial batches flush on the existing 100 ms phase or on
  explicit Flush/shutdown. Calls are serialized in file-sink admission order;
  even a call larger than the queue is contiguous and keeps one destination
  through rotation. Shutdown drains accepted calls, including blocked large
  calls, and rejects later admissions with a diagnostic. A blocked sink still
  applies backpressure. The default payload-disabled fout path is unchanged.

### Client Exit Statuses (dcat, dgrep)
`dcat` and `dgrep` exit `0` on success (`dgrep` also when no line matched, unlike
`grep`), `1` when a server could not be connected to (refused or unreachable),
the SSH handshake failed or no SSH session could be opened, the server rejected
or did not acknowledge an `--interactive-query` `SESSION START` or the commands
could not be sent, a server lacks `journal-v1` for a requested journal target,
the server is of another protocol version (see "Wire Framing"), a serverless
session failed, a session was cut short (below), or the
client could not start, and `2`
(`clients.ExitStatusFailedCommand`, also the status of an invalid flag) when any
server reported a failed command of the session with `.syn command failed
<reason>` (every protocol 5 dserver, `handlers.SessionOutcome.Failures`).
This covers the file read failures (`protocol.IsFileReadFailure`: missing, not
permitted, unreadable, reader not created, glob capped) and deliberately also the
other reasons (`command: ...`, `map: invalid query`, `read: unable to parse
command`, `output: too large for the session output buffer`), as each means
output that was asked for is missing. Serverless mode behaves the same, as its
in-process server reports failures the same way. The client exits with the highest
status of its connections (`baseClient.runConnections`), so a read failure on
one server outweighs a connection failure on another. The raise is opt-in per
client (`clientProfile.failedCommandStatus`, set by `readClientProfile`):
`dtail` follow mode, `dmap` (its scheduled modes handle failures, see "Failed
scheduled jobs" below) and `dtailhealth` keep their statuses; interactive
`dmap` thus exits `0` and writes its outfile also after a reported `map:
incomplete result`, which dserver only sends when a final result was cut off
(see "Server-Side MapReduce" below), and also after a session cut short
(below; `dmap` does not set `cutShortStatus` either), e.g. by the idle
timeout, a dserver SIGTERM or a lost connection while it sent the final
result, which then has no report. This is by design, so that it always
writes what it received; instead it logs every reported failure that is no
file read failure, and every session cut short that the client did not end
itself, at WARN first (`The mapreduce result may be incomplete|<server>:
<reason>; ...`, the reason for a session cut short being `the session ended
before the server completed it`, `MaprClient.incompleteResultReasons` called
by `MaprClient.finish`). Whether the client ended a session is decided per
session at the moment it ended (`baseClient.runSession` records it next to the
connection, reset by `replaceConnection`): it did if the context of
`runConnections` was done by then, which SIGINT/SIGTERM and an
`--interactive-query` `:quit` or control TTY end cancel. So `:quit` warns
about nothing, while a server that went away mid-run is still warned about
when the user presses Ctrl-C only later.
A session cut short (it ended without `.syn close connection`,
`handlers.SessionOutcome.Completed` false: e.g. a dserver SIGKILLed, crashed or
stopped mid-read, or a lost connection) exits `1` (`clients.exitStatusCutShort`
via `clientProfile.cutShortStatus`, which only `readClientProfile` sets), and the
client logs `Session ended without the server's close handshake, the output may
be incomplete` for that server, unless the connection already reported an
error (handler status above 0; client handlers start at `-1`). A session the
client ended itself keeps its status: `baseClient.runSession` checks, when the
session ended, the context of all connections (not the per-connection
`connCtx`, which the connector cancels itself when its session ends), which
SIGINT/SIGTERM (cancel of `Client.Start`'s context) and an `--interactive-query`
`:quit` or control TTY end (cancel of `runConnections`' context) cancel, and
passes the result to `baseClient.sessionStatus`. A
reported failed command (`2`) outweighs it. Every protocol 5 server (also the
in-process server of serverless mode) sends the close handshake after a
completed session's output, and a server of another protocol version is
rejected (see "Wire Framing"), so no capability gates this;
`--max` stops on the server, which then closes normally. A session whose
frames are of another protocol version is no completed one: dserver sends the
mismatch error once (`handlers.commandDispatcher.rejectProtocolVersion`).
How the session goes on depends on the kind of client
(`handlers.mismatchedClient`, `internal/handlers/protocol_codec.go`):
- An older client (protocol major version <= 4, an unparsable version, or no
  `protocol` prefix at all, as v1.x clients send `%s;` frames) keeps its
  session, until the client exits or `IdleSessionTimeoutS`
  (`ServerHandler.versionRejected`). Clients that retry reconnect a few
  seconds after a session ended (v1.x-v3 `dtail`, v3 `dmap` in tail mode
  without outfile after 2 s), so ending it would make them reconnect in a
  loop with full SSH handshakes and server log lines. Observed with 4.1
  builds of this fork from before protocol 5 (3ea4784) against a protocol 5
  dserver that ended the session: their `dtail` reconnected about every 3 s
  until stopped (their `dcat` and `dmap` exited `1`). What holds for every
  such client, released 4.x and <= 3 ones included: it gets the mismatch
  error (`... does not match client protocol version '...', please update
  DTail client`), possibly after unreadable output (the capabilities frame
  is sent in protocol 5 framing before dserver knows the client's version),
  and its session stays open until the client exits or
  `IdleSessionTimeoutS`, so a one-shot client such as `dcat` waits until it
  is stopped. The pre-protocol-5 fork builds above also report
  `unsupported DTail server: missing capability escaped-framing-v1`;
  released 4.x clients (e.g. v4.3.4) predate that check.
- The health handler keeps only the sessions of clients of protocol 3 and
  lower (and unparsable or missing versions) open
  (`HealthHandler.versionRejected`): `dtailhealth` does not reconnect, so a
  4.x `dtailhealth` gets its session ended at once and its check fails.
- Any other version (e.g. a future one, or another 5.x minor version) gets
  the session ended without the close handshake
  (`handlers.baseHandler.closeWithoutHandshake`), as such a client may not
  recognize or acknowledge it, so it sees the error and the session ends at
  once instead of after the 5 s acknowledgement timeout. Clients that check
  for the close handshake treat that session as cut short (`dcat`/`dgrep`
  exit `1`, a scheduled job counts a transport failure).
In `--plain` server mode a payload line starting with `.syn command failed`
or a payload line `.syn close connection` is payload (printed, exit
unchanged), as every control frame is marked (see "Wire Framing" above). With
`--interactive-query` the client keeps the failures of every workload of the
session that reached it, also of workloads a `:reload` replaced; dserver drops
queued messages of a stale generation (`outputCoordinator.shouldDropGeneration`),
failure reports included, and a cancelled workload reports none
(`readCommand.reportFailure`), so those do not count.
User docs: `doc/examples.md` ("Exit statuses of `dcat` and `dgrep`").

### Output Path and MapReduce Operations
DTail uses a single, channel-less read/output path for both direct output
operations (cat, grep, tail) and MapReduce operations in server mode. This was
formerly called "turbo boost" mode and offered as an opt-out optimization; it is
now the one and only mode. There is no on/off toggle: the old
`DTAIL_TURBOBOOST_DISABLE` environment variable and the `Server.TurboBoostDisable`
config field have been removed. `DTAIL_TURBOBOOST_DISABLE` is now inert (a no-op),
and a leftover `TurboBoostDisable` key in an old config file is silently ignored
(config decoding does not reject unknown keys). The example schema
`examples/dtail.schema.json` does not list the removed `Turbo*` keys, so schema
validation flags them as stale, while the runtime still ignores them.

**Technical Details:**
- For cat/grep/tail: the read path writes directly to the output/connection
  without channel hand-offs.
- Without before-context (max/after-only context is eligible), an emitted file-backed
  cat/grep/tail line is handed to `DirectLineProcessor` as a borrowed slice of
  the reader's buffer through the optional `line.RawProcessor` interface, so no
  pooled per-line buffer is used; the slice is only valid during the call and
  must not be retained. Before-context greps, journal (`journal:`) reads and the
  MapReduce processor keep the owned-buffer `ProcessLine` path.
- For MapReduce in server mode: lines are processed directly without channels.
- For MapReduce in serverless/client mode: the server-side direct processing does
  not apply — client-side aggregation runs on the client.
- Client receive of aggregate messages (`internal/clients/handlers/maprhandler.go`,
  `internal/mapr/client/aggregate.go`): frames are cut with `bytes.IndexByte`,
  each message is validated and copied into a reused queue (`Aggregate.Add`),
  and at the end of every `Write` call the queue is aggregated, in arrival
  order and each message on its own, straight into the global group set under
  one non-blocking lock attempt (`GlobalGroupSet.UpdateNoblock`); a missed
  attempt keeps the queue, retries 10 ms later in the background
  (`Aggregate.retryMerge`, again without waiting for the lock; without it
  the rows waited for the connection's next aggregate messages, one
  interval later, as the servers send at the same moment) and at the next
  call (waiting for the lock only once 4 MiB are queued), and `Flush` on
  shutdown waits. The result therefore does
  not depend on how messages were batched or on lock contention.

**Server-Side MapReduce (dserver):**
- Lines are processed directly without channel overhead
- Batch processing reduces lock contention
- Memory pooling reduces garbage collection pressure
- Same output format and accuracy regardless of workload
- Results are sent as one aggregate frame per group, but a session output read
  frames every already queued group, up to the read buffer (32 KiB from the
  SSH copy), into one SSH channel write (`sessionFramer.readMaprMessage`,
  `outputCoordinator.tryReadQueuedMapr`; the per-session queue holds
  `maprMessageQueueSize` = 256 groups). It takes a group only where the next
  read would have taken it anyway (no payload, flush error or server message
  pending), filters each by its generation, and changes no byte of the stream.
  A 200k-group result went from about 149k write(2) calls to about 2.2-2.7k.
  The rest of a batch that does not fit into one read goes out with the
  groups queued behind it, not alone in a tiny write of its own.
  The queue is bounded by count only and lies outside `OutputBufferMaxBytes`:
  a group's key and values (e.g. `last()`) can each be up to `MaxLineLength`
  bytes, so a session can hold up to (256 + 16 + 1) x (1 + select fields) x
  `MaxLineLength` bytes of queued groups (the session queue, the map command's
  16-entry forwarding channel and the forwarder's message in hand). Typical
  groups are a few dozen bytes; the aggregate itself holds all groups anyway.
- The final serialization of a map command (`Aggregate.finalize` in
  `internal/mapr/aggregate/aggregate.go`) has no deadline of its own: a slow
  or paused client only slows it down through output backpressure, and only
  the session's context (session teardown, dserver shutdown, a replaced
  generation) or the connection's `IdleSessionTimeoutS` ends it early. It
  used to stop after a fixed 10 s, dropping the unsent groups while the
  session still closed normally (an incomplete outfile with exit status 0).
  The finalization records a three-way outcome: none (it never ran, e.g.
  an abort won because the command was canceled or the session generation
  was replaced, which has no final result), complete, or interrupted (the
  finalization context ended while it waited for the processors or part way
  through the sends). An interrupted final serialization drops the unsent
  groups instead of re-merging them, as nothing would send them any more,
  and logs one WARN (`Aggregate final serialization interrupted; final
  result incomplete`, with the number of unsent groups). As a defensive
  guard, the session's graceful shutdown then reports `.syn command failed
  map: incomplete result` (`protocol.CommandFailureMapIncomplete`,
  `Aggregate.FinalResultInterrupted`, `reportIncompleteMapResult` in
  `internal/handlers/basehandler.go`) before the close handshake, so a
  scheduled job writes no outfile. With the deadline gone, the context ends
  listed above tear the session down rather than close it gracefully, so a
  client normally sees a session cut short instead (a scheduled job then
  writes no outfile either); the report covers any path that still ends a
  final result early and then closes normally. Interactive `dmap` keeps its
  exit status (`0`) and writes its outfile in both cases, but warns about
  the reported failure as well as about the session cut short (see "Client
  Exit Statuses" below). Each final serialization logs `Aggregate final
  serialization started` with its group count at INFO; the count (like the
  unsent count of the WARN) excludes the groups a periodic serialization
  still in flight took for its own snapshot, which that pass sends or
  re-merges. `integrationtests/dmap_slow_client_test.go` (query with
  `interval 3600`, so no periodic pass runs) stops a client for 15 s once
  that line shows all 100,000 groups still to be sent, and requires every
  group in the outfile and no interrupted serialization in the server log.

**Output limits and deadlines (server config):**
The output path exposes four optional settings: `OutputBufferMaxBytes`,
`OutputFlushTimeoutMs`, `OutputReadRetryIntervalMs`, and
`OutputEOFAckTimeoutMs`. The buffer setting bounds retained payload memory; the
two timeout settings bound flush and EOF-ack waits. The flush timeout of the
payload buffer (`outputManager.flush`) is a stall timeout: it starts over
whenever the reader took something since it was last checked, so a client on a
slow link is waited for. It, the session flush (`flushContext`) and the 5 s
wait for the close acknowledgement also do not run out while the session
reader is still writing bytes it already took to the client
(`outputCoordinator.clientSlow`): a paused client (a pager, a stopped
pipeline) only applies backpressure, as during the read, and the connection's
idle timeout bounds one that never comes back. Only a reader that takes
nothing for a whole timeout without a pending delivery fails the flush.
`OutputReadRetryIntervalMs`
is a compatibility fallback for stale-generation writers that do not provide a
cancellation signal, and values below the implementation safety minimum are
clamped. Removed `Turbo*`, output-delay, EOF-wait-sizing, flush-poll, and
shutdown-wait keys in older config files are ignored by the lenient decoder.

Authenticated connections use the rolling `IdleSessionTimeoutS` timeout (default
900 seconds). Successful network reads or writes mark the connection active, and a
per-connection ticker (timeout/4, clamped to 1-30 seconds) pushes the deadline to
now + timeout + two intervals when activity was seen, so the hot I/O path does no
clock reads or `SetDeadline` calls. Active follow sessions stay connected: any idle
gap up to the timeout is still refreshed in time even when the refresh tick runs
less than one interval late. Clients with no SSH activity are closed between the
timeout plus two intervals and the timeout plus three intervals after their last
activity (about 960-990 seconds with the defaults; late ticks can only extend
this). The interval never drops below one second, so a timeout below four seconds
closes two to three seconds past it instead of proportionally.
Per-session payload backing memory waiting on a slow client is capped by
`OutputBufferMaxBytes` (default 2 MiB); producers apply backpressure when the cap is reached. The
configured cap must leave room for one maximum-length formatted line: it must be
at least `MaxLineLength + 128 KiB`. Every session escapes its frames (see Wire
Framing), so every client session (`ServerHandler`; a health session sends
only a few short frames and keeps the default) raises its own cap to at least `2*MaxLineLength + 128 KiB`
(`escapedOutputBufferMaxBytes`, `sessionOutputBufferMaxBytes` in
`internal/handlers/serverhandler.go`), since escaping 0xAC/0xFF bytes can
almost double a line frame; validation is unchanged, so every configuration
valid before keeps working, and memory stays bounded at about one more
maximum-length line per session. A batch of line
frames that still exceeds the cap is reported, not dropped silently: the client
gets a visible `Dropped output too large for the session output buffer` warning
and the failed-command reason `output: too large for the session output buffer`.

**Shared follow reads (dserver):**
Sessions that tail the same uncompressed file share one reader of it in
dserver (`internal/io/fs/readhub`); `Server.SharedReadsDisable: true` turns
this off. Each session still passes its own permission check, holds its own
tail slot, and filters, numbers and processes the lines itself. A session
starts at the end of the file at the path as of its join, like a private
follow read: it skips published lines that were in the file before, also when
it joins after a rotation or copytruncate the shared reader has not caught up
with yet. The shared reader never waits for a session: one that falls more
than 64 chunks (about 64 KiB each) behind is evicted, logged at INFO with the
remaining subscriber count. It handles what it had queued (including a
rotation, truncation or reader panic that no longer fit) and goes on with a
private reader of its own target just past its last line, keeping its line
numbering and local context; a shared reader failing without a panic hands
every session over the same way. Every session holds its own descriptor of
the file the shared reader has open: a separate open (not a dup, so offsets
are not shared) through the session's own validated target, made when the
reader opens a file or the session joins and kept only if it is that same
file. The private reader starts in that descriptor, so a rotation loses no
line, also when the path was rotated before the eviction while the shared
reader was still behind in the old file (a burst larger than the queue
followed by a size-triggered logrotate). This is portable (no `/proc`) and
costs one descriptor per session, as a private reader does. The one
remaining gap: if the path is rotated in the moment between the shared
reader opening a file and a session opening its descriptor, that session has
none, and if it is later evicted with lines of the old file unread, it reads
the new file from its beginning and logs a warning. Follow reads of compressed
files and max-count (`--max`) follow reads read privately, as do journal
targets, stdin and serverless mode; one-shot reads (`dcat`, `dgrep`, `dmap`)
read privately unless they belong to a scheduled job group (below), whose
shared read also covers compressed files.

At a rotation, every follow read, shared or private
(`internal/io/fs/readfile_processor_optimized.go`), reads the old file to its
end, then polls it again 100 ms later, and after every poll that found more
lines, at most 10 times, for lines a writer that has not reopened the path yet
still appends, before it moves on to the new file. Those lines keep their
order and numbering. A last line of the old file that is still unfinished when
the draining ends is dropped, as a follow read always dropped it.

Output with sharing on equals output with sharing off, checked with
SIGSTOPped, context, late-joining (also right after a rotation or
copytruncate), `--query` `dtail` clients and a rotation right after a burst
that evicts lagging sessions (with and without a SIGSTOPped client). Apart
from the rotation gap above, two differences remain: a session that joins in
the middle of a line being written gets that whole line, where a private read
gets the rest of it from the join; and a trailing line the writer has not
finished yet is not passed on when a session leaves.

Long line warnings match too: a session sends one `Long log line, splitting
into multiple lines` warning per line cut at `MaxLineLength`, as a private
read does, also when it is evicted between a line's warning and its first
piece (it holds the warning until that piece arrives and leaves it to its
private reader otherwise, `sessionRead.longLineWarning`) or in the middle of
the line (its private reader does not warn again, `StartOffsetInSplitLine`).

A failed read reaches the client the same way, too. A private read reports
every failed iteration of its retry loop to the client with the hidden
message `.syn command failed read: unable to read file` (once per command,
see `protocol.HiddenCommandFailedPrefix`), and a shared read reports the same
for each of the four ways it fails: the session's processor or filter failing
(`readCommand.readShared`, which then restarts privately), one read of the
shared reader failing (it reads the file again, as a private reader does, and
every session of it is told), the shared reader failing for good without a
panic (each session goes on privately) and a failed read of a session's own
reader after an eviction. The hub reports the last three through
`readhub.Session.ReportFailure`, with the error that failed, which the read
command reports as its own read loop does (`readCommand.reportReadError`):
output a session's own reader after an eviction dropped as too large for the
session output buffer gets the `Dropped output too large ...` warning and the
reason `output: too large for the session output buffer`, not `read: unable to
read file`. A max-count stop is no failure in either
path, and a reader worker panic ends the session as a panic in both, without
a report.

An evicted session rejoins the shared reader once it caught up
(`internal/io/fs/readhub/rejoin.go`): whenever its private reader reaches the
end of the file at the start of a line, at offset P of file F, it asks
(`fs.ReadOptions.HandOverAtEOF`) to hand over. Under the entry's `publishMu`
the session is added back if the shared reader has F open, is not in the
middle of a read call (it flags each read before it starts, atomically, and
clears the flag under `publishMu` once it recorded how far it read), and has
neither read past P (e.g. an unfinished line: should F then be truncated to a
size between P and it, the reader would restart and publish the rewritten
lines up to P again) nor published a line ending past P; it then skips
published lines ending at or before P, so the hand-over loses and repeats no
line, and keeps its filter (numbering, context, max-count state), processor
and a held descriptor of F. If no shared reader exists (its last session left
or was evicted, or its reader failed, so the hub closed or dropped the entry),
the session starts a new one at P, reading from a descriptor of F opened
through its own target, and stays private when no such descriptor can be
opened, e.g. because the path is no longer F. Otherwise (reader in a read,
read or published past P, on another file, between reads, or no descriptor of
F to hold) it stays private and tries again at a later end of the file. A
declined attempt, or an eviction within 30 s of a rejoin, pauses the attempts
for 1 s doubling up to 30 s. Each rejoin is logged at INFO ("Evicted
subscriber rejoined the shared follow read", with the subscriber count). A
large burst (about 55-60 MiB written at once in the checks) can evict sessions
that are merely slower than the reader; they rejoin afterwards. In a check
with 10 sessions after a 300,000-line burst, dserver read about 40 MiB for the
next 200,000 lines instead of about 380 MiB without rejoin (CPU time
differences were within noise), and every output equalled sharing off.

Historical measured costs (N=4, 100 MiB; report retained in Git via
`git show 5eece8d:docs/shared-reads-benchmark-2026-09.md`):
scheduled groups read the file once and finish about 3.7 times sooner, at a
slightly higher dserver CPU (about 6-8%); a 100 MiB follow burst evicts every
session, so it costs what sharing off does; a paced follow (10 MiB/s) used
about 14% more dserver CPU with sharing on than off.

**Shared one-shot reads of scheduled job groups (dserver):**
The scheduler (`internal/jobs/schedulergroup.go`) starts due jobs that read
the same files from the same servers, and do not write each other's outfiles
or read files another writes, together as a group, when all their servers are
this dserver (`internal/jobs/localserver.go`), in waves of at most
`min(MaxConnections/4, MaxConcurrentCats)` divided by the number of servers,
and of at least one job, so that a wave is no larger than the group read the
hub shares among at most `MaxConcurrentCats` members; other jobs, and all
jobs when `Server.SharedReadsDisable` is set, run one at a time. Each set of
servers (a job's `Servers` and `Discovery`) is discovered and resolved once
per scheduler run for static server lists, not once per group that run forms,
and not at all while only one run is left pending, as there is nothing left
to group it with. For a static hostname the DNS answer is kept for that one
run; the next scheduler run refreshes it. FILE discovery (explicit or inferred
from a file path) and its hostname lookups run again for every wave because
each job's client reads
the file at connection time, and the file or DNS answer may change between
waves. A
scheduler run looks the servers up as soon as a due run is evaluated with
other runs still pending, even when those all turn out not to be due; a run
that is not due leaves the group formation before the lookup, so a scheduler
run whose only due run comes after the others needs no lookup at all.
Each job sends the option `share=<group>:<members>`, where members counts
the sessions of the group (its jobs times the servers each connects to, see
`scheduler.runGroup`); dserver honours it only
for the scheduler's user `DTAIL-SCHEDULE` and ignores it elsewhere. Each
member passes its own permission check and joins
with its own validated target. The hub (`internal/io/fs/readhub/oneshot.go`)
waits until the members joined or `GroupWait` (3 s) passed, takes a free cat
slot per member without waiting for one while holding another, reads the file
once from its beginning and delivers every line (with its newline, empty lines
included) to every member, which numbers from 1 and filters on its own. This
is a snapshot read: delivery blocks for a slow member instead of evicting it,
there is no join skip and no held descriptor; a cancelled member leaves and
releases the group. A member arriving after the read started, beyond
`MaxConcurrentCats` members, without a free slot, or within 10 minutes after
the group's read ended reads privately. The follow-only hub behaviour
(eviction, join skip, held descriptors, read positions, in-order long line
warnings) is wired through optional interfaces the fan-out processor
type-asserts (`readTracker`, `warningSource`), which only the follow entry
implements.

**Failed scheduled jobs: strict until the TimeRange ends (dserver):**
The scheduler skips a job for the rest of its date period once its outfile
exists, so a scheduled job writes its outfile and `.query` file (both via
`.tmp` and rename, no interim results) only as described here. Two kinds of
failure are told apart:
- **Transport failures** never write an outfile, not even after the job's
  TimeRange ended: no server connection at all; a connection that ended with
  a non-zero status (refused connection, failed SSH handshake, rejected
  session); the job's context canceled (e.g. the scheduling dserver's own
  shutdown); a session that ended without the server's close handshake
  (`.syn close connection`, sent only after all of the session's output),
  e.g. a remote dserver killed with SIGKILL, stopped with SIGTERM/SIGINT,
  crashed, or a lost connection; the client refusing to send a job's read
  commands to a server lacking `journal-v1` for `journal:` targets in its
  `Files`, which sets exit status 1;
  and a failed command that is no file read
  failure (`command: unable to decode command`, `command: rejected`,
  `command: unknown command`, `map: invalid query`, `map: incomplete result`,
  `read: unable to parse command`, `output: too large for the session output
  buffer`, or a reason the client does not know). `map: incomplete result`
  means the map command's final result did not reach the client in full (see
  "Server-Side MapReduce" above). The output reason means a read dropped line
  frames that did not fit the session's output buffer (see the output limits
  above); it is no file read failure, as the file was read. A scheduled job's
  server-side MapReduce sends aggregates, not line frames, so it cannot get it.
  dserver does not run the read commands that follow a failed map command of
  the same command stream (plain command stream or session generation,
  `internal/handlers/mapqueryfailure.go`): each reports `map: invalid query`
  too, so no line is streamed as plain payload in place of MapReduce results.
  This includes a map command rejected before it runs (e.g. options that do
  not parse), which itself reports `command: rejected`. A SESSION START or
  UPDATE builds its map command before it changes the session, so one that
  can not be built rejects the request and a running generation keeps
  running.
- **File read failures**, reported by dserver with the hidden message
  `.syn command failed <reason>`
  (fixed reasons from `internal/protocol/session.go`, no paths or error
  details; those stay in the server log), sent before the close handshake
  (`protocol.IsFileReadFailure`): `read: no file to read` (a glob matched no
  file after its retries, a file vanished or is a dangling symlink, or every
  path a glob matched is no regular file), `read: no permission to read file`
  (denied by the read permissions), `read: unable to create file reader`,
  `read: unable to read file` (open/read error, e.g. permission denied on the
  file system) and `read: more files than the server reads` (the glob matched
  more than `MaxGlobTargets`; only the first ones are read). This covers a
  file of the job that does not exist yet (e.g. `$today`'s log), a
  comma-separated `Files` list with a file that never appears, a glob matching
  no file, and a multi-server job whose file is missing on one host.

Directories and other non-regular files a glob matches are skipped silently
(server log at INFO, no client warning, no failure), unless the glob matched
nothing else (then `read: no file to read`). Read permissions are checked
before this, so a denied path reports `no permission` whatever it is.

Within the job's TimeRange (`clients.ScheduledMode`) any failure blocks the
outfile: the job writes nothing, keeps an outfile of an earlier run untouched,
and logs `Job <name> failed and wrote no outfile <path> (failure <n> in a
row), it runs again from about <time>`; the client logs why (`Not writing the
mapreduce result as the query did not complete` for transport failures, `Not
writing the mapreduce result as files could not be read ...` with
`<server>: <reason>` entries for file read failures). The job is backed off in
memory (`internal/jobs/backoff.go`, keyed by job and filled-in outfile): it
runs again 1, 2, 4, 8, 16, 32 and then every 60 minutes after the start of
its last failed run, but at least half that time after the run's end (a job
running longer than its backoff does not run back to back), on the first
scheduler run (every minute) from then on, with 5 seconds of slack for
scheduler drift. Runs skipped by the backoff are logged at DEBUG only.

Once the TimeRange of a failed run has ended (its end hour on the day of the
run; for `[0, 24]` that is the next midnight, when the `$today` period of the
outfile has passed; a job with no or an empty TimeRange never runs at all),
the scheduler runs **final runs** (`clients.ScheduledPartialMode`) with the
files and outfile (dates filled in) of the job's last failed run within its
TimeRange, before the scheduler run's other jobs: the first at the first
scheduler run from the range end on (ignoring the backoff), later ones after
the backoff, which goes on counting. Due final runs are grouped like runs
within the TimeRange (same files, servers and discovery, no conflicting
outfiles or reads, configured job order, waves of the same bound), and each
wave shares its reads, so jobs that failed together read their files once for
their final runs too. A final run logs `Starting final run of job <name> for
outfile <path> after its TimeRange ended at <time>, ...`. If its only failures
are file read failures, it writes what it could read (as every run did
before runs within the TimeRange became strict)
and the client logs `Writing partial mapreduce result after the job's
TimeRange ended|<server>: <reason>; ...`; if every file could be read by then,
it writes the complete result. A transport failure still writes nothing and
the final runs go on after the backoff. The scheduler gives up on an outfile
24 hours after its TimeRange ended (`Giving up job <name> after <n> failures
in a row: it wrote no outfile <path> within 24h0m0s ...`), and skips (and
forgets) a final run whose outfile exists by then. For an outfile without
dates, the next day's TimeRange takes over: its first run ignores the previous
range's backoff, a failure within it starts the failure count and the final
runs over, and runs within it are strict again and move the range end.
Failures are forgotten when the job writes the outfile and with a dserver
restart: after a restart within the TimeRange the job runs strictly again, but
a dserver restarted after the TimeRange ended does not know the failed run and
writes no outfile for that period.

Compatibility: every protocol 5 dserver reports failed commands, and a peer
of another protocol version is rejected (see "Wire Framing"), so a scheduler
always gets the failure reports and the close handshake. Continuous jobs and interactive `dmap` with an outfile
keep writing interim and final results whatever the exit status. A job whose
server discovery yields no server at all (e.g. a server list file of only
blank and comment lines; empty `Servers` mean this dserver, see `jobServersStr`
in `internal/jobs/runner.go`) fails when its client is created
(`clients.ErrNoServers`, `no server to connect to`, logged at WARN as `Unable
to create job <name>`): a scheduled job counts that as a failed run as above,
and a continuous job writes no outfile and runs again at its next retry,
every minute, without a backoff.

**Best Practices for High-Concurrency MapReduce:**
1. Increase MaxConcurrentCats in the server configuration to match workload
2. Use server mode for large-scale MapReduce operations
3. Monitor logs for performance metrics

Note: three operator-facing diagnostic log lines still contain the word "turbo"
verbatim ("Using turbo mode for reading", "Using turbo aggregate processor for
MapReduce", "Creating turbo aggregate for MapReduce"). These are deliberately kept
as stable log strings and do not imply a separate mode.

## Benchmarking & Profiling

```bash
# Run benchmarks
make benchmark

# Run performance profiling (see the Profiling section for all profile-* targets)
make profile-quick
make profile-all

# Run specific benchmark suites
make benchmark-quick
make benchmark-full

# Compare the local fork with upstream
make benchmark-upstream-smoke
make benchmark-upstream
```

## Profile-Guided Optimization (PGO)

```bash
# Run PGO for all commands
make pgo

# Quick PGO with smaller datasets
make pgo-quick

# PGO for specific commands
make pgo-commands COMMANDS='dcat dgrep'

# Clean PGO artifacts
make pgo-clean

# Show PGO help
make pgo-help

# Direct usage with dtail-tools
dtail-tools pgo                    # Optimize all commands
dtail-tools pgo dcat dgrep         # Optimize specific commands
dtail-tools pgo -v -iterations 5   # Verbose with 5 iterations

# After PGO, optimized binaries are in pgo-build/
```

### PGO Notes

- PGO uses profile data from real workloads to optimize binary performance
- The process involves: building baseline → generating profiles → building with PGO
- Typical improvements range from 5-20% depending on the workload
- Optimized binaries are placed in the `pgo-build/` directory

## Architecture & Code Organization

### Binary Entry Points
- `/cmd/dtail/` - Remote log tailing client
- `/cmd/dserver/` - Server daemon
- `/cmd/dcat/` - Remote file reading client
- `/cmd/dgrep/` - Remote file searching client
- `/cmd/dmap/` - MapReduce query client
- `/cmd/dtailhealth/` - Health check client

### Core Implementation
- `/internal/clients/` - Client implementations for each tool
- `/internal/server/` - Server daemon logic
- `/internal/handlers/` - Transport-neutral session handlers and read engine
- `/internal/jobs/` - Scheduled and continuous MapReduce client workloads
- `/internal/mapr/` - MapReduce engine and query parsing
- `/internal/authkey/` - Shared in-memory cache for registered public keys
- `/internal/sessionuser/` - Session identity and read authorization
- `/internal/ssh/` - SSH client/server components
- `/internal/config/` - Configuration management
- `/internal/io/` - File operations, logging, compression handling

### Key Architectural Patterns

1. **Client-Server Communication**: All clients communicate with dserver instances via SSH protocol on port 2222 (configurable)

2. **MapReduce Query Engine**: Located in `/internal/mapr/`, implements SQL-like query language for distributed log aggregation

3. **Configuration System**: JSON-based configuration in `/internal/config/`, supports both client and server settings

4. **SSH Integration**: Custom SSH server implementation in `/internal/ssh/server/` and client in `/internal/ssh/client/`

5. **Compression Support**: Automatic handling of gzip and zstd compressed files in `/internal/io/`

6. **Auth-Key Fast Reconnect**: Client registers a public key via `AUTHKEY`; server validates against in-memory auth-key cache before falling back to `authorized_keys`

## Important Implementation Details

- **Main Server Loop**: `/internal/server/server.go` - Core server processing logic
- **Client Base**: `/internal/clients/baseclient.go` - Common client functionality
- **MapReduce Parser**: `/internal/mapr/` (query.go, token.go, whereclause.go, wherecondition.go, selectcondition.go, groupset.go, etc.) - SQL-like query language parser
- **Partial Query Analysis**: `/internal/mapr/complete.go` (`AnalyzePartial`, with complete_scan.go and complete_walk.go) - reads a query typed up to the cursor with the tokenizer and clause rules of the parser: token kinds with byte offsets for highlighting, the word under the cursor, whether a comma precedes it (`AfterComma`) and the kinds of words expected there for completion; `AnalyzeQuery` reads a whole query (both sides of the cursor) the same way, with no word under the cursor, for the clauses and table a completion must know; tests compare its tokens with `tokenize` so they cannot drift
- **Log Format Parsers**: `/internal/mapr/logformat/` - Extensible log parsing system
- **Field Probe Sampler**: `probe.Sample` (`/internal/mapr/probe/sample.go`)
  returns a bounded, one-shot sample of one validated read target for the
  MapReduce field probe; it does no permission check (the caller resolves the
  target with `ResolveReadTarget` first). Uncompressed files: the lines
  starting in the last 256 KiB (partial first line dropped), at most 500, via
  `fs.SampleTail` (`/internal/io/fs/sample.go`, `ReadAt`, file offset
  untouched), plus the file's first line in `fs.Sample.FirstLine` when the
  window starts past offset 0, so a csv probe can still see its header.
  gzip/zstd files (by path suffix, as read commands): the first 500 lines
  (`fs.SampleHead`; `.zst` fails in `DTAIL_NO_ZSTD` builds); a head
  sample stops after scanning 4 x 256 KiB of decompressed input
  (`fs.SampleHeadScanFactor`, skipped lines and terminators included, plus
  the rest of the line or `MaxLineLength` piece crossing it), so a file of
  oversized lines only is never decompressed to its end. `journal:unit`
  targets (one literal unit name with its type, `fs.validateJournalSpec`):
  `journalctl -u unit -n 500` through the journal reader (error
  without `journalctl`/off Linux). The lines of every sample are at most 500 and
  256 KiB of line data (`fs.SampleOptions`; `FirstLine` comes on top), cut
  at `MaxLineLength` with `fs.SplitLine` and returned without their LF/CRLF
  terminator; a line larger than 256 KiB on its own is skipped, so it neither
  evicts nor ends the sample. The tail window is streamed (64 KiB buffer),
  never held whole. Options of a table probe: `ScanBytes` (the tail window
  and head scan bound instead of the defaults), `ScanLines` (`journalctl
  -n`), `Keep` (lines kept; the others count towards the scan bounds only;
  `FirstLine` is then also returned when the window starts at 0 but its
  first line was not kept) and `Observe` (sees every scanned line).
- **SSH Authorization Callback**: `/internal/ssh/server/publickeycallback.go` - auth-key fast-path + `authorized_keys` fallback
- **AUTHKEY Handler**: `/internal/handlers/serverhandler.go` - session command handling for auth-key registration
- **Auth-Key Cache**: `/internal/authkey/authkeystore.go` - in-memory per-user key cache (TTL/max-keys)

## Configuration Files

- Server config: `/etc/dserver/dtail.json` or `./dtail.json`
- Example configs: `/examples/`
- Docker configs: `/docker/`

### Auth-Key Related Options

- Client: `--auth-key-path`, `--no-auth-key`
- Client config: `Client.AuthKeyPath`, `Client.AuthKeyDisable`
- Client env: `DTAIL_AUTH_KEY_PATH` (takes precedence over `DTAIL_SSH_PRIVATE_KEYFILE_PATH`)
- Client env: `DTAIL_SSH_PRIVATE_KEYFILE_PATH` (legacy; used only when `DTAIL_AUTH_KEY_PATH` is unset)
- Server config: `Server.AuthKeyEnabled`, `Server.AuthKeyTTLSeconds`, `Server.AuthKeyMaxPerUser`

## Common Development Tasks

When modifying client behavior:
1. Check `/internal/clients/` for the specific client implementation
2. Common functionality is in `baseclient.go`
3. Client-specific logic is in respective files (e.g., `tailclient.go`, `catclient.go`)

When modifying server behavior:
1. Core server logic is in `/internal/server/server.go`
2. Session identity and read authorization in `/internal/sessionuser/`
3. Handler implementations in `/internal/handlers/`

When working with MapReduce:
1. Query parsing in `/internal/mapr/` (query.go, token.go, whereclause.go, wherecondition.go, selectcondition.go, groupset.go, etc.)
2. In-process aggregation in `/internal/mapr/aggregate/`
3. Log format parsing in `/internal/mapr/logformat/`
4. Log format probes (`logformat.Probe`, `ProbeWithParser`, the optional `Prober`, `Names`, `KnownVariables`) in `/internal/mapr/logformat/probe.go`; see "Custom probes" in `doc/logformats.md`
5. The language vocabulary is table-driven: clause keywords (`keywords`,
   `mapr.Keywords`), aggregations (`aggregationTable`,
   `mapr.AggregationNames`), where operators (`whereOperatorTable`,
   `mapr.WhereOperators`, with a numeric/string kind) and set functions
   (`funcs.functionTable`, `funcs.Names`). The parser and the exported
   accessors (for query completion) read the same tables, resolved at parse
   time only; add a new word to its table, not to a switch. The table
   only names words: a new spelling of an existing operation (e.g. `=<`)
   needs nothing else, and a set function brings its implementation in its
   table entry, but new behaviour still needs code beyond the table: a
   clause keyword its clause in the parser (`query.go`), a where operator
   its `QueryOperation` evaluated in `whereCondition.floatClause` or
   `stringClause`, and an aggregation its `AggregateOperation` handled in
   `AggregateSet.Aggregate` and `fieldMergerFor` (`aggregateset.go`) and in
   the result rendering (`GroupSet.resultSelect`, `groupset.go`).
   `TestVocabularyMatchesDoc` fails when `doc/querylanguage.md` differs.
