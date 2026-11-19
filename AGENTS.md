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

# Compare current performance against a baseline
make benchmark-compare BASELINE=benchmarks/baselines/baseline_TIMESTAMP.txt
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
- The tooling verifies every captured profile has non-zero CPU samples and
  fails loudly otherwise (see `internal/tools/pgo`), so an idle-server or
  I/O-bound capture can no longer silently produce an empty/zero-sample profile.
- **dtail is intentionally excluded from PGO.** Its follow client does not
  return from `client.Start` under `-shutdownAfter`, SIGINT or SIGTERM (the
  pre-existing "auto shutdown does not work" bug noted in `cmd/dtail/main.go`),
  so it never flushes a CPU profile. Attempting to profile it produced the
  0-byte `dtail.pprof`. The `dtail-tools pgo` default command set therefore
  covers dcat, dgrep, dmap and dserver only, matching the existing
  `internal/tools/profile` harness which also omits dtail. Re-enable dtail here
  once its follow shutdown is fixed.

## Profiling

```bash
# Profile all commands (dcat, dgrep, dmap)
make profile-all

# Profile individual commands
make profile-dcat         # Profile dcat with test data
make profile-dgrep        # Profile dgrep with test data
make profile-dmap         # Profile dmap MapReduce queries

# Quick profiling with smaller datasets
make profile-quick

# Full automated profiling (includes larger files)
make profile-auto

# Clean all profile data
make profile-clean

# Analyze a specific profile interactively
make profile-analyze PROFILE=profiles/dcat_cpu_*.prof

# Generate flame graph visualization
make profile-flamegraph PROFILE=profiles/dcat_cpu_*.prof

# Custom profiling options
PROFILE_SIZE=10000000 make profile-all    # Profile with 10M lines
PROFILE_DIR=myprofiles make profile-dcat  # Custom profile directory

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
- Daily file rotation uses a day name cached by the file sink and refreshed on its
  100 ms idle-flush tick, so no clock is read per line. The file is chosen at
  write time: around midnight a line (including a timestamped dserver diagnostic)
  can land in the neighbouring day's file. This is intended.

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
- Without local context (before/after/max), a matching file-backed
  cat/grep/tail line is handed to `DirectLineProcessor` as a borrowed slice of
  the reader's buffer through the optional `line.RawProcessor` interface, so no
  pooled per-line buffer is used; the slice is only valid during the call and
  must not be retained. Context greps, journal (`journal:`) reads and the
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

# Run performance profiling
make profile

# Generate profiling reports
make profile-report

# Run specific benchmark suites
make benchmark-network
make benchmark-mapreduce
make benchmark-ssh
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
- **MapReduce Parser**: `/internal/mapr/parse/` - SQL-like query language parser
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
1. Query parsing in `/internal/mapr/parse/`
2. In-process aggregation in `/internal/mapr/aggregate/`
3. Log format parsing in `/internal/mapr/logformat/`
