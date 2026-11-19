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

## Known Limitations

### Interactive Query Reload
Interactive query control is opt-in on the client with `--interactive-query`.
The controlling TTY accepts `:reload <flags>`, `:show`, `:help`, and `:quit`.
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
- Initial interactive bootstrap prefers `SESSION START` when the server
  advertises capability `query-update-v1`
- If that capability is absent, startup falls back to the legacy command stream
  automatically so mixed-version client/server combinations still run
- Live `:reload` updates require every active server connection to advertise
  `query-update-v1`; unsupported servers cause the reload to be rejected while
  the current workload keeps running
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

### Auth-Key Fast Reconnect
Auth-key fast reconnect is enabled by default. The client can register a public key with `dserver` over an already-authenticated session, and subsequent connections can use this in-memory key before falling back to normal SSH auth.

**Technical Details:**
- Client sends `AUTHKEY <base64-pubkey>` command during session setup
- Server stores keys in memory, per user, with TTL and max-keys limits
- SSH `PublicKeyCallback` checks in-memory auth-key store before `authorized_keys`
- If fast-path auth misses (restart/expiry/mismatch), normal SSH auth is used automatically

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

### File Paths With Spaces
dserver splits every command on single spaces and a read command is
`<mode>:<options> <path> <regex>`, so a path is one word. `session.Spec.Commands`
(the one builder of read commands, for the legacy stream and for the commands
dserver builds from `SESSION START`/`UPDATE` Files, including the
`timeout N <mode> <path> <regex>` form) passes every path through
`protocol.EncodeReadPath` (`internal/protocol/readpath.go`): a path containing a
space, or starting with the prefix `base64%`, is sent as `base64%` + standard
base64 of the path (the prefix config option values already use); every other
path is sent verbatim, byte-identical to older clients. `readCommand.parsePath`
decodes the word before anything else sees it, so globbing, the `readfiles`
permission check (which must match the decoded path), `journal:` targets and
shared reads all use the real path; a prefixed word that does not decode fails
the command with `read: unable to parse command` and reads nothing.

**Compatibility:** dserver (and the serverless runtime) advertise capability
`encoded-read-path-v1`. Clients check it before sending any read command of a
spec with such a path (`requireSpecCapabilities` in
`internal/clients/connectors/sessiontransport.go`, legacy commands and
`SESSION START`) and otherwise fail with `file paths containing spaces or
starting with base64% require server capability encoded-read-path-v1` (exit
status 1), instead of letting an older dserver split the path. A `:reload`
checks every connection up front (`checkReloadCapabilities` in
`internal/clients/interactive_control.go`, also for `journal-v1`) before it
updates any, so with one server lacking the capability the reload is rejected
on the TTY as a whole, no server switches, and the workload and exit status
stay as they were. Space-free paths need no capability. Older clients send
raw paths: in their legacy command stream a spaced path still fails as
before, and a path starting with `base64%` is decoded by a new dserver; via
`--interactive-query` (`SESSION START`/`UPDATE`, Files as JSON) a new dserver
builds and encodes the commands itself, so a spaced path is read and a
`base64%`-prefixed path is read literally.

**Commas:** `--files`, positional arguments (joined with commas) and job `Files`
are split at commas with blanks around each entry trimmed (`splitFiles` in
`internal/session/spec.go`), so paths containing a comma, or with leading or
trailing blanks, cannot be read. The scheduler's job grouping parses the same
comma lists, so this is not changed.

### Journal Source Reads
Journal targets use `journal:unit.service` syntax. Permission rules should match the full target, for example `Server.Permissions.Users[user]: ["readfiles:^journal:.*\\.service$"]`.

**Technical Details:**
- Server capability `journal-v1` is required for any journal-backed read target; clients reject journal sources when the server does not advertise it.
- The capability is advertised only on Linux when `journalctl` is available on `PATH`.
- Journal reads use `journalctl` via `os/exec` only; there is no cgo or `libsystemd` path, and the feature is Linux-only at runtime.
- Non-follow reads the current journal snapshot once. Follow mode adds `-f -n 0` and keeps restarting `journalctl` until canceled.
- Entries are cut at `Server.MaxLineLength` exactly like file lines (shared `fs.SplitLine`/`fs.ScannerTokenLimit`, same long-line warning, passed via `journal.ReaderOptions.MaxLineLength`), so an entry over 1 MiB no longer fails with `bufio.Scanner: token too long`. An unterminated last entry of a `journalctl` run is still dropped (pieces cut off before it are kept).

