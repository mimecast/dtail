# PGO Command Execution Details

This document shows the exact commands executed during Profile-Guided Optimization (PGO) generation for DTail tools.

## Overview

When running `make pgo-generate` or `dtail-tools pgo`, the following commands are executed to generate performance profiles for each tool.

## Commands Executed

### 1. Building Baseline Binaries

```bash
go build -o pgo-build/dtail-baseline ./cmd/dtail
go build -o pgo-build/dcat-baseline ./cmd/dcat
go build -o pgo-build/dgrep-baseline ./cmd/dgrep
go build -o pgo-build/dmap-baseline ./cmd/dmap
go build -o pgo-build/dserver-baseline ./cmd/dserver
```

### 2. Profile Generation Commands

#### DTail Profile Generation

The dtail workload (see `runDtailWorkload` in `internal/tools/pgo/pgo.go`) is
not serverless: the follow client needs a live dserver, working SSH auth and a
file that keeps growing during the capture. The workload therefore stands up
its own infrastructure per iteration:

1. An isolated work directory is created with deterministic key-based auth: a
   throwaway RSA keypair is generated, and the public key is written to
   `./cache/pgoprofile.authorized_keys`, which the ephemeral `-cfg none`
   dserver reads (a `.pub` sibling is written for AUTHKEY fast-reconnect
   registration).
2. A baseline `dserver` is started inside that work directory
   (so its `./cache` lookups resolve) on port `12223`:
```bash
pgo-build/dserver-baseline \
  -cfg none \
  -port 12223
```
3. A follow file (`dtail_follow.log`) is created and a background appender keeps
   adding a matching line every 50 ms for the whole capture window.
4. The dtail client runs against that server; `-shutdownAfter 6` bounds the
   session so the client returns from `client.Start` and flushes its CPU profile
   (the follow shutdown is what used to be broken, which is how a 0-byte
   `dtail.pprof` was once produced and committed):
```bash
pgo-build/dtail-baseline \
  -cfg none \
  -plain \
  -trustAllHosts \
  -user pgoprofile \
  -auth-key-path <workdir>/id_rsa \
  -profile \
  -profiledir pgo-profiles/iter_dtail_TIMESTAMP \
  -servers localhost:12223 \
  -files <workdir>/dtail_follow.log \
  -regex ERROR \
  -shutdownAfter 6
```
5. Representativeness guard: the client output is scanned for SSH-handshake
   failure markers ("SSH handshake failed", "unable to authenticate", ...). A
   failed handshake would produce a profile dominated by reconnect crypto churn
   rather than the streaming path, so the workload fails loudly in that case
   instead of shipping that garbage profile.

The resulting `dtail_cpu_<timestamp>.prof` is copied to
`pgo-profiles/dtail.pprof.<i>.pprof` for merging.

#### DCat Profile Generation
```bash
pgo-build/dcat-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dcat_TIMESTAMP \
  pgo-profiles/test.log
```

#### DGrep Profile Generation
```bash
pgo-build/dgrep-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dgrep_TIMESTAMP \
  -regex "ERROR|WARN" \
  pgo-profiles/test.log
```

#### DMap Profile Generation
```bash
pgo-build/dmap-baseline \
  -cfg none \
  -plain \
  -profile \
  -profiledir pgo-profiles/iter_dmap_TIMESTAMP \
  -files pgo-profiles/test.csv \
  -query "select status, count(*) group by status"
```

#### DServer Profile Generation

The dserver workload (see `runDServerWorkload` in `internal/tools/pgo/pgo.go`)
drives real, authenticated client traffic through the server and captures the
server-side CPU profile over a fixed window via the HTTP pprof endpoint:

1. Like the dtail workload, an isolated work directory is prepared with a
   throwaway RSA keypair and `./cache/pgoprofile.authorized_keys` for
   deterministic key-based auth.
2. The server is started inside that work directory with the pprof endpoint
   enabled, on SSH port `12222`:
```bash
pgo-build/dserver-baseline \
  -cfg none \
  -pprof localhost:16060 \
  -port 12222
```
3. Load clients use the plural `-servers` flag (the `-server` singular flag does
   not exist) and authenticate with the prepared key via `-user` and
   `-auth-key-path`. Three client commands generate the load (note `-files` for
   the file arguments):
```bash
pgo-build/dcat-baseline \
  -cfg none -plain -trustAllHosts \
  -user pgoprofile -auth-key-path <workdir>/id_rsa \
  -servers localhost:12222 \
  -files <test.log>

pgo-build/dgrep-baseline \
  -cfg none -plain -trustAllHosts \
  -user pgoprofile -auth-key-path <workdir>/id_rsa \
  -servers localhost:12222 \
  -regex "ERROR|WARN" \
  -files <test.log>

pgo-build/dmap-baseline \
  -cfg none -plain -trustAllHosts \
  -user pgoprofile -auth-key-path <workdir>/id_rsa \
  -servers localhost:12222 \
  -files <test.csv> \
  -query "select status, count(*) group by status"
```
4. Pre-capture auth probe: one client (dcat) runs synchronously first; if it
cannot complete the SSH handshake the workload fails before any capture
   window opens (broken auth would make every load client churn through the
   asymmetric SSH handshake and dominate the profile with crypto work).
