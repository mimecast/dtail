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

**Compatibility and session semantics:**
- Initial interactive bootstrap prefers `SESSION START` when the server
  advertises capability `query-update-v1`
- If that capability is absent, startup falls back to the legacy command stream
  automatically so mixed-version client/server combinations still run
- Live `:reload` updates require every active server connection to advertise
  `query-update-v1`; unsupported servers cause the reload to be rejected while
  the current workload keeps running
- Successful reloads reuse the existing SSH session and advance a generation
  boundary so stale output from older workloads is dropped

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

### Journal Source Reads
Journal targets use `journal:unit.service` syntax. Permission rules should match the full target, for example `Server.Permissions.Users[user]: ["readfiles:^journal:.*\\.service$"]`.

**Technical Details:**
- Server capability `journal-v1` is required for any journal-backed read target; clients reject journal sources when the server does not advertise it.
- The capability is advertised only on Linux when `journalctl` is available on `PATH`.
- Journal reads use `journalctl` via `os/exec` only; there is no cgo or `libsystemd` path, and the feature is Linux-only at runtime.
- Non-follow reads the current journal snapshot once. Follow mode adds `-f -n 0` and keeps restarting `journalctl` until canceled.

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

**Server-Side MapReduce (dserver):**
- Lines are processed directly without channel overhead
- Batch processing reduces lock contention
- Memory pooling reduces garbage collection pressure
- Same output format and accuracy regardless of workload

**Output limits and deadlines (server config):**
The output path exposes four optional settings: `OutputBufferMaxBytes`,
`OutputFlushTimeoutMs`, `OutputReadRetryIntervalMs`, and
`OutputEOFAckTimeoutMs`. The buffer setting bounds retained payload memory; the
two timeout settings bound flush and EOF-ack waits. `OutputReadRetryIntervalMs`
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
configured cap must leave room for one maximum-length formatted line.

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
`readhub.Session.ReportFailure`. A max-count stop is no failure in either
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
Each job sends the option `share=<group>:<members>`; dserver honours it only
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
  crashed, or a lost connection; and a failed command that is no file read
  failure (`command: unable to decode command`, `command: rejected`,
  `command: unknown command`, `map: invalid query`, `read: unable to parse
  command`, or a reason the client does not know).
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
keep writing interim and final results whatever the exit status.

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