### Wire Framing (server-to-client stream)
Every server-to-client message (frame) ends with `protocol.MessageDelimiter`,
the single byte 0xAC. That byte is also a UTF-8 continuation byte (`€` is
E2 82 AC, `Ь` D0 AC, `¬` C2 AC, about 1/64 of all CJK characters), so raw
framing cuts such lines in two and drops the byte. Sessions therefore
negotiate escaped framing (`internal/protocol/framing.go`; design in
`docs/dev/wire_framing.md`):
- dserver (and the in-process serverless server) advertises capability
  `escaped-framing-v1` in its capabilities frame, always the first frame and
  always raw.
- `NewClientHandler` and `NewMaprHandler` send `.ack framing escaped-v1` as the
  very first command frame, before `AUTHKEY` and `.ack input begin`, without
  waiting for the capabilities. The health handler does not (the server's health
  handler ends the session after the first command).
- The server switches the session to escaped framing on that request (only if it
  advertised it, only when the request is the session's first command frame, a
  later or repeated one is ignored and logged, and only after its capabilities
  frame was encoded); every later frame body has 0xAC replaced by `FF 01` and
  0xFF by `FF 02`, in time linear in the body however many bytes are escaped
  and with the escaped body sized exactly (counted first), so a dense 1 MiB
  body costs one allocation of its escaped size.
  The client decides from the session's first frame only: if it is the
  capabilities frame listing the capability, every later frame is decoded as
  escaped; a later frame that reads like a capabilities frame never switches
  it. Valid UTF-8 never contains 0xFF, so frames without 0xAC are
  byte-identical in both framings.
- Escaping can grow a frame to almost twice its raw size, so a session that
  switches to escaped framing raises its output buffer cap to at least
  `2*MaxLineLength + 128 KiB` (see the output limits below). Output that still
  does not fit is reported, never dropped silently: a visible warning plus the
  failed-command reason `output: too large for the session output buffer`.
- Current clients require `escaped-framing-v1` in the first server frame:
  without it they report an unsupported-server error with upgrade guidance
  and set status 1, even after a normal close handshake. Older protocol-4.1
  servers can finish a session on unknown `.ack` commands before reading files
  (framing requests and input-batch markers both trigger it), so they must
  never look like successful empty queries. Servers with escaped framing only
  remain supported. Older clients keep raw framing against current servers.
  `ProtocolCompat` stays `4.1`.