5. Sustained load: four background worker goroutines keep re-running the three
   client commands in a loop, so the server is continuously busy for the whole
   profiling window (a previous fire-once variant captured an idle server).
6. After 500 ms of ramp-up, the CPU profile is captured for 8 seconds via an
   HTTP GET of the pprof endpoint (from Go, equivalent to):
```bash
curl "http://localhost:16060/debug/pprof/profile?seconds=8" > dserver.pprof
```
7. Post-capture representativeness guard: the captured profile's `go tool pprof
   -top` output must be dominated by streaming/read work, not asymmetric
   SSH-handshake crypto (`verifyDServerProfileRepresentative`); a
   handshake-dominated profile is rejected loudly. The HTTP capture always
   returns a file, so this backstop is what prevents a broken-auth capture from
   shipping silently.

### 3. Profile Merging

When multiple iterations are run, profiles are merged:

```bash
# Merge multiple profile iterations
go tool pprof -proto \
  pgo-profiles/dcat.pprof.0.pprof \
  pgo-profiles/dcat.pprof.1.pprof \
  > pgo-profiles/dcat.pprof
```

### 4. Building with PGO

```bash
# Build optimized binaries using profiles
go build -pgo=pgo-profiles/dcat.pprof -o pgo-build/dcat ./cmd/dcat
go build -pgo=pgo-profiles/dgrep.pprof -o pgo-build/dgrep ./cmd/dgrep
go build -pgo=pgo-profiles/dmap.pprof -o pgo-build/dmap ./cmd/dmap
go build -pgo=pgo-profiles/dtail.pprof -o pgo-build/dtail ./cmd/dtail
go build -pgo=pgo-profiles/dserver.pprof -o pgo-build/dserver ./cmd/dserver
```

### 5. Performance Comparison Commands

Quick benchmarks are run to compare baseline vs optimized:

```bash
# Baseline benchmark
pgo-build/dcat-baseline -cfg none -plain /tmp/pgo_bench.log

# Optimized benchmark
pgo-build/dcat -cfg none -plain /tmp/pgo_bench.log

# Similar commands for dgrep and dmap
pgo-build/dgrep-baseline -cfg none -plain -regex ERROR /tmp/pgo_bench.log
pgo-build/dmap-baseline -cfg none -plain -files /tmp/pgo_bench.csv -query "select count(*)"
```

## Test Data Generation

The PGO framework generates realistic test data:

### Log File (test.log)
- Contains timestamps, log levels (INFO, WARN, ERROR, DEBUG)
- Includes user actions, durations, and status information
- Default size: 1,000,000 lines (configurable with -datasize)

### CSV File (test.csv)
- Contains employee data with departments, salaries, status
- Used for MapReduce queries
- Default size: 100,000 rows (1/10 of log file size)

### Follow Log File (dtail workload)
- Used specifically for dtail profile generation
- Created by the dtail workload itself inside its isolated work directory
- A background appender writes a matching (`-regex ERROR`) line every 50 ms,
  simulating real-time log generation for the whole capture window

## Customization Options

### Adjust Test Data Size
```bash
dtail-tools pgo -datasize 5000000  # 5 million lines
```

### Run More Iterations
```bash
dtail-tools pgo -iterations 5  # Run 5 iterations per command
```

### Profile Specific Commands Only
```bash
dtail-tools pgo dcat dgrep  # Only optimize dcat and dgrep
```

### Verbose Output
```bash
dtail-tools pgo -v  # Show all command execution details
```

### Profile Generation Only
```bash
dtail-tools pgo -profileonly  # Skip building optimized binaries
```

## Notes

1. **Profile verification**: every captured profile is verified to exist, be
   non-empty (0 bytes) and contain at least one CPU sample
   (`verifyProfileNonEmpty`); the run fails loudly otherwise, and
   `mergeProfiles` fails when all iteration profiles of a command are empty.
   This prevents the class of silent empty-profile regressions that once put a
   0-byte `dtail.pprof` and a zero-sample `dserver.pprof` into the repository.

2. **DServer Profiling**: Uses HTTP pprof endpoint instead of command-line profiling to capture server-side performance data, under sustained authenticated client load, with a post-capture handshake-dominated-profile rejection guard.

3. **Concurrent Execution**: Multiple client commands are run concurrently against dserver to generate realistic load patterns.

4. **Profile Quality**: The effectiveness of PGO depends on how well the test workload represents real-world usage patterns. Both SSH-based workloads (dtail, dserver) include representativeness guards that reject handshake-churn profiles.