- Control frames (`internal/protocol/control_frames.go`): with `--plain`, log
  lines and control messages (hidden `.syn ...` messages, `AUTHKEY` replies)
  are both untagged frames, so a client told them apart by content only and
  dropped log lines starting with `.` or reading `AUTHKEY OK`, and acted upon
  `.syn close connection` / `.syn command failed x` lines. dserver therefore
  also advertises `control-frames-v1`; clients send `.ack framing control-v1`
  as the second command frame, in the same write as the escaped framing
  request. The server honours it only if advertised, the session is escaped
  and it is the second command, and then switches to
  `protocol.EscapedControlFraming`: every control frame body starts with
  `FF 03`, which escaping makes unforgeable, and every other frame is
  byte-identical to escaped framing. The client uses it when the first frame
  lists both capabilities; then every unmarked frame is payload. dserver
  encodes no frame after the capabilities frame until the session framing
  settled (`sessionFramer.awaitFramingSettled`: before a command that can no
  longer change it, i.e. a first command other than the escaped framing
  request or a second one other than the control frames request, after the
  framing requests, or at session end), so e.g. the
  close handshake of a dserver shutting down before the requests arrived is
  still marked. A framing request with a `timeout N` prefix is ignored. Any other
  client/server combination (older ones, or with escaped framing only) keeps
  the content-based routing and its loss of such `--plain` lines.

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
  path), which is what this setting gates.
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
could not be sent, a server lacks `journal-v1` for a requested journal target
or `encoded-read-path-v1` for a path containing a space or starting with
`base64%`, a serverless session failed, a session was cut short (below), or the
client could not start, and `2`
(`clients.ExitStatusFailedCommand`, also the status of an invalid flag) when any
server reported a failed command of the session with `.syn command failed
<reason>` (capability `command-failure-v1`, `handlers.SessionOutcome.Failures`).
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
when the user presses Ctrl-C only later. Servers
without `command-failure-v1` report nothing, so with them a missing file still
exits `0`.
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
reported failed command (`2`) outweighs it. Servers of every protocol version
(since v1.0.0, also the in-process server of serverless mode) send the close
handshake after a completed session's output, so no capability gates this;
`--max` stops on the server, which then closes normally. A session whose
frames are of another protocol version is no completed one: dserver sends the
mismatch error once (`handlers.commandDispatcher.rejectProtocolVersion`).
For a legacy client (protocol major version <= 3, an unparsable version, or no
`protocol` prefix at all, as v1.x clients send `%s;` frames) it then keeps the
session open, as before, until the client exits or `IdleSessionTimeoutS`: such
clients do not check the close handshake, and those that retry (v1.x-v3
`dtail`, v3 `dmap` in tail mode without outfile) reconnect 2 s after a session
ended, so ending it would make them reconnect in a loop with full SSH
handshakes and server log lines. For any other version (e.g. a future 4.x+ client) it ends the session
without the close handshake (`handlers.baseHandler.closeWithoutHandshake`),
as such a client may not recognize or acknowledge it, so it sees the error and
the session ends at once instead of after the 5 s acknowledgement timeout.
Clients that check for the close handshake (this fork since ef3) treat that
session as cut short (`dcat`/`dgrep` exit `1`, a scheduled job counts a
transport failure).
Known gap: in `--plain` server mode a payload line starting with `.syn
command failed` or a payload line `.syn close connection` is payload (printed,
exit unchanged) when client and server negotiated control frames
(`control-frames-v1`, see "Wire Framing" above); only with an older client or
dserver is the first still taken as a failure report (exit `2`) and the second
as the close handshake (the client stops reading that server and exits `0`
with truncated output). With
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
  attempt keeps the queue for the next call (waiting for the lock only once
  4 MiB are queued) and `Flush` on shutdown waits. The result therefore does
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
at least `MaxLineLength + 128 KiB`. A session that negotiated escaped framing
(see Wire Framing) raises its own cap to at least `2*MaxLineLength + 128 KiB`
(`escapedOutputBufferMaxBytes` in `internal/handlers/serverhandler.go`), since
escaping 0xAC/0xFF bytes can almost double a line frame; validation is
unchanged, so every configuration valid before keeps working, and memory stays
bounded at about one more maximum-length line per session. A batch of line
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
for the scheduler's user `DTAIL-SCHEDULE` and ignores it elsewhere (older
dservers ignore it too). Each member passes its own permission check and joins
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
  commands to a server lacking a capability its `Files` need
  (`encoded-read-path-v1` for paths containing spaces or starting with
  `base64%`, `journal-v1` for `journal:` targets), which sets exit status 1;
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
  the same command stream (legacy stream or session generation,
  `internal/handlers/mapqueryfailure.go`): each reports `map: invalid query`
  too, so no line is streamed as plain payload in place of MapReduce results.
  This includes a map command rejected before it runs (e.g. options that do
  not parse), which itself reports `command: rejected`. A SESSION START or
  UPDATE builds its map command before it changes the session, so one that
  can not be built rejects the request and a running generation keeps
  running.
- **File read failures**, reported by dservers advertising capability
  `command-failure-v1` with the hidden message `.syn command failed <reason>`
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
are file read failures, it writes what it could read (the pre-d9 behaviour)
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

Compatibility: older clients ignore the unknown hidden message (and a client
of another protocol version never gets it). A current scheduler reading from
an older dserver (no `command-failure-v1`) only has the close handshake:
killed or shut down servers and lost connections are still detected, failed
reads on such a server are not (they still give a header-only or partial
outfile, as before). Continuous jobs and interactive `dmap` with an outfile
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
- **Client Base**: `/internal/clients/baseClient.go` - Common client functionality
- **MapReduce Parser**: `/internal/mapr/` (query.go, token.go, whereclause.go, wherecondition.go, selectcondition.go, groupset.go, etc.) - SQL-like query language parser
- **Log Format Parsers**: `/internal/mapr/logformat/` - Extensible log parsing system
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
2. Common functionality is in `baseClient.go`
3. Client-specific logic is in respective files (e.g., `tail.go`, `cat.go`)

When modifying server behavior:
1. Core server logic is in `/internal/server/server.go`
2. Session identity and read authorization in `/internal/sessionuser/`
3. Handler implementations in `/internal/handlers/`

When working with MapReduce:
1. Query parsing in `/internal/mapr/` (query.go, token.go, whereclause.go, wherecondition.go, selectcondition.go, groupset.go, etc.)
2. In-process aggregation in `/internal/mapr/aggregate/`
3. Log format parsing in `/internal/mapr/logformat/`